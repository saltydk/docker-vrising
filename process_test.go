package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeWaitResult struct {
	exitCode int
	err      error
}

type fakeManagedProcess struct {
	mu            sync.Mutex
	pid           int
	startTicks    uint64
	startTicksErr error
	wait          chan fakeWaitResult
	waitOnce      sync.Once
	waitCalls     int
	tickCalls     int
	signals       []os.Signal
	exitOn        map[os.Signal]int
}

func newFakeManagedProcess(pid int) *fakeManagedProcess {
	return &fakeManagedProcess{
		pid:        pid,
		startTicks: uint64(pid * 10),
		wait:       make(chan fakeWaitResult, 1),
		exitOn:     make(map[os.Signal]int),
	}
}

func (p *fakeManagedProcess) PID() int { return p.pid }

func (p *fakeManagedProcess) StartTicks() (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tickCalls++
	return p.startTicks, p.startTicksErr
}

func (p *fakeManagedProcess) SignalGroup(signal os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, signal)
	exitCode, exits := p.exitOn[signal]
	p.mu.Unlock()
	if exits {
		p.finish(exitCode, nil)
	}
	return nil
}

func (p *fakeManagedProcess) Wait() (int, error) {
	p.mu.Lock()
	p.waitCalls++
	p.mu.Unlock()
	result := <-p.wait
	return result.exitCode, result.err
}

func (p *fakeManagedProcess) finish(exitCode int, err error) {
	p.waitOnce.Do(func() {
		p.wait <- fakeWaitResult{exitCode: exitCode, err: err}
	})
}

func (p *fakeManagedProcess) snapshot() (int, int, []os.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tickCalls, p.waitCalls, slices.Clone(p.signals)
}

type fakeProcessFactory struct {
	mu              sync.Mutex
	xvfb            *fakeManagedProcess
	wine            *fakeManagedProcess
	display         string
	startOrder      []string
	xvfbArgs        []string
	wineSpecs       []CommandSpec
	wineServerEnvs  [][]string
	wineStarted     chan struct{}
	wineStartedOnce sync.Once
	startXvfbErr    error
	startWineErr    error
	killWineHook    func()
}

func newFakeProcessFactory() *fakeProcessFactory {
	xvfb := newFakeManagedProcess(101)
	xvfb.exitOn[syscall.SIGTERM] = 0
	xvfb.exitOn[syscall.SIGKILL] = 0
	return &fakeProcessFactory{
		xvfb:        xvfb,
		wine:        newFakeManagedProcess(202),
		display:     "77",
		wineStarted: make(chan struct{}),
	}
}

func (f *fakeProcessFactory) StartXvfb(_ context.Context, args []string) (ManagedProcess, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startOrder = append(f.startOrder, "xvfb")
	f.xvfbArgs = slices.Clone(args)
	return f.xvfb, f.display, f.startXvfbErr
}

func (f *fakeProcessFactory) StartWine(_ context.Context, spec CommandSpec) (ManagedProcess, error) {
	f.mu.Lock()
	f.startOrder = append(f.startOrder, "wine")
	f.wineSpecs = append(f.wineSpecs, cloneProcessCommandSpec(spec))
	f.mu.Unlock()
	f.wineStartedOnce.Do(func() { close(f.wineStarted) })
	return f.wine, f.startWineErr
}

