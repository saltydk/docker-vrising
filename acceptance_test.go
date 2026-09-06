package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type liveInstallationFixture struct {
	config   Config
	store    *Store
	staged   StagedGeneration
	manifest ManagedManifest
}

func TestValidateLiveInstallationAcceptsCompleteSyntheticTree(t *testing.T) {
	fixture := newLiveInstallationFixture(t)

	if err := ValidateLiveInstallation(t.Context(), fixture.config); err != nil {
		t.Fatalf("ValidateLiveInstallation() error = %v", err)
	}
}

func TestVerifyCommandReportsValidatedSyntheticTree(t *testing.T) {
	fixture := newLiveInstallationFixture(t)
	var output bytes.Buffer

	if status := verifyCommand(&output, fixture.config); status != 0 {
		t.Fatalf("verifyCommand() status = %d, output %q", status, output.String())
	}
	if got, want := output.String(), "verified\n"; got != want {
		t.Fatalf("verifyCommand() output = %q, want %q", got, want)
	}
}

func TestValidateLiveInstallationRejectsBrokenRelationshipsAndManagedFiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *liveInstallationFixture)
	}{
		{
			name: "appmanifest build differs from state",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				writeLiveSteamManifest(t, fixture.config.ServerDir, "different-build", "depot-manifest", true)
			},
		},
		{
			name: "appmanifest required depot is missing",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				writeLiveSteamManifest(t, fixture.config.ServerDir, "steam-build", "", false)
			},
		},
		{
			name: "server executable is a symlink",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				executable := filepath.Join(fixture.config.ServerDir, "VRisingServer.exe")
				if err := os.Remove(executable); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("steam_appid.txt", executable); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "runtime app id has extra content",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				writeTestFile(t, filepath.Join(fixture.config.ServerDir, "steam_appid.txt"), "1604030\nextra\n")
			},
		},
		{
			name: "global package digest is not recomputed",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				lock := loadLivePackageLock(t, fixture)
				lock.Digest = strings.Repeat("f", 64)
				replaceLiveLockMetadata(t, fixture, lock)
			},
		},
		{
			name: "managed roots are not canonical",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				lock := loadLivePackageLock(t, fixture)
				lock.Roots[0], lock.Roots[1] = lock.Roots[1], lock.Roots[0]
				lock.Digest = PackageLockDigest(lock)
				replaceLiveLockMetadata(t, fixture, lock)
			},
		},
		{
			name: "managed packages are not in canonical order",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				lock := loadLivePackageLock(t, fixture)
				lock.Packages[0], lock.Packages[1] = lock.Packages[1], lock.Packages[0]
				lock.Digest = PackageLockDigest(lock)
				replaceLiveLockMetadata(t, fixture, lock)
			},
		},
		{
			name: "managed package identity is replaced",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				lock := loadLivePackageLock(t, fixture)
				lock.Packages[0].Ref.Name = "DifferentPack"
				lock.Packages[0].FullName = packageVersionFullName(lock.Packages[0].Ref)
				lock.Digest = PackageLockDigest(lock)
				replaceLiveLockMetadata(t, fixture, lock)
			},
		},
		{
			name: "dependency references an absent package",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				lock := loadLivePackageLock(t, fixture)
				lock.Packages[2].Dependencies = append(lock.Packages[2].Dependencies, PackageRef{
					Namespace: "missing", Name: "Dependency", Version: "1.0.0",
				})
				lock.Digest = PackageLockDigest(lock)
				replaceLiveLockMetadata(t, fixture, lock)
			},
		},
		{
			name: "dependency references are not canonically ordered",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				lock := loadLivePackageLock(t, fixture)
				slices.Reverse(lock.Packages[2].Dependencies)
				lock.Digest = PackageLockDigest(lock)
				replaceLiveLockMetadata(t, fixture, lock)
			},
		},
		{
			name: "active generation id is not the persisted generation",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				state := loadLiveState(t, fixture)
				state.Active.ID = "different-generation"
				saveLiveState(t, fixture, state)
			},
		},
		{
			name: "active generation status is not active",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				state := loadLiveState(t, fixture)
				state.Active.Status = "previous"
				saveLiveState(t, fixture, state)
			},
		},
		{
			name: "active lock digest differs from global lock",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				state := loadLiveState(t, fixture)
				state.Active.LockDigest = strings.Repeat("e", 64)
				saveLiveState(t, fixture, state)
			},
		},
		{
			name: "generation lock differs from global lock",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				lock := loadLivePackageLock(t, fixture)
				lock.ResolvedAt = lock.ResolvedAt.AddDate(0, 0, 1)
				writeLiveJSON(t, generationLockPath(fixture), lock)
			},
		},
		{
			name: "managed manifest generation id differs",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				manifest := loadLiveManifest(t, fixture)
				manifest.GenerationID = "different-generation"
				writeLiveJSON(t, generationManifestPath(fixture), manifest)
			},
		},
		{
			name: "managed manifest digest differs",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				manifest := loadLiveManifest(t, fixture)
				manifest.LockDigest = strings.Repeat("d", 64)
				writeLiveJSON(t, generationManifestPath(fixture), manifest)
			},
		},
		{
			name: "managed manifest path escapes the server root",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				manifest := loadLiveManifest(t, fixture)
				manifest.Files[0].RelativePath = "../escape.dll"
				writeLiveJSON(t, generationManifestPath(fixture), manifest)
			},
		},
		{
			name: "managed manifest hash differs",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				manifest := loadLiveManifest(t, fixture)
				manifest.Files[0].SHA256 = strings.Repeat("c", 64)
				writeLiveJSON(t, generationManifestPath(fixture), manifest)
			},
		},
		{
			name: "live managed file content differs",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				writeTestFile(t, liveManagedPath(fixture, fixture.manifest.Files[0]), "changed-live-bytes")
			},
		},
		{
			name: "live managed file mode differs",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				path := liveManagedPath(fixture, fixture.manifest.Files[0])
				want := fixture.manifest.Files[0].Mode.Perm() ^ 0o040
				if err := os.Chmod(path, want); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "live managed file is a symlink",
			mutate: func(t *testing.T, fixture *liveInstallationFixture) {
				path := liveManagedPath(fixture, fixture.manifest.Files[0])
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("steam_appid.txt", path); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLiveInstallationFixture(t)
			test.mutate(t, fixture)

			if test.name == "live managed file mode differs" && os.Geteuid() == 0 {
				if err := ValidateLiveInstallation(t.Context(), fixture.config); err != nil {
					t.Fatalf("root rejected managed-file permissions: %v", err)
				}
				return
			}
			if err := ValidateLiveInstallation(t.Context(), fixture.config); err == nil {
				t.Fatalf("ValidateLiveInstallation() accepted %s", test.name)
			}
		})
	}
}

