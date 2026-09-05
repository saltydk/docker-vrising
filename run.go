package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

type packageGraphResolver interface {
	Resolve(context.Context, []RootSelection) (ResolvedGraph, error)
}

type archiveFetcher interface {
	Fetch(context.Context, ResolvedPackage, LockedPackage) (*ValidatedArchive, error)
}

type modLifecycle interface {
	ValidateActive(context.Context, GenerationRecord, PackageLock) (StagedGeneration, error)
	Stage(context.Context, PackageLock, map[PackageRef]*ValidatedArchive) (StagedGeneration, error)
	Apply(context.Context, StagedGeneration) error
	Rollback(context.Context) error
	CommitPromotion(context.Context, StagedGeneration) (PromotionCommitOutcome, error)
	RecoverPromotion(context.Context) (PromotionCommitOutcome, error)
	Discard(context.Context, StagedGeneration) error
}

type backupCreator interface {
	Create(context.Context, BackupRequest) (BackupRecord, error)
	Prune(int) error
}

type steamLifecycle interface {
	InstalledBuild() (SteamBuild, error)
	ValidateInstalled(SteamBuild) (SteamBuild, error)
	RemoteBuild(context.Context) (SteamBuild, error)
	Update(context.Context, SteamBuild) (SteamBuild, error)
}

type serverSupervisor interface {
	Run(context.Context, LaunchRequest) (RunResult, error)
}

type applicationStateStore interface {
	OpenLifetimeLock() (io.Closer, error)
	RecoverInterruptedTransaction() error
	Load() (State, error)
	LoadPackageLock() (PackageLock, error)
	Save(State) error
}

type Application struct {
	Config     Config
	Identity   RuntimeIdentity
	Store      *Store
	Resolver   packageGraphResolver
	Archives   archiveFetcher
	Mods       modLifecycle
	Backups    backupCreator
	Steam      steamLifecycle
	Supervisor serverSupervisor
	Proc       ProcInspector

	stateStore     applicationStateStore
	validateMounts func(Config) error
	pruneLogs      func(string, int, time.Time) error
	now            func() time.Time
}

type runError struct {
	Code int
	Err  error
}

func (e *runError) Error() string {
	return e.Err.Error()
}

func (e *runError) Unwrap() error {
	return e.Err
}

func newApplication(cfg Config, identity RuntimeIdentity) *Application {
	store := &Store{StateDir: cfg.StateDir}
	client := &http.Client{Timeout: 30 * time.Second}
	mods := &ModManager{
		ServerDir:      cfg.ServerDir,
		GenerationsDir: filepath.Join(cfg.StateDir, "generations"),
		Store:          store,
	}
	readiness := &ReadinessMonitor{
		BepInExLog: filepath.Join(cfg.ServerDir, "BepInEx", "LogOutput.log"),
		Output:     os.Stdout,
	}
	return &Application{
		Config:   cfg,
		Identity: identity,
		Store:    store,
		Resolver: &Thunderstore{
			Client: client,
		},
		Archives: &ArchiveCache{
			Dir:    filepath.Join(cfg.StateDir, "cache"),
			Client: client,
		},
		Mods: mods,
		Backups: &BackupManager{
			ServerDir: cfg.ServerDir,
			DataDir:   cfg.DataDir,
			BackupDir: filepath.Join(cfg.StateDir, "backups"),
		},
		Steam: &SteamClient{
			Runner:    execCommandRunner{},
			SteamCMD:  "steamcmd",
			ServerDir: cfg.ServerDir,
			HomeDir:   identity.Home,
			Branch:    cfg.Branch,
		},
		Supervisor: &Supervisor{
			Store:     store,
			Readiness: readiness,
			Processes: ExecProcessFactory{Stdout: os.Stdout, Stderr: os.Stderr},
		},
		Proc: procFSInspector{},
	}
}

