#!/usr/bin/env bash
# Every public mode intentionally owns cleanup state inside its subshell.
# shellcheck disable=SC2030,SC2031
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
fixture_script=$repo_root/hack/container-fixture-test.sh

fail() {
  echo "container fixture harness test: $*" >&2
  exit 1
}

group_has_owned_token() {
  local owned_pgid=$1 process_token=$2 pid pgid sid
  [[ $owned_pgid =~ ^[1-9][0-9]*$ && -n $process_token ]] || return 1
  while read -r pid pgid sid; do
    [[ $pgid == "$owned_pgid" && $sid == "$owned_pgid" ]] || continue
    if [[ -r /proc/$pid/environ ]] \
      && grep -Fzqx "CONTAINER_FIXTURE_HARNESS_PROCESS_TOKEN=$process_token" \
        "/proc/$pid/environ" 2>/dev/null; then
      return 0
    fi
  done < <(ps -eo pid=,pgid=,sid=)
  return 1
}

terminate_owned_group() {
  local leader=$1 owned_pgid=$2 process_token=$3
  [[ $leader =~ ^[1-9][0-9]*$ && $leader == "$owned_pgid" ]] || return 0
  group_has_owned_token "$owned_pgid" "$process_token" || return 0
  kill -TERM -- "-$owned_pgid" 2>/dev/null || true
  for _ in {1..20}; do
    group_has_owned_token "$owned_pgid" "$process_token" || break
    sleep 0.05
  done
  if group_has_owned_token "$owned_pgid" "$process_token"; then
    kill -KILL -- "-$owned_pgid" 2>/dev/null || true
  fi
}

run_rejection_helper_cases() (
  local elapsed_ms started_ms

  if [[ ${CONTAINER_FIXTURE_HARNESS_INJECT_OUTER_FAILURE-} == 1 ]]; then
    trap '' TERM
    while :; do sleep 1; done
  fi

  # Exercise the function body that the container driver actually uses.
  # shellcheck disable=SC1090
  source <(awk '/^expect_reject\(\)/ { capture=1 } capture { print } capture && /^}/ { exit }' "$fixture_script")

  expect_reject exact expected-token bash -c 'echo expected-token >&2; exit 64' \
    || fail 'exact status 64 and stderr token were rejected'
  if expect_reject wrong-status expected-token bash -c 'echo expected-token >&2; exit 125' >/dev/null 2>&1; then
    fail 'Docker status 125 was accepted as a fake rejection'
  fi
  if expect_reject wrong-stderr expected-token bash -c 'echo unrelated >&2; exit 64' >/dev/null 2>&1; then
    fail 'wrong fake stderr was accepted'
  fi
  if expect_reject success expected-token bash -c 'echo expected-token >&2; exit 0' >/dev/null 2>&1; then
    fail 'successful command was accepted as a fake rejection'
  fi
  if expect_reject crash expected-token bash -c 'echo expected-token >&2; kill -SEGV $$' >/dev/null 2>&1; then
    fail 'crashed command was accepted as a fake rejection'
  fi

  started_ms=$(date +%s%3N)
  if expect_reject timeout expected-token bash -c '
    trap "" TERM
    while :; do sleep 1; done
  ' >/dev/null 2>&1; then
    fail 'timeout was accepted as a fake rejection'
  fi
  elapsed_ms=$(( $(date +%s%3N) - started_ms ))
  (( elapsed_ms < 5500 )) || fail 'rejection helper did not bound a long-lived command'
)

