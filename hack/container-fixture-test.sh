#!/usr/bin/env bash
set -euo pipefail

fixture_image=${FIXTURE_IMAGE:-docker-vrising:fixture-test}
production_image=${PRODUCTION_IMAGE:-docker-vrising:production-test}
default_image=${DEFAULT_IMAGE:-docker-vrising:default-test}
for command in docker jq openssl sha256sum stat timeout; do
  command -v "$command" >/dev/null 2>&1 || { echo "required command is unavailable: $command" >&2; exit 1; }
done

fixture_tmp_parent=${TMPDIR:-/tmp}
test_root=
suite_token=
suite_label=
network_name=
sidecar_name=
controller_name=

cleanup() {
  if [[ -n ${controller_name:-} ]]; then
    docker rm -fv "$controller_name" >/dev/null 2>&1 || true
  fi
  if [[ -n ${sidecar_name:-} ]]; then
    docker rm -fv "$sidecar_name" >/dev/null 2>&1 || true
  fi
  if [[ -n ${suite_label:-} ]]; then
    local -a labeled_containers=()
    mapfile -t labeled_containers < <(docker ps -aq --filter "label=$suite_label" 2>/dev/null || true)
    if (( ${#labeled_containers[@]} > 0 )); then
      docker rm -fv "${labeled_containers[@]}" >/dev/null 2>&1 || true
    fi
  fi
  if [[ -n ${network_name:-} ]]; then
    docker network rm "$network_name" >/dev/null 2>&1 || true
  fi
  if [[ -n ${test_root:-} && -d $test_root && ! -L $test_root ]]; then
    case $test_root in
      "$fixture_tmp_parent"/docker-vrising-fixture.*) rm -rf -- "$test_root" ;;
    esac
  fi
}

test_root=$(mktemp -d "$fixture_tmp_parent/docker-vrising-fixture.XXXXXX")
case $test_root in
  "$fixture_tmp_parent"/docker-vrising-fixture.*) ;;
  *) echo "refusing unsafe temporary root: $test_root" >&2; exit 1 ;;
esac
[[ -d $test_root && ! -L $test_root ]] || { echo "invalid temporary root: $test_root" >&2; exit 1; }
trap cleanup EXIT INT TERM

suite_token=vrising-fixture-$(openssl rand -hex 8)
label_key=com.saltydk.docker-vrising.fixture-suite
suite_label=$label_key=$suite_token
network_name=$suite_token-network
sidecar_name=$suite_token-sidecar
controller_name=$suite_token-controller
server_dir=$test_root/server
data_dir=$test_root/persistentdata
record_dir=$test_root/records
tls_dir=$test_root/tls
probe_server_dir=$test_root/probe-server
probe_data_dir=$test_root/probe-persistentdata
mkdir -p "$server_dir" "$data_dir" "$record_dir" "$tls_dir" \
  "$probe_server_dir/.docker-vrising/home" "$probe_data_dir"

fail() {
  echo "FAIL: $*" >&2
  docker logs "$controller_name" >&2 2>/dev/null || true
  docker logs "$sidecar_name" >&2 2>/dev/null || true
  exit 1
}

assert_zero_suite_resources() {
  [[ -z $(docker ps -aq --filter "label=$suite_label") ]] || fail 'fixture containers remain'
  [[ -z $(docker network ls -q --filter "label=$suite_label") ]] || fail 'fixture networks remain'
  [[ -z $(docker volume ls -q --filter "label=$suite_label") ]] || fail 'labeled fixture volumes remain'
  [[ -z $(docker volume ls -q --filter "name=$suite_token") ]] || fail 'named fixture volumes remain'
}

assert_bind_mounts() {
  local container=$1
  docker inspect "$container" | jq -e '
    .[0].Mounts as $mounts |
    ($mounts | all(.Type == "bind")) and
    ($mounts | map(select(.Destination == "/mnt/vrising/server" and .Type == "bind")) | length == 1) and
    ($mounts | map(select(.Destination == "/mnt/vrising/persistentdata" and .Type == "bind")) | length == 1)
  ' >/dev/null || fail "$container has an inherited anonymous/named volume"
}

