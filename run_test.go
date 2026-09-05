package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type runRecorder struct {
	events []string
}

func (r *runRecorder) add(event string) {
	r.events = append(r.events, event)
}

type recordingRunStore struct {
	recorder *runRecorder
	state    State
	loadErr  error
	lock     PackageLock
	lockErr  error
	saved    []State
	held     bool
}

func (s *recordingRunStore) OpenLifetimeLock() (io.Closer, error) {
	s.recorder.add("lock")
	if s.held {
		return nil, errors.New("lifetime lock already held")
	}
	s.held = true
	return closeFunc(func() error {
		s.held = false
		s.recorder.add("unlock")
		return nil
	}), nil
}

func (s *recordingRunStore) RecoverInterruptedTransaction() error {
	s.recorder.add("recover")
	return nil
}

func (s *recordingRunStore) Load() (State, error) {
	return s.state, s.loadErr
}

func (s *recordingRunStore) LoadPackageLock() (PackageLock, error) {
	return s.lock, s.lockErr
}

func (s *recordingRunStore) Save(state State) error {
	s.state = state
	s.saved = append(s.saved, state)
	return nil
}

type closeFunc func() error

func (f closeFunc) Close() error {
	return f()
}

type recordingPackageResolver struct {
	recorder *runRecorder
	graph    ResolvedGraph
	err      error
	got      []RootSelection
}

func (r *recordingPackageResolver) Resolve(_ context.Context, roots []RootSelection) (ResolvedGraph, error) {
	r.recorder.add("resolve")
	r.got = slices.Clone(roots)
	return r.graph, r.err
}

type recordingArchiveFetcher struct {
	t        *testing.T
	recorder *runRecorder
	errAt    int
	files    []*os.File
}

func (f *recordingArchiveFetcher) Fetch(_ context.Context, pkg ResolvedPackage, _ LockedPackage) (*ValidatedArchive, error) {
	f.t.Helper()
	f.recorder.add("fetch:" + pkg.Ref.Name)
	if f.errAt > 0 && len(f.files)+1 == f.errAt {
		return nil, errors.New("archive unavailable")
	}
	file, err := os.CreateTemp(f.t.TempDir(), "archive-*.zip")
	if err != nil {
		f.t.Fatal(err)
	}
	f.files = append(f.files, file)
	locked := LockedPackage{
		Ref:          pkg.Ref,
		FullName:     pkg.FullName,
		DownloadURL:  pkg.DownloadURL,
		FileSize:     1,
		SHA256:       strings.Repeat("a", 64),
		Dependencies: slices.Clone(pkg.Dependencies),
	}
	return newValidatedArchive(file, locked), nil
}

func (f *recordingArchiveFetcher) assertClosed(t *testing.T) {
	t.Helper()
	for _, file := range f.files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("archive descriptor %q remains open: %v", file.Name(), err)
		}
	}
}

type recordingModLifecycle struct {
	recorder         *runRecorder
	validateErr      error
	stageErr         error
	applyErr         error
	promoteErr       error
	promoteOutcome   PromotionCommitOutcome
	rollbackCtxErr   error
	rollbackDeadline bool
	discardErr       error
	corruptStageLock bool
	staged           StagedGeneration
	stageLock        PackageLock
	archives         map[PackageRef]*ValidatedArchive
}

func (m *recordingModLifecycle) ValidateActive(_ context.Context, record GenerationRecord, lock PackageLock) (StagedGeneration, error) {
	if m.validateErr != nil {
		return StagedGeneration{}, m.validateErr
	}
	return StagedGeneration{Record: record, Lock: lock}, nil
}

func (m *recordingModLifecycle) Stage(_ context.Context, lock PackageLock, archives map[PackageRef]*ValidatedArchive) (StagedGeneration, error) {
	m.recorder.add("stage")
	m.stageLock = lock
	m.archives = archives
	if m.stageErr != nil {
		return StagedGeneration{}, m.stageErr
	}
	m.staged = StagedGeneration{
		Record: GenerationRecord{ID: "candidate", LockDigest: lock.Digest, Status: "staged"},
		Lock:   lock,
	}
	if m.corruptStageLock {
		m.staged.Lock = PackageLock{Digest: "stale-global-lock"}
	}
	return m.staged, nil
}

