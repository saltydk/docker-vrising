# Docker V Rising

`saltydk/vrising` is a Linux AMD64 container image for installing and running
the Windows V Rising dedicated server under Wine. The image downloads the game
from Steam and the managed mod packages from Thunderstore at runtime; it does
not redistribute game or mod binaries.

The managed mod stack has two independently selected roots:
`odjit/KindredCommands` and `Team_GreenEye/Satisvampory`. With the default
`latest` selectors, each root is resolved independently to its newest active
version. The controller then follows the exact recursive dependency versions
declared by those two root manifests, merges identical requirements, and
rejects conflicts. Transitive packages such as BepInEx,
VampireCommandFramework, and HookDOTS API are never independently upgraded to
their own latest releases.

The compatible closure validated when this image was designed contains exactly
five packages: KindredCommands `2.5.8` declares BepInExPack V Rising `1.733.2`
and VampireCommandFramework `0.10.4`; Satisvampory `1.0.85` declares that same
BepInEx and VCF pair plus HookDOTS API `1.1.1`. VCF and HookDOTS each declare
the same exact BepInEx version. Moving `latest` selectors can change version
numbers, so the active `package-lock.json`, not this historical example, is the
authority for a running installation.

## Drop-in Compose migration

For an existing TrueOsiris-style deployment, change only its image line:

```diff
-    image: trueosiris/vrising
+    image: saltydk/vrising
```

Keep both existing bind mounts. The first start operates on the current server
installation, saves, settings, ports, network, and compatible environment
variables in place. It is not a byte-immutable operation: Steam may replace or
remove game binaries, the managed-file transaction may replace or prune the
recognized mod overlay, `LOGDAYS` may prune recognized server logs, and backup
retention may prune older recognized backup archives.

Existing saves and settings are not deleted or rewritten by the controller as
part of migration. They are included in the documented pre-Steam-upgrade
backup transaction and are never restored automatically; after launch, the
game itself may legitimately write or migrate them. Unmanaged server and mod
content remains outside the managed-file inventory and is not pruned by the
controller.

The exact checked-in [`compose.yaml`](compose.yaml) is:

```yaml
services:
  vrising:
    container_name: v-rising
    image: saltydk/vrising
    volumes:
      - /opt/v-rising/server:/mnt/vrising/server
      - /opt/v-rising/data:/mnt/vrising/persistentdata
    environment:
      TZ: "Europe/Copenhagen"
      SERVERNAME: "Salty"
      WINEDEBUG: "fixme-all"
    ports:
      - 9876:9876/udp
      - 9877:9877/udp
      - 25575:25575/tcp
    restart: unless-stopped
    networks:
      - saltbox

networks:
  saltbox:
    name: saltbox
    external: true
```

The example expects the external `saltbox` network to exist. Start or update
the deployment with:

```bash
docker compose pull
docker compose up -d
```

`docker compose restart vrising` restarts the controller in the image already
present. With the default settings, that restart checks for and applies newer
Steam and Thunderstore runtime payloads, but it does **not** pull a newer
container image. Run `docker compose pull` followed by `docker compose up -d`
when the image itself must be updated.

### First-start resources

The first installation downloads the Steam server, both current mod roots and
their exact dependency closure, creates a Wine prefix, and performs BepInEx
IL2CPP initialization. It can remain in Docker's `starting` health state for
up to the default 30-minute startup deadline. Large existing worlds can use
most of that window; do not treat `starting` as a failure before the deadline.

There is no fixed disk quota in the image. Size the server bind for the Steam
installation, Wine prefix, validated package cache, active and previous managed
generations, and retained backups. Before each package download and each
pre-upgrade backup, the controller requires enough free space for the content
being written plus a 512 MiB safety reserve. A backup check conservatively uses
the full selected source size even though the resulting archive is compressed.

Game and save trees are streamed for downloads, hashing, and backup creation;
the controller does not hold a second complete game or save tree in memory.
Transient memory is nevertheless higher on first start because SteamCMD,
Xvfb, Wine, the V Rising server, and BepInEx initialization overlap. Do not set
a tight container memory limit until first-start and representative-world
usage have been measured on the target host.

## Configuration

Defaults require no additional environment values.

