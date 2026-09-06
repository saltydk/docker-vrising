# Docker V Rising Image Implementation Plan

Historical plan: its mount-owner inference and one-time ownership migration
were superseded on 2026-09-06. Use the current root-bootstrap and permission-repair
contract in the [README](../../../README.md); do not reintroduce those behaviors.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and verify a drop-in Docker Hub image that updates the V Rising dedicated server and installs the newest compatible KindredCommands and Satisvampory dependency graphs safely on every restart.

**Architecture:** A flat Go 1.27 control-plane binary runs beneath tini and owns configuration, durable update state, Thunderstore resolution, archive validation, save snapshots, SteamCMD execution, managed mod activation, health, and Wine/Xvfb supervision. Existing game and save mounts remain in place; mod changes are journaled and reversible, while an in-place Steam mutation fails closed if validation cannot complete.

**Tech Stack:** Go 1.27, golang.org/x/sys, Ubuntu 22.04, WineHQ stable, SteamCMD, Xvfb, tini, Docker Buildx, GitHub Actions, Docker Hub

**Spec:** [docs/superpowers/specs/2026-09-05-docker-vrising-image-design.md](../specs/2026-09-05-docker-vrising-image-design.md)

## Global Constraints

- Target Linux AMD64 only.
- Keep /mnt/vrising/server and /mnt/vrising/persistentdata unchanged.
- Preserve existing saves, settings, and server files during migration.
- Resolve the newest KindredCommands and Satisvampory roots independently,
  merge their exact recursive dependency graphs, and reject conflicts before
  downloading archives.
- Never independently upgrade VCF, BepInEx, or another transitive dependency.
- Run game and mod downloads at container startup; do not embed or redistribute them in the image.
- Use clean-room code; do not copy the unlicensed TrueOsiris implementation.
- Fall back only when failure occurs before live Steam or managed mod mutation.
- Never restore saves automatically.
- Keep the newest three pre-Steam-upgrade backups by default.
- Use DOCKERHUB_USERNAME and DOCKERHUB_TOKEN as GitHub Actions secrets.
- Publish only linux/amd64 images to saltydk/vrising.
- Every commit must use a Conventional Commit subject.

## Non-goals

- A general-purpose Thunderstore mod manager.
- Independent dependency upgrades that override either root manifest.
- Partial activation of only one managed root.

---

## File Map

All Go files use package main so the application remains flat and navigable.

