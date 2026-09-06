package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	managedManifestName = ".metadata/manifest.json"
	managedLockName     = ".metadata/package-lock.json"
	configTemplateName  = ".metadata/BepInEx.cfg"
	overlayDirectory    = "overlay"
)

type ManagedFile struct {
	RelativePath string
	SHA256       string
	Mode         fs.FileMode
}

type ManagedManifest struct {
	SchemaVersion int
	GenerationID  string
	LockDigest    string
	Files         []ManagedFile
}

type StagedGeneration struct {
	Record   GenerationRecord
	Dir      string
	Manifest ManagedManifest
	Lock     PackageLock
}

type PromotionCommitOutcome uint8

const (
	PromotionNotCommitted PromotionCommitOutcome = iota
	PromotionCommitted
	PromotionRecoveryRequired
)

type ModManager struct {
	ServerDir      string
	GenerationsDir string
	Store          *Store

	beforeManagedMutation func(string) error
	beforeConfigMutation  func() error
	applyFileHook         func(string) error
	promotionHook         func(string) error
	namespaceHook         func(string, string) error
}

func (m *ModManager) ValidateActive(ctx context.Context, record GenerationRecord, lock PackageLock) (StagedGeneration, error) {
	if err := ctx.Err(); err != nil {
		return StagedGeneration{}, err
	}
	if err := m.validate(); err != nil {
		return StagedGeneration{}, err
	}
	if record.ID == "" || record.Status != "active" || record.LockDigest == "" || record.LockDigest != lock.Digest {
		return StagedGeneration{}, fmt.Errorf("active generation record does not match its package lock")
	}
	if err := validateManagedPackageLock(lock); err != nil {
		return StagedGeneration{}, fmt.Errorf("validate active package lock: %w", err)
	}
	manifest, err := m.loadManagedManifest(record.ID)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("load active managed manifest: %w", err)
	}
	staged := StagedGeneration{
		Record:   record,
		Dir:      filepath.Join(m.GenerationsDir, record.ID, overlayDirectory),
		Manifest: manifest,
		Lock:     clonePackageLock(lock),
	}
	_, _, generationRoot, err := m.openAndValidateStaged(staged)
	if err != nil {
		return StagedGeneration{}, err
	}
	defer unix.Close(generationRoot)
	if err := m.verifyStagedOverlay(generationRoot, manifest); err != nil {
		return StagedGeneration{}, err
	}
	return staged, nil
}

func verifyInstalledManagedFiles(ctx context.Context, serverDir string, manifest ManagedManifest) error {
	for _, managed := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, exists, err := snapshotFileBelow(serverDir, managed.RelativePath)
		if err != nil {
			return fmt.Errorf("open live managed file %s without following links: %w", managed.RelativePath, err)
		}
		if !exists {
			return fmt.Errorf("live managed file %s is missing", managed.RelativePath)
		}
		if snapshot.SHA256 != managed.SHA256 || os.Geteuid() != 0 && snapshot.Mode.Perm() != managed.Mode.Perm() {
			return fmt.Errorf("live managed file %s does not match its manifest", managed.RelativePath)
		}
	}
	return nil
}