func (f *fakeProcessFactory) KillWineServer(_ context.Context, env []string) error {
	f.mu.Lock()
	f.wineServerEnvs = append(f.wineServerEnvs, slices.Clone(env))
	hook := f.killWineHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func cloneProcessCommandSpec(spec CommandSpec) CommandSpec {
	spec.Args = slices.Clone(spec.Args)
	spec.Env = slices.Clone(spec.Env)
	return spec
}

func (f *fakeProcessFactory) snapshot() ([]string, []string, []CommandSpec, [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	order := slices.Clone(f.startOrder)
	xvfbArgs := slices.Clone(f.xvfbArgs)
	specs := make([]CommandSpec, len(f.wineSpecs))
	for index, spec := range f.wineSpecs {
		specs[index] = cloneProcessCommandSpec(spec)
	}
	kills := make([][]string, len(f.wineServerEnvs))
	for index, env := range f.wineServerEnvs {
		kills[index] = slices.Clone(env)
	}
	return order, xvfbArgs, specs, kills
}

type supervisorOutcome struct {
	result RunResult
	err    error
}

func testLaunchRequest(t *testing.T, modsEnabled bool) (LaunchRequest, *Store) {
	t.Helper()
	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(serverDir, "BepInEx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ServerDir:           serverDir,
		DataDir:             dataDir,
		StateDir:            filepath.Join(root, "state"),
		ModsEnabled:         modsEnabled,
		KindredVersion:      "1.2.3",
		SatisvamporyVersion: "4.5.6",
		StartupTimeout:      time.Minute,
		ShutdownTimeout:     time.Minute,
		GameEnv:             make(map[string]string),
		BaseEnv:             []string{"PATH=/usr/bin"},
	}
	return LaunchRequest{
		Config:     cfg,
		Identity:   RuntimeIdentity{UID: 1000, GID: 1000, Home: filepath.Join(root, "home")},
		Generation: "generation-1",
		SteamBuild: "steam-build-1",
	}, &Store{StateDir: cfg.StateDir}
}

func testSupervisor(store *Store, factory ProcessFactory) *Supervisor {
	return &Supervisor{
		Store:     store,
		Readiness: &ReadinessMonitor{},
		Processes: factory,
		Now: func() time.Time {
			return time.Date(2026, time.September, 5, 12, 34, 56, 789, time.UTC)
		},
		waitReadiness: func(context.Context, ReadinessMonitor, ExpectedReadiness) error {
			return nil
		},
		signals: make(chan os.Signal),
		after: func(time.Duration) <-chan time.Time {
			return make(chan time.Time)
		},
	}
}

func runSupervisor(t *testing.T, supervisor *Supervisor, request LaunchRequest, factory *fakeProcessFactory) <-chan supervisorOutcome {
	t.Helper()
	outcome := make(chan supervisorOutcome, 1)
	go func() {
		result, err := supervisor.Run(t.Context(), request)
		outcome <- supervisorOutcome{result: result, err: err}
	}()
	select {
	case <-factory.wineStarted:
	case <-time.After(time.Second):
		t.Fatal("Wine did not start")
	}
	return outcome
}

func receiveOutcome(t *testing.T, outcome <-chan supervisorOutcome) supervisorOutcome {
	t.Helper()
	select {
	case got := <-outcome:
		return got
	case <-time.After(time.Second):
		t.Fatal("Supervisor.Run did not return")
		return supervisorOutcome{}
	}
}

func envValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func TestSupervisorStartsXvfbBeforeWine(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)
	outcome := runSupervisor(t, supervisor, request, factory)
	factory.wine.finish(0, nil)
	got := receiveOutcome(t, outcome)
	if got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}

	order, xvfbArgs, _, _ := factory.snapshot()
	if !slices.Equal(order, []string{"xvfb", "wine"}) {
		t.Fatalf("start order = %v", order)
	}
	if !slices.Contains(xvfbArgs, "-displayfd") {
		t.Fatalf("Xvfb args = %q, want -displayfd", xvfbArgs)
	}
	if slices.ContainsFunc(xvfbArgs, func(arg string) bool { return strings.Contains(arg, "/tmp/.X") }) {
		t.Fatalf("Xvfb args unexpectedly reference X locks: %q", xvfbArgs)
	}
}