func (m *recordingModLifecycle) Apply(_ context.Context, staged StagedGeneration) error {
	m.recorder.add("apply")
	m.staged = staged
	return m.applyErr
}

func (m *recordingModLifecycle) Rollback(ctx context.Context) error {
	m.recorder.add("rollback")
	m.rollbackCtxErr = ctx.Err()
	_, m.rollbackDeadline = ctx.Deadline()
	return nil
}

func (m *recordingModLifecycle) Promote(_ context.Context, staged StagedGeneration) error {
	m.recorder.add("promote")
	m.staged = staged
	return m.promoteErr
}

func (m *recordingModLifecycle) CommitPromotion(_ context.Context, staged StagedGeneration) (PromotionCommitOutcome, error) {
	m.recorder.add("promote")
	m.staged = staged
	outcome := m.promoteOutcome
	if outcome == PromotionNotCommitted && m.promoteErr == nil {
		outcome = PromotionCommitted
	}
	return outcome, m.promoteErr
}

func (m *recordingModLifecycle) RecoverPromotion(context.Context) (PromotionCommitOutcome, error) {
	return PromotionNotCommitted, nil
}

func (m *recordingModLifecycle) Discard(context.Context, StagedGeneration) error {
	m.recorder.add("discard")
	return m.discardErr
}

type recordingBackupCreator struct {
	recorder *runRecorder
	err      error
}

func (b *recordingBackupCreator) Create(_ context.Context, _ BackupRequest) (BackupRecord, error) {
	b.recorder.add("backup")
	return BackupRecord{}, b.err
}

func (b *recordingBackupCreator) Prune(int) error {
	return nil
}

type recordingSteamLifecycle struct {
	recorder     *runRecorder
	installed    SteamBuild
	installedErr error
	remote       SteamBuild
	remoteErr    error
	updated      SteamBuild
	updateErr    error
	validateErr  error
	validated    []SteamBuild
}

func (s *recordingSteamLifecycle) InstalledBuild() (SteamBuild, error) {
	return s.installed, s.installedErr
}

func (s *recordingSteamLifecycle) RemoteBuild(context.Context) (SteamBuild, error) {
	s.recorder.add("remote-build")
	return s.remote, s.remoteErr
}

func (s *recordingSteamLifecycle) Update(_ context.Context, _ SteamBuild) (SteamBuild, error) {
	s.recorder.add("steam-update")
	return s.updated, s.updateErr
}

func (s *recordingSteamLifecycle) ValidateInstalled(expected SteamBuild) (SteamBuild, error) {
	s.validated = append(s.validated, expected)
	if s.validateErr != nil {
		return SteamBuild{}, s.validateErr
	}
	return expected, nil
}

type recordingServerSupervisor struct {
	recorder     *runRecorder
	store        *recordingRunStore
	result       RunResult
	err          error
	request      LaunchRequest
	beforeReturn func()
}

func (s *recordingServerSupervisor) Run(ctx context.Context, request LaunchRequest) (RunResult, error) {
	s.recorder.add("launch")
	if s.store != nil && !s.store.held {
		return RunResult{}, errors.New("lifetime lock released before launch")
	}
	s.request = request
	if s.result.Ready {
		s.recorder.add("readiness")
		if request.Generation == "candidate" && request.OnReady == nil {
			return RunResult{}, errors.New("candidate launch has no readiness commit")
		}
		if request.OnReady != nil {
			if err := request.OnReady(ctx); err != nil {
				return RunResult{}, err
			}
		}
		s.recorder.add("ready")
	}
	if s.beforeReturn != nil {
		s.beforeReturn()
	}
	return s.result, s.err
}

type recordingProcInspector struct {
	identity ProcessIdentity
	err      error
	pids     []int
}

func (p *recordingProcInspector) Identity(pid int) (ProcessIdentity, error) {
	p.pids = append(p.pids, pid)
	return p.identity, p.err
}

