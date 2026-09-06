#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
live_script=$repo_root/hack/live-acceptance.sh
fixture_tmp_parent=${TMPDIR:-/tmp}
sandbox=

fail() {
	printf 'live acceptance test: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	if [[ -n ${sandbox:-} && -d $sandbox && ! -L $sandbox ]]; then
		case $sandbox in
			"$fixture_tmp_parent"/docker-vrising-live-test.*) rm -rf -- "$sandbox" ;;
		esac
	fi
}
trap cleanup EXIT INT TERM

sandbox=$(mktemp -d "$fixture_tmp_parent/docker-vrising-live-test.XXXXXX")
case $sandbox in
	"$fixture_tmp_parent"/docker-vrising-live-test.*) ;;
	*) fail "refusing unsafe test root: $sandbox" ;;
esac
[[ -d $sandbox && ! -L $sandbox ]] || fail "invalid test root: $sandbox"

fake_bin=$sandbox/fake-bin
mkdir -p "$fake_bin"

cat >"$fake_bin/docker" <<'FAKE_DOCKER'
#!/usr/bin/env bash
set -euo pipefail

state=${FAKE_DOCKER_STATE:?}
scenario=${FAKE_DOCKER_SCENARIO:?}
container_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
network_id=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
mkdir -p "$state"
printf '%q ' "$@" >>"$state/calls"
printf '\n' >>"$state/calls"