func TestSupervisorBuildsLiteralWineArguments(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)

	runOnce := func(factory *fakeProcessFactory) CommandSpec {
		supervisor.Processes = factory
		outcome := runSupervisor(t, supervisor, request, factory)
		factory.wine.finish(0, nil)
		if got := receiveOutcome(t, outcome); got.err != nil {
			t.Fatalf("Run() error = %v", got.err)
		}
		_, _, specs, _ := factory.snapshot()
		if len(specs) != 1 {
			t.Fatalf("Wine starts = %d, want 1", len(specs))
		}
		return specs[0]
	}

	first := runOnce(factory)
	second := runOnce(newFakeProcessFactory())
	if first.Path != "wine64" {
		t.Fatalf("Wine path = %q", first.Path)
	}
	if len(first.Args) != 5 || first.Args[0] != "VRisingServer.exe" || first.Args[1] != "-persistentDataPath" || first.Args[2] != request.Config.DataDir || first.Args[3] != "-logFile" {
		t.Fatalf("Wine args = %q", first.Args)
	}
	if first.Dir != request.Config.ServerDir {
		t.Fatalf("Wine dir = %q", first.Dir)
	}
	if first.Args[4] == second.Args[4] {
		t.Fatalf("server log path was reused: %q", first.Args[4])
	}
	if !strings.Contains(filepath.Base(first.Args[4]), "20260905T123456") {
		t.Fatalf("server log is not dated: %q", first.Args[4])
	}
}

func TestSupervisorMergesWinHTTPOverrideWhenModsEnabled(t *testing.T) {
	request, store := testLaunchRequest(t, true)
	request.Config.BaseEnv = []string{
		"PATH=/usr/bin",
		"WINEDLLOVERRIDES=foo,winhttp=n;bar=b",
		"WINEDEBUG=+seh,+tid",
	}
	request.Config.GameEnv["WINEDEBUG"] = "+seh,+tid"
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)
	outcome := runSupervisor(t, supervisor, request, factory)
	factory.wine.finish(0, nil)
	if got := receiveOutcome(t, outcome); got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}

	_, _, specs, _ := factory.snapshot()
	overrides, _ := envValue(specs[0].Env, "WINEDLLOVERRIDES")
	if overrides != "foo=n;bar=b;winhttp=n,b" {
		t.Fatalf("WINEDLLOVERRIDES = %q", overrides)
	}
	debug, _ := envValue(specs[0].Env, "WINEDEBUG")
	if debug != "+seh,+tid" {
		t.Fatalf("WINEDEBUG = %q", debug)
	}
}

func TestSupervisorForcesBuiltinWinHTTPWhenModsDisabled(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	request.Config.BaseEnv = []string{"WINEDLLOVERRIDES=foo,winhttp=n;bar=b", "WINEDEBUG=-all"}
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)
	outcome := runSupervisor(t, supervisor, request, factory)
	factory.wine.finish(0, nil)
	if got := receiveOutcome(t, outcome); got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}

	_, _, specs, _ := factory.snapshot()
	overrides, _ := envValue(specs[0].Env, "WINEDLLOVERRIDES")
	if overrides != "foo=n;bar=b;winhttp=b" {
		t.Fatalf("WINEDLLOVERRIDES = %q", overrides)
	}
	debug, _ := envValue(specs[0].Env, "WINEDEBUG")
	if debug != "-all" {
		t.Fatalf("WINEDEBUG = %q", debug)
	}
}

func TestSupervisorForwardsTERMAndINTToProcessGroup(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(signal.String(), func(t *testing.T) {
			request, store := testLaunchRequest(t, false)
			factory := newFakeProcessFactory()
			factory.wine.exitOn[signal] = 0
			supervisor := testSupervisor(store, factory)
			signals := make(chan os.Signal, 1)
			supervisor.signals = signals
			outcome := runSupervisor(t, supervisor, request, factory)
			signals <- signal
			if got := receiveOutcome(t, outcome); got.err != nil {
				t.Fatalf("Run() error = %v", got.err)
			}
			_, _, forwarded := factory.wine.snapshot()
			if !slices.Equal(forwarded, []os.Signal{signal}) {
				t.Fatalf("Wine signals = %v, want %v", forwarded, signal)
			}
		})
	}
}