type runFixture struct {
	recorder   *runRecorder
	store      *recordingRunStore
	resolver   *recordingPackageResolver
	archives   *recordingArchiveFetcher
	mods       *recordingModLifecycle
	backups    *recordingBackupCreator
	steam      *recordingSteamLifecycle
	supervisor *recordingServerSupervisor
	app        *Application
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(serverDir, ".state")
	for _, dir := range []string{serverDir, dataDir, stateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	recorder := &runRecorder{}
	store := &recordingRunStore{recorder: recorder, state: State{SchemaVersion: schemaVersion}, lockErr: os.ErrNotExist}
	graph := ResolvedGraph{
		Roots: []PackageRef{
			{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8"},
			{Namespace: "Team_GreenEye", Name: "Satisvampory", Version: "1.0.85"},
		},
		Packages: []ResolvedPackage{
			{Ref: PackageRef{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8"}, FullName: "odjit-KindredCommands-2.5.8", DownloadURL: "https://example.invalid/kindred.zip", IsActive: true},
			{Ref: PackageRef{Namespace: "Team_GreenEye", Name: "Satisvampory", Version: "1.0.85"}, FullName: "Team_GreenEye-Satisvampory-1.0.85", DownloadURL: "https://example.invalid/satisvampory.zip", IsActive: true},
		},
	}
	resolver := &recordingPackageResolver{recorder: recorder, graph: graph}
	archives := &recordingArchiveFetcher{t: t, recorder: recorder}
	mods := &recordingModLifecycle{recorder: recorder}
	backups := &recordingBackupCreator{recorder: recorder}
	steam := &recordingSteamLifecycle{
		recorder:  recorder,
		installed: SteamBuild{BuildID: "100", DepotManifest: "old", Branch: "public"},
		remote:    SteamBuild{BuildID: "200", DepotManifest: "new", Branch: "public"},
		updated:   SteamBuild{BuildID: "200", DepotManifest: "new", Branch: "public"},
	}
	supervisor := &recordingServerSupervisor{recorder: recorder, store: store, result: RunResult{Ready: true}}
	cfg := Config{
		ServerDir:           serverDir,
		DataDir:             dataDir,
		StateDir:            stateDir,
		UpdateGame:          true,
		UpdateMods:          true,
		ModsEnabled:         true,
		KindredVersion:      "latest",
		SatisvamporyVersion: "latest",
		BackupRetention:     3,
		StartupTimeout:      time.Minute,
		ShutdownTimeout:     time.Minute,
		LogDays:             7,
		Branch:              "public",
	}
	app := &Application{
		Config:     cfg,
		Identity:   RuntimeIdentity{UID: os.Getuid(), GID: os.Getgid(), Home: root},
		Resolver:   resolver,
		Archives:   archives,
		Mods:       mods,
		Backups:    backups,
		Steam:      steam,
		Supervisor: supervisor,
		stateStore: store,
		validateMounts: func(Config) error {
			recorder.add("mounts")
			return nil
		},
		pruneLogs: func(string, int, time.Time) error {
			recorder.add("prune-logs")
			return nil
		},
		now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	return &runFixture{recorder, store, resolver, archives, mods, backups, steam, supervisor, app}
}

func TestRunFirstInstallSuccess(t *testing.T) {
	fixture := newRunFixture(t)

	err := fixture.app.Run(t.Context())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantEvents := []string{
		"mounts", "lock", "recover", "resolve",
		"fetch:KindredCommands", "fetch:Satisvampory", "stage",
		"remote-build", "backup", "steam-update", "apply",
		"prune-logs", "launch", "readiness", "promote", "ready", "unlock",
	}
	if !slices.Equal(fixture.recorder.events, wantEvents) {
		t.Fatalf("events = %q, want %q", fixture.recorder.events, wantEvents)
	}
	if got := fixture.resolver.got; len(got) != 2 ||
		got[0].Namespace != "odjit" || got[0].Name != "KindredCommands" || got[0].Version != "latest" ||
		got[1].Namespace != "Team_GreenEye" || got[1].Name != "Satisvampory" || got[1].Version != "latest" {
		t.Fatalf("Resolve() roots = %#v", got)
	}
	if got := fixture.supervisor.request.PackageLock; got.Digest == "" || got.Digest != fixture.mods.stageLock.Digest {
		t.Fatalf("launch package lock = %#v, staged lock = %#v", got, fixture.mods.stageLock)
	}
	fixture.archives.assertClosed(t)
}

func TestRunRejectsRecordedLiveProcessBeforeMutation(t *testing.T) {
	fixture := newRunFixture(t)
	recorded := ProcessIdentity{PID: 4242, StartTicks: 987654}
	store := installRealRunState(t, fixture, State{
		SchemaVersion: schemaVersion,
		Runtime:       RuntimeState{Phase: "ready", Ready: true, Server: recorded},
	})
	proc := &recordingProcInspector{identity: recorded}
	fixture.app.Proc = proc

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitPreflight)

	if !slices.Equal(proc.pids, []int{recorded.PID}) {
		t.Fatalf("inspected PIDs = %v, want %d", proc.pids, recorded.PID)
	}
	assertNoEvent(t, fixture.recorder.events, "resolve")
	assertNoEvent(t, fixture.recorder.events, "remote-build")
	assertNoEvent(t, fixture.recorder.events, "launch")
	state, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.Runtime.Server != recorded {
		t.Fatalf("live runtime identity = %#v, want preserved %#v", state.Runtime.Server, recorded)
	}
}

func TestRunRejectsActualRecordedCurrentProcess(t *testing.T) {
	fixture := newRunFixture(t)
	inspector := procFSInspector{}
	recorded, err := inspector.Identity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	installRealRunState(t, fixture, State{
		SchemaVersion: schemaVersion,
		Runtime:       RuntimeState{Phase: "ready", Ready: true, Server: recorded},
	})
	fixture.app.Proc = inspector

	err = fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitPreflight)
	assertNoEvent(t, fixture.recorder.events, "resolve")
	assertNoEvent(t, fixture.recorder.events, "remote-build")
}

func TestRunClearsReusedRecordedProcessBeforeMutation(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.Config.ModsEnabled = false
	fixture.app.Config.UpdateGame = false
	recorded := ProcessIdentity{PID: 4242, StartTicks: 987654}
	store := installRealRunState(t, fixture, State{
		SchemaVersion: schemaVersion,
		Runtime:       RuntimeState{Phase: "ready", Ready: true, Server: recorded},
	})
	fixture.app.Proc = &recordingProcInspector{identity: ProcessIdentity{PID: recorded.PID, StartTicks: recorded.StartTicks + 1}}

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Runtime.Server != (ProcessIdentity{}) || state.Runtime.Ready || state.Runtime.Phase != "stopped" {
		t.Fatalf("cleared stale runtime = %#v", state.Runtime)
	}
	assertEventBefore(t, fixture.recorder.events, "prune-logs", "launch")
}

func TestRunPreservesRecordedProcessWhenInspectionIsInconclusive(t *testing.T) {
	fixture := newRunFixture(t)
	recorded := ProcessIdentity{PID: 4242, StartTicks: 987654}
	store := installRealRunState(t, fixture, State{
		SchemaVersion: schemaVersion,
		Runtime:       RuntimeState{Phase: "ready", Ready: true, Server: recorded},
	})
	fixture.app.Proc = &recordingProcInspector{}

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitPreflight)
	state, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.Runtime.Server != recorded || !state.Runtime.Ready {
		t.Fatalf("inconclusive inspection changed runtime = %#v", state.Runtime)
	}
	assertNoEvent(t, fixture.recorder.events, "resolve")
}

func TestRunClearsDeadRecordedProcessBeforeMutation(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.Config.ModsEnabled = false
	fixture.app.Config.UpdateGame = false
	recorded := ProcessIdentity{PID: 4242, StartTicks: 987654}
	store := installRealRunState(t, fixture, State{
		SchemaVersion: schemaVersion,
		Runtime:       RuntimeState{Phase: "starting", Server: recorded},
	})
	fixture.app.Proc = &recordingProcInspector{err: os.ErrNotExist}

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Runtime.Server != (ProcessIdentity{}) || state.Runtime.Phase != "stopped" {
		t.Fatalf("cleared dead runtime = %#v", state.Runtime)
	}
}

func TestRunFirstInstallAcceptsMissingState(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.store.state = State{}
	fixture.store.loadErr = os.ErrNotExist

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertEventBefore(t, fixture.recorder.events, "recover", "resolve")
}

func TestRunFirstInstallFailsWithoutModArtifacts(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.mods.stageErr = errors.New("missing managed DLL")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitModUpdate)

	wantEvents := []string{
		"mounts", "lock", "recover", "resolve",
		"fetch:KindredCommands", "fetch:Satisvampory", "stage", "unlock",
	}
	if !slices.Equal(fixture.recorder.events, wantEvents) {
		t.Fatalf("events = %q, want %q", fixture.recorder.events, wantEvents)
	}
	fixture.archives.assertClosed(t)
}

func TestRunRemoteOutageUsesKnownGoodBeforeMutation(t *testing.T) {
	fixture := newRunFixture(t)
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.steam.remoteErr = errors.New("steam metadata unavailable")

	err := fixture.app.Run(t.Context())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantEvents := []string{
		"mounts", "lock", "recover", "resolve",
		"fetch:KindredCommands", "fetch:Satisvampory", "stage",
		"remote-build", "discard", "prune-logs", "launch", "readiness", "ready", "unlock",
	}
	if !slices.Equal(fixture.recorder.events, wantEvents) {
		t.Fatalf("events = %q, want %q", fixture.recorder.events, wantEvents)
	}
	if got := fixture.supervisor.request.PackageLock.Digest; got != activeLock.Digest {
		t.Fatalf("fallback lock digest = %q, want %q", got, activeLock.Digest)
	}
	if got := fixture.supervisor.request.Generation; got != "active" {
		t.Fatalf("fallback generation = %q, want active", got)
	}
	if len(fixture.store.saved) == 0 || !fixture.store.saved[len(fixture.store.saved)-1].Runtime.Degraded {
		t.Fatal("remote outage did not persist degraded runtime state")
	}
	fixture.archives.assertClosed(t)
}

func TestRunFallbackDiscardsUnreferencedCandidate(t *testing.T) {
	fixture := newRunFixture(t)
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.steam.remoteErr = errors.New("steam metadata unavailable")

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertEventBefore(t, fixture.recorder.events, "stage", "discard")
	assertEventBefore(t, fixture.recorder.events, "discard", "launch")
}

func TestRunRemoteOutageWithoutKnownGoodUsesSteamExit(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.steam.remoteErr = errors.New("steam metadata unavailable")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitSteam)

	assertNoEvent(t, fixture.recorder.events, "launch")
}

