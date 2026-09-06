package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	testRuntimeUID = 42420
	testRuntimeGID = 42421
)

func TestResolveIdentityRetainsContainerIdentityByDefault(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	chownTestPath(t, cfg.ServerDir, testRuntimeUID, testRuntimeGID)
	chownTestPath(t, cfg.DataDir, testRuntimeUID, testRuntimeGID)

	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UID != os.Geteuid() || identity.GID != os.Getegid() {
		t.Fatalf("identity = %d:%d, want container identity %d:%d", identity.UID, identity.GID, os.Geteuid(), os.Getegid())
	}
	if identity.Home != filepath.Join(cfg.StateDir, "home") {
		t.Fatalf("Home = %q, want StateDir/home", identity.Home)
	}
}

func TestResolveIdentityRequiresPersistentMountWritable(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	chownTestPath(t, cfg.ServerDir, testRuntimeUID, testRuntimeGID)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)

	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result := runIdentityHelper(t, cfg, identity)
	if result.ProbeError == "" || !strings.Contains(result.ProbeError, "persistent-data mount") {
		t.Fatalf("probe error = %q, want persistent-data mount writability failure", result.ProbeError)
	}
	assertDroppedIdentity(t, result, identity)
	assertNoIdentityProbes(t, cfg.ServerDir)
	assertNoIdentityProbes(t, cfg.DataDir)
}

func TestDefaultRootAdoptsPrivatePrefixWithoutChowningGameData(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	gameFile := filepath.Join(cfg.ServerDir, "operator-file")
	prefix := filepath.Join(cfg.StateDir, "wineprefix")
	writeTestFile(t, gameFile, "preserve")
	if err := os.MkdirAll(prefix, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.ServerDir, cfg.DataDir, cfg.StateDir, prefix, gameFile} {
		chownTestPath(t, path, testRuntimeUID, testRuntimeGID)
	}
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, prefix, 0, 0)
	assertPathOwner(t, cfg.ServerDir, testRuntimeUID, testRuntimeGID)
	assertPathOwner(t, cfg.DataDir, testRuntimeUID, testRuntimeGID)
	assertPathOwner(t, gameFile, testRuntimeUID, testRuntimeGID)
}

func TestResolveIdentityUsesExplicitPUIDAndPGID(t *testing.T) {
	cfg := newIdentityTestConfig(t)
	cfg.PUID = intPointer(testRuntimeUID)
	cfg.PGID = intPointer(testRuntimeGID)

	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UID != testRuntimeUID || identity.GID != testRuntimeGID {
		t.Fatalf("identity = %d:%d, want explicit %d:%d", identity.UID, identity.GID, testRuntimeUID, testRuntimeGID)
	}
}

func TestPrepareOwnershipRejectsIDsOutsideKernelRangeBeforeMutation(t *testing.T) {
	requireRoot(t)
	maximum := int(uint64(1<<32 - 2))
	valid := newIdentityTestConfig(t)
	valid.PUID, valid.PGID = &maximum, &maximum
	identity, err := ResolveIdentity(valid)
	if err != nil {
		t.Fatalf("ResolveIdentity rejected maximum valid IDs: %v", err)
	}
	if identity.UID != maximum || identity.GID != maximum {
		t.Fatalf("maximum identity = %d:%d, want %d:%d", identity.UID, identity.GID, maximum, maximum)
	}
	for _, raw := range []uint64{1<<32 - 1, 1 << 32, 1<<32 + 1} {
		t.Run(strconv.FormatUint(raw, 10), func(t *testing.T) {
			cfg := newIdentityTestConfig(t)
			nested := filepath.Join(cfg.ServerDir, "unchanged.txt")
			writeIdentityTestFile(t, nested)
			uid := int(raw)
			cfg.PUID, cfg.PGID = &uid, intPointer(testRuntimeGID)
			identity := RuntimeIdentity{UID: uid, GID: testRuntimeGID, Home: filepath.Join(cfg.StateDir, "home")}

			if _, err := ResolveIdentity(cfg); err == nil {
				t.Error("ResolveIdentity accepted an out-of-range explicit UID")
			}
			if err := PrepareOwnership(cfg, identity); err == nil {
				t.Error("PrepareOwnership accepted an out-of-range explicit UID")
			}
			assertPathOwner(t, nested, 0, 0)
			if _, err := os.Lstat(cfg.StateDir); !os.IsNotExist(err) {
				t.Fatalf("StateDir mutation error = %v, want not exist", err)
			}
		})
	}
}