argument_after() {
	local expected=$1
	shift
	while (( $# > 0 )); do
		if [[ $1 == "$expected" && $# -gt 1 ]]; then
			printf '%s\n' "$2"
			return 0
		fi
		shift
	done
	return 1
}

case ${1-} in
	image)
		[[ ${2-} == inspect ]] || exit 2
		if [[ $* == *--format* ]]; then
			printf '%s\n' linux/amd64
		fi
		;;
	ps)
		filter=$(argument_after --filter "$@" || true)
		case $scenario:$filter in
			collision-container:name=*) printf '%s\n' preexisting-container ;;
			collision-container:label=*)
				printf '%s\n' "$filter" >"$state/queried-label"
				printf '%s\n' preexisting-container
				;;
			*:label=*)
				if [[ -f $state/container-id ]]; then
					printf '%s\n' "$(<"$state/container-id")"
				fi
				;;
		esac
		;;
	network)
		case ${2-} in
			ls)
				filter=$(argument_after --filter "$@" || true)
				case $scenario:$filter in
					collision-network:name=*) printf '%s\n' preexisting-network ;;
					collision-network:label=*)
						printf '%s\n' "$filter" >"$state/queried-label"
						printf '%s\n' preexisting-network
						;;
					*:label=*)
						if [[ -f $state/network-id ]]; then
							printf '%s\n' "$(<"$state/network-id")"
						fi
						;;
				esac
				;;
			create)
				label=$(argument_after --label "$@")
				name=${!#}
				printf '%s\n' "$network_id" >"$state/network-id"
				printf '%s\n' "$name" >"$state/network-name"
				printf '%s\n' "$label" >"$state/network-label"
				printf '%s\n' "$network_id"
				if [[ $scenario == network-create-term ]]; then
					kill -TERM "$PPID"
				fi
				if [[ $scenario == network-create-id-failure ]]; then
					exit 125
				fi
				;;
			inspect)
				format=$(argument_after --format "$@")
				target=${!#}
				if [[ $target == preexisting-network ]]; then
					label=$(<"$state/queried-label")
					case $format in
						*Labels*) printf '%s\n' "${label##*=}" ;;
						*Name*) printf '%s\n' preexisting-network-name ;;
						*Id*) printf '%s\n' preexisting-network ;;
					esac
				else
					case $format in
						*Labels*) label=$(<"$state/network-label"); printf '%s\n' "${label##*=}" ;;
						*Name*) printf '%s\n' "$(<"$state/network-name")" ;;
						*Id*) printf '%s\n' "$(<"$state/network-id")" ;;
					esac
				fi
				;;
			rm)
				rm -f -- "$state/network-id" "$state/network-name" "$state/network-label"
				;;
		esac
		;;
	create)
		cidfile=$(argument_after --cidfile "$@")
		name=$(argument_after --name "$@")
		label=$(argument_after --label "$@")
		mapfile -t mounts < <(
			while (( $# > 0 )); do
				if [[ $1 == --mount && $# -gt 1 ]]; then
					printf '%s\n' "$2"
					shift
				fi
				shift
			done
		)
		if [[ $scenario == network-only ]]; then
			exit 125
		fi
		printf '%s\n' "$container_id" >"$cidfile"
		printf '%s\n' "$container_id" >"$state/container-id"
		printf '%s\n' "$name" >"$state/container-name"
		printf '%s\n' "$label" >"$state/container-label"
		printf '%s\n' "${mounts[@]}" >"$state/container-mounts"
		if [[ $scenario == container-create-term ]]; then
			kill -TERM "$PPID"
		fi
		if [[ $scenario == container-create-id-failure ]]; then
			exit 125
		fi
		printf '%s\n' "$container_id"
		;;
	start)
		if [[ $scenario == create-start-failure ]]; then
			exit 125
		fi
		printf '%s\n' "${!#}"
		;;
	logs)
		[[ ${2-} == --follow && ${3-} == "$container_id" ]] || exit 2
		printf '%s\n' "$BASHPID" >"$state/log-follower-pid"
		trap 'printf "TERM\n" >"$state/log-follower-term"; exit 143' TERM INT
		if [[ $scenario == logs-already-exited ]]; then
			printf '%s\n' exited >"$state/log-follower-exit"
			exit 0
		fi
		while [[ -f $state/container-id ]]; do
			/bin/sleep 0.01
		done
		printf '%s\n' exited >"$state/log-follower-exit"
		;;
	run)
		if [[ $scenario == network-only ]]; then
			exit 125
		fi
		name=$(argument_after --name "$@")
		label=$(argument_after --label "$@")
		mapfile -t mounts < <(
			while (( $# > 0 )); do
				if [[ $1 == --mount && $# -gt 1 ]]; then
					printf '%s\n' "$2"
					shift
				fi
				shift
			done
		)
		printf '%s\n' "$container_id" >"$state/container-id"
		printf '%s\n' "$name" >"$state/container-name"
		printf '%s\n' "$label" >"$state/container-label"
		printf '%s\n' "${mounts[@]}" >"$state/container-mounts"
		printf '%s\n' "$container_id"
		;;
	inspect)
		format=$(argument_after --format "$@")
		target=${!#}
		if [[ $target == preexisting-container ]]; then
			label=$(<"$state/queried-label")
			case $format in
				*Labels*) printf '%s\n' "${label##*=}" ;;
				*Name*) printf '/%s\n' preexisting-container-name ;;
				*Id*) printf '%s\n' preexisting-container ;;
			esac
			exit 0
		fi
		case $format in
			*Labels*) label=$(<"$state/container-label"); printf '%s\n' "${label##*=}" ;;
			*'json .Mounts'*)
				if [[ $scenario == bad-mount ]]; then
					printf '[]\n'
				else
					server=$(sed -n '1s/^.*src=\([^,]*\),dst=.*$/\1/p' "$state/container-mounts")
					data=$(sed -n '2s/^.*src=\([^,]*\),dst=.*$/\1/p' "$state/container-mounts")
					jq -cn --arg server "$server" --arg data "$data" '[
						{"Type":"bind","Source":$server,"Destination":"/mnt/vrising/server"},
						{"Type":"bind","Source":$data,"Destination":"/mnt/vrising/persistentdata"}
					]'
				fi
				;;
			*State.Status*) printf '%s\n' running ;;
			*State.Health*)
			if [[ $scenario == signal || $scenario == signal-int ]]; then printf '%s\n' starting; else printf '%s\n' healthy; fi
				;;
			*State.ExitCode*) printf '%s\n' 0 ;;
			*Name*) printf '/%s\n' "$(<"$state/container-name")" ;;
			*Id*) printf '%s\n' "$(<"$state/container-id")" ;;
		esac
		;;
	rm)
		rm -f -- "$state/container-id" "$state/container-name" "$state/container-label" "$state/container-mounts"
		;;
	exec)
		if [[ $scenario == verify-failure ]]; then
			exit 42
		fi
		;;
	stop | port)
		;;
	*)
		exit 2
		;;
