#!/usr/bin/env bash
set -euo pipefail

fixture_image=${FIXTURE_IMAGE:-docker-vrising:fixture-test}
production_image=${PRODUCTION_IMAGE:-docker-vrising:production-test}
test_root=$(mktemp -d "${TMPDIR:-/tmp}/docker-vrising-fixture.XXXXXX")
case $test_root in
  "${TMPDIR:-/tmp}"/docker-vrising-fixture.*) ;;
  *) echo "refusing unsafe temporary root: $test_root" >&2; exit 1 ;;
esac
[[ -d $test_root && ! -L $test_root ]] || { echo "invalid temporary root: $test_root" >&2; exit 1; }

suffix=$RANDOM-$$
network_name=docker-vrising-fixture-$suffix
sidecar_name=docker-vrising-fixture-sidecar-$suffix
controller_name=docker-vrising-fixture-controller-$suffix
server_dir=$test_root/server
data_dir=$test_root/persistentdata
record_dir=$test_root/records
mkdir -p "$server_dir" "$data_dir" "$record_dir"

cleanup() {
  docker rm -f "$controller_name" "$sidecar_name" >/dev/null 2>&1 || true
  docker network rm "$network_name" >/dev/null 2>&1 || true
  if [[ -n ${test_root:-} && -d $test_root && ! -L $test_root ]]; then
    case $test_root in
      "${TMPDIR:-/tmp}"/docker-vrising-fixture.*) rm -rf -- "$test_root" ;;
    esac
  fi
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $*" >&2
  docker logs "$controller_name" >&2 2>/dev/null || true
  docker logs "$sidecar_name" >&2 2>/dev/null || true
  exit 1
}

start_sidecar() {
  docker run -d \
    --name "$sidecar_name" \
    --network "$network_name" \
    --network-alias thunderstore.io \
    --no-healthcheck \
    --mount "type=bind,src=$record_dir,dst=/fixture/records" \
    --env FIXTURE_RECORD_DIR=/fixture/records \
    --entrypoint /usr/local/bin/fixture-sidecar \
    "$fixture_image" >/dev/null
}

start_controller() {
  local network=$1
  shift
  docker rm -f "$controller_name" >/dev/null 2>&1 || true
  docker run -d \
    --name "$controller_name" \
    --network "$network" \
    --health-interval 1s \
    --health-timeout 1s \
    --health-start-period 0s \
    --health-retries 3 \
    --mount "type=bind,src=$server_dir,dst=/mnt/vrising/server" \
    --mount "type=bind,src=$data_dir,dst=/mnt/vrising/persistentdata" \
    --mount "type=bind,src=$record_dir,dst=/fixture/records" \
    --env FIXTURE_RECORD_DIR=/fixture/records \
    --env STARTUP_TIMEOUT=15s \
    --env SHUTDOWN_TIMEOUT=4s \
    "$@" \
    "$fixture_image" >/dev/null
}

wait_healthy() {
  local status health
  for _ in $(seq 1 80); do
    status=$(docker inspect --format '{{.State.Status}}' "$controller_name")
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$controller_name")
    [[ $health == healthy ]] && return 0
    [[ $status == exited || $status == dead ]] && fail "controller exited before becoming healthy"
    sleep 0.25
  done
  fail "controller did not reach Docker healthy"
}

stop_controller() {
  docker stop --time 6 "$controller_name" >/dev/null
  docker rm "$controller_name" >/dev/null
}

managed_digest() {
  find "$server_dir/BepInEx/plugins/saltydk-managed" -type f -print0 | sort -z | xargs -0 sha256sum
}

tree_digest() {
  local tree=$1
  find "$tree" -type f -printf '%P\0' \
    | sort -z \
    | while IFS= read -r -d '' relative; do
        printf '%s  %s\n' "$(sha256sum "$tree/$relative" | cut -d ' ' -f1)" "$relative"
      done \
    | sha256sum \
    | cut -d ' ' -f1
}

docker network create --internal "$network_name" >/dev/null
start_sidecar

echo 'scenario 1/6: empty mounts install exact graph and become healthy'
start_controller "$network_name"
wait_healthy

