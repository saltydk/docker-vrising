#!/usr/bin/env bash

set -euo pipefail

export LC_ALL=C

readonly startup_wait_seconds=1800
readonly label_key=com.saltydk.docker-vrising.live-acceptance

mode=
image=
source_server_dir=
source_data_dir=
temp_parent=
temp_root=
suite_token=
suite_label=
container_name=
network_name=
server_dir=
data_dir=
container_created=false
created_container_id=
network_created=false
created_network_id=
creation_handoff=false
deferred_signal=0
log_follower_pid=

usage() {
	printf 'usage: %s fresh IMAGE\n' "$0" >&2
	printf '       %s migrate IMAGE SOURCE_SERVER_DIR SOURCE_DATA_DIR\n' "$0" >&2
	exit 64
}

fail() {
	printf 'live acceptance: %s\n' "$*" >&2
	exit 1
}

handle_int() {
	if [[ $creation_handoff == true ]]; then
		deferred_signal=130
		return
	fi
	exit 130
}

handle_term() {
	if [[ $creation_handoff == true ]]; then
		deferred_signal=143
		return
	fi
	exit 143
}

begin_creation_handoff() {
	deferred_signal=0
	creation_handoff=true
}

finish_creation_handoff() {
	local pending

	creation_handoff=false
	pending=$deferred_signal
	deferred_signal=0
	if [[ $pending -ne 0 ]]; then
		exit "$pending"
	fi
}