esac
FAKE_DOCKER
chmod 0755 "$fake_bin/docker"

cat >"$fake_bin/sleep" <<'FAKE_SLEEP'
#!/usr/bin/env bash
set -euo pipefail
signal_marker=${FAKE_DOCKER_STATE:?}/signal-sent
if [[ ${FAKE_DOCKER_SCENARIO-} == signal && ! -e $signal_marker ]]; then
	: >"$signal_marker"
	kill -TERM "$PPID"
	exit 143
fi
if [[ ${FAKE_DOCKER_SCENARIO-} == signal-int && ! -e $signal_marker ]]; then
	: >"$signal_marker"
	kill -INT "$PPID"
	exit 130
fi
exit 0
FAKE_SLEEP
chmod 0755 "$fake_bin/sleep"

fake_container_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
fake_network_id=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

run_handoff_boundary_case() {
	local boundary=$1
	local status

	set +e
	LIVE_SCRIPT="$live_script" BOUNDARY="$boundary" /bin/bash -c '
		set -eTuo pipefail
		source "$LIVE_SCRIPT"
		trap handle_term TERM
		case $BOUNDARY in
			begin)
				creation_handoff=false
				deferred_signal=0
				trap '\''if [[ $BASH_COMMAND == "deferred_signal=0" ]]; then trap - DEBUG; kill -TERM $$; fi'\'' DEBUG
				begin_creation_handoff
				;;
			finish)
				creation_handoff=true
				deferred_signal=0
				trap '\''if [[ $BASH_COMMAND == "creation_handoff=false" ]]; then trap - DEBUG; kill -TERM $$; fi'\'' DEBUG
				finish_creation_handoff
				;;
			*) exit 64 ;;
		esac
		exit 0
	' >/dev/null 2>&1
	status=$?
	set -e
	[[ $status -eq 143 ]] || fail "$boundary handoff boundary lost TERM (status $status)"
}

run_handoff_boundary_case begin
run_handoff_boundary_case finish

run_cleanup_case() {
	local scenario=$1
	local expected_status=$2
	local state=$sandbox/state-$scenario
	local controlled_tmp=$sandbox/tmp-$scenario
	local output status

	mkdir -p "$state" "$controlled_tmp"
	set +e
	output=$(PATH="$fake_bin:$PATH" TMPDIR="$controlled_tmp" \
		FAKE_DOCKER_STATE="$state" FAKE_DOCKER_SCENARIO="$scenario" \
		bash "$live_script" fresh fake/image:test 2>&1)
	status=$?
	set -e
	[[ $status -eq $expected_status ]] || fail "$scenario status = $status, want $expected_status: $output"
	printf '%s\n' "$state"
}

container_collision_state=$(run_cleanup_case collision-container 1)
if grep -Eq '^rm .*preexisting-container' "$container_collision_state/calls"; then
	fail 'collision cleanup removed a pre-existing labeled container'
fi

network_collision_state=$(run_cleanup_case collision-network 1)
if grep -Eq '^network rm .*preexisting-network' "$network_collision_state/calls"; then
	fail 'collision cleanup removed a pre-existing labeled network'
fi

start_failure_state=$(run_cleanup_case create-start-failure 1)
grep -Eq '^create .*--cidfile ' "$start_failure_state/calls" \
	|| fail 'container was not created with a durable cidfile before start'
grep -Fq "start $fake_container_id " "$start_failure_state/calls" \
	|| fail 'created container was not started by its recorded ID'
grep -Fq "rm -fv $fake_container_id " "$start_failure_state/calls" \
	|| fail 'start failure did not remove its created container'
grep -Fq "network rm $fake_network_id " "$start_failure_state/calls" \
	|| fail 'start failure did not remove its created network'

container_term_state=$(run_cleanup_case container-create-term 143)
grep -Eq '^create .*--cidfile ' "$container_term_state/calls" \
	|| fail 'container TERM handoff did not use a durable cidfile'
grep -Fq "rm -fv $fake_container_id " "$container_term_state/calls" \
	|| fail 'TERM during container create handoff leaked its recorded container'
