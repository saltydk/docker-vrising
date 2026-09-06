package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultXvfbPath       = "Xvfb"
	defaultWinePath       = "wine64"
	defaultWineServerPath = "wineserver"
	xvfbDisplayFD         = 3
)

var serverLogSequence atomic.Uint64

type ManagedProcess interface {
	PID() int
	StartTicks() (uint64, error)
	SignalGroup(os.Signal) error
	Wait() (int, error)
}

type ProcessFactory interface {
	InitializeWine(context.Context, []string) error
	StartXvfb(context.Context, []string) (ManagedProcess, string, error)
	StartWine(context.Context, CommandSpec) (ManagedProcess, error)
	KillWineServer(context.Context, []string) error
}

type LaunchRequest struct {
	Config      Config
	Identity    RuntimeIdentity
	PackageLock PackageLock
	Generation  string
	SteamBuild  string
	OnReady     func(context.Context) error
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
	Output    io.Writer

	waitReadiness func(context.Context, ReadinessMonitor, ExpectedReadiness) error
	signals       <-chan os.Signal
	after         func(time.Duration) <-chan time.Time
}

type processWaitResult struct {
	exitCode int
	err      error
}

type runningProcess struct {
	process    ManagedProcess
	startTicks uint64
	done       chan struct{}
	result     processWaitResult
	stopOnce   sync.Once
	stopErr    error
}

func newRunningProcess(process ManagedProcess) *runningProcess {
	running := &runningProcess{
		process: process,
		done:    make(chan struct{}),
	}
	go func() {
		running.result.exitCode, running.result.err = process.Wait()
		close(running.done)
	}()
	return running
}

func (p *runningProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *runningProcess) recordStartTicks(name string) error {
	startTicks, err := p.process.StartTicks()
	if err != nil {
		return fmt.Errorf("read %s start ticks: %w", name, err)
	}
	p.startTicks = startTicks
	return nil
}

func (p *runningProcess) verifyAlive(name string) error {
	select {
	case <-p.done:
		return processBeforeReadinessError(name, p.result)
	default:
	}
	startTicks, err := p.process.StartTicks()
	if err != nil {
		return fmt.Errorf("%s exited before readiness: verify start ticks: %w", name, err)
	}
	if startTicks != p.startTicks {
		return fmt.Errorf("%s exited before readiness: start ticks changed from %d to %d", name, p.startTicks, startTicks)
	}
	select {
	case <-p.done:
		return processBeforeReadinessError(name, p.result)
	default:
		return nil
	}
}

