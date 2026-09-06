# V Rising image: upstream and mod-stack research

**Retrieved:** 2026-09-05 (UTC)
**Scope:** design inputs for a new Linux container running the Windows V Rising dedicated server under Wine, with optional KindredCommands.
**Evidence rule:** factual statements below use only the requested first-party sources. “Inference” and “Recommendation” are engineering conclusions, not upstream promises.

## Executive conclusion

TrueOsiris is a useful behavioral reference, especially its two-volume split, persistent settings, log forwarding, and attempt to forward `SIGTERM`. Its current implementation should not be copied as the new image’s control plane: it runs as root, updates the live server tree unconditionally and without checking SteamCMD success, has no health check or save backup, does not configure the Wine DLL override required by BepInEx, and has no transactional activation or rollback path. These observations come directly from its pinned [`Dockerfile`](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/Dockerfile), [`start.sh`](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh), and [`docker-compose.yml`](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/docker-compose.yml).

The exact current KindredCommands closure is `odjit-KindredCommands-2.5.8` → `BepInEx-BepInExPack_V_Rising-1.733.2` plus `deca-VampireCommandFramework-0.10.4`, with VCF → the same BepInEx pack and BepInEx → no dependencies. This is an exact-version graph, not a request for each package’s latest release; VCF’s package-level latest is now `0.11.0`, so replacing the declared `0.10.4` would violate the root manifest. [Kindred version API](https://thunderstore.io/api/experimental/package/odjit/KindredCommands/2.5.8/) · [VCF 0.10.4 API](https://thunderstore.io/api/experimental/package/deca/VampireCommandFramework/0.10.4/) · [BepInEx 1.733.2 API](https://thunderstore.io/api/experimental/package/BepInEx/BepInExPack_V_Rising/1.733.2/) · [VCF package API](https://thunderstore.io/api/experimental/package/deca/VampireCommandFramework/)

Compatibility with today’s server is **not established by the allowed sources**. BepInExPack `1.733.2` is the RC2/Oakveil (V Rising 1.1) build, while an official SteamCMD observation on 2026-09-05 returned public server build `24686592` and depot `1829351` manifest `1049007059193047563`. The safe design is therefore to stage and smoke-test the exact server/mod tuple before activation, and otherwise retain the last-known-good tuple. [BepInExPack 1.733.2 release](https://github.com/decaprime/VRising-Modding/releases/tag/1.733.2) · [Valve SteamCMD download procedure](https://developer.valvesoftware.com/wiki/SteamCMD#Downloading_an_app)

The replacement also has a fixed deployment-compatibility boundary supplied for this project: retain host bind mounts `/opt/v-rising/server` → `/mnt/vrising/server` and `/opt/v-rising/data` → `/mnt/vrising/persistentdata`; preserve `TZ`, `SERVERNAME`, and `WINEDEBUG`; and continue publishing UDP `9876`/`9877` plus TCP `25575`. The entrypoint and the rest of the TrueOsiris environment-variable API are not compatibility requirements.

## 1. TrueOsiris upstream

### Runtime-baseline addendum (2026-09-06)

After the first production-image acceptance exposed fresh-prefix Wine behavior,
the runtime audit was expanded to AndrewSav's TrueOsiris-derived image at
commit [`cc2e8e5e4a2079e2567d954338479ec2a83d2126`](https://github.com/AndrewSav/vrising-docker/commit/cc2e8e5e4a2079e2567d954338479ec2a83d2126).
That fork is a useful second executable baseline: it installs WineHQ stable
with recommended packages, retries SteamCMD, copies the tested BepInEx pack
layout, exports `WINEDLLOVERRIDES=winhttp=n,b`, and explicitly invokes
`winecfg` before the server. [Dockerfile](https://github.com/AndrewSav/vrising-docker/blob/cc2e8e5e4a2079e2567d954338479ec2a83d2126/Dockerfile) ·
[entrypoint](https://github.com/AndrewSav/vrising-docker/blob/cc2e8e5e4a2079e2567d954338479ec2a83d2126/entrypoint.sh)

The initial criticism of the fork's pre-Xvfb `winecfg` sequence was not supported
by a full runtime test. A subsequent test of its actual published image (Wine
10.0), default entrypoint, `ENABLE_MODS=1`, three mounts, and independently
verified packages successfully loaded all mods and started the game. The full
stack remained running for five minutes after readiness and wrote autosaves.
The earlier Wine 11 approximation also lacked installed mod DLLs after rollback,
so it cannot establish mod compatibility or superiority of DLL suppression.

The independent Ubuntu image initially reproduced WineHQ 10.0 and the tested
`winecfg`, five-second delay, then Xvfb sequence. Forced `mscoree`/`mshtml`
suppression is not part of the selected runtime. Current VCF startup output is
`[Message:VampireCommandFramework] VCF Loaded: 0.10.4`; the earlier synthetic
`is loaded!` fixture was incorrect. See the
[runtime acceptance report](2026-09-06-runtime-acceptance.md) for the subsequent
Wine 11 comparison, selected runtime, reproduction commands, and evidence limits.

### Snapshot and history

- Research was pinned to `main` commit [`cce267ba7c83eab33b0485cea063d22edfad7ee5`](https://github.com/TrueOsiris/docker-vrising/commit/cce267ba7c83eab33b0485cea063d22edfad7ee5), tree [`2ac715e322075b86f3bb4982af6ae01186c3d313`](https://api.github.com/repos/TrueOsiris/docker-vrising/git/trees/2ac715e322075b86f3bb4982af6ae01186c3d313), with 217 commits visible in the repository history. [Commit history](https://github.com/TrueOsiris/docker-vrising/commits/main/)
- The repository has no GitHub tags and no GitHub releases at retrieval time; Docker image tags mentioned in the README are a separate publication mechanism. [Tags API](https://api.github.com/repos/TrueOsiris/docker-vrising/tags) · [Releases API](https://api.github.com/repos/TrueOsiris/docker-vrising/releases) · [README](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/README.md)
- Relevant design lineage: persistent settings were added in [`8daea365087d7ba3c6f8d2ab6cb968adb4939976`](https://github.com/TrueOsiris/docker-vrising/commit/8daea365087d7ba3c6f8d2ab6cb968adb4939976); a `SIGTERM` shutdown path in [`22aaa1df076689576bedb3a9e55cf71a78545cd7`](https://github.com/TrueOsiris/docker-vrising/commit/22aaa1df076689576bedb3a9e55cf71a78545cd7); log retention in [`0b2daec77902d6f22bfd76f891020d47e04e6c7c`](https://github.com/TrueOsiris/docker-vrising/commit/0b2daec77902d6f22bfd76f891020d47e04e6c7c); environment-to-JSON settings in [`3e13d353aebe7e9fc4a497971c071b39aecda92e`](https://github.com/TrueOsiris/docker-vrising/commit/3e13d353aebe7e9fc4a497971c071b39aecda92e); and a compose-time CRLF workaround in [`271b37e79cf609df40e1e811f9ae5f469f2d0ea4`](https://github.com/TrueOsiris/docker-vrising/commit/271b37e79cf609df40e1e811f9ae5f469f2d0ea4).
- CI builds every push to `main` (and manual dispatch), publishes mutable `trueosiris/vrising:latest`, and also publishes a minute-resolution date tag; the workflow does not publish a source-SHA tag or a GitHub release. [Pinned workflow](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/.github/workflows/docker-image.yml)

### Architecture, startup, and update flow

| Area | Sourced behavior | Engineering assessment |
|---|---|---|
| Base image | Ubuntu 22.04; installs Ubuntu’s `steam`, `steamcmd`, `wine`, `winbind`, `winetricks`, Xorg/Xvfb, and `jq`; performs unpinned `apt upgrade` during the build. [Dockerfile](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/Dockerfile) | **Inference:** rebuilds are not reproducible, and the full Steam client/Xorg stack is broader than the runtime contract needs. |
| Startup update | Every container start runs anonymous SteamCMD for Windows app `1829350` with optional `-beta $BRANCH` and `validate`, directly against `/mnt/vrising/server`. The script has neither `set -e` nor an explicit status check before continuing. [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L52-L65) | **Inference:** a failed or partial update can be followed by a launch from a mixed live tree; there is no atomic cutover. |
| CPU workaround | If `/proc/cpuinfo` lacks an `avx*` token, the script renames `VRisingServer_Data/Plugins/x86_64/lib_burst_generated.dll` to `.bak`. [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L67-L75) | **Inference:** this is an undocumented binary mutation that Steam `validate` may restore on every start; treat it as an explicit compatibility mode, not a silent default. |
| Configuration | Missing persistent host/game JSON files are copied from the freshly updated server defaults. `GAME_SETTINGS_*` and `HOST_SETTINGS_*` variables then case-insensitively modify existing keys, use `__` for nesting, validate primitive/array types, and replace each JSON through a temporary file. [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L77-L230) · [README](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/README.md#newest-modifications) | Reusable schema-aware override idea. **Inference:** because overrides mutate the persistent files, removing an environment variable does not restore the former value; configuration intent and runtime state are not cleanly separated. |
| Launch | Starts Xvfb display `:0`, then `wine64 .../VRisingServer.exe` with `-persistentDataPath`, server name, optional save/ports, and a dated log; backgrounds Wine and tails that log to container stdout. [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L233-L262) | Reusable stdout log forwarding. **Inference:** Xvfb and `tail` are not reaped or explicitly stopped, and optional multi-token flags are passed as single quoted arguments, so this needs replacement with an argument array and exact child tracking. |
| Signals | PID 1 traps only `SIGTERM`, searches with `pgrep -f`, sends signal 15, waits, then calls `wineserver -k`; the handler exits without preserving a child exit status. [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L5-L25) | The intent is reusable. **Inference:** process discovery can miss or select the wrong process, and `SIGINT`, abnormal exit, timeout escalation, descendant reaping, and signal-derived exit status are not handled. |
| Image entry | The Dockerfile declares `CMD ["/start.sh"]`, but current examples override it with a shell that strips CR characters before executing the script. [Dockerfile](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/Dockerfile#L36-L38) · [docker-compose.yml](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/docker-compose.yml#L3-L5) | **Inference:** the published default and the documented working invocation have diverged. Normalize line endings at build time. |

### Filesystem, ports, and environment contract

- `/mnt/vrising/server` is the mutable Steam installation, and `/mnt/vrising/persistentdata` holds the world, `Settings/`, and dated logs; both are Docker `VOLUME`s. The README warns that Steam updates overwrite files under the server path while the persistent settings take precedence over server defaults. [Dockerfile](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/Dockerfile#L1-L3) · [README volumes and remarks](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/README.md#volumes)
- The compose contract publishes UDP `9876` and `9877`; `GAMEPORT` and `QUERYPORT` create command-line overrides, while `SERVERNAME` defaults to `trueosiris-V` and `WORLDNAME` optionally sets `-saveName`. [README environment and ports](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/README.md#environment-variables) · [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L36-L55)
- `LOGDAYS` defaults to 30 and deletes matching `*.log` below the entire persistent tree; `BRANCH` selects a Steam beta; `TZ` and `WINEDEBUG` are passed through in examples; the two JSON prefixes are described above. [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L20-L29) · [README](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/README.md#environment-variables) · [docker-compose.yml](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/docker-compose.yml)
- `UID`/`GID` handling is internally inconsistent: the image creates `steam` but never switches to it, while startup attempts to change a `docker` user/group; Bash’s `UID` is also intrinsically populated. [Dockerfile](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/Dockerfile#L15-L24) · [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L30-L35) **Inference:** the advertised ownership remapping cannot provide a reliable non-root runtime.

### Existing deployment contract to preserve

| Existing host setting | Required replacement behavior |
|---|---|
| `/opt/v-rising/server:/mnt/vrising/server` | Keep the bind target and all existing bytes. Versioned candidates, locks, and the active pointer may live beneath this mount, but migration must not erase or overwrite the legacy live tree before a candidate is proven. |
| `/opt/v-rising/data:/mnt/vrising/persistentdata` | Keep this exact persistent-data path for saves, `Settings/`, and logs. Backups and probes must not mutate the only copy. |
| `TZ`, `SERVERNAME`, `WINEDEBUG` | Preserve their current meanings/pass-through behavior. Additional configuration may use a new, narrower interface. |
| UDP `9876`, UDP `9877`, TCP `25575` | Preserve game/query and RCON publication. A readiness probe may use RCON on `25575` when enabled, but must still work without making RCON a new requirement. |

### Health, backup, update, and security posture

- There is no `HEALTHCHECK` in the Dockerfile and no compose health check; startup readiness is represented only by the running foreground shell/Wine child and log output. [Dockerfile](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/Dockerfile) · [docker-compose.yml](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/docker-compose.yml)
- There is no save/config backup or release rollback in the complete current tree; the only retention routine deletes old logs. [Pinned tree](https://api.github.com/repos/TrueOsiris/docker-vrising/git/trees/2ac715e322075b86f3bb4982af6ae01186c3d313?recursive=1) · [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L20-L23)
- Runtime remains root because the Dockerfile has no `USER`; it recursively makes `/root/.steam` mode `777`. Package installation performs full upgrades and installs from moving Ubuntu repositories without version pins. [Dockerfile](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/Dockerfile) · [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L58-L63)
- The image does not install BepInEx or set `WINEDLLOVERRIDES`; official BepInEx guidance says Wine requires configuring its `winhttp.dll` proxy and recommends Proton, although Wine is not excluded. [Pinned BepInEx Wine guidance](https://github.com/BepInEx/bepinex-docs/blob/f6050e7aa2eb7cccd7c9e2602ad00438871809ad/articles/advanced/steam_interop.md#protonwine) · [start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh)

## 2. Exact KindredCommands closure

### Identities, URLs, and downloaded bytes

The UUIDs and byte sizes below came from the V Rising category API; dependency and active-state claims are independently exposed by each version-specific API. [Category API](https://thunderstore.io/c/v-rising/api/v1/package/)

| Exact package | Declared direct dependencies | UUID / size | First-party download and retrieved metadata | Locally computed SHA-256 |
|---|---|---|---|---|
| `odjit-KindredCommands-2.5.8` | `BepInEx-BepInExPack_V_Rising-1.733.2`; `deca-VampireCommandFramework-0.10.4` [API](https://thunderstore.io/api/experimental/package/odjit/KindredCommands/2.5.8/) | `a251850f-8743-43fa-8526-ff75dffbc5ec`; 1,384,283 bytes [API](https://thunderstore.io/c/v-rising/api/v1/package/) | [CDN ZIP](https://gcdn.thunderstore.io/live/repository/packages/odjit-KindredCommands-2.5.8.zip); `Last-Modified: 2025-07-09 21:28:15 GMT`; ETag `ae1e9e16dadf377dd67d4010cc762e26` (also the downloaded file’s MD5) | `80b9cf909389a4869f45b052e393164079db82a68e962f4a18a2619b0924e7b1` |
| `deca-VampireCommandFramework-0.10.4` | `BepInEx-BepInExPack_V_Rising-1.733.2` [API](https://thunderstore.io/api/experimental/package/deca/VampireCommandFramework/0.10.4/) | `bb02d61f-a8ac-4693-96a7-a68c6d65348c`; 45,335 bytes [API](https://thunderstore.io/c/v-rising/api/v1/package/) | [CDN ZIP](https://gcdn.thunderstore.io/live/repository/packages/deca-VampireCommandFramework-0.10.4.zip); `Last-Modified: 2025-06-20 23:33:01 GMT`; ETag `905b57c89fad84513a120578484810ed` (also MD5) | `e554e964242db0b1cfef5824129aa6794c8a7138c9321b617d633f9aaffac19b` |
| `BepInEx-BepInExPack_V_Rising-1.733.2` | none [API](https://thunderstore.io/api/experimental/package/BepInEx/BepInExPack_V_Rising/1.733.2/) | `6d556719-d76d-4266-aad0-fe7678980649`; 33,503,624 bytes [API](https://thunderstore.io/c/v-rising/api/v1/package/) | [CDN ZIP](https://gcdn.thunderstore.io/live/repository/packages/BepInEx-BepInExPack_V_Rising-1.733.2.zip); `Last-Modified: 2025-05-17 18:12:34 GMT`; multipart ETag `1d4bb5b644a8edc6bef0026cb2e14b22-4` (not an MD5) | `eabfb53d80bed427ddbd1ecf8af4dcd6e82e152809d44ab3218d0a09984f491c` |

All three version APIs reported `is_active: true` on retrieval. Their human-readable first-party pages are [Kindred 2.5.8](https://thunderstore.io/c/v-rising/p/odjit/KindredCommands/v/2.5.8/), [VCF 0.10.4](https://thunderstore.io/c/v-rising/p/deca/VampireCommandFramework/v/0.10.4/), and [BepInExPack 1.733.2](https://thunderstore.io/c/v-rising/p/BepInEx/BepInExPack_V_Rising/v/1.733.2/).

### `manifest.json` shape

Each ZIP has a root `manifest.json` containing at least `name`, `version_number`, `description`, `website_url`, and `dependencies`; VCF additionally includes `namespace` and `FullName`. These are the exact identity/dependency portions downloaded on 2026-09-05. [Kindred ZIP](https://gcdn.thunderstore.io/live/repository/packages/odjit-KindredCommands-2.5.8.zip) · [VCF ZIP](https://gcdn.thunderstore.io/live/repository/packages/deca-VampireCommandFramework-0.10.4.zip) · [BepInEx ZIP](https://gcdn.thunderstore.io/live/repository/packages/BepInEx-BepInExPack_V_Rising-1.733.2.zip)

```json
{
  "KindredCommands": {
    "version_number": "2.5.8",
    "dependencies": [
      "BepInEx-BepInExPack_V_Rising-1.733.2",
      "deca-VampireCommandFramework-0.10.4"
    ]
  },
  "VampireCommandFramework": {
    "namespace": "deca",
    "version_number": "0.10.4",
    "dependencies": ["BepInEx-BepInExPack_V_Rising-1.733.2"],
    "FullName": "deca-VampireCommandFramework"
  },
  "BepInExPack_V_Rising": {
    "version_number": "1.733.2",
    "dependencies": []
  }
}
```

### Archive contents and install destinations

| Package | Exact archive shape | Manual destination |
|---|---|---|
| KindredCommands | Seven root files: `KindredCommands.dll`, `NetTopologySuite.dll`, `manifest.json`, `README.md`, `THIRD-PARTY-NOTICES.md`, `CHANGELOG.md`, `icon.png`. [ZIP](https://gcdn.thunderstore.io/live/repository/packages/odjit-KindredCommands-2.5.8.zip) | Put `KindredCommands.dll` in `<game>/BepInEx/plugins`. `NetTopologySuite.dll` is optional and belongs in the same directory; the package says it is needed for `.forcerespawn`. [Version page](https://thunderstore.io/c/v-rising/p/odjit/KindredCommands/v/2.5.8/) |
| VCF | Four root files: `VampireCommandFramework.dll`, `manifest.json`, `README.md`, `icon.png`. [ZIP](https://gcdn.thunderstore.io/live/repository/packages/deca-VampireCommandFramework-0.10.4.zip) | Put `VampireCommandFramework.dll` in `<game>/BepInEx/plugins`. [Version page](https://thunderstore.io/c/v-rising/p/deca/VampireCommandFramework/v/0.10.4/) |
| BepInExPack | 243 ZIP entries (236 files, seven directories), including root package metadata and an inner `BepInExPack_V_Rising/` containing `.doorstop_version`, `BepInEx/{config,core,patchers,plugins}`, `changelog.txt`, `doorstop_config.ini`, bundled Windows `dotnet/`, and `winhttp.dll` (217 DLL entries in total). [ZIP](https://gcdn.thunderstore.io/live/repository/packages/BepInEx-BepInExPack_V_Rising-1.733.2.zip) | Move the **contents** of `BepInExPack_V_Rising/` into the game root beside `VRisingServer.exe`, not the wrapper directory itself. [Version page](https://thunderstore.io/c/v-rising/p/BepInEx/BepInExPack_V_Rising/v/1.733.2/) |

The BepInEx pack identifies itself as BepInEx `6.0.0-be.733`, uses CoreCLR/.NET 6 and Unity IL2CPP, and configures Doorstop to load `BepInEx\core\BepInEx.Unity.IL2CPP.dll`; its official release is tag commit [`1eb6f2bf058f1a341f703747ac0148ad3c4ac2c0`](https://github.com/decaprime/VRising-Modding/commit/1eb6f2bf058f1a341f703747ac0148ad3c4ac2c0), built from BepInEx fork `3a12d1810bf4dd5ca1635352ccf5bff5d95f987f`, Cpp2IL `1486c69f8d11d06b349b57912c24f09a3c555833`, and Il2CppInterop `6b6a9f1a1a2959fa5183a329203deef7a5e1ed65`. [Official release](https://github.com/decaprime/VRising-Modding/releases/tag/1.733.2) · [BepInEx ZIP](https://gcdn.thunderstore.io/live/repository/packages/BepInEx-BepInExPack_V_Rising-1.733.2.zip)

### Deterministic “latest” resolution

**Recommendation:** Treat “latest KindredCommands” as a one-time root resolution, then lock the entire exact closure:

1. GET the [Kindred package endpoint](https://thunderstore.io/api/experimental/package/odjit/KindredCommands/) once and record `latest.full_name` plus retrieval time.
2. GET that exact [version endpoint](https://thunderstore.io/api/experimental/package/odjit/KindredCommands/2.5.8/). Traverse every dependency string as an immutable full-name/version requirement; deduplicate exact full names and reject conflicts or cycles.
3. Never ask a dependency’s package endpoint for its own `latest`: doing so would currently choose VCF `0.11.0`, contradicting Kindred’s `0.10.4` declaration. [VCF package endpoint](https://thunderstore.io/api/experimental/package/deca/VampireCommandFramework/)
4. Download only each version API’s URL, follow it to the first-party CDN, enforce recorded size, reject unsafe ZIP paths/symlinks/duplicates, check archive integrity, and verify the embedded manifest identity and dependency list before extraction.
5. Persist a lock containing root selection, exact closure, version UUIDs, CDN URLs, sizes, `Last-Modified`, ETags, and SHA-256 values. A repeated deployment consumes the lock and rejects changed bytes. A deliberate refresh creates a new lock and candidate release; it never mutates the prior lock in place.

## 3. Compatibility findings and limits

- **Server:** an anonymous `app_info_print 1829350` through Valve’s official SteamCMD on 2026-09-05 identified a released Windows x64 tool (parent app `1604030`), public build ID `24686592`, and depot `1829351` manifest GID `1049007059193047563`; Steam’s reported build-update time was 2026-08-12T06:18:38Z. The captured command output SHA-256 was `e7a9b4a11a1e19c55da660ab5654db34e0ae4d8dd99023c95f753932209a41d2`. [Official SteamCMD archive](https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz) · [Valve SteamCMD app-info procedure](https://developer.valvesoftware.com/wiki/SteamCMD#App_info)
- **BepInEx versus current server:** `1.733.2` is explicitly an RC2 for V Rising 1.1/Oakveil and a bleeding-edge build rather than a stable BepInEx release. The permitted sources contain no successful test against Steam build `24686592`; therefore current compatibility is **unknown**, not confirmed. [Official 1.733.2 release](https://github.com/decaprime/VRising-Modding/releases/tag/1.733.2) · [BepInEx version page](https://thunderstore.io/c/v-rising/p/BepInEx/BepInExPack_V_Rising/v/1.733.2/)
- **VCF and Kindred:** their manifests form a self-consistent exact graph around BepInEx `1.733.2`; that establishes declared dependency compatibility only, not runtime compatibility with a newer server binary. [Kindred API](https://thunderstore.io/api/experimental/package/odjit/KindredCommands/2.5.8/) · [VCF API](https://thunderstore.io/api/experimental/package/deca/VampireCommandFramework/0.10.4/)
- **Bloodstone and launch helpers:** Bloodstone and launchers are absent from the complete recursive dependency closure, so they should not be installed implicitly. Kindred’s own instructions mention `ServerLaunchFix` only for single-player; a dedicated server needs BepInEx, VCF, and Kindred, not that helper. [Kindred version page](https://thunderstore.io/c/v-rising/p/odjit/KindredCommands/v/2.5.8/)
- **Wine/Linux:** BepInEx’s official documentation requires `winhttp` proxy forwarding under Wine and gives `WINEDLLOVERRIDES="winhttp.dll=n,b"`; the BepInExPack supplies the Windows proxy/runtime files. TrueOsiris launches plain `wine64` without that override, so mod loading is not assured by the upstream image. [Pinned BepInEx Wine guidance](https://github.com/BepInEx/bepinex-docs/blob/f6050e7aa2eb7cccd7c9e2602ad00438871809ad/articles/advanced/steam_interop.md#protonwine) · [BepInEx ZIP](https://gcdn.thunderstore.io/live/repository/packages/BepInEx-BepInExPack_V_Rising-1.733.2.zip) · [TrueOsiris start.sh](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/start.sh#L249-L262)

## 4. Recommended transactional startup/update design

Everything in this section is an **engineering recommendation** derived from the evidence above.

### State and privilege boundaries

- Run a small init as PID 1 and run SteamCMD/Wine as a fixed unprivileged service account. Make ownership an explicit first-start check; do not remap a nonexistent account or make root state world-writable.
- Keep four logical areas while preserving the existing two bind mounts: read-only image tooling; versioned server releases and update metadata under `/mnt/vrising/server`; persistent game data at `/mnt/vrising/persistentdata`; and checksummed backups that cannot be reached by log cleanup. Never update the active release directory in place, and never repurpose or discard pre-existing bytes during first migration.
- Keep desired configuration read-only and render validated JSON into a candidate/staging area. Do not make removal of an environment variable leave an invisible persistent override. Prefer mounted secrets for RCON passwords rather than environment variables.

### Cold-start transaction

1. Acquire an update lock; verify no server process is alive, all required paths are writable, free space is sufficient for candidate plus backup, configuration parses, and the previous activation pointer is coherent.
2. Resolve the requested Steam branch/build policy and the exact mod lock. Default to an already approved lock; make “refresh latest” an explicit mode. Record the observed Steam build/depot tuple and complete mod closure before changing active state.
3. Create a fresh candidate directory. Run SteamCMD against it and treat every non-zero status, missing `VRisingServer.exe`, unexpected app/build metadata, or incomplete validation as fatal. Never proceed merely because some files exist.
4. Download every mod archive to temporary files, verify HTTPS result, size, ZIP safety, embedded manifest, exact closure, and locked SHA-256. Extract the inner BepInEx pack into the candidate game root, then only the required VCF/Kindred DLLs into `BepInEx/plugins`; include `NetTopologySuite.dll` only when its optional command is requested. Set the Wine `winhttp` native-then-builtin override whenever mods are enabled.
5. While the server is stopped, create a checksummed snapshot of persistent settings and saves. Retain at least the last known-good pre-upgrade snapshot; never let routine log pruning traverse backup storage.
6. Run a bounded candidate probe with a copy/snapshot of persistent data. Require: the exact server process remains alive, expected UDP sockets/readiness evidence appears, BepInEx initializes when enabled, VCF and Kindred load without fatal errors, and no crash signature occurs. A PID alone is insufficient. Allow a long startup grace period because upstream warns startup can take up to ten minutes. [Upstream README](https://github.com/TrueOsiris/docker-vrising/blob/cce267ba7c83eab33b0485cea063d22edfad7ee5/README.md#remarks)
7. Promote by atomic rename plus an atomic `current` pointer switch; preserve `previous`. Start the promoted release against real persistent data and emit the selected server build, mod lock digest, candidate ID, and fallback decision in structured logs.

### Process, health, and shutdown

- Build launch arguments as an array. Track the exact Wine server and X server PIDs/process group; forward `TERM` and `INT`, wait for a configurable graceful timeout, then escalate once and reap all children. Return the server’s real exit status; reserve distinct non-zero statuses for update, mod-resolution, readiness, and shutdown failures.
- Readiness should stay false during update/probe and become true only after process plus protocol/log evidence. Liveness should verify the exact server process and an advancing or responsive server signal, with a start period large enough for existing saves. Health must report “update failed, serving previous” distinctly from “candidate current.”
- Never update while the game is running. Offer `never`, `startup-if-needed`, and explicit maintenance modes; do not hide a network update inside every restart.

### Rollback and failure policy

- **Before candidate launch:** any SteamCMD, download, checksum, archive, manifest, configuration, or dependency error leaves `current` untouched. If policy allows, run the unchanged last-known-good release and report degraded/update-failed status; a first installation fails stopped.
- **Probe failure on copied data:** preserve candidate files and logs for diagnosis, discard only on explicit retention policy, and keep serving the prior release.
- **Failure after launch on real data:** stop and preserve both the pre-launch snapshot and failed data. Do not automatically restore saves: the new server may already have migrated or written them, and blind restore could discard legitimate writes. Require an operator decision to restore data; binary-pointer rollback alone is safe only when the system can prove real persistent data was not mutated.
- **Compatibility policy:** a new Steam tuple with an unchanged Oakveil-era mod lock is a compatibility change requiring a successful probe. If no compatible mod tuple is proven, fail closed or start the prior complete server+mod tuple according to an explicit operator policy; never silently disable mods or substitute VCF `0.11.0`.

## Evidence gaps

The allowed primary sources do not provide a compatibility matrix for Steam build `24686592` versus BepInExPack `1.733.2`, VCF `0.10.4`, or KindredCommands `2.5.8`; nor do they provide an upstream health/readiness protocol or save-migration rollback guarantee. Those gaps are why the proposed design makes compatibility an executable acceptance gate and treats persistent-data rollback as an operator-visible decision.