func (m *ModManager) Apply(ctx context.Context, staged StagedGeneration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	if err := m.reconcilePromotion(ctx); err != nil {
		return fmt.Errorf("reconcile promotion before apply: %w", err)
	}
	manifest, _, generationRoot, err := m.openAndValidateStaged(staged)
	if err != nil {
		return err
	}
	defer unix.Close(generationRoot)

	state, err := m.Store.Load()
	if errors.Is(err, fs.ErrNotExist) {
		state = State{SchemaVersion: schemaVersion}
		err = nil
	}
	if err != nil {
		return fmt.Errorf("load mod state: %w", err)
	}
	if state.Transaction != nil {
		return fmt.Errorf("an interrupted managed-file transaction must be recovered before apply")
	}
	if state.Candidate != nil {
		return fmt.Errorf("candidate generation %s must be resolved before apply", state.Candidate.ID)
	}

	var prior ManagedManifest
	if state.Active != nil {
		prior, err = m.loadManagedManifest(state.Active.ID)
		if err != nil {
			return fmt.Errorf("load active managed manifest: %w", err)
		}
		if prior.GenerationID != state.Active.ID || prior.LockDigest != state.Active.LockDigest {
			return fmt.Errorf("active generation record does not match its managed manifest")
		}
	}
	if err := m.verifyStagedOverlay(generationRoot, manifest); err != nil {
		return err
	}

	priorFiles := managedFilesByPath(prior.Files)
	nextFiles := managedFilesByPath(manifest.Files)
	paths := unionManagedPaths(priorFiles, nextFiles)
	serverFiles := make(map[string]fileSnapshot, len(priorFiles))
	for _, relativePath := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		priorFile, wasManaged := priorFiles[relativePath]
		snapshot, exists, err := snapshotFileBelow(m.ServerDir, relativePath)
		if err != nil {
			return fmt.Errorf("inspect current managed path %s: %w", relativePath, err)
		}
		if wasManaged {
			if !exists || snapshot.SHA256 != priorFile.SHA256 {
				return fmt.Errorf("current managed file %s differs from the prior inventory", relativePath)
			}
			serverFiles[relativePath] = snapshot
			continue
		}
		if exists {
			return fmt.Errorf("current file %s is not in the prior managed inventory", relativePath)
		}
	}

	configData, configMode, configChanged, configSnapshot, err := m.prepareConsoleConfig(generationRoot)
	if err != nil {
		return err
	}
	if err := ensureDirectoryAt(generationRoot, "rollback/quarantine"); err != nil {
		return fmt.Errorf("create generation rollback area: %w", err)
	}
	preparedPaths := append([]string(nil), paths...)
	if configChanged {
		preparedPaths = append(preparedPaths, "BepInEx/config/BepInEx.cfg")
	}
	if err := ensureServerParents(m.ServerDir, preparedPaths); err != nil {
		return fmt.Errorf("prepare managed-file parents: %w", err)
	}

	entries := make([]JournalEntry, 0, len(paths))
	for index, relativePath := range paths {
		snapshot, existed := serverFiles[relativePath]
		quarantinePath, tombstonePath := canonicalTransactionArtifactPaths(staged.Record.ID, index)
		entry := JournalEntry{
			RelativePath:   relativePath,
			QuarantinePath: quarantinePath,
			TombstonePath:  tombstonePath,
			Existed:        existed,
		}
		if existed {
			entry.OriginalSHA256 = snapshot.SHA256
		}
		if next, ok := nextFiles[relativePath]; ok {
			entry.InstalledSHA256 = next.SHA256
			entry.TombstoneSHA256 = next.SHA256
		}
		entries = append(entries, entry)
	}
	var configEntry *JournalEntry
	if configChanged {
		quarantinePath, tombstonePath := canonicalTransactionArtifactPaths(staged.Record.ID, len(paths))
		entry := JournalEntry{
			RelativePath:    "BepInEx/config/BepInEx.cfg",
			QuarantinePath:  quarantinePath,
			TombstonePath:   tombstonePath,
			Existed:         configSnapshot != nil,
			InstalledSHA256: hashBytes(configData),
			TombstoneSHA256: hashBytes(configData),
		}
		if configSnapshot != nil {
			entry.OriginalSHA256 = configSnapshot.SHA256
		}
		configEntry = &entry
	}

	candidate := staged.Record
	candidate.Status = "candidate"
	state.Candidate = &candidate
	state.Transaction = &TransactionJournal{
		GenerationID: staged.Record.ID,
		Phase:        "applying",
		Entries:      entries,
		Config:       configEntry,
	}
	if err := validateTransactionState(state, m.Store.StateDir); err != nil {
		return fmt.Errorf("validate managed-file journal: %w", err)
	}
	journalEntries := append([]JournalEntry(nil), entries...)
	if configEntry != nil {
		journalEntries = append(journalEntries, *configEntry)
	}
	for _, entry := range journalEntries {
		if err := m.Store.validateProtectedArtifactParent(entry.QuarantinePath); err != nil {
			return fmt.Errorf("validate managed-file artifact namespace: %w", err)
		}
	}
	if err := m.Store.Save(state); err != nil {
		return fmt.Errorf("persist managed-file journal: %w", err)
	}
	entriesByPath := make(map[string]JournalEntry, len(entries))
	for _, entry := range entries {
		entriesByPath[entry.RelativePath] = entry
	}

	for _, relativePath := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.beforeManagedMutation != nil {
			if err := m.beforeManagedMutation(relativePath); err != nil {
				return fmt.Errorf("before mutating managed file %s: %w", relativePath, err)
			}
		}
		entry := entriesByPath[relativePath]
		expected, existed := serverFiles[relativePath]
		if existed {
			if err := m.quarantineManagedFile(entry, expected); err != nil {
				if _, installing := nextFiles[relativePath]; installing {
					return fmt.Errorf("current file changed before install %s: %w", relativePath, err)
				}
				return fmt.Errorf("current file changed before delete %s: %w", relativePath, err)
			}
		}
		if next, ok := nextFiles[relativePath]; ok {
			data, mode, err := readFileAt(generationRoot, overlayDirectory+"/"+relativePath)
			if err != nil {
				return fmt.Errorf("read staged managed file %s: %w", relativePath, err)
			}
			if hashBytes(data) != next.SHA256 || os.Geteuid() != 0 && mode.Perm() != next.Mode.Perm() {
				return fmt.Errorf("staged managed file %s no longer matches its manifest", relativePath)
			}
			if err := publishServerFile(
				m.ServerDir, relativePath, data, next.Mode, "current file appeared before install",
			); err != nil {
				return fmt.Errorf("install managed file %s: %w", relativePath, err)
			}
			if err := fixtureAfterManagedPublication(ctx, relativePath); err != nil {
				return fmt.Errorf("after publishing managed file %s: %w", relativePath, err)
			}
		} else {
			_, exists, err := snapshotFileBelow(m.ServerDir, relativePath)
			if err != nil {
				return fmt.Errorf("inspect stale managed path %s: %w", relativePath, err)
			}
			if exists {
				return fmt.Errorf("stale managed path %s was concurrently replaced", relativePath)
			}
		}
		if m.applyFileHook != nil {
			if err := m.applyFileHook(relativePath); err != nil {
				return fmt.Errorf("apply managed file %s: %w", relativePath, err)
			}
		}
	}
	if configChanged {
		if m.beforeConfigMutation != nil {
			if err := m.beforeConfigMutation(); err != nil {
				return fmt.Errorf("before editing BepInEx console config: %w", err)
			}
		}
		if configSnapshot != nil {
			if err := m.quarantineManagedFile(*configEntry, *configSnapshot); err != nil {
				return fmt.Errorf("config changed before edit: %w", err)
			}
		}
		if err := publishServerFile(
			m.ServerDir, "BepInEx/config/BepInEx.cfg", configData, configMode, "config appeared before edit",
		); err != nil {
			return fmt.Errorf("disable BepInEx console logging: %w", err)
		}
	}

	state.Transaction.Phase = "applied"
	if err := m.Store.Save(state); err != nil {
		return fmt.Errorf("persist applied managed-file journal: %w", err)
	}
	return nil
}

func (m *ModManager) Rollback(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	if err := m.reconcilePromotion(ctx); err != nil {
		return fmt.Errorf("reconcile promotion: %w", err)
	}
	state, err := m.Store.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load mod state for rollback: %w", err)
	}
	if state.Transaction == nil {
		return nil
	}
	if state.Candidate == nil || state.Transaction.GenerationID != state.Candidate.ID {
		return fmt.Errorf("managed-file journal does not match the candidate generation")
	}
	if err := m.Store.RecoverInterruptedTransaction(); err != nil {
		return fmt.Errorf("recover managed-file transaction: %w", err)
	}
	return nil
}

func (m *ModManager) Promote(ctx context.Context, staged StagedGeneration) error {
	_, err := m.CommitPromotion(ctx, staged)
	return err
}

func (m *ModManager) CommitPromotion(ctx context.Context, staged StagedGeneration) (PromotionCommitOutcome, error) {
	if err := ctx.Err(); err != nil {
		return PromotionNotCommitted, err
	}
	if err := m.validate(); err != nil {
		return PromotionNotCommitted, err
	}
	_, lock, generationRoot, err := m.openAndValidateStaged(staged)
	if err != nil {
		return PromotionNotCommitted, err
	}
	if err := unix.Close(generationRoot); err != nil {
		return PromotionNotCommitted, fmt.Errorf("close staged generation: %w", err)
	}

	state, err := m.Store.Load()
	if err != nil {
		return PromotionNotCommitted, fmt.Errorf("load mod state for promotion: %w", err)
	}
	if state.Candidate == nil || state.Candidate.ID != staged.Record.ID || state.Candidate.LockDigest != staged.Record.LockDigest {
		return PromotionNotCommitted, fmt.Errorf("staged generation is not the current candidate")
	}
	if state.Transaction == nil || state.Transaction.GenerationID != staged.Record.ID || state.Transaction.Phase != "applied" {
		return PromotionNotCommitted, fmt.Errorf("candidate generation has not completed apply")
	}
	if state.Promotion != nil {
		return PromotionRecoveryRequired, fmt.Errorf("another generation promotion is already pending")
	}
	if _, err := m.Store.transactionArtifacts(state); err != nil {
		return PromotionNotCommitted, fmt.Errorf("validate promotion transaction: %w", err)
	}
	state.Promotion = &PromotionJournal{GenerationID: staged.Record.ID, Lock: clonePackageLock(lock)}
	state.PendingCleanup = cleanupGenerationIDs(state.Previous, state.Failed)
	if err := m.Store.Save(state); err != nil {
		return PromotionRecoveryRequired, fmt.Errorf("persist generation promotion intent: %w", err)
	}
	if err := m.reconcilePromotion(ctx); err != nil {
		return PromotionRecoveryRequired, err
	}
	return PromotionCommitted, nil
}