- **go.mod**, **go.sum**: module github.com/saltydk/docker-vrising, Go 1.27, x/sys dependency.
- **main.go**: run, health, and version command dispatch plus stable exit codes.
- **config.go**: environment parsing, defaults, legacy aliases, launch environment, and fixed paths.
- **identity.go**: mount-owner inference, optional PUID/PGID migration, and privilege drop.
- **state.go**: JSON schemas, atomic persistence, whole-lifetime flock, and interrupted-transaction recovery metadata.
- **thunderstore.go**: package client, dependency parsing, exact graph traversal, conflict/cycle detection.
- **archive.go**: bounded HTTP retrieval, cache integrity, ZIP validation, and safe extraction.
- **mods.go**: BepInEx layout mapping, managed-file inventory, staging, journaled apply/rollback, and known-good promotion.
- **backup.go**: streaming tar.gz snapshots, free-space gate, verification, and retention.
- **steam.go**: installed/remote build parsing, SteamCMD check/update, retry policy, and validation.
- **logs.go**: prefixed log streaming and readiness evidence.
- **process.go**: Xvfb allocation, Wine process group, signal forwarding, timeout escalation, and reaping.
- **health.go**: process identity checks and Docker health result.
- **run.go**: the ordered startup/update/activation transaction.
- **testdata/**: exact Thunderstore, Steam, ZIP, and log fixtures.
- **Dockerfile**, **.dockerignore**, **compose.yaml**: runtime image and drop-in example.
- **Makefile**: repeatable local quality, build, image, and acceptance commands.
- **hack/container-fixture-test.sh**: deterministic image-level acceptance.
- **hack/live-acceptance.sh**: disposable full-runtime and migration acceptance.
- **.github/workflows/ci.yml**: non-publishing checks.
- **.github/workflows/publish.yml**: tested Docker Hub publication.
- **.github/dependabot.yml**: reviewed Go, Docker, and Actions dependency updates.
- **README.md**: migration, configuration, update semantics, backup recovery, and publication behavior.

### Task 1: Initialize the Go control plane and configuration contract

**Files:**

- Create: **go.mod**
- Create: **main.go**
- Create: **config.go**
- Create: **config_test.go**
- Create: **Makefile**
- Create: **.gitignore**

**Interfaces:**

- Produces: **Config**, **LoadConfig(map[string]string) (Config, []string, error)**, **EnvironmentMap([]string) map[string]string**, and the run/health/version command dispatch.
- Produces exit codes: usage 2, preflight 10, mod update 20, backup 21, Steam update 22, readiness 30, and shutdown 31.

- [ ] **Step 1: Write failing configuration tests**

Create table-driven tests with these representative exact assertions:

~~~go
func TestLoadConfigDefaults(t *testing.T) {
    cfg, warnings, err := LoadConfig(map[string]string{})
    if err != nil {
        t.Fatal(err)
    }
    if len(warnings) != 0 {
        t.Fatalf("warnings = %v", warnings)
    }
    if cfg.ServerDir != "/mnt/vrising/server" ||
        cfg.DataDir != "/mnt/vrising/persistentdata" ||
        !cfg.UpdateGame || !cfg.UpdateMods || !cfg.ModsEnabled ||
        cfg.KindredVersion != "latest" ||
        cfg.SatisvamporyVersion != "latest" ||
        cfg.BackupRetention != 3 ||
        cfg.StartupTimeout != 30*time.Minute ||
        cfg.ShutdownTimeout != 120*time.Second ||
        cfg.LogDays != 30 {
        t.Fatalf("unexpected defaults: %#v", cfg)
    }
}

func TestLoadConfigNativeValuesWinOverLegacyAliases(t *testing.T) {
    cfg, warnings, err := LoadConfig(map[string]string{
        "SERVERNAME": "legacy",
        "VR_SERVER_NAME": "native",
        "WORLDNAME": "legacy-world",
        "VR_SAVE_NAME": "native-world",
    })
    if err != nil {
        t.Fatal(err)
    }
    if cfg.GameEnv["VR_SERVER_NAME"] != "native" ||
        cfg.GameEnv["VR_SAVE_NAME"] != "native-world" ||
        len(warnings) != 2 {
        t.Fatalf("cfg=%#v warnings=%v", cfg, warnings)
    }
}
~~~

Add rejection cases for non-boolean values, zero/negative durations,
BACKUP_RETENTION below one, only one of PUID/PGID, non-numeric IDs, and a
KINDRED_COMMANDS_VERSION or SATISVAMPORY_VERSION values other than latest or
three numeric components.

- [ ] **Step 2: Run the tests and confirm the red state**

~~~bash
go test ./...
~~~

Expected: compilation fails because LoadConfig and Config do not exist.

- [ ] **Step 3: Add the module, command shell, and configuration**

Use:

~~~text
module github.com/saltydk/docker-vrising

go 1.27.0

require golang.org/x/sys v0.47.0
~~~

Define:

~~~go
type Config struct {
    ServerDir       string
    DataDir         string
    StateDir        string
    UpdateGame      bool
    UpdateMods      bool
    ModsEnabled     bool
    KindredVersion  string
    SatisvamporyVersion string
    BackupRetention int
    StartupTimeout  time.Duration
    ShutdownTimeout time.Duration
    LogDays         int
    Branch          string
    PUID            *int
    PGID            *int
    GameEnv         map[string]string
    BaseEnv         []string
}
~~~

Map SERVERNAME, WORLDNAME, GAMEPORT, and QUERYPORT to native VR_ forms only
when the native form is absent. Preserve every incoming VR_ variable plus TZ
and WINEDEBUG. Return one warning per shadowed alias. Keep LoadConfig pure.

Dispatch only run, health, and version. Unknown commands print
"usage: vrisingctl {run|health|version}" and exit 2. Add Make targets
**fmt-check**, **test**, **vet**, **build**, **image**, and **check**; check runs
format validation, tests, and vet without rewriting.

For the compile-safe foundation, runCommand returns exitPreflight with
"runtime not implemented", healthCommand returns one with "not running", and
buildVersion defaults to "dev". Task 9 replaces healthCommand and Task 11
replaces runCommand; no temporary behavior reaches the final image.

- [ ] **Step 4: Format, tidy, and verify**

~~~bash
gofmt -w main.go config.go config_test.go
go mod tidy
make check
~~~

Expected: all tests pass and module files are tidy.

- [ ] **Step 5: Commit**

~~~bash
git add .gitignore Makefile go.mod go.sum main.go config.go config_test.go
git commit -m "build: initialize vrising control plane"
~~~

### Task 2: Add durable state, atomic JSON, and the lifetime lock

**Files:**

- Create: **state.go**
- Create: **state_test.go**

**Interfaces:**

- Consumes: **Config.StateDir**.
- Produces:

~~~go
func (s *Store) OpenLifetimeLock() (io.Closer, error)
func (s *Store) Load() (State, error)
func (s *Store) Save(State) error
func (s *Store) LoadPackageLock() (PackageLock, error)
func (s *Store) SavePackageLock(PackageLock) error
func (s *Store) RecoverInterruptedTransaction() error
~~~

Also produces **PackageRef**, **LockedPackage**, **PackageLock**,
**GenerationRecord**, **ProcessIdentity**, **RuntimeState**, and **State**.

- [ ] **Step 1: Write failing state tests**

~~~go
func TestStoreSaveIsAtomicAndRoundTrips(t *testing.T)
func TestStoreRejectsUnknownSchemaVersion(t *testing.T)
func TestLifetimeLockRejectsSecondOwner(t *testing.T)
func TestRecoverInterruptedTransactionRestoresRecordedFiles(t *testing.T)
func TestProcessIdentityRejectsReusedPID(t *testing.T)
~~~

The recovery fixture includes one replaced managed file, one new managed file,
and one user file. Recovery restores/removes only the journaled managed paths.

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'Test(Store|Lifetime|Recover|ProcessIdentity)' ./...
~~~

Expected: compilation fails because Store and state types are undefined.

- [ ] **Step 3: Implement schema version 1 and atomic persistence**

Define:

~~~go
type PackageRef struct {
    Namespace string
    Name      string
    Version   string
}

type LockedPackage struct {
    Ref          PackageRef
    FullName     string
    DownloadURL  string
    FileSize     int64
    SHA256       string
    Dependencies []PackageRef
}

type PackageLock struct {
    SchemaVersion int
    Roots         []PackageRef
    Packages      []LockedPackage
    Digest        string
    ResolvedAt    time.Time
}

type GenerationRecord struct {
    ID         string
    LockDigest string
    Status     string
    CreatedAt  time.Time
}

type ProcessIdentity struct {
    PID        int
    StartTicks uint64
}

type RuntimeState struct {
    Phase       string
    Ready       bool
    Degraded    bool
    Reason      string
    Server      ProcessIdentity
    Generation string
    SteamBuild string
    UpdatedAt  time.Time
}

type JournalEntry struct {
    RelativePath string
    BackupPath   string
    Existed      bool
}

type TransactionJournal struct {
    GenerationID string
    Phase        string
    Entries      []JournalEntry
}

type State struct {
    SchemaVersion int
    SteamBuild    string
    Active        *GenerationRecord
    Previous      *GenerationRecord
    Candidate     *GenerationRecord
    Failed        *GenerationRecord
    Transaction   *TransactionJournal
    Runtime       RuntimeState
}
~~~

Store state.json and package-lock.json as 0600. Write a sibling temporary file,
fsync it, rename it, then fsync the parent. Acquire update.lock with
unix.Flock LOCK_EX plus LOCK_NB and retain its descriptor until run exits.
Recovery is idempotent.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w state.go state_test.go
go test -race -run 'Test(Store|Lifetime|Recover|ProcessIdentity)' ./...
go test -race ./...
~~~

Expected: all tests pass without races.

- [ ] **Step 5: Commit**

~~~bash
git add state.go state_test.go
git commit -m "feat: add durable runtime state"
~~~

### Task 3: Resolve the exact Thunderstore dependency graph

**Files:**

- Create: **thunderstore.go**
- Create: **thunderstore_test.go**
- Create: **testdata/thunderstore/kindred-package.json**
- Create: **testdata/thunderstore/kindred-2.5.8.json**
- Create: **testdata/thunderstore/satisvampory-package.json**
- Create: **testdata/thunderstore/satisvampory-1.0.85.json**
- Create: **testdata/thunderstore/hookdots-package.json**
- Create: **testdata/thunderstore/hookdots-1.1.1.json**
- Create: **testdata/thunderstore/vcf-0.10.4.json**
- Create: **testdata/thunderstore/bepinex-1.733.2.json**

**Interfaces:**

- Consumes: **PackageRef** and **HTTPDoer**.
- Produces: **Thunderstore.Resolve(ctx, []RootSelection) (ResolvedGraph, error)**.

~~~go
type HTTPDoer interface {
    Do(*http.Request) (*http.Response, error)
}
~~~

- [ ] **Step 1: Write failing resolver tests**

~~~go
func TestResolveLatestManagedRootsUsesExactMergedDependencies(t *testing.T)
func TestResolvePinnedManagedRootsSkipLatestEndpoints(t *testing.T)
func TestResolveDeduplicatesBepInEx(t *testing.T)
func TestResolveRejectsConflictingExactVersions(t *testing.T)
func TestResolveRejectsConflictAcrossManagedRoots(t *testing.T)
func TestResolveRejectsDependencyCycle(t *testing.T)
func TestResolveRejectsInactivePackage(t *testing.T)
func TestResolveRetriesTransientMetadataFailure(t *testing.T)
func TestPackageLockDigestIsStableAcrossResponseOrder(t *testing.T)
func TestParseDependencyRejectsMalformedReference(t *testing.T)
~~~

The expected order is BepInEx 1.733.2, VCF 0.10.4, KindredCommands 2.5.8,
HookDOTS API 1.1.1, and Satisvampory 1.0.85. Assert that VCF and HookDOTS
package-level latest endpoints are never requested.

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'TestResolve|TestPackageLock|TestParseDependency' ./...
~~~

Expected: compilation fails because Thunderstore and RootSelection are absent.

- [ ] **Step 3: Implement exact graph traversal**

Use:

~~~text
Latest root:
https://thunderstore.io/api/experimental/package/odjit/KindredCommands/
https://thunderstore.io/api/experimental/package/Team_GreenEye/Satisvampory/

Exact version:
https://thunderstore.io/api/experimental/package/{namespace}/{name}/{version}/
~~~

Define:

~~~go
type RootSelection struct {
    Namespace string
    Name      string
    Version   string
}

type ResolvedPackage struct {
    Ref          PackageRef
    FullName     string
    DownloadURL  string
    FileSize     int64
    IsActive     bool
    Dependencies []PackageRef
}

type ResolvedGraph struct {
    Roots    []PackageRef
    Packages []ResolvedPackage
}

type Thunderstore struct {
    BaseURL string
    Client  HTTPDoer
}
~~~

Validate exactly the ordered KindredCommands and Satisvampory roots and allow
latest only for those identities. Resolve latest independently, then parse
dependency strings with one anchored expression accepting namespace, name, and
a three-component numeric version. Traverse the merged graph with shared
visiting/visited and identity-version sets, reject a second version for one
namespace/name across either root, and topologically sort dependencies before
dependants. Require HTTP 200, JSON content, active
versions, positive sizes, HTTPS downloads, and response/request identity match.
Retry 429 and 5xx metadata responses at most three times with one- and
two-second delays; do not retry other 4xx responses. Build the package
lock digest from canonical JSON with packages and each dependency sorted by
namespace, name, and version.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w thunderstore.go thunderstore_test.go
go test -race -run 'TestResolve|TestPackageLock|TestParseDependency' ./...
go test -race ./...
~~~

Expected: all tests pass with the exact five-package merged closure.

- [ ] **Step 5: Commit**

~~~bash
git add thunderstore.go thunderstore_test.go testdata/thunderstore
git commit -m "feat: resolve thunderstore dependency graphs"
~~~

### Task 4: Download, cache, and safely extract package archives

**Files:**

- Create: **archive.go**
- Create: **archive_test.go**

**Interfaces:**

- Consumes: **ResolvedPackage**, prior **LockedPackage**, and **HTTPDoer**.
- Produces: **ArchiveCache.Fetch(ctx, pkg, prior) (LockedPackage, string, error)**
  and **ExtractArchive(path, destination, policy) (ExtractedArchive, error)**.

- [ ] **Step 1: Write failing archive tests**

~~~go
func TestArchiveCacheDownloadsAndRecordsSHA256(t *testing.T)
func TestArchiveCacheReusesMatchingFile(t *testing.T)
func TestArchiveCacheRejectsSizeMismatch(t *testing.T)
func TestArchiveCacheRejectsChangedBytesForLockedVersion(t *testing.T)
func TestArchiveCacheRetries503ThreeTimes(t *testing.T)
func TestArchiveCacheRejectsInsufficientSpace(t *testing.T)
func TestExtractArchiveRejectsTraversal(t *testing.T)
func TestExtractArchiveRejectsAbsolutePath(t *testing.T)
func TestExtractArchiveRejectsSymlink(t *testing.T)
func TestExtractArchiveRejectsDuplicateDestination(t *testing.T)
func TestExtractArchiveRejectsManifestMismatch(t *testing.T)
func TestExtractBepInExRequiresSingleWrapperDirectory(t *testing.T)
~~~

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'TestArchive|TestExtract' ./...
~~~

Expected: compilation fails because archive types are absent.

- [ ] **Step 3: Implement bounded retrieval and ZIP validation**

Define:

~~~go
type ArchiveCache struct {
    Dir      string
    Client   HTTPDoer
    Attempts int
    Timeout  time.Duration
    Backoff  func(context.Context, time.Duration) error
}

type ExtractPolicy int

const (
    ExtractBepInEx ExtractPolicy = iota
    ExtractPlugin
)

type ExtractedArchive struct {
    Manifest PackageRef
    Files    []string
}
~~~

Use three attempts, a 30-second request timeout, and one- then two-second
delays. Before download, require cache space for missing archive sizes plus
512 MiB. Stream into a sibling temporary file; enforce Content-Length when
present and Thunderstore file_size always; fsync, hash, validate, then rename.
Before extraction, require space for ZIP uncompressed sizes plus 512 MiB.

Reject absolute/empty/escaping names, backslashes after normalization, duplicate
destinations, and non-regular/non-directory modes. Extract create-exclusive at
0600 and set final files 0644. Verify manifest identity, version, and exact
dependencies before extraction. Require one BepInExPack_V_Rising wrapper;
plugin archives expose DLLs only at archive root or directly below one
plugins/ subtree. Flatten that subtree and reject deeper layouts, non-DLL
payloads there, and duplicate flattened destination names.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w archive.go archive_test.go
go test -race -run 'TestArchive|TestExtract' ./...
go test -race ./...
~~~

Expected: all tests pass.

- [ ] **Step 5: Commit**

~~~bash
git add archive.go archive_test.go
git commit -m "feat: validate and cache mod archives"
~~~

### Task 5: Stage and journal managed BepInEx generations

**Files:**

- Create: **mods.go**
- Create: **mods_test.go**

**Interfaces:**

- Consumes: **PackageLock** and validated archive paths.
- Produces:

~~~go
type StagedGeneration struct {
    Record   GenerationRecord
    Dir      string
    Manifest ManagedManifest
    Lock     PackageLock
}

func (m *ModManager) Stage(
    context.Context,
    PackageLock,
    map[PackageRef]string,
) (StagedGeneration, error)
func (m *ModManager) Apply(context.Context, StagedGeneration) error
func (m *ModManager) Rollback(context.Context) error
func (m *ModManager) Promote(context.Context, StagedGeneration) error
~~~

- [ ] **Step 1: Write failing mod-generation tests**

~~~go
func TestStageMapsBepInExAndPluginFiles(t *testing.T)
func TestStageAlwaysIncludesNetTopologySuite(t *testing.T)
func TestApplyPreservesMutableBepInExDirectories(t *testing.T)
func TestApplyPreservesManualPlugins(t *testing.T)
func TestApplyRemovesOnlyStaleManagedFiles(t *testing.T)
func TestApplyRefusesEditedManagedFile(t *testing.T)
func TestRollbackRestoresPreviousGeneration(t *testing.T)
func TestRecoverAfterInterruptedApply(t *testing.T)
func TestDisableModsPreservesGenerationsAndConfig(t *testing.T)
func TestBepInExConsoleLoggingIsDisabled(t *testing.T)
~~~

Mutable fixtures include BepInEx/config, cache, interop, unity-libs,
LogOutput.log, and a manual plugin outside the managed subtree.

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'Test(Stage|Apply|Rollback|Recover|Disable|BepInEx)' ./...
~~~

Expected: compilation fails because ModManager is absent.

- [ ] **Step 3: Implement generation staging**

Define:

~~~go
type ManagedFile struct {
    RelativePath string
    SHA256       string
    Mode         fs.FileMode
}

type ManagedManifest struct {
    SchemaVersion int
    GenerationID  string
    LockDigest    string
    Files         []ManagedFile
}

type ModManager struct {
    ServerDir      string
    GenerationsDir string
    Store          *Store
}
~~~

Generate IDs from lock digest plus UTC timestamp. Stage .doorstop_version,
doorstop_config.ini, winhttp.dll, dotnet/**, BepInEx/core/**, and
BepInEx/patchers/**. Create BepInEx/config/BepInEx.cfg only when absent, then
set Logging.Console Enabled to false with a narrow section-aware edit.

Place VampireCommandFramework.dll, KindredCommands.dll, NetTopologySuite.dll,
HookDOTS.API.dll, and Satisvampory.dll below
BepInEx/plugins/saltydk-managed. Reject missing or additional plugin DLLs.

- [ ] **Step 4: Implement journaled apply, rollback, and promotion**

Compare each current managed path with the prior manifest before replacement.
If its hash was changed outside the manager, fail without overwriting. Move old
managed files into the generation rollback area, persist the journal, install
new files through sibling temporary files plus rename, and sync directories.
Never touch unlisted paths.

Rollback restores only journaled files and is idempotent. Promotion changes
candidate to active only after readiness. Retain one previous overlay and
failed diagnostic metadata until a later successful promotion.

- [ ] **Step 5: Verify**

~~~bash
gofmt -w mods.go mods_test.go
go test -race -run 'Test(Stage|Apply|Rollback|Recover|Disable|BepInEx)' ./...
go test -race ./...
~~~

Expected: all tests pass and user-controlled files survive.

- [ ] **Step 6: Commit**

~~~bash
git add mods.go mods_test.go
git commit -m "feat: add managed mod generations"
~~~

### Task 6: Create verified pre-upgrade backups

**Files:**

- Create: **backup.go**
- Create: **backup_test.go**

**Interfaces:**

- Produces: **BackupManager.Create(ctx, request) (BackupRecord, error)** and
  **BackupManager.Prune(retain int) error**.

- [ ] **Step 1: Write failing backup tests**

~~~go
func TestBackupIncludesSavesSettingsListsAndModConfig(t *testing.T)
func TestBackupExcludesLogsGameFilesAndCache(t *testing.T)
func TestBackupDoesNotAppearUntilVerified(t *testing.T)
func TestBackupFailureLeavesExistingBackupsUntouched(t *testing.T)
func TestBackupPruneKeepsNewestThree(t *testing.T)
func TestBackupRejectsInsufficientSpace(t *testing.T)
func TestBackupDoesNotFollowSymlinks(t *testing.T)
~~~

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run TestBackup ./...
~~~

Expected: compilation fails because BackupManager is absent.

- [ ] **Step 3: Implement streaming tar.gz backup and verification**

Define:

~~~go
type BackupRequest struct {
    InstalledBuild string
    TargetBuild    string
    PackageDigest  string
}

type BackupRecord struct {
    Path      string
    SHA256    string
    Size      int64
    CreatedAt time.Time
}

type BackupManager struct {
    ServerDir string
    DataDir   string
    BackupDir string
    Now       func() time.Time
}
~~~

Back up DataDir/Settings, DataDir/Saves, and ServerDir/BepInEx/config when
present. Include a manifest with source/target builds, package digest, creation
time, and members. Exclude logs, caches, binaries, and the backup directory.

Sum regular files without following symlinks and require available space of at
least source bytes plus 512 MiB. Stream to a temporary tar.gz, fsync, reopen and
fully verify gzip/tar members, hash, then rename. Delete excess archives only
after a successful new backup.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w backup.go backup_test.go
go test -race -run TestBackup ./...
go test -race ./...
~~~

Expected: all tests pass.

- [ ] **Step 5: Commit**

~~~bash
git add backup.go backup_test.go
git commit -m "feat: add pre-update save backups"
~~~

### Task 7: Check and update the Steam server safely

**Files:**

- Create: **steam.go**
- Create: **steam_test.go**
- Create: **testdata/steam/app-info-public.txt**
- Create: **testdata/steam/appmanifest-installed.acf**

**Interfaces:**

- Produces:

~~~go
func (s *SteamClient) InstalledBuild() (SteamBuild, error)
func (s *SteamClient) RemoteBuild(context.Context) (SteamBuild, error)
func (s *SteamClient) Update(context.Context, SteamBuild) (SteamBuild, error)
~~~

Use this command seam:

~~~go
type CommandSpec struct {
    Path string
    Args []string
    Env  []string
    Dir  string
}

type CommandResult struct {
    ExitCode int
    Stdout   []byte
    Stderr   []byte
}

type CommandRunner interface {
    Run(context.Context, CommandSpec) (CommandResult, error)
}
~~~

- [ ] **Step 1: Write failing Steam tests**

~~~go
func TestParseInstalledBuild(t *testing.T)
func TestParseRemotePublicBuildAndDepot(t *testing.T)
func TestRemoteBuildSelectsConfiguredBranch(t *testing.T)
func TestSteamMetadataFailureIsPreMutation(t *testing.T)
func TestSteamUpdateUsesLiteralArgumentSlice(t *testing.T)
func TestSteamUpdateRetriesThreeTimes(t *testing.T)
func TestSteamUpdateRejectsMissingExecutable(t *testing.T)
func TestSteamUpdateRejectsUnexpectedBuild(t *testing.T)
func TestSteamFailureAfterMutationIsFatal(t *testing.T)
~~~

Assert separate argv entries for force platform windows, force_install_dir,
anonymous login, app_update 1829350, optional beta branch, validate, and quit.
Never pass a shell command string.

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'Test(Parse.*Build|RemoteBuild|Steam)' ./...
~~~

Expected: compilation fails because SteamClient is absent.

- [ ] **Step 3: Implement Steam metadata and update**

Define:

~~~go
type SteamBuild struct {
    BuildID       string
    DepotManifest string
    Branch        string
}

type SteamClient struct {
    Runner    CommandRunner
    SteamCMD  string
    ServerDir string
    HomeDir   string
    Branch    string
}
~~~

Parse installed buildid from steamapps/appmanifest_1829350.acf. Query with
app_info_update 1 and app_info_print 1829350, parsing the configured branch and
depot 1829351 manifest.

Try app_update at most three times. Once the first update begins, every final
failure is post-mutation. Require success, VRisingServer.exe, steam_appid.txt,
appmanifest 1829350, and installed/target build agreement. Never delete Steam
metadata as a retry strategy.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w steam.go steam_test.go
go test -race -run 'Test(Parse.*Build|RemoteBuild|Steam)' ./...
go test -race ./...
~~~

Expected: all tests pass.

- [ ] **Step 5: Commit**

~~~bash
git add steam.go steam_test.go testdata/steam
git commit -m "feat: manage steam server updates"
~~~

### Task 8: Resolve identity and perform explicit ownership migration

**Files:**

- Create: **identity.go**
- Create: **identity_test.go**

**Interfaces:**

- Produces: **ResolveIdentity(Config) (RuntimeIdentity, error)**,
  **PrepareOwnership(Config, RuntimeIdentity) error**, and
  **DropPrivileges(RuntimeIdentity) error**.

- [ ] **Step 1: Write failing identity tests**

~~~go
func TestResolveIdentityUsesServerMountOwnerByDefault(t *testing.T)
func TestResolveIdentityRequiresPersistentMountWritable(t *testing.T)
func TestResolveIdentityUsesExplicitPUIDAndPGID(t *testing.T)
func TestPrepareOwnershipRunsOnlyWhenIdentityChanges(t *testing.T)
func TestPrepareOwnershipUsesLchownForSymlinks(t *testing.T)
func TestPrepareOwnershipRecordsCompletedMigration(t *testing.T)
~~~

Use a symlink to an external file and prove its target ownership is unchanged.

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'TestResolveIdentity|TestPrepareOwnership' ./...
~~~

Expected: compilation fails because RuntimeIdentity is absent.

- [ ] **Step 3: Implement identity handling**

~~~go
type RuntimeIdentity struct {
    UID  int
    GID  int
    Home string
}
~~~

Without explicit IDs, use the server mount's numeric owner and require that
identity to create/remove a probe file in both mounts. With explicit IDs, walk
both roots without following symlinks, use Lchown, and record the pair in
StateDir/ownership.json only after completion.

Create Home at StateDir/home and WINEPREFIX at StateDir/wineprefix. Clear
supplementary groups, set GID, then UID exactly once after root-only setup.
Retain root for owner 0 compatibility and log one security notice.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w identity.go identity_test.go
go test -race -run 'TestResolveIdentity|TestPrepareOwnership' ./...
go test -race ./...
~~~

Expected: all tests pass.

- [ ] **Step 5: Commit**

~~~bash
git add identity.go identity_test.go
git commit -m "feat: add runtime identity management"
~~~

### Task 9: Detect readiness and expose durable health

**Files:**

- Create: **logs.go**
- Create: **logs_test.go**
- Create: **health.go**
- Create: **health_test.go**
- Create: **testdata/logs/server-ready.log**
- Create: **testdata/logs/bepinex-ready.log**
- Create: **testdata/logs/bepinex-fatal.log**

**Interfaces:**

- Produces: **ReadinessMonitor.Wait(ctx, expected) error** and
  **CheckHealth(state State, proc ProcInspector) error**.
- Produces: **PruneLogs(dataDir string, days int, now time.Time) error**.

- [ ] **Step 1: Write failing readiness and health tests**

~~~go
func TestReadinessRequiresServerBepInExVCFKindredAndSatisvampory(t *testing.T)
func TestReadinessRejectsFatalBepInExOutput(t *testing.T)
func TestReadinessRequiresExpectedKindredVersion(t *testing.T)
func TestReadinessRequiresExpectedSatisvamporyVersion(t *testing.T)
func TestReadinessTimesOut(t *testing.T)
func TestHealthRejectsMissingProcess(t *testing.T)
func TestHealthRejectsReusedPID(t *testing.T)
func TestHealthAcceptsDegradedKnownGoodServer(t *testing.T)
func TestPruneLogsDeletesOnlyOwnedExpiredServerLogs(t *testing.T)
func TestPruneLogsNeverTraversesSubdirectories(t *testing.T)
~~~

Fixtures contain V Rising startup completion, BepInEx chainloader completion,
VCF load evidence, "Plugin aa.odjit.KindredCommands version 2.5.8 is loaded!",
and "Satisvampory 1.0.85 (Satisvampory) ready.".

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'TestReadiness|TestHealth|TestPruneLogs' ./...
~~~

Expected: compilation fails because readiness and health types are absent.

- [ ] **Step 3: Implement log following and health**

~~~go
type ExpectedReadiness struct {
    KindredVersion       string
    SatisvamporyVersion string
    RequireMods          bool
}

type ReadinessMonitor struct {
    ServerLog  string
    BepInExLog string
    Output     io.Writer
    PollEvery  time.Duration
}

type ProcInspector interface {
    Identity(int) (ProcessIdentity, error)
}
~~~

Follow files created after monitoring starts, preserve offsets over rotation,
prefix output with server or bepinex, and cancel every watcher. With mods
disabled, require only server startup. With mods enabled, require all markers
and reject fatal BepInEx, Doorstop, CoreCLR, or plugin-load signatures.

Health requires schema-valid RuntimeState, Ready true, and matching live PID
start ticks. Degraded known-good operation remains healthy. The health command
returns zero or one and prints one short reason.

Prune only top-level files matching the image's dated V Rising log pattern and
older than LOGDAYS. Never recurse, follow symlinks, or delete BepInEx, backup,
Settings, Saves, or user-named logs.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w logs.go logs_test.go health.go health_test.go
go test -race -run 'TestReadiness|TestHealth|TestPruneLogs' ./...
go test -race ./...
~~~

Expected: all tests pass and watchers exit under race detection.

- [ ] **Step 5: Commit**

~~~bash
git add logs.go logs_test.go health.go health_test.go testdata/logs
git commit -m "feat: add server readiness health"
~~~

### Task 9A: Add Satisvampory as a second compatible root

**Files:**

- Modify: **config.go**, **config_test.go**, **state.go**, **state_test.go**
- Modify: **thunderstore.go**, **thunderstore_test.go**, **archive.go**, **archive_test.go**
- Modify: **mods.go**, **mods_test.go**, **logs.go**, **logs_test.go**
- Create: Satisvampory and HookDOTS fixtures below **testdata/thunderstore/**
- Modify: **testdata/logs/** and this design/plan documentation

Resolve `odjit/KindredCommands` and `Team_GreenEye/Satisvampory` independently,
then merge their exact graphs before any archive download. The fixed acceptance
fixture is Satisvampory 1.0.85 with BepInEx 1.733.2, VCF 0.10.4, and HookDOTS
API 1.1.1; HookDOTS itself requires BepInEx 1.733.2. The final ordered roots are
KindredCommands then Satisvampory, and `MODS_ENABLED` controls both.

Stage exactly `VampireCommandFramework.dll`, `KindredCommands.dll`,
`NetTopologySuite.dll`, `HookDOTS.API.dll`, and `Satisvampory.dll` in the
managed plugin subtree. Readiness requires both exact root versions, including
the marker `Satisvampory <version> (Satisvampory) ready.`.

Verify with focused tests, repeated readiness tests under the race detector,
the full race suite, `make check`, and `git diff --check`.

### Task 10: Supervise Xvfb and Wine with exact process ownership

**Files:**

- Create: **process.go**
- Create: **process_test.go**

**Interfaces:**

- Produces: **Supervisor.Run(ctx, LaunchRequest) (RunResult, error)**.
- Uses a real exec adapter and a fake through this consumer-owned seam:

~~~go
type ManagedProcess interface {
    PID() int
    StartTicks() (uint64, error)
    SignalGroup(os.Signal) error
    Wait() (int, error)
}

type ProcessFactory interface {
    StartXvfb(context.Context, []string) (ManagedProcess, string, error)
    StartWine(context.Context, CommandSpec) (ManagedProcess, error)
    KillWineServer(context.Context, []string) error
}
~~~

- [ ] **Step 1: Write failing supervisor tests**

~~~go
func TestSupervisorStartsXvfbBeforeWine(t *testing.T)
func TestSupervisorBuildsLiteralWineArguments(t *testing.T)
func TestSupervisorMergesWinHTTPOverrideWhenModsEnabled(t *testing.T)
func TestSupervisorForcesBuiltinWinHTTPWhenModsDisabled(t *testing.T)
func TestSupervisorForwardsTERMAndINTToProcessGroup(t *testing.T)
func TestSupervisorEscalatesOnceAfterTimeout(t *testing.T)
func TestSupervisorReapsXvfbAndWine(t *testing.T)
func TestSupervisorReturnsServerExitStatus(t *testing.T)
func TestSupervisorStopsCandidateWhenReadinessTimesOut(t *testing.T)
~~~

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run TestSupervisor ./...
~~~

Expected: compilation fails because Supervisor is absent.

- [ ] **Step 3: Implement process startup and shutdown**

~~~go
type LaunchRequest struct {
    Config     Config
    Identity   RuntimeIdentity
    Generation string
    SteamBuild string
}

type RunResult struct {
    ExitCode int
    Ready    bool
}

type Supervisor struct {
    Store     *Store
    Readiness *ReadinessMonitor
    Processes ProcessFactory
    Now       func() time.Time
}
~~~

Start Xvfb with -displayfd through a dedicated pipe; consume its selected
display and never remove /tmp X locks. Start wine64 in a new process group with
VRisingServer.exe, -persistentDataPath, and a unique dated log. Legacy aliases
already live in GameEnv and are not duplicated as command-line flags.

Merge WINEDLLOVERRIDES by replacing only winhttp: use winhttp=n,b with mods and
winhttp=b without. Preserve WINEDEBUG exactly.

Record PID and /proc start ticks immediately. Run readiness and process wait
under one cancelable context. Forward TERM/INT to the negative process-group
ID, wait ShutdownTimeout, invoke wineserver -k once, then KILL once if still
needed. Reap Wine and Xvfb and stop log followers on every return.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w process.go process_test.go
go test -race -run TestSupervisor ./...
go test -race ./...
~~~

Expected: all tests pass without races or leaked goroutines.

- [ ] **Step 5: Commit**

~~~bash
git add process.go process_test.go
git commit -m "feat: supervise wine server lifecycle"
~~~

### Task 11: Orchestrate the complete startup transaction

**Files:**

- Create: **run.go**
- Create: **run_test.go**
- Modify: **main.go**

**Interfaces:**

- Consumes all prior modules through **newApplication(Config, RuntimeIdentity)** and narrow
  consumer-owned interfaces implemented by concrete modules and recording
  fakes.
- Produces: **Application.Run(ctx) error** and final CLI behavior.

- [ ] **Step 1: Write failing orchestration tests**

~~~go
func TestRunFirstInstallSuccess(t *testing.T)
func TestRunFirstInstallFailsWithoutModArtifacts(t *testing.T)
func TestRunRemoteOutageUsesKnownGoodBeforeMutation(t *testing.T)
func TestRunStagesModsBeforeSteamUpdate(t *testing.T)
func TestRunSkipsSteamUpdateWhenBuildIsCurrent(t *testing.T)
func TestRunUpdateGameFalseSkipsRemoteSteamCheck(t *testing.T)
func TestRunBacksUpOnlyWhenSteamBuildChanges(t *testing.T)
func TestRunBackupFailureSkipsSteamAndUsesKnownGood(t *testing.T)
func TestRunSteamFailureAfterMutationFailsClosed(t *testing.T)
func TestRunReappliesModsAfterSteamValidation(t *testing.T)
func TestRunPromotesCandidateOnlyAfterReadiness(t *testing.T)
func TestRunFailedCandidateRestoresPreviousGeneration(t *testing.T)
func TestRunFailedFirstCandidateRemainsFailedClosed(t *testing.T)
func TestRunRejectsPreviouslyFailedLockWithoutRetry(t *testing.T)
func TestRunModsDisabledSkipsThunderstore(t *testing.T)
func TestRunUpdateModsFalseRequiresExistingLock(t *testing.T)
~~~

Use recording fakes and assert:

~~~text
lock, recover, resolve, fetch, stage, remote-build, backup,
steam-update, apply, launch, ready, promote
~~~

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run TestRun ./...
~~~

Expected: compilation fails because Application is absent.

- [ ] **Step 3: Implement the ordered flow**

~~~go
type graphResolver interface {
    Resolve(context.Context, []RootSelection) (ResolvedGraph, error)
}

type archiveFetcher interface {
    Fetch(context.Context, ResolvedPackage, *LockedPackage) (LockedPackage, string, error)
}

type modLifecycle interface {
    Stage(context.Context, PackageLock, map[PackageRef]string) (StagedGeneration, error)
    Apply(context.Context, StagedGeneration) error
    Rollback(context.Context) error
    Promote(context.Context, StagedGeneration) error
}

type backupCreator interface {
    Create(context.Context, BackupRequest) (BackupRecord, error)
    Prune(int) error
}

type steamLifecycle interface {
    InstalledBuild() (SteamBuild, error)
    RemoteBuild(context.Context) (SteamBuild, error)
    Update(context.Context, SteamBuild) (SteamBuild, error)
}

type serverSupervisor interface {
    Run(context.Context, LaunchRequest) (RunResult, error)
}

type Application struct {
    Config     Config
    Identity   RuntimeIdentity
    Store      *Store
    Resolver   graphResolver
    Archives   archiveFetcher
    Mods       modLifecycle
    Backups    backupCreator
    Steam      steamLifecycle
    Supervisor serverSupervisor
}
~~~

Implement exactly:

1. In runCommand, resolve identity, complete root-only ownership setup, drop
   privileges, and construct newApplication(Config, RuntimeIdentity).
2. In Application.Run, validate both mounts and acquire the lifetime lock.
3. Recover interrupted managed-file work.
4. Load installed Steam and active mod state.
5. Resolve both requested roots into one exact graph, then fetch, validate, and
   stage the complete candidate without partial root updates.
6. Query remote Steam metadata; skip update and backup when already current.
7. Back up before a changed build.
8. Update and validate Steam in place.
9. Reapply staged or active BepInEx after Steam validation.
10. Prune only owned expired logs, then launch and monitor.
11. Promote only after readiness.
12. On candidate failure, stop Wine, restore previous mods, preserve evidence,
    and return exitReadiness.

A pre-mutation remote failure requires a valid executable and active package
lock, records degraded state, and launches them. A post-app_update Steam failure
returns exitSteamUpdate without launch. Map typed errors to Task 1 exit codes;
after launch preserve the server's real exit status unless readiness/shutdown
uses a reserved code. UPDATE_GAME=false skips remote Steam metadata and update.
A package-lock digest already marked failed is not retried automatically; the
operator must select another version or disable mods.

- [ ] **Step 4: Verify**

~~~bash
gofmt -w main.go run.go run_test.go
go test -race -run TestRun ./...
make check
go test -race ./...
~~~

Expected: all tests pass and event ordering matches.

- [ ] **Step 5: Commit**

~~~bash
git add main.go run.go run_test.go
git commit -m "feat: orchestrate safe server startup"
~~~

### Task 12: Build the runtime image and drop-in Compose example

**Files:**

- Create: **Dockerfile**
- Create: **.dockerignore**
- Create: **compose.yaml**
- Create: **docker_test.go**
- Modify: **Makefile**

**Interfaces:**

- Produces: the linux/amd64 **saltydk/vrising** image with built-in entrypoint
  and health check.

- [ ] **Step 1: Write failing Docker contract tests**

~~~go
func TestDockerfilePinsBuilderAndRuntimeDigests(t *testing.T)
func TestDockerfileUsesTiniAndVRisingctl(t *testing.T)
func TestDockerfileDeclaresLegacyVolumesAndPorts(t *testing.T)
func TestDockerfileHealthcheckUsesVRisingctl(t *testing.T)
func TestComposePreservesMigrationContract(t *testing.T)
~~~

Compose assertions include saltydk/vrising, exact mount targets, TZ,
SERVERNAME, WINEDEBUG, UDP 9876/9877, TCP 25575, restart unless-stopped, and
the external saltbox network.

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'TestDockerfile|TestCompose' ./...
~~~

Expected: tests fail because image files are absent.

- [ ] **Step 3: Add the multi-stage Dockerfile**

Pin:

~~~dockerfile
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
FROM ubuntu:22.04@sha256:2edbbc5dc405e9612ba3584ce95480277e3eb374407b5505fe26f17df77c7dbc
~~~

Build vrisingctl with CGO_ENABLED=0 for linux/amd64, trimpath, and revision/build
time linker values. Add i386 and multiverse, then install CA certificates,
gnupg, timezone data, tini, Xvfb, winbind, SteamCMD, and WineHQ stable from the
signed Jammy source. Accept SteamCMD's license non-interactively. Remove apt
lists and build-only tooling. Never run SteamCMD/Wine or fetch payloads at
build.

Declare both legacy volumes and ports 9876/udp, 9877/udp, 25575/tcp. Use:

~~~dockerfile
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/vrisingctl"]
CMD ["run"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30m --retries=3 CMD ["/usr/local/bin/vrisingctl", "health"]
~~~

Add OCI source/revision/version/description labels without claiming payload
ownership.

- [ ] **Step 4: Add Compose and image Make targets**

Create compose.yaml from the approved service, changing only its image. Add:

~~~make
image:
	docker buildx build --platform linux/amd64 --load -t saltydk/vrising:local .

image-check: image
	docker image inspect saltydk/vrising:local >/dev/null
	docker run --rm --entrypoint /usr/local/bin/vrisingctl saltydk/vrising:local version
~~~

- [ ] **Step 5: Verify the image**

~~~bash
gofmt -w docker_test.go
go test -run 'TestDockerfile|TestCompose' ./...
make check
make image-check
docker run --rm --entrypoint sh saltydk/vrising:local -c 'test -x /usr/local/bin/vrisingctl && test -x /usr/bin/tini && command -v steamcmd && command -v wine64 && command -v Xvfb'
~~~

Expected: tests pass and required executables exist.

- [ ] **Step 6: Commit**

~~~bash
git add Dockerfile .dockerignore compose.yaml docker_test.go Makefile
git commit -m "build: add vrising container image"
~~~

### Task 13: Add deterministic container acceptance

**Files:**

- Create: **hack/container-fixture-test.sh**
- Create: **testdata/container/bin/steamcmd**
- Create: **testdata/container/bin/wine64**
- Create: **testdata/container/bin/wineserver**
- Create: **testdata/container/bin/Xvfb**
- Modify: **Dockerfile**
- Modify: **Makefile**

**Interfaces:**

- Produces: **make container-test** against a Docker fixture target.

- [ ] **Step 1: Write the failing fixture script**

Create one mktemp directory with trap cleanup and assert:

~~~text
1. Empty mounts install and become healthy.
2. Offline restart uses degraded known-good state.
3. Corrupt mod bytes never change active files.
4. Interrupted apply recovers on restart.
5. MODS_ENABLED=false disables both roots and preserves cache, generations, and config.
6. TERM exits within SHUTDOWN_TIMEOUT and leaves no child.
~~~

Use validated temporary paths and never mount /opt/v-rising.

- [ ] **Step 2: Confirm failure**

~~~bash
bash -n hack/container-fixture-test.sh
make container-test
~~~

Expected: syntax passes; the test fails because fakes are not wired.

- [ ] **Step 3: Add fixture target and fake tools**

Each fake records argv and implements only its scenario. Fake SteamCMD writes
appmanifest_1829350.acf and VRisingServer.exe. Fake Wine emits server/BepInEx,
VCF, exact KindredCommands, and exact Satisvampory readiness, records TERM, and
exits cleanly. Fake Xvfb writes a display number to displayfd and remains alive.
Overlay fakes only in Docker target **fixture**.

- [ ] **Step 4: Verify**

~~~bash
shellcheck hack/container-fixture-test.sh testdata/container/bin/*
make container-test
~~~

Expected: all six scenarios pass and cleanup touches only the temp directory.

- [ ] **Step 5: Commit**

~~~bash
git add Dockerfile Makefile hack/container-fixture-test.sh testdata/container
git commit -m "test: add container fixture acceptance"
~~~

### Task 14: Add CI and Docker Hub publication

**Files:**

- Create: **.github/workflows/ci.yml**
- Create: **.github/workflows/publish.yml**
- Create: **.github/dependabot.yml**
- Modify: **docker_test.go**

**Interfaces:**

- Consumes: Make targets, Dockerfile, **DOCKERHUB_USERNAME**, and
  **DOCKERHUB_TOKEN**.
- Produces: non-publishing PR checks and linux/amd64 tags under
  **saltydk/vrising**.

- [ ] **Step 1: Write failing workflow contract tests**

~~~go
func TestCIHasNoPublishCredentials(t *testing.T)
func TestPublishUsesApprovedDockerHubSecrets(t *testing.T)
func TestPublishTargetsOnlyLinuxAMD64(t *testing.T)
func TestPublishCreatesLatestAndImmutableBuildTag(t *testing.T)
func TestDependabotCoversGoDockerAndActions(t *testing.T)
~~~

Assert publish has no pull_request trigger and references
secrets.DOCKERHUB_USERNAME and secrets.DOCKERHUB_TOKEN exactly.

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run 'TestCI|TestPublish|TestDependabot' ./...
~~~

Expected: tests fail because workflows are absent.

- [ ] **Step 3: Add non-publishing CI**

Use actions/checkout@v7 and actions/setup-go@v7 with go-version-file go.mod.
Grant contents read. Trigger on pull_request and push. Run:

~~~bash
make check
go test -race ./...
make image-check
make container-test
~~~

Do not reference registry secrets or push.

- [ ] **Step 4: Add publication workflow**

Trigger only on main push, weekly schedule, and workflow_dispatch. Grant
contents read. Use actions/checkout@v7, docker/setup-buildx-action@v4,
docker/login-action@v4, and docker/build-push-action@v7.

Login exactly:

~~~yaml
with:
  username: ${{ secrets.DOCKERHUB_USERNAME }}
  password: ${{ secrets.DOCKERHUB_TOKEN }}
~~~

Before login, run make check, go test -race, make image-check, and
make container-test. Build only linux/amd64. Publish:

~~~text
saltydk/vrising:latest
saltydk/vrising:build-{github.run_id}-{first 12 characters of github.sha}
~~~

Generate the immutable tag exactly and pass it through GITHUB_OUTPUT:

~~~bash
short_sha=$(printf '%s' "${{ github.sha }}" | cut -c1-12)
printf 'tag=build-%s-%s\n' "${{ github.run_id }}" "$short_sha" >> "$GITHUB_OUTPUT"
~~~

Set no-cache true only for schedule events. Use registry cache
saltydk/vrising:buildcache. Never execute untrusted PR code here.

- [ ] **Step 5: Add dependency updates**

Configure weekly Dependabot updates for gomod, docker, and github-actions at
repository root, each limited to one open pull request.

- [ ] **Step 6: Verify and commit**

~~~bash
gofmt -w docker_test.go
go test -run 'TestCI|TestPublish|TestDependabot' ./...
make check
docker buildx build --check .
git diff --check
git add .github docker_test.go
git commit -m "ci: publish docker hub image"
~~~

Expected: all checks pass. Do not push or dispatch without separate user
authorization.

### Task 15: Document operation, migration, and recovery

**Files:**

- Create: **README.md**
- Create: **hack/live-acceptance.sh**
- Modify: **Makefile**
- Modify: **docker_test.go**

**Interfaces:**

- Produces: operator documentation and **make live-acceptance**.

- [ ] **Step 1: Write failing documentation tests**

~~~go
func TestREADMEContainsDropInMigration(t *testing.T)
func TestREADMEDocumentsLatestCompatibleSemantics(t *testing.T)
func TestREADMEDocumentsOfflineFallbackAndFatalSteamFailure(t *testing.T)
func TestREADMEDocumentsBackupAndManualRestore(t *testing.T)
func TestREADMEDocumentsEmergencyModDisableAndPin(t *testing.T)
func TestREADMEWarnsRestartDoesNotPullNewImage(t *testing.T)
~~~

- [ ] **Step 2: Confirm failure**

~~~bash
go test -run TestREADME ./...
~~~

Expected: tests fail because README.md is absent.

- [ ] **Step 3: Write the README**

Document the one-line image migration and exact Compose example; first-start
time, disk, and transient-memory expectations; every variable/default; native
VR_ precedence; newest-compatible graph semantics; lock/state inspection;
pre-mutation fallback versus post-Steam fail-closed behavior; backup/manual
restore; emergency MODS_ENABLED=false, UPDATE_GAME=false, UPDATE_MODS=false,
and exact KindredCommands and Satisvampory pinning; health/log paths; Linux
AMD64 scope; runtime-download licensing; and:

~~~bash
docker compose pull
docker compose up -d
~~~

Explain that restart updates runtime payloads but does not pull a newer image.

- [ ] **Step 4: Add the live acceptance script**

Accept exactly:

~~~text
hack/live-acceptance.sh fresh IMAGE
hack/live-acceptance.sh migrate IMAGE SOURCE_SERVER_DIR SOURCE_DATA_DIR
~~~

Fresh mode creates disposable bind directories, waits 30 minutes for healthy,
asserts Steam/package state, stops cleanly, then restarts with network none and
requires degraded-but-healthy cached startup.

Migrate mode requires both source paths and refuses to copy while a process has
either tree open. After the operator quiesces the existing container, copy to
mktemp destinations with rsync -a --numeric-ids and run only against copies.
Record before/after hashes for Settings and Saves, allow new runtime files, and
fail if any prior file disappears. Assert UDP 9876/9877. If settings enable
RCON, assert TCP 25575 without reading or printing its password.

Never delete sources or use /opt/v-rising as cleanup target. Retain the temp
path on failure.

- [ ] **Step 5: Verify**

~~~bash
gofmt -w docker_test.go
go test -run TestREADME ./...
shellcheck hack/live-acceptance.sh
bash -n hack/live-acceptance.sh
make check
git diff --check
~~~

Expected: all checks pass.

- [ ] **Step 6: Commit**

~~~bash
git add README.md Makefile docker_test.go hack/live-acceptance.sh
git commit -m "docs: document vrising image operations"
~~~

### Task 16: Run final local and real acceptance gates

**Files:**

- Modify: **README.md** (acceptance evidence only).
- No implementation modification is planned; a failure returns to its owning
  task and repeats that task's test/commit cycle.

**Interfaces:**

- Produces: a locally committed and acceptance-proven candidate; no remote
  mutation.

- [ ] **Step 1: Run static and fixture gates**

~~~bash
make check
go test -race ./...
make image-check
make container-test
git diff --check
~~~

Expected: every command exits zero.

- [ ] **Step 2: Run fresh live acceptance**

~~~bash
make live-acceptance IMAGE=saltydk/vrising:local MODE=fresh
~~~

Expected: current Steam installs, exact BepInEx/VCF/KindredCommands/HookDOTS/
Satisvampory loads, health becomes healthy, shutdown is clean, and offline
restart uses the complete known-good cache.

- [ ] **Step 3: Rehearse migration on the deployment host**

~~~bash
hack/live-acceptance.sh migrate saltydk/vrising:local /opt/v-rising/server /opt/v-rising/data
~~~

Expected: copied save starts, every prior Settings/Saves file remains, UDP
ports bind, and configured RCON remains available.

If those paths are unavailable on the implementation host, stop at this gate
and hand this exact command to the user. Do not claim migration acceptance.

- [ ] **Step 4: Inspect worktree and commit subjects**

~~~bash
git status --short
git log --format='%h %s' --reverse cf45fc7..HEAD
git diff --check cf45fc7..HEAD
~~~

Expected: no unintended/unstaged implementation files and every new subject
uses Conventional Commits.

- [ ] **Step 5: Record and commit acceptance evidence**

Add tested image ID, source commit, Steam build, package-lock digest, exact
package versions, fresh/offline/migration results, and timestamp to README.
Never include passwords, tokens, save contents, or host secrets.

~~~bash
git add README.md
git commit -m "test: record vrising runtime acceptance"
~~~

- [ ] **Step 6: Stop at the publication boundary**

Report exactly:

~~~text
Local implementation and acceptance are complete.
Proposed remote actions:
1. Push main to https://github.com/saltydk/docker-vrising.
2. Allow the main workflow to publish saltydk/vrising:latest and its immutable build tag.
No push or workflow action has been performed.
~~~

Wait for explicit authorization before either remote action.