mapfile -t managed_files < <(find "$server_dir/BepInEx/plugins/saltydk-managed" -maxdepth 1 -type f -printf '%f\n' | sort)
expected_files=(HookDOTS.API.dll KindredCommands.dll NetTopologySuite.dll Satisvampory.dll VampireCommandFramework.dll)
[[ ${managed_files[*]} == "${expected_files[*]}" ]] || fail "managed DLL set is not exact: ${managed_files[*]}"

package_lock=$server_dir/.docker-vrising/package-lock.json
state_file=$server_dir/.docker-vrising/state.json
jq -e '
  (.Roots | length) == 2 and
  [.Roots[] | [.Namespace, .Name, .Version]] == [
    ["odjit", "KindredCommands", "2.5.8"],
    ["Team_GreenEye", "Satisvampory", "1.0.85"]
  ] and
  (.Packages | length) == 5 and
  [.Packages[].Ref | [.Namespace, .Name, .Version]] == [
    ["BepInEx", "BepInExPack_V_Rising", "1.733.2"],
    ["deca", "VampireCommandFramework", "0.10.4"],
    ["odjit", "KindredCommands", "2.5.8"],
    ["cheesasaurus", "HookDOTS_API", "1.1.1"],
    ["Team_GreenEye", "Satisvampory", "1.0.85"]
  ]
' "$package_lock" >/dev/null || fail "package lock is not the exact two-root, five-package graph"
grep -Fq 'Enabled = false' "$server_dir/BepInEx/config/BepInEx.cfg" || fail 'BepInEx console logging was not disabled'
[[ -f $server_dir/VRisingServer.exe ]] || fail 'Steam fake did not install VRisingServer.exe'
[[ $(<"$server_dir/steam_appid.txt") == 1604030 ]] || fail 'Steam fake wrote the wrong steam_appid.txt'
grep -Fq '+app_info_print 1829350' "$record_dir/steamcmd.argv" || fail 'Steam remote inspection argv was not recorded'
grep -Fq '+app_update 1829350 validate +quit' "$record_dir/steamcmd.argv" || fail 'Steam update argv was not recorded'
initial_managed=$(managed_digest)
stop_controller

echo 'scenario 2/6: offline restart is degraded but healthy'
start_controller none
wait_healthy
jq -e '.Runtime.Ready == true and .Runtime.Phase == "ready" and .Runtime.Degraded == true and (.Runtime.Reason | length > 0)' "$state_file" >/dev/null \
  || fail 'offline restart did not record degraded healthy state'
[[ $(managed_digest) == "$initial_managed" ]] || fail 'offline restart changed active managed files'
stop_controller

echo 'scenario 3/6: corrupt cached and downloaded bytes preserve active files'
cache_file=$(find "$server_dir/.docker-vrising/cache" -type f | sort | head -n 1)
[[ -n $cache_file ]] || fail 'mod cache is empty before corruption scenario'
printf 'corrupt cached fixture bytes\n' >"$cache_file"
start_controller "$network_name"
wait_healthy
jq -e '.Runtime.Degraded == true and (.Runtime.Reason | length > 0)' "$state_file" >/dev/null \
  || fail 'corrupt cached archive fallback did not record degradation'
[[ $(managed_digest) == "$initial_managed" ]] || fail 'corrupt cached archive changed active managed files'
stop_controller
rm "$cache_file"
touch "$record_dir/corrupt-downloads"
start_controller "$network_name"
wait_healthy
jq -e '.Runtime.Degraded == true and (.Runtime.Reason | length > 0)' "$state_file" >/dev/null \
  || fail 'corrupt downloaded archive fallback did not record degradation'
[[ $(managed_digest) == "$initial_managed" ]] || fail 'corrupt downloaded archive changed active managed files'
grep -Fq 'GET /package/download/' "$record_dir/sidecar.requests" || fail 'corrupt download endpoint was not exercised'
stop_controller
rm "$record_dir/corrupt-downloads"

echo 'scenario 4/6: interrupted managed apply recovers on restart'
touch "$record_dir/updated-packages"
start_controller "$network_name" \
  --env FIXTURE_KINDRED_VERSION=2.5.9 \
  --env FIXTURE_SATISVAMPORY_VERSION=1.0.86