func (m *ModManager) RecoverPromotion(ctx context.Context) (PromotionCommitOutcome, error) {
	if err := ctx.Err(); err != nil {
		return PromotionRecoveryRequired, err
	}
	if err := m.validate(); err != nil {
		return PromotionRecoveryRequired, err
	}
	state, err := m.Store.Load()
	if err != nil {
		return PromotionRecoveryRequired, fmt.Errorf("load promotion state: %w", err)
	}
	if state.Promotion == nil {
		return PromotionNotCommitted, nil
	}
	generationID := state.Promotion.GenerationID
	if err := m.reconcilePromotion(ctx); err != nil {
		return PromotionRecoveryRequired, fmt.Errorf("reconcile promotion: %w", err)
	}
	state, err = m.Store.Load()
	if err != nil {
		return PromotionRecoveryRequired, fmt.Errorf("load reconciled promotion state: %w", err)
	}
	if state.Promotion != nil || state.Active == nil || state.Active.ID != generationID {
		return PromotionRecoveryRequired, fmt.Errorf("promotion reconciliation did not commit generation %s", generationID)
	}
	return PromotionCommitted, nil
}

func (m *ModManager) reconcilePromotion(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := m.Store.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := m.Store.transactionArtifacts(state); err != nil {
		return fmt.Errorf("validate pending transaction: %w", err)
	}
	if state.Promotion != nil {
		promotion := state.Promotion
		if promotion.GenerationID == "" || promotion.Lock.Digest == "" ||
			promotion.Lock.Digest != PackageLockDigest(promotion.Lock) {
			return fmt.Errorf("pending promotion metadata is invalid")
		}
		if state.Candidate != nil {
			if state.Candidate.ID != promotion.GenerationID || state.Candidate.LockDigest != promotion.Lock.Digest ||
				state.Transaction == nil || state.Transaction.GenerationID != promotion.GenerationID || state.Transaction.Phase != "applied" {
				return fmt.Errorf("pending promotion does not match applied candidate")
			}
			if err := m.Store.SavePackageLock(promotion.Lock); err != nil {
				return fmt.Errorf("persist pending promotion lock: %w", err)
			}
			if m.promotionHook != nil {
				if err := m.promotionHook("lock-written"); err != nil {
					return err
				}
			}
			if err := m.Store.commitTransactionArtifacts(state); err != nil {
				return fmt.Errorf("commit transaction artifacts: %w", err)
			}
			if state.Active != nil {
				previous := *state.Active
				previous.Status = "previous"
				state.Previous = &previous
			} else {
				state.Previous = nil
			}
			active := *state.Candidate
			active.Status = "active"
			state.Active = &active
			state.Candidate = nil
			state.Failed = nil
			state.Transaction = nil
			if err := m.Store.Save(state); err != nil {
				return fmt.Errorf("persist promoted mod state: %w", err)
			}
		} else {
			if state.Active == nil || state.Active.ID != promotion.GenerationID || state.Active.LockDigest != promotion.Lock.Digest {
				return fmt.Errorf("pending promotion has no matching candidate or active generation")
			}
			lock, err := m.Store.LoadPackageLock()
			if err != nil || !samePackageLock(lock, promotion.Lock) {
				if err := m.Store.SavePackageLock(promotion.Lock); err != nil {
					return fmt.Errorf("reconcile promoted package lock: %w", err)
				}
			}
		}
		if m.promotionHook != nil {
			if err := m.promotionHook("state-written"); err != nil {
				return err
			}
		}
	}

	for len(state.PendingCleanup) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		generationID := state.PendingCleanup[0]
		if generationIsReferenced(state, generationID) {
			return fmt.Errorf("pending cleanup generation %s is still referenced", generationID)
		}
		if err := m.removeGeneration(generationID); err != nil {
			return fmt.Errorf("remove pending generation %s: %w", generationID, err)
		}
		state.PendingCleanup = append([]string(nil), state.PendingCleanup[1:]...)
		if err := m.Store.Save(state); err != nil {
			return fmt.Errorf("clear completed generation cleanup %s: %w", generationID, err)
		}
	}
	if state.Promotion != nil {
		state.Promotion = nil
		if err := m.Store.Save(state); err != nil {
			return fmt.Errorf("clear completed promotion intent: %w", err)
		}
	}
	return nil
}

func cleanupGenerationIDs(records ...*GenerationRecord) []string {
	ids := make([]string, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if record == nil || record.ID == "" || seen[record.ID] {
			continue
		}
		seen[record.ID] = true
		ids = append(ids, record.ID)
	}
	return ids
}

func generationIsReferenced(state State, generationID string) bool {
	for _, record := range []*GenerationRecord{state.Active, state.Previous, state.Candidate, state.Failed} {
		if record != nil && record.ID == generationID {
			return true
		}
	}
	return false
}