func (a *Application) Run(ctx context.Context) (returnErr error) {
	if err := a.dependenciesReady(); err != nil {
		return exitFailure(exitPreflight, err)
	}
	validateMounts := a.validateMounts
	if validateMounts == nil {
		validateMounts = validateRunMounts
	}
	if err := validateMounts(a.Config); err != nil {
		return exitFailure(exitPreflight, fmt.Errorf("validate runtime mounts: %w", err))
	}

	store := a.applicationStore()
	lifetimeLock, err := store.OpenLifetimeLock()
	if err != nil {
		return exitFailure(exitPreflight, fmt.Errorf("acquire lifetime lock: %w", err))
	}
	defer func() {
		if err := lifetimeLock.Close(); err != nil {
			closeErr := exitFailure(exitPreflight, fmt.Errorf("release lifetime lock: %w", err))
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()

	if err := store.RecoverInterruptedTransaction(); err != nil {
		var pending *PendingPromotionError
		if !errors.As(err, &pending) || a.Mods == nil {
			return exitFailure(exitPreflight, fmt.Errorf("recover interrupted transaction: %w", err))
		}
		outcome, recoverErr := a.Mods.RecoverPromotion(ctx)
		if recoverErr != nil || outcome != PromotionCommitted {
			return exitFailure(exitPreflight, errors.Join(
				fmt.Errorf("recover pending promotion for generation %s", pending.GenerationID),
				recoverErr,
			))
		}
		if err := store.RecoverInterruptedTransaction(); err != nil {
			return exitFailure(exitPreflight, fmt.Errorf("resume transaction recovery after promotion: %w", err))
		}
	}
	state, err := store.Load()
	if errors.Is(err, os.ErrNotExist) {
		state = State{SchemaVersion: schemaVersion}
	} else if err != nil {
		return exitFailure(exitPreflight, fmt.Errorf("load runtime state: %w", err))
	}
	state, err = a.fencePriorProcess(store, state)
	if err != nil {
		return exitFailure(exitPreflight, err)
	}
	installed, installedErr := a.Steam.InstalledBuild()
	active := StagedGeneration{}
	activeErr := errors.New("active generation is unavailable")
	if a.Config.ModsEnabled {
		active, activeErr = a.loadActiveGeneration(ctx, store, state)
	}

	selected := active
	candidate := false
	if a.Config.ModsEnabled {
		if a.Config.UpdateMods {
			staged, err := a.stageCandidate(ctx, state, active)
			if err != nil {
				return a.fallbackOrFailure(ctx, store, state, installed, installedErr, active, exitModUpdate, err)
			}
			selected = staged
			candidate = true
		} else {
			if activeErr != nil {
				return exitFailure(exitModUpdate, fmt.Errorf("UPDATE_MODS=false requires a valid active generation: %w", activeErr))
			}
			if err := a.validateKnownGoodSteam(state, installed, installedErr); err != nil {
				return exitFailure(exitSteam, fmt.Errorf("UPDATE_MODS=false requires a valid recorded Steam runtime: %w", err))
			}
		}
	} else {
		selected = StagedGeneration{}
	}

	steamBuild, fallback, err := a.prepareSteam(ctx, store, state, installed, installedErr, active)
	if err != nil {
		return err
	}
	if fallback {
		if candidate {
			cleanupCtx, cancel := a.cleanupContext(ctx)
			discardErr := a.Mods.Discard(cleanupCtx, selected)
			cancel()
			if discardErr != nil {
				return exitFailure(exitModUpdate, fmt.Errorf("discard unreferenced staged generation: %w", discardErr))
			}
		}
		return a.launch(ctx, active, installed, false)
	}

	if a.Config.ModsEnabled {
		if err := a.Mods.Apply(ctx, selected); err != nil {
			if candidate {
				return a.rollbackCandidate(ctx, fmt.Errorf("apply candidate mods: %w", err))
			}
			return exitFailure(exitModUpdate, fmt.Errorf("reapply active mods: %w", err))
		}
	}

	return a.launch(ctx, selected, steamBuild, candidate)
}

func (a *Application) fencePriorProcess(store applicationStateStore, state State) (State, error) {
	recorded := state.Runtime.Server
	if recorded == (ProcessIdentity{}) {
		if state.Runtime.Ready {
			return State{}, errors.New("recorded ready runtime has no process identity")
		}
		return state, nil
	}
	if recorded.PID <= 0 || recorded.StartTicks == 0 {
		return State{}, errors.New("recorded runtime process identity is incomplete")
	}
	if a.Proc == nil {
		return State{}, errors.New("process inspector is unavailable")
	}
	live, err := a.Proc.Identity(recorded.PID)
	if err == nil {
		if live.PID != recorded.PID || live.StartTicks == 0 {
			return State{}, fmt.Errorf("process inspector returned an invalid identity for PID %d", recorded.PID)
		}
		if recorded.Matches(live) {
			return State{}, fmt.Errorf("recorded server process %d with start ticks %d is still running", recorded.PID, recorded.StartTicks)
		}
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return State{}, fmt.Errorf("inspect recorded server process: %w", err)
	}

	state.Runtime.Server = ProcessIdentity{}
	state.Runtime.Ready = false
	state.Runtime.Phase = "stopped"
	state.Runtime.UpdatedAt = a.clock()()
	if err := store.Save(state); err != nil {
		return State{}, fmt.Errorf("clear stale server process identity: %w", err)
	}
	return state, nil
}

func (a *Application) dependenciesReady() error {
	if a.applicationStore() == nil {
		return errors.New("state store is required")
	}
	if a.Steam == nil || a.Supervisor == nil {
		return errors.New("Steam and supervisor dependencies are required")
	}
	if a.Config.UpdateGame && a.Backups == nil {
		return errors.New("backup lifecycle is required")
	}
	if !a.Config.ModsEnabled {
		return nil
	}
	if a.Mods == nil {
		return errors.New("mod lifecycle is required")
	}
	if a.Config.UpdateMods && (a.Resolver == nil || a.Archives == nil) {
		return errors.New("mod resolver and archive cache are required")
	}
	return nil
}

func (a *Application) applicationStore() applicationStateStore {
	if a.stateStore != nil {
		return a.stateStore
	}
	if a.Store == nil {
		return nil
	}
	return a.Store
}

func (a *Application) loadActiveGeneration(ctx context.Context, store applicationStateStore, state State) (StagedGeneration, error) {
	if state.Active == nil {
		return StagedGeneration{}, errors.New("active generation record is missing")
	}
	lock, err := store.LoadPackageLock()
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("load active package lock: %w", err)
	}
	active, err := a.Mods.ValidateActive(ctx, *state.Active, lock)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("validate active generation: %w", err)
	}
	return active, nil
}

