# Docker V Rising image design

Date: 2026-09-05
Status: approved

## Summary

Build a clean-room Linux AMD64 container image for the Windows V Rising
dedicated server. The image will update the game through SteamCMD at startup,
install the newest KindredCommands and Satisvampory releases independently,
merge the exact recursive dependency versions declared by both releases, and
run the resulting server under Wine.

The replacement must migrate the existing deployment by changing only its
image name to **saltydk/vrising**. Existing game files, saves, settings, ports,
networking, and environment values must continue to work.

The image will favor availability before an update begins and correctness after
mutation begins. Temporary Steam or Thunderstore outages may fall back to the
already installed known-good state. A failed in-place Steam update must fail
closed because the previous game tree can no longer be trusted.

## Evidence and constraints

The supporting upstream and compatibility research is recorded in
[the research note](../../research/2026-09-05-vrising-image-upstream-and-mods.md).

The current compatible Thunderstore closure is:

- odjit-KindredCommands-2.5.8
- Team_GreenEye-Satisvampory-1.0.85
- deca-VampireCommandFramework-0.10.4
- BepInEx-BepInExPack_V_Rising-1.733.2
- cheesasaurus-HookDOTS_API-1.1.1

Both independently selected roots currently require the same exact BepInEx and
VCF versions, while Satisvampory additionally requires HookDOTS API 1.1.1. The
newest VCF release is newer than the version declared by either root. The
resolver must preserve both roots' exact dependency versions, deduplicate
identical requirements, and reject conflicts. It must never independently
upgrade dependencies.

TrueOsiris provides useful operational precedent but no repository license.
Implementation must be clean-room and must not copy its scripts. Steam game
files and Thunderstore package binaries will be downloaded at runtime rather
than redistributed in the image.

## Deployment compatibility

The following existing Compose service remains valid after changing only the
image:

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

Compatibility requirements:

- Keep the exact container paths **/mnt/vrising/server** and
  **/mnt/vrising/persistentdata**.
- Never erase or relocate existing bytes during first migration.
- Preserve **TZ**, **SERVERNAME**, and **WINEDEBUG** behavior.
- Continue exposing UDP 9876 and 9877. TCP 25575 remains available for existing
  RCON configuration but is not required for health checks.
- Preserve inexpensive TrueOsiris aliases: **WORLDNAME**, **GAMEPORT**,
  **QUERYPORT**, **BRANCH**, and **LOGDAYS**.
- Pass native V Rising **VR_** environment variables through unchanged. When a
  native variable and its legacy alias are both present, the native variable
  wins and startup logs a warning.
- Existing persistent settings files remain authoritative. The image does not
  rewrite arbitrary settings JSON from environment variables.

## Image and control-plane architecture

The image targets **linux/amd64** and contains:

- An Ubuntu 22.04 base pinned by digest.
- WineHQ stable, refreshed only through a tested image rebuild.
- SteamCMD, Xvfb, tini, CA certificates, timezone data, archive support, and no
  full graphical desktop or Steam client beyond required runtime dependencies.
- One statically compiled Go binary named **vrisingctl**.

The image contains no game or mod binaries. A pinned Go builder image produces
the control binary; the runtime image contains only the resulting binary and
runtime dependencies.

Tini is PID 1 and runs **vrisingctl run**. After any explicitly requested
ownership migration, the controller drops to the selected mount identity before
locking, downloading, backing up, updating, or launching. The binary presents one deep runtime
interface while hiding update, archive, backup, process, and health mechanics.
The Go source remains flat by default. Concrete implementations are preferred;
small internal interfaces exist only at filesystem, HTTP, clock, and subprocess
seams where tests require alternate adapters.

### Runtime state

Existing game and persistent-data paths remain unchanged. Image-owned state is
stored below **/mnt/vrising/server/.docker-vrising**:

- **cache/** contains validated Thunderstore archives.
- **generations/** contains staged and previous managed mod overlays.
- **backups/** contains pre-upgrade save/configuration snapshots.
- **wineprefix/** contains the persistent Wine prefix.
- **state.json** records the active, candidate, previous, and failed
  generations plus the observed Steam build.
- **package-lock.json** records the ordered selected roots, exact merged dependency closure,
  download metadata, sizes, calculated SHA-256 values, and activation state.
- **update.lock** prevents concurrent use of the same installation.
- **runtime.json** records current process IDs, startup phase, readiness,
  selected versions, and degraded/fallback state for health inspection.

State files and activation records are written through temporary files followed
by atomic rename and directory sync. An interrupted transaction is detected and
recovered before another update begins.

### Ownership

Without **PUID** and **PGID**, startup infers the numeric owner of the existing
server mount and requires the persistent-data mount to be writable by that
identity. A root-owned legacy installation therefore remains root-compatible.

If supplied, **PUID** and **PGID** must appear together and be valid positive
integers. Their first use performs a clearly logged, one-time ownership
migration of both mounts and records the selected identity. SteamCMD, Xvfb, and
Wine run as that identity. The image never applies world-writable permissions.

## Startup and update transaction

### Preflight

On every start, **vrisingctl**:

1. Validates configuration and path ownership.
2. Acquires an exclusive update lock.
3. Confirms no prior V Rising process is using the installation.
4. Recovers or rolls back any interrupted managed-file journal.
5. Loads the current Steam build, active package lock, and previous known-good
   generation.

If a valid existing server is available and a remote metadata check fails
before mutation, startup records a degraded reason and uses the installed
known-good state. A first installation without required metadata or artifacts
fails closed.

### Mod resolution and staging

When **UPDATE_MODS=true** and **MODS_ENABLED=true**, the resolver:

1. Reads Thunderstore's package endpoints for the latest KindredCommands and
   Satisvampory versions independently, unless **KINDRED_COMMANDS_VERSION** or
   **SATISVAMPORY_VERSION** specifies that root's exact version.
2. Traverses both exact roots' dependency strings recursively and merges their
   complete graphs before downloading any archive.
3. Deduplicates identical requirements and rejects cycles or conflicting exact
   versions across either graph.
4. Downloads missing archives to temporary cache files with bounded retries and
   timeouts.
5. Verifies the reported file size, ZIP integrity, safe relative paths, absence
   of unsafe symlinks and duplicate destinations, embedded manifest identity,
   version, and dependency list.
6. Calculates SHA-256 for every archive. If Thunderstore serves different bytes
   for an already locked package version, the candidate is rejected and the
   prior lock remains active.

Plugin archives may provide DLLs at archive root or directly below one
**plugins/** subtree. The latter is flattened into the managed plugin directory;
deeper layouts, non-DLL payloads inside that subtree, links, unsafe paths, and
duplicate destination names are rejected before extraction.

The complete closure is staged before SteamCMD or the live mod files are
changed. The BepInEx pack's inner directory is expanded as a managed overlay.
VCF, KindredCommands, Kindred's bundled NetTopologySuite library, HookDOTS API,
and Satisvampory are placed in one image-managed plugin subtree. Bloodstone and
ServerLaunchFix are not installed because they are not dedicated-server
dependencies.

That subtree contains exactly **VampireCommandFramework.dll**,
**KindredCommands.dll**, **NetTopologySuite.dll**, **HookDOTS.API.dll**, and
**Satisvampory.dll**. Missing or additional DLLs reject the candidate.

Mutable BepInEx content is preserved, including configuration, cache, interop,
unity libraries, generated files, and logs. The image updates only paths listed
in its prior managed-file inventory. Manually installed mods outside the
image-managed subtree are not deleted.

BepInEx console logging is disabled for Wine compatibility while file logging
remains enabled. Modded launches force Wine's winhttp resolution to
native-then-builtin. **MODS_ENABLED=false** disables both roots, selects Wine's
builtin winhttp, and starts unmodded without deleting package caches,
generations, or configuration.

### Steam update and backup

When **UPDATE_GAME=true**, the controller queries Steam metadata before running
an update.

- If the remote check fails before SteamCMD mutates files, use the installed
  known-good server and log degraded state.
- If the remote build differs from the installed build, first create an atomic
  compressed snapshot.
- The snapshot contains persistent saves, settings, administration lists, ban
  lists, and BepInEx/Kindred configuration. It excludes game binaries, caches,
  and ordinary logs.
- The archive is written under the image-owned backup directory, verified, and
  renamed into place before SteamCMD starts.
- Keep the newest **BACKUP_RETENTION** successful snapshots, defaulting to
  three. Retention runs only after a new snapshot is complete.
- A backup failure prevents the upgrade and leaves the installed server active.

SteamCMD updates and validates app 1829350 for Windows directly in the existing
server tree, honoring the optional **BRANCH** value. A non-zero command result,
missing executable, unexpected app metadata, or incomplete validation is
fatal. Once SteamCMD has begun mutating the live tree, failure does not fall
back to an unverified old installation.

After successful Steam validation, the controller reapplies the complete staged
BepInEx overlay because Steam may replace files in the game directory. Managed
files are replaced through a journaled transaction while the server is
stopped. The former overlay remains available as the previous generation.

### Activation

The candidate generation remains provisional during server startup. It becomes
known-good only after all readiness evidence succeeds:

- The exact Wine server process remains alive.
- V Rising emits its startup-complete marker.
- BepInEx initializes.
- VCF loads.
- The locked KindredCommands version loads without a fatal error.
- The locked Satisvampory version emits
  **Satisvampory <version> (Satisvampory) ready.** without a fatal error.

The startup deadline defaults to 30 minutes to allow first-run IL2CPP generation
and large existing saves. If the process exits or readiness fails before the
deadline, the candidate is marked failed, logs and backup are retained, the
server is stopped, and the controller exits non-zero. Docker's restart policy
then starts the previous mod generation. If no previous mod generation exists,
startup continues to fail closed until the operator disables mods or a valid
candidate is available. Save snapshots are never restored automatically because
the game may already have written or migrated data.

## Process and health behavior

The controller starts Xvfb on a dynamically allocated display without deleting
global lock files, then starts Wine in a dedicated process group with launch
arguments represented as an argument slice.

It forwards TERM and INT to the tracked process group, waits
**SHUTDOWN_TIMEOUT** (default 120 seconds), invokes one Wine shutdown escalation
if needed, reaps Xvfb and every child, and exits with the server's meaningful
status. Broad process-name searches are forbidden.

The controller streams the current V Rising and BepInEx logs to container
stdout with source prefixes while retaining their normal files. **LOGDAYS**
defaults to 30 for compatibility, but cleanup is restricted to image-created
server logs and never recursively traverses the persistent tree or backup
directory.

The Dockerfile health check runs **vrisingctl health**. Health remains in
startup state while updating or initializing, becomes healthy only after the
activation evidence above, and becomes unhealthy if the exact server process
dies or the runtime state becomes incoherent. RCON is not required. Degraded
operation caused by a skipped update remains healthy when the known-good server
is running, with the degraded reason visible in logs and runtime state.

## Public configuration

Defaults require no Compose changes:

| Variable | Default | Meaning |
|---|---:|---|
| UPDATE_GAME | true | Check and apply the newest Steam build at startup. |
| UPDATE_MODS | true | Check and stage the newest compatible mod graph. |
| MODS_ENABLED | true | Launch with the managed BepInEx, KindredCommands, and Satisvampory stack. |
| KINDRED_COMMANDS_VERSION | latest | Select newest Kindred root or one exact version. |
| SATISVAMPORY_VERSION | latest | Select newest Satisvampory root or one exact version. |
| BACKUP_RETENTION | 3 | Successful pre-Steam-upgrade snapshots to retain. |
| STARTUP_TIMEOUT | 30m | Deadline for server and mod readiness. |
| SHUTDOWN_TIMEOUT | 120s | Graceful shutdown wait before escalation. |
| PUID / PGID | unset | Optional paired ownership migration and runtime identity. |
| LOGDAYS | 30 | Retention for image-created server logs. |

Boolean values accept only **true** or **false**. Durations use Go duration
syntax and must be positive. Invalid values fail before any mutation.

Legacy mappings are:

- **SERVERNAME** to **VR_SERVER_NAME**
- **WORLDNAME** to **VR_SAVE_NAME**
- **GAMEPORT** to **VR_GAME_PORT**
- **QUERYPORT** to **VR_QUERY_PORT**

The native **VR_** value takes precedence when both forms are present.
**BRANCH** controls Steam's beta branch. **TZ** configures the process timezone,
and **WINEDEBUG** is passed to Wine unchanged.

## Verification and acceptance

### Go and fixture tests

Use standard table-driven Go tests and simple fakes. Required cases include:

- Configuration defaults, strict parsing, alias precedence, and ownership
  selection.
- Independent latest-root selection, exact merged recursive resolution,
  duplicate requirements, cross-root cycles and conflicts, inactive versions,
  and malformed dependency strings.
- HTTP errors, timeouts, bounded retries, interrupted downloads, file-size
  mismatches, changed bytes for locked versions, and cache reuse.
- ZIP traversal, absolute paths, duplicate destinations, unsafe symlinks,
  corrupt archives, wrapper-directory handling, and manifest mismatches.
- Managed-file preservation, stale managed-file removal, journal recovery,
  candidate promotion, failed-candidate rollback, and manual mod preservation.
- Backup creation, atomic completion, failed backups, and three-generation
  retention.
- Process start, readiness deadlines, log matching, exit-status propagation,
  TERM/INT forwarding, escalation, and child reaping.

Integration tests use local HTTP fixtures and fake SteamCMD, Wine, and Xvfb
adapters. Container tests cover first install, no-op restart, exact version
pinning, metadata outages, corrupt artifacts, interrupted installation,
unmodded recovery, built-in health state, and graceful shutdown.

### Real runtime acceptance

Before the first production migration:

1. Build the Linux AMD64 image.
2. Start it with disposable volumes and download the current Steam server.
3. Require healthy status and log evidence for V Rising, BepInEx, VCF, and the
   exact KindredCommands and Satisvampory versions.
4. Stop the container and verify clean process exit.
5. Restart with Steam and Thunderstore unavailable and verify the known-good
   cache starts in degraded-but-healthy state.
6. Rehearse migration against copies of the existing
   **/opt/v-rising/server** and **/opt/v-rising/data** directories.