func (m *ModManager) removeGeneration(generationID string) error {
	if _, err := relativePathParts(generationID); err != nil {
		return err
	}
	root, err := openDirectoryPath(m.GenerationsDir, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err := removeTreeAt(root, generationID); err != nil {
		return err
	}
	return unix.Fsync(root)
}

func (m *ModManager) Discard(ctx context.Context, staged StagedGeneration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	if staged.Record.ID == "" {
		return fmt.Errorf("staged generation ID is empty")
	}
	if _, err := relativePathParts(staged.Record.ID); err != nil {
		return fmt.Errorf("invalid staged generation ID: %w", err)
	}
	wantDir := filepath.Join(m.GenerationsDir, staged.Record.ID, overlayDirectory)
	if filepath.Clean(staged.Dir) != wantDir {
		return fmt.Errorf("staged generation directory does not match its record")
	}
	state, err := m.Store.Load()
	if err != nil {
		return fmt.Errorf("load mod state before discard: %w", err)
	}
	if generationIsReferenced(state, staged.Record.ID) {
		return fmt.Errorf("generation %s is still referenced", staged.Record.ID)
	}
	if err := m.removeGeneration(staged.Record.ID); err != nil {
		return fmt.Errorf("remove unreferenced generation %s: %w", staged.Record.ID, err)
	}
	return nil
}

func (m *ModManager) Stage(ctx context.Context, lock PackageLock, archives map[PackageRef]*ValidatedArchive) (staged StagedGeneration, err error) {
	if err := ctx.Err(); err != nil {
		return StagedGeneration{}, err
	}
	if err := m.validate(); err != nil {
		return StagedGeneration{}, err
	}
	if err := validateManagedArchiveSet(lock, archives); err != nil {
		return StagedGeneration{}, err
	}

	generationID := lock.Digest + "-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	generationsRoot, err := openDirectoryPath(m.GenerationsDir, true)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("open generations directory: %w", err)
	}
	defer unix.Close(generationsRoot)
	if err := unix.Mkdirat(generationsRoot, generationID, 0o700); err != nil {
		return StagedGeneration{}, fmt.Errorf("create generation: %w", err)
	}
	keepGeneration := false
	defer func() {
		if !keepGeneration {
			_ = removeTreeAt(generationsRoot, generationID)
			_ = unix.Fsync(generationsRoot)
		}
	}()
	if err := unix.Fsync(generationsRoot); err != nil {
		return StagedGeneration{}, fmt.Errorf("sync generations directory: %w", err)
	}

	generationRoot, err := unix.Openat(generationsRoot, generationID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("open generation: %w", err)
	}
	defer unix.Close(generationRoot)
	for _, directory := range []string{
		overlayDirectory,
		".metadata",
		".extract-bepinex",
		".extract-vcf",
		".extract-kindred",
		".extract-hookdots",
		".extract-satisvampory",
	} {
		if err := ensureDirectoryAt(generationRoot, directory); err != nil {
			return StagedGeneration{}, fmt.Errorf("create generation directory %s: %w", directory, err)
		}
	}

	generationPath := filepath.Join(m.GenerationsDir, generationID)
	bepInExResult, err := ExtractArchive(archives[bepInExPackageRef(lock)], filepath.Join(generationPath, ".extract-bepinex"), ExtractBepInEx)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("extract BepInEx archive: %w", err)
	}
	vcfResult, err := ExtractArchive(archives[vcfPackageRef(lock)], filepath.Join(generationPath, ".extract-vcf"), ExtractPlugin)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("extract VampireCommandFramework archive: %w", err)
	}
	kindredResult, err := ExtractArchive(archives[kindredPackageRef(lock)], filepath.Join(generationPath, ".extract-kindred"), ExtractPlugin)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("extract KindredCommands archive: %w", err)
	}
	hookDOTSResult, err := ExtractArchive(archives[hookDOTSPackageRef(lock)], filepath.Join(generationPath, ".extract-hookdots"), ExtractPlugin)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("extract HookDOTS API archive: %w", err)
	}
	satisvamporyResult, err := ExtractArchive(archives[satisvamporyPackageRef(lock)], filepath.Join(generationPath, ".extract-satisvampory"), ExtractPlugin)
	if err != nil {
		return StagedGeneration{}, fmt.Errorf("extract Satisvampory archive: %w", err)
	}

	files := make([]ManagedFile, 0,
		len(bepInExResult.Files)+len(vcfResult.Files)+len(kindredResult.Files)+
			len(hookDOTSResult.Files)+len(satisvamporyResult.Files),
	)
	for _, relativePath := range bepInExResult.Files {
		if err := ctx.Err(); err != nil {
			return StagedGeneration{}, err
		}
		switch {
		case isManagedBepInExPath(relativePath):
			managed, err := copyManagedFile(
				filepath.Join(generationPath, ".extract-bepinex"),
				generationRoot,
				relativePath,
				relativePath,
			)
			if err != nil {
				return StagedGeneration{}, fmt.Errorf("stage BepInEx file %s: %w", relativePath, err)
			}
			files = append(files, managed)
		case relativePath == "BepInEx/config/BepInEx.cfg":
			data, mode, err := readFileBelow(filepath.Join(generationPath, ".extract-bepinex"), relativePath)
			if err != nil {
				return StagedGeneration{}, fmt.Errorf("read BepInEx config template: %w", err)
			}
			if err := writeGenerationFile(generationRoot, configTemplateName, data, mode); err != nil {
				return StagedGeneration{}, fmt.Errorf("stage BepInEx config template: %w", err)
			}
		}
	}

	pluginInputs := []struct {
		label      string
		extractDir string
		result     ExtractedArchive
		allowed    map[string]bool
	}{
		{
			label:      "VampireCommandFramework",
			extractDir: ".extract-vcf",
			result:     vcfResult,
			allowed:    map[string]bool{"VampireCommandFramework.dll": true},
		},
		{
			label:      "KindredCommands",
			extractDir: ".extract-kindred",
			result:     kindredResult,
			allowed: map[string]bool{
				"KindredCommands.dll":  true,
				"NetTopologySuite.dll": true,
			},
		},
		{
			label:      "HookDOTS API",
			extractDir: ".extract-hookdots",
			result:     hookDOTSResult,
			allowed:    map[string]bool{"HookDOTS.API.dll": true},
		},
		{
			label:      "Satisvampory",
			extractDir: ".extract-satisvampory",
			result:     satisvamporyResult,
			allowed:    map[string]bool{"Satisvampory.dll": true},
		},
	}
	for _, plugin := range pluginInputs {
		seen := make(map[string]bool, len(plugin.result.Files))
		for _, name := range plugin.result.Files {
			if !plugin.allowed[name] {
				return StagedGeneration{}, fmt.Errorf("%s archive contains unmapped plugin DLL %s", plugin.label, name)
			}
			seen[name] = true
			destination := "BepInEx/plugins/saltydk-managed/" + name
			managed, err := copyManagedFile(filepath.Join(generationPath, plugin.extractDir), generationRoot, name, destination)
			if err != nil {
				return StagedGeneration{}, fmt.Errorf("stage %s file %s: %w", plugin.label, name, err)
			}
			files = append(files, managed)
		}
		for required := range plugin.allowed {
			if !seen[required] {
				return StagedGeneration{}, fmt.Errorf("%s archive is missing %s", plugin.label, required)
			}
		}
	}

	if !generationFileExists(generationRoot, configTemplateName) {
		return StagedGeneration{}, fmt.Errorf("BepInEx archive is missing BepInEx/config/BepInEx.cfg")
	}
	slices.SortFunc(files, func(left, right ManagedFile) int {
		return strings.Compare(left.RelativePath, right.RelativePath)
	})
	if err := validateManagedFiles(files); err != nil {
		return StagedGeneration{}, err
	}
	if err := requireManagedRuntimeFiles(files); err != nil {
		return StagedGeneration{}, err
	}

	manifest := ManagedManifest{
		SchemaVersion: schemaVersion,
		GenerationID:  generationID,
		LockDigest:    lock.Digest,
		Files:         files,
	}
	if err := writeGenerationJSON(generationRoot, managedManifestName, manifest); err != nil {
		return StagedGeneration{}, fmt.Errorf("persist managed manifest: %w", err)
	}
	if err := writeGenerationJSON(generationRoot, managedLockName, lock); err != nil {
		return StagedGeneration{}, fmt.Errorf("persist generation lock: %w", err)
	}
	for _, extraction := range []string{
		".extract-bepinex",
		".extract-vcf",
		".extract-kindred",
		".extract-hookdots",
		".extract-satisvampory",
	} {
		if err := removeTreeAt(generationRoot, extraction); err != nil {
			return StagedGeneration{}, fmt.Errorf("remove staging extraction %s: %w", extraction, err)
		}
	}
	if err := unix.Fsync(generationRoot); err != nil {
		return StagedGeneration{}, fmt.Errorf("sync staged generation: %w", err)
	}

	now := time.Now().UTC()
	record := GenerationRecord{
		ID:         generationID,
		LockDigest: lock.Digest,
		Status:     "candidate",
		CreatedAt:  now,
	}
	keepGeneration = true
	return StagedGeneration{
		Record:   record,
		Dir:      filepath.Join(generationPath, overlayDirectory),
		Manifest: manifest,
		Lock:     clonePackageLock(lock),
	}, nil
}