func (a *Application) stageCandidate(ctx context.Context, state State, active StagedGeneration) (StagedGeneration, error) {
	graph, err := a.Resolver.Resolve(ctx, []RootSelection{
		{Namespace: "odjit", Name: "KindredCommands", Version: a.Config.KindredVersion},
		{Namespace: "Team_GreenEye", Name: "Satisvampory", Version: a.Config.SatisvamporyVersion},
	})
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("resolve mod package graph: %w", err)
	}
	if err := validateManagedRootRefs(graph.Roots); err != nil {
		return StagedGeneration{}, fmt.Errorf("validate resolved roots: %w", err)
	}

	prior := make(map[PackageRef]LockedPackage, len(active.Lock.Packages))
	for _, pkg := range active.Lock.Packages {
		prior[pkg.Ref] = pkg
	}
	archives := make(map[PackageRef]*ValidatedArchive, len(graph.Packages))
	for _, pkg := range graph.Packages {
		if _, duplicate := archives[pkg.Ref]; duplicate {
			return StagedGeneration{}, errors.Join(
				fmt.Errorf("resolved package graph contains duplicate %s/%s/%s", pkg.Ref.Namespace, pkg.Ref.Name, pkg.Ref.Version),
				closeValidatedArchives(archives),
			)
		}
		archive, err := a.Archives.Fetch(ctx, pkg, prior[pkg.Ref])
		if err != nil {
			return StagedGeneration{}, errors.Join(
				fmt.Errorf("fetch mod archive %s: %w", pkg.FullName, err),
				closeValidatedArchives(archives),
			)
		}
		archives[pkg.Ref] = archive
	}

	lock := PackageLock{
		SchemaVersion: schemaVersion,
		Roots:         slices.Clone(graph.Roots),
		Packages:      make([]LockedPackage, 0, len(graph.Packages)),
		ResolvedAt:    a.clock()(),
	}
	for _, pkg := range graph.Packages {
		lock.Packages = append(lock.Packages, archives[pkg.Ref].LockedPackage())
	}
	lock.Digest = PackageLockDigest(lock)
	if state.Failed != nil && state.Failed.LockDigest == lock.Digest {
		return StagedGeneration{}, errors.Join(
			fmt.Errorf("package lock %s was previously marked failed", lock.Digest),
			closeValidatedArchives(archives),
		)
	}

	staged, stageErr := a.Mods.Stage(ctx, lock, archives)
	closeErr := closeValidatedArchives(archives)
	if stageErr != nil || closeErr != nil {
		return StagedGeneration{}, errors.Join(stageErr, closeErr)
	}
	if staged.Record.ID == "" {
		return StagedGeneration{}, errors.New("stage candidate returned an empty generation ID")
	}
	staged.Record.LockDigest = lock.Digest
	staged.Lock = lock
	return staged, nil
}

