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

type ModManager struct {
	ServerDir      string
	GenerationsDir string
	Store          *Store

	applyFileHook func(string) error
}

func (m *ModManager) Apply(ctx context.Context, staged StagedGeneration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
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

	configData, configMode, configChanged, err := m.prepareConsoleConfig(generationRoot)
	if err != nil {
		return err
	}
	if err := ensureDirectoryAt(generationRoot, "rollback"); err != nil {
		return fmt.Errorf("create generation rollback area: %w", err)
	}
	if err := ensureServerParents(m.ServerDir, paths); err != nil {
		return fmt.Errorf("prepare managed-file parents: %w", err)
	}

	entries := make([]JournalEntry, 0, len(paths))
	for _, relativePath := range paths {
		snapshot, existed := serverFiles[relativePath]
		entry := JournalEntry{RelativePath: relativePath, Existed: existed}
		if existed {
			generationBackup := "rollback/" + relativePath
			if err := writeGenerationFile(generationRoot, generationBackup, snapshot.Data, snapshot.Mode); err != nil {
				return fmt.Errorf("back up managed file %s: %w", relativePath, err)
			}
			entry.BackupPath = "generations/" + staged.Record.ID + "/" + generationBackup
		}
		entries = append(entries, entry)
	}

	candidate := staged.Record
	candidate.Status = "candidate"
	state.Candidate = &candidate
	state.Transaction = &TransactionJournal{
		GenerationID: staged.Record.ID,
		Phase:        "applying",
		Entries:      entries,
	}
	if err := m.Store.Save(state); err != nil {
		return fmt.Errorf("persist managed-file journal: %w", err)
	}

	for _, relativePath := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if next, ok := nextFiles[relativePath]; ok {
			data, mode, err := readFileAt(generationRoot, overlayDirectory+"/"+relativePath)
			if err != nil {
				return fmt.Errorf("read staged managed file %s: %w", relativePath, err)
			}
			if hashBytes(data) != next.SHA256 || mode.Perm() != next.Mode.Perm() {
				return fmt.Errorf("staged managed file %s no longer matches its manifest", relativePath)
			}
			if err := writeServerFile(m.ServerDir, relativePath, data, next.Mode); err != nil {
				return fmt.Errorf("install managed file %s: %w", relativePath, err)
			}
		} else if err := removeServerFile(m.ServerDir, relativePath); err != nil {
			return fmt.Errorf("remove stale managed file %s: %w", relativePath, err)
		}
		if m.applyFileHook != nil {
			if err := m.applyFileHook(relativePath); err != nil {
				return fmt.Errorf("apply managed file %s: %w", relativePath, err)
			}
		}
	}
	if configChanged {
		if err := writeServerFile(m.ServerDir, "BepInEx/config/BepInEx.cfg", configData, configMode); err != nil {
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
	failed := *state.Candidate
	if err := m.Store.RecoverInterruptedTransaction(); err != nil {
		return fmt.Errorf("recover managed-file transaction: %w", err)
	}
	state, err = m.Store.Load()
	if err != nil {
		return fmt.Errorf("reload mod state after rollback: %w", err)
	}
	failed.Status = "failed"
	state.Candidate = nil
	state.Failed = &failed
	if err := m.Store.Save(state); err != nil {
		return fmt.Errorf("persist rolled-back mod state: %w", err)
	}
	return nil
}

func (m *ModManager) Promote(ctx context.Context, staged StagedGeneration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	_, lock, generationRoot, err := m.openAndValidateStaged(staged)
	if err != nil {
		return err
	}
	defer unix.Close(generationRoot)

	state, err := m.Store.Load()
	if err != nil {
		return fmt.Errorf("load mod state for promotion: %w", err)
	}
	if state.Candidate == nil || state.Candidate.ID != staged.Record.ID || state.Candidate.LockDigest != staged.Record.LockDigest {
		return fmt.Errorf("staged generation is not the current candidate")
	}
	if state.Transaction == nil || state.Transaction.GenerationID != staged.Record.ID || state.Transaction.Phase != "applied" {
		return fmt.Errorf("candidate generation has not completed apply")
	}

	oldPrevious := state.Previous
	oldFailed := state.Failed
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
	if err := m.Store.SavePackageLock(lock); err != nil {
		return fmt.Errorf("persist promoted package lock: %w", err)
	}
	if err := m.Store.Save(state); err != nil {
		return fmt.Errorf("persist promoted mod state: %w", err)
	}

	if err := removeTreeAt(generationRoot, "rollback"); err != nil {
		return fmt.Errorf("remove promoted rollback area: %w", err)
	}
	if err := unix.Fsync(generationRoot); err != nil {
		return fmt.Errorf("sync promoted generation: %w", err)
	}
	keep := map[string]bool{active.ID: true}
	if state.Previous != nil {
		keep[state.Previous.ID] = true
	}
	for _, obsolete := range []*GenerationRecord{oldPrevious, oldFailed} {
		if obsolete == nil || keep[obsolete.ID] {
			continue
		}
		if err := m.removeGeneration(obsolete.ID); err != nil {
			return fmt.Errorf("remove obsolete generation %s: %w", obsolete.ID, err)
		}
	}
	return nil
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
	for _, directory := range []string{overlayDirectory, ".metadata", ".extract-bepinex", ".extract-vcf", ".extract-kindred"} {
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

	files := make([]ManagedFile, 0, len(bepInExResult.Files)+len(vcfResult.Files)+len(kindredResult.Files))
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
	for _, extraction := range []string{".extract-bepinex", ".extract-vcf", ".extract-kindred"} {
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
	for _, file := range manifest.Files {
		data, mode, err := readFileAt(generationRoot, overlayDirectory+"/"+file.RelativePath)
		if err != nil {
			return fmt.Errorf("read staged managed file %s: %w", file.RelativePath, err)
		}
		if hashBytes(data) != file.SHA256 || mode.Perm() != file.Mode.Perm() {
			return fmt.Errorf("staged managed file %s does not match its manifest", file.RelativePath)
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

func (m *ModManager) prepareConsoleConfig(generationRoot int) ([]byte, fs.FileMode, bool, error) {
	data, mode, err := readFileBelow(m.ServerDir, "BepInEx/config/BepInEx.cfg")
	if err != nil {
		if !errors.Is(err, unix.ENOENT) {
			return nil, 0, false, fmt.Errorf("read BepInEx console config: %w", err)
		}
		data, mode, err = readFileAt(generationRoot, configTemplateName)
		if err != nil {
			return nil, 0, false, fmt.Errorf("read staged BepInEx config template: %w", err)
		}
	}
	updated := disableConsoleLogging(data)
	return updated, mode.Perm(), !bytes.Equal(updated, data) || !generationFileExistsBelow(m.ServerDir, "BepInEx/config/BepInEx.cfg"), nil
}

type fileSnapshot struct {
	Data   []byte
	SHA256 string
	Mode   fs.FileMode
}

func snapshotFileBelow(rootPath, relativePath string) (fileSnapshot, bool, error) {
	data, mode, err := readFileBelow(rootPath, relativePath)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fileSnapshot{}, false, nil
		}
		return fileSnapshot{}, false, err
	}
	return fileSnapshot{Data: data, SHA256: hashBytes(data), Mode: mode.Perm()}, true, nil
}

func writeServerFile(serverDir, relativePath string, data []byte, mode fs.FileMode) error {
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
	return atomicWriteAt(parent, name, data, mode, unix.Fsync)
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

func removeServerFile(serverDir, relativePath string) error {
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
	if err := unix.Unlinkat(parent, name, 0); err != nil && err != unix.ENOENT {
		return err
	}
	return unix.Fsync(parent)
}

func readFileAt(root int, relativePath string) ([]byte, fs.FileMode, error) {
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
	if left.SchemaVersion != right.SchemaVersion || left.Root != right.Root || left.Digest != right.Digest ||
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
	if lock.SchemaVersion != schemaVersion {
		return fmt.Errorf("package lock schema version %d is unsupported", lock.SchemaVersion)
	}
	if lock.Digest == "" || lock.Digest != PackageLockDigest(lock) {
		return fmt.Errorf("package lock digest is invalid")
	}
	if lock.Root.Namespace != "odjit" || lock.Root.Name != "KindredCommands" {
		return fmt.Errorf("package lock root must be odjit/KindredCommands")
	}
	if len(lock.Packages) != 3 || len(archives) != len(lock.Packages) {
		return fmt.Errorf("managed package lock must contain exactly BepInEx, VampireCommandFramework, and KindredCommands")
	}
	seen := make(map[string]PackageRef, len(lock.Packages))
	for _, locked := range lock.Packages {
		kind := managedPackageKind(locked.Ref)
		if kind == "" {
			return fmt.Errorf("locked package %s has no managed mapping", packageVersionFullName(locked.Ref))
		}
		if _, exists := seen[kind]; exists {
			return fmt.Errorf("managed package lock contains duplicate %s package", kind)
		}
		seen[kind] = locked.Ref
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
	for _, kind := range []string{"bepinex", "vcf", "kindred"} {
		if _, ok := seen[kind]; !ok {
			return fmt.Errorf("managed package lock is missing %s", kind)
		}
	}
	if seen["kindred"] != lock.Root {
		return fmt.Errorf("package lock root does not match locked KindredCommands")
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
	} {
		if _, ok := paths[required]; !ok {
			return fmt.Errorf("staged generation is missing required managed file %s", required)
		}
	}
	for prefix, label := range map[string]string{
		"dotnet/":           "dotnet runtime",
		"BepInEx/core/":     "BepInEx core",
		"BepInEx/patchers/": "BepInEx patchers",
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