func (s *Supervisor) Run(ctx context.Context, request LaunchRequest) (result RunResult, returnErr error) {
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}
	if s.Store == nil {
		return RunResult{}, fmt.Errorf("state store is required")
	}
	if s.Readiness == nil {
		return RunResult{}, fmt.Errorf("readiness monitor is required")
	}
	if s.Processes == nil {
		return RunResult{}, fmt.Errorf("process factory is required")
	}
	if request.Config.StartupTimeout <= 0 {
		return RunResult{}, fmt.Errorf("startup timeout must be positive")
	}
	if request.Config.ShutdownTimeout <= 0 {
		return RunResult{}, fmt.Errorf("shutdown timeout must be positive")
	}

	expected, err := s.expectedReadiness(request)
	if err != nil {
		return RunResult{}, err
	}
	state, err := s.Store.Load()
	if err != nil {
		return RunResult{}, fmt.Errorf("load runtime state: %w", err)
	}

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	serverLog := uniqueServerLog(request.Config.DataDir, now())
	if err := os.MkdirAll(filepath.Dir(serverLog), 0o755); err != nil {
		return RunResult{}, fmt.Errorf("create server log directory: %w", err)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	startupCtx, cancelStartup := context.WithTimeout(runCtx, request.Config.StartupTimeout)
	defer cancelStartup()
	signalChannel, stopSignals := s.signalChannel()
	defer stopSignals()

	var xvfb *runningProcess
	var wine *runningProcess
	var wineEnv []string
	var runtimeRecorded bool
	var readinessDone <-chan struct{}
	defer func() {
		cancelRun()
		if readinessDone != nil {
			<-readinessDone
		}
		if wine != nil && !wine.exited() {
			returnErr = errors.Join(returnErr, s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv))
		}
		if xvfb != nil && !xvfb.exited() {
			returnErr = errors.Join(returnErr, s.stopProcess(xvfb, request.Config.ShutdownTimeout))
		}
		if runtimeRecorded {
			latest, err := s.Store.Load()
			if err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("load stopped runtime state: %w", err))
			} else {
				latest.Runtime.Phase = "stopped"
				latest.Runtime.Ready = false
				latest.Runtime.UpdatedAt = now()
				returnErr = errors.Join(returnErr, wrapError("save stopped runtime state", s.Store.Save(latest)))
			}
		}
	}()

	wineEnv = buildWineEnvironment(request, "")
	if s.Output != nil {
		fmt.Fprintf(s.Output, "wine: initializing prefix %s with winecfg before Xvfb\n", filepath.Join(request.Config.StateDir, "wineprefix"))
	}
	initCtx, cancelInit := context.WithCancel(startupCtx)
	defer cancelInit()
	initDone := make(chan error, 1)
	go func() { initDone <- s.Processes.InitializeWine(initCtx, wineEnv) }()
	initProgress := time.NewTicker(30 * time.Second)
	defer initProgress.Stop()
	initialized := false
	for !initialized {
		select {
		case err := <-initDone:
			cancelInit()
			if err != nil {
				return RunResult{}, fmt.Errorf("initialize Wine: %w", err)
			}
			initialized = true
		case received := <-signalChannel:
			cancelInit()
			<-initDone
			return RunResult{}, fmt.Errorf("Wine initialization interrupted by %s", received)
		case <-initCtx.Done():
			cancelInit()
			<-initDone
			return RunResult{}, fmt.Errorf("initialize Wine: %w", initCtx.Err())
		case <-initProgress.C:
			if s.Output != nil {
				fmt.Fprintln(s.Output, "wine: still waiting for prefix initialization")
			}
		}
	}
	initProgress.Stop()
	if s.Output != nil {
		fmt.Fprintln(s.Output, "wine: prefix initialized; starting Xvfb and dedicated server")
	}
	xvfbProcess, display, startErr := s.Processes.StartXvfb(startupCtx, []string{
		"-displayfd", strconv.Itoa(xvfbDisplayFD),
		"-screen", "0", "1024x768x24",
		"-nolisten", "tcp",
	})
	if xvfbProcess != nil {
		xvfb = newRunningProcess(xvfbProcess)
	}
	if startErr != nil {
		return RunResult{}, fmt.Errorf("start Xvfb: %w", startErr)
	}
	if xvfb == nil {
		return RunResult{}, fmt.Errorf("start Xvfb: process is nil")
	}
	if strings.TrimSpace(display) == "" {
		return RunResult{}, fmt.Errorf("start Xvfb: selected display is empty")
	}
	if err := xvfb.recordStartTicks("Xvfb"); err != nil {
		return RunResult{}, err
	}

	if err := removeStaleBepInExLog(request.Config.ServerDir); err != nil {
		return RunResult{}, err
	}

	wineEnv = buildWineEnvironment(request, display)
	wineProcess, startErr := s.Processes.StartWine(runCtx, CommandSpec{
		Path: defaultWinePath,
		Args: []string{
			"VRisingServer.exe",
			"-persistentDataPath", request.Config.DataDir,
			"-logFile", serverLog,
		},
		Env: wineEnv,
		Dir: request.Config.ServerDir,
	})
	if wineProcess != nil {
		wine = newRunningProcess(wineProcess)
	}
	if startErr != nil {
		return RunResult{}, fmt.Errorf("start Wine: %w", startErr)
	}
	if wine == nil {
		return RunResult{}, fmt.Errorf("start Wine: process is nil")
	}

	if err := wine.recordStartTicks("Wine"); err != nil {
		return RunResult{}, err
	}
	state.Runtime.Phase = "starting"
	state.Runtime.Ready = false
	state.Runtime.Server = ProcessIdentity{PID: wine.process.PID(), StartTicks: wine.startTicks}
	state.Runtime.Generation = request.Generation
	state.Runtime.SteamBuild = request.SteamBuild
	state.Runtime.UpdatedAt = now()
	if err := s.Store.Save(state); err != nil {
		return RunResult{}, fmt.Errorf("save starting runtime state: %w", err)
	}
	runtimeRecorded = true
	if state.Runtime.Degraded && s.Readiness.Output != nil {
		fmt.Fprintf(s.Readiness.Output, "warning: starting degraded server: %s\n", shortLogReason(state.Runtime.Reason))
	}

	monitor := *s.Readiness
	monitor.ServerLog = serverLog
	monitor.BepInExLog = filepath.Join(request.Config.ServerDir, "BepInEx", "LogOutput.log")
	waitReadiness := s.waitReadiness
	startupDeadline := startupCtx.Done()
	readinessResults := make(chan error, 1)
	monitoringErrors := make(chan error, 1)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		if waitReadiness != nil {
			readinessResults <- waitReadiness(startupCtx, monitor, expected)
			return
		}
		monitoringErrors <- monitor.watch(runCtx, expected, func() { readinessResults <- nil })
	}()
	readinessDone = monitorDone

	for {
		select {
		case <-startupDeadline:
			return result, fmt.Errorf("wait for server readiness: %w", startupCtx.Err())
		case err := <-monitoringErrors:
			return result, fmt.Errorf("monitor server logs: %w", err)
		case <-xvfb.done:
			cancelRun()
			returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
			result.ExitCode = wine.result.exitCode
			return result, errors.Join(returnErr, unexpectedProcessExitError("Xvfb", xvfb.result))
		case <-wine.done:
			cancelRun()
			result.ExitCode = wine.result.exitCode
			return result, wineExitError(wine, xvfb)
		case readinessErr := <-readinessResults:
			if readinessErr != nil {
				cancelRun()
				returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
				result.ExitCode = wine.result.exitCode
				return result, errors.Join(fmt.Errorf("wait for server readiness: %w", readinessErr), returnErr)
			}
			if err := xvfb.verifyAlive("Xvfb"); err != nil {
				cancelRun()
				returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
				result.ExitCode = wine.result.exitCode
				return result, errors.Join(err, returnErr)
			}
			if err := wine.verifyAlive("Wine"); err != nil {
				cancelRun()
				returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
				result.ExitCode = wine.result.exitCode
				return result, errors.Join(err, returnErr)
			}
			if request.OnReady != nil {
				if err := request.OnReady(runCtx); err != nil {
					cancelRun()
					returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
					result.ExitCode = wine.result.exitCode
					return result, errors.Join(fmt.Errorf("commit server readiness: %w", err), returnErr)
				}
				if err := xvfb.verifyAlive("Xvfb"); err != nil {
					cancelRun()
					returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
					result.ExitCode = wine.result.exitCode
					return result, errors.Join(err, returnErr)
				}
				if err := wine.verifyAlive("Wine"); err != nil {
					cancelRun()
					returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
					result.ExitCode = wine.result.exitCode
					return result, errors.Join(err, returnErr)
				}
			}
			state, err = s.Store.Load()
			if err != nil {
				cancelRun()
				returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
				result.ExitCode = wine.result.exitCode
				return result, errors.Join(fmt.Errorf("reload runtime state after readiness commit: %w", err), returnErr)
			}
			state.Runtime.Phase = "ready"
			state.Runtime.Ready = true
			state.Runtime.UpdatedAt = now()
			state.SteamBuild = request.SteamBuild
			if err := s.Store.Save(state); err != nil {
				cancelRun()
				returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
				result.ExitCode = wine.result.exitCode
				return result, errors.Join(fmt.Errorf("save ready runtime state: %w", err), returnErr)
			}
			result.Ready = true
			startupDeadline = nil
			cancelStartup()
			if s.Output != nil {
				fmt.Fprintln(s.Output, "server: ready; continuing game and mod log streaming")
			}
			readinessResults = nil
		case received := <-signalChannel:
			if received != syscall.SIGTERM && received != os.Interrupt {
				continue
			}
			cancelRun()
			returnErr = s.stopWine(ctx, wine, received, request.Config.ShutdownTimeout, wineEnv)
			result.ExitCode = wine.result.exitCode
			if signal, ok := received.(syscall.Signal); ok && result.ExitCode == 128+int(signal) {
				result.ExitCode = 0
			}
			return result, errors.Join(returnErr, processExitError("Wine", wine.result))
		case <-ctx.Done():
			cancelRun()
			returnErr = s.stopWine(ctx, wine, syscall.SIGTERM, request.Config.ShutdownTimeout, wineEnv)
			result.ExitCode = wine.result.exitCode
			return result, errors.Join(ctx.Err(), returnErr, processExitError("Wine", wine.result))
		}
	}
}

