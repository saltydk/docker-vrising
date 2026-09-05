#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
fixture_script=$repo_root/hack/container-fixture-test.sh

fail() {
  echo "container fixture harness test: $*" >&2
  exit 1
}

test_rejection_helper() {
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

  started=$SECONDS
  if expect_reject timeout expected-token bash -c 'sleep 30' >/dev/null 2>&1; then
    fail 'timeout was accepted as a fake rejection'
  fi
  (( SECONDS - started < 5 )) || fail 'rejection helper did not bound a long-lived command'
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

case ${1-} in
  rejection) test_rejection_helper ;;
  cleanup) test_bootstrap_cleanup ;;
  *) fail 'usage: container-fixture-harness-test.sh rejection|cleanup' ;;
esac

echo "container fixture harness test: PASS ($1)"