grep -Fq "network rm $fake_network_id " "$container_term_state/calls" \
	|| fail 'TERM during container create handoff leaked its recorded network'

network_term_state=$(run_cleanup_case network-create-term 143)
if grep -Eq '^create ' "$network_term_state/calls"; then
	fail 'TERM during network create handoff continued to container creation'
fi
grep -Fq "network rm $fake_network_id " "$network_term_state/calls" \
	|| fail 'TERM during network create handoff leaked its recorded network'

container_id_failure_state=$(run_cleanup_case container-create-id-failure 1)
grep -Fq "rm -fv $fake_container_id " "$container_id_failure_state/calls" \
	|| fail 'nonzero container create after cidfile write leaked its recorded container'
grep -Fq "network rm $fake_network_id " "$container_id_failure_state/calls" \
	|| fail 'nonzero container create after cidfile write leaked its recorded network'

network_id_failure_state=$(run_cleanup_case network-create-id-failure 1)
if grep -Eq '^create ' "$network_id_failure_state/calls"; then
	fail 'nonzero network create after ID write continued to container creation'
fi
grep -Fq "network rm $fake_network_id " "$network_id_failure_state/calls" \
	|| fail 'nonzero network create after ID write leaked its recorded network'

network_only_state=$(run_cleanup_case network-only 1)
grep -Fq "network rm $fake_network_id " "$network_only_state/calls" \
	|| fail 'network-only partial creation did not remove its created network'
if grep -Fq "rm -fv $fake_container_id " "$network_only_state/calls"; then
	fail 'network-only partial creation removed an uncreated container'
fi

container_partial_state=$(run_cleanup_case bad-mount 1)
grep -Fq "rm -fv $fake_container_id " "$container_partial_state/calls" \
	|| fail 'container partial creation did not remove its created container'
grep -Fq "network rm $fake_network_id " "$container_partial_state/calls" \
	|| fail 'container partial creation did not remove its created network'

signal_state=$(run_cleanup_case signal 143)
grep -Fq "rm -fv $fake_container_id " "$signal_state/calls" \
	|| fail 'signal cleanup did not remove its created container'
grep -Fq "network rm $fake_network_id " "$signal_state/calls" \
	|| fail 'signal cleanup did not remove its created network'
[[ -f $signal_state/log-follower-exit && ! -e $signal_state/log-follower-term ]] \
	|| fail 'TERM cleanup did not naturally reap the current log follower'

signal_int_state=$(run_cleanup_case signal-int 130)
grep -Fq "rm -fv $fake_container_id " "$signal_int_state/calls" \
	|| fail 'INT cleanup did not remove its created container'
grep -Fq "network rm $fake_network_id " "$signal_int_state/calls" \
	|| fail 'INT cleanup did not remove its created network'
[[ -f $signal_int_state/log-follower-exit && ! -e $signal_int_state/log-follower-term ]] \
	|| fail 'INT cleanup did not naturally reap the current log follower'

verify_state=$(run_cleanup_case verify-failure 1)
grep -Fq "logs --follow $fake_container_id " "$verify_state/calls" \
	|| fail 'acceptance did not stream the current container logs'
[[ -f $verify_state/log-follower-exit && ! -e $verify_state/log-follower-term ]] \
	|| fail 'failure cleanup did not naturally reap the current log follower'
grep -Eq '^exec .* vrisingctl verify' "$verify_state/calls" \
	|| fail 'healthy acceptance did not invoke the deep live-state verifier'
grep -Fq "rm -fv $fake_container_id " "$verify_state/calls" \
	|| fail 'deep verifier failure did not remove its created container'
grep -Fq "network rm $fake_network_id " "$verify_state/calls" \
	|| fail 'deep verifier failure did not remove its created network'

already_exited_state=$(run_cleanup_case logs-already-exited 1)
[[ -f $already_exited_state/log-follower-exit && ! -e $already_exited_state/log-follower-term ]] \
	|| fail 'already-exited log follower was not reaped without signaling'