func TestRunStagesModsBeforeSteamUpdate(t *testing.T) {
	fixture := newRunFixture(t)

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertEventBefore(t, fixture.recorder.events, "stage", "remote-build")
	assertEventBefore(t, fixture.recorder.events, "stage", "steam-update")
}

func TestRunSkipsSteamUpdateWhenBuildIsCurrent(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.steam.remote = fixture.steam.installed

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertNoEvent(t, fixture.recorder.events, "backup")
	assertNoEvent(t, fixture.recorder.events, "steam-update")
	assertEventBefore(t, fixture.recorder.events, "remote-build", "apply")
}

func TestRunUpdateGameFalseSkipsRemoteSteamCheck(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.Config.UpdateGame = false

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertNoEvent(t, fixture.recorder.events, "remote-build")
	assertNoEvent(t, fixture.recorder.events, "backup")
	assertNoEvent(t, fixture.recorder.events, "steam-update")
	if got := fixture.supervisor.request.SteamBuild; got != fixture.steam.installed.BuildID {
		t.Fatalf("launch Steam build = %q, want %q", got, fixture.steam.installed.BuildID)
	}
}

func TestRunBacksUpOnlyWhenSteamBuildChanges(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		fixture := newRunFixture(t)
		fixture.steam.remote = fixture.steam.installed

		if err := fixture.app.Run(t.Context()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		assertNoEvent(t, fixture.recorder.events, "backup")
	})

	t.Run("changed", func(t *testing.T) {
		fixture := newRunFixture(t)

		if err := fixture.app.Run(t.Context()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		assertEventBefore(t, fixture.recorder.events, "remote-build", "backup")
		assertEventBefore(t, fixture.recorder.events, "backup", "steam-update")
	})
}