generate_tls_fixture() {
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -sha256 \
    -subj /CN=docker-vrising-fixture-ca \
    -addext 'basicConstraints=critical,CA:TRUE' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -keyout "$tls_dir/ca.key" -out "$tls_dir/ca.crt" >/dev/null 2>&1
  openssl req -newkey rsa:2048 -nodes -sha256 \
    -subj /CN=thunderstore.io \
    -addext 'subjectAltName=DNS:thunderstore.io' \
    -addext 'extendedKeyUsage=serverAuth' \
    -keyout "$tls_dir/server.key" -out "$tls_dir/server.csr" >/dev/null 2>&1
  openssl x509 -req -days 30 -sha256 -copy_extensions copy \
    -in "$tls_dir/server.csr" -CA "$tls_dir/ca.crt" -CAkey "$tls_dir/ca.key" \
    -CAcreateserial -out "$tls_dir/server.crt" >/dev/null 2>&1
  chmod 0600 "$tls_dir/ca.key" "$tls_dir/server.key"
}

validate_tls_fixture() {
  openssl verify -CAfile "$tls_dir/ca.crt" "$tls_dir/server.crt" >/dev/null || fail 'fixture TLS chain is invalid'
  openssl x509 -in "$tls_dir/server.crt" -checkhost thunderstore.io -noout >/dev/null \
    || fail 'fixture TLS certificate does not identify thunderstore.io'
  openssl x509 -in "$tls_dir/server.crt" -checkend 604800 -noout >/dev/null \
    || fail 'fixture TLS certificate has less than seven days remaining'
  [[ -s $tls_dir/ca.key && -s $tls_dir/server.key ]] || fail 'fixture TLS private keys are missing'
}

expect_reject() {
  local description=$1 expected_stderr=$2 stderr status
  shift 2
  set +e
  stderr=$(timeout --foreground 3s "$@" 2>&1 >/dev/null)
  status=$?
  set -e
  if [[ $status -ne 64 ]]; then
    echo "fake rejection $description: exit status $status, want 64" >&2
    return 1
  fi
  if [[ $stderr != "$expected_stderr" ]]; then
    echo "fake rejection $description: stderr '$stderr', want '$expected_stderr'" >&2
    return 1
  fi
  return 0
}

probe_mounts=(
  --label "$suite_label"
  --mount "type=bind,src=$probe_server_dir,dst=/mnt/vrising/server"
  --mount "type=bind,src=$probe_data_dir,dst=/mnt/vrising/persistentdata"
  --mount "type=bind,src=$record_dir,dst=/fixture/records"
  --mount "type=bind,src=$tls_dir,dst=/fixture/tls,readonly"
)

