package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStoreSaveIsAtomicAndRoundTrips(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), ".docker-vrising")
	store := Store{StateDir: stateDir}
	want := State{
		SchemaVersion: 1,
		SteamBuild:    "123456",
		Active: &GenerationRecord{
			ID:         "generation-1",
			LockDigest: "abc123",
			Status:     "active",
			CreatedAt:  time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC),
		},
		Runtime: RuntimeState{
			Phase:      "ready",
			Ready:      true,
			Server:     ProcessIdentity{PID: 123, StartTicks: 456},
			Generation: "generation-1",
			SteamBuild: "123456",
			UpdatedAt:  time.Date(2026, time.September, 5, 12, 1, 0, 0, time.UTC),
		},
	}

	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state file mode = %o, want 600", got)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read state directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("state directory entries = %v, want only state.json", entries)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestStorePackageLockSaveIsAtomicAndRoundTrips(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), ".docker-vrising")
	store := Store{StateDir: stateDir}
	want := PackageLock{
		SchemaVersion: 1,
		Root:          PackageRef{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8"},
		Packages: []LockedPackage{{
			Ref:          PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"},
			FullName:     "BepInEx-BepInExPack_V_Rising-1.733.2",
			DownloadURL:  "https://example.invalid/bepinex.zip",
			FileSize:     42,
			SHA256:       "deadbeef",
			Dependencies: []PackageRef{},
		}},
		Digest:     "lock-digest",
		ResolvedAt: time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC),
	}

	if err := store.SavePackageLock(want); err != nil {
		t.Fatalf("SavePackageLock() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(stateDir, "package-lock.json"))
	if err != nil {
		t.Fatalf("stat package lock: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("package lock mode = %o, want 600", got)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read state directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "package-lock.json" {
		t.Fatalf("state directory entries = %v, want only package-lock.json", entries)
	}

	got, err := store.LoadPackageLock()
	if err != nil {
		t.Fatalf("LoadPackageLock() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadPackageLock() = %#v, want %#v", got, want)
	}
}

func TestStoreRejectsUnknownSchemaVersion(t *testing.T) {
	stateDir := t.TempDir()
	data, err := json.Marshal(State{SchemaVersion: 2})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if _, err := (&Store{StateDir: stateDir}).Load(); err == nil {
		t.Fatal("Load() succeeded for an unknown schema version")
	}
}

func TestStoreRejectsUnknownPackageLockSchemaVersion(t *testing.T) {
	stateDir := t.TempDir()
	data, err := json.Marshal(PackageLock{SchemaVersion: 2})
	if err != nil {
		t.Fatalf("marshal package lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "package-lock.json"), data, 0o600); err != nil {
		t.Fatalf("write package lock: %v", err)
	}

	if _, err := (&Store{StateDir: stateDir}).LoadPackageLock(); err == nil {
		t.Fatal("LoadPackageLock() succeeded for an unknown schema version")
	}
}

func TestLifetimeLockRejectsSecondOwner(t *testing.T) {
	stateDir := t.TempDir()
	first, err := (&Store{StateDir: stateDir}).OpenLifetimeLock()
	if err != nil {
		t.Fatalf("open first lifetime lock: %v", err)
	}
	defer first.Close()

	if _, err := (&Store{StateDir: stateDir}).OpenLifetimeLock(); err == nil {
		t.Fatal("OpenLifetimeLock() succeeded while the first owner remained open")
	}
}

func TestRecoverInterruptedTransactionRestoresRecordedFiles(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	store := Store{StateDir: stateDir}
	managedPath := filepath.Join(serverDir, "BepInEx", "managed.dll")
	newManagedPath := filepath.Join(serverDir, "BepInEx", "plugins", "new.dll")
	userPath := filepath.Join(serverDir, "BepInEx", "plugins", "user.dll")
	backupPath := filepath.Join(stateDir, "transaction", "managed.dll")

	writeTestFile(t, managedPath, "replacement")
	writeTestFile(t, newManagedPath, "new managed file")
	writeTestFile(t, userPath, "user file")
	writeTestFile(t, backupPath, "original")
	if err := store.Save(State{
		SchemaVersion: 1,
		Transaction: &TransactionJournal{
			GenerationID: "generation-2",
			Phase:        "applying",
			Entries: []JournalEntry{
				{RelativePath: "BepInEx/managed.dll", BackupPath: "transaction/managed.dll", Existed: true},
				{RelativePath: "BepInEx/plugins/new.dll", Existed: false},
			},
		},
	}); err != nil {
		t.Fatalf("save transaction state: %v", err)
	}

	if err := store.RecoverInterruptedTransaction(); err != nil {
		t.Fatalf("RecoverInterruptedTransaction() error = %v", err)
	}
	if got := readTestFile(t, managedPath); got != "original" {
		t.Fatalf("recovered managed file = %q, want original", got)
	}
	if _, err := os.Stat(newManagedPath); !os.IsNotExist(err) {
		t.Fatalf("new managed file stat error = %v, want not exist", err)
	}
	if got := readTestFile(t, userPath); got != "user file" {
		t.Fatalf("user file = %q, want unchanged user file", got)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("load recovered state: %v", err)
	}
	if state.Transaction != nil {
		t.Fatalf("recovered transaction = %#v, want nil", state.Transaction)
	}
	if err := store.RecoverInterruptedTransaction(); err != nil {
		t.Fatalf("second RecoverInterruptedTransaction() error = %v", err)
	}
}

func TestProcessIdentityRejectsReusedPID(t *testing.T) {
	recorded := ProcessIdentity{PID: 123, StartTicks: 456}
	reused := ProcessIdentity{PID: 123, StartTicks: 789}

	if recorded.Matches(reused) {
		t.Fatal("Matches() accepted a reused PID with different start ticks")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