test_rejection_helper() (
  local fixture_tmp_parent=${TMPDIR:-/tmp}
  local group_leader='' group_record observed_pgid observed_pid observed_sid
  local output output_file owned_pgid='' process_token=''
  local sandbox='' status validated_group=false

  # shellcheck disable=SC2317
  cleanup_rejection_test() {
    terminate_owned_group "$group_leader" "$owned_pgid" "$process_token"
    if [[ $group_leader =~ ^[1-9][0-9]*$ ]]; then
      wait "$group_leader" 2>/dev/null || true
    fi
    if [[ -n $sandbox && -d $sandbox && ! -L $sandbox ]]; then
      case $sandbox in
        "$fixture_tmp_parent"/docker-vrising-rejection.*) rm -rf -- "$sandbox" ;;
      esac
    fi
  }
  trap cleanup_rejection_test EXIT

  for command in ps setsid timeout; do
    command -v "$command" >/dev/null 2>&1 || fail "required rejection command is unavailable: $command"
  done
  sandbox=$(mktemp -d "$fixture_tmp_parent/docker-vrising-rejection.XXXXXX")
  case $sandbox in
    "$fixture_tmp_parent"/docker-vrising-rejection.*) ;;
    *) fail "refusing unsafe rejection root: $sandbox" ;;
  esac
  [[ -d $sandbox && ! -L $sandbox ]] || fail "invalid rejection root: $sandbox"

  output_file=$sandbox/output
  process_token=${CONTAINER_FIXTURE_HARNESS_PROCESS_TOKEN:-docker-vrising-rejection-$$-$RANDOM-$RANDOM}
  [[ $process_token =~ ^docker-vrising-rejection-[a-z0-9-]+$ ]] \
    || fail "invalid rejection process token: $process_token"

  CONTAINER_FIXTURE_HARNESS_PROCESS_TOKEN="$process_token" \
    setsid timeout --foreground --kill-after=1s 6s \
    env CONTAINER_FIXTURE_HARNESS_INJECT_OUTER_FAILURE="${CONTAINER_FIXTURE_HARNESS_INJECT_OUTER_FAILURE-}" \
    bash "$0" rejection-inner "$process_token" >"$output_file" 2>&1 &
  group_leader=$!

  for _ in {1..100}; do
    observed_pid=
    observed_pgid=
    observed_sid=
    read -r observed_pid observed_pgid observed_sid \
      < <(ps -o pid=,pgid=,sid= -p "$group_leader" 2>/dev/null || true) || true
    if [[ $observed_pid == "$group_leader" && $observed_pgid == "$group_leader" \
      && $observed_sid == "$group_leader" ]] \
      && group_has_owned_token "$group_leader" "$process_token"; then
      validated_group=true
      break
    fi
    kill -0 "$group_leader" 2>/dev/null || break
    sleep 0.01
  done
  [[ $validated_group == true ]] || fail 'rejection session leader did not establish its exact owned PGID/SID'
  owned_pgid=$group_leader

  group_record=${CONTAINER_FIXTURE_HARNESS_GROUP_RECORD-}
  if [[ -n $group_record ]]; then
    case $group_record in
      "$fixture_tmp_parent"/docker-vrising-group-record.*) ;;
      *) fail "refusing unsafe rejection group record: $group_record" ;;
    esac
    [[ -f $group_record && ! -L $group_record ]] || fail "invalid rejection group record: $group_record"
    printf '%s %s %s\n' "$group_leader" "$owned_pgid" "$process_token" >"$group_record"
  fi

  set +e
  wait "$group_leader"
  status=$?
  set -e
  output=$(<"$output_file")
  if [[ $status -eq 124 || $status -eq 137 ]]; then
    fail "outer hard timeout fired; inner helper lacks a KILL deadline (status $status)"
  fi
  if group_has_owned_token "$owned_pgid" "$process_token"; then
    fail 'rejection helper left unexpected processes in its owned group'
  fi
  group_leader=
  owned_pgid=
  [[ $status -eq 0 ]] || fail "inner rejection test failed with status $status: $output"
)