run_strict_fake_probes() {
  local home=/mnt/vrising/server/.docker-vrising/home
  local token=$suite_token-probe
  local log=/mnt/vrising/persistentdata/logs/VRisingServer-20260905T120000.000000000Z-000001.log
  local steam=(+@sSteamCmdForcePlatformType windows +login anonymous +app_info_update 1 +app_info_print 1829350 +quit)
  local wine=(VRisingServer.exe -persistentDataPath /mnt/vrising/persistentdata -logFile "$log")

  expect_reject 'steamcmd extra argument' 'fake steamcmd: unexpected argv' docker run --rm --name "$suite_token-probe-steam-extra" "${probe_mounts[@]}" \
    --workdir "$home" --env HOME="$home" --entrypoint /fixture/bin/steamcmd "$fixture_image" "${steam[@]}" unexpected
  expect_reject 'steamcmd missing argument' 'fake steamcmd: unexpected argv' docker run --rm --name "$suite_token-probe-steam-missing" "${probe_mounts[@]}" \
    --workdir "$home" --env HOME="$home" --entrypoint /fixture/bin/steamcmd "$fixture_image" "${steam[@]:0:8}"
  expect_reject 'steamcmd reordered arguments' 'fake steamcmd: unexpected argv' docker run --rm --name "$suite_token-probe-steam-order" "${probe_mounts[@]}" \
    --workdir "$home" --env HOME="$home" --entrypoint /fixture/bin/steamcmd "$fixture_image" \
    +login anonymous +@sSteamCmdForcePlatformType windows +app_info_update 1 +app_info_print 1829350 +quit
  expect_reject 'steamcmd wrong HOME' 'fake steamcmd: unexpected HOME: /tmp' docker run --rm --name "$suite_token-probe-steam-env" "${probe_mounts[@]}" \
    --workdir "$home" --env HOME=/tmp --entrypoint /fixture/bin/steamcmd "$fixture_image" "${steam[@]}"

  local wine_env=(
    --env FIXTURE_RECORD_DIR=/fixture/records --env FIXTURE_RUN_TOKEN="$token"
    --env HOME="$home" --env WINEPREFIX=/mnt/vrising/server/.docker-vrising/wineprefix
    --env DISPLAY=:99 --env 'WINEDLLOVERRIDES=winhttp=n,b'
  )
  expect_reject 'wine64 duplicate argument' 'fake wine64: unexpected argument count' docker run --rm --name "$suite_token-probe-wine-duplicate" "${probe_mounts[@]}" \
    --workdir /mnt/vrising/server "${wine_env[@]}" --entrypoint /fixture/bin/wine64 "$fixture_image" \
    "${wine[@]}" -logFile "$log"
  expect_reject 'wine64 wrong cwd' 'fake wine64: unexpected cwd: /' docker run --rm --name "$suite_token-probe-wine-cwd" "${probe_mounts[@]}" \
    --workdir / "${wine_env[@]}" --entrypoint /fixture/bin/wine64 "$fixture_image" "${wine[@]}"
  expect_reject 'wine64 wrong environment' 'fake wine64: unexpected DISPLAY' docker run --rm --name "$suite_token-probe-wine-env" "${probe_mounts[@]}" \
    --workdir /mnt/vrising/server "${wine_env[@]}" --env DISPLAY=:98 \
    --entrypoint /fixture/bin/wine64 "$fixture_image" "${wine[@]}"

  expect_reject 'Xvfb reordered arguments' 'fake Xvfb: unexpected argv' docker run --rm --name "$suite_token-probe-xvfb-order" "${probe_mounts[@]}" \
    --workdir / --env FIXTURE_RECORD_DIR=/fixture/records --env FIXTURE_RUN_TOKEN="$token" \
    --entrypoint /fixture/bin/Xvfb "$fixture_image" -screen 0 1024x768x24 -displayfd 3 -nolisten tcp
  expect_reject 'Xvfb wrong environment' 'fake Xvfb: unexpected record directory' docker run --rm --name "$suite_token-probe-xvfb-env" "${probe_mounts[@]}" \
    --workdir / --env FIXTURE_RECORD_DIR=/wrong --env FIXTURE_RUN_TOKEN="$token" \
    --entrypoint /fixture/bin/Xvfb "$fixture_image" -displayfd 3 -screen 0 1024x768x24 -nolisten tcp

  expect_reject 'wineserver extra argument' 'fake wineserver: unexpected argv' docker run --rm --name "$suite_token-probe-wineserver-extra" "${probe_mounts[@]}" \
    --workdir / "${wine_env[@]}" --entrypoint /fixture/bin/wineserver "$fixture_image" -k extra
  expect_reject 'wineserver wrong environment' 'fake wineserver: unexpected WINEPREFIX' docker run --rm --name "$suite_token-probe-wineserver-env" "${probe_mounts[@]}" \
    --workdir / "${wine_env[@]}" --env WINEPREFIX=/wrong --entrypoint /fixture/bin/wineserver "$fixture_image" -k
  docker run --rm --name "$suite_token-probe-wineserver-valid" "${probe_mounts[@]}" \
    --workdir / "${wine_env[@]}" --entrypoint /fixture/bin/wineserver "$fixture_image" -k >/dev/null
}

wait_sidecar_ready() {
  local token=$1 marker=$record_dir/sidecar-ready.$1
  for _ in $(seq 1 100); do
    if [[ -f $marker && $(<"$marker") == "$token" ]]; then
      return 0
    fi
    [[ $(docker inspect --format '{{.State.Running}}' "$sidecar_name" 2>/dev/null) == true ]] \
      || fail 'fixture sidecar exited before TLS readiness'
    sleep 0.05
  done
  fail 'fixture sidecar did not publish its exact TLS-listener readiness token'
}

start_sidecar() {
  local token=$suite_token-sidecar
  docker run -d \
    --name "$sidecar_name" --label "$suite_label" \
    --network "$network_name" --network-alias thunderstore.io --no-healthcheck \
    --mount "type=bind,src=$probe_server_dir,dst=/mnt/vrising/server" \
    --mount "type=bind,src=$probe_data_dir,dst=/mnt/vrising/persistentdata" \
    --mount "type=bind,src=$record_dir,dst=/fixture/records" \
    --mount "type=bind,src=$tls_dir,dst=/fixture/tls,readonly" \
    --env FIXTURE_RECORD_DIR=/fixture/records --env FIXTURE_RUN_TOKEN="$token" \
    --entrypoint /usr/local/bin/fixture-sidecar "$fixture_image" serve >/dev/null
  assert_bind_mounts "$sidecar_name"
  wait_sidecar_ready "$token"
}