func TestRunBackupFailureSkipsSteamAndUsesKnownGood(t *testing.T) {
	fixture := newRunFixture(t)
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.backups.err = errors.New("backup disk full")

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertNoEvent(t, fixture.recorder.events, "steam-update")
	assertNoEvent(t, fixture.recorder.events, "apply")
	if got := fixture.supervisor.request.PackageLock.Digest; got != activeLock.Digest {
		t.Fatalf("fallback lock digest = %q, want %q", got, activeLock.Digest)
	}
	if got := fixture.supervisor.request.Generation; got != "active" {
		t.Fatalf("fallback generation = %q, want active", got)
	}
	if len(fixture.store.saved) == 0 || !fixture.store.saved[len(fixture.store.saved)-1].Runtime.Degraded {
		t.Fatal("backup failure did not persist degraded runtime state")
	}
}

func TestRunBackupFailureWithoutKnownGoodUsesBackupExit(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.backups.err = errors.New("backup disk full")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitBackup)

	assertNoEvent(t, fixture.recorder.events, "steam-update")
	assertNoEvent(t, fixture.recorder.events, "launch")
}

func TestRunSteamFailureAfterMutationFailsClosed(t *testing.T) {
	fixture := newRunFixture(t)
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.steam.updateErr = &SteamPostMutationError{Err: errors.New("validation failed")}

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitSteam)

	assertNoEvent(t, fixture.recorder.events, "apply")
	assertNoEvent(t, fixture.recorder.events, "launch")
	assertNoEvent(t, fixture.recorder.events, "promote")
	fixture.archives.assertClosed(t)
}