func (a *Application) prepareSteam(
	ctx context.Context,
	store applicationStateStore,
	state State,
	installed SteamBuild,
	installedErr error,
	active StagedGeneration,
) (SteamBuild, bool, error) {
	if !a.Config.UpdateGame {
		if installedErr != nil {
			return SteamBuild{}, false, exitFailure(exitSteam, fmt.Errorf("validate installed Steam build: %w", installedErr))
		}
		validated, err := a.Steam.ValidateInstalled(installed)
		if err != nil {
			return SteamBuild{}, false, exitFailure(exitSteam, err)
		}
		return validated, false, nil
	}

	target, err := a.Steam.RemoteBuild(ctx)
	if err != nil {
		cause := fmt.Errorf("query remote Steam build: %w", err)
		fallbackErr := a.recordFallback(store, state, installed, installedErr, active, cause)
		if fallbackErr != nil {
			return SteamBuild{}, false, exitFailure(exitSteam, errors.Join(cause, fallbackErr))
		}
		return installed, true, nil
	}
	if installedErr == nil && installed == target {
		validated, err := a.Steam.ValidateInstalled(installed)
		if err != nil {
			return SteamBuild{}, false, exitFailure(exitSteam, err)
		}
		return validated, false, nil
	}

	if installedErr == nil && installed.BuildID != "" {
		_, err := a.Backups.Create(ctx, BackupRequest{
			InstalledBuild: installed.BuildID,
			TargetBuild:    target.BuildID,
			PackageDigest:  active.Lock.Digest,
		})
		if err == nil {
			err = a.Backups.Prune(a.Config.BackupRetention)
		}
		if err != nil {
			cause := fmt.Errorf("back up current server: %w", err)
			fallbackErr := a.recordFallback(store, state, installed, installedErr, active, cause)
			if fallbackErr != nil {
				return SteamBuild{}, false, exitFailure(exitBackup, errors.Join(cause, fallbackErr))
			}
			return installed, true, nil
		}
	}

	updated, err := a.Steam.Update(ctx, target)
	if err == nil {
		return updated, false, nil
	}
	var preMutation *SteamPreMutationError
	if errors.As(err, &preMutation) {
		cause := fmt.Errorf("update Steam before mutation: %w", err)
		fallbackErr := a.recordFallback(store, state, installed, installedErr, active, cause)
		if fallbackErr != nil {
			return SteamBuild{}, false, exitFailure(exitSteam, errors.Join(cause, fallbackErr))
		}
		return installed, true, nil
	}
	return SteamBuild{}, false, exitFailure(exitSteam, fmt.Errorf("update and validate Steam: %w", err))
}

func (a *Application) fallbackOrFailure(
	ctx context.Context,
	store applicationStateStore,
	state State,
	installed SteamBuild,
	installedErr error,
	active StagedGeneration,
	code int,
	cause error,
) error {
	if err := a.recordFallback(store, state, installed, installedErr, active, cause); err != nil {
		return exitFailure(code, errors.Join(cause, err))
	}
	return a.launch(ctx, active, installed, false)
}

func (a *Application) recordFallback(
	store applicationStateStore,
	state State,
	installed SteamBuild,
	installedErr error,
	active StagedGeneration,
	cause error,
) error {
	if active.Record.ID == "" {
		return errors.New("known-good active generation is unavailable")
	}
	if err := a.validateKnownGoodSteam(state, installed, installedErr); err != nil {
		return err
	}
	state.Runtime = RuntimeState{
		Phase:      "degraded",
		Degraded:   true,
		Reason:     cause.Error(),
		Generation: active.Record.ID,
		SteamBuild: installed.BuildID,
		UpdatedAt:  a.clock()(),
	}
	if err := store.Save(state); err != nil {
		return fmt.Errorf("record degraded runtime state: %w", err)
	}
	return nil
}

