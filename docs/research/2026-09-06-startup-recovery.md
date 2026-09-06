# Root compatibility and failed-start recovery

## What testing missed

The published image contained two independent defects. It selected a runtime
UID from the server mount owner despite starting as root, and configuration
recovery rejected legitimate changes to `BepInEx.cfg`.

The final local connection tests used root-owned mounts, initialized mod state,
and `UPDATE_GAME=false` / `UPDATE_MODS=false`. They proved player connectivity,
not first-install failure recovery with the supplied update-enabled Compose.
Their four-CPU/14-GiB limits also differed from the supplied deployment; that
difference is not established as a cause.

Real fresh-install tests also ran, but their successful startup paths did not
exercise rollback after BepInEx rewrote its configuration. A separate retained
local failure already contained the exact error later reported in deployment:

```text
server did not become ready
rollback failed candidate: recover managed-file transaction: recover config file BepInEx/config/BepInEx.cfg: new config was changed externally
```

That unresolved result was missed before publication. The local failure also
reported an interop-generation exception, but the original deployment's first
startup failure has not been established from the supplied log excerpt. Running
as root did not prevent the configuration-recovery defect in the local case.

## Corrected behavior

### Follow-up: pending transactions retained the previous runtime owner

The first published correction still failed on existing non-root-owned pending
transactions. Its root-adoption test used a completed transaction; its interrupted
transaction test ran under root throughout. Neither combined a pending transaction
with an identity change. Root then hit an application policy error rather than an
operating-system access failure:

```text
validate transaction artifacts: transaction artifact namespace must be runtime-owned mode 0700
```

`TestRootRecoveryAdoptsPendingNonRootTransaction` reproduced this exact failure
twice before the follow-up correction. Root now bypasses UID/mode policy checks
across state, artifacts, cache, and backups, while retaining path and integrity
validation. Non-root runtimes keep their protections, with recursive repair
performed as root under the lifetime lock before privileges are dropped. Existing
ownership markers no longer skip repairs. Non-root Docker user overrides are
rejected; `PUID`/`PGID` remains the supported opt-in interface.

The following describes the earlier correction and its original test evidence;
the follow-up acceptance results must be recorded separately, not inferred from it.

- Without explicit `PUID`/`PGID`, retain the container identity: root with the
  supplied image and Compose. Mount ownership no longer changes the process UID.
- Explicit ownership migration and privilege dropping remain opt-in. Health
  and verification commands honor those explicit IDs too.
- Prepare the private runtime directories for the selected identity without
  recursively changing game/save ownership by default.
- Preserve a current regular `BepInEx.cfg` even when its contents differ from
  the installed template. Retain its verified original backup when superseded.
- Restore a missing original configuration from its verified backup. Corrupt
  artifacts, unsafe paths, and altered managed binaries remain errors.
- Clear a successfully recovered transaction. A failed package lock is no longer
  permanently blocked; the next restart can attempt it again.

## Reproduction and verification

From the repository root, run:

```bash
go test -run 'TestResolveIdentityRetainsContainerIdentityByDefault|TestDefaultRootAdoptsPrivatePrefixWithoutChowningGameData|TestRollbackPreservesRuntimeWrittenConfig|TestConfigRecoveryRestoresMissingOriginalAndRejectsCorruptBackup|TestRunRetriesPreviouslyFailedLockOnRestart' -count=1
make check
go test -race ./...
sudo -E make container-test
sudo -E env "PATH=$PATH" make root-test
```

The configuration regressions apply an actual managed-file transaction, rewrite
the configuration, and recover twice. They cover newly created and pre-existing
configurations, preserved original backups, missing configuration, and corrupt
recovery artifacts. The root regression uses non-root-owned directories and
checks that only private runtime directories are adopted.

A real-container reproduction used a copy of an existing test world with
UID-1000-owned mount roots and updates enabled. The controller, Xvfb, and game
all ran as UID 0. Once real BepInEx changed the configuration hash but before
readiness, startup was terminated. Recovery cleared the candidate/transaction
and retained the rewritten configuration. Restarting the same container retried
the same package set and reached healthy status with both mods loaded.

A subsequent run explicitly selected UID/GID 1000. Both health and deep
verification succeeded when invoked through Docker's default root exec user;
the read-only commands launch under the explicit runtime credentials. After
stopping that container, another run omitted the ownership settings on the
same installation. The controller and game returned to UID 0, became healthy,
and passed deep verification with the previously non-root-owned prefix.

Only one game container ran at a time, and source saves were not changed.

## Affected deployments

### Follow-up acceptance

The root-bootstrap follow-up passed unit tests, vet, race tests, privileged
root/non-root regressions, image-contract validation, and container fixtures.
The fixtures now include non-root Docker-user rejection, pending recovery with
legacy owners and permissive modes, and explicit-ID transitions. Separate lock
tests cover competing instances with either a shared server mount or shared
saves and different server mounts.

Root permission-policy tests cover private namespaces and both installed and
staged managed-file modes. Non-root subprocess tests retain permission-policy
rejection; integrity and unsafe-path tests remain required under both identities.

Real fresh installation, offline restart, and transitions through UID/GID 1000,
1001, and default root passed against this immutable local candidate:

```text
sha256:4920b88d23a39ac745c6d35b363520246f9311dba59837b18a0e39c1739aed31
```

Each identity reached readiness with both mods, passed deep verification and
controller/game UID checks, and stopped cleanly before its replacement started.
The acceptance helper now pins the input image tag to its image ID before any
container is created, so later local builds cannot change the tested image or
the reported identity.

Migration acceptance also passed against that same image ID using a disposable
copy of an existing `world1` save. It verified loading the existing save,
default-root preservation of settings, and the same 1000 → 1001 → root runtime
transitions. Source settings/saves snapshots were unchanged. All acceptance
containers and disposable copies were removed after clean shutdown.

### Deployment recovery

Keep a looping deployment stopped and preserve its settings, saves, mod
configuration, and complete controller state (including recovery artifacts)
before installing a corrected image. Existing journals are handled by recovery;
do not delete `.docker-vrising` or replace `BepInEx.cfg` to bypass the error.

The primary startup failure still needs the original game/BepInEx logs or an
affected-copy reproduction. Successful recovery is not evidence that every
underlying startup problem has been resolved.
