#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
fixture_script=$repo_root/hack/container-fixture-test.sh

fail() {
  echo "container fixture harness test: $*" >&2
  exit 1
}

run_rejection_helper_cases() {
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

  started=$SECONDS
  # The child shell, not this shell, expands its PID-file environment.
  # shellcheck disable=SC2016
  if expect_reject timeout expected-token bash -c '
    printf "%s\n" "$$" >"${TERM_IGNORE_PID_FILE:?}"
    trap "" TERM
    while :; do sleep 1; done
  ' >/dev/null 2>&1; then
    fail 'timeout was accepted as a fake rejection'
  fi
  (( SECONDS - started < 5 )) || fail 'rejection helper did not bound a long-lived command'
}

test_rejection_helper() {
  local child_pid inner_timeout_pid output pid_file status
  pid_file=$(mktemp "${TMPDIR:-/tmp}/docker-vrising-term-ignore.XXXXXX")
  set +e
  output=$(TERM_IGNORE_PID_FILE=$pid_file timeout --kill-after=1s 6s bash "$0" rejection-inner 2>&1)
  status=$?
  set -e
  if [[ -s $pid_file ]]; then
    child_pid=$(<"$pid_file")
    inner_timeout_pid=$(ps -o ppid= -p "$child_pid" 2>/dev/null | tr -d ' ' || true)
    if [[ $child_pid =~ ^[0-9]+$ ]]; then
      kill -KILL "$child_pid" 2>/dev/null || true
    fi
    if [[ $inner_timeout_pid =~ ^[0-9]+$ ]]; then
      kill -KILL "$inner_timeout_pid" 2>/dev/null || true
    fi
  fi
  rm -f -- "$pid_file"
  if [[ $status -eq 124 || $status -eq 137 ]]; then
    fail "outer hard timeout fired; inner helper lacks a KILL deadline (status $status)"
  fi
  [[ $status -eq 0 ]] || fail "inner rejection test failed with status $status: $output"
}

test_bootstrap_cleanup() {
  local sandbox fake_bin controlled_tmp real_openssl status
  sandbox=$(mktemp -d "${TMPDIR:-/tmp}/docker-vrising-harness.XXXXXX")
  fake_bin=$sandbox/bin
  controlled_tmp=$sandbox/tmp
  real_openssl=$(command -v openssl)
  mkdir -p "$fake_bin" "$controlled_tmp"
  cleanup_harness() {
    rm -rf -- "$sandbox"
  }
  trap cleanup_harness RETURN

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
}

test_docker_cleanup() {
  local token container network sandbox controlled_tmp output status
  token=vrising-fixture-cleanup-$$
  container=$token-controller
  network=$token-network
  sandbox=$(mktemp -d "${TMPDIR:-/tmp}/docker-vrising-docker-cleanup.XXXXXX")
  controlled_tmp=$sandbox/tmp
  mkdir -p "$controlled_tmp"

  cleanup_docker_test() {
    local volume
    docker rm -fv "$container" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
    while IFS= read -r volume; do
      if [[ $volume == "$token"* ]]; then
        docker volume rm -f "$volume" >/dev/null 2>&1 || true
      fi
    done < <(docker volume ls -q --filter "name=$token")
    rm -rf -- "$sandbox"
  }
  trap cleanup_docker_test RETURN
  cleanup_docker_test
  mkdir -p "$controlled_tmp"

  set +e
  output=$(timeout --kill-after=1s 8s env \
    TMPDIR="$controlled_tmp" \
    CONTAINER_FIXTURE_SUITE_TOKEN=$token \
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
}

case ${1-} in
  rejection) test_rejection_helper ;;
  rejection-inner) run_rejection_helper_cases ;;
  cleanup) test_bootstrap_cleanup ;;
  docker-cleanup) test_docker_cleanup ;;
  *) fail 'usage: container-fixture-harness-test.sh rejection|cleanup|docker-cleanup' ;;
esac

echo "container fixture harness test: PASS ($1)"