func TestValidateLiveInstallationRequiresExactDirectDependencyTopology(t *testing.T) {
	tests := []struct {
		name       string
		packageRef PackageRef
		remove     PackageRef
		add        PackageRef
	}{
		{name: "VCF requires BepInEx", packageRef: vcfPackage, remove: bepInExPackage},
		{name: "Kindred requires BepInEx", packageRef: kindredPackage, remove: bepInExPackage},
		{name: "Kindred requires VCF", packageRef: kindredPackage, remove: vcfPackage},
		{name: "HookDOTS requires BepInEx", packageRef: hookDOTSPackage, remove: bepInExPackage},
		{name: "Satisvampory requires BepInEx", packageRef: satisvamporyPackage, remove: bepInExPackage},
		{name: "Satisvampory requires HookDOTS", packageRef: satisvamporyPackage, remove: hookDOTSPackage},
		{name: "Satisvampory requires VCF", packageRef: satisvamporyPackage, remove: vcfPackage},
		{name: "HookDOTS rejects reachable VCF edge", packageRef: hookDOTSPackage, add: vcfPackage},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLiveInstallationFixture(t)
			lock := loadLivePackageLock(t, fixture)
			for index := range lock.Packages {
				if lock.Packages[index].Ref != test.packageRef {
					continue
				}
				if test.remove != (PackageRef{}) {
					dependencyIndex := slices.Index(lock.Packages[index].Dependencies, test.remove)
					if dependencyIndex < 0 {
						t.Fatalf("fixture dependency %v is missing", test.remove)
					}
					lock.Packages[index].Dependencies = slices.Delete(lock.Packages[index].Dependencies, dependencyIndex, dependencyIndex+1)
				}
				if test.add != (PackageRef{}) {
					lock.Packages[index].Dependencies = append(lock.Packages[index].Dependencies, test.add)
					slices.SortFunc(lock.Packages[index].Dependencies, comparePackageRefs)
				}
				lock.Digest = PackageLockDigest(lock)
				replaceLiveLockMetadata(t, fixture, lock)

				if err := ValidateLiveInstallation(t.Context(), fixture.config); err == nil {
					t.Fatalf("ValidateLiveInstallation() accepted %s", test.name)
				}
				return
			}
			t.Fatalf("fixture package %v is missing", test.packageRef)
		})
	}
}