func TestValidateConfiguredIdentityRejectsKernelSentinel(t *testing.T) {
	sentinel := ^uint32(0)
	valid := RuntimeIdentity{UID: os.Geteuid(), GID: os.Getegid(), Home: "/state/home"}
	if _, err := validateConfiguredIdentity(Config{StateDir: "/state"}, valid); err != nil {
		t.Fatalf("validateConfiguredIdentity rejected container identity: %v", err)
	}
	identity := RuntimeIdentity{UID: int(uint64(sentinel)), GID: int(uint64(sentinel)), Home: "/state/home"}
	if _, err := validateConfiguredIdentity(Config{StateDir: "/state"}, identity); err == nil {
		t.Fatal("validateConfiguredIdentity accepted the kernel no-change sentinel")
	}
}

func TestResolveIdentityRejectsUnsafeMountRoots(t *testing.T) {
	base := identityTestBase(t)
	directory := filepath.Join(base, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(base, "symlink")
	if err := os.Symlink(directory, symlink); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(base, "regular")
	if err := os.WriteFile(regular, []byte("not a mount"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "server symlink", cfg: Config{ServerDir: symlink, DataDir: directory, StateDir: filepath.Join(symlink, ".docker-vrising")}},
		{name: "persistent symlink", cfg: Config{ServerDir: directory, DataDir: symlink, StateDir: filepath.Join(directory, ".docker-vrising")}},
		{name: "server regular file", cfg: Config{ServerDir: regular, DataDir: directory, StateDir: filepath.Join(regular, ".docker-vrising")}},
		{name: "persistent regular file", cfg: Config{ServerDir: directory, DataDir: regular, StateDir: filepath.Join(directory, ".docker-vrising")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ResolveIdentity(tt.cfg); err == nil {
				t.Fatal("ResolveIdentity accepted an unsafe mount root")
			}
		})
	}
}

func TestResolveIdentityReportsContainerIdentity(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	var output bytes.Buffer
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UID != 0 || identity.GID != 0 {
		t.Fatalf("identity = %d:%d, want retained root", identity.UID, identity.GID)
	}
	if got := strings.Count(output.String(), "retaining container UID/GID 0:0"); got != 1 {
		t.Fatalf("runtime identity notice count = %d, output %q", got, output.String())
	}
}

func TestPrepareOwnershipRunsOnlyWhenIdentityChanges(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	nested := filepath.Join(cfg.DataDir, "nested.txt")
	writeIdentityTestFile(t, nested)
	cfg.PUID = intPointer(testRuntimeUID)
	cfg.PGID = intPointer(testRuntimeGID)
	first, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, first); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, nested, testRuntimeUID, testRuntimeGID)

	chownTestPath(t, nested, 0, 0)
	if err := PrepareOwnership(cfg, first); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, nested, 0, 0)

	secondUID, secondGID := testRuntimeUID+1, testRuntimeGID+1
	cfg.PUID, cfg.PGID = intPointer(secondUID), intPointer(secondGID)
	second, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, second); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, nested, secondUID, secondGID)
}

func TestPrepareOwnershipLogsExplicitMigrationOnlyWhenRequired(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(output.String(), "ownership migration"); got != 1 {
		t.Fatalf("ownership migration log count = %d, output %q", got, output.String())
	}
}