test_bootstrap_cleanup() (
  local fixture_tmp_parent=${TMPDIR:-/tmp}
  local sandbox='' fake_bin controlled_tmp real_openssl status

  # shellcheck disable=SC2317
  cleanup_harness() {
    if [[ -n $sandbox && -d $sandbox && ! -L $sandbox ]]; then
      case $sandbox in
        "$fixture_tmp_parent"/docker-vrising-harness.*) rm -rf -- "$sandbox" ;;
      esac
    fi
  }
  trap cleanup_harness EXIT

  sandbox=$(mktemp -d "$fixture_tmp_parent/docker-vrising-harness.XXXXXX")
  case $sandbox in
    "$fixture_tmp_parent"/docker-vrising-harness.*) ;;
    *) fail "refusing unsafe bootstrap-cleanup root: $sandbox" ;;
  esac
  [[ -d $sandbox && ! -L $sandbox ]] || fail "invalid bootstrap-cleanup root: $sandbox"
  fake_bin=$sandbox/bin
  controlled_tmp=$sandbox/tmp
  real_openssl=$(command -v openssl)
  mkdir -p "$fake_bin" "$controlled_tmp"

  cat >"$fake_bin/openssl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1-} == rand ]]; then
  echo 'injected openssl rand failure' >&2
  exit 42
fi
exec "${REAL_OPENSSL:?}" "$@"
FAKE
  chmod 0755 "$fake_bin/openssl"

  set +e
  REAL_OPENSSL=$real_openssl TMPDIR=$controlled_tmp PATH=$fake_bin:$PATH \
    bash "$fixture_script" >/dev/null 2>"$sandbox/stderr"
  status=$?
  set -e
  [[ $status -eq 42 ]] || fail "injected bootstrap failure returned $status, want 42"
  grep -Fxq 'injected openssl rand failure' "$sandbox/stderr" \
    || fail 'injected bootstrap stderr was not preserved'
  if find "$controlled_tmp" -mindepth 1 -maxdepth 1 -type d -name 'docker-vrising-fixture.*' -print -quit | grep -q .; then
    fail 'validated fixture root leaked after pre-setup bootstrap failure'
  fi
)

test_docker_cleanup() (
  local fixture_tmp_parent=${TMPDIR:-/tmp}
  local token container='' network='' sandbox='' controlled_tmp output status suite_label='' volume

  # shellcheck disable=SC2317
  cleanup_docker_test() {
    if [[ -n $container ]]; then
      docker rm -fv "$container" >/dev/null 2>&1 || true
    fi
    if [[ -n $network ]]; then
      docker network rm "$network" >/dev/null 2>&1 || true
    fi
    if [[ -n $suite_label ]]; then
      while IFS= read -r volume; do
        if [[ -n $volume ]]; then
          docker volume rm -f "$volume" >/dev/null 2>&1 || true
        fi
      done < <(docker volume ls -q --filter "label=$suite_label")
    fi
    if [[ -n $sandbox && -d $sandbox && ! -L $sandbox ]]; then
      case $sandbox in
        "$fixture_tmp_parent"/docker-vrising-docker-cleanup.*) rm -rf -- "$sandbox" ;;
      esac
    fi
  }
  trap cleanup_docker_test EXIT

  token=${CONTAINER_FIXTURE_SUITE_TOKEN:-vrising-fixture-cleanup-$$}
  [[ $token =~ ^vrising-fixture-[a-z0-9-]+$ ]] || fail "invalid Docker cleanup token: $token"
  container=$token-controller
  network=$token-network
  suite_label=com.saltydk.docker-vrising.fixture-suite=$token
  cleanup_docker_test
  sandbox=$(mktemp -d "$fixture_tmp_parent/docker-vrising-docker-cleanup.XXXXXX")
  case $sandbox in
    "$fixture_tmp_parent"/docker-vrising-docker-cleanup.*) ;;
    *) fail "refusing unsafe Docker cleanup root: $sandbox" ;;
  esac
  [[ -d $sandbox && ! -L $sandbox ]] || fail "invalid Docker cleanup root: $sandbox"
  controlled_tmp=$sandbox/tmp
  mkdir -p "$controlled_tmp"

  if [[ ${CONTAINER_FIXTURE_HARNESS_INJECT_ASSERTION_FAILURE-} == 1 ]]; then
    mkdir -p "$sandbox/injected-server" "$sandbox/injected-persistentdata"
    docker network create --internal \
      --label "$suite_label" "$network" >/dev/null
    docker run -d \
      --name "$container" --label "$suite_label" \
      --network "$network" \
      --mount "type=bind,src=$sandbox/injected-server,dst=/mnt/vrising/server" \
      --mount "type=bind,src=$sandbox/injected-persistentdata,dst=/mnt/vrising/persistentdata" \
      --entrypoint /bin/bash "${FIXTURE_IMAGE:-docker-vrising:fixture-test}" \
      -c 'trap "" TERM; while :; do sleep 1; done' >/dev/null
    fail 'injected Docker cleanup assertion failure'
  fi

  set +e
  output=$(timeout --kill-after=1s 8s env \
    TMPDIR="$controlled_tmp" \
    CONTAINER_FIXTURE_SUITE_TOKEN="$token" \
    CONTAINER_FIXTURE_TEST_MODE=docker-cleanup \
    bash "$fixture_script" 2>&1)
  status=$?
  set -e
  [[ $status -ne 0 ]] || fail 'Docker cleanup child unexpectedly succeeded'
  [[ $status -ne 124 && $status -ne 137 ]] || fail "outer Docker cleanup timeout fired with status $status"
  grep -Eq 'fake rejection docker cleanup timeout: exit status (124|137), want 64' <<<"$output" \
    || fail "Docker cleanup child failed for the wrong reason: $output"
  [[ -z $(docker ps -aq --filter "label=com.saltydk.docker-vrising.fixture-suite=$token") ]] \
    || fail 'scoped Docker cleanup container remains'
  [[ -z $(docker network ls -q --filter "label=com.saltydk.docker-vrising.fixture-suite=$token") ]] \
    || fail 'scoped Docker cleanup network remains'
  [[ -z $(docker volume ls -q --filter "label=com.saltydk.docker-vrising.fixture-suite=$token") ]] \
    || fail 'scoped Docker cleanup volume remains'
  if find "$controlled_tmp" -mindepth 1 -maxdepth 1 -type d -name 'docker-vrising-fixture.*' -print -quit | grep -q .; then
    fail 'Docker cleanup child left its fixture root'
  fi
)

