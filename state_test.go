package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

func TestLifetimeLockRejectsSymlinkedLock(t *testing.T) {
	stateDir := t.TempDir()
	externalLock := filepath.Join(t.TempDir(), "update.lock")
	writeTestFile(t, externalLock, "external lock")
	if err := os.Symlink(externalLock, filepath.Join(stateDir, "update.lock")); err != nil {
		t.Fatalf("create lock symlink: %v", err)
	}

	if _, err := (&Store{StateDir: stateDir}).OpenLifetimeLock(); err == nil {
		t.Fatal("OpenLifetimeLock() succeeded for a symlinked lock")
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

func TestRecoverInterruptedTransactionDoesNotDeleteThroughSymlink(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	externalDir := t.TempDir()
	externalFile := filepath.Join(externalDir, "managed.dll")
	store := Store{StateDir: stateDir}

	writeTestFile(t, externalFile, "external managed file")
	if err := os.Symlink(externalDir, filepath.Join(serverDir, "BepInEx")); err != nil {
		t.Fatalf("create target symlink: %v", err)
	}
	if err := store.Save(State{
		SchemaVersion: 1,
		Transaction: &TransactionJournal{Entries: []JournalEntry{{
			RelativePath: "BepInEx/managed.dll",
			Existed:      false,
		}}},
	}); err != nil {
		t.Fatalf("save transaction state: %v", err)
	}

	if err := store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("RecoverInterruptedTransaction() succeeded through a target symlink")
	}
	if got := readTestFile(t, externalFile); got != "external managed file" {
		t.Fatalf("external file = %q, want unchanged external managed file", got)
	}
}

func TestRecoverInterruptedTransactionDoesNotRestoreFromSymlinkedBackup(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	externalDir := t.TempDir()
	managedPath := filepath.Join(serverDir, "BepInEx", "managed.dll")
	externalBackup := filepath.Join(externalDir, "managed.dll")
	store := Store{StateDir: stateDir}

	writeTestFile(t, managedPath, "replacement")
	writeTestFile(t, externalBackup, "external backup")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	if err := os.Symlink(externalDir, filepath.Join(stateDir, "transaction")); err != nil {
		t.Fatalf("create backup symlink: %v", err)
	}
	if err := store.Save(State{
		SchemaVersion: 1,
		Transaction: &TransactionJournal{Entries: []JournalEntry{{
			RelativePath: "BepInEx/managed.dll",
			BackupPath:   "transaction/managed.dll",
			Existed:      true,
		}}},
	}); err != nil {
		t.Fatalf("save transaction state: %v", err)
	}

	if err := store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("RecoverInterruptedTransaction() restored from a symlinked backup")
	}
	if got := readTestFile(t, managedPath); got != "replacement" {
		t.Fatalf("managed file = %q, want unchanged replacement", got)
	}
}

func TestRecoverInterruptedTransactionRetainsJournalWhenRemovalParentSyncFails(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	managedPath := filepath.Join(serverDir, "BepInEx", "new.dll")
	store := Store{StateDir: stateDir}

	writeTestFile(t, managedPath, "new managed file")
	if err := store.Save(State{
		SchemaVersion: 1,
		Transaction: &TransactionJournal{Entries: []JournalEntry{{
			RelativePath: "BepInEx/new.dll",
			Existed:      false,
		}}},
	}); err != nil {
		t.Fatalf("save transaction state: %v", err)
	}
	store.syncDirectory = func(int) error { return errors.New("target parent sync failed") }

	if err := store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("RecoverInterruptedTransaction() succeeded after target parent sync failed")
	}
	assertTransactionRetained(t, &store)
}

func TestRecoverInterruptedTransactionRetainsJournalWhenCreatedAncestorSyncFails(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	backupPath := filepath.Join(stateDir, "transaction", "managed.dll")
	store := Store{StateDir: stateDir}

	writeTestFile(t, backupPath, "original")
	if err := store.Save(State{
		SchemaVersion: 1,
		Transaction: &TransactionJournal{Entries: []JournalEntry{{
			RelativePath: "BepInEx/plugins/managed.dll",
			BackupPath:   "transaction/managed.dll",
			Existed:      true,
		}}},
	}); err != nil {
		t.Fatalf("save transaction state: %v", err)
	}
	store.syncDirectory = func(int) error { return errors.New("created ancestor sync failed") }

	if err := store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("RecoverInterruptedTransaction() succeeded after created ancestor sync failed")
	}
	assertTransactionRetained(t, &store)
}

func TestRecoverInterruptedTransactionResyncsParentAfterFailedDeleteSync(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	managedPath := filepath.Join(serverDir, "BepInEx", "new.dll")
	store := Store{StateDir: stateDir}

	writeTestFile(t, managedPath, "new managed file")
	if err := store.Save(State{
		SchemaVersion: 1,
		Transaction: &TransactionJournal{Entries: []JournalEntry{{
			RelativePath: "BepInEx/new.dll",
			Existed:      false,
		}}},
	}); err != nil {
		t.Fatalf("save transaction state: %v", err)
	}
	syncer := newFailFirstDirectorySync(t, filepath.Dir(managedPath))
	store.syncDirectory = syncer.Sync

	if err := store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("first RecoverInterruptedTransaction() succeeded after target parent sync failed")
	}
	assertTransactionRetained(t, &store)
	if err := store.RecoverInterruptedTransaction(); err != nil {
		t.Fatalf("second RecoverInterruptedTransaction() error = %v", err)
	}
	if syncer.targetCalls != 2 {
		t.Fatalf("target parent sync calls = %d, want 2 across failed and successful attempts", syncer.targetCalls)
	}
	assertTransactionCleared(t, &store)
}