interrupted=false
for _ in $(seq 1 1000); do
  if jq -e '.Transaction.Phase == "applying"' "$state_file" >/dev/null 2>&1; then
    docker kill --signal KILL "$controller_name" >/dev/null
    interrupted=true
    break
  fi
  sleep 0.01
done
[[ $interrupted == true ]] || fail 'could not observe the managed apply journal before interruption'
docker rm "$controller_name" >/dev/null
start_controller "$network_name" \
  --env MODS_ENABLED=false \
  --env UPDATE_GAME=false
wait_healthy
jq -e '
  .Transaction == null and .Candidate == null and
  .Active.Status == "active" and .Failed.Status == "failed" and
  .Active.LockDigest != .Failed.LockDigest
' "$state_file" >/dev/null || fail 'restart did not clear the interrupted apply journal'
[[ $(managed_digest) == "$initial_managed" ]] || fail 'restart did not restore the known-good managed files'
stop_controller

echo 'scenario 5/6: MODS_ENABLED=false skips roots and preserves mod state'
cache_before=$(tree_digest "$server_dir/.docker-vrising/cache")
generations_before=$(tree_digest "$server_dir/.docker-vrising/generations")
config_before=$(sha256sum "$server_dir/BepInEx/config/BepInEx.cfg" | cut -d ' ' -f1)
requests_before=$(wc -l <"$record_dir/sidecar.requests")
start_controller "$network_name" \
  --env MODS_ENABLED=false \
  --env UPDATE_GAME=false
wait_healthy
requests_after=$(wc -l <"$record_dir/sidecar.requests")
[[ $requests_after == "$requests_before" ]] || fail 'unmodded start contacted a managed root endpoint'
[[ $(tree_digest "$server_dir/.docker-vrising/cache") == "$cache_before" ]] || fail 'unmodded start changed package cache'
[[ $(tree_digest "$server_dir/.docker-vrising/generations") == "$generations_before" ]] || fail 'unmodded start changed generations'
[[ $(sha256sum "$server_dir/BepInEx/config/BepInEx.cfg" | cut -d ' ' -f1) == "$config_before" ]] \
  || fail 'unmodded start changed BepInEx configuration'
tail -n 1 "$record_dir/wine64.env" | grep -Fxq 'WINEDLLOVERRIDES=winhttp=b' \
  || fail 'unmodded start did not select Wine builtin winhttp'

echo 'scenario 6/6: TERM exits within SHUTDOWN_TIMEOUT and reaps children'
started_ms=$(date +%s%3N)
docker kill --signal TERM "$controller_name" >/dev/null
timeout 5 docker wait "$controller_name" >/dev/null || fail 'controller did not exit within SHUTDOWN_TIMEOUT'
elapsed_ms=$(( $(date +%s%3N) - started_ms ))
(( elapsed_ms <= 4000 )) || fail "TERM shutdown took ${elapsed_ms}ms, exceeding SHUTDOWN_TIMEOUT"
[[ $(docker inspect --format '{{.State.Running}}' "$controller_name") == false ]] || fail 'controller remains running after TERM'
[[ $(docker inspect --format '{{.State.Pid}}' "$controller_name") == 0 ]] || fail 'container retains a managed child PID after TERM'
tail -n 1 "$record_dir/wine64.signals" | grep -Fxq TERM || fail 'Wine child did not record TERM'
tail -n 1 "$record_dir/xvfb.signals" | grep -Fxq TERM || fail 'Xvfb child did not record TERM'
[[ ! -e $record_dir/wineserver.argv ]] || fail 'graceful TERM unexpectedly required wineserver escalation'
docker rm "$controller_name" >/dev/null

docker run --rm --entrypoint /bin/sh "$production_image" -c '
  test ! -e /fixture &&
  test ! -e /usr/local/bin/fixture-sidecar &&
  case "$PATH" in *:/fixture/bin|/fixture/bin:*|*:/fixture/bin:*) exit 1;; esac
' || fail 'fixture-only assets leaked into the production target'

echo "container fixture acceptance: PASS (${elapsed_ms}ms TERM shutdown)"