func TestSupervisorEscalatesOnceAfterTimeout(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	factory := newFakeProcessFactory()
	factory.wine.exitOn[syscall.SIGKILL] = 137
	supervisor := testSupervisor(store, factory)
	timeouts := make(chan time.Time, 2)
	supervisor.after = func(time.Duration) <-chan time.Time { return timeouts }
	signals := make(chan os.Signal, 1)
	supervisor.signals = signals

	outcome := runSupervisor(t, supervisor, request, factory)
	signals <- syscall.SIGTERM
	timeouts <- time.Time{}
	timeouts <- time.Time{}
	got := receiveOutcome(t, outcome)
	if got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}
	_, _, wineSignals := factory.wine.snapshot()
	if !slices.Equal(wineSignals, []os.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Fatalf("Wine signals = %v", wineSignals)
	}
	_, _, _, kills := factory.snapshot()
	if len(kills) != 1 {
		t.Fatalf("wineserver -k calls = %d, want 1", len(kills))
	}
}

func TestSupervisorReapsXvfbAndWine(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)
	outcome := runSupervisor(t, supervisor, request, factory)
	factory.wine.finish(0, nil)
	if got := receiveOutcome(t, outcome); got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}

	_, wineWaits, _ := factory.wine.snapshot()
	_, xvfbWaits, _ := factory.xvfb.snapshot()
	if wineWaits != 1 || xvfbWaits != 1 {
		t.Fatalf("Wait calls: Wine=%d Xvfb=%d, want 1 each", wineWaits, xvfbWaits)
	}
}

func TestSupervisorReturnsServerExitStatus(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)
	outcome := runSupervisor(t, supervisor, request, factory)
	factory.wine.finish(23, nil)
	got := receiveOutcome(t, outcome)
	if got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}
	if got.result.ExitCode != 23 {
		t.Fatalf("ExitCode = %d, want 23", got.result.ExitCode)
	}
}

func TestSupervisorStopsCandidateWhenReadinessTimesOut(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	factory := newFakeProcessFactory()
	factory.wine.exitOn[syscall.SIGTERM] = 0
	supervisor := testSupervisor(store, factory)
	supervisor.waitReadiness = func(context.Context, ReadinessMonitor, ExpectedReadiness) error {
		return context.DeadlineExceeded
	}

	got := receiveOutcome(t, runSupervisor(t, supervisor, request, factory))
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline exceeded", got.err)
	}
	if got.result.Ready {
		t.Fatal("Ready = true after readiness timeout")
	}
	_, _, signals := factory.wine.snapshot()
	if !slices.Equal(signals, []os.Signal{syscall.SIGTERM}) {
		t.Fatalf("Wine signals = %v", signals)
	}
}