func TestPrepareOwnershipUsesLchownForSymlinks(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	external := filepath.Join(identityTestBase(t), "external.txt")
	writeIdentityTestFile(t, external)
	link := filepath.Join(cfg.ServerDir, "external-link")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}

	assertPathOwner(t, link, testRuntimeUID, testRuntimeGID)
	assertPathOwner(t, external, 0, 0)
}

func TestPrepareOwnershipRecordsCompletedMigration(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	if err := os.Mkdir(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"generations", "cache", "backups"} {
		if err := os.Mkdir(filepath.Join(cfg.StateDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}

	for _, directory := range []string{
		cfg.StateDir,
		identity.Home,
		filepath.Join(cfg.StateDir, "wineprefix"),
		filepath.Join(cfg.StateDir, "generations"),
		filepath.Join(cfg.StateDir, "cache"),
		filepath.Join(cfg.StateDir, "backups"),
	} {
		assertPrivateDirectory(t, directory, identity)
	}
	markerPath := filepath.Join(cfg.StateDir, "ownership.json")
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		UID int `json:"uid"`
		GID int `json:"gid"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.UID != identity.UID || marker.GID != identity.GID {
		t.Fatalf("marker = %d:%d, want %d:%d", marker.UID, marker.GID, identity.UID, identity.GID)
	}
	assertPathOwnerAndMode(t, markerPath, identity.UID, identity.GID, 0o600)
	assertNoOwnershipTemporaries(t, cfg.StateDir)
}

func TestPrepareOwnershipRetriesMissingOrIncompleteMarker(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	nested := filepath.Join(cfg.DataDir, "nested.txt")
	writeIdentityTestFile(t, nested)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}

	markerPath := filepath.Join(cfg.StateDir, "ownership.json")
	for _, rewrite := range []struct {
		name string
		do   func()
	}{
		{name: "missing", do: func() {
			if err := os.Remove(markerPath); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "incomplete", do: func() {
			if err := os.WriteFile(markerPath, []byte(fmt.Sprintf(`{"uid":%d}`, identity.UID)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(rewrite.name, func(t *testing.T) {
			chownTestPath(t, nested, 0, 0)
			rewrite.do()
			if err := PrepareOwnership(cfg, identity); err != nil {
				t.Fatal(err)
			}
			assertPathOwner(t, nested, identity.UID, identity.GID)
		})
	}
}

func TestPrepareOwnershipRejectsMarkerOutsidePrivateStateDirectory(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	nested := filepath.Join(cfg.DataDir, "nested.txt")
	writeIdentityTestFile(t, nested)
	if err := os.Mkdir(cfg.StateDir, 0o777); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(cfg.StateDir, "ownership.json")
	marker := []byte(fmt.Sprintf(`{"uid":%d,"gid":%d}`, testRuntimeUID, testRuntimeGID))
	if err := os.WriteFile(markerPath, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	chownTestPath(t, markerPath, testRuntimeUID, testRuntimeGID)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, nested, identity.UID, identity.GID)
	assertPrivateDirectory(t, cfg.StateDir, identity)
}

func TestPrepareOwnershipInvalidatesMarkerBeforePersistentFailure(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	if err := os.Mkdir(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(cfg.StateDir, ownershipMarkerName)
	marker := []byte(fmt.Sprintf(`{"uid":%d,"gid":%d}`, testRuntimeUID, testRuntimeGID))
	if err := os.WriteFile(markerPath, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	chownTestPath(t, markerPath, testRuntimeUID, testRuntimeGID)
	nested := filepath.Join(cfg.DataDir, "persistent.txt")
	writeIdentityTestFile(t, nested)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected persistent-data walk failure")
	hooks := ownershipHooks{afterEntryInspect: func(mount string, _ int, name string, _ unix.Stat_t) error {
		if mount == "persistent-data" && name == "persistent.txt" {
			return injected
		}
		return nil
	}}

	if err := prepareOwnership(cfg, identity, hooks); !errors.Is(err, injected) {
		t.Fatalf("prepareOwnership() error = %v, want injected failure", err)
	}
	if _, err := os.Lstat(markerPath); !os.IsNotExist(err) {
		t.Errorf("ownership marker after partial migration error = %v, want not exist", err)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, nested, identity.UID, identity.GID)
}

func TestPrepareOwnershipRejectsRegularReplacedByDirectory(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	entry := filepath.Join(cfg.ServerDir, "raced-entry")
	writeIdentityTestFile(t, entry)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	replaced := false
	hooks := ownershipHooks{afterEntryInspect: func(mount string, _ int, name string, _ unix.Stat_t) error {
		if mount != "server" || name != "raced-entry" || replaced {
			return nil
		}
		replaced = true
		if err := os.Rename(entry, filepath.Join(cfg.ServerDir, "held-regular")); err != nil {
			return err
		}
		return os.Mkdir(entry, 0o700)
	}}

	if err := prepareOwnership(cfg, identity, hooks); err == nil {
		t.Fatal("prepareOwnership accepted a regular entry replaced by a directory")
	}
	assertOwnershipMarkerAbsent(t, cfg.StateDir)
}

func TestPrepareOwnershipRejectsDirectoryReplacementAfterOpen(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	entry := filepath.Join(cfg.ServerDir, "raced-directory")
	if err := os.Mkdir(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	writeIdentityTestFile(t, filepath.Join(entry, "original.txt"))
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	replaced := false
	hooks := ownershipHooks{afterEntryOpen: func(mount string, _ int, name string, _ int, _ unix.Stat_t) error {
		if mount != "server" || name != "raced-directory" || replaced {
			return nil
		}
		replaced = true
		if err := os.Rename(entry, filepath.Join(cfg.ServerDir, "held-directory")); err != nil {
			return err
		}
		if err := os.Mkdir(entry, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(entry, "replacement.txt"), []byte("replacement"), 0o600)
	}}

	if err := prepareOwnership(cfg, identity, hooks); err == nil {
		t.Fatal("prepareOwnership accepted a replaced directory after opening it")
	}
	assertOwnershipMarkerAbsent(t, cfg.StateDir)
}

func TestPrepareOwnershipRejectsStateDirectoryReplacementDuringPublication(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	displaced := cfg.StateDir + ".displaced"
	hooks := ownershipHooks{beforeMarkerRename: func() error {
		if err := os.Rename(cfg.StateDir, displaced); err != nil {
			return err
		}
		if err := os.Mkdir(cfg.StateDir, 0o700); err != nil {
			return err
		}
		return os.Chown(cfg.StateDir, identity.UID, identity.GID)
	}}

	if err := prepareOwnership(cfg, identity, hooks); err == nil {
		t.Fatal("prepareOwnership accepted StateDir replacement during marker publication")
	}
	assertOwnershipMarkerAbsent(t, cfg.StateDir)
}

func TestPrepareOwnershipReservesHeldStateDirectoryDuringServerWalk(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	if err := os.Mkdir(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	initialMarker := filepath.Join(cfg.StateDir, ownershipMarkerName)
	writeMatchingOwnershipMarker(t, initialMarker, testRuntimeUID, testRuntimeGID)
	trigger := filepath.Join(cfg.ServerDir, "!state-swap-trigger")
	writeIdentityTestFile(t, trigger)
	persistent := filepath.Join(cfg.DataDir, "persistent.txt")
	writeIdentityTestFile(t, persistent)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	displaced := cfg.StateDir + ".held"
	replacementContent := filepath.Join(cfg.StateDir, "replacement.txt")
	replaced := false
	injected := errors.New("injected persistent-data walk failure")
	hooks := ownershipHooks{afterEntryInspect: func(mount string, _ int, name string, _ unix.Stat_t) error {
		if mount == "server" && name == "!state-swap-trigger" && !replaced {
			replaced = true
			if err := os.Rename(cfg.StateDir, displaced); err != nil {
				return err
			}
			if err := os.Mkdir(cfg.StateDir, 0o700); err != nil {
				return err
			}
			writeMatchingOwnershipMarker(t, filepath.Join(cfg.StateDir, ownershipMarkerName), identity.UID, identity.GID)
			return os.WriteFile(replacementContent, []byte("replacement"), 0o600)
		}
		if mount == "persistent-data" && name == "persistent.txt" {
			return injected
		}
		return nil
	}}

	if err := prepareOwnership(cfg, identity, hooks); err == nil {
		t.Fatal("prepareOwnership accepted replacement StateDir during server migration")
	}
	if uid, gid := identityTestPathOwner(t, replacementContent); uid != 0 || gid != 0 {
		t.Errorf("replacement content owner = %d:%d, want untouched 0:0", uid, gid)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, persistent, identity.UID, identity.GID)
}

func TestPrepareOwnershipRejectsInsertionAfterDirectorySnapshot(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	trigger := filepath.Join(cfg.ServerDir, "!insert-trigger")
	writeIdentityTestFile(t, trigger)
	inserted := filepath.Join(cfg.ServerDir, "inserted.txt")
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	insertedOnce := false
	hooks := ownershipHooks{afterEntryInspect: func(mount string, _ int, name string, _ unix.Stat_t) error {
		if mount != "server" || name != "!insert-trigger" || insertedOnce {
			return nil
		}
		insertedOnce = true
		return os.WriteFile(inserted, []byte("late sibling"), 0o600)
	}}

	if err := prepareOwnership(cfg, identity, hooks); err == nil {
		t.Fatal("prepareOwnership accepted a sibling inserted after the directory snapshot")
	}
	assertOwnershipMarkerAbsent(t, cfg.StateDir)
}

func TestPrepareOwnershipVerifiesCompletedSubtreesBeforePublication(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	earlier := filepath.Join(cfg.ServerDir, "a")
	if err := os.Mkdir(earlier, 0o700); err != nil {
		t.Fatal(err)
	}
	later := filepath.Join(cfg.ServerDir, "z")
	writeIdentityTestFile(t, later)
	late := filepath.Join(earlier, "late.txt")
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inserted := false
	hooks := ownershipHooks{afterEntryInspect: func(mount string, _ int, name string, _ unix.Stat_t) error {
		if mount != "server" || name != "z" || inserted {
			return nil
		}
		inserted = true
		return os.WriteFile(late, []byte("late subtree member"), 0o600)
	}}

	if err := prepareOwnership(cfg, identity, hooks); err == nil {
		t.Fatal("prepareOwnership accepted a late insertion into a completed subtree")
	}
	assertOwnershipMarkerAbsent(t, cfg.StateDir)
	assertPathOwner(t, late, 0, 0)
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, late, identity.UID, identity.GID)
}

func TestPrepareOwnershipDoesNotPublishAfterFilesystemSyncFailure(t *testing.T) {
	requireRoot(t)
	for _, targetMount := range []string{"server", "persistent-data"} {
		t.Run(targetMount, func(t *testing.T) {
			cfg := newIdentityTestConfig(t)
			external := filepath.Join(identityTestBase(t), "external.txt")
			writeIdentityTestFile(t, external)
			mountPath := cfg.ServerDir
			if targetMount == "persistent-data" {
				mountPath = cfg.DataDir
			}
			link := filepath.Join(mountPath, "barrier-link")
			if err := os.Symlink(external, link); err != nil {
				t.Fatal(err)
			}
			cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
			identity, err := ResolveIdentity(cfg)
			if err != nil {
				t.Fatal(err)
			}
			injected := fmt.Errorf("injected %s syncfs failure", targetMount)
			hooks := ownershipHooks{syncFilesystem: func(mount string, fd int) error {
				if mount == targetMount {
					return injected
				}
				return unix.Syncfs(fd)
			}}

			if err := prepareOwnership(cfg, identity, hooks); !errors.Is(err, injected) {
				t.Fatalf("prepareOwnership() error = %v, want injected syncfs failure", err)
			}
			assertOwnershipMarkerAbsent(t, cfg.StateDir)
			assertPathOwner(t, link, identity.UID, identity.GID)
			assertPathOwner(t, external, 0, 0)
			chownTestPath(t, link, 0, 0)
			if err := PrepareOwnership(cfg, identity); err != nil {
				t.Fatal(err)
			}
			assertPathOwner(t, link, identity.UID, identity.GID)
		})
	}
}

func TestPrepareOwnershipHoldsBarrierDescriptorsFromBeforeFirstChownThroughPublication(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	writeIdentityTestFile(t, filepath.Join(cfg.ServerDir, "server.txt"))
	writeIdentityTestFile(t, filepath.Join(cfg.DataDir, "persistent.txt"))
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	type barrierIdentity struct {
		fd       int
		dev, ino uint64
		synced   bool
	}
	barriers := make(map[string]barrierIdentity)
	publicationObserved := false
	hooks := ownershipHooks{
		barrierOpened: func(mount string, fd int, stat unix.Stat_t) error {
			barriers[mount] = barrierIdentity{fd: fd, dev: uint64(stat.Dev), ino: stat.Ino}
			return nil
		},
		beforeChown: func(_ string, _ string, _ int) error {
			if len(barriers) != 2 {
				return fmt.Errorf("first chown observed before both barrier descriptors")
			}
			return nil
		},
		syncFilesystem: func(mount string, fd int) error {
			barrier, ok := barriers[mount]
			if !ok || fd != barrier.fd {
				return fmt.Errorf("syncfs descriptor for %s was not the original barrier", mount)
			}
			var stat unix.Stat_t
			if err := unix.Fstat(fd, &stat); err != nil {
				return err
			}
			if uint64(stat.Dev) != barrier.dev || stat.Ino != barrier.ino {
				return fmt.Errorf("syncfs descriptor for %s changed identity", mount)
			}
			barrier.synced = true
			barriers[mount] = barrier
			return unix.Syncfs(fd)
		},
		beforeMarkerRename: func() error {
			for mount, barrier := range barriers {
				var stat unix.Stat_t
				if err := unix.Fstat(barrier.fd, &stat); err != nil {
					return fmt.Errorf("barrier descriptor for %s closed before publication: %w", mount, err)
				}
				if uint64(stat.Dev) != barrier.dev || stat.Ino != barrier.ino || !barrier.synced {
					return fmt.Errorf("barrier descriptor for %s was not retained and synced", mount)
				}
			}
			publicationObserved = true
			return nil
		},
	}

	if err := prepareOwnership(cfg, identity, hooks); err != nil {
		t.Fatal(err)
	}
	if !publicationObserved {
		t.Fatal("marker publication did not observe retained barrier descriptors")
	}
}

func TestPrepareOwnershipRejectsDescendantFilesystemCrossing(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	crossing := filepath.Join(cfg.ServerDir, "crossing")
	if err := os.Mkdir(crossing, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(crossing, "nested.txt")
	writeIdentityTestFile(t, nested)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hooks := ownershipHooks{entryDevice: func(mount, name string, actual uint64) uint64 {
		if mount == "server" && (name == "crossing" || strings.HasPrefix(name, "crossing/")) {
			return actual ^ 1
		}
		return actual
	}}

	if err := prepareOwnership(cfg, identity, hooks); err == nil {
		t.Fatal("prepareOwnership accepted a descendant filesystem crossing")
	}
	assertPathOwner(t, crossing, 0, 0)
	assertPathOwner(t, nested, 0, 0)
	assertOwnershipMarkerAbsent(t, cfg.StateDir)
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, crossing, identity.UID, identity.GID)
	assertPathOwner(t, nested, identity.UID, identity.GID)
}

func TestPrepareOwnershipDoesNotPublishAfterRegularFileSyncFailure(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	entry := filepath.Join(cfg.ServerDir, "durability.txt")
	writeIdentityTestFile(t, entry)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected regular file fsync failure")
	hooks := ownershipHooks{syncInode: func(mount, name string, fd int) error {
		if mount == "server" && name == "durability.txt" {
			return injected
		}
		return unix.Fsync(fd)
	}}

	if err := prepareOwnership(cfg, identity, hooks); !errors.Is(err, injected) {
		t.Fatalf("prepareOwnership() error = %v, want injected fsync failure", err)
	}
	assertOwnershipMarkerAbsent(t, cfg.StateDir)
	chownTestPath(t, entry, 0, 0)
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	assertPathOwner(t, entry, identity.UID, identity.GID)
}

func TestPrepareOwnershipDoesNotRecordFailedPrivateSetup(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	if err := os.Mkdir(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chownTestPath(t, cfg.StateDir, testRuntimeUID, testRuntimeGID)
	nested := filepath.Join(cfg.DataDir, "nested.txt")
	writeIdentityTestFile(t, nested)
	external := filepath.Join(identityTestBase(t), "external-state")
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(cfg.StateDir, "home")); err != nil {
		t.Fatal(err)
	}
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, identity); err == nil {
		t.Fatal("PrepareOwnership accepted a symlinked private directory")
	}
	if _, err := os.Lstat(filepath.Join(cfg.StateDir, "ownership.json")); !os.IsNotExist(err) {
		t.Fatalf("marker after failed setup error = %v, want not exist", err)
	}
	assertPathOwner(t, nested, identity.UID, identity.GID)
	assertPathOwner(t, external, 0, 0)
}

func TestDropPrivilegesExposesRuntimeIdentityAndWritableMounts(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	cfg.PUID, cfg.PGID = intPointer(testRuntimeUID), intPointer(testRuntimeGID)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	result := runIdentityHelper(t, cfg, identity)
	if result.DropError != "" || result.ProbeError != "" {
		t.Fatalf("helper drop error = %q, probe error = %q", result.DropError, result.ProbeError)
	}
	assertDroppedIdentity(t, result, identity)
	assertNoIdentityProbes(t, cfg.ServerDir)
	assertNoIdentityProbes(t, cfg.DataDir)
}

func TestDropPrivilegesRetainsInferredRootIdentity(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	identity, err := ResolveIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwnership(cfg, identity); err != nil {
		t.Fatal(err)
	}
	result := runIdentityHelper(t, cfg, identity)
	if result.DropError != "" || result.ProbeError != "" {
		t.Fatalf("helper drop error = %q, probe error = %q", result.DropError, result.ProbeError)
	}
	assertDroppedIdentity(t, result, identity)
}

func TestIdentityPrivilegeDropHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_IDENTITY_HELPER") != "1" {
		return
	}
	uid, uidErr := strconv.Atoi(os.Getenv("IDENTITY_UID"))
	gid, gidErr := strconv.Atoi(os.Getenv("IDENTITY_GID"))
	if uidErr != nil || gidErr != nil {
		os.Exit(2)
	}
	cfg := Config{
		ServerDir: os.Getenv("IDENTITY_SERVER_DIR"),
		DataDir:   os.Getenv("IDENTITY_DATA_DIR"),
		StateDir:  os.Getenv("IDENTITY_STATE_DIR"),
	}
	identity := RuntimeIdentity{UID: uid, GID: gid, Home: filepath.Join(cfg.StateDir, "home")}
	result := identityHelperResult{}
	if err := DropPrivileges(identity); err != nil {
		result.DropError = err.Error()
	} else {
		result.UID = os.Getuid()
		result.EUID = os.Geteuid()
		result.GID = os.Getgid()
		result.EGID = os.Getegid()
		result.Groups, _ = os.Getgroups()
		if err := identity.VerifyWritable(cfg); err != nil {
			result.ProbeError = err.Error()
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

type identityHelperResult struct {
	UID        int    `json:"uid"`
	EUID       int    `json:"euid"`
	GID        int    `json:"gid"`
	EGID       int    `json:"egid"`
	Groups     []int  `json:"groups"`
	DropError  string `json:"drop_error"`
	ProbeError string `json:"probe_error"`
}

func runIdentityHelper(t *testing.T, cfg Config, identity RuntimeIdentity) identityHelperResult {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestIdentityPrivilegeDropHelperProcess$")
	command.Env = append(os.Environ(),
		"GO_WANT_IDENTITY_HELPER=1",
		"IDENTITY_UID="+strconv.Itoa(identity.UID),
		"IDENTITY_GID="+strconv.Itoa(identity.GID),
		"IDENTITY_SERVER_DIR="+cfg.ServerDir,
		"IDENTITY_DATA_DIR="+cfg.DataDir,
		"IDENTITY_STATE_DIR="+cfg.StateDir,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("identity helper: %v\n%s", err, output)
	}
	var result identityHelperResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode helper output %q: %v", output, err)
	}
	return result
}

func assertDroppedIdentity(t *testing.T, result identityHelperResult, want RuntimeIdentity) {
	t.Helper()
	if result.DropError != "" {
		t.Fatalf("DropPrivileges() error = %q", result.DropError)
	}
	if result.UID != want.UID || result.EUID != want.UID || result.GID != want.GID || result.EGID != want.GID {
		t.Fatalf("real/effective identity = %d/%d:%d/%d, want %d:%d", result.UID, result.EUID, result.GID, result.EGID, want.UID, want.GID)
	}
	if len(result.Groups) != 0 {
		t.Fatalf("supplementary groups = %v, want none", result.Groups)
	}
}

func newIdentityTestConfig(t *testing.T) Config {
	t.Helper()
	base := identityTestBase(t)
	serverDir := filepath.Join(base, "server")
	dataDir := filepath.Join(base, "persistentdata")
	for _, directory := range []string{serverDir, dataDir} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return Config{ServerDir: serverDir, DataDir: dataDir, StateDir: filepath.Join(serverDir, ".docker-vrising")}
}

func identityTestBase(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "docker-vrising-identity-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return base
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise ownership and credential changes")
	}
}

func intPointer(value int) *int { return &value }

func writeIdentityTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chownTestPath(t *testing.T, path string, uid, gid int) {
	t.Helper()
	if err := os.Lchown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
}

func assertPathOwner(t *testing.T, path string, uid, gid int) {
	t.Helper()
	gotUID, gotGID := identityTestPathOwner(t, path)
	if gotUID != uid || gotGID != gid {
		t.Fatalf("%s owner = %d:%d, want %d:%d", path, gotUID, gotGID, uid, gid)
	}
}

func identityTestPathOwner(t *testing.T, path string) (int, int) {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return int(stat.Uid), int(stat.Gid)
}

func assertPathOwnerAndMode(t *testing.T, path string, uid, gid int, mode os.FileMode) {
	t.Helper()
	assertPathOwner(t, path, uid, gid)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), mode)
	}
}

func assertPrivateDirectory(t *testing.T, path string, identity RuntimeIdentity) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
	assertPathOwnerAndMode(t, path, identity.UID, identity.GID, 0o700)
}

func assertNoIdentityProbes(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".writable-probe.tmp-") {
			t.Fatalf("probe %q was not removed", entry.Name())
		}
	}
}

func assertNoOwnershipTemporaries(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ownership.json.tmp-") {
			t.Fatalf("ownership marker temporary %q remains", entry.Name())
		}
	}
}

func assertOwnershipMarkerAbsent(t *testing.T, stateDir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(stateDir, ownershipMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("ownership marker error = %v, want not exist", err)
	}
}

func writeMatchingOwnershipMarker(t *testing.T, path string, uid, gid int) {
	t.Helper()
	marker := []byte(fmt.Sprintf(`{"uid":%d,"gid":%d}`, uid, gid))
	if err := os.WriteFile(path, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	chownTestPath(t, path, uid, gid)
}