make_bin=$sandbox/make-bin
mkdir -p "$make_bin"
cat >"$make_bin/bash" <<'FAKE_BASH'
#!/bin/bash
set -euo pipefail
: >"${ARGV_LOG:?}"
printf '%s\0' "$@" >"$ARGV_LOG"
FAKE_BASH
chmod 0755 "$make_bin/bash"

assert_argv() {
	local file=$1
	shift
	local -a actual=()
	mapfile -d '' -t actual <"$file"
	[[ ${#actual[@]} -eq $# ]] || fail "argv count = ${#actual[@]}, want $#"
	local index=0
	for expected in "$@"; do
		[[ ${actual[index]} == "$expected" ]] || fail "argv[$index] was not preserved exactly"
		index=$((index + 1))
	done
}

fresh_argv=$sandbox/fresh.argv
PATH="$make_bin:$PATH" ARGV_LOG="$fresh_argv" \
	make -s -C "$repo_root" live-acceptance MODE=fresh IMAGE='registry.example/vrising:build-1'
assert_argv "$fresh_argv" hack/live-acceptance.sh fresh 'registry.example/vrising:build-1'

migrate_argv=$sandbox/migrate.argv
migrate_server=$sandbox/'source server;literal'
migrate_data="$sandbox/source data\$(literal)"
PATH="$make_bin:$PATH" ARGV_LOG="$migrate_argv" \
	make -s -C "$repo_root" live-acceptance MODE=migrate IMAGE='registry.example/vrising:build-2' \
	SOURCE_SERVER_DIR="$migrate_server" SOURCE_DATA_DIR="$migrate_data"
assert_argv "$migrate_argv" hack/live-acceptance.sh migrate 'registry.example/vrising:build-2' "$migrate_server" "$migrate_data"

override_marker=$sandbox/internal-override-injection
internal_malicious="\`touch $override_marker\`"
override_fresh_argv=$sandbox/override-fresh.argv
PATH="$make_bin:$PATH" ARGV_LOG="$override_fresh_argv" \
	make -s -C "$repo_root" live-acceptance MODE=fresh IMAGE='registry.example/vrising:expected' \
	LIVE_ACCEPTANCE_MODE=migrate LIVE_ACCEPTANCE_IMAGE="$internal_malicious" \
	LIVE_ACCEPTANCE_SOURCE_SERVER_DIR="$internal_malicious" LIVE_ACCEPTANCE_SOURCE_DATA_DIR="$internal_malicious"
assert_argv "$override_fresh_argv" hack/live-acceptance.sh fresh 'registry.example/vrising:expected'
[[ ! -e $override_marker ]] || fail 'internal Make overrides executed shell source during fresh mapping'

override_migrate_argv=$sandbox/override-migrate.argv
PATH="$make_bin:$PATH" ARGV_LOG="$override_migrate_argv" \
	make -s -C "$repo_root" live-acceptance MODE=migrate IMAGE='registry.example/vrising:expected-2' \
	SOURCE_SERVER_DIR="$migrate_server" SOURCE_DATA_DIR="$migrate_data" \
	LIVE_ACCEPTANCE_MODE=fresh LIVE_ACCEPTANCE_IMAGE="$internal_malicious" \
	LIVE_ACCEPTANCE_SOURCE_SERVER_DIR="$internal_malicious" LIVE_ACCEPTANCE_SOURCE_DATA_DIR="$internal_malicious"
assert_argv "$override_migrate_argv" hack/live-acceptance.sh migrate 'registry.example/vrising:expected-2' "$migrate_server" "$migrate_data"
[[ ! -e $override_marker ]] || fail 'internal Make overrides executed shell source during migrate mapping'

for variable in MODE IMAGE SOURCE_SERVER_DIR SOURCE_DATA_DIR; do
	marker=$sandbox/injection-$variable
	malicious="\`touch $marker\`"
	case $variable in
		MODE)
			set +e
			PATH="$make_bin:$PATH" ARGV_LOG="$sandbox/injection.argv" \
				make -s -C "$repo_root" live-acceptance MODE="$malicious" IMAGE=image:test >/dev/null 2>&1
			set -e
			;;
		IMAGE)
			PATH="$make_bin:$PATH" ARGV_LOG="$sandbox/injection.argv" \
				make -s -C "$repo_root" live-acceptance MODE=fresh IMAGE="$malicious" >/dev/null 2>&1 || true
			;;
		SOURCE_SERVER_DIR)
			PATH="$make_bin:$PATH" ARGV_LOG="$sandbox/injection.argv" \
				make -s -C "$repo_root" live-acceptance MODE=migrate IMAGE=image:test \
				SOURCE_SERVER_DIR="$malicious" SOURCE_DATA_DIR=/safe/data >/dev/null 2>&1 || true
			;;
		SOURCE_DATA_DIR)
			PATH="$make_bin:$PATH" ARGV_LOG="$sandbox/injection.argv" \
				make -s -C "$repo_root" live-acceptance MODE=migrate IMAGE=image:test \
				SOURCE_SERVER_DIR=/safe/server SOURCE_DATA_DIR="$malicious" >/dev/null 2>&1 || true
			;;
	esac
	[[ ! -e $marker ]] || fail "$variable executed shell source from Make data"