func (s *Supervisor) expectedReadiness(request LaunchRequest) (ExpectedReadiness, error) {
	expected := ExpectedReadiness{RequireMods: request.Config.ModsEnabled}
	if !request.Config.ModsEnabled {
		return expected, nil
	}

	if err := validateManagedRootRefs(request.PackageLock.Roots); err != nil {
		return ExpectedReadiness{}, fmt.Errorf("validate selected readiness roots: %w", err)
	}
	expected.KindredVersion = request.PackageLock.Roots[0].Version
	expected.SatisvamporyVersion = request.PackageLock.Roots[1].Version
	return expected, nil
}

func uniqueServerLog(dataDir string, now time.Time) string {
	sequence := serverLogSequence.Add(1)
	name := fmt.Sprintf("VRisingServer-%s-%06d.log", now.UTC().Format("20060102T150405.000000000Z"), sequence)
	return filepath.Join(dataDir, "logs", name)
}

func removeStaleBepInExLog(serverDir string) error {
	serverFD, err := unix.Open(serverDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open server directory without following links: %w", err)
	}
	defer unix.Close(serverFD)

	bepInExFD, err := unix.Openat(serverFD, "BepInEx", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open BepInEx directory without following links: %w", err)
	}
	defer unix.Close(bepInExFD)

	if err := unix.Unlinkat(bepInExFD, "LogOutput.log", 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove stale BepInEx log without following links: %w", err)
	}
	return nil
}