| Variable | Default | Behavior |
|---|---|---|
| `UPDATE_GAME` | `true` | Query Steam and update app 1829350 when its remote build differs. |
| `UPDATE_MODS` | `true` | Resolve, download, and stage the current compatible two-root mod graph. |
| `MODS_ENABLED` | `true` | Launch with the managed BepInEx, KindredCommands, and Satisvampory stack. |
| `KINDRED_COMMANDS_VERSION` | `latest` | Select the newest active KindredCommands root or an exact `x.y.z` version. |
| `SATISVAMPORY_VERSION` | `latest` | Select the newest active Satisvampory root or an exact `x.y.z` version. |
| `BACKUP_RETENTION` | `3` | Retain this many newest completed pre-Steam-upgrade archives; minimum `1`. |
| `STARTUP_TIMEOUT` | `30m` | Positive Go-duration deadline for game and mod readiness. |
| `SHUTDOWN_TIMEOUT` | `120s` | Positive Go-duration graceful-stop wait before one Wine shutdown escalation. |
| `PUID` / `PGID` | unset | Optional paired positive numeric IDs for a one-time ownership migration and runtime identity. |
| `LOGDAYS` | `30` | Days to retain image-created server logs; integer `1` through `365000`. |
| `BRANCH` | unset (`public`) | Optional Steam beta branch passed to SteamCMD. |
| `TZ` | unset | Process timezone, passed through when set. |
| `WINEDEBUG` | unset | Wine diagnostic setting, passed through unchanged when set. |
| `VR_*` | unset | Native V Rising environment variables, passed through unchanged. |

Boolean controls accept only the literal lowercase values `true` and `false`.
Invalid booleans, versions, durations, retention values, log retention, or
ownership IDs fail during preflight before update mutation. `PUID` and `PGID`
must be supplied together. Without them, the controller infers the numeric
owner of the server bind and requires the persistent-data bind to be writable
by that identity; it never makes the trees world-writable.

The supported legacy aliases have no image-defined default:

| Legacy variable | Native variable |
|---|---|
| `SERVERNAME` | `VR_SERVER_NAME` |
| `WORLDNAME` | `VR_SAVE_NAME` |
| `GAMEPORT` | `VR_GAME_PORT` |
| `QUERYPORT` | `VR_QUERY_PORT` |

If both forms are present, the native `VR_*` variable wins, even when its value
is empty, and startup logs that the legacy alias was shadowed. Existing JSON
under `persistentdata/Settings` remains authoritative; this image does not
rewrite arbitrary settings JSON from environment values.

## Updates, fallback, and fail-closed behavior

At startup the controller validates both mounts, takes an exclusive lifetime
lock, recovers any interrupted managed-file transaction, and validates the
installed Steam build and active package generation before launching.

Before SteamCMD or live managed files have been mutated, a temporary Steam or
Thunderstore metadata/download failure may start the complete installed
known-good Steam build and mod generation. This is a degraded but healthy
fallback: the reason is written to runtime state and logged at launch. A fresh
installation has no known-good fallback and therefore fails closed when its
required metadata or artifacts are unavailable.

Once SteamCMD has begun mutating the live server tree, any update or validation
failure is fatal. The controller does not trust or launch that partially
updated tree. Likewise, a candidate mod generation is promoted only after the
server, BepInEx, VCF, KindredCommands, and Satisvampory readiness evidence all
succeeds. Failed candidate logs and backup state are retained; saves are never
restored automatically because the game may already have migrated or written
them.

### Emergency controls and exact pins

Use these controls deliberately and remove them after recovery:

- `MODS_ENABLED=false` starts unmodded with Wine's built-in `winhttp`; it does
  not delete cached packages, generations, or mod configuration.
- `UPDATE_GAME=false` prevents a Steam metadata check or update and requires an
  already installed server that still validates.
- `UPDATE_MODS=false` prevents Thunderstore resolution and requires both a
  valid active mod generation and a valid recorded known-good Steam build. It
  cannot bootstrap an empty installation.
- `KINDRED_COMMANDS_VERSION=x.y.z` and
  `SATISVAMPORY_VERSION=x.y.z` pin the two roots independently. Their
  transitive dependencies remain the exact versions declared by those pinned
  manifests; do not pin or upgrade dependencies separately.

For example, to isolate a mod failure while preserving all mod state:

```yaml
environment:
  MODS_ENABLED: "false"
  UPDATE_GAME: "false"
```

## State, cache, and locking

Image-owned state is under
`/mnt/vrising/server/.docker-vrising` (the example host path is
`/opt/v-rising/server/.docker-vrising`):

| Path | Purpose |
|---|---|
| `state.json` | Steam build, active/previous/candidate/failed generations, transaction journals, and current runtime/health state. |
| `package-lock.json` | Ordered root selections, exact recursive package graph, source metadata, archive sizes and SHA-256 values, and the active lock digest. |
| `update.lock` | Exclusive lifetime lock preventing two controllers from operating on one installation. The file's presence is normal; do not delete it. |
| `cache/` | Validated Thunderstore archives retained for known-good fallback and offline restart. |
| `generations/` | Staged, active, previous, and failed managed overlays and their inventories. |
| `backups/` | Verified pre-Steam-upgrade archives. |
| `wineprefix/` | Persistent Wine prefix. |

Useful read-only inspection commands are:

```bash
docker exec v-rising vrisingctl health
docker logs --tail 200 v-rising
sudo jq '{SteamBuild,Active,Previous,Failed,Runtime}' \
  /opt/v-rising/server/.docker-vrising/state.json
sudo jq '{Digest,Roots,Packages:[.Packages[]|{Ref,Dependencies,SHA256}]}' \
  /opt/v-rising/server/.docker-vrising/package-lock.json
sudo ls -la /opt/v-rising/server/.docker-vrising
```

Do not edit state files, package locks, generations, cache entries, or the lock
by hand while the container is running.

## Backups and manual restore

Before changing from one installed Steam build to another, the controller
creates and verifies
`server/.docker-vrising/backups/vrising-backup-<UTC timestamp>.tar.gz`.
It contains `persistentdata/Settings`, `persistentdata/Saves`, and
`server/BepInEx/config` when present, plus `manifest.json`. It excludes game
binaries, package caches, state, and ordinary logs. Only after a new archive is
complete does retention prune older recognized archives; the default keeps the
newest three.

Restore is intentionally manual and can overwrite current settings, saves, or
mod configuration. First stop the service, preserve the current data
separately, choose one archive, inspect it, and restore only its two explicit
trees. This example does not use `--delete`:

```bash
docker compose stop vrising

backup=/opt/v-rising/server/.docker-vrising/backups/vrising-backup-YYYYMMDDTHHMMSS.NNNNNNNNNZ.tar.gz
sudo gzip -t "$backup"
sudo tar -tzf "$backup"

restore_root=$(mktemp -d /tmp/vrising-restore.XXXXXX)
sudo tar -xzf "$backup" -C "$restore_root"
sudo rsync -a --numeric-ids "$restore_root/persistentdata/" /opt/v-rising/data/
sudo rsync -a --numeric-ids "$restore_root/server/" /opt/v-rising/server/

docker compose up -d
```

Keep the extracted restore directory until the restarted server and selected
world have been verified. A binary or mod-generation rollback is not a
substitute for deciding whether persistent data should be restored.

## Health, logs, ports, and RCON

The image health check runs `vrisingctl health`. Health stays `starting` during
updates and initialization, becomes `healthy` only when the exact tracked
server process and all required readiness markers agree, and becomes unhealthy
if that process dies or runtime state is incoherent. A known-good degraded
fallback remains healthy and records its reason in `state.json` and container
logs. RCON is not part of health.

The controller mirrors the current logs to container stdout with `[server]`
and `[bepinex]` source prefixes. Files remain at:

- `/mnt/vrising/persistentdata/logs/VRisingServer-<UTC timestamp>-<sequence>.log`
- `/mnt/vrising/server/BepInEx/LogOutput.log`

`LOGDAYS` removes only image-created V Rising server logs in the supported
server-log locations. It does not recursively traverse persistent data and
never prunes backups or BepInEx logs.

Publish UDP 9876 for gameplay and UDP 9877 for queries. TCP 25575 is available
for an existing RCON configuration, but RCON must also be enabled in
`ServerHostSettings.json`; keep its password out of Compose output, shell
history, diagnostics, and issue reports. If native `VR_GAME_PORT`,
`VR_QUERY_PORT`, or an RCON port changes the internal port, update the matching
Compose publication as well.

## Platform, licensing, and support boundary

The image supports `linux/amd64` hosts. On other host architectures, Docker
must provide AMD64 emulation; native non-AMD64 execution is not supported.
No CPU or memory limit is imposed by the Compose example because world size,
player count, first-run Wine/BepInEx work, and host emulation materially change
resource demand.

The container image includes the controller and its Linux runtime tools, but
downloads V Rising dedicated-server files through SteamCMD and managed package
archives through Thunderstore on the operator's host. Running it requires
acceptance of and compliance with the applicable Valve/Steam, Stunlock/V
Rising, Wine, Thunderstore, and individual mod licenses and terms. Those
runtime downloads are not covered or relicensed by this repository.

## Build and test

Local Go checks:

```bash
make check
go test -race ./...
```

Build and validate the production Linux AMD64 image:

```bash
make image
make image-check
```

Run the Docker-backed fixture suite (it builds dedicated local test images):

```bash
make container-test
```

Real runtime acceptance is deliberately separate because it downloads the
current server and mods and can take up to 30 minutes:

```bash
make live-acceptance IMAGE=saltydk/vrising:local MODE=fresh
hack/live-acceptance.sh migrate saltydk/vrising:local SOURCE_SERVER_DIR SOURCE_DATA_DIR
```

Migration acceptance validates copies made beneath one disposable temporary
root; it never runs against, writes to, or deletes the source trees. Failed
runs retain and print that root for diagnosis. Do not describe a deployment as
migration-compatible until both fresh and copied-data acceptance pass on a
representative host.
