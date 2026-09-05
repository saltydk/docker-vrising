package main

import (
	"crypto/rand"
	"encoding/hex"
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
	StateDir      string
	syncDirectory func(int) error
}

func (s *Store) syncDirectoryFD(fd int) error {
	if s.syncDirectory != nil {
		return s.syncDirectory(fd)
	}
	return unix.Fsync(fd)
}

func (s *Store) OpenLifetimeLock() (io.Closer, error) {
	stateDir, err := s.openStateDirectory(true)
	if err != nil {
		return nil, err
	}
	defer unix.Close(stateDir)

	lock, err := unix.Openat(stateDir, "update.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lifetime lock: %w", err)
	}
	var info unix.Stat_t
	if err := unix.Fstat(lock, &info); err != nil {
		unix.Close(lock)
		return nil, fmt.Errorf("stat lifetime lock: %w", err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(lock)
		return nil, fmt.Errorf("lifetime lock is not a regular file")
	}
	if err := unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := unix.Close(lock)
		if closeErr != nil {
			return nil, fmt.Errorf("acquire lifetime lock: %w (close lock: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("acquire lifetime lock: %w", err)
	}
	return os.NewFile(uintptr(lock), "update.lock"), nil
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
		if !entry.Existed {
			if err := s.removeInterruptedTarget(serverDir, entry.RelativePath); err != nil {
				return fmt.Errorf("remove interrupted managed file %s: %w", entry.RelativePath, err)
			}
			continue
		}
		data, mode, err := readFileBelow(s.StateDir, entry.BackupPath)
		if err != nil {
			return fmt.Errorf("read transaction backup %s: %w", entry.BackupPath, err)
		}
		if err := s.restoreInterruptedTarget(serverDir, entry.RelativePath, data, mode); err != nil {
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
	stateDir, err := s.openStateDirectory(true)
	if err != nil {
		return err
	}
	if err := unix.Close(stateDir); err != nil {
		return fmt.Errorf("close state directory: %w", err)
	}
	return nil
}

func (s *Store) openStateDirectory(create bool) (int, error) {
	if s.StateDir == "" {
		return -1, fmt.Errorf("state directory is empty")
	}
	stateDir, err := openDirectoryPath(s.StateDir, create)
	if err != nil {
		return -1, fmt.Errorf("open state directory: %w", err)
	}
	return stateDir, nil
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

func (s *Store) removeInterruptedTarget(serverDir, relativePath string) error {
	root, err := openDirectoryPath(serverDir, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)

	parent, name, err := openRelativeParent(root, relativePath, false, nil)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if err := unix.Unlinkat(parent, name, 0); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return err
	}
	if err := s.syncDirectoryFD(parent); err != nil {
		return fmt.Errorf("sync target parent: %w", err)
	}
	return nil
}

func (s *Store) restoreInterruptedTarget(serverDir, relativePath string, data []byte, mode os.FileMode) error {
	root, err := openDirectoryPath(serverDir, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)

	parent, name, err := openRelativeParent(root, relativePath, true, s.syncDirectoryFD)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return atomicWriteAt(parent, name, data, mode, s.syncDirectoryFD)
}

func readFileBelow(rootPath, relativePath string) ([]byte, os.FileMode, error) {
	root, err := openDirectoryPath(rootPath, false)
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(root)

	parent, name, err := openRelativeParent(root, relativePath, false, nil)
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return nil, 0, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, 0, fmt.Errorf("is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, err
	}
	return data, os.FileMode(info.Mode).Perm(), nil
}

func openDirectoryPath(path string, create bool) (int, error) {
	if !filepath.IsAbs(path) {
		return -1, fmt.Errorf("directory path must be absolute")
	}
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if component == "" || component == "." {
			continue
		}
		next, err := unix.Openat(root, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == unix.ENOENT && create {
			if err := unix.Mkdirat(root, component, 0o700); err != nil && err != unix.EEXIST {
				unix.Close(root)
				return -1, err
			}
			if err := unix.Fsync(root); err != nil {
				unix.Close(root)
				return -1, fmt.Errorf("sync created directory parent: %w", err)
			}
			next, err = unix.Openat(root, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		unix.Close(root)
		if err != nil {
			return -1, err
		}
		root = next
	}
	return root, nil
}

func openRelativeParent(root int, relativePath string, create bool, syncDirectory func(int) error) (int, string, error) {
	parts, err := relativePathParts(relativePath)
	if err != nil {
		return -1, "", err
	}
	parent, err := unix.Dup(root)
	if err != nil {
		return -1, "", err
	}
	for _, component := range parts[:len(parts)-1] {
		created := false
		next, err := unix.Openat(parent, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == unix.ENOENT && create {
			if err := unix.Mkdirat(parent, component, 0o700); err != nil && err != unix.EEXIST {
				unix.Close(parent)
				return -1, "", err
			}
			created = true
			if err := syncDirectory(parent); err != nil {
				unix.Close(parent)
				return -1, "", fmt.Errorf("sync created ancestor parent: %w", err)
			}
			next, err = unix.Openat(parent, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		unix.Close(parent)
		if err != nil {
			return -1, "", err
		}
		if created {
			if err := syncDirectory(next); err != nil {
				unix.Close(next)
				return -1, "", fmt.Errorf("sync created ancestor: %w", err)
			}
		}
		parent = next
	}
	return parent, parts[len(parts)-1], nil
}

func atomicWriteAt(parent int, name string, data []byte, mode os.FileMode, syncDirectory func(int) error) error {
	temporaryName, fd, err := createTemporaryFileAt(parent, name, mode)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(parent, temporaryName, 0)
	file := os.NewFile(uintptr(fd), temporaryName)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := unix.Renameat(parent, temporaryName, parent, name); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync target parent: %w", err)
	}
	return nil
}

func createTemporaryFileAt(parent int, name string, mode os.FileMode) (string, int, error) {
	for range 16 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", -1, fmt.Errorf("create temporary suffix: %w", err)
		}
		temporaryName := "." + name + ".tmp-" + hex.EncodeToString(suffix[:])
		fd, err := unix.Openat(parent, temporaryName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
		if err == unix.EEXIST {
			continue
		}
		if err != nil {
			return "", -1, err
		}
		if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
			unix.Close(fd)
			unix.Unlinkat(parent, temporaryName, 0)
			return "", -1, err
		}
		return temporaryName, fd, nil
	}
	return "", -1, fmt.Errorf("create temporary file: too many collisions")
}

func relativePathParts(relativePath string) ([]string, error) {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return nil, fmt.Errorf("must be a relative path")
	}
	parts := strings.Split(filepath.ToSlash(relativePath), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("must remain below its root")
		}
	}
	return parts, nil
}