test_failure_cleanup() (
  local fixture_tmp_parent=${TMPDIR:-/tmp}
  local container='' controlled_tmp docker_output group_leader='' group_record='' network='' owned_pgid=''
  local process_token='' recorded_token='' rejection_output sandbox='' status token='' volume

  # shellcheck disable=SC2317
  cleanup_failure_test() {
    if [[ -n $group_record && -s $group_record && ! -L $group_record ]]; then
      read -r group_leader owned_pgid recorded_token <"$group_record" || true
      if [[ $recorded_token == "$process_token" ]]; then
        terminate_owned_group "$group_leader" "$owned_pgid" "$process_token"
      fi
    fi
    if [[ -n $container ]]; then
      docker rm -fv "$container" >/dev/null 2>&1 || true
    fi
    if [[ -n $network ]]; then
      docker network rm "$network" >/dev/null 2>&1 || true
    fi
    if [[ -n $token ]]; then
      while IFS= read -r volume; do
        if [[ -n $volume ]]; then
          docker volume rm -f "$volume" >/dev/null 2>&1 || true
        fi
      done < <(docker volume ls -q --filter "label=com.saltydk.docker-vrising.fixture-suite=$token")
    fi
    if [[ -n $sandbox && -d $sandbox && ! -L $sandbox ]]; then
      case $sandbox in
        "$fixture_tmp_parent"/docker-vrising-failure-cleanup.*) rm -rf -- "$sandbox" ;;
      esac
    fi
  }
  trap cleanup_failure_test EXIT

  token=vrising-fixture-failure-cleanup-$$
  [[ $token =~ ^vrising-fixture-[a-z0-9-]+$ ]] || fail "invalid failure-cleanup token: $token"
  container=$token-controller
  network=$token-network
  sandbox=$(mktemp -d "$fixture_tmp_parent/docker-vrising-failure-cleanup.XXXXXX")
  case $sandbox in
    "$fixture_tmp_parent"/docker-vrising-failure-cleanup.*) ;;
    *) fail "refusing unsafe failure-cleanup root: $sandbox" ;;
  esac
  [[ -d $sandbox && ! -L $sandbox ]] || fail "invalid failure-cleanup root: $sandbox"
  controlled_tmp=$sandbox/tmp
  mkdir -p "$controlled_tmp"

  set +e
  docker_output=$(timeout --kill-after=1s 8s env \
    TMPDIR="$controlled_tmp" \
    CONTAINER_FIXTURE_SUITE_TOKEN="$token" \
    CONTAINER_FIXTURE_HARNESS_INJECT_ASSERTION_FAILURE=1 \
    bash "$0" docker-cleanup 2>&1)
  status=$?
  set -e
  [[ $status -eq 1 ]] || fail "injected Docker cleanup failure returned $status, want 1: $docker_output"
  grep -Fxq 'container fixture harness test: injected Docker cleanup assertion failure' <<<"$docker_output" \
    || fail "injected Docker cleanup failure was not observed: $docker_output"
  [[ -z $(docker ps -aq --filter "label=com.saltydk.docker-vrising.fixture-suite=$token") ]] \
    || fail 'injected failure left its scoped Docker container'
  [[ -z $(docker network ls -q --filter "label=com.saltydk.docker-vrising.fixture-suite=$token") ]] \
    || fail 'injected failure left its scoped Docker network'
  [[ -z $(docker volume ls -q --filter "label=com.saltydk.docker-vrising.fixture-suite=$token") ]] \
    || fail 'injected failure left its scoped Docker volume'
  if find "$controlled_tmp" -mindepth 1 -maxdepth 1 -type d -name 'docker-vrising-docker-cleanup.*' -print -quit | grep -q .; then
    fail 'injected failure left its Docker cleanup root'
  fi

  process_token=docker-vrising-rejection-failure-$$-$RANDOM-$RANDOM
  group_record=$(mktemp "$controlled_tmp/docker-vrising-group-record.XXXXXX")
  set +e
  rejection_output=$(timeout --kill-after=1s 10s env \
    TMPDIR="$controlled_tmp" \
    CONTAINER_FIXTURE_HARNESS_PROCESS_TOKEN="$process_token" \
    CONTAINER_FIXTURE_HARNESS_GROUP_RECORD="$group_record" \
    CONTAINER_FIXTURE_HARNESS_INJECT_OUTER_FAILURE=1 \
    bash "$0" rejection 2>&1)
  status=$?
  set -e
  [[ $status -eq 1 ]] || fail "injected rejection outer failure returned $status, want 1: $rejection_output"
  grep -Eq 'container fixture harness test: outer hard timeout fired; inner helper lacks a KILL deadline \(status (124|137)\)' \
    <<<"$rejection_output" \
    || fail "injected rejection outer failure was not observed: $rejection_output"
  read -r group_leader owned_pgid recorded_token <"$group_record"
  [[ $group_leader =~ ^[1-9][0-9]*$ && $group_leader == "$owned_pgid" \
    && $recorded_token == "$process_token" ]] \
    || fail 'injected rejection group record was not exact'
  if group_has_owned_token "$owned_pgid" "$process_token"; then
    fail 'injected rejection outer failure left its owned process group'
  fi
  if find "$controlled_tmp" -mindepth 1 -maxdepth 1 -type d -name 'docker-vrising-rejection.*' -print -quit | grep -q .; then
    fail 'injected rejection outer failure left its rejection root'
  fi
)

case ${1-} in
  rejection) test_rejection_helper ;;
  rejection-inner) run_rejection_helper_cases ;;
  cleanup) test_bootstrap_cleanup ;;
  docker-cleanup) test_docker_cleanup ;;
  failure-cleanup) test_failure_cleanup ;;
  *) fail 'usage: container-fixture-harness-test.sh rejection|cleanup|docker-cleanup|failure-cleanup' ;;
esac

echo "container fixture harness test: PASS ($1)"