7. Verify the existing save is discovered, both UDP ports bind, optional RCON
   remains available when configured, and no pre-existing settings or saves are
   removed.

Production directories are not used for destructive acceptance. A real runtime
pass is required before describing the image as compatible.

## Build and publication

GitHub Actions uses exactly these repository secrets:

- **DOCKERHUB_USERNAME**
- **DOCKERHUB_TOKEN**

Pull requests run Go formatting checks, tests, static analysis, fixture
integration tests, and an AMD64 image build without publishing.

Successful main builds publish:

- **saltydk/vrising:latest**
- An immutable build tag containing the GitHub run ID and short source commit.

A weekly scheduled no-cache build refreshes moving WineHQ and SteamCMD packages
from their configured repositories. The pinned Ubuntu and Go image digests move
only through reviewed repository dependency updates; the workflow never rewrites
source or dependency pins. The same verification gates apply before publication.

Publishing the image, configuring secrets, pushing commits, and operating the
production container are separate remote or operational actions and require
explicit authorization when implementation reaches those boundaries.

## Non-goals

- Linux ARM64 support or CPU emulation.
- A general-purpose Thunderstore mod manager.
- Independent dependency upgrades that override either root manifest.
- Partial root updates that activate only KindredCommands or Satisvampory.
- Automatic save restoration.
- Automatic image replacement from inside a running container.
- Reimplementing every TrueOsiris environment-to-JSON feature.
- Editing or publishing upstream projects.