func buildWineEnvironment(request LaunchRequest, display string) []string {
	env := slices.Clone(request.Config.BaseEnv)
	gameKeys := make([]string, 0, len(request.Config.GameEnv))
	for key := range request.Config.GameEnv {
		gameKeys = append(gameKeys, key)
	}
	slices.Sort(gameKeys)
	for _, key := range gameKeys {
		env = setEnvironmentValue(env, key, request.Config.GameEnv[key])
	}
	if request.Identity.Home != "" {
		env = setEnvironmentValue(env, "HOME", request.Identity.Home)
	}
	env = setEnvironmentValue(env, "WINEPREFIX", filepath.Join(request.Config.StateDir, "wineprefix"))
	if display != "" {
		env = setEnvironmentValue(env, "DISPLAY", ":"+strings.TrimPrefix(strings.TrimSpace(display), ":"))
	} else {
		env = slices.DeleteFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "DISPLAY=") })
	}
	overrides, _ := environmentValue(env, "WINEDLLOVERRIDES")
	override := "winhttp=b"
	if request.Config.ModsEnabled {
		override = "winhttp=n,b"
	}
	env = setEnvironmentValue(env, "WINEDLLOVERRIDES", mergeDLLOverride(overrides, override))
	return env
}

func environmentValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for index := len(env) - 1; index >= 0; index-- {
		if strings.HasPrefix(env[index], prefix) {
			return strings.TrimPrefix(env[index], prefix), true
		}
	}
	return "", false
}

func setEnvironmentValue(env []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				result = append(result, prefix+value)
				replaced = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !replaced {
		result = append(result, prefix+value)
	}
	return result
}

func mergeDLLOverride(current, replacement string) string {
	entries := strings.Split(current, ";")
	merged := make([]string, 0, len(entries)+1)
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		names, mode, found := strings.Cut(entry, "=")
		if !found {
			merged = append(merged, entry)
			continue
		}
		kept := make([]string, 0, strings.Count(names, ",")+1)
		for _, name := range strings.Split(names, ",") {
			if !strings.EqualFold(strings.TrimSpace(name), "winhttp") {
				kept = append(kept, name)
			}
		}
		if len(kept) != 0 {
			merged = append(merged, strings.Join(kept, ",")+"="+mode)
		}
	}
	merged = append(merged, replacement)
	return strings.Join(merged, ";")
}