func (a *Application) validateKnownGoodSteam(state State, installed SteamBuild, installedErr error) error {
	if installedErr != nil || installed.BuildID == "" {
		return errors.New("known-good executable is unavailable")
	}
	if state.SteamBuild == "" || state.SteamBuild != installed.BuildID {
		return fmt.Errorf("installed Steam build %q does not match recorded known-good build %q", installed.BuildID, state.SteamBuild)
	}
	validated, err := a.Steam.ValidateInstalled(installed)
	if err != nil {
		return fmt.Errorf("validate known-good Steam runtime: %w", err)
	}
	if validated != installed {
		return fmt.Errorf("validated Steam runtime %#v does not match installed build %#v", validated, installed)
	}
	return nil
}

func (a *Application) launch(ctx context.Context, selected StagedGeneration, steam SteamBuild, candidate bool) error {
	pruneLogs := a.pruneLogs
	if pruneLogs == nil {
		pruneLogs = PruneLogs
	}
	if err := pruneLogs(filepath.Join(a.Config.DataDir, "logs"), a.Config.LogDays, a.clock()()); err != nil {
		if candidate {
			return a.rollbackCandidate(ctx, fmt.Errorf("prune server logs: %w", err))
		}
		return exitFailure(exitPreflight, fmt.Errorf("prune server logs: %w", err))
	}

	promotionOutcome := PromotionNotCommitted
	var onReady func(context.Context) error
	if candidate {
		onReady = func(readyCtx context.Context) error {
			outcome, err := a.Mods.CommitPromotion(readyCtx, selected)
			promotionOutcome = outcome
			return err
		}
	}
	result, err := a.Supervisor.Run(ctx, LaunchRequest{
		Config:      a.Config,
		Identity:    a.Identity,
		PackageLock: selected.Lock,
		Generation:  selected.Record.ID,
		SteamBuild:  steam.BuildID,
		OnReady:     onReady,
	})
	if !result.Ready {
		readinessErr := errors.Join(errors.New("server did not become ready"), err)
		if candidate {
			if promotionOutcome == PromotionRecoveryRequired || promotionOutcome == PromotionCommitted {
				return exitFailure(exitReadiness, readinessErr)
			}
			return a.rollbackCandidate(ctx, readinessErr)
		}
		return exitFailure(exitReadiness, readinessErr)
	}

	if candidate && promotionOutcome != PromotionCommitted {
		return exitFailure(exitReadiness, errors.New("server became ready without a committed candidate promotion"))
	}
	if err != nil {
		return exitFailure(exitShutdown, fmt.Errorf("supervise server shutdown: %w", err))
	}
	if result.ExitCode != 0 {
		return exitFailure(result.ExitCode, fmt.Errorf("server exited with status %d", result.ExitCode))
	}
	return nil
}

func (a *Application) rollbackCandidate(ctx context.Context, cause error) error {
	cleanupCtx, cancel := a.cleanupContext(ctx)
	defer cancel()
	err := a.Mods.Rollback(cleanupCtx)
	if err != nil {
		cause = errors.Join(cause, fmt.Errorf("rollback failed candidate: %w", err))
	}
	return exitFailure(exitReadiness, cause)
}

func (a *Application) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := a.Config.ShutdownTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func (a *Application) clock() func() time.Time {
	if a.now != nil {
		return a.now
	}
	return time.Now
}

func closeValidatedArchives(archives map[PackageRef]*ValidatedArchive) error {
	errs := make([]error, 0, len(archives))
	for _, archive := range archives {
		if archive == nil {
			continue
		}
		if err := archive.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateRunMounts(cfg Config) error {
	server, _, err := openMountRoot(cfg.ServerDir, "server")
	if err != nil {
		return err
	}
	defer unix.Close(server)
	data, _, err := openMountRoot(cfg.DataDir, "data")
	if err != nil {
		return err
	}
	return unix.Close(data)
}

func exitFailure(code int, err error) error {
	if err == nil {
		return nil
	}
	return &runError{Code: code, Err: err}
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, spec CommandSpec) (CommandResult, error) {
	command := exec.CommandContext(ctx, spec.Path, spec.Args...)
	command.Env = slices.Clone(spec.Env)
	command.Dir = spec.Dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}