func (m *ModManager) openAndValidateStaged(staged StagedGeneration) (ManagedManifest, PackageLock, int, error) {
	if staged.Record.ID == "" {
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("staged generation ID is empty")
	}
	if _, err := relativePathParts(staged.Record.ID); err != nil {
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("invalid staged generation ID: %w", err)
	}
	wantDir := filepath.Join(m.GenerationsDir, staged.Record.ID, overlayDirectory)
	if filepath.Clean(staged.Dir) != wantDir {
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("staged generation directory does not match its record")
	}

	generationsRoot, err := openDirectoryPath(m.GenerationsDir, false)
	if err != nil {
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("open generations directory: %w", err)
	}
	generationRoot, err := unix.Openat(generationsRoot, staged.Record.ID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	unix.Close(generationsRoot)
	if err != nil {
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("open staged generation: %w", err)
	}

	manifest, err := readGenerationJSON[ManagedManifest](generationRoot, managedManifestName)
	if err != nil {
		unix.Close(generationRoot)
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("read staged managed manifest: %w", err)
	}
	lock, err := readGenerationJSON[PackageLock](generationRoot, managedLockName)
	if err != nil {
		unix.Close(generationRoot)
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("read staged package lock: %w", err)
	}
	if err := validateManagedManifest(manifest); err != nil {
		unix.Close(generationRoot)
		return ManagedManifest{}, PackageLock{}, -1, err
	}
	if err := requireManagedRuntimeFiles(manifest.Files); err != nil {
		unix.Close(generationRoot)
		return ManagedManifest{}, PackageLock{}, -1, err
	}
	if manifest.GenerationID != staged.Record.ID || manifest.LockDigest != staged.Record.LockDigest ||
		lock.Digest != staged.Record.LockDigest || lock.Digest != PackageLockDigest(lock) {
		unix.Close(generationRoot)
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("staged generation metadata is inconsistent")
	}
	if !sameManagedManifest(manifest, staged.Manifest) || !samePackageLock(lock, staged.Lock) {
		unix.Close(generationRoot)
		return ManagedManifest{}, PackageLock{}, -1, fmt.Errorf("staged generation value does not match persisted metadata")
	}
	return manifest, lock, generationRoot, nil
}

func (m *ModManager) verifyStagedOverlay(generationRoot int, manifest ManagedManifest) error {
	expected := managedFilesByPath(manifest.Files)
	overlay, err := unix.Openat(generationRoot, overlayDirectory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open staged overlay: %w", err)
	}
	if err := verifyOverlayInventory(overlay, "", expected); err != nil {
		return err
	}
	for _, file := range manifest.Files {
		data, mode, err := readFileAt(generationRoot, overlayDirectory+"/"+file.RelativePath)
		if err != nil {
			return fmt.Errorf("read staged managed file %s: %w", file.RelativePath, err)
		}
		if hashBytes(data) != file.SHA256 || os.Geteuid() != 0 && mode.Perm() != file.Mode.Perm() {
			return fmt.Errorf("staged managed file %s does not match its manifest", file.RelativePath)
		}
	}
	return nil
}

func verifyOverlayInventory(directoryFD int, relativeDir string, expected map[string]ManagedFile) error {
	directory := os.NewFile(uintptr(directoryFD), relativeDir)
	entries, err := directory.Readdirnames(-1)
	if err != nil {
		directory.Close()
		return fmt.Errorf("read staged overlay directory %s: %w", relativeDir, err)
	}
	defer directory.Close()
	for _, name := range entries {
		relativePath := filepath.Join(relativeDir, name)
		var stat unix.Stat_t
		if err := unix.Fstatat(directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("stat staged overlay path %s: %w", relativePath, err)
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			if _, ok := expected[relativePath]; !ok {
				return fmt.Errorf("staged overlay contains unlisted file %s", relativePath)
			}
		case unix.S_IFDIR:
			prefix := relativePath + string(filepath.Separator)
			allowed := false
			for expectedPath := range expected {
				if strings.HasPrefix(expectedPath, prefix) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("staged overlay contains unlisted directory %s", relativePath)
			}
			child, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return fmt.Errorf("open staged overlay directory %s: %w", relativePath, err)
			}
			if err := verifyOverlayInventory(child, relativePath, expected); err != nil {
				return err
			}
		default:
			return fmt.Errorf("staged overlay path %s is not a regular file or directory", relativePath)
		}
	}
	return nil
}

func (m *ModManager) loadManagedManifest(generationID string) (ManagedManifest, error) {
	if _, err := relativePathParts(generationID); err != nil {
		return ManagedManifest{}, err
	}
	data, _, err := readFileBelow(m.GenerationsDir, generationID+"/"+managedManifestName)
	if err != nil {
		return ManagedManifest{}, err
	}
	var manifest ManagedManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ManagedManifest{}, err
	}
	if err := validateManagedManifest(manifest); err != nil {
		return ManagedManifest{}, err
	}
	return manifest, nil
}

func (m *ModManager) prepareConsoleConfig(generationRoot int) ([]byte, fs.FileMode, bool, *fileSnapshot, error) {
	snapshot, exists, err := snapshotFileBelow(m.ServerDir, "BepInEx/config/BepInEx.cfg")
	if err != nil {
		return nil, 0, false, nil, fmt.Errorf("read BepInEx console config: %w", err)
	}
	if exists {
		updated := disableConsoleLogging(snapshot.Data)
		return updated, snapshot.Mode, !bytes.Equal(updated, snapshot.Data), &snapshot, nil
	}
	data, mode, err := readFileAt(generationRoot, configTemplateName)
	if err != nil {
		return nil, 0, false, nil, fmt.Errorf("read staged BepInEx config template: %w", err)
	}
	return disableConsoleLogging(data), mode.Perm(), true, nil, nil
}

type fileSnapshot struct {
	Data   []byte
	SHA256 string
	Mode   fs.FileMode
	Dev    uint64
	Ino    uint64
}

func snapshotFileBelow(rootPath, relativePath string) (fileSnapshot, bool, error) {
	root, err := openDirectoryPath(rootPath, false)
	if err != nil {
		return fileSnapshot{}, false, err
	}
	defer unix.Close(root)
	parent, name, err := openRelativeParent(root, relativePath, false, nil)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fileSnapshot{}, false, nil
		}
		return fileSnapshot{}, false, err
	}
	defer unix.Close(parent)
	snapshot, err := snapshotFileAt(parent, name)
	if errors.Is(err, unix.ENOENT) {
		return fileSnapshot{}, false, nil
	}
	if err != nil {
		return fileSnapshot{}, false, err
	}
	return snapshot, true, nil
}