func (s *Supervisor) signalChannel() (<-chan os.Signal, func()) {
	if s.signals != nil {
		return s.signals, func() {}
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	return signals, func() { signal.Stop(signals) }
}

func (s *Supervisor) timeout(duration time.Duration) <-chan time.Time {
	if s.after != nil {
		return s.after(duration)
	}
	return time.After(duration)
}

func (s *Supervisor) stopWine(parent context.Context, wine *runningProcess, firstSignal os.Signal, timeout time.Duration, env []string) error {
	wine.stopOnce.Do(func() {
		if wine.exited() {
			return
		}
		wine.stopErr = errors.Join(wine.stopErr, ignoreNoProcess(wine.process.SignalGroup(firstSignal)))
		select {
		case <-wine.done:
			return
		case <-s.timeout(timeout):
		}

		killContext, cancelKill := context.WithTimeout(context.WithoutCancel(parent), timeout)
		wine.stopErr = errors.Join(wine.stopErr, wrapError("stop wineserver", s.Processes.KillWineServer(killContext, env)))
		cancelKill()
		select {
		case <-wine.done:
			return
		case <-s.timeout(timeout):
		}

		wine.stopErr = errors.Join(wine.stopErr, wrapError("kill Wine process group", ignoreNoProcess(wine.process.SignalGroup(syscall.SIGKILL))))
		<-wine.done
	})
	return wine.stopErr
}

func (s *Supervisor) stopProcess(process *runningProcess, timeout time.Duration) error {
	process.stopOnce.Do(func() {
		if process.exited() {
			return
		}
		process.stopErr = errors.Join(process.stopErr, wrapError("terminate process group", ignoreNoProcess(process.process.SignalGroup(syscall.SIGTERM))))
		select {
		case <-process.done:
			return
		case <-s.timeout(timeout):
		}
		process.stopErr = errors.Join(process.stopErr, wrapError("kill process group", ignoreNoProcess(process.process.SignalGroup(syscall.SIGKILL))))
		<-process.done
	})
	return process.stopErr
}

func ignoreNoProcess(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func processExitError(name string, result processWaitResult) error {
	if result.err == nil {
		return nil
	}
	return fmt.Errorf("wait for %s: %w", name, result.err)
}

func wineExitError(wine, xvfb *runningProcess) error {
	wineErr := processExitError("Wine", wine.result)
	select {
	case <-xvfb.done:
		return errors.Join(wineErr, unexpectedProcessExitError("Xvfb", xvfb.result))
	default:
		return wineErr
	}
}

func unexpectedProcessExitError(name string, result processWaitResult) error {
	if result.err != nil {
		return fmt.Errorf("%s exited unexpectedly: %w", name, result.err)
	}
	return fmt.Errorf("%s exited unexpectedly with status %d", name, result.exitCode)
}

func processBeforeReadinessError(name string, result processWaitResult) error {
	if result.err != nil {
		return fmt.Errorf("%s exited before readiness: %w", name, result.err)
	}
	return fmt.Errorf("%s exited with status %d before readiness", name, result.exitCode)
}

func wrapError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

type ExecProcessFactory struct {
	XvfbPath       string
	WinePath       string
	WineConfigPath string
	WineServerPath string
	Stdout         io.Writer
	Stderr         io.Writer
}

// InitializeWine follows the upstream headless winecfg + delay sequence.
// Starting Xvfb first can expose interactive installer windows on a fresh prefix.
func (f ExecProcessFactory) InitializeWine(ctx context.Context, env []string) (returnErr error) {
	defer func() {
		if returnErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, f.KillWineServer(cleanupCtx, env))
		}
	}()
	path := f.WineConfigPath
	if path == "" {
		path = "winecfg"
	}
	command := exec.CommandContext(ctx, path)
	command.Env = slices.Clone(env)
	command.Dir = "/"
	command.Stdout = f.output()
	command.Stderr = f.errorOutput()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 2 * time.Second
	if err := command.Run(); err != nil {
		return fmt.Errorf("execute winecfg: %w", err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f ExecProcessFactory) StartXvfb(ctx context.Context, args []string) (ManagedProcess, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	displayReader, displayWriter, err := os.Pipe()
	if err != nil {
		return nil, "", fmt.Errorf("create Xvfb display pipe: %w", err)
	}
	defer displayReader.Close()

	path := f.XvfbPath
	if path == "" {
		path = defaultXvfbPath
	}
	command := exec.Command(path, args...)
	command.ExtraFiles = []*os.File{displayWriter}
	command.Stdout = f.output()
	command.Stderr = f.errorOutput()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		displayWriter.Close()
		return nil, "", fmt.Errorf("execute Xvfb: %w", err)
	}
	process := &execManagedProcess{command: command}
	if err := displayWriter.Close(); err != nil {
		return process, "", fmt.Errorf("close Xvfb display writer: %w", err)
	}

	display, err := readDisplay(ctx, displayReader)
	if err != nil {
		return process, "", err
	}
	return process, display, nil
}

func (f ExecProcessFactory) StartWine(ctx context.Context, spec CommandSpec) (ManagedProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := spec.Path
	if path == "" || path == defaultWinePath {
		if f.WinePath != "" {
			path = f.WinePath
		} else {
			path = defaultWinePath
		}
	}
	command := exec.Command(path, spec.Args...)
	command.Env = slices.Clone(spec.Env)
	command.Dir = spec.Dir
	command.Stdout = f.output()
	command.Stderr = f.errorOutput()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("execute wine64: %w", err)
	}
	return &execManagedProcess{command: command}, nil
}