func TestRunSteamPreMutationFailureWithoutKnownGoodUsesSteamExit(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.steam.updateErr = &SteamPreMutationError{Err: errors.New("SteamCMD unavailable")}

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitSteam)

	assertNoEvent(t, fixture.recorder.events, "apply")
	assertNoEvent(t, fixture.recorder.events, "launch")
}

func TestRunReappliesModsAfterSteamValidation(t *testing.T) {
	fixture := newRunFixture(t)

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertEventBefore(t, fixture.recorder.events, "steam-update", "apply")
}

func TestRunPromotesCandidateOnlyAfterReadiness(t *testing.T) {
	fixture := newRunFixture(t)

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertEventBefore(t, fixture.recorder.events, "readiness", "promote")
	assertEventBefore(t, fixture.recorder.events, "promote", "ready")
	if fixture.mods.staged.Record.ID != "candidate" {
		t.Fatalf("promoted generation = %q, want candidate", fixture.mods.staged.Record.ID)
	}
}

func TestRunLaunchesTheCanonicalSelectedLock(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.mods.corruptStageLock = true

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got := fixture.supervisor.request.PackageLock.Digest; got != fixture.mods.stageLock.Digest {
		t.Fatalf("launch lock digest = %q, want selected canonical digest %q", got, fixture.mods.stageLock.Digest)
	}
}

func TestRunPromotionFailureRollsBackCandidate(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.mods.promoteErr = errors.New("promotion journal unavailable")
	fixture.mods.promoteOutcome = PromotionNotCommitted

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitReadiness)

	assertEventBefore(t, fixture.recorder.events, "readiness", "promote")
	assertEventBefore(t, fixture.recorder.events, "promote", "rollback")
}

func TestRunPromotionRecoveryRequiredDoesNotRollbackCandidate(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.mods.promoteErr = errors.New("promotion lock was written before failure")
	fixture.mods.promoteOutcome = PromotionRecoveryRequired

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitReadiness)

	assertEventBefore(t, fixture.recorder.events, "readiness", "promote")
	assertNoEvent(t, fixture.recorder.events, "rollback")
}

func TestRunFailedCandidateRestoresPreviousGeneration(t *testing.T) {
	fixture := newRunFixture(t)
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.supervisor.result = RunResult{}
	fixture.supervisor.err = errors.New("readiness failed")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitReadiness)

	assertEventBefore(t, fixture.recorder.events, "apply", "launch")
	assertEventBefore(t, fixture.recorder.events, "launch", "rollback")
	assertNoEvent(t, fixture.recorder.events, "promote")
}

func TestRunFailedFirstCandidateRemainsFailedClosed(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.supervisor.result = RunResult{}
	fixture.supervisor.err = errors.New("readiness failed")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitReadiness)

	if got := countEvent(fixture.recorder.events, "launch"); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	if got := countEvent(fixture.recorder.events, "rollback"); got != 1 {
		t.Fatalf("rollback count = %d, want 1", got)
	}
	assertNoEvent(t, fixture.recorder.events, "promote")
}