func snapshotFileAt(parent int, name string) (fileSnapshot, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fileSnapshot{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fileSnapshot{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileSnapshot{}, fmt.Errorf("is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{
		Data: data, SHA256: hashBytes(data), Mode: fs.FileMode(stat.Mode).Perm(), Dev: stat.Dev, Ino: stat.Ino,
	}, nil
}

func publishServerFile(
	serverDir, relativePath string,
	data []byte,
	mode fs.FileMode,
	appearedMessage string,
) error {
	root, err := openDirectoryPath(serverDir, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	parent, name, err := openRelativeParent(root, relativePath, true, unix.Fsync)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	temporaryName, err := writeTemporaryFileAt(parent, name, data, mode)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = unix.Unlinkat(parent, temporaryName, 0)
		}
	}()
	if err := unix.Renameat2(parent, temporaryName, parent, name, unix.RENAME_NOREPLACE); err != nil {
		if err == unix.EEXIST {
			return fmt.Errorf("%s", appearedMessage)
		}
		return err
	}
	removeTemporary = false
	return unix.Fsync(parent)
}

func ensureServerParents(serverDir string, relativePaths []string) error {
	root, err := openDirectoryPath(serverDir, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	for _, relativePath := range relativePaths {
		parent, _, err := openRelativeParent(root, relativePath, true, unix.Fsync)
		if err != nil {
			return err
		}
		if err := unix.Close(parent); err != nil {
			return err
		}
	}
	return nil
}

func writeTemporaryFileAt(parent int, targetName string, data []byte, mode fs.FileMode) (string, error) {
	temporaryName, fd, err := createTemporaryFileAt(parent, targetName, mode)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), temporaryName)
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = unix.Unlinkat(parent, temporaryName, 0)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	remove = false
	return temporaryName, nil
}

func sameFileSnapshot(left, right fileSnapshot) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.SHA256 == right.SHA256
}

func (m *ModManager) quarantineManagedFile(entry JournalEntry, expected fileSnapshot) error {
	if err := moveFileNoReplace(
		m.ServerDir, entry.RelativePath,
		m.Store.StateDir, entry.QuarantinePath,
		unix.Fsync,
	); err != nil {
		return fmt.Errorf("move current file to quarantine: %w", err)
	}
	if m.namespaceHook != nil {
		if err := m.namespaceHook("quarantined", entry.RelativePath); err != nil {
			return err
		}
	}
	displaced, exists, err := snapshotFileBelow(m.Store.StateDir, entry.QuarantinePath)
	if err == nil && exists && sameFileSnapshot(displaced, expected) {
		return nil
	}
	restoreErr := moveFileNoReplace(
		m.Store.StateDir, entry.QuarantinePath,
		m.ServerDir, entry.RelativePath,
		unix.Fsync,
	)
	if restoreErr != nil {
		if err != nil {
			return fmt.Errorf("inspect displaced file: %v; preserve quarantine after blocked restore: %w", err, restoreErr)
		}
		return fmt.Errorf("current file changed before mutation; preserve quarantine after blocked restore: %w", restoreErr)
	}
	if err != nil {
		return fmt.Errorf("inspect displaced file: %w", err)
	}
	return fmt.Errorf("current file changed before mutation")
}

func moveFileNoReplace(sourceRootPath, sourcePath, targetRootPath, targetPath string, syncDirectory func(int) error) error {
	sourceRoot, err := openDirectoryPath(sourceRootPath, false)
	if err != nil {
		return err
	}
	defer unix.Close(sourceRoot)
	targetRoot, err := openDirectoryPath(targetRootPath, false)
	if err != nil {
		return err
	}
	defer unix.Close(targetRoot)
	sourceParent, sourceName, err := openRelativeParent(sourceRoot, sourcePath, false, nil)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	targetParent, targetName, err := openRelativeParent(targetRoot, targetPath, true, syncDirectory)
	if err != nil {
		return err
	}
	defer unix.Close(targetParent)
	if err := unix.Renameat2(sourceParent, sourceName, targetParent, targetName, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := syncDirectory(sourceParent); err != nil {
		return fmt.Errorf("sync source parent: %w", err)
	}
	if err := syncDirectory(targetParent); err != nil {
		return fmt.Errorf("sync target parent: %w", err)
	}
	return nil
}

func readFileAt(root int, relativePath string) ([]byte, fs.FileMode, error) {
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
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, 0, fmt.Errorf("is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, err
	}
	return data, fs.FileMode(stat.Mode).Perm(), nil
}

func readGenerationJSON[T any](root int, relativePath string) (T, error) {
	var value T
	data, _, err := readFileAt(root, relativePath)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, err
	}
	return value, nil
}

func managedFilesByPath(files []ManagedFile) map[string]ManagedFile {
	byPath := make(map[string]ManagedFile, len(files))
	for _, file := range files {
		byPath[file.RelativePath] = file
	}
	return byPath
}

func unionManagedPaths(left, right map[string]ManagedFile) []string {
	paths := make([]string, 0, len(left)+len(right))
	seen := make(map[string]bool, len(left)+len(right))
	for relativePath := range left {
		seen[relativePath] = true
		paths = append(paths, relativePath)
	}
	for relativePath := range right {
		if !seen[relativePath] {
			paths = append(paths, relativePath)
		}
	}
	slices.Sort(paths)
	return paths
}

func validateManagedManifest(manifest ManagedManifest) error {
	if manifest.SchemaVersion != schemaVersion {
		return fmt.Errorf("managed manifest schema version %d is unsupported", manifest.SchemaVersion)
	}
	if manifest.GenerationID == "" || manifest.LockDigest == "" {
		return fmt.Errorf("managed manifest identity is incomplete")
	}
	if _, err := relativePathParts(manifest.GenerationID); err != nil {
		return fmt.Errorf("managed manifest generation ID is invalid: %w", err)
	}
	if err := validateManagedFiles(manifest.Files); err != nil {
		return err
	}
	if !slices.IsSortedFunc(manifest.Files, func(left, right ManagedFile) int {
		return strings.Compare(left.RelativePath, right.RelativePath)
	}) {
		return fmt.Errorf("managed manifest files are not sorted")
	}
	return nil
}

func sameManagedManifest(left, right ManagedManifest) bool {
	if left.SchemaVersion != right.SchemaVersion || left.GenerationID != right.GenerationID ||
		left.LockDigest != right.LockDigest || len(left.Files) != len(right.Files) {
		return false
	}
	for i := range left.Files {
		if left.Files[i] != right.Files[i] {
			return false
		}
	}
	return true
}

