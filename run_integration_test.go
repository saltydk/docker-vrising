package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestApplicationNormalRestartClearsCachedDegradedState(t *testing.T) {
	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	for _, dir := range []string{serverDir, dataDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{StateDir: stateDir}
	const oldReason = "cached remote metadata failure"
	if err := store.Save(State{
		SchemaVersion: schemaVersion,
		SteamBuild:    "100",
		Runtime: RuntimeState{
			Phase:      "stopped",
			Degraded:   true,
			Reason:     oldReason,
			SteamBuild: "100",
		},
	}); err != nil {
		t.Fatal(err)
	}
	recorder := &runRecorder{}
	steam := &recordingSteamLifecycle{
		recorder:  recorder,
		installed: SteamBuild{BuildID: "100", DepotManifest: "manifest", Branch: publicBranch},
		remote:    SteamBuild{BuildID: "100", DepotManifest: "manifest", Branch: publicBranch},
	}
	backups := &recordingBackupCreator{recorder: recorder}
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)
	var output bytes.Buffer
	supervisor.Readiness.Output = &output
	starting := make(chan RuntimeState, 1)
	supervisor.waitReadiness = func(context.Context, ReadinessMonitor, ExpectedReadiness) error {
		state, err := store.Load()
		if err != nil {
			return err
		}
		starting <- state.Runtime
		return nil
	}
	readyPersisted := make(chan struct{})
	var readyOnce sync.Once
	store.syncDirectory = func(fd int) error {
		if err := unix.Fsync(fd); err != nil {
			return err
		}
		state, err := store.Load()
		if err == nil && state.Runtime.Ready {
			readyOnce.Do(func() { close(readyPersisted) })
		}
		return nil
	}
	app := &Application{
		Config: Config{
			ServerDir:       serverDir,
			DataDir:         dataDir,
			StateDir:        stateDir,
			UpdateGame:      true,
			ModsEnabled:     false,
			StartupTimeout:  time.Minute,
			ShutdownTimeout: time.Minute,
			LogDays:         7,
		},
		Store:      store,
		Steam:      steam,
		Backups:    backups,
		Supervisor: supervisor,
		validateMounts: func(Config) error {
			return nil
		},
	}
	outcome := make(chan error, 1)
	go func() { outcome <- app.Run(t.Context()) }()

	gotStarting := <-starting
	if gotStarting.Degraded || gotStarting.Reason != "" || gotStarting.Phase != "starting" {
		t.Fatalf("normal starting runtime = %#v", gotStarting)
	}
	select {
	case <-readyPersisted:
	case err := <-outcome:
		t.Fatalf("Run() returned before ready: %v", err)
	case <-time.After(time.Second):
		t.Fatal("ready state was not persisted")
	}
	ready, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if ready.Runtime.Degraded || ready.Runtime.Reason != "" || !ready.Runtime.Ready {
		t.Fatalf("normal ready runtime = %#v", ready.Runtime)
	}
	if output.String() != "" {
		t.Fatalf("normal restart repeated degraded warning %q", output.String())
	}
	factory.wine.finish(0, nil)
	if err := <-outcome; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	stopped, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Runtime.Degraded || stopped.Runtime.Reason != "" || stopped.Runtime.Phase != "stopped" {
		t.Fatalf("normal stopped runtime = %#v", stopped.Runtime)
	}
}