run_counter=0
current_run_token=
start_controller() {
  local network=$1
  shift
  docker rm -fv "$controller_name" >/dev/null 2>&1 || true
  run_counter=$((run_counter + 1))
  current_run_token=$suite_token-run-$run_counter
  docker run -d \
    --name "$controller_name" --label "$suite_label" --network "$network" \
    --health-interval 1s --health-timeout 1s --health-start-period 0s --health-retries 3 \
    --mount "type=bind,src=$server_dir,dst=/mnt/vrising/server" \
    --mount "type=bind,src=$data_dir,dst=/mnt/vrising/persistentdata" \
    --mount "type=bind,src=$record_dir,dst=/fixture/records" \
    --mount "type=bind,src=$tls_dir,dst=/fixture/tls,readonly" \
    --env FIXTURE_RECORD_DIR=/fixture/records --env FIXTURE_RUN_TOKEN="$current_run_token" \
    --env SSL_CERT_FILE=/fixture/tls/ca.crt \
    --env STARTUP_TIMEOUT=15s --env SHUTDOWN_TIMEOUT=4s \
    "$@" "$fixture_image" >/dev/null
  assert_bind_mounts "$controller_name"
}

wait_healthy() {
  local status health
  for _ in $(seq 1 80); do
    status=$(docker inspect --format '{{.State.Status}}' "$controller_name")
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$controller_name")
    [[ $health == healthy ]] && return 0
    [[ $status == exited || $status == dead ]] && fail 'controller exited before becoming healthy'
    sleep 0.25
  done
  fail 'controller did not reach Docker healthy'
}

assert_run_started() {
  local run_dir=$record_dir/runs/$1
  [[ -s $run_dir/wine.pid && -s $run_dir/wine.argv && -s $run_dir/wine.env ]] || fail "Wine evidence missing for $1"
  [[ -s $run_dir/xvfb.pid && -s $run_dir/xvfb.argv && -s $run_dir/xvfb.env ]] || fail "Xvfb evidence missing for $1"
}

assert_current_run_reaped() {
  local token=$1 run_dir=$record_dir/runs/$1 wine_pid xvfb_pid
  wine_pid=$(<"$run_dir/wine.pid")
  xvfb_pid=$(<"$run_dir/xvfb.pid")
  [[ $(<"$run_dir/wine.term") == "token=$token pid=$wine_pid signal=TERM" ]] || fail "Wine TERM evidence mismatch for $token"
  [[ $(<"$run_dir/wine.exit") == "token=$token pid=$wine_pid status=0" ]] || fail "Wine exit evidence mismatch for $token"
  [[ $(<"$run_dir/xvfb.term") == "token=$token pid=$xvfb_pid signal=TERM" ]] || fail "Xvfb TERM evidence mismatch for $token"
  [[ $(<"$run_dir/xvfb.exit") == "token=$token pid=$xvfb_pid status=0" ]] || fail "Xvfb exit evidence mismatch for $token"
}

stop_controller() {
  local token=$current_run_token
  docker stop --time 6 "$controller_name" >/dev/null
  assert_current_run_reaped "$token"
  docker rm -fv "$controller_name" >/dev/null
}