func samePackageLock(left, right PackageLock) bool {
	if left.SchemaVersion != right.SchemaVersion || !samePackageRefs(left.Roots, right.Roots) || left.Digest != right.Digest ||
		!left.ResolvedAt.Equal(right.ResolvedAt) || len(left.Packages) != len(right.Packages) {
		return false
	}
	for i := range left.Packages {
		if !sameLockedPackage(left.Packages[i], right.Packages[i]) {
			return false
		}
	}
	return true
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func generationFileExistsBelow(rootPath, relativePath string) bool {
	_, exists, err := snapshotFileBelow(rootPath, relativePath)
	return err == nil && exists
}

type configLine struct {
	body string
	eol  string
}

func disableConsoleLogging(data []byte) []byte {
	lines := splitConfigLines(string(data))
	inConsole := false
	foundSection := false
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i].body)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inConsole {
				lines = slices.Insert(lines, i, configLine{body: "Enabled = false", eol: preferredConfigEOL(lines)})
				return []byte(joinConfigLines(lines))
			}
			inConsole = strings.EqualFold(strings.TrimSpace(trimmed[1:len(trimmed)-1]), "Logging.Console")
			foundSection = foundSection || inConsole
			continue
		}
		if !inConsole || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		equals := strings.IndexByte(lines[i].body, '=')
		if equals < 0 || !strings.EqualFold(strings.TrimSpace(lines[i].body[:equals]), "Enabled") {
			continue
		}
		lines[i].body = replaceConfigValue(lines[i].body, equals, "false")
		return []byte(joinConfigLines(lines))
	}

	eol := preferredConfigEOL(lines)
	if inConsole {
		if len(lines) > 0 && lines[len(lines)-1].eol == "" {
			lines[len(lines)-1].eol = eol
		}
		lines = append(lines, configLine{body: "Enabled = false", eol: eol})
		return []byte(joinConfigLines(lines))
	}
	if len(lines) > 0 && lines[len(lines)-1].eol == "" {
		lines[len(lines)-1].eol = eol
	}
	if len(lines) > 0 && lines[len(lines)-1].body != "" {
		lines = append(lines, configLine{eol: eol})
	}
	if !foundSection {
		lines = append(lines,
			configLine{body: "[Logging.Console]", eol: eol},
			configLine{body: "Enabled = false", eol: eol},
		)
	}
	return []byte(joinConfigLines(lines))
}

func splitConfigLines(value string) []configLine {
	if value == "" {
		return nil
	}
	lines := make([]configLine, 0, strings.Count(value, "\n")+1)
	for len(value) > 0 {
		newline := strings.IndexByte(value, '\n')
		if newline < 0 {
			lines = append(lines, configLine{body: value})
			break
		}
		body := value[:newline]
		eol := "\n"
		if strings.HasSuffix(body, "\r") {
			body = strings.TrimSuffix(body, "\r")
			eol = "\r\n"
		}
		lines = append(lines, configLine{body: body, eol: eol})
		value = value[newline+1:]
	}
	return lines
}

func preferredConfigEOL(lines []configLine) string {
	for _, line := range lines {
		if line.eol != "" {
			return line.eol
		}
	}
	return "\n"
}

func joinConfigLines(lines []configLine) string {
	var output strings.Builder
	for _, line := range lines {
		output.WriteString(line.body)
		output.WriteString(line.eol)
	}
	return output.String()
}

func replaceConfigValue(line string, equals int, value string) string {
	after := line[equals+1:]
	leadingLength := len(after) - len(strings.TrimLeft(after, " \t"))
	comment := len(after)
	for _, marker := range []byte{'#', ';'} {
		if index := strings.IndexByte(after, marker); index >= 0 && index < comment {
			comment = index
		}
	}
	valuePart := after[leadingLength:comment]
	trailingLength := len(valuePart) - len(strings.TrimRight(valuePart, " \t"))
	return line[:equals+1] + after[:leadingLength] + value + valuePart[len(valuePart)-trailingLength:] + after[comment:]
}

func (m *ModManager) validate() error {
	if m == nil {
		return fmt.Errorf("mod manager is nil")
	}
	if m.ServerDir == "" || m.GenerationsDir == "" || m.Store == nil || m.Store.StateDir == "" {
		return fmt.Errorf("mod manager paths and store are required")
	}
	wantGenerations := filepath.Join(filepath.Clean(m.Store.StateDir), "generations")
	if filepath.Clean(m.GenerationsDir) != wantGenerations {
		return fmt.Errorf("generations directory must be the store generations directory")
	}
	if filepath.Clean(filepath.Dir(m.Store.StateDir)) != filepath.Clean(m.ServerDir) {
		return fmt.Errorf("state directory must be directly below the server directory")
	}
	return nil
}