func TestApplicationPromotesWithConcreteSupervisorWhileWineIsAlive(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("live")})
	if err := manager.Apply(t.Context(), staged); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(manager.Store, factory)
	readyPersisted := make(chan struct{})
	var readyOnce sync.Once
	manager.Store.syncDirectory = func(fd int) error {
		if err := unix.Fsync(fd); err != nil {
			return err
		}
		state, err := manager.Store.Load()
		if err == nil && state.Runtime.Ready {
			readyOnce.Do(func() { close(readyPersisted) })
		}
		return nil
	}
	app := &Application{
		Config: Config{
			ServerDir:           manager.ServerDir,
			DataDir:             dataDir,
			StateDir:            manager.Store.StateDir,
			ModsEnabled:         true,
			KindredVersion:      staged.Lock.Roots[0].Version,
			SatisvamporyVersion: staged.Lock.Roots[1].Version,
			StartupTimeout:      time.Minute,
			ShutdownTimeout:     time.Minute,
			LogDays:             7,
			BaseEnv:             []string{"PATH=/usr/bin"},
			GameEnv:             make(map[string]string),
		},
		Mods:            manager,
		Supervisor:      supervisor,
		pruneServerLogs: func(string, int, time.Time) error { return nil },
	}
	outcome := make(chan error, 1)
	go func() {
		outcome <- app.launch(t.Context(), staged, SteamBuild{BuildID: "known-good-build"}, true)
	}()
	select {
	case <-factory.wineStarted:
	case <-time.After(time.Second):
		t.Fatal("Wine did not start")
	}
	select {
	case <-readyPersisted:
	case err := <-outcome:
		t.Fatalf("launch returned before ready state: %v", err)
	case <-time.After(time.Second):
		t.Fatal("ready state was not persisted")
	}
	select {
	case err := <-outcome:
		t.Fatalf("launch returned while ready Wine was alive: %v", err)
	default:
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := manager.Store.LoadPackageLock()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.ID != staged.Record.ID || state.Candidate != nil || state.Transaction != nil ||
		state.Promotion != nil || !state.Runtime.Ready || state.SteamBuild != "known-good-build" || !samePackageLock(lock, staged.Lock) {
		t.Fatalf("live promoted state = %#v, lock = %#v", state, lock)
	}

	factory.wine.finish(0, nil)
	if err := <-outcome; err != nil {
		t.Fatalf("launch error = %v", err)
	}
}

func TestApplicationRecoversPendingConcretePromotionBeforeLaunch(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("first")})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("second")})
	if err := manager.Apply(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	manager.promotionHook = func(stage string) error {
		if stage == "lock-written" {
			return errors.New("injected restart boundary")
		}
		return nil
	}
	if outcome, err := manager.CommitPromotion(t.Context(), second); err == nil || outcome != PromotionRecoveryRequired {
		t.Fatalf("CommitPromotion() = %v, %v, want recovery required", outcome, err)
	}
	manager.promotionHook = nil

	dataDir := t.TempDir()
	recorder := &runRecorder{}
	steam := &recordingSteamLifecycle{
		recorder:  recorder,
		installed: SteamBuild{BuildID: "100", DepotManifest: "manifest", Branch: publicBranch},
	}
	supervisor := &recordingServerSupervisor{recorder: recorder, result: RunResult{Ready: true}}
	app := &Application{
		Config: Config{
			ServerDir:       manager.ServerDir,
			DataDir:         dataDir,
			StateDir:        manager.Store.StateDir,
			ModsEnabled:     false,
			UpdateGame:      false,
			StartupTimeout:  time.Minute,
			ShutdownTimeout: time.Minute,
			LogDays:         7,
		},
		Store:      manager.Store,
		Mods:       manager,
		Steam:      steam,
		Supervisor: supervisor,
		validateMounts: func(Config) error {
			return nil
		},
		pruneServerLogs: func(string, int, time.Time) error { return nil },
	}

	if err := app.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := manager.Store.LoadPackageLock()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.ID != second.Record.ID || state.Promotion != nil || state.Candidate != nil ||
		state.Transaction != nil || !samePackageLock(lock, second.Lock) {
		t.Fatalf("recovered application state = %#v, lock = %#v", state, lock)
	}
}