func newLiveInstallationFixture(t *testing.T) *liveInstallationFixture {
	t.Helper()
	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	dataDir := filepath.Join(root, "persistentdata")
	if err := os.MkdirAll(serverDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	store := &Store{StateDir: stateDir}
	manager := &ModManager{
		ServerDir:      serverDir,
		GenerationsDir: filepath.Join(stateDir, "generations"),
		Store:          store,
	}
	staged := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("live")})
	applyAndPromote(t, manager, staged)
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.SteamBuild = "steam-build"
	state.Runtime = RuntimeState{
		Phase:      "ready",
		Ready:      true,
		Generation: state.Active.ID,
		SteamBuild: state.SteamBuild,
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(serverDir, "VRisingServer.exe"), "synthetic executable")
	writeTestFile(t, filepath.Join(serverDir, "steam_appid.txt"), "1604030\n")
	writeLiveSteamManifest(t, serverDir, state.SteamBuild, "depot-manifest", true)

	fixture := &liveInstallationFixture{
		config: Config{
			ServerDir: serverDir,
			DataDir:   dataDir,
			StateDir:  stateDir,
		},
		store:    store,
		staged:   staged,
		manifest: staged.Manifest,
	}
	lock := loadLivePackageLock(t, fixture)
	for index := range lock.Packages {
		slices.SortFunc(lock.Packages[index].Dependencies, comparePackageRefs)
	}
	lock.Digest = PackageLockDigest(lock)
	replaceLiveLockMetadata(t, fixture, lock)
	fixture.manifest = loadLiveManifest(t, fixture)
	return fixture
}

func writeLiveSteamManifest(t *testing.T, serverDir, buildID, depotManifest string, includeDepot bool) {
	t.Helper()
	installedDepots := ""
	if includeDepot {
		installedDepots = `
	"InstalledDepots"
	{
		"1829351"
		{
			"manifest"		"` + depotManifest + `"
		}
	}`
	}
	manifest := `"AppState"
{
	"appid"		"1829350"
	"buildid"		"` + buildID + `"` + installedDepots + `
}
`
	writeTestFile(t, filepath.Join(serverDir, "steamapps", "appmanifest_1829350.acf"), manifest)
}

func loadLiveState(t *testing.T, fixture *liveInstallationFixture) State {
	t.Helper()
	state, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func saveLiveState(t *testing.T, fixture *liveInstallationFixture, state State) {
	t.Helper()
	if err := fixture.store.Save(state); err != nil {
		t.Fatal(err)
	}
}

func loadLivePackageLock(t *testing.T, fixture *liveInstallationFixture) PackageLock {
	t.Helper()
	lock, err := fixture.store.LoadPackageLock()
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

func replaceLiveLockMetadata(t *testing.T, fixture *liveInstallationFixture, lock PackageLock) {
	t.Helper()
	if err := fixture.store.SavePackageLock(lock); err != nil {
		t.Fatal(err)
	}
	state := loadLiveState(t, fixture)
	state.Active.LockDigest = lock.Digest
	saveLiveState(t, fixture, state)
	manifest := loadLiveManifest(t, fixture)
	manifest.LockDigest = lock.Digest
	writeLiveJSON(t, generationManifestPath(fixture), manifest)
	writeLiveJSON(t, generationLockPath(fixture), lock)
}

func loadLiveManifest(t *testing.T, fixture *liveInstallationFixture) ManagedManifest {
	t.Helper()
	data, err := os.ReadFile(generationManifestPath(fixture))
	if err != nil {
		t.Fatal(err)
	}
	var manifest ManagedManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func generationManifestPath(fixture *liveInstallationFixture) string {
	return filepath.Join(
		fixture.config.StateDir,
		"generations",
		fixture.staged.Record.ID,
		filepath.FromSlash(managedManifestName),
	)
}

func generationLockPath(fixture *liveInstallationFixture) string {
	return filepath.Join(
		fixture.config.StateDir,
		"generations",
		fixture.staged.Record.ID,
		filepath.FromSlash(managedLockName),
	)
}

func liveManagedPath(fixture *liveInstallationFixture, file ManagedFile) string {
	return filepath.Join(fixture.config.ServerDir, filepath.FromSlash(file.RelativePath))
}

func writeLiveJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, fs.FileMode(0o600)); err != nil {
		t.Fatal(err)
	}
}