func TestRunFailedCandidateRollsBackWithBoundedUncancelledContext(t *testing.T) {
	fixture := newRunFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	fixture.supervisor.result = RunResult{}
	fixture.supervisor.err = context.Canceled
	fixture.supervisor.beforeReturn = cancel

	err := fixture.app.Run(ctx)
	assertRunExitCode(t, err, exitReadiness)

	if fixture.mods.rollbackCtxErr != nil {
		t.Fatalf("Rollback() context error = %v, want live cleanup context", fixture.mods.rollbackCtxErr)
	}
	if !fixture.mods.rollbackDeadline {
		t.Fatal("Rollback() context has no cleanup deadline")
	}
}

func TestRunRejectsPreviouslyFailedLockWithoutRetry(t *testing.T) {
	fixture := newRunFixture(t)
	failedLock := canonicalTestLock(fixture.resolver.graph, "a")
	fixture.store.state.Failed = &GenerationRecord{ID: "failed", LockDigest: failedLock.Digest, Status: "failed"}

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitModUpdate)

	assertNoEvent(t, fixture.recorder.events, "stage")
	assertNoEvent(t, fixture.recorder.events, "remote-build")
	assertNoEvent(t, fixture.recorder.events, "launch")
	fixture.archives.assertClosed(t)
}

func TestRunModsDisabledSkipsThunderstore(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.Config.ModsEnabled = false
	fixture.steam.remote = fixture.steam.installed
	active := &GenerationRecord{ID: "active", LockDigest: strings.Repeat("c", 64), Status: "active"}
	fixture.store.state.Active = active

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, event := range []string{"resolve", "stage", "apply", "rollback", "promote"} {
		assertNoEvent(t, fixture.recorder.events, event)
	}
	if got := fixture.supervisor.request.PackageLock; got.Digest != "" || len(got.Roots) != 0 || len(got.Packages) != 0 {
		t.Fatalf("disabled launch package lock = %#v, want zero lock", got)
	}
	if fixture.store.state.Active != active {
		t.Fatal("disabled mods changed active generation")
	}
}

func TestRunPrunesCurrentServerLogDirectory(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.Config.ModsEnabled = false
	fixture.app.Config.UpdateGame = false
	var gotDir string
	fixture.app.pruneLogs = func(dir string, _ int, _ time.Time) error {
		gotDir = dir
		return nil
	}

	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantDir := filepath.Join(fixture.app.Config.DataDir, "logs")
	if gotDir != wantDir {
		t.Fatalf("PruneLogs() directory = %q, want %q", gotDir, wantDir)
	}
}

func TestRunModsDisabledStillRequiresBackupDependencyForUpdates(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.Config.ModsEnabled = false
	fixture.app.Backups = nil

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitPreflight)
}

func TestRunUpdateModsFalseRequiresExistingLock(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		fixture := newRunFixture(t)
		fixture.app.Config.UpdateMods = false

		err := fixture.app.Run(t.Context())
		assertRunExitCode(t, err, exitModUpdate)

		assertNoEvent(t, fixture.recorder.events, "resolve")
		assertNoEvent(t, fixture.recorder.events, "remote-build")
		assertNoEvent(t, fixture.recorder.events, "launch")
	})

	t.Run("canonical active lock", func(t *testing.T) {
		fixture := newRunFixture(t)
		fixture.app.Config.UpdateMods = false
		fixture.app.Config.UpdateGame = false
		activeLock := canonicalTestLock(fixture.resolver.graph, "b")
		fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
		fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
		fixture.store.lock = activeLock
		fixture.store.lockErr = nil

		if err := fixture.app.Run(t.Context()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}

		assertNoEvent(t, fixture.recorder.events, "resolve")
		if got := fixture.supervisor.request.PackageLock.Digest; got != activeLock.Digest {
			t.Fatalf("launch lock digest = %q, want %q", got, activeLock.Digest)
		}
		if got := fixture.supervisor.request.Generation; got != "active" {
			t.Fatalf("launch generation = %q, want active", got)
		}
	})
}

func TestRunFallbackRequiresDurablyRecordedSteamBuild(t *testing.T) {
	fixture := newRunFixture(t)
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = "different-build"
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.steam.remoteErr = errors.New("steam metadata unavailable")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitSteam)
	assertNoEvent(t, fixture.recorder.events, "launch")
}