func TestApplicationPromotionFailurePreservesConcreteForwardRecovery(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("recovery")})
	if err := manager.Apply(t.Context(), staged); err != nil {
		t.Fatal(err)
	}
	manager.promotionHook = func(stage string) error {
		if stage == "lock-written" {
			return errors.New("injected live promotion failure")
		}
		return nil
	}
	dataDir := t.TempDir()
	factory := newFakeProcessFactory()
	factory.wine.exitOn[os.Interrupt] = 0
	factory.wine.exitOn[unix.SIGTERM] = 0
	supervisor := testSupervisor(manager.Store, factory)
	app := &Application{
		Config: Config{
			ServerDir:           manager.ServerDir,
			DataDir:             dataDir,
			StateDir:            manager.Store.StateDir,
			ModsEnabled:         true,
			KindredVersion:      staged.Lock.Roots[0].Version,
			SatisvamporyVersion: staged.Lock.Roots[1].Version,
			StartupTimeout:      time.Minute,
			ShutdownTimeout:     time.Minute,
			LogDays:             7,
			BaseEnv:             []string{"PATH=/usr/bin"},
			GameEnv:             make(map[string]string),
		},
		Mods:            manager,
		Supervisor:      supervisor,
		pruneServerLogs: func(string, int, time.Time) error { return nil },
	}

	err := app.launch(t.Context(), staged, SteamBuild{BuildID: "candidate-build"}, true)
	assertRunExitCode(t, err, exitReadiness)
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := manager.Store.LoadPackageLock()
	if err != nil {
		t.Fatal(err)
	}
	if state.Promotion == nil || state.Candidate == nil || state.Transaction == nil || state.Active != nil ||
		!samePackageLock(lock, staged.Lock) {
		t.Fatalf("preserved forward recovery state = %#v, lock = %#v", state, lock)
	}

	manager.promotionHook = nil
	outcome, err := manager.RecoverPromotion(t.Context())
	if err != nil || outcome != PromotionCommitted {
		t.Fatalf("RecoverPromotion() = %v, %v, want committed", outcome, err)
	}
	state, err = manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.ID != staged.Record.ID || state.Candidate != nil || state.Transaction != nil || state.Promotion != nil {
		t.Fatalf("recovered live promotion state = %#v", state)
	}
}

func TestApplicationStrictKnownGoodUsesConcreteValidators(t *testing.T) {
	t.Run("corrupt active overlay", func(t *testing.T) {
		manager, staged, steam, app := concreteKnownGoodApplication(t, true)
		writeTestFile(t, filepath.Join(staged.Dir, "winhttp.dll"), "corrupt")

		err := app.Run(t.Context())
		assertRunExitCode(t, err, exitModUpdate)
		if state, loadErr := manager.Store.Load(); loadErr != nil || state.Runtime.Server != (ProcessIdentity{}) {
			t.Fatalf("state after corrupt overlay = %#v, %v", state, loadErr)
		}
		if _, err := steam.InstalledBuild(); err != nil {
			t.Fatalf("fixture manifest error = %v", err)
		}
	})

	t.Run("missing executable", func(t *testing.T) {
		_, _, _, app := concreteKnownGoodApplication(t, false)

		err := app.Run(t.Context())
		assertRunExitCode(t, err, exitSteam)
	})
}

func concreteKnownGoodApplication(t *testing.T, executable bool) (*ModManager, StagedGeneration, *SteamClient, *Application) {
	t.Helper()
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("active")})
	applyAndPromote(t, manager, staged)
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.SteamBuild = steamTestBuildID
	if err := manager.Store.Save(state); err != nil {
		t.Fatal(err)
	}
	steam := &SteamClient{ServerDir: manager.ServerDir}
	installSteamFixture(t, steam, publicBranch, executable, steamRuntimeAppID)
	recorder := &runRecorder{}
	supervisor := &recordingServerSupervisor{recorder: recorder, result: RunResult{Ready: true}}
	app := &Application{
		Config: Config{
			ServerDir:           manager.ServerDir,
			DataDir:             t.TempDir(),
			StateDir:            manager.Store.StateDir,
			ModsEnabled:         true,
			UpdateMods:          false,
			UpdateGame:          false,
			KindredVersion:      staged.Lock.Roots[0].Version,
			SatisvamporyVersion: staged.Lock.Roots[1].Version,
			StartupTimeout:      time.Minute,
			ShutdownTimeout:     time.Minute,
			LogDays:             7,
		},
		Store:      manager.Store,
		Mods:       manager,
		Steam:      steam,
		Supervisor: supervisor,
		validateMounts: func(Config) error {
			return nil
		},
		pruneServerLogs: func(string, int, time.Time) error { return nil },
	}
	return manager, staged, steam, app
}