func TestSupervisorRecordsPIDAndExactReadinessVersions(t *testing.T) {
	request, store := testLaunchRequest(t, true)
	request.Config.KindredVersion = "latest"
	request.Config.SatisvamporyVersion = "latest"
	if err := store.SavePackageLock(PackageLock{
		SchemaVersion: schemaVersion,
		Roots: []PackageRef{
			{Namespace: "odjit", Name: "KindredCommands", Version: "2.3.4"},
			{Namespace: "skytech6", Name: "Satisvampory", Version: "5.6.7"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)
	checked := make(chan error, 1)
	supervisor.waitReadiness = func(_ context.Context, _ ReadinessMonitor, expected ExpectedReadiness) error {
		state, err := store.Load()
		if err == nil && state.Runtime.Server != (ProcessIdentity{PID: 202, StartTicks: 2020}) {
			err = errors.New("runtime process identity was not recorded before readiness")
		}
		if err == nil && (expected.KindredVersion != "2.3.4" || expected.SatisvamporyVersion != "5.6.7" || !expected.RequireMods) {
			err = errors.New("readiness did not receive exact root versions")
		}
		checked <- err
		return nil
	}
	outcome := runSupervisor(t, supervisor, request, factory)
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
	factory.wine.finish(0, nil)
	if got := receiveOutcome(t, outcome); got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}
	ticks, _, _ := factory.wine.snapshot()
	if ticks != 1 {
		t.Fatalf("StartTicks calls = %d, want 1", ticks)
	}
}

func TestSupervisorRemovesStaleBepInExLogWithoutFollowingLinks(t *testing.T) {
	t.Run("final symlink", func(t *testing.T) {
		request, store := testLaunchRequest(t, false)
		target := filepath.Join(t.TempDir(), "target.log")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		stale := filepath.Join(request.Config.ServerDir, "BepInEx", "LogOutput.log")
		if err := os.Symlink(target, stale); err != nil {
			t.Fatal(err)
		}
		factory := newFakeProcessFactory()
		supervisor := testSupervisor(store, factory)
		outcome := runSupervisor(t, supervisor, request, factory)
		factory.wine.finish(0, nil)
		if got := receiveOutcome(t, outcome); got.err != nil {
			t.Fatalf("Run() error = %v", got.err)
		}
		if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale link still exists: %v", err)
		}
		content, err := os.ReadFile(target)
		if err != nil || string(content) != "keep" {
			t.Fatalf("symlink target changed: content=%q err=%v", content, err)
		}
	})

	t.Run("parent symlink", func(t *testing.T) {
		request, store := testLaunchRequest(t, false)
		if err := os.Remove(filepath.Join(request.Config.ServerDir, "BepInEx")); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		target := filepath.Join(outside, "LogOutput.log")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(request.Config.ServerDir, "BepInEx")); err != nil {
			t.Fatal(err)
		}
		factory := newFakeProcessFactory()
		supervisor := testSupervisor(store, factory)
		_, err := supervisor.Run(t.Context(), request)
		if err == nil {
			t.Fatal("Run() error = nil with symlinked BepInEx directory")
		}
		if content, readErr := os.ReadFile(target); readErr != nil || string(content) != "keep" {
			t.Fatalf("outside log changed: content=%q err=%v", content, readErr)
		}
		order, _, _, _ := factory.snapshot()
		if !slices.Equal(order, []string{"xvfb"}) {
			t.Fatalf("start order = %v, Wine must not start", order)
		}
		_, xvfbWaits, _ := factory.xvfb.snapshot()
		if xvfbWaits != 1 {
			t.Fatalf("Xvfb Wait calls = %d, want 1", xvfbWaits)
		}
	})
}

func TestSupervisorCancelsReadinessWhenWineExits(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	factory := newFakeProcessFactory()
	supervisor := testSupervisor(store, factory)
	readinessStopped := make(chan struct{})
	supervisor.waitReadiness = func(ctx context.Context, _ ReadinessMonitor, _ ExpectedReadiness) error {
		<-ctx.Done()
		close(readinessStopped)
		return ctx.Err()
	}
	outcome := runSupervisor(t, supervisor, request, factory)
	factory.wine.finish(0, nil)
	if got := receiveOutcome(t, outcome); got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}
	select {
	case <-readinessStopped:
	case <-time.After(time.Second):
		t.Fatal("readiness worker was not stopped")
	}
}

func TestSupervisorReapsProcessesWhenStartTicksFails(t *testing.T) {
	request, store := testLaunchRequest(t, false)
	factory := newFakeProcessFactory()
	factory.wine.startTicksErr = errors.New("stat failed")
	factory.wine.exitOn[syscall.SIGTERM] = 0
	supervisor := testSupervisor(store, factory)
	_, err := supervisor.Run(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "start ticks") {
		t.Fatalf("Run() error = %v", err)
	}
	_, wineWaits, _ := factory.wine.snapshot()
	_, xvfbWaits, _ := factory.xvfb.snapshot()
	if wineWaits != 1 || xvfbWaits != 1 {
		t.Fatalf("Wait calls: Wine=%d Xvfb=%d, want 1 each", wineWaits, xvfbWaits)
	}
}