func TestRecoverInterruptedTransactionResyncsExistingAncestorAfterFailedCreateSync(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	backupPath := filepath.Join(stateDir, "transaction", "managed.dll")
	store := Store{StateDir: stateDir}

	writeTestFile(t, backupPath, "original")
	if err := store.Save(State{
		SchemaVersion: 1,
		Transaction: &TransactionJournal{Entries: []JournalEntry{{
			RelativePath: "BepInEx/plugins/managed.dll",
			BackupPath:   "transaction/managed.dll",
			Existed:      true,
		}}},
	}); err != nil {
		t.Fatalf("save transaction state: %v", err)
	}
	syncer := newFailFirstDirectorySync(t, serverDir)
	store.syncDirectory = syncer.Sync

	if err := store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("first RecoverInterruptedTransaction() succeeded after created ancestor parent sync failed")
	}
	assertTransactionRetained(t, &store)
	if info, err := os.Stat(filepath.Join(serverDir, "BepInEx")); err != nil || !info.IsDir() {
		t.Fatalf("first attempt did not leave the expected created ancestor: info=%v err=%v", info, err)
	}
	if err := store.RecoverInterruptedTransaction(); err != nil {
		t.Fatalf("second RecoverInterruptedTransaction() error = %v", err)
	}
	if syncer.targetCalls != 2 {
		t.Fatalf("created ancestor parent sync calls = %d, want 2 across failed and successful attempts", syncer.targetCalls)
	}
	assertTransactionCleared(t, &store)
}

func TestRecoverInterruptedTransactionDoesNotLeakDescriptorsAfterFailedAncestorSync(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	backupPath := filepath.Join(stateDir, "transaction", "managed.dll")
	store := Store{StateDir: stateDir}

	writeTestFile(t, backupPath, "original")
	if err := os.MkdirAll(filepath.Join(serverDir, "BepInEx"), 0o700); err != nil {
		t.Fatalf("create managed parent: %v", err)
	}
	if err := store.Save(State{
		SchemaVersion: 1,
		Transaction: &TransactionJournal{Entries: []JournalEntry{{
			RelativePath: "BepInEx/managed.dll",
			BackupPath:   "transaction/managed.dll",
			Existed:      true,
		}}},
	}); err != nil {
		t.Fatalf("save transaction state: %v", err)
	}
	store.syncDirectory = func(int) error { return errors.New("injected ancestor parent sync failure") }

	before := descriptorCount(t)
	for range 64 {
		if err := store.RecoverInterruptedTransaction(); err == nil {
			t.Fatal("RecoverInterruptedTransaction() succeeded after ancestor parent sync failed")
		}
	}
	after := descriptorCount(t)
	if after > before+3 {
		t.Fatalf("open descriptor count grew from %d to %d after failed recovery attempts", before, after)
	}
	assertTransactionRetained(t, &store)
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

func assertTransactionRetained(t *testing.T, store *Store) {
	t.Helper()
	state, err := store.Load()
	if err != nil {
		t.Fatalf("load transaction state: %v", err)
	}
	if state.Transaction == nil {
		t.Fatal("transaction journal was cleared before all recovery directories synced")
	}
}

func assertTransactionCleared(t *testing.T, store *Store) {
	t.Helper()
	state, err := store.Load()
	if err != nil {
		t.Fatalf("load recovered state: %v", err)
	}
	if state.Transaction != nil {
		t.Fatalf("transaction journal = %#v, want nil", state.Transaction)
	}
}

type failFirstDirectorySync struct {
	target      directoryIdentity
	targetCalls int
}

func newFailFirstDirectorySync(t *testing.T, path string) *failFirstDirectorySync {
	t.Helper()
	return &failFirstDirectorySync{target: directoryIdentityFor(t, path)}
}

func (s *failFirstDirectorySync) Sync(fd int) error {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	if (directoryIdentity{dev: info.Dev, ino: info.Ino}) != s.target {
		return nil
	}
	s.targetCalls++
	if s.targetCalls == 1 {
		return errors.New("injected directory sync failure")
	}
	return nil
}

type directoryIdentity struct {
	dev uint64
	ino uint64
}

func directoryIdentityFor(t *testing.T, path string) directoryIdentity {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open directory %s: %v", path, err)
	}
	defer unix.Close(fd)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		t.Fatalf("stat directory %s: %v", path, err)
	}
	return directoryIdentity{dev: info.Dev, ino: info.Ino}
}

func descriptorCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read process descriptors: %v", err)
	}
	return len(entries)
}
