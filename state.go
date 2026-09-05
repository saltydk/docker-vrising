package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const schemaVersion = 1

type PackageRef struct {
	Namespace string
	Name      string
	Version   string
}

type LockedPackage struct {
	Ref          PackageRef
	FullName     string
	DownloadURL  string
	FileSize     int64
	SHA256       string
	Dependencies []PackageRef
}

type PackageLock struct {
	SchemaVersion int
	Root          PackageRef
	Packages      []LockedPackage
	Digest        string
	ResolvedAt    time.Time
}

type GenerationRecord struct {
	ID         string
	LockDigest string
	Status     string
	CreatedAt  time.Time
}

type ProcessIdentity struct {
	PID        int
	StartTicks uint64
}

func (p ProcessIdentity) Matches(other ProcessIdentity) bool {
	return p.PID == other.PID && p.StartTicks == other.StartTicks
}

type RuntimeState struct {
	Phase      string
	Ready      bool
	Degraded   bool
	Reason     string
	Server     ProcessIdentity
	Generation string
	SteamBuild string
	UpdatedAt  time.Time
}

type JournalEntry struct {
	RelativePath string
	BackupPath   string
	Existed      bool
}

type TransactionJournal struct {
	GenerationID string
	Phase        string
	Entries      []JournalEntry
}

type State struct {
	SchemaVersion int
	SteamBuild    string
	Active        *GenerationRecord
	Previous      *GenerationRecord
	Candidate     *GenerationRecord
	Failed        *GenerationRecord
	Transaction   *TransactionJournal
	Runtime       RuntimeState
}

type Store struct {
	StateDir string
}

func (s *Store) OpenLifetimeLock() (io.Closer, error) {
	if err := s.ensureStateDir(); err != nil {
		return nil, err
	}

	lock, err := os.OpenFile(s.path("update.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lifetime lock: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := lock.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("acquire lifetime lock: %w (close lock: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("acquire lifetime lock: %w", err)
	}
	return lock, nil
}

func (s *Store) Load() (State, error) {
	var state State
	if err := s.loadJSON("state.json", &state); err != nil {
		if os.IsNotExist(err) {
			return State{SchemaVersion: schemaVersion}, nil
		}
		return State{}, err
	}
	if state.SchemaVersion != schemaVersion {
		return State{}, fmt.Errorf("state schema version %d is unsupported", state.SchemaVersion)
	}
	return state, nil
}

func (s *Store) Save(state State) error {
	if state.SchemaVersion != schemaVersion {
		return fmt.Errorf("state schema version %d is unsupported", state.SchemaVersion)
	}
	return s.saveJSON("state.json", state)
}

func (s *Store) LoadPackageLock() (PackageLock, error) {
	var lock PackageLock
	if err := s.loadJSON("package-lock.json", &lock); err != nil {
		return PackageLock{}, err
	}
	if lock.SchemaVersion != schemaVersion {
		return PackageLock{}, fmt.Errorf("package lock schema version %d is unsupported", lock.SchemaVersion)
	}
	return lock, nil
}

func (s *Store) SavePackageLock(lock PackageLock) error {
	if lock.SchemaVersion != schemaVersion {
		return fmt.Errorf("package lock schema version %d is unsupported", lock.SchemaVersion)
	}
	return s.saveJSON("package-lock.json", lock)
}

func (s *Store) RecoverInterruptedTransaction() error {
	state, err := s.Load()
	if err != nil {
		return fmt.Errorf("load transaction state: %w", err)
	}
	if state.Transaction == nil {
		return nil
	}

	serverDir := filepath.Dir(s.StateDir)
	for _, entry := range state.Transaction.Entries {
		target, err := safeRelativePath(serverDir, entry.RelativePath)
		if err != nil {
			return fmt.Errorf("invalid transaction target %q: %w", entry.RelativePath, err)
		}
		if !entry.Existed {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove interrupted managed file %s: %w", entry.RelativePath, err)
			}
			continue
		}

		backup, err := safeRelativePath(s.StateDir, entry.BackupPath)
		if err != nil {
			return fmt.Errorf("invalid transaction backup %q: %w", entry.BackupPath, err)
		}
		data, err := os.ReadFile(backup)
		if err != nil {
			return fmt.Errorf("read transaction backup %s: %w", entry.BackupPath, err)
		}
		info, err := os.Stat(backup)
		if err != nil {
			return fmt.Errorf("stat transaction backup %s: %w", entry.BackupPath, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create managed parent for %s: %w", entry.RelativePath, err)
		}
		if err := atomicWrite(target, data, info.Mode().Perm()); err != nil {
			return fmt.Errorf("restore interrupted managed file %s: %w", entry.RelativePath, err)
		}
	}

	state.Transaction = nil
	if err := s.Save(state); err != nil {
		return fmt.Errorf("clear recovered transaction: %w", err)
	}
	return nil
}

func (s *Store) loadJSON(name string, value any) error {
	data, err := os.ReadFile(s.path(name))
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}

func (s *Store) saveJSON(name string, value any) error {
	if err := s.ensureStateDir(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	if err := atomicWrite(s.path(name), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func (s *Store) ensureStateDir() error {
	if s.StateDir == "" {
		return fmt.Errorf("state directory is empty")
	}
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	return nil
}

func (s *Store) path(name string) string {
	return filepath.Join(s.StateDir, name)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary file mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}

	parent, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent directory: %w", err)
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}

func safeRelativePath(root, path string) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", fmt.Errorf("must be a relative path")
	}
	cleaned := filepath.Clean(filepath.FromSlash(path))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("must remain below its root")
	}
	return filepath.Join(root, cleaned), nil
}