done

nosleep_bin=$sandbox/no-sleep-bin
mkdir -p "$nosleep_bin"
for command in awk cmp comm find grep jq mkdir mktemp readlink realpath rm sha256sum sort stat tr; do
	ln -s "$(command -v "$command")" "$nosleep_bin/$command"
done
ln -s "$(command -v bash)" "$nosleep_bin/bash"
ln -s "$fake_bin/docker" "$nosleep_bin/docker"
nosleep_state=$sandbox/state-no-sleep
nosleep_tmp=$sandbox/tmp-no-sleep
mkdir -p "$nosleep_state" "$nosleep_tmp"
set +e
nosleep_output=$(PATH="$nosleep_bin" TMPDIR="$nosleep_tmp" \
	FAKE_DOCKER_STATE="$nosleep_state" FAKE_DOCKER_SCENARIO=bad-mount \
	/bin/bash "$live_script" fresh fake/image:test 2>&1)
nosleep_status=$?
set -e
[[ $nosleep_status -eq 1 && $nosleep_output == *'required command is unavailable: sleep'* ]] \
	|| fail "missing sleep was not rejected during prerequisite checks: $nosleep_output"

(
	# The loaded-save record is captured from real Wine server output.
	# shellcheck source=hack/live-acceptance.sh
	source "$live_script"
	temp_root=$sandbox/save-evidence
	source_data_dir=$temp_root/source
	mkdir -p "$source_data_dir/Settings" "$source_data_dir/Saves/v4/world1"
	printf '{"SaveName":"world1"}\n' >"$source_data_dir/Settings/ServerHostSettings.json"
	printf 'original-save\n' >"$source_data_dir/Saves/v4/world1/AutoSave_0.save.gz"
	created_container_id=fixture
	loaded_name=AutoSave_0.save.gz
	docker() {
		printf '[server] CreateAndHostServer - SaveDirectory:Z:\\mnt\\vrising\\persistentdata\\Saves\\v4\\world1, Loaded Save:%s\n' "$loaded_name"
	}
	assert_loaded_existing_save
	loaded_name='<None>'
	if (assert_loaded_existing_save) >/dev/null 2>&1; then
		fail 'a newly created world passed migration acceptance'
	fi
	loaded_name=AutoSave_99.save.gz
	if (assert_loaded_existing_save) >/dev/null 2>&1; then
		fail 'a save absent from the source passed migration acceptance'
	fi
	snapshot_protected_tree "$source_data_dir" "$temp_root/before" settings
	printf 'game-written-save\n' >"$source_data_dir/Saves/v4/world1/AutoSave_0.save.gz"
	snapshot_protected_tree "$source_data_dir" "$temp_root/after" settings
	assert_snapshot_preserved "$temp_root/before" "$temp_root/after"
	printf '{"SaveName":"wrong-world"}\n' >"$source_data_dir/Settings/ServerHostSettings.json"
	snapshot_protected_tree "$source_data_dir" "$temp_root/after" settings
	if (assert_snapshot_preserved "$temp_root/before" "$temp_root/after") >/dev/null 2>&1; then
		fail 'modified existing settings passed migration acceptance'
	fi
)

printf 'live acceptance helper tests: PASS\n'