func TestRunFallbackRequiresValidatedSteamRuntime(t *testing.T) {
	fixture := newRunFixture(t)
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.steam.remoteErr = errors.New("steam metadata unavailable")
	fixture.steam.validateErr = errors.New("steam_appid.txt is missing")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitSteam)
	assertNoEvent(t, fixture.recorder.events, "launch")
}

func TestRunUpdateModsFalseRequiresValidatedActiveGeneration(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.Config.UpdateMods = false
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.mods.validateErr = errors.New("active overlay digest mismatch")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitModUpdate)
	assertNoEvent(t, fixture.recorder.events, "remote-build")
	assertNoEvent(t, fixture.recorder.events, "launch")
}

func TestRunUpdateModsFalseRequiresValidatedSteamRuntime(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.Config.UpdateMods = false
	activeLock := canonicalTestLock(fixture.resolver.graph, "b")
	fixture.store.state.Active = &GenerationRecord{ID: "active", LockDigest: activeLock.Digest, Status: "active"}
	fixture.store.state.SteamBuild = fixture.steam.installed.BuildID
	fixture.store.lock = activeLock
	fixture.store.lockErr = nil
	fixture.steam.validateErr = errors.New("V Rising executable is missing")

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitSteam)
	assertNoEvent(t, fixture.recorder.events, "remote-build")
	assertNoEvent(t, fixture.recorder.events, "launch")
}

func TestRunPreservesServerExitStatus(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.supervisor.result = RunResult{Ready: true, ExitCode: 42}

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, 42)

	assertEventBefore(t, fixture.recorder.events, "promote", "ready")
}

func TestRunClosesPartialArchiveSet(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.archives.errAt = 2

	err := fixture.app.Run(t.Context())
	assertRunExitCode(t, err, exitModUpdate)

	assertNoEvent(t, fixture.recorder.events, "stage")
	assertNoEvent(t, fixture.recorder.events, "remote-build")
	fixture.archives.assertClosed(t)
}

func canonicalTestLock(graph ResolvedGraph, digestByte string) PackageLock {
	packages := make([]LockedPackage, 0, len(graph.Packages))
	for _, pkg := range graph.Packages {
		packages = append(packages, LockedPackage{
			Ref:          pkg.Ref,
			FullName:     pkg.FullName,
			DownloadURL:  pkg.DownloadURL,
			FileSize:     1,
			SHA256:       strings.Repeat(digestByte, 64),
			Dependencies: slices.Clone(pkg.Dependencies),
		})
	}
	lock := PackageLock{
		SchemaVersion: schemaVersion,
		Roots:         slices.Clone(graph.Roots),
		Packages:      packages,
		ResolvedAt:    time.Unix(1_600_000_000, 0).UTC(),
	}
	lock.Digest = PackageLockDigest(lock)
	return lock
}

func installRealRunState(t *testing.T, fixture *runFixture, state State) *Store {
	t.Helper()
	store := &Store{StateDir: fixture.app.Config.StateDir}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	fixture.app.Store = store
	fixture.app.stateStore = nil
	fixture.supervisor.store = nil
	return store
}

func assertRunExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("Run() error = nil, want exit code %d", want)
	}
	var runErr *runError
	if !errors.As(err, &runErr) {
		t.Fatalf("Run() error = %T %v, want *runError", err, err)
	}
	if runErr.Code != want {
		t.Fatalf("Run() exit code = %d, want %d", runErr.Code, want)
	}
}

func assertEventBefore(t *testing.T, events []string, before, after string) {
	t.Helper()
	beforeIndex := slices.Index(events, before)
	afterIndex := slices.Index(events, after)
	if beforeIndex < 0 || afterIndex < 0 || beforeIndex >= afterIndex {
		t.Fatalf("events = %q, want %q before %q", events, before, after)
	}
}

func assertNoEvent(t *testing.T, events []string, event string) {
	t.Helper()
	if slices.Contains(events, event) {
		t.Fatalf("events = %q, must not contain %q", events, event)
	}
}

func countEvent(events []string, event string) int {
	count := 0
	for _, got := range events {
		if got == event {
			count++
		}
	}
	return count
}
