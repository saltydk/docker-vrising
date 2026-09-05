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
				printf '%s\n' fake-network-id >"$state/network-id"
				printf '%s\n' "$name" >"$state/network-name"
				printf '%s\n' "$label" >"$state/network-label"
				printf '%s\n' fake-network-id
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
		printf '%s\n' fake-container-id >"$state/container-id"
		printf '%s\n' "$name" >"$state/container-name"
		printf '%s\n' "$label" >"$state/container-label"
		printf '%s\n' "${mounts[@]}" >"$state/container-mounts"
		printf '%s\n' fake-container-id
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
				if [[ $scenario == signal ]]; then printf '%s\n' starting; else printf '%s\n' healthy; fi
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
if [[ ${FAKE_DOCKER_SCENARIO-} == signal ]]; then
	kill -TERM "$PPID"
	exit 143
fi
exit 0
FAKE_SLEEP
chmod 0755 "$fake_bin/sleep"

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

network_only_state=$(run_cleanup_case network-only 1)
grep -Eq '^network rm .*fake-network-id' "$network_only_state/calls" \
	|| fail 'network-only partial creation did not remove its created network'
if grep -Eq '^rm .*fake-container-id' "$network_only_state/calls"; then
	fail 'network-only partial creation removed an uncreated container'
fi

container_partial_state=$(run_cleanup_case bad-mount 1)
grep -Eq '^rm .*fake-container-id' "$container_partial_state/calls" \
	|| fail 'container partial creation did not remove its created container'
grep -Eq '^network rm .*fake-network-id' "$container_partial_state/calls" \
	|| fail 'container partial creation did not remove its created network'

signal_state=$(run_cleanup_case signal 143)
grep -Eq '^rm .*fake-container-id' "$signal_state/calls" \
	|| fail 'signal cleanup did not remove its created container'
grep -Eq '^network rm .*fake-network-id' "$signal_state/calls" \
	|| fail 'signal cleanup did not remove its created network'

verify_state=$(run_cleanup_case verify-failure 1)
grep -Eq '^exec .* vrisingctl verify' "$verify_state/calls" \
	|| fail 'healthy acceptance did not invoke the deep live-state verifier'
grep -Eq '^rm .*fake-container-id' "$verify_state/calls" \
	|| fail 'deep verifier failure did not remove its created container'
grep -Eq '^network rm .*fake-network-id' "$verify_state/calls" \
	|| fail 'deep verifier failure did not remove its created network'

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

printf 'live acceptance helper tests: PASS\n'