read_single_docker_id() {
	local source=$1
	local -a values=()

	[[ -s $source ]] || return 1
	mapfile -t values <"$source"
	[[ ${#values[@]} -eq 1 && ${values[0]} =~ ^[0-9a-f]{64}$ ]] || return 1
	printf '%s\n' "${values[0]}"
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

paths_overlap() {
	local left=$1
	local right=$2

	[[ $left == "$right" || $left == "$right"/* || $right == "$left"/* ]]
}

canonical_source() {
	local label=$1
	local source=$2
	local canonical

	[[ $source == /* ]] || fail "$label must be an absolute path"
	[[ -d $source && ! -L $source ]] || fail "$label must be an existing non-symlink directory"
	canonical=$(realpath -e -- "$source") || fail "cannot resolve $label"
	[[ -d $canonical && ! -L $canonical ]] || fail "$label did not resolve to a directory"
	printf '%s\n' "$canonical"
}

assert_no_open_processes() {
	local tree=$1
	local status

	if lsof -t +D "$tree" >/dev/null 2>&1; then
		fail "a host process has a source tree open; quiesce it before migration"
	else
		status=$?
	fi
	[[ $status -eq 1 ]] || fail "could not prove that a source tree is quiescent"
}

assert_no_running_container_uses_sources() {
	local container_ids container_id mount_list mount_source canonical_mount

	container_ids=$(docker ps -q) || fail "could not list running containers"
	while IFS= read -r container_id; do
		[[ -n $container_id ]] || continue
		mount_list=$(docker inspect --format '{{range .Mounts}}{{if eq .Type "bind"}}{{println .Source}}{{end}}{{end}}' "$container_id") \
			|| fail "could not inspect running container mounts"
		while IFS= read -r mount_source; do
			[[ -n $mount_source ]] || continue
			canonical_mount=$(realpath -m -- "$mount_source")
			if paths_overlap "$canonical_mount" "$source_server_dir" || paths_overlap "$canonical_mount" "$source_data_dir"; then
				fail "a running container uses a source tree; stop it before migration"
			fi
		done <<<"$mount_list"
	done <<<"$container_ids"
}

assert_sources_quiescent() {
	assert_no_open_processes "$source_server_dir"
	assert_no_open_processes "$source_data_dir"
	assert_no_running_container_uses_sources
}

container_identity_matches() {
	local actual_id actual_name owner

	[[ $container_created == true && -n $created_container_id ]] || return 1
	actual_id=$(docker inspect --format '{{.Id}}' "$created_container_id" 2>/dev/null) || return 1
	actual_name=$(docker inspect --format '{{.Name}}' "$created_container_id" 2>/dev/null) || return 1
	owner=$(docker inspect --format "{{ index .Config.Labels \"$label_key\" }}" "$created_container_id" 2>/dev/null) || return 1
	[[ $actual_id == "$created_container_id" && $actual_name == "/$container_name" && $owner == "$suite_token" ]]
}

network_identity_matches() {
	local actual_id actual_name owner

	[[ $network_created == true && -n $created_network_id ]] || return 1
	actual_id=$(docker network inspect --format '{{.Id}}' "$created_network_id" 2>/dev/null) || return 1
	actual_name=$(docker network inspect --format '{{.Name}}' "$created_network_id" 2>/dev/null) || return 1
	owner=$(docker network inspect --format "{{ index .Labels \"$label_key\" }}" "$created_network_id" 2>/dev/null) || return 1
	[[ $actual_id == "$created_network_id" && $actual_name == "$network_name" && $owner == "$suite_token" ]]
}

cleanup_created_resources() {
	local failed=0

	if [[ $container_created == true ]]; then
		if container_identity_matches && docker rm -fv "$created_container_id" >/dev/null 2>&1; then
			container_created=false
			created_container_id=
			reap_log_follower
		else
			failed=1
			cancel_log_follower
		fi
	else
		cancel_log_follower
	fi
	if [[ $network_created == true ]]; then
		if network_identity_matches && docker network rm "$created_network_id" >/dev/null 2>&1; then
			network_created=false
			created_network_id=
		else
			failed=1
		fi
	fi
	return "$failed"
}

start_log_follower() {
	[[ -z $log_follower_pid ]] || fail "container log follower is already active"
	printf 'live acceptance: streaming container logs for %s\n' "$created_container_id"
	docker logs --follow "$created_container_id" &
	log_follower_pid=$!
}

log_follower_is_running() {
	local running

	[[ -n $log_follower_pid ]] || return 1
	while IFS= read -r running; do
		[[ $running == "$log_follower_pid" ]] && return 0
	done < <(jobs -pr)
	return 1
}

reap_log_follower() {
	[[ -n $log_follower_pid ]] || return 0
	for _ in {1..50}; do
		if ! log_follower_is_running; then
			break
		fi
		sleep 0.1
	done
	if log_follower_is_running; then
		kill "$log_follower_pid" >/dev/null 2>&1 || true
	fi
	wait "$log_follower_pid" >/dev/null 2>&1 || true
	log_follower_pid=
}

cancel_log_follower() {
	[[ -n $log_follower_pid ]] || return 0
	if log_follower_is_running; then
		kill "$log_follower_pid" >/dev/null 2>&1 || true
	fi
	wait "$log_follower_pid" >/dev/null 2>&1 || true
	log_follower_pid=
}

remove_temp_root() {
	[[ -n ${temp_root:-} && -d $temp_root && ! -L $temp_root ]] || return 1
	case $temp_root in
		"$temp_parent"/docker-vrising-live.*) ;;
		*) return 1 ;;
	esac
	rm -rf -- "$temp_root"
}

cleanup() {
	local status=$?

	trap - EXIT INT TERM
	set +e
	if ! cleanup_created_resources && [[ $status -eq 0 ]]; then
		status=1
	fi
	if [[ $status -eq 0 ]]; then
		if ! remove_temp_root; then
			printf 'live acceptance: could not remove validated temporary root\n' >&2
			status=1
		fi
	fi
	if [[ $status -ne 0 && -n ${temp_root:-} ]]; then
		printf 'live acceptance: retained temporary root: %s\n' "$temp_root" >&2
	fi
	exit "$status"
}

assert_no_resource_collisions() {
	local matches

	matches=$(docker ps -aq --filter "name=^/${container_name}$") || fail "could not inspect container names"
	[[ -z $matches ]] \
		|| fail "generated container name already exists"
	matches=$(docker ps -aq --filter "label=$suite_label") || fail "could not inspect container labels"
	[[ -z $matches ]] \
		|| fail "generated container label is already in use"
	matches=$(docker network ls -q --filter "name=^${network_name}$") || fail "could not inspect network names"
	[[ -z $matches ]] \
		|| fail "generated network name already exists"
	matches=$(docker network ls -q --filter "label=$suite_label") || fail "could not inspect network labels"
	[[ -z $matches ]] \
		|| fail "generated network label is already in use"
}

assert_container_owned() {
	container_identity_matches \
		|| fail "acceptance container identity does not match its successful creation"
}

assert_bind_mounts() {
	local mounts

	mounts=$(docker inspect --format '{{json .Mounts}}' "$created_container_id") \
		|| fail "cannot inspect acceptance container mounts"
	jq -e --arg server "$server_dir" --arg data "$data_dir" '
		length == 2 and
		all(.Type == "bind") and
		(map(select(.Source == $server and .Destination == "/mnt/vrising/server")) | length == 1) and
		(map(select(.Source == $data and .Destination == "/mnt/vrising/persistentdata")) | length == 1)
	' <<<"$mounts" >/dev/null || fail "container is not running only against the disposable bind copies"
}

create_network() {
	local id_file=$temp_root/network.id
	local command_status captured_id=

	: >"$id_file"
	begin_creation_handoff
	set +e
	docker network create --label "$suite_label" "$network_name" >"$id_file"
	command_status=$?
	set -e
	if captured_id=$(read_single_docker_id "$id_file"); then
		created_network_id=$captured_id
		network_created=true
	fi
	rm -f -- "$id_file"
	finish_creation_handoff

	[[ $command_status -eq 0 ]] || fail "could not create acceptance network"
	[[ $network_created == true ]] || fail "Docker returned an invalid acceptance network ID"
	network_identity_matches || fail "acceptance network identity does not match its creation"
}

start_container() {
	local network=$1
	local publish_ports=$2
	local cid_file=$temp_root/container.cid
	local command_status captured_id=
	local -a arguments=(
		create
		--cidfile "$cid_file"
		--name "$container_name"
		--label "$suite_label"
		--network "$network"
		--env TZ=Europe/Copenhagen
		--env SERVERNAME=Salty
		--env WINEDEBUG=fixme-all
		--mount "type=bind,src=$server_dir,dst=/mnt/vrising/server"
		--mount "type=bind,src=$data_dir,dst=/mnt/vrising/persistentdata"
	)

	if [[ $publish_ports == true ]]; then
		arguments+=(
			--publish 127.0.0.1::9876/udp
			--publish 127.0.0.1::9877/udp
			--publish 127.0.0.1::25575/tcp
		)
	fi
	arguments+=("$image")

	rm -f -- "$cid_file"
	begin_creation_handoff
	set +e
	docker "${arguments[@]}" >/dev/null
	command_status=$?
	set -e
	if captured_id=$(read_single_docker_id "$cid_file"); then
		created_container_id=$captured_id
		container_created=true
	fi
	rm -f -- "$cid_file"
	finish_creation_handoff

	[[ $command_status -eq 0 ]] || fail "could not create acceptance container"
	[[ $container_created == true ]] || fail "Docker did not record a valid acceptance container ID"
	assert_container_owned
	assert_bind_mounts
	docker start "$created_container_id" >/dev/null || fail "could not start acceptance container"
	start_log_follower
}

remove_container() {
	assert_container_owned
	docker rm -fv "$created_container_id" >/dev/null || fail "could not remove acceptance container"
	container_created=false
	created_container_id=
	reap_log_follower
}

stop_cleanly() {
	local exit_code

	docker stop --time 130 "$created_container_id" >/dev/null || fail "container did not stop within the shutdown allowance"
	exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$created_container_id") \
		|| fail "cannot inspect stopped container exit status"
	[[ $exit_code -eq 0 ]] || fail "controller returned a non-zero status during clean shutdown"
	remove_container
}

wait_healthy() {
	local deadline=$((SECONDS + startup_wait_seconds))
	local status health

	while (( SECONDS < deadline )); do
		status=$(docker inspect --format '{{.State.Status}}' "$created_container_id" 2>/dev/null) \
			|| fail "acceptance container disappeared before health validation"
		health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$created_container_id") \
			|| fail "cannot inspect acceptance container health"
		[[ $health == healthy ]] && return 0
		[[ $health != missing ]] || fail "image has no Docker health check"
		[[ $status != exited && $status != dead ]] || fail "acceptance container exited before becoming healthy"
		sleep 5
	done
	fail "acceptance container did not become healthy within 30 minutes"
}

assert_published_port() {
	local port=$1
	local protocol=$2

	docker port "$created_container_id" "$port/$protocol" >/dev/null 2>&1 \
		|| fail "$protocol port $port is not published"
}

assert_container_socket() {
	local protocol=$1
	local port=$2
	local port_hex
	local -a tables

	printf -v port_hex '%04X' "$port"
	case $protocol in
		udp) tables=(/proc/net/udp /proc/net/udp6) ;;
		tcp) tables=(/proc/net/tcp /proc/net/tcp6) ;;
		*) fail "unsupported socket protocol" ;;
	esac

	docker exec "$created_container_id" awk -v expected_port="$port_hex" -v protocol="$protocol" '
		NR > 1 {
			split($2, local_address, ":")
			if (toupper(local_address[2]) == expected_port && (protocol != "tcp" || $4 == "0A")) {
				found = 1
			}
		}
		END { exit(found ? 0 : 1) }
	' "${tables[@]}" >/dev/null || fail "$protocol port $port is not bound inside the container"
}

assert_udp_ports() {
	for port in 9876 9877; do
		assert_published_port "$port" udp
		assert_container_socket udp "$port"
	done
}

assert_live_installation() {
	docker exec "$created_container_id" vrisingctl verify >/dev/null \
		|| fail "deep live Steam, lock, generation, and managed-file validation failed"
}

assert_rcon_if_enabled() {
	local settings=$data_dir/Settings/ServerHostSettings.json

	[[ -f $settings ]] || return 0
	jq empty "$settings" >/dev/null 2>&1 || fail "copied ServerHostSettings.json is not valid JSON"
	if jq -e '(.Rcon.Enabled? // false) == true' "$settings" >/dev/null; then
		assert_published_port 25575 tcp
		assert_container_socket tcp 25575
	fi
}

assert_steam_state() {
	local runtime_app_id

	[[ -f $server_dir/VRisingServer.exe ]] || fail "Steam server executable is missing"
	[[ -f $server_dir/steamapps/appmanifest_1829350.acf ]] || fail "Steam app manifest is missing"
	[[ -f $server_dir/steam_appid.txt ]] || fail "runtime Steam app ID is missing"
	runtime_app_id=$(tr -d '\r\n' <"$server_dir/steam_appid.txt")
	[[ $runtime_app_id == 1604030 ]] || fail "runtime Steam app ID is not canonical"
	grep -Eq '"appid"[[:space:]]+"1829350"' "$server_dir/steamapps/appmanifest_1829350.acf" \
		|| fail "Steam app manifest has the wrong application ID"
}

assert_package_graph() {
	local lock=$server_dir/.docker-vrising/package-lock.json

	[[ -s $lock ]] || fail "managed package lock is missing"
	jq -e '
		def ref: [.Namespace, .Name, .Version];
		.Packages as $packages |
		(.SchemaVersion == 1) and
		(.Digest | test("^[0-9a-f]{64}$")) and
		(.Roots | length == 2) and
		($packages | length == 5) and
		([.Roots[] | [.Namespace, .Name]] == [
			["odjit", "KindredCommands"],
			["Team_GreenEye", "Satisvampory"]
		]) and
		([$packages[] | [.Ref.Namespace, .Ref.Name]] == [
			["BepInEx", "BepInExPack_V_Rising"],
			["deca", "VampireCommandFramework"],
			["odjit", "KindredCommands"],
			["cheesasaurus", "HookDOTS_API"],
			["Team_GreenEye", "Satisvampory"]
		]) and
		([.Roots[] | ref] == [($packages[2].Ref | ref), ($packages[4].Ref | ref)]) and
		(all($packages[];
			(.Ref.Version | test("^[0-9]+\\.[0-9]+\\.[0-9]+$")) and
			(.FullName == (.Ref.Namespace + "-" + .Ref.Name + "-" + .Ref.Version)) and
			(.DownloadURL | length > 0) and
			(.FileSize > 0) and
			(.SHA256 | test("^[0-9a-f]{64}$"))
		)) and
		(($packages[0].Dependencies // []) == []) and
		(([(($packages[1].Dependencies // [])[] | ref)] | sort) == [($packages[0].Ref | ref)]) and
		(([(($packages[2].Dependencies // [])[] | ref)] | sort) == ([($packages[0].Ref | ref), ($packages[1].Ref | ref)] | sort)) and
		(([(($packages[3].Dependencies // [])[] | ref)] | sort) == [($packages[0].Ref | ref)]) and
		(([(($packages[4].Dependencies // [])[] | ref)] | sort) == ([($packages[0].Ref | ref), ($packages[3].Ref | ref), ($packages[1].Ref | ref)] | sort))
	' "$lock" >/dev/null || fail "managed package state is not the canonical exact two-root/five-package graph"
}

assert_managed_dlls() {
	local managed_dir=$server_dir/BepInEx/plugins/saltydk-managed
	local actual
	local expected

	[[ -d $managed_dir && ! -L $managed_dir ]] || fail "managed plugin directory is missing"
	[[ -z $(find "$managed_dir" -mindepth 1 -maxdepth 1 ! -type f -print -quit) ]] \
		|| fail "managed plugin directory contains a non-file entry"
	actual=$(find "$managed_dir" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort)
	expected=$(printf '%s\n' \
		HookDOTS.API.dll \
		KindredCommands.dll \
		NetTopologySuite.dll \
		Satisvampory.dll \
		VampireCommandFramework.dll)
	[[ $actual == "$expected" ]] || fail "managed plugin DLL set is not exact"
}

assert_runtime_state() {
	local expected_degraded=$1
	local state=$server_dir/.docker-vrising/state.json
	local lock=$server_dir/.docker-vrising/package-lock.json

	[[ -s $state ]] || fail "runtime state is missing"
	jq -e --slurpfile lock "$lock" --argjson degraded "$expected_degraded" '
		.SchemaVersion == 1 and
		(.SteamBuild | type == "string" and length > 0) and
		(.Active.ID | type == "string" and length > 0) and
		.Active.Status == "active" and
		.Active.LockDigest == $lock[0].Digest and
		.Candidate == null and
		.Transaction == null and
		.Promotion == null and
		.Runtime.Ready == true and
		.Runtime.Phase == "ready" and
		.Runtime.Degraded == $degraded and
		(.Runtime.Generation == .Active.ID) and
		(.Runtime.SteamBuild == .SteamBuild) and
		(if $degraded then (.Runtime.Reason | type == "string" and length > 0) else .Runtime.Reason == "" end)
	' "$state" >/dev/null || fail "runtime state does not match healthy activation expectations"
}

snapshot_protected_tree() {
	local root=$1
	local output=$2
	local selection=${3:-all}
	local unsorted=$output.unsorted
	local paths=$output.paths
	local -a protected_roots=()
	local protected entry relative mode entry_type digest target

	: >"$unsorted"
	for protected in Settings Saves; do
		[[ $selection != settings || $protected == Settings ]] || continue
		if [[ -e $root/$protected || -L $root/$protected ]]; then
			protected_roots+=("$root/$protected")
		fi
	done

	if (( ${#protected_roots[@]} > 0 )); then
		find "${protected_roots[@]}" -print0 >"$paths" \
			|| fail "could not enumerate protected Settings/Saves entries"
		while IFS= read -r -d '' entry; do
			relative=${entry#"$root"/}
			mode=$(stat -c '%f' -- "$entry") || fail "could not record protected entry mode"
			if [[ -L $entry ]]; then
				entry_type='symlink'
				target=$(readlink -- "$entry") || fail "could not record protected symlink"
				digest=$(printf '%s' "$target" | sha256sum | awk '{print $1}')
			elif [[ -f $entry ]]; then
				entry_type='file'
				digest=$(sha256sum -- "$entry" | awk '{print $1}')
			elif [[ -d $entry ]]; then
				entry_type='directory'
				digest=-
			else
				fail "protected Settings/Saves contains an unsupported entry type"
			fi
			printf '%s\t%s\t%s\t%s\0' "$relative" "$entry_type" "$mode" "$digest" >>"$unsorted"
		done <"$paths"
		rm -- "$paths"
	fi

	sort -z "$unsorted" >"$output"
	rm -- "$unsorted"
}

assert_loaded_existing_save() {
	local output record directory saved_file relative expected_world
	output=$(docker logs "$created_container_id" 2>&1) || fail "cannot inspect save-load evidence"
	record=$(awk '/CreateAndHostServer - SaveDirectory:/ {line=$0; sub(/^.*SaveDirectory:/, "", line); sub(/\r$/, "", line); print line; exit}' <<<"$output")
	[[ $record == *', Loaded Save:'* ]] || fail "server did not report a loaded save"
	directory=${record%%, Loaded Save:*}
	saved_file=${record#*, Loaded Save:}
	[[ -n $saved_file && $saved_file != '<None>' && $saved_file != */* && $saved_file != *\\* ]] \
		|| fail "server created a new world instead of loading the existing save"
	directory=${directory//\\//}
	[[ $directory == Z:/mnt/vrising/persistentdata/Saves/* ]] || fail "save loaded outside persistent data"
	relative=${directory#Z:/mnt/vrising/persistentdata/}
	[[ $relative != *'/../'* && -f $source_data_dir/$relative/$saved_file ]] \
		|| fail "loaded save did not exist in the source world"
	expected_world=
	if [[ -f $source_data_dir/Settings/ServerHostSettings.json ]]; then
		expected_world=$(jq -r '.SaveName // empty' "$source_data_dir/Settings/ServerHostSettings.json")
	fi
	[[ -z $expected_world || ${directory##*/} == "$expected_world" ]] || fail "server loaded the wrong configured world"
}

assert_snapshot_equal() {
	local before=$1
	local after=$2
	local description=$3

	cmp -s "$before" "$after" || fail "$description"
}

assert_snapshot_preserved() {
	local before=$1
	local after=$2
	local missing=$temp_root/protected-missing.bin

	comm -z -23 "$before" "$after" >"$missing"
	[[ ! -s $missing ]] || fail "a pre-existing Settings/Saves entry disappeared or changed content, type, or mode"
}

print_evidence() {
	local state=$server_dir/.docker-vrising/state.json
	local lock=$server_dir/.docker-vrising/package-lock.json
	local image_id steam_build lock_digest packages

	image_id=$(docker image inspect --format '{{.Id}}' "$image")
	steam_build=$(jq -er '.SteamBuild' "$state")
	lock_digest=$(jq -er '.Digest' "$lock")
	packages=$(jq -er '[.Packages[].FullName] | join(",")' "$lock")
	printf 'live acceptance: PASS mode=%s image=%s steam_build=%s package_lock=%s packages=%s\n' \
		"$mode" "$image_id" "$steam_build" "$lock_digest" "$packages"
}

run_fresh() {
	local active_id lock_digest package_lock_sha

	[[ -z $(find "$server_dir" -mindepth 1 -print -quit) ]] || fail "fresh server bind is not empty"
	[[ -z $(find "$data_dir" -mindepth 1 -print -quit) ]] || fail "fresh data bind is not empty"

	create_network
	start_container "$network_name" true
	wait_healthy
	assert_live_installation
	assert_steam_state
	assert_package_graph
	assert_managed_dlls
	assert_runtime_state false
	assert_udp_ports
	active_id=$(jq -er '.Active.ID' "$server_dir/.docker-vrising/state.json")
	lock_digest=$(jq -er '.Digest' "$server_dir/.docker-vrising/package-lock.json")
	package_lock_sha=$(sha256sum "$server_dir/.docker-vrising/package-lock.json" | awk '{print $1}')
	stop_cleanly

	start_container none false
	wait_healthy
	assert_live_installation
	assert_runtime_state true
	[[ $(jq -er '.Active.ID' "$server_dir/.docker-vrising/state.json") == "$active_id" ]] \
		|| fail "offline restart changed the active generation"
	[[ $(jq -er '.Digest' "$server_dir/.docker-vrising/package-lock.json") == "$lock_digest" ]] \
		|| fail "offline restart changed the package lock digest"
	[[ $(sha256sum "$server_dir/.docker-vrising/package-lock.json" | awk '{print $1}') == "$package_lock_sha" ]] \
		|| fail "offline restart changed the package lock bytes"
	assert_managed_dlls
	stop_cleanly
	print_evidence
}

run_migrate() {
	local source_before=$temp_root/source-protected.before
	local source_after_copy=$temp_root/source-protected.after-copy
	local source_final=$temp_root/source-protected.final
	local copy_before=$temp_root/copy-protected.before
	local copy_after=$temp_root/copy-protected.after
	local settings_before=$temp_root/copy-settings.before

	[[ -d $source_data_dir/Saves ]] || fail "migration source has no Saves directory"
	[[ -n $(find "$source_data_dir/Saves" -type f -print -quit) ]] \
		|| fail "migration source has no existing save file"

	snapshot_protected_tree "$source_data_dir" "$source_before"
	rsync -a --numeric-ids -- "$source_server_dir/" "$server_dir/"
	rsync -a --numeric-ids -- "$source_data_dir/" "$data_dir/"
	snapshot_protected_tree "$data_dir" "$copy_before"
	assert_snapshot_equal "$source_before" "$copy_before" "rsync did not preserve every source Settings/Saves entry"
	snapshot_protected_tree "$data_dir" "$settings_before" settings
	snapshot_protected_tree "$source_data_dir" "$source_after_copy"
	assert_snapshot_equal "$source_before" "$source_after_copy" "a source Settings/Saves entry changed while it was copied"
	assert_sources_quiescent

	create_network
	start_container "$network_name" true
	wait_healthy
	assert_live_installation
	assert_steam_state
	assert_package_graph
	assert_managed_dlls
	assert_runtime_state false
	assert_udp_ports
	assert_rcon_if_enabled
	assert_loaded_existing_save
	stop_cleanly

	snapshot_protected_tree "$data_dir" "$copy_after" settings
	assert_snapshot_preserved "$settings_before" "$copy_after"
	snapshot_protected_tree "$source_data_dir" "$source_final"
	assert_snapshot_equal "$source_before" "$source_final" "a source Settings/Saves entry changed during acceptance"
	assert_sources_quiescent
	print_evidence
}

if [[ ${BASH_SOURCE[0]} != "$0" ]]; then
	return 0
fi

case ${1:-} in
	fresh)
		[[ $# -eq 2 && -n ${2:-} ]] || usage
		mode=fresh
		image=$2
		;;
	migrate)
		[[ $# -eq 4 && -n ${2:-} && -n ${3:-} && -n ${4:-} ]] || usage
		mode=migrate
		image=$2
		source_server_dir=$3
		source_data_dir=$4
		;;
	*) usage ;;
esac

for command in awk cmp comm docker find grep jq mkdir mktemp readlink realpath rm sha256sum sleep sort stat tr; do
	require_command "$command"
done

if [[ $mode == migrate ]]; then
	for command in lsof rsync; do
		require_command "$command"
	done
	(( EUID == 0 )) || fail "migrate mode must run as root to inspect every process and preserve numeric ownership"
	source_server_dir=$(canonical_source SOURCE_SERVER_DIR "$source_server_dir")
	source_data_dir=$(canonical_source SOURCE_DATA_DIR "$source_data_dir")
	paths_overlap "$source_server_dir" "$source_data_dir" \
		&& fail "migration source directories must be distinct and non-overlapping"
	[[ $(stat -Lc '%d:%i' -- "$source_server_dir") != $(stat -Lc '%d:%i' -- "$source_data_dir") ]] \
		|| fail "migration source directories resolve to the same filesystem object"
fi

docker image inspect "$image" >/dev/null 2>&1 || fail "image is unavailable: $image"
[[ $(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image") == linux/amd64 ]] \
	|| fail "image must be linux/amd64"

if [[ $mode == migrate ]]; then
	assert_sources_quiescent
fi

temp_parent=$(realpath -e -- "${TMPDIR:-/tmp}") || fail "cannot resolve temporary parent"
[[ $temp_parent == /* && -d $temp_parent && ! -L $temp_parent && $temp_parent != / ]] \
	|| fail "temporary parent must be a safe absolute directory"
[[ $temp_parent != /opt/v-rising && $temp_parent != /opt/v-rising/* ]] \
	|| fail "temporary cleanup must never target /opt/v-rising"
[[ $temp_parent != *,* && $temp_parent != *$'\n'* ]] \
	|| fail "temporary parent contains unsupported characters"
if [[ $mode == migrate ]] && { paths_overlap "$temp_parent" "$source_server_dir" || paths_overlap "$temp_parent" "$source_data_dir"; }; then
	fail "temporary parent must not overlap either migration source"
fi

temp_root=$(mktemp -d "$temp_parent/docker-vrising-live.XXXXXX")
trap cleanup EXIT
trap handle_int INT
trap handle_term TERM
[[ -d $temp_root && ! -L $temp_root && ${temp_root%/*} == "$temp_parent" ]] \
	|| fail "mktemp returned an invalid temporary root"
case ${temp_root##*/} in
	docker-vrising-live.[A-Za-z0-9][A-Za-z0-9][A-Za-z0-9][A-Za-z0-9][A-Za-z0-9][A-Za-z0-9]) ;;
	*) fail "mktemp returned an unexpected temporary root" ;;
esac
suite_token=${temp_root##*.}
suite_token=${suite_token,,}
[[ $suite_token =~ ^[a-z0-9]{6}$ ]] || fail "temporary token is invalid"
suite_label=$label_key=$suite_token
container_name=docker-vrising-live-$suite_token
network_name=$container_name-network
server_dir=$temp_root/server
data_dir=$temp_root/persistentdata
mkdir -p "$server_dir" "$data_dir"

assert_no_resource_collisions

case $mode in
	fresh) run_fresh ;;
	migrate) run_migrate ;;
esac