func (f ExecProcessFactory) KillWineServer(ctx context.Context, env []string) error {
	path := f.WineServerPath
	if path == "" {
		path = defaultWineServerPath
	}
	command := exec.CommandContext(ctx, path, "-k")
	command.Env = slices.Clone(env)
	command.Stdout = f.output()
	command.Stderr = f.errorOutput()
	if err := command.Run(); err != nil {
		return fmt.Errorf("execute wineserver -k: %w", err)
	}
	return nil
}

func (f ExecProcessFactory) output() io.Writer {
	if f.Stdout != nil {
		return f.Stdout
	}
	return os.Stdout
}

func (f ExecProcessFactory) errorOutput() io.Writer {
	if f.Stderr != nil {
		return f.Stderr
	}
	return os.Stderr
}

func readDisplay(ctx context.Context, displayReader *os.File) (string, error) {
	reader := bufio.NewReader(displayReader)
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("wait for Xvfb display: %w", err)
		}
		if err := displayReader.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return "", fmt.Errorf("set Xvfb display read deadline: %w", err)
		}
		line, err := reader.ReadString('\n')
		if err == nil {
			display := strings.TrimSpace(line)
			if _, parseErr := strconv.ParseUint(display, 10, 31); parseErr != nil {
				return "", fmt.Errorf("parse Xvfb display %q: %w", display, parseErr)
			}
			return display, nil
		}
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			continue
		}
		return "", fmt.Errorf("read Xvfb display: %w", err)
	}
}

type execManagedProcess struct {
	command *exec.Cmd
}

func (p *execManagedProcess) PID() int {
	return p.command.Process.Pid
}

func (p *execManagedProcess) StartTicks() (uint64, error) {
	content, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(p.PID()), "stat"))
	if err != nil {
		return 0, err
	}
	return parseProcStartTicks(content)
}

func parseProcStartTicks(content []byte) (uint64, error) {
	closingParenthesis := strings.LastIndexByte(string(content), ')')
	if closingParenthesis < 0 {
		return 0, fmt.Errorf("process stat has no command terminator")
	}
	fields := strings.Fields(string(content[closingParenthesis+1:]))
	const startTimeIndexAfterCommand = 19
	if len(fields) <= startTimeIndexAfterCommand {
		return 0, fmt.Errorf("process stat has %d fields after command, need at least %d", len(fields), startTimeIndexAfterCommand+1)
	}
	if err := validateRunningProcessState(fields[0]); err != nil {
		return 0, err
	}
	startTicks, err := strconv.ParseUint(fields[startTimeIndexAfterCommand], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start ticks: %w", err)
	}
	return startTicks, nil
}

func (p *execManagedProcess) SignalGroup(signal os.Signal) error {
	unixSignal, ok := signal.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported process signal %T", signal)
	}
	return syscall.Kill(-p.PID(), unixSignal)
}

func (p *execManagedProcess) Wait() (int, error) {
	err := p.command.Wait()
	if p.command.ProcessState == nil {
		return -1, err
	}
	exitCode := p.command.ProcessState.ExitCode()
	if status, ok := p.command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		exitCode = 128 + int(status.Signal())
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitCode, nil
	}
	return exitCode, err
}
