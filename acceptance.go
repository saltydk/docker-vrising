package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// ValidateLiveInstallation verifies the complete read-only identity chain used
// by live acceptance. It does not acquire the runtime lock or mutate state.
func ValidateLiveInstallation(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.ServerDir == "" || cfg.StateDir == "" {
		return fmt.Errorf("server and state directories are required")
	}

	store := &Store{StateDir: cfg.StateDir}
	state, err := store.Load()
	if err != nil {
		return fmt.Errorf("load live runtime state: %w", err)
	}
	if state.Active == nil {
		return fmt.Errorf("live runtime has no active generation")
	}
	if state.Active.ID == "" || state.Active.Status != "active" || state.Active.LockDigest == "" {
		return fmt.Errorf("live active generation identity is incomplete")
	}
	if state.Runtime.Phase != "ready" || !state.Runtime.Ready ||
		state.Runtime.Generation != state.Active.ID || state.Runtime.SteamBuild != state.SteamBuild {
		return fmt.Errorf("live runtime state does not match its active generation and Steam build")
	}
	if state.Candidate != nil || state.Transaction != nil || state.Promotion != nil {
		return fmt.Errorf("live runtime has an incomplete managed-file transaction")
	}

	steam := &SteamClient{ServerDir: cfg.ServerDir, Branch: cfg.Branch}
	installed, err := steam.InstalledBuild()
	if err != nil {
		return fmt.Errorf("validate live Steam appmanifest: %w", err)
	}
	if installed.BuildID != state.SteamBuild {
		return fmt.Errorf("live Steam build does not match recorded runtime state")
	}
	if _, err := steam.ValidateInstalled(installed); err != nil {
		return fmt.Errorf("validate live Steam installation: %w", err)
	}

	lock, err := store.LoadPackageLock()
	if err != nil {
		return fmt.Errorf("load live package lock: %w", err)
	}
	if err := validateLivePackageLock(lock); err != nil {
		return fmt.Errorf("validate live package lock: %w", err)
	}
	if state.Active.LockDigest != lock.Digest {
		return fmt.Errorf("live active generation does not match the global package lock")
	}

	manager := &ModManager{
		ServerDir:      cfg.ServerDir,
		GenerationsDir: filepath.Join(cfg.StateDir, "generations"),
		Store:          store,
	}
	active, err := manager.ValidateActive(ctx, *state.Active, lock)
	if err != nil {
		return fmt.Errorf("validate live active generation: %w", err)
	}
	for _, managed := range active.Manifest.Files {
		snapshot, exists, err := snapshotFileBelow(cfg.ServerDir, managed.RelativePath)
		if err != nil {
			return fmt.Errorf("open live managed file %s without following links: %w", managed.RelativePath, err)
		}
		if !exists {
			return fmt.Errorf("live managed file %s is missing", managed.RelativePath)
		}
		if snapshot.SHA256 != managed.SHA256 || snapshot.Mode.Perm() != managed.Mode.Perm() {
			return fmt.Errorf("live managed file %s does not match its manifest", managed.RelativePath)
		}
	}
	return nil
}

func validateLivePackageLock(lock PackageLock) error {
	if err := validateManagedPackageLock(lock); err != nil {
		return err
	}
	expectedDependencies := map[string][]PackageRef{
		"bepinex":      nil,
		"vcf":          {bepInExPackageRef(lock)},
		"kindred":      {bepInExPackageRef(lock), vcfPackageRef(lock)},
		"hookdots":     {bepInExPackageRef(lock)},
		"satisvampory": {bepInExPackageRef(lock), hookDOTSPackageRef(lock), vcfPackageRef(lock)},
	}
	for _, pkg := range lock.Packages {
		if !semanticVersion.MatchString(pkg.Ref.Version) {
			return fmt.Errorf("locked package %s version is not exact", pkg.FullName)
		}
		for index, dependency := range pkg.Dependencies {
			if !semanticVersion.MatchString(dependency.Version) {
				return fmt.Errorf("locked package %s dependency version is not exact", pkg.FullName)
			}
			if index > 0 && comparePackageRefs(pkg.Dependencies[index-1], dependency) >= 0 {
				return fmt.Errorf("locked package %s dependency references are not strictly canonical", pkg.FullName)
			}
		}
		kind := managedPackageKind(pkg.Ref)
		if !slices.Equal(pkg.Dependencies, expectedDependencies[kind]) {
			return fmt.Errorf("locked package %s does not have the exact required direct dependencies", pkg.FullName)
		}
	}
	return nil
}

func verifyCommand(output io.Writer, cfg Config) int {
	if cfg.ServerDir == "" && cfg.DataDir == "" && cfg.StateDir == "" {
		loaded, warnings, err := LoadConfig(EnvironmentMap(os.Environ()))
		for _, warning := range warnings {
			fmt.Fprintln(output, warning)
		}
		if err != nil {
			fmt.Fprintln(output, err)
			return exitPreflight
		}
		cfg = loaded
	}
	if err := ValidateLiveInstallation(context.Background(), cfg); err != nil {
		fmt.Fprintln(output, err)
		return exitPreflight
	}
	fmt.Fprintln(output, "verified")
	return 0
}
