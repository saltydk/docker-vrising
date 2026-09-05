package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	RelativePath            string
	BackupPath              string
	QuarantinePath          string
	TombstonePath           string
	Existed                 bool
	OriginalSHA256          string
	InstalledSHA256         string
	TombstoneSHA256         string
	LegacyDeletePlaceholder bool
}

type TransactionJournal struct {
	GenerationID string
	Phase        string
	Entries      []JournalEntry
	Config       *JournalEntry
}

type PromotionJournal struct {
	GenerationID string
	Lock         PackageLock
}

type PendingPromotionError struct {
	GenerationID string
}

func (e *PendingPromotionError) Error() string {
	return fmt.Sprintf("generation %s has pending promotion intent", e.GenerationID)
}

type State struct {
	SchemaVersion  int
	SteamBuild     string
	Active         *GenerationRecord
	Previous       *GenerationRecord
	Candidate      *GenerationRecord
	Failed         *GenerationRecord
	Transaction    *TransactionJournal
	Promotion      *PromotionJournal
	PendingCleanup []string
	Runtime        RuntimeState
}

type Store struct {
	StateDir            string
	syncDirectory       func(int) error
	beforeWrite         func(string) error
	recoveryHook        func(string, string) error
	artifactCleanupHook func(string) error
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
		if errors.Is(err, unix.ENOENT) {
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
	if state.Promotion != nil {
		return &PendingPromotionError{GenerationID: state.Promotion.GenerationID}
	}
	if state.Transaction == nil {
		return nil
	}
	if err := validateTransactionState(state, true); err != nil {
		return fmt.Errorf("validate transaction journal: %w", err)
	}
	journalChanged := false
	for index := range state.Transaction.Entries {
		entry := &state.Transaction.Entries[index]
		if entry.OriginalSHA256 == "" && entry.InstalledSHA256 == "" {
			continue
		}
		if entry.QuarantinePath == "" {
			entry.QuarantinePath, entry.TombstonePath = canonicalTransactionArtifactPaths(state.Candidate.ID, index)
			journalChanged = true
		}
		if entry.TombstoneSHA256 == "" && entry.InstalledSHA256 != "" {
			entry.TombstoneSHA256 = entry.InstalledSHA256
			journalChanged = true
		}
	}
	if journalChanged {
		if err := s.Save(state); err != nil {
			return fmt.Errorf("persist recovery namespace: %w", err)
		}
	}
	if err := validateTransactionState(state, false); err != nil {
		return fmt.Errorf("validate normalized transaction journal: %w", err)
	}
	for _, entry := range state.Transaction.Entries {
		if entry.OriginalSHA256 == "" && entry.InstalledSHA256 == "" {
			continue
		}
		for _, relativePath := range []string{entry.QuarantinePath, entry.TombstonePath} {
			if err := s.ensureProtectedArtifactParent(relativePath); err != nil {
				return fmt.Errorf("prepare recovery namespace: %w", err)
			}
		}
		if err := s.validateProtectedArtifactParent(entry.QuarantinePath); err != nil {
			return fmt.Errorf("validate transaction artifact namespace: %w", err)
		}
	}
	if state.Transaction.Config != nil {
		if err := s.validateProtectedArtifactParent(state.Transaction.Config.QuarantinePath); err != nil {
			return fmt.Errorf("validate config artifact namespace: %w", err)
		}
	}
	converted, err := s.convertLegacyDeletePlaceholders(&state, filepath.Dir(s.StateDir))
	if err != nil {
		return fmt.Errorf("convert legacy stale-delete placeholder: %w", err)
	}
	if len(converted) > 0 {
		if err := s.Save(state); err != nil {
			return fmt.Errorf("persist legacy stale-delete conversion: %w", err)
		}
		if s.recoveryHook != nil {
			for _, relativePath := range converted {
				if err := s.recoveryHook("legacy-placeholder-converted", relativePath); err != nil {
					return err
				}
			}
		}
	}

	serverDir := filepath.Dir(s.StateDir)
	for _, entry := range state.Transaction.Entries {
		if entry.OriginalSHA256 != "" || entry.InstalledSHA256 != "" {
			if err := s.recoverQuarantinedJournalEntry(serverDir, entry); err != nil {
				return fmt.Errorf("recover managed file %s: %w", entry.RelativePath, err)
			}
			if err := s.syncQuarantinedJournalParents(serverDir, entry); err != nil {
				return fmt.Errorf("sync recovered managed file %s: %w", entry.RelativePath, err)
			}
			continue
		}
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
	if state.Transaction.Config != nil {
		if err := s.recoverConfigJournalEntry(serverDir, *state.Transaction.Config); err != nil {
			return fmt.Errorf("recover config file %s: %w", state.Transaction.Config.RelativePath, err)
		}
	}

	state.Transaction = nil
	if state.Candidate != nil {
		failed := *state.Candidate
		failed.Status = "failed"
		state.Failed = &failed
		state.Candidate = nil
	}
	if err := s.Save(state); err != nil {
		return fmt.Errorf("clear recovered transaction: %w", err)
	}
	return nil
}

func (s *Store) recoverQuarantinedJournalEntry(serverDir string, entry JournalEntry) error {
	tombstoneHash := entry.TombstoneSHA256
	if tombstoneHash == "" {
		tombstoneHash = entry.InstalledSHA256
	}
	replacementHash := entry.InstalledSHA256
	if entry.LegacyDeletePlaceholder {
		replacementHash = tombstoneHash
	}
	live, liveExists, err := snapshotFileBelow(serverDir, entry.RelativePath)
	if err != nil {
		return fmt.Errorf("inspect live path: %w", err)
	}
	quarantine, quarantineExists, err := snapshotFileBelow(s.StateDir, entry.QuarantinePath)
	if err != nil {
		return fmt.Errorf("inspect quarantine: %w", err)
	}
	if entry.Existed && !quarantineExists && (!liveExists || live.SHA256 != entry.OriginalSHA256) {
		if err := s.seedOriginalQuarantine(entry); err != nil {
			return fmt.Errorf("seed original quarantine: %w", err)
		}
		quarantine, quarantineExists, err = snapshotFileBelow(s.StateDir, entry.QuarantinePath)
		if err != nil {
			return fmt.Errorf("inspect seeded quarantine: %w", err)
		}
	}
	tombstone, tombstoneExists, err := snapshotFileBelow(s.StateDir, entry.TombstonePath)
	if err != nil {
		return fmt.Errorf("inspect tombstone: %w", err)
	}
	if quarantineExists && (!entry.Existed || quarantine.SHA256 != entry.OriginalSHA256) {
		return fmt.Errorf("quarantine contains externally changed content")
	}
	if tombstoneExists && (tombstoneHash == "" || tombstone.SHA256 != tombstoneHash) {
		return fmt.Errorf("tombstone contains externally changed content")
	}

	if !entry.Existed {
		if quarantineExists {
			return fmt.Errorf("new-file journal has unexpected quarantine content")
		}
		if tombstoneExists {
			if liveExists {
				return fmt.Errorf("live path appeared while candidate tombstone was retained")
			}
			if err := s.removeJournalArtifact(entry.TombstonePath, tombstone); err != nil {
				return err
			}
			return nil
		}
		if !liveExists {
			return nil
		}
		if live.SHA256 != entry.InstalledSHA256 {
			return fmt.Errorf("current new file was changed externally")
		}
		if err := moveFileNoReplace(
			serverDir, entry.RelativePath,
			s.StateDir, entry.TombstonePath,
			s.syncDirectoryFD,
		); err != nil {
			return fmt.Errorf("quarantine installed new file: %w", err)
		}
		if s.recoveryHook != nil {
			if err := s.recoveryHook("tombstoned", entry.RelativePath); err != nil {
				return err
			}
		}
		tombstone, exists, err := snapshotFileBelow(s.StateDir, entry.TombstonePath)
		if err != nil || !exists || tombstone.SHA256 != entry.InstalledSHA256 {
			return fmt.Errorf("verify installed new-file tombstone")
		}
		if err := s.removeJournalArtifact(entry.TombstonePath, tombstone); err != nil {
			return err
		}
		return nil
	}

	if !quarantineExists {
		if liveExists && live.SHA256 == entry.OriginalSHA256 {
			if tombstoneExists {
				if err := s.removeJournalArtifact(entry.TombstonePath, tombstone); err != nil {
					return err
				}
			}
			return nil
		}
		return fmt.Errorf("original file is missing from both live path and quarantine")
	}

	if liveExists && live.SHA256 == entry.OriginalSHA256 && !tombstoneExists {
		if err := s.syncJournalTargetParent(serverDir, entry.RelativePath); err != nil {
			return err
		}
		return s.removeJournalArtifact(entry.QuarantinePath, quarantine)
	}

	if entry.InstalledSHA256 == "" && !entry.LegacyDeletePlaceholder {
		if tombstoneExists {
			return fmt.Errorf("stale deletion has unexpected tombstone content")
		}
		if liveExists {
			return fmt.Errorf("live path appeared after stale file was quarantined")
		}
		return moveFileNoReplace(
			s.StateDir, entry.QuarantinePath,
			serverDir, entry.RelativePath,
			s.syncDirectoryFD,
		)
	}

	if tombstoneExists {
		if liveExists {
			return fmt.Errorf("live path appeared while replacement tombstone was retained")
		}
		if err := moveFileNoReplace(
			s.StateDir, entry.QuarantinePath,
			serverDir, entry.RelativePath,
			s.syncDirectoryFD,
		); err != nil {
			return fmt.Errorf("restore quarantined original: %w", err)
		}
		if err := s.removeJournalArtifact(entry.TombstonePath, tombstone); err != nil {
			return err
		}
		return nil
	}
	if !liveExists {
		return moveFileNoReplace(
			s.StateDir, entry.QuarantinePath,
			serverDir, entry.RelativePath,
			s.syncDirectoryFD,
		)
	}
	if live.SHA256 != replacementHash {
		return fmt.Errorf("current replacement was changed externally")
	}
	if err := moveFileNoReplace(
		serverDir, entry.RelativePath,
		s.StateDir, entry.TombstonePath,
		s.syncDirectoryFD,
	); err != nil {
		return fmt.Errorf("quarantine installed replacement: %w", err)
	}
	if s.recoveryHook != nil {
		if err := s.recoveryHook("tombstoned", entry.RelativePath); err != nil {
			return err
		}
	}
	if err := moveFileNoReplace(
		s.StateDir, entry.QuarantinePath,
		serverDir, entry.RelativePath,
		s.syncDirectoryFD,
	); err != nil {
		return fmt.Errorf("restore quarantined original: %w", err)
	}
	tombstone, exists, err := snapshotFileBelow(s.StateDir, entry.TombstonePath)
	if err != nil || !exists || tombstone.SHA256 != replacementHash {
		return fmt.Errorf("verify replacement tombstone")
	}
	return s.removeJournalArtifact(entry.TombstonePath, tombstone)
}

func (s *Store) convertLegacyDeletePlaceholders(state *State, serverDir string) ([]string, error) {
	converted := make([]string, 0)
	for index := range state.Transaction.Entries {
		entry := &state.Transaction.Entries[index]
		if entry.OriginalSHA256 == "" && entry.InstalledSHA256 == "" ||
			!entry.Existed || entry.InstalledSHA256 != "" || entry.LegacyDeletePlaceholder {
			continue
		}
		live, liveExists, err := snapshotFileBelow(serverDir, entry.RelativePath)
		if err != nil {
			return nil, err
		}
		quarantine, quarantineExists, err := snapshotFileBelow(s.StateDir, entry.QuarantinePath)
		if err != nil {
			return nil, err
		}
		_, tombstoneExists, err := snapshotFileBelow(s.StateDir, entry.TombstonePath)
		if err != nil {
			return nil, err
		}
		if !liveExists || len(live.Data) != 0 || !quarantineExists || quarantine.SHA256 != entry.OriginalSHA256 || tombstoneExists {
			continue
		}
		entry.LegacyDeletePlaceholder = true
		entry.TombstoneSHA256 = hashBytes(nil)
		converted = append(converted, entry.RelativePath)
	}
	return converted, nil
}

func (s *Store) seedOriginalQuarantine(entry JournalEntry) error {
	data, mode, err := readFileBelow(s.StateDir, entry.BackupPath)
	if err != nil {
		return err
	}
	if hashBytes(data) != entry.OriginalSHA256 {
		return fmt.Errorf("transaction backup does not match original SHA-256")
	}
	root, err := s.openStateDirectory(false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	parent, name, err := openRelativeParent(root, entry.QuarantinePath, true, s.syncDirectoryFD)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	temporaryName, err := writeTemporaryFileAt(parent, name, data, mode)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(parent, temporaryName, 0)
	if err := unix.Renameat2(parent, temporaryName, parent, name, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return s.syncDirectoryFD(parent)
}

func (s *Store) recoverConfigJournalEntry(serverDir string, entry JournalEntry) error {
	live, liveExists, err := snapshotFileBelow(serverDir, entry.RelativePath)
	if err != nil {
		return fmt.Errorf("inspect live config: %w", err)
	}
	quarantine, quarantineExists, err := snapshotFileBelow(s.StateDir, entry.QuarantinePath)
	if err != nil {
		return fmt.Errorf("inspect config quarantine: %w", err)
	}
	if quarantineExists && (!entry.Existed || quarantine.SHA256 != entry.OriginalSHA256) {
		return fmt.Errorf("config quarantine contains externally changed content")
	}
	if !entry.Existed {
		if quarantineExists {
			return fmt.Errorf("new config has unexpected quarantine content")
		}
		if !liveExists {
			return s.syncQuarantinedJournalParents(serverDir, entry)
		}
		if live.SHA256 != entry.InstalledSHA256 {
			return fmt.Errorf("new config was changed externally")
		}
		return s.syncQuarantinedJournalParents(serverDir, entry)
	}
	if !quarantineExists {
		if !liveExists || live.SHA256 != entry.OriginalSHA256 && live.SHA256 != entry.InstalledSHA256 {
			return fmt.Errorf("config is not an observed journal state")
		}
		return s.syncQuarantinedJournalParents(serverDir, entry)
	}
	if !liveExists {
		if err := moveFileNoReplace(
			s.StateDir, entry.QuarantinePath,
			serverDir, entry.RelativePath,
			s.syncDirectoryFD,
		); err != nil {
			return fmt.Errorf("restore config before edit: %w", err)
		}
		return s.syncQuarantinedJournalParents(serverDir, entry)
	}
	if live.SHA256 != entry.InstalledSHA256 {
		return fmt.Errorf("config was changed externally")
	}
	if err := s.syncJournalTargetParent(serverDir, entry.RelativePath); err != nil {
		return err
	}
	if err := s.removeJournalArtifact(entry.QuarantinePath, quarantine); err != nil {
		return err
	}
	return s.syncQuarantinedJournalParents(serverDir, entry)
}

func (s *Store) removeJournalArtifact(relativePath string, expected fileSnapshot) error {
	if err := s.validateProtectedArtifactParent(relativePath); err != nil {
		return err
	}
	root, err := s.openStateDirectory(false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	parent, name, err := openRelativeParent(root, relativePath, false, nil)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if s.artifactCleanupHook != nil {
		if err := s.artifactCleanupHook(relativePath); err != nil {
			return err
		}
	}
	current, err := snapshotFileAt(parent, name)
	if err != nil {
		return err
	}
	if current.SHA256 != expected.SHA256 {
		return fmt.Errorf("journal artifact content changed before removal")
	}
	var named unix.Stat_t
	if err := unix.Fstatat(parent, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if named.Mode&unix.S_IFMT != unix.S_IFREG || named.Dev != expected.Dev || named.Ino != expected.Ino {
		return fmt.Errorf("journal artifact changed before removal")
	}
	if err := unix.Unlinkat(parent, name, 0); err != nil {
		return err
	}
	return s.syncDirectoryFD(parent)
}

func validateTransactionState(state State, allowMissingArtifacts bool) error {
	journal := state.Transaction
	if journal == nil {
		return nil
	}
	hashed := journal.Config != nil
	for _, entry := range journal.Entries {
		hashed = hashed || entry.OriginalSHA256 != "" || entry.InstalledSHA256 != ""
	}
	if !hashed {
		return nil
	}
	if state.Candidate == nil || state.Candidate.ID == "" || state.Candidate.ID != journal.GenerationID {
		return fmt.Errorf("transaction generation does not match candidate")
	}
	if state.Candidate.Status != "candidate" {
		return fmt.Errorf("transaction candidate status is invalid")
	}
	if journal.Phase != "applying" && journal.Phase != "applied" {
		return fmt.Errorf("transaction phase is invalid")
	}
	if parts, err := relativePathParts(state.Candidate.ID); err != nil || len(parts) != 1 {
		return fmt.Errorf("candidate generation ID is invalid")
	}
	livePaths := make(map[string]bool, len(journal.Entries)+1)
	artifactPaths := make(map[string]bool, 2*(len(journal.Entries)+1))
	validateEntry := func(entry JournalEntry, index int, config bool) error {
		if _, err := relativePathParts(entry.RelativePath); err != nil {
			return fmt.Errorf("live path %q is invalid", entry.RelativePath)
		}
		if livePaths[entry.RelativePath] {
			return fmt.Errorf("duplicate live path %q", entry.RelativePath)
		}
		livePaths[entry.RelativePath] = true
		if entry.Existed {
			if !validSHA256(entry.OriginalSHA256) {
				return fmt.Errorf("original SHA-256 for %q is invalid", entry.RelativePath)
			}
		} else if entry.OriginalSHA256 != "" {
			return fmt.Errorf("new path %q has an original SHA-256", entry.RelativePath)
		}
		if entry.InstalledSHA256 != "" && !validSHA256(entry.InstalledSHA256) {
			return fmt.Errorf("installed SHA-256 for %q is invalid", entry.RelativePath)
		}
		if entry.TombstoneSHA256 != "" && !validSHA256(entry.TombstoneSHA256) {
			return fmt.Errorf("tombstone SHA-256 for %q is invalid", entry.RelativePath)
		}
		if entry.LegacyDeletePlaceholder && (!entry.Existed || entry.InstalledSHA256 != "" || entry.TombstoneSHA256 != hashBytes(nil)) {
			return fmt.Errorf("legacy placeholder metadata for %q is invalid", entry.RelativePath)
		}
		wantQuarantine, wantTombstone := canonicalTransactionArtifactPaths(state.Candidate.ID, index)
		if allowMissingArtifacts && entry.QuarantinePath == "" && entry.TombstonePath == "" {
			return nil
		}
		if entry.QuarantinePath != wantQuarantine || entry.TombstonePath != wantTombstone {
			return fmt.Errorf("transaction artifacts for %q are not canonical", entry.RelativePath)
		}
		for _, artifactPath := range []string{entry.QuarantinePath, entry.TombstonePath} {
			if artifactPath == "state.json" || artifactPath == "package-lock.json" || artifactPath == "update.lock" {
				return fmt.Errorf("transaction artifact uses reserved name %q", artifactPath)
			}
			if _, err := relativePathParts(artifactPath); err != nil {
				return fmt.Errorf("transaction artifact %q is invalid", artifactPath)
			}
			if artifactPaths[artifactPath] || livePaths[artifactPath] {
				return fmt.Errorf("transaction artifact path %q collides", artifactPath)
			}
			artifactPaths[artifactPath] = true
		}
		if config && entry.RelativePath != "BepInEx/config/BepInEx.cfg" {
			return fmt.Errorf("config journal path is invalid")
		}
		return nil
	}
	for index, entry := range journal.Entries {
		if err := validateEntry(entry, index, false); err != nil {
			return err
		}
	}
	if journal.Config != nil {
		if err := validateEntry(*journal.Config, len(journal.Entries), true); err != nil {
			return err
		}
	}
	for livePath := range livePaths {
		if artifactPaths[livePath] {
			return fmt.Errorf("live path %q collides with transaction artifact", livePath)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func canonicalTransactionArtifactPaths(candidateID string, index int) (string, string) {
	base := fmt.Sprintf("generations/%s/rollback/quarantine/%04d", candidateID, index)
	return base + ".displaced", base + ".tombstone"
}

func (s *Store) validateProtectedArtifactParent(relativePath string) error {
	// The runtime UID is the transaction trust boundary. Recovery assumes no
	// untrusted process can write as that UID, so every artifact ancestor must
	// remain private (0700) and owned by the runtime before name-based cleanup.
	parts, err := relativePathParts(relativePath)
	if err != nil {
		return err
	}
	root, err := s.openStateDirectory(false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	current, err := unix.Dup(root)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(current) }()
	for _, component := range parts[:len(parts)-1] {
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Fstat(next, &stat); err != nil {
			unix.Close(next)
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o7777 != 0o700 {
			unix.Close(next)
			return fmt.Errorf("transaction artifact namespace must be runtime-owned mode 0700")
		}
		unix.Close(current)
		current = next
	}
	return nil
}

func (s *Store) commitTransactionArtifacts(state State) error {
	journal := state.Transaction
	if journal == nil {
		return nil
	}
	if err := validateTransactionState(state, false); err != nil {
		return err
	}
	entries := append([]JournalEntry(nil), journal.Entries...)
	if journal.Config != nil {
		entries = append(entries, *journal.Config)
	}
	for _, entry := range entries {
		if err := s.validateProtectedArtifactParent(entry.QuarantinePath); err != nil {
			return err
		}
		tombstoneHash := entry.TombstoneSHA256
		if tombstoneHash == "" {
			tombstoneHash = entry.InstalledSHA256
		}
		for _, artifact := range []struct {
			path string
			hash string
		}{
			{path: entry.QuarantinePath, hash: entry.OriginalSHA256},
			{path: entry.TombstonePath, hash: tombstoneHash},
		} {
			if artifact.path == "" {
				continue
			}
			snapshot, exists, err := snapshotFileBelow(s.StateDir, artifact.path)
			if err != nil {
				return err
			}
			if !exists {
				if err := s.syncStatePathParent(artifact.path); err != nil {
					return err
				}
				continue
			}
			if artifact.hash == "" || snapshot.SHA256 != artifact.hash {
				return fmt.Errorf("transaction artifact %s contains unexpected content", artifact.path)
			}
			if err := s.removeJournalArtifact(artifact.path, snapshot); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) syncQuarantinedJournalParents(serverDir string, entry JournalEntry) error {
	if err := s.syncJournalTargetParent(serverDir, entry.RelativePath); err != nil {
		return err
	}
	for _, relativePath := range []string{entry.QuarantinePath, entry.TombstonePath} {
		if err := s.syncStatePathParent(relativePath); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) syncStatePathParent(relativePath string) error {
	root, err := s.openStateDirectory(false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	parent, _, err := openRelativeParent(root, relativePath, false, nil)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return s.syncDirectoryFD(parent)
}

func (s *Store) ensureProtectedArtifactParent(relativePath string) error {
	parts, err := relativePathParts(relativePath)
	if err != nil {
		return err
	}
	root, err := s.openStateDirectory(false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	current, err := unix.Dup(root)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(current) }()
	for _, component := range parts[:len(parts)-1] {
		created := false
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == unix.ENOENT {
			if err := unix.Mkdirat(current, component, 0o700); err != nil && err != unix.EEXIST {
				return err
			}
			created = true
			if err := s.syncDirectoryFD(current); err != nil {
				return err
			}
			next, err = unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Fstat(next, &stat); err != nil {
			unix.Close(next)
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o7777 != 0o700 {
			unix.Close(next)
			return fmt.Errorf("transaction artifact namespace must be runtime-owned mode 0700")
		}
		if created {
			if err := s.syncDirectoryFD(next); err != nil {
				unix.Close(next)
				return err
			}
		}
		unix.Close(current)
		current = next
	}
	return nil
}

func (s *Store) syncJournalTargetParent(serverDir, relativePath string) error {
	root, err := openDirectoryPath(serverDir, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	parent, _, err := openRelativeParent(root, relativePath, false, nil)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return s.syncDirectoryFD(parent)
}

func (s *Store) loadJSON(name string, value any) error {
	stateDir, err := s.openStateDirectory(false)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	defer unix.Close(stateDir)
	data, _, err := readFileAt(stateDir, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}

func (s *Store) saveJSON(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	stateDir, err := s.openStateDirectory(true)
	if err != nil {
		return err
	}
	defer unix.Close(stateDir)
	if s.beforeWrite != nil {
		if err := s.beforeWrite(name); err != nil {
			return fmt.Errorf("before writing %s: %w", name, err)
		}
	}
	if err := atomicWriteAt(stateDir, name, data, 0o600, s.syncDirectoryFD); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
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
	var stat unix.Stat_t
	if err := unix.Fstat(stateDir, &stat); err != nil {
		unix.Close(stateDir)
		return -1, fmt.Errorf("stat state directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o7777 != 0o700 {
		unix.Close(stateDir)
		return -1, fmt.Errorf("state directory must be runtime-owned mode 0700")
	}
	return stateDir, nil
}

func (s *Store) removeInterruptedTarget(serverDir, relativePath string) error {
	digest := sha256.Sum256([]byte(relativePath))
	artifactPath := "transaction/legacy-remove/" + hex.EncodeToString(digest[:])
	if err := s.ensureProtectedArtifactParent(artifactPath); err != nil {
		return err
	}
	artifact, artifactExists, err := snapshotFileBelow(s.StateDir, artifactPath)
	if err != nil {
		return err
	}
	_, liveExists, err := snapshotFileBelow(serverDir, relativePath)
	if err != nil {
		return err
	}
	if artifactExists && liveExists {
		return fmt.Errorf("legacy removal has both live and quarantined content")
	}
	if liveExists {
		if err := moveFileNoReplace(serverDir, relativePath, s.StateDir, artifactPath, s.syncDirectoryFD); err != nil {
			return err
		}
		artifact, artifactExists, err = snapshotFileBelow(s.StateDir, artifactPath)
		if err != nil || !artifactExists {
			return fmt.Errorf("verify legacy removal quarantine")
		}
	}
	if artifactExists {
		if err := s.removeJournalArtifact(artifactPath, artifact); err != nil {
			return err
		}
	}
	if err := s.syncJournalTargetParent(serverDir, relativePath); err != nil {
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
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
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
			next, err = unix.Openat(parent, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if err == nil && syncDirectory != nil {
			if err := syncDirectory(parent); err != nil {
				unix.Close(next)
				unix.Close(parent)
				return -1, "", fmt.Errorf("sync ancestor parent: %w", err)
			}
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
