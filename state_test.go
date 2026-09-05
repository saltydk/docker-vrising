package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
		Roots: []PackageRef{
			{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8"},
			{Namespace: "Team_GreenEye", Name: "Satisvampory", Version: "1.0.85"},
		},
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

func TestStoreWritesRemainConfinedAfterStateDirSymlinkSwap(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		save     func(*Store) error
	}{
		{
			name:     "state",
			fileName: "state.json",
			save: func(store *Store) error {
				return store.Save(State{SchemaVersion: schemaVersion})
			},
		},
		{
			name:     "package lock",
			fileName: "package-lock.json",
			save: func(store *Store) error {
				return store.SavePackageLock(PackageLock{SchemaVersion: schemaVersion})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, ".docker-vrising")
			heldDir := filepath.Join(root, "held-state")
			externalDir := t.TempDir()
			store := &Store{StateDir: stateDir}
			store.beforeWrite = func(name string) error {
				if name != tt.fileName {
					t.Fatalf("write hook name = %q, want %q", name, tt.fileName)
				}
				if err := os.Rename(stateDir, heldDir); err != nil {
					return err
				}
				return os.Symlink(externalDir, stateDir)
			}

			if err := tt.save(store); err != nil {
				t.Fatalf("save after StateDir swap: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(externalDir, tt.fileName)); !os.IsNotExist(err) {
				t.Fatalf("external %s stat error = %v, want not exist", tt.fileName, err)
			}
			info, err := os.Stat(filepath.Join(heldDir, tt.fileName))
			if err != nil {
				t.Fatalf("stat confined %s: %v", tt.fileName, err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("confined %s mode = %04o, want 0600", tt.fileName, info.Mode().Perm())
			}
		})
	}
}

func TestStoreRejectsPermissiveStateDirectory(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := &Store{StateDir: stateDir}

	if err := store.Save(State{SchemaVersion: schemaVersion}); err == nil {
		t.Fatal("Save() accepted a StateDir not protected as mode 0700")
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("state file stat error = %v, want not exist", err)
	}
}

func TestStoreLoadsRejectSymlinkedStateDir(t *testing.T) {
	externalDir := t.TempDir()
	stateData, err := json.Marshal(State{SchemaVersion: schemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	lockData, err := json.Marshal(PackageLock{SchemaVersion: schemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalDir, "state.json"), stateData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalDir, "package-lock.json"), lockData, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), ".docker-vrising")
	if err := os.Symlink(externalDir, stateDir); err != nil {
		t.Fatal(err)
	}
	store := &Store{StateDir: stateDir}

	if _, err := store.Load(); err == nil {
		t.Fatal("Load() accepted state through a symlinked StateDir")
	}
	if _, err := store.LoadPackageLock(); err == nil {
		t.Fatal("LoadPackageLock() accepted a lock through a symlinked StateDir")
	}
}

func TestStoreRejectsUnknownSchemaVersion(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
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
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
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

func TestStoreRejectsUnpublishedSingleRootPackageLock(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"SchemaVersion":1,"Root":{"Namespace":"odjit","Name":"KindredCommands","Version":"2.5.8"},"Packages":[]}`
	if err := os.WriteFile(filepath.Join(stateDir, "package-lock.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Store{StateDir: stateDir}).LoadPackageLock(); err == nil {
		t.Fatal("LoadPackageLock() accepted unpublished single-root state")
	}
}

func TestLifetimeLockRejectsSecondOwner(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
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
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	externalLock := filepath.Join(t.TempDir(), "update.lock")
	writeTestFile(t, externalLock, "external lock")
	if err := os.Symlink(externalLock, filepath.Join(stateDir, "update.lock")); err != nil {
		t.Fatalf("create lock symlink: %v", err)
	}

	if _, err := (&Store{StateDir: stateDir}).OpenLifetimeLock(); err == nil {
		t.Fatal("OpenLifetimeLock() succeeded for a symlinked lock")
	}
}

func TestRecoverFinalSchemaRejectsInvalidJournalWithoutMutation(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*State)
		obsoleteField string
		obsoleteValue any
	}{
		{name: "live path inside state", mutate: func(s *State) {
			s.Transaction.Entries[1].RelativePath = ".docker-vrising/operator.txt"
		}},
		{name: "live aliases earlier quarantine", mutate: func(s *State) {
			s.Transaction.Entries[1].RelativePath = ".docker-vrising/" + s.Transaction.Entries[0].QuarantinePath
		}},
		{name: "live aliases later tombstone", mutate: func(s *State) {
			s.Transaction.Entries[0].RelativePath = ".docker-vrising/" + s.Transaction.Entries[1].TombstonePath
		}},
		{name: "unvalidated backup", obsoleteField: "BackupPath", obsoleteValue: "operator-backup.dll"},
		{name: "missing namespace", mutate: func(s *State) {
			s.Transaction.Entries[1].QuarantinePath = ""
			s.Transaction.Entries[1].TombstonePath = ""
		}},
		{name: "missing tombstone hash", mutate: func(s *State) {
			s.Transaction.Entries[1].TombstoneSHA256 = ""
		}},
		{name: "wrong tombstone hash", mutate: func(s *State) {
			s.Transaction.Entries[1].TombstoneSHA256 = hashBytes([]byte("wrong"))
		}},
		{name: "unhashed removal", mutate: func(s *State) {
			for i := range s.Transaction.Entries {
				s.Transaction.Entries[i] = JournalEntry{RelativePath: s.Transaction.Entries[i].RelativePath}
			}
		}},
		{name: "unhashed candidate mismatch", mutate: func(s *State) {
			s.Transaction.GenerationID = "other"
			for i := range s.Transaction.Entries {
				s.Transaction.Entries[i] = JournalEntry{RelativePath: s.Transaction.Entries[i].RelativePath}
			}
		}},
		{name: "empty candidate mismatch", mutate: func(s *State) {
			s.Transaction.GenerationID = "other"
			s.Transaction.Entries = nil
		}},
		{name: "legacy placeholder", obsoleteField: "LegacyDeletePlaceholder", obsoleteValue: true, mutate: func(s *State) {
			e := &s.Transaction.Entries[1]
			e.InstalledSHA256 = ""
			e.TombstoneSHA256 = hashBytes(nil)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverDir := t.TempDir()
			store := Store{StateDir: filepath.Join(serverDir, ".docker-vrising")}
			state := State{
				SchemaVersion: schemaVersion,
				Candidate:     &GenerationRecord{ID: "candidate", Status: "candidate"},
				Transaction: &TransactionJournal{GenerationID: "candidate", Phase: "applied", Entries: []JournalEntry{
					canonicalReplacementEntry("candidate", 0, "one.dll"),
					canonicalReplacementEntry("candidate", 1, "two.dll"),
				}},
			}
			for _, e := range state.Transaction.Entries {
				writeTestFile(t, filepath.Join(serverDir, e.RelativePath), "candidate")
				writeTestFile(t, filepath.Join(store.StateDir, e.QuarantinePath), "original")
			}
			writeTestFile(t, filepath.Join(store.StateDir, "operator.txt"), "candidate")
			writeTestFile(t, filepath.Join(store.StateDir, "operator-backup.dll"), "original")
			if tt.mutate != nil {
				tt.mutate(&state)
			}
			if tt.name == "legacy placeholder" {
				writeTestFile(t, filepath.Join(serverDir, "two.dll"), "")
			}
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if tt.obsoleteField != "" {
				var raw map[string]any
				if err := json.Unmarshal(data, &raw); err != nil {
					t.Fatal(err)
				}
				entries := raw["Transaction"].(map[string]any)["Entries"].([]any)
				entries[1].(map[string]any)[tt.obsoleteField] = tt.obsoleteValue
				data, err = json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			writeTestFile(t, filepath.Join(store.StateDir, "state.json"), string(data))
			before := transactionTree(t, serverDir)
			if err := store.RecoverInterruptedTransaction(); err == nil {
				t.Error("recovery accepted invalid final-schema journal")
			}
			if after := transactionTree(t, serverDir); !reflect.DeepEqual(before, after) {
				t.Error("rejected journal mutated the server or state tree")
			}
		})
	}
}

func TestStoreTrueOsirisMigrationRequiresPristineState(t *testing.T) {
	for _, input := range []string{"pristine", "malformed", "intermediate"} {
		t.Run(input, func(t *testing.T) {
			serverDir := t.TempDir()
			store := Store{StateDir: filepath.Join(serverDir, ".docker-vrising")}
			for _, path := range []string{"VRisingServer.exe", "save-data/Saves/world.save", "BepInEx/config/BepInEx.cfg", "BepInEx/plugins/manual.dll"} {
				writeTestFile(t, filepath.Join(serverDir, path), "existing TrueOsiris data")
			}
			if input != "pristine" {
				data := "{broken"
				if input == "intermediate" {
					data = `{"SchemaVersion":1,"Candidate":{"ID":"candidate","Status":"candidate"},"Transaction":{"GenerationID":"candidate","Phase":"applying","Entries":[{"RelativePath":"BepInEx/plugins/manual.dll","Existed":false}]}}`
				}
				writeTestFile(t, filepath.Join(store.StateDir, "state.json"), data)
			}
			before := transactionTree(t, serverDir)
			state, err := store.Load()
			if input == "pristine" {
				if err != nil {
					t.Fatal(err)
				}
				if state.SchemaVersion != 1 || state.Transaction != nil || state.Candidate != nil {
					t.Fatalf("initial state = %#v", state)
				}
				if _, err := os.Stat(store.StateDir); !os.IsNotExist(err) {
					t.Fatalf("Load created StateDir: %v", err)
				}
				if err := store.Save(state); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Load(); err != nil {
					t.Fatal(err)
				}
				// State initialization may only add its private directory and state.json.
				after := transactionTree(t, serverDir)
				delete(after, ".docker-vrising")
				delete(after, ".docker-vrising/state.json")
				if !reflect.DeepEqual(before, after) {
					t.Fatal("initialization touched existing server files")
				}
				return
			}
			if err == nil {
				t.Error("Load accepted malformed or intermediate state")
			}
			if err := store.RecoverInterruptedTransaction(); err == nil {
				t.Error("recovery accepted malformed or intermediate state")
			}
			if !reflect.DeepEqual(before, transactionTree(t, serverDir)) {
				t.Fatal("rejection touched existing files")
			}
		})
	}
}

func transactionTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[relative] = info.Mode().String()
		if info.Mode().IsRegular() {
			snapshot, exists, err := snapshotFileBelow(root, relative)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("fixture disappeared: %s", path)
			}
			files[relative] += fmt.Sprintf(":%d:%d:%s", snapshot.Dev, snapshot.Ino, snapshot.SHA256)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRecoverInterruptedTransactionMarksCandidateFailedWithJournalClear(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	store := Store{StateDir: stateDir}
	candidate := GenerationRecord{
		ID:         "candidate-generation",
		LockDigest: "candidate-lock",
		Status:     "candidate",
		CreatedAt:  time.Date(2026, time.September, 5, 14, 0, 0, 0, time.UTC),
	}
	managedPath := filepath.Join(serverDir, "winhttp.dll")
	writeTestFile(t, managedPath, "candidate")
	entry := canonicalReplacementEntry(candidate.ID, 0, "winhttp.dll")
	entry.Existed, entry.OriginalSHA256 = false, ""
	prepareTestArtifactParent(t, &store, entry)
	if err := store.Save(State{
		SchemaVersion: schemaVersion,
		Candidate:     &candidate,
		Transaction: &TransactionJournal{
			GenerationID: candidate.ID,
			Phase:        "applying",
			Entries:      []JournalEntry{entry},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.RecoverInterruptedTransaction(); err != nil {
		t.Fatalf("RecoverInterruptedTransaction() error = %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Transaction != nil || state.Candidate != nil {
		t.Fatalf("recovered state = %#v, want cleared transaction and candidate", state)
	}
	if state.Failed == nil || state.Failed.ID != candidate.ID || state.Failed.Status != "failed" {
		t.Fatalf("failed generation = %#v, want recovered candidate marked failed", state.Failed)
	}
}

func TestRecoverInterruptedTransactionDistinguishesObservedEntryState(t *testing.T) {
	tests := []struct {
		name          string
		existed       bool
		installedHash string
		targetContent string
		wantContent   string
		wantAbsent    bool
		wantErr       bool
		preserveInode bool
	}{
		{
			name:    "replacement unapplied",
			existed: true, installedHash: hashBytes([]byte("candidate")),
			targetContent: "original", wantContent: "original", preserveInode: true,
		},
		{
			name:    "replacement applied",
			existed: true, installedHash: hashBytes([]byte("candidate")),
			targetContent: "candidate", wantContent: "original",
		},
		{
			name:    "replacement externally changed",
			existed: true, installedHash: hashBytes([]byte("candidate")),
			targetContent: "operator", wantContent: "operator", wantErr: true, preserveInode: true,
		},
		{
			name:          "new file unapplied",
			installedHash: hashBytes([]byte("candidate")), wantAbsent: true,
		},
		{
			name:          "new file applied",
			installedHash: hashBytes([]byte("candidate")),
			targetContent: "candidate", wantAbsent: true,
		},
		{
			name:          "new file externally collided",
			installedHash: hashBytes([]byte("candidate")),
			targetContent: "operator", wantContent: "operator", wantErr: true, preserveInode: true,
		},
		{
			name:    "stale delete unapplied",
			existed: true, targetContent: "original", wantContent: "original", preserveInode: true,
		},
		{
			name:    "stale delete applied",
			existed: true, wantContent: "original",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverDir := t.TempDir()
			stateDir := filepath.Join(serverDir, ".docker-vrising")
			store := Store{StateDir: stateDir}
			target := filepath.Join(serverDir, "managed.dll")
			if tt.targetContent != "" {
				writeTestFile(t, target, tt.targetContent)
			}
			var before os.FileInfo
			if tt.preserveInode {
				var err error
				before, err = os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
			}
			entry := JournalEntry{
				RelativePath:    "managed.dll",
				Existed:         tt.existed,
				InstalledSHA256: tt.installedHash,
				TombstoneSHA256: tt.installedHash,
				QuarantinePath:  "generations/candidate/rollback/quarantine/0000.displaced",
				TombstonePath:   "generations/candidate/rollback/quarantine/0000.tombstone",
			}
			prepareTestArtifactParent(t, &store, entry)
			if tt.existed {
				if tt.targetContent != "original" {
					writeTestFile(t, filepath.Join(stateDir, entry.QuarantinePath), "original")
				}
				entry.OriginalSHA256 = hashBytes([]byte("original"))
			}
			candidate := GenerationRecord{ID: "candidate", LockDigest: "lock", Status: "candidate"}
			if err := store.Save(State{
				SchemaVersion: schemaVersion,
				Candidate:     &candidate,
				Transaction: &TransactionJournal{
					GenerationID: "candidate",
					Phase:        "applying",
					Entries:      []JournalEntry{entry},
				},
			}); err != nil {
				t.Fatal(err)
			}

			err := store.RecoverInterruptedTransaction()
			if (err != nil) != tt.wantErr {
				t.Fatalf("RecoverInterruptedTransaction() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantAbsent {
				if _, err := os.Lstat(target); !os.IsNotExist(err) {
					t.Fatalf("target stat error = %v, want not exist", err)
				}
			} else if got := readTestFile(t, target); got != tt.wantContent {
				t.Fatalf("target content = %q, want %q", got, tt.wantContent)
			}
			if tt.preserveInode {
				after, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(before, after) {
					t.Fatal("recovery replaced a file that it did not apply")
				}
			}
			state, loadErr := store.Load()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if tt.wantErr && state.Transaction == nil {
				t.Fatal("recovery cleared journal after an external change")
			}
			if !tt.wantErr && state.Transaction != nil {
				t.Fatalf("recovery retained completed journal = %#v", state.Transaction)
			}
		})
	}
}

func TestRecoverInterruptedHashedNoopResyncsBeforeJournalClear(t *testing.T) {
	tests := []struct {
		name   string
		entry  JournalEntry
		create bool
	}{
		{
			name: "already absent new file",
			entry: JournalEntry{
				RelativePath:    "managed.dll",
				InstalledSHA256: hashBytes([]byte("candidate")),
			},
		},
		{
			name:   "already original replacement",
			create: true,
			entry: JournalEntry{
				RelativePath:    "managed.dll",
				Existed:         true,
				OriginalSHA256:  hashBytes([]byte("original")),
				InstalledSHA256: hashBytes([]byte("candidate")),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverDir := t.TempDir()
			stateDir := filepath.Join(serverDir, ".docker-vrising")
			store := Store{StateDir: stateDir}
			if tt.create {
				writeTestFile(t, filepath.Join(serverDir, "managed.dll"), "original")
			}
			tt.entry.QuarantinePath = "generations/candidate/rollback/quarantine/0000.displaced"
			tt.entry.TombstonePath = "generations/candidate/rollback/quarantine/0000.tombstone"
			tt.entry.TombstoneSHA256 = tt.entry.InstalledSHA256
			prepareTestArtifactParent(t, &store, tt.entry)
			candidate := GenerationRecord{ID: "candidate", LockDigest: "lock", Status: "candidate"}
			if err := store.Save(State{
				SchemaVersion: schemaVersion,
				Candidate:     &candidate,
				Transaction: &TransactionJournal{
					GenerationID: "candidate",
					Phase:        "applying",
					Entries:      []JournalEntry{tt.entry},
				},
			}); err != nil {
				t.Fatal(err)
			}
			syncer := newFailFirstDirectorySync(t, serverDir)
			store.syncDirectory = syncer.Sync

			if err := store.RecoverInterruptedTransaction(); err == nil {
				t.Fatal("first recovery succeeded after managed parent sync failure")
			}
			assertTransactionRetained(t, &store)
			if err := store.RecoverInterruptedTransaction(); err != nil {
				t.Fatalf("second recovery error = %v", err)
			}
			if syncer.targetCalls != 2 {
				t.Fatalf("managed parent sync calls = %d, want 2", syncer.targetCalls)
			}
			assertTransactionCleared(t, &store)
		})
	}
}

func TestRecoverInterruptedTransactionRejectsFIFOWithoutBlocking(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	store := Store{StateDir: stateDir}
	target := filepath.Join(serverDir, "managed.dll")
	if err := unix.Mkfifo(target, 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := GenerationRecord{ID: "candidate", LockDigest: "lock", Status: "candidate"}
	entry := canonicalReplacementEntry(candidate.ID, 0, "managed.dll")
	entry.Existed, entry.OriginalSHA256 = false, ""
	prepareTestArtifactParent(t, &store, entry)
	if err := store.Save(State{
		SchemaVersion: schemaVersion,
		Candidate:     &candidate,
		Transaction: &TransactionJournal{
			GenerationID: "candidate",
			Phase:        "applying",
			Entries:      []JournalEntry{entry},
		},
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- store.RecoverInterruptedTransaction() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("recovery accepted FIFO as a managed file")
		}
	case <-time.After(250 * time.Millisecond):
		writer, err := unix.Open(target, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			unix.Close(writer)
		}
		<-done
		t.Fatal("recovery blocked opening a FIFO")
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO after recovery: info=%v err=%v", info, err)
	}
}

func TestRecoverConfigSyncsLiveBeforeDeletingFallback(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	store := Store{StateDir: stateDir}
	candidate := GenerationRecord{ID: "candidate", LockDigest: "lock", Status: "candidate"}
	configRelative := "BepInEx/config/BepInEx.cfg"
	quarantinePath := "generations/candidate/rollback/quarantine/0000.displaced"
	livePath := filepath.Join(serverDir, filepath.FromSlash(configRelative))
	writeTestFile(t, livePath, "console=false")
	writeTestFile(t, filepath.Join(stateDir, filepath.FromSlash(quarantinePath)), "console=true")
	if err := store.Save(State{
		SchemaVersion: schemaVersion,
		Candidate:     &candidate,
		Transaction: &TransactionJournal{
			GenerationID: candidate.ID,
			Phase:        "applying",
			Config: &JournalEntry{
				RelativePath:    configRelative,
				QuarantinePath:  quarantinePath,
				TombstonePath:   "generations/candidate/rollback/quarantine/0000.tombstone",
				Existed:         true,
				OriginalSHA256:  hashBytes([]byte("console=true")),
				InstalledSHA256: hashBytes([]byte("console=false")),
				TombstoneSHA256: hashBytes([]byte("console=false")),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	syncer := newFailFirstDirectorySync(t, filepath.Dir(livePath))
	store.syncDirectory = syncer.Sync

	if err := store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("first recovery succeeded after live config parent sync failure")
	}
	if got := readTestFile(t, filepath.Join(stateDir, filepath.FromSlash(quarantinePath))); got != "console=true" {
		t.Fatalf("config fallback after failed live sync = %q, want preserved", got)
	}
	assertTransactionRetained(t, &store)
	if err := store.RecoverInterruptedTransaction(); err != nil {
		t.Fatalf("second recovery error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, filepath.FromSlash(quarantinePath))); !os.IsNotExist(err) {
		t.Fatalf("config fallback after retry stat error = %v, want not exist", err)
	}
	if syncer.targetCalls < 2 {
		t.Fatalf("live config parent sync calls = %d, want at least 2", syncer.targetCalls)
	}
}

func TestRecoverCleansDuplicateOriginalQuarantine(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "stale deletion"
		if replacement {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			serverDir := t.TempDir()
			stateDir := filepath.Join(serverDir, ".docker-vrising")
			store := Store{StateDir: stateDir}
			candidate := GenerationRecord{ID: "candidate", LockDigest: "lock", Status: "candidate"}
			quarantinePath := "generations/candidate/rollback/quarantine/0000.displaced"
			livePath := filepath.Join(serverDir, "managed.dll")
			writeTestFile(t, livePath, "original")
			before, err := os.Stat(livePath)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(stateDir, filepath.FromSlash(quarantinePath)), "original")
			entry := JournalEntry{
				RelativePath:   "managed.dll",
				QuarantinePath: quarantinePath,
				TombstonePath:  "generations/candidate/rollback/quarantine/0000.tombstone",
				Existed:        true,
				OriginalSHA256: hashBytes([]byte("original")),
			}
			if replacement {
				entry.InstalledSHA256 = hashBytes([]byte("candidate"))
				entry.TombstoneSHA256 = entry.InstalledSHA256
			}
			if err := store.Save(State{
				SchemaVersion: schemaVersion,
				Candidate:     &candidate,
				Transaction: &TransactionJournal{
					GenerationID: candidate.ID,
					Phase:        "applying",
					Entries:      []JournalEntry{entry},
				},
			}); err != nil {
				t.Fatal(err)
			}

			if err := store.RecoverInterruptedTransaction(); err != nil {
				t.Fatalf("RecoverInterruptedTransaction() error = %v", err)
			}
			after, err := os.Stat(livePath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || readTestFile(t, livePath) != "original" {
				t.Fatal("recovery replaced the already-restored live original")
			}
			if _, err := os.Lstat(filepath.Join(stateDir, filepath.FromSlash(quarantinePath))); !os.IsNotExist(err) {
				t.Fatalf("duplicate quarantine stat error = %v, want not exist", err)
			}
			assertTransactionCleared(t, &store)
		})
	}
}

func TestRecoverRejectsPermissiveTransactionArtifactNamespace(t *testing.T) {
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	store := Store{StateDir: stateDir}
	candidate := GenerationRecord{ID: "candidate", LockDigest: "lock", Status: "candidate"}
	quarantinePath := "generations/candidate/rollback/quarantine/0000.displaced"
	livePath := filepath.Join(serverDir, "managed.dll")
	writeTestFile(t, livePath, "candidate")
	writeTestFile(t, filepath.Join(stateDir, filepath.FromSlash(quarantinePath)), "original")
	if err := store.Save(State{
		SchemaVersion: schemaVersion,
		Candidate:     &candidate,
		Transaction: &TransactionJournal{
			GenerationID: candidate.ID,
			Phase:        "applying",
			Entries: []JournalEntry{{
				RelativePath:    "managed.dll",
				QuarantinePath:  quarantinePath,
				TombstonePath:   "generations/candidate/rollback/quarantine/0000.tombstone",
				Existed:         true,
				OriginalSHA256:  hashBytes([]byte("original")),
				InstalledSHA256: hashBytes([]byte("candidate")),
				TombstoneSHA256: hashBytes([]byte("candidate")),
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Dir(filepath.Join(stateDir, filepath.FromSlash(quarantinePath)))
	if err := os.Chmod(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("recovery accepted a transaction artifact namespace not protected as mode 0700")
	}
	if got := readTestFile(t, livePath); got != "candidate" {
		t.Fatalf("live file after rejected namespace = %q, want candidate", got)
	}
	if got := readTestFile(t, filepath.Join(stateDir, filepath.FromSlash(quarantinePath))); got != "original" {
		t.Fatalf("quarantine after rejected namespace = %q, want original", got)
	}
}

func TestRecoverValidatesCompleteJournalBeforeMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]JournalEntry) []JournalEntry
	}{
		{
			name: "noncanonical artifact",
			mutate: func(entries []JournalEntry) []JournalEntry {
				entries[0].QuarantinePath = "generations/candidate/rollback/quarantine/operator.displaced"
				return entries
			},
		},
		{
			name: "reserved artifact",
			mutate: func(entries []JournalEntry) []JournalEntry {
				entries[0].QuarantinePath = "package-lock.json"
				return entries
			},
		},
		{
			name: "artifact live-path collision",
			mutate: func(entries []JournalEntry) []JournalEntry {
				entries[0].QuarantinePath = "BepInEx/core/core.dll"
				return entries
			},
		},
		{
			name: "duplicate artifact",
			mutate: func(entries []JournalEntry) []JournalEntry {
				entries[1].QuarantinePath = entries[0].QuarantinePath
				return entries
			},
		},
		{
			name: "artifact alias",
			mutate: func(entries []JournalEntry) []JournalEntry {
				entries[0].QuarantinePath = "generations/candidate/rollback/quarantine/../quarantine/0000.displaced"
				return entries
			},
		},
		{
			name:   "candidate mismatch",
			mutate: func(entries []JournalEntry) []JournalEntry { return entries },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverDir := t.TempDir()
			stateDir := filepath.Join(serverDir, ".docker-vrising")
			store := Store{StateDir: stateDir}
			candidateID := "candidate"
			transactionID := candidateID
			if tt.name == "candidate mismatch" {
				transactionID = "other"
			}
			entries := []JournalEntry{
				canonicalReplacementEntry(candidateID, 0, "one.dll"),
				canonicalReplacementEntry(candidateID, 1, "two.dll"),
			}
			entries = tt.mutate(entries)
			candidate := GenerationRecord{ID: candidateID, LockDigest: "lock", Status: "candidate"}
			for _, entry := range entries {
				writeTestFile(t, filepath.Join(serverDir, filepath.FromSlash(entry.RelativePath)), "candidate")
			}
			if err := store.saveJSON("state.json", State{
				SchemaVersion: schemaVersion,
				Candidate:     &candidate,
				Transaction: &TransactionJournal{
					GenerationID: transactionID,
					Phase:        "applying",
					Entries:      entries,
				},
			}); err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.QuarantinePath == "state.json" {
					continue
				}
				writeTestFile(t, filepath.Join(stateDir, filepath.FromSlash(entry.QuarantinePath)), "original")
			}

			if err := store.RecoverInterruptedTransaction(); err == nil {
				t.Fatal("recovery accepted malicious transaction journal")
			}
			for _, relativePath := range []string{"one.dll", "two.dll"} {
				if got := readTestFile(t, filepath.Join(serverDir, relativePath)); got != "candidate" {
					t.Fatalf("live %s after rejected journal = %q, want candidate", relativePath, got)
				}
			}
		})
	}
}

func canonicalReplacementEntry(candidateID string, index int, relativePath string) JournalEntry {
	artifactBase := fmt.Sprintf("generations/%s/rollback/quarantine/%04d", candidateID, index)
	return JournalEntry{
		RelativePath:    relativePath,
		QuarantinePath:  artifactBase + ".displaced",
		TombstonePath:   artifactBase + ".tombstone",
		Existed:         true,
		OriginalSHA256:  hashBytes([]byte("original")),
		InstalledSHA256: hashBytes([]byte("candidate")),
		TombstoneSHA256: hashBytes([]byte("candidate")),
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
	entry := canonicalReplacementEntry("candidate", 0, "BepInEx/managed.dll")
	entry.Existed, entry.OriginalSHA256 = false, ""
	prepareTestArtifactParent(t, &store, entry)
	if err := store.Save(State{
		SchemaVersion: 1,
		Candidate:     &GenerationRecord{ID: "candidate", Status: "candidate"},
		Transaction:   &TransactionJournal{GenerationID: "candidate", Phase: "applying", Entries: []JournalEntry{entry}},
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

func prepareTestArtifactParent(t *testing.T, store *Store, entry JournalEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(store.StateDir, entry.QuarantinePath)), 0o700); err != nil {
		t.Fatal(err)
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