func validateManagedArchiveSet(lock PackageLock, archives map[PackageRef]*ValidatedArchive) error {
	if err := validateManagedPackageLock(lock); err != nil {
		return err
	}
	if len(archives) != len(lock.Packages) {
		return fmt.Errorf("validated archive set does not match the managed package lock")
	}
	for _, locked := range lock.Packages {
		archive, ok := archives[locked.Ref]
		if !ok || archive == nil {
			return fmt.Errorf("validated archive for %s is required", packageVersionFullName(locked.Ref))
		}
		if !sameLockedPackage(archive.LockedPackage(), locked) {
			return fmt.Errorf("validated archive for %s does not match package lock", packageVersionFullName(locked.Ref))
		}
	}
	for ref := range archives {
		found := false
		for _, locked := range lock.Packages {
			if locked.Ref == ref {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("validated archive %s is outside the package lock", packageVersionFullName(ref))
		}
	}
	return nil
}

func validateManagedPackageLock(lock PackageLock) error {
	if lock.SchemaVersion != schemaVersion {
		return fmt.Errorf("package lock schema version %d is unsupported", lock.SchemaVersion)
	}
	if lock.Digest == "" || lock.Digest != PackageLockDigest(lock) {
		return fmt.Errorf("package lock digest is invalid")
	}
	if err := validateManagedRootRefs(lock.Roots); err != nil {
		return err
	}
	if len(lock.Packages) != 5 {
		return fmt.Errorf("managed package lock must contain exactly BepInEx, VampireCommandFramework, KindredCommands, HookDOTS API, and Satisvampory")
	}
	seen := make(map[string]PackageRef, len(lock.Packages))
	dependencies := make(map[PackageRef][]PackageRef, len(lock.Packages))
	for _, locked := range lock.Packages {
		if locked.FullName != packageVersionFullName(locked.Ref) || locked.DownloadURL == "" || locked.FileSize <= 0 || !validSHA256(locked.SHA256) {
			return fmt.Errorf("locked package %s metadata is invalid", packageVersionFullName(locked.Ref))
		}
		kind := managedPackageKind(locked.Ref)
		if kind == "" {
			return fmt.Errorf("locked package %s has no managed mapping", packageVersionFullName(locked.Ref))
		}
		if _, exists := seen[kind]; exists {
			return fmt.Errorf("managed package lock contains duplicate %s package", kind)
		}
		seen[kind] = locked.Ref
		dependencies[locked.Ref] = locked.Dependencies
	}
	for _, kind := range []string{"bepinex", "vcf", "kindred", "hookdots", "satisvampory"} {
		if _, ok := seen[kind]; !ok {
			return fmt.Errorf("managed package lock is missing %s", kind)
		}
	}
	if seen["kindred"] != lock.Roots[0] || seen["satisvampory"] != lock.Roots[1] {
		return fmt.Errorf("package lock roots do not match locked root packages")
	}
	canonicalOrder, err := canonicalPackageOrder(lock.Roots, dependencies)
	if err != nil {
		return fmt.Errorf("validate package order: %w", err)
	}
	for i, ref := range canonicalOrder {
		if lock.Packages[i].Ref != ref {
			return fmt.Errorf("managed package lock packages are not in canonical dependency order")
		}
	}
	return nil
}

func managedPackageKind(ref PackageRef) string {
	switch {
	case ref.Namespace == "BepInEx" && ref.Name == "BepInExPack_V_Rising":
		return "bepinex"
	case ref.Namespace == "deca" && ref.Name == "VampireCommandFramework":
		return "vcf"
	case ref.Namespace == "odjit" && ref.Name == "KindredCommands":
		return "kindred"
	case ref.Namespace == "cheesasaurus" && ref.Name == "HookDOTS_API":
		return "hookdots"
	case ref.Namespace == "Team_GreenEye" && ref.Name == "Satisvampory":
		return "satisvampory"
	default:
		return ""
	}
}

func bepInExPackageRef(lock PackageLock) PackageRef {
	return packageRefForKind(lock, "bepinex")
}

func vcfPackageRef(lock PackageLock) PackageRef {
	return packageRefForKind(lock, "vcf")
}

func kindredPackageRef(lock PackageLock) PackageRef {
	return packageRefForKind(lock, "kindred")
}

func hookDOTSPackageRef(lock PackageLock) PackageRef {
	return packageRefForKind(lock, "hookdots")
}

func satisvamporyPackageRef(lock PackageLock) PackageRef {
	return packageRefForKind(lock, "satisvampory")
}

func packageRefForKind(lock PackageLock, kind string) PackageRef {
	for _, locked := range lock.Packages {
		if managedPackageKind(locked.Ref) == kind {
			return locked.Ref
		}
	}
	return PackageRef{}
}

func sameLockedPackage(left, right LockedPackage) bool {
	if left.Ref != right.Ref || left.FullName != right.FullName || left.DownloadURL != right.DownloadURL ||
		left.FileSize != right.FileSize || left.SHA256 != right.SHA256 || len(left.Dependencies) != len(right.Dependencies) {
		return false
	}
	for i := range left.Dependencies {
		if left.Dependencies[i] != right.Dependencies[i] {
			return false
		}
	}
	return true
}

func clonePackageLock(lock PackageLock) PackageLock {
	lock.Roots = append([]PackageRef(nil), lock.Roots...)
	lock.Packages = append([]LockedPackage(nil), lock.Packages...)
	for i := range lock.Packages {
		lock.Packages[i] = cloneLockedPackage(lock.Packages[i])
	}
	return lock
}

func isManagedBepInExPath(relativePath string) bool {
	switch relativePath {
	case ".doorstop_version", "doorstop_config.ini", "winhttp.dll":
		return true
	}
	return strings.HasPrefix(relativePath, "dotnet/") ||
		strings.HasPrefix(relativePath, "BepInEx/core/") ||
		strings.HasPrefix(relativePath, "BepInEx/patchers/")
}

func copyManagedFile(sourceRoot string, generationRoot int, sourcePath, destinationPath string) (ManagedFile, error) {
	data, mode, err := readFileBelow(sourceRoot, sourcePath)
	if err != nil {
		return ManagedFile{}, err
	}
	if err := writeGenerationFile(generationRoot, overlayDirectory+"/"+destinationPath, data, mode); err != nil {
		return ManagedFile{}, err
	}
	digest := sha256.Sum256(data)
	return ManagedFile{
		RelativePath: destinationPath,
		SHA256:       hex.EncodeToString(digest[:]),
		Mode:         mode.Perm(),
	}, nil
}

func writeGenerationJSON(root int, relativePath string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeGenerationFile(root, relativePath, data, 0o600)
}

func writeGenerationFile(root int, relativePath string, data []byte, mode fs.FileMode) error {
	parent, name, err := openRelativeParent(root, relativePath, true, unix.Fsync)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return atomicWriteAt(parent, name, data, mode, unix.Fsync)
}

func generationFileExists(root int, relativePath string) bool {
	parent, name, err := openRelativeParent(root, relativePath, false, nil)
	if err != nil {
		return false
	}
	defer unix.Close(parent)
	var stat unix.Stat_t
	return unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW) == nil && stat.Mode&unix.S_IFMT == unix.S_IFREG
}

func validateManagedFiles(files []ManagedFile) error {
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if _, err := relativePathParts(file.RelativePath); err != nil {
			return fmt.Errorf("managed manifest path %s is invalid: %w", file.RelativePath, err)
		}
		if !isManagedRuntimePath(file.RelativePath) {
			return fmt.Errorf("managed manifest path %s is outside managed runtime paths", file.RelativePath)
		}
		if seen[file.RelativePath] {
			return fmt.Errorf("managed manifest contains duplicate path %s", file.RelativePath)
		}
		seen[file.RelativePath] = true
		decoded, err := hex.DecodeString(file.SHA256)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("managed manifest path %s has invalid SHA-256", file.RelativePath)
		}
		if file.Mode.Perm() == 0 || file.Mode&^fs.ModePerm != 0 {
			return fmt.Errorf("managed manifest path %s has invalid mode", file.RelativePath)
		}
	}
	return nil
}

func isManagedRuntimePath(relativePath string) bool {
	if isManagedBepInExPath(relativePath) {
		return true
	}
	return strings.HasPrefix(relativePath, "BepInEx/plugins/saltydk-managed/")
}

func requireManagedRuntimeFiles(files []ManagedFile) error {
	paths := managedFilesByPath(files)
	for _, required := range []string{
		".doorstop_version",
		"doorstop_config.ini",
		"winhttp.dll",
		"BepInEx/plugins/saltydk-managed/VampireCommandFramework.dll",
		"BepInEx/plugins/saltydk-managed/KindredCommands.dll",
		"BepInEx/plugins/saltydk-managed/NetTopologySuite.dll",
		"BepInEx/plugins/saltydk-managed/HookDOTS.API.dll",
		"BepInEx/plugins/saltydk-managed/Satisvampory.dll",
	} {
		if _, ok := paths[required]; !ok {
			return fmt.Errorf("staged generation is missing required managed file %s", required)
		}
	}
	for prefix, label := range map[string]string{
		"dotnet/":       "dotnet runtime",
		"BepInEx/core/": "BepInEx core",
	} {
		found := false
		for relativePath := range paths {
			if strings.HasPrefix(relativePath, prefix) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("staged generation is missing required %s files", label)
		}
	}
	return nil
}

func ensureDirectoryAt(root int, relativePath string) error {
	directory, _, err := openRelativeParent(root, relativePath+"/.directory", true, unix.Fsync)
	if err != nil {
		return err
	}
	return unix.Close(directory)
}

func removeTreeAt(parent int, name string) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(parent, name, 0)
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), name)
	entries, readErr := directory.Readdirnames(-1)
	if readErr != nil {
		directory.Close()
		return readErr
	}
	for _, entry := range entries {
		if err := removeTreeAt(fd, entry); err != nil {
			directory.Close()
			return err
		}
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return err
	}
	if err := directory.Close(); err != nil {
		return err
	}
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}