capture_active_identity() {
  local destination=$1 active_id manifest inventory_jsonl inventory_json
  active_id=$(jq -er '.Active.ID' "$server_dir/.docker-vrising/state.json")
  manifest=$server_dir/.docker-vrising/generations/$active_id/.metadata/manifest.json
  inventory_jsonl=$test_root/inventory.jsonl
  inventory_json=$test_root/inventory.json
  : >"$inventory_jsonl"
  jq -e --arg id "$active_id" '.GenerationID == $id and (.Files | length > 0)' "$manifest" >/dev/null \
    || fail 'active manifest does not match active generation'
  while IFS=$'\t' read -r relative expected_sha expected_mode; do
    local actual_sha actual_mode
    actual_sha=$(sha256sum "$server_dir/$relative" | cut -d ' ' -f1)
    actual_mode=$((8#$(stat -c '%a' "$server_dir/$relative")))
    [[ $actual_sha == "$expected_sha" && $actual_mode -eq $expected_mode ]] \
      || fail "live managed identity mismatch: $relative"
    jq -cn --arg path "$relative" --arg sha "$actual_sha" --argjson mode "$actual_mode" \
      '{path: $path, sha256: $sha, mode: $mode}' >>"$inventory_jsonl"
  done < <(jq -r '.Files[] | [.RelativePath, .SHA256, .Mode] | @tsv' "$manifest")
  jq -s '.' "$inventory_jsonl" >"$inventory_json"
  jq -S -n \
    --slurpfile state "$server_dir/.docker-vrising/state.json" \
    --slurpfile manifest "$manifest" \
    --slurpfile lock "$server_dir/.docker-vrising/package-lock.json" \
    --slurpfile inventory "$inventory_json" \
    '{active: $state[0].Active, manifest: $manifest[0], package_lock: $lock[0], inventory: $inventory[0]}' \
    >"$destination"
}

identity_sequence=0
assert_identity_unchanged() {
  local baseline=$1 description=$2 actual
  identity_sequence=$((identity_sequence + 1))
  actual=$record_dir/identity-$identity_sequence.json
  capture_active_identity "$actual"
  cmp -s "$baseline" "$actual" || fail "$description changed complete active identity"
}

seed_sentinels() {
  mkdir -p "$data_dir/Saves/v1" "$data_dir/Settings" "$server_dir/manual"
  printf 'fixture save sentinel\n' >"$data_dir/Saves/v1/fixture-save.dat"
  printf '{"fixture":"settings sentinel"}\n' >"$data_dir/Settings/ServerGameSettings.json"
  printf 'fixture unmanaged server sentinel\n' >"$server_dir/manual/unmanaged.txt"
  sha256sum \
    "$data_dir/Saves/v1/fixture-save.dat" \
    "$data_dir/Settings/ServerGameSettings.json" \
    "$server_dir/manual/unmanaged.txt" >"$record_dir/sentinels.expected"
}

assert_sentinels() {
  sha256sum \
    "$data_dir/Saves/v1/fixture-save.dat" \
    "$data_dir/Settings/ServerGameSettings.json" \
    "$server_dir/manual/unmanaged.txt" >"$record_dir/sentinels.actual"
  cmp -s "$record_dir/sentinels.expected" "$record_dir/sentinels.actual" || fail 'operator sentinel bytes changed'
}

tree_digest() {
  local tree=$1
  find "$tree" -type f -printf '%P\0' | sort -z \
    | while IFS= read -r -d '' relative; do
        printf '%s  %s  %s\n' "$(sha256sum "$tree/$relative" | cut -d ' ' -f1)" \
          "$(stat -c '%a' "$tree/$relative")" "$relative"
      done | sha256sum | cut -d ' ' -f1
}

request_count() {
  local request=$1
  awk -v expected="GET $request" '$0 == expected { count++ } END { print count + 0 }' "$record_dir/sidecar.requests"
}

assert_request_count() {
  local request=$1 expected=$2
  [[ $(request_count "$request") -eq $expected ]] || fail "request count for $request is not $expected"
}

assert_initial_graph_and_requests() {
  local lock=$server_dir/.docker-vrising/package-lock.json
  jq -e '
    [.Roots[] | [.Namespace, .Name, .Version]] == [
      ["odjit", "KindredCommands", "2.5.8"],
      ["Team_GreenEye", "Satisvampory", "1.0.85"]
    ] and
    [.Packages[] | [.Ref.Namespace, .Ref.Name, .Ref.Version, [(.Dependencies // [])[] | [.Namespace, .Name, .Version]]]] == [
      ["BepInEx", "BepInExPack_V_Rising", "1.733.2", []],
      ["deca", "VampireCommandFramework", "0.10.4", [["BepInEx", "BepInExPack_V_Rising", "1.733.2"]]],
      ["odjit", "KindredCommands", "2.5.8", [["BepInEx", "BepInExPack_V_Rising", "1.733.2"], ["deca", "VampireCommandFramework", "0.10.4"]]],
      ["cheesasaurus", "HookDOTS_API", "1.1.1", [["BepInEx", "BepInExPack_V_Rising", "1.733.2"]]],
      ["Team_GreenEye", "Satisvampory", "1.0.85", [["BepInEx", "BepInExPack_V_Rising", "1.733.2"], ["cheesasaurus", "HookDOTS_API", "1.1.1"], ["deca", "VampireCommandFramework", "0.10.4"]]]
    ]
  ' "$lock" >/dev/null || fail 'package graph identities or dependency edges are not exact'

  local roots=(
    /api/experimental/package/odjit/KindredCommands/
    /api/experimental/package/Team_GreenEye/Satisvampory/
  )
  local versions=(
    /api/experimental/package/odjit/KindredCommands/2.5.8/
    /api/experimental/package/Team_GreenEye/Satisvampory/1.0.85/
    /api/experimental/package/BepInEx/BepInExPack_V_Rising/1.733.2/
    /api/experimental/package/deca/VampireCommandFramework/0.10.4/
    /api/experimental/package/cheesasaurus/HookDOTS_API/1.1.1/
  )
  local downloads=(
    /package/download/odjit/KindredCommands/2.5.8/
    /package/download/Team_GreenEye/Satisvampory/1.0.85/
    /package/download/BepInEx/BepInExPack_V_Rising/1.733.2/
    /package/download/deca/VampireCommandFramework/0.10.4/
    /package/download/cheesasaurus/HookDOTS_API/1.1.1/
  )
  local request
  for request in "${roots[@]}" "${versions[@]}" "${downloads[@]}"; do
    assert_request_count "$request" 1
  done
  for request in \
    /api/experimental/package/BepInEx/BepInExPack_V_Rising/ \
    /api/experimental/package/deca/VampireCommandFramework/ \
    /api/experimental/package/cheesasaurus/HookDOTS_API/; do
    assert_request_count "$request" 0
  done
  [[ $(grep -c '^GET /package/download/' "$record_dir/sidecar.requests") -eq 5 ]] \
    || fail 'initial install did not make exactly five package downloads'
  [[ $(wc -l <"$record_dir/sidecar.requests") -eq 12 ]] || fail 'initial request trace contains unexpected requests'
}

wait_apply_barrier() {
  local token=$1 marker=$record_dir/apply-barrier.$1.ready
  for _ in $(seq 1 200); do
    if [[ -f $marker && $(<"$marker") == "$token" ]]; then
      return 0
    fi
    [[ $(docker inspect --format '{{.State.Running}}' "$controller_name" 2>/dev/null) == true ]] \
      || fail 'controller exited before reaching fixture apply barrier'
    sleep 0.05
  done
  fail 'fixture apply barrier did not publish its exact run token'
}

assert_production_isolation() {
  [[ $(docker image inspect --format '{{json .RootFS.Layers}}' "$production_image") == \
     "$(docker image inspect --format '{{json .RootFS.Layers}}' "$default_image")" ]] \
    || fail 'production and default rootfs layers differ'
  [[ $(docker image inspect --format '{{json .Config}}' "$production_image") == \
     "$(docker image inspect --format '{{json .Config}}' "$default_image")" ]] \
    || fail 'production and default image configs differ'

  local image name
  for image in "$production_image" "$default_image"; do
    name=$suite_token-isolation-${image##*:}
    docker create --name "$name" --label "$suite_label" \
      --mount "type=bind,src=$probe_server_dir,dst=/mnt/vrising/server" \
      --mount "type=bind,src=$probe_data_dir,dst=/mnt/vrising/persistentdata" \
      --entrypoint /bin/sh "$image" -c '
        test ! -e /fixture &&
        test ! -e /usr/local/bin/fixture-sidecar &&
        test ! -e /usr/local/bin/vrisingctl-fixture &&
        test ! -e /usr/local/share/ca-certificates/docker-vrising-fixture-ca.crt &&
        case "$PATH" in *fixture*) exit 1;; esac &&
        case "$(command -v steamcmd) $(command -v wine64) $(command -v wineserver) $(command -v Xvfb)" in *fixture*) exit 1;; esac &&
        ! grep -aFq FIXTURE_APPLY_BARRIER /usr/local/bin/vrisingctl
      ' >/dev/null
    assert_bind_mounts "$name"
    docker start -a "$name" >/dev/null || fail "$image contains fixture-only assets or hook strings"
    docker rm -fv "$name" >/dev/null
  done
}

assert_zero_suite_resources
generate_tls_fixture
validate_tls_fixture
run_strict_fake_probes

docker network create --internal --label "$suite_label" "$network_name" >/dev/null
start_sidecar

echo 'scenario 1/6: empty mounts install exact graph and become healthy'
[[ -z $(find "$server_dir" -mindepth 1 -print -quit) && -z $(find "$data_dir" -mindepth 1 -print -quit) ]] \
  || fail 'first-install bind mounts are not genuinely empty'
start_controller "$network_name"
wait_healthy
assert_run_started "$current_run_token"
mapfile -t managed_files < <(find "$server_dir/BepInEx/plugins/saltydk-managed" -maxdepth 1 -type f -printf '%f\n' | sort)
expected_files=(HookDOTS.API.dll KindredCommands.dll NetTopologySuite.dll Satisvampory.dll VampireCommandFramework.dll)
[[ ${managed_files[*]} == "${expected_files[*]}" ]] || fail "managed DLL set is not exact: ${managed_files[*]}"
assert_initial_graph_and_requests
grep -Fq 'Enabled = false' "$server_dir/BepInEx/config/BepInEx.cfg" || fail 'BepInEx console logging was not disabled'
[[ -f $server_dir/VRisingServer.exe && $(<"$server_dir/steam_appid.txt") == 1604030 ]] || fail 'Steam installation is invalid'
[[ $(grep -c '^mode=remote ' "$record_dir/steamcmd.accepted") -eq 1 && \
   $(grep -c '^mode=update ' "$record_dir/steamcmd.accepted") -eq 1 ]] || fail 'Steam accepted argv evidence is not exact'
baseline_identity=$record_dir/identity-baseline.json
capture_active_identity "$baseline_identity"
seed_sentinels
assert_sentinels
stop_controller
assert_sentinels

state_file=$server_dir/.docker-vrising/state.json
package_lock=$server_dir/.docker-vrising/package-lock.json
echo 'scenario 2/6: offline restart is degraded but healthy'
start_controller none
wait_healthy
assert_run_started "$current_run_token"
jq -e '.Runtime.Ready == true and .Runtime.Phase == "ready" and .Runtime.Degraded == true and (.Runtime.Reason | test("resolve mod package graph|metadata"; "i"))' "$state_file" >/dev/null \
  || fail 'offline restart did not record expected degraded healthy state'
assert_identity_unchanged "$baseline_identity" 'offline restart'
assert_sentinels
stop_controller
assert_sentinels

echo 'scenario 3/6: corrupt cached and downloaded bytes preserve active identity'
bepinex_sha=$(jq -er '.Packages[] | select(.Ref.Namespace == "BepInEx") | .SHA256' "$package_lock")
cache_file=
while IFS= read -r candidate; do
  if [[ $(sha256sum "$candidate" | cut -d ' ' -f1) == "$bepinex_sha" ]]; then
    cache_file=$candidate
    break
  fi
done < <(find "$server_dir/.docker-vrising/cache" -type f | sort)
[[ -n $cache_file ]] || fail 'exact BepInEx cache entry was not found'
printf 'corrupt cached fixture bytes\n' >"$cache_file"
start_controller "$network_name"
wait_healthy
jq -e '.Runtime.Degraded == true and (.Runtime.Reason | test("BepInEx-BepInExPack_V_Rising-1.733.2")) and (.Runtime.Reason | test("size|integrity|sha"; "i"))' "$state_file" >/dev/null \
  || fail 'corrupt cache did not record the expected package/integrity degradation'
assert_identity_unchanged "$baseline_identity" 'corrupt cache fallback'
assert_sentinels
stop_controller
rm "$cache_file"

corrupt_url=/package/download/BepInEx/BepInExPack_V_Rising/1.733.2/
corrupt_requests_before=$(request_count "$corrupt_url")
touch "$record_dir/corrupt-downloads"
start_controller "$network_name"
wait_healthy
corrupt_requests_after=$(request_count "$corrupt_url")
[[ $((corrupt_requests_after - corrupt_requests_before)) -eq 1 ]] || fail 'corrupt download did not make exactly one exact package request'
jq -e '.Runtime.Degraded == true and (.Runtime.Reason | test("BepInEx-BepInExPack_V_Rising-1.733.2")) and (.Runtime.Reason | test("size|integrity|sha"; "i"))' "$state_file" >/dev/null \
  || fail 'corrupt download did not record the expected package/integrity degradation'
assert_identity_unchanged "$baseline_identity" 'corrupt download fallback'
assert_sentinels
stop_controller
rm "$record_dir/corrupt-downloads"

echo 'scenario 4/6: deterministic partial managed apply recovers on restart'
touch "$record_dir/updated-packages"
start_controller "$network_name" \
  --env FIXTURE_KINDRED_VERSION=2.5.9 --env FIXTURE_SATISVAMPORY_VERSION=1.0.86 \
  --env FIXTURE_APPLY_BARRIER=true
barrier_token=$current_run_token
wait_apply_barrier "$barrier_token"
jq -e --arg token "$barrier_token" '.Transaction.Phase == "applying" and .Candidate.ID != .Active.ID' "$state_file" >/dev/null \
  || fail 'barrier was not reached in a distinct applying candidate'
grep -Fxq 'fixture kindred v2' "$server_dir/BepInEx/plugins/saltydk-managed/KindredCommands.dll" \
  || fail 'candidate KindredCommands bytes were not live at the barrier'
grep -Fxq 'fixture satisvampory v1' "$server_dir/BepInEx/plugins/saltydk-managed/Satisvampory.dll" \
  || fail 'known-good Satisvampory bytes were not retained at the barrier'
assert_sentinels
docker kill --signal KILL "$controller_name" >/dev/null
docker wait "$controller_name" >/dev/null
docker rm -fv "$controller_name" >/dev/null
assert_sentinels
start_controller "$network_name" --env MODS_ENABLED=false --env UPDATE_GAME=false
wait_healthy
jq -e '.Transaction == null and .Candidate == null and .Active.Status == "active" and .Failed.Status == "failed" and .Active.ID != .Failed.ID and .Active.LockDigest != .Failed.LockDigest' "$state_file" >/dev/null \
  || fail 'restart did not clear and distinguish the interrupted candidate'
assert_identity_unchanged "$baseline_identity" 'interrupted apply recovery'
assert_sentinels
stop_controller
assert_sentinels

echo 'scenario 5/6: MODS_ENABLED=false skips roots and preserves mod state'
cache_before=$(tree_digest "$server_dir/.docker-vrising/cache")
generations_before=$(tree_digest "$server_dir/.docker-vrising/generations")
config_before=$(sha256sum "$server_dir/BepInEx/config/BepInEx.cfg" | cut -d ' ' -f1)
requests_before=$(wc -l <"$record_dir/sidecar.requests")
start_controller "$network_name" --env MODS_ENABLED=false --env UPDATE_GAME=false
wait_healthy
assert_run_started "$current_run_token"
[[ $(wc -l <"$record_dir/sidecar.requests") -eq $requests_before ]] || fail 'unmodded start contacted Thunderstore'
[[ $(tree_digest "$server_dir/.docker-vrising/cache") == "$cache_before" ]] || fail 'unmodded start changed package cache'
[[ $(tree_digest "$server_dir/.docker-vrising/generations") == "$generations_before" ]] || fail 'unmodded start changed generations'
[[ $(sha256sum "$server_dir/BepInEx/config/BepInEx.cfg" | cut -d ' ' -f1) == "$config_before" ]] || fail 'unmodded start changed BepInEx configuration'
grep -Fxq 'WINEDLLOVERRIDES=winhttp=b' "$record_dir/runs/$current_run_token/wine.env" || fail 'unmodded Wine environment is wrong'
assert_identity_unchanged "$baseline_identity" 'unmodded launch'
assert_sentinels

echo 'scenario 6/6: TERM exits within SHUTDOWN_TIMEOUT and reaps exact current children'
term_token=$current_run_token
started_ms=$(date +%s%3N)
docker kill --signal TERM "$controller_name" >/dev/null
timeout 5 docker wait "$controller_name" >/dev/null || fail 'controller did not exit within SHUTDOWN_TIMEOUT'
elapsed_ms=$(( $(date +%s%3N) - started_ms ))
(( elapsed_ms <= 4000 )) || fail "TERM shutdown took ${elapsed_ms}ms, exceeding SHUTDOWN_TIMEOUT"
[[ $(docker inspect --format '{{.State.Running}} {{.State.Pid}}' "$controller_name") == 'false 0' ]] || fail 'controller retains a process after TERM'
assert_current_run_reaped "$term_token"
[[ ! -e $record_dir/runs/$term_token/wineserver.argv ]] || fail 'graceful TERM unexpectedly used wineserver escalation'
assert_identity_unchanged "$baseline_identity" 'TERM shutdown'
assert_sentinels
docker rm -fv "$controller_name" >/dev/null

assert_production_isolation
docker rm -fv "$sidecar_name" >/dev/null
docker network rm "$network_name" >/dev/null
assert_zero_suite_resources
echo "fixture resources: containers=0 networks=0 volumes=0 ($suite_token)"
echo "container fixture acceptance: PASS (${elapsed_ms}ms TERM shutdown)"
