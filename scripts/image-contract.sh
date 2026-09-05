#!/usr/bin/env bash

set -euo pipefail

readonly image=${1:-}
readonly compose_file=compose.yaml

fail() {
	printf 'image contract: %s\n' "$*" >&2
	exit 1
}

[[ $# -eq 1 && -n "$image" ]] || fail "usage: $0 IMAGE"
command -v docker >/dev/null 2>&1 || fail "docker is required"
command -v python3 >/dev/null 2>&1 || fail "python3 is required"

docker buildx build --platform linux/amd64 --load -t "$image" .
docker image inspect "$image" >/dev/null 2>&1 || fail "image not found: $image"
[[ -f "$compose_file" ]] || fail "Compose model not found: $compose_file"

assert_inspect() {
	local description=$1
	local format=$2
	local expected=$3
	local actual

	actual=$(docker image inspect --format "$format" "$image")
	[[ "$actual" == "$expected" ]] || fail "$description: expected '$expected', got '$actual'"
}

assert_inspect "platform" '{{.Os}}/{{.Architecture}}' "linux/amd64"
assert_inspect "entrypoint" '{{json .Config.Entrypoint}}' '["/usr/bin/tini","--","/usr/local/bin/vrisingctl"]'
assert_inspect "command" '{{json .Config.Cmd}}' '["run"]'
assert_inspect "health command" '{{json .Config.Healthcheck.Test}}' '["CMD","/usr/local/bin/vrisingctl","health"]'
assert_inspect "health interval" '{{.Config.Healthcheck.Interval}}' "30s"
assert_inspect "health timeout" '{{.Config.Healthcheck.Timeout}}' "5s"
assert_inspect "health start period" '{{.Config.Healthcheck.StartPeriod}}' "30m0s"
assert_inspect "health retries" '{{.Config.Healthcheck.Retries}}' "3"
assert_inspect "volumes" '{{json .Config.Volumes}}' '{"/mnt/vrising/persistentdata":{},"/mnt/vrising/server":{}}'
assert_inspect "ports" '{{json .Config.ExposedPorts}}' '{"25575/tcp":{},"9876/udp":{},"9877/udp":{}}'

docker run --rm --entrypoint sh "$image" -c \
	'test -x /usr/local/bin/vrisingctl && test -x /usr/bin/tini && command -v steamcmd && command -v wine64 && command -v Xvfb' \
	>/dev/null || fail "required executable probe failed"

temp_dir=$(mktemp -d)
readonly temp_dir
readonly compose_json="$temp_dir/compose.json"
[[ -d "$temp_dir" && "$compose_json" == "$temp_dir/compose.json" ]] || fail "invalid temporary path"

cleanup() {
	if [[ -f "$compose_json" ]]; then
		rm -- "$compose_json"
	fi
	rmdir -- "$temp_dir"
}
trap cleanup EXIT

docker compose -f "$compose_file" config --format json >"$compose_json"
python3 - "$compose_json" <<'PY'
import json
import sys


def fail(message):
    raise SystemExit(f"image contract: {message}")


with open(sys.argv[1], encoding="utf-8") as source:
    model = json.load(source)

unexpected_top_level_fields = set(model) - {"name", "networks", "services"}
if unexpected_top_level_fields:
    fail(f"Compose has unexpected top-level fields: {sorted(unexpected_top_level_fields)}")

services = model.get("services")
if not isinstance(services, dict) or set(services) != {"vrising"}:
    fail("Compose must define only the vrising service")

service = services["vrising"]
allowed_service_fields = {
    "command",
    "container_name",
    "entrypoint",
    "environment",
    "image",
    "networks",
    "ports",
    "restart",
    "volumes",
}
unexpected_service_fields = set(service) - allowed_service_fields
if unexpected_service_fields:
    fail(f"Compose service has unexpected fields: {sorted(unexpected_service_fields)}")
if service.get("command") is not None or service.get("entrypoint") is not None:
    fail("Compose must use the image command and entrypoint")

scalar_fields = {
    "container_name": "v-rising",
    "image": "saltydk/vrising",
    "restart": "unless-stopped",
}
for field, expected in scalar_fields.items():
    if service.get(field) != expected:
        fail(f"Compose {field} must be {expected!r}")

expected_environment = {
    "SERVERNAME": "Salty",
    "TZ": "Europe/Copenhagen",
    "WINEDEBUG": "fixme-all",
}
if service.get("environment") != expected_environment:
    fail("Compose environment does not match the approved service")

expected_mounts = {
    ("/opt/v-rising/server", "/mnt/vrising/server"),
    ("/opt/v-rising/data", "/mnt/vrising/persistentdata"),
}
mounts = {
    (mount.get("source"), mount.get("target"))
    for mount in service.get("volumes", [])
    if mount.get("type") == "bind"
}
if mounts != expected_mounts or len(service.get("volumes", [])) != 2:
    fail("Compose mounts do not match the approved service")
for mount in service["volumes"]:
    if set(mount) != {"bind", "source", "target", "type"} or mount.get("bind") != {}:
        fail("Compose mount options do not match the approved service")

expected_ports = {
    (9876, "9876", "udp", "ingress"),
    (9877, "9877", "udp", "ingress"),
    (25575, "25575", "tcp", "ingress"),
}
ports = {
    (port.get("target"), str(port.get("published")), port.get("protocol"), port.get("mode"))
    for port in service.get("ports", [])
}
if ports != expected_ports or len(service.get("ports", [])) != 3:
    fail("Compose ports do not match the approved service")
for port in service["ports"]:
    if set(port) != {"mode", "protocol", "published", "target"}:
        fail("Compose port options do not match the approved service")

if set(service.get("networks", {})) != {"saltbox"}:
    fail("Compose service must use only the saltbox network")

networks = model.get("networks")
if not isinstance(networks, dict) or set(networks) != {"saltbox"}:
    fail("Compose must define only the saltbox network")
network = networks["saltbox"]
if network.get("name") != "saltbox" or network.get("external") is not True:
    fail("Compose saltbox network must be named saltbox and external")
if set(network) != {"external", "ipam", "name"} or network.get("ipam") != {}:
    fail("Compose saltbox network has unexpected options")
PY

printf 'image contract: PASS (%s)\n' "$image"
