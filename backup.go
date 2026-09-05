package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	backupNamePrefix = "vrising-backup-"
	backupNameSuffix = ".tar.gz"
	backupTimeLayout = "20060102T150405.000000000Z"
)

type BackupRequest struct {
	InstalledBuild string
	TargetBuild    string
	PackageDigest  string
}

type BackupRecord struct {
	Path      string
	SHA256    string
	Size      int64
	CreatedAt time.Time
}

type BackupManager struct {
	ServerDir string
	DataDir   string
	BackupDir string
	Now       func() time.Time

	availableSpace           func(int) (uint64, error)
	syncDirectory            func(int) error
	beforeVerification       func(int, string) error
	afterPublication         func(int, string) error
	beforePruneDelete        func(int, string) error
	afterTemporaryCreate     func()
	beforeVerificationMember func(string)
}

type backupManifest struct {
	InstalledBuild string    `json:"installed_build"`
	TargetBuild    string    `json:"target_build"`
	PackageDigest  string    `json:"package_digest"`
	CreatedAt      time.Time `json:"created_at"`
	Members        []string  `json:"members"`
}

type backupSourceEntry struct {
	root        int
	sourcePath  string
	archivePath string
	stat        unix.Stat_t
	isDirectory bool
	payloadHash string
}

type backupSource struct {
	root        int
	sourcePath  string
	archivePath string
}

type backupExpectedMember struct {
	name        string
	typeflag    byte
	size        int64
	body        []byte
	payloadHash string
}

type completedBackup struct {
	name      string
	createdAt time.Time
	stat      unix.Stat_t
	sha256    string
}

func (m *BackupManager) Create(ctx context.Context, request BackupRequest) (BackupRecord, error) {
	if err := ctx.Err(); err != nil {
		return BackupRecord{}, err
	}
	backupRoot, err := m.openBackupDirectory(true)
	if err != nil {
		return BackupRecord{}, err
	}
	defer unix.Close(backupRoot)

	dataRoot, err := openDirectoryPath(m.DataDir, false)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("open persistent data root: %w", err)
	}
	defer unix.Close(dataRoot)
	serverRoot, err := openDirectoryPath(m.ServerDir, false)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("open server root: %w", err)
	}
	defer unix.Close(serverRoot)

	sources := []backupSource{
		{root: dataRoot, sourcePath: "Settings", archivePath: "persistentdata/Settings"},
		{root: dataRoot, sourcePath: "Saves", archivePath: "persistentdata/Saves"},
		{root: serverRoot, sourcePath: "BepInEx/config", archivePath: "server/BepInEx/config"},
	}
	var entries []backupSourceEntry
	var sourceBytes uint64
	for _, source := range sources {
		if err := collectBackupSource(ctx, source.root, source.sourcePath, source.archivePath, &entries, &sourceBytes); err != nil {
			return BackupRecord{}, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].archivePath < entries[j].archivePath })
	if err := m.requireSpace(backupRoot, sourceBytes); err != nil {
		return BackupRecord{}, err
	}
	if err := ctx.Err(); err != nil {
		return BackupRecord{}, err
	}

	createdAt := time.Now().UTC()
	if m.Now != nil {
		createdAt = m.Now().UTC()
	}
	memberNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		memberNames = append(memberNames, entry.archivePath)
	}
	manifest := backupManifest{
		InstalledBuild: request.InstalledBuild,
		TargetBuild:    request.TargetBuild,
		PackageDigest:  request.PackageDigest,
		CreatedAt:      createdAt,
		Members:        memberNames,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("encode backup manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	finalName := backupNamePrefix + createdAt.Format(backupTimeLayout) + backupNameSuffix
	temporaryName, temporaryFD, err := createArchiveTemporaryFileAt(backupRoot, finalName)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("create backup temporary file: %w", err)
	}
	temporary := os.NewFile(uintptr(temporaryFD), temporaryName)
	defer temporary.Close()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_, _ = unlinkBackupNameIfIdentity(backupRoot, temporaryName, temporary)
		}
	}()
	if m.afterTemporaryCreate != nil {
		m.afterTemporaryCreate()
	}
	if err := ctx.Err(); err != nil {
		return BackupRecord{}, err
	}
	if err := writeBackupArchive(ctx, temporary, createdAt, manifestBytes, entries); err != nil {
		return BackupRecord{}, err
	}
	if err := temporary.Sync(); err != nil {
		return BackupRecord{}, fmt.Errorf("sync backup temporary file: %w", err)
	}

	if m.beforeVerification != nil {
		if err := m.beforeVerification(backupRoot, temporaryName); err != nil {
			return BackupRecord{}, fmt.Errorf("before backup verification: %w", err)
		}
	}
	verified, err := openBackupRegularAt(backupRoot, temporaryName)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("reopen backup for verification: %w", err)
	}
	defer verified.Close()
	if err := verifyBackupDescriptorIdentity(temporary, verified); err != nil {
		return BackupRecord{}, fmt.Errorf("verify reopened backup identity: %w", err)
	}
	expected := expectedBackupMembers(manifestBytes, entries)
	digest, size, err := verifyBackupArchiveWithHook(ctx, verified, expected, m.beforeVerificationMember)
	if err != nil {
		return BackupRecord{}, fmt.Errorf("verify backup archive: %w", err)
	}
	if err := validateBackupSourceSnapshot(ctx, sources, entries, sourceBytes); err != nil {
		return BackupRecord{}, err
	}
	if err := verifyBackupDescriptorIdentity(temporary, verified); err != nil {
		return BackupRecord{}, fmt.Errorf("recheck backup descriptor identity: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return BackupRecord{}, err
	}
	if err := verifyNamedArchiveIdentity(backupRoot, temporaryName, verified); err != nil {
		return BackupRecord{}, fmt.Errorf("verify backup temporary identity: %w", err)
	}
	if err := unix.Renameat2(backupRoot, temporaryName, backupRoot, finalName, unix.RENAME_NOREPLACE); err != nil {
		return BackupRecord{}, fmt.Errorf("publish backup archive: %w", err)
	}
	removeTemporary = false
	if m.afterPublication != nil {
		if err := m.afterPublication(backupRoot, finalName); err != nil {
			cleanupErr := m.removePublishedBackupIfIdentity(backupRoot, finalName, verified)
			return BackupRecord{}, errors.Join(fmt.Errorf("after backup publication: %w", err), cleanupErr)
		}
	}
	if err := verifyNamedArchiveIdentity(backupRoot, finalName, verified); err != nil {
		cleanupErr := m.removePublishedBackupIfIdentity(backupRoot, finalName, verified)
		return BackupRecord{}, errors.Join(fmt.Errorf("verify published backup identity: %w", err), cleanupErr)
	}
	if err := m.syncBackupDirectory(backupRoot); err != nil {
		cleanupErr := m.removePublishedBackupIfIdentity(backupRoot, finalName, verified)
		return BackupRecord{}, errors.Join(fmt.Errorf("sync backup directory: %w", err), cleanupErr)
	}

	return BackupRecord{
		Path:      filepath.Join(filepath.Clean(m.BackupDir), finalName),
		SHA256:    digest,
		Size:      size,
		CreatedAt: createdAt,
	}, nil
}

func (m *BackupManager) Prune(retain int) (retErr error) {
	if retain < 1 {
		return fmt.Errorf("backup retention must be at least one")
	}
	root, err := m.openBackupDirectory(false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(root)
	deleted := false
	defer func() {
		if !deleted {
			return
		}
		if err := m.syncBackupDirectory(root); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("sync pruned backup directory: %w", err))
		}
	}()
	names, err := backupDirectoryNames(root)
	if err != nil {
		return fmt.Errorf("list backup directory: %w", err)
	}
	backups := make([]completedBackup, 0, len(names))
	for _, name := range names {
		createdAt, ok := completedBackupTime(name)
		if !ok {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(root, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("inspect completed backup: %w", err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("recognized backup entry is not a regular file")
		}
		file, err := openBackupRegularAt(root, name)
		if err != nil {
			return fmt.Errorf("open completed backup for pruning preflight: %w", err)
		}
		digest, opened, hashErr := hashStableBackupFile(file)
		closeErr := file.Close()
		if hashErr != nil || closeErr != nil {
			if hashErr != nil {
				hashErr = fmt.Errorf("hash completed backup for pruning preflight: %w", hashErr)
			}
			return errors.Join(hashErr, closeErr)
		}
		if !sameBackupSnapshot(opened, stat) {
			return fmt.Errorf("completed backup changed during pruning preflight")
		}
		backups = append(backups, completedBackup{name: name, createdAt: createdAt, stat: stat, sha256: digest})
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].createdAt.Equal(backups[j].createdAt) {
			return backups[i].name > backups[j].name
		}
		return backups[i].createdAt.After(backups[j].createdAt)
	})
	if len(backups) <= retain {
		return nil
	}
	for _, backup := range backups[retain:] {
		if m.beforePruneDelete != nil {
			if err := m.beforePruneDelete(root, backup.name); err != nil {
				return fmt.Errorf("before pruning completed backup: %w", err)
			}
		}
		file, err := openBackupRegularAt(root, backup.name)
		if err != nil {
			return fmt.Errorf("reopen completed backup for pruning: %w", err)
		}
		digest, held, err := hashStableBackupFile(file)
		if err != nil {
			file.Close()
			return fmt.Errorf("rehash completed backup for pruning: %w", err)
		}
		if !sameBackupSnapshot(held, backup.stat) || digest != backup.sha256 {
			file.Close()
			return fmt.Errorf("completed backup content or identity changed before pruning")
		}
		if err := verifyNamedArchiveIdentity(root, backup.name, file); err != nil {
			file.Close()
			return fmt.Errorf("verify completed backup before pruning: %w", err)
		}
		if err := unix.Unlinkat(root, backup.name, 0); err != nil {
			file.Close()
			return fmt.Errorf("prune completed backup: %w", err)
		}
		deleted = true
		if err := file.Close(); err != nil {
			return fmt.Errorf("close pruned backup: %w", err)
		}
	}
	return nil
}

func (m *BackupManager) openBackupDirectory(create bool) (int, error) {
	if !filepath.IsAbs(m.BackupDir) || filepath.Clean(m.BackupDir) != m.BackupDir {
		return -1, fmt.Errorf("backup directory must be a clean absolute path")
	}
	parentPath, name := filepath.Split(m.BackupDir)
	parentPath = filepath.Clean(parentPath)
	if name == "" || name == "." || name == ".." {
		return -1, fmt.Errorf("backup directory name is invalid")
	}
	parent, err := openDirectoryPath(parentPath, false)
	if err != nil {
		return -1, fmt.Errorf("open backup state directory: %w", err)
	}
	defer unix.Close(parent)
	if err := validatePrivateBackupDirectory(parent, "backup state directory"); err != nil {
		return -1, err
	}
	root, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == unix.ENOENT && create {
		if err := unix.Mkdirat(parent, name, 0o700); err != nil && err != unix.EEXIST {
			return -1, fmt.Errorf("create backup directory: %w", err)
		}
		if err := m.syncBackupDirectory(parent); err != nil {
			return -1, fmt.Errorf("sync backup state directory: %w", err)
		}
		root, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return -1, err
	}
	if err := validatePrivateBackupDirectory(root, "backup directory"); err != nil {
		unix.Close(root)
		return -1, err
	}
	return root, nil
}

func validatePrivateBackupDirectory(fd int, label string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o7777 != 0o700 {
		return fmt.Errorf("%s must be runtime-owned mode 0700", label)
	}
	return nil
}

func collectBackupSource(ctx context.Context, root int, sourcePath, archivePath string, entries *[]backupSourceEntry, sourceBytes *uint64) error {
	directory, err := openBackupDirectoryAt(root, sourcePath)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open selected backup source: %w", err)
	}
	return collectBackupDirectory(ctx, root, directory, sourcePath, archivePath, entries, sourceBytes)
}

func collectBackupDirectory(ctx context.Context, root, directory int, sourcePath, archivePath string, entries *[]backupSourceEntry, sourceBytes *uint64) error {
	file := os.NewFile(uintptr(directory), archivePath)
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	var directoryStat unix.Stat_t
	if err := unix.Fstat(directory, &directoryStat); err != nil {
		return fmt.Errorf("inspect selected backup directory: %w", err)
	}
	if directoryStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("selected backup source is not a directory")
	}
	*entries = append(*entries, backupSourceEntry{
		root: root, sourcePath: sourcePath, archivePath: archivePath,
		stat: directoryStat, isDirectory: true,
	})
	names, err := file.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("list selected backup directory: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := relativePathParts(name); err != nil {
			return fmt.Errorf("selected backup entry has an unsafe name")
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(directory, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("inspect selected backup entry: %w", err)
		}
		childSourcePath := path.Join(sourcePath, name)
		childArchivePath := path.Join(archivePath, name)
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			child, err := unix.Openat(directory, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return fmt.Errorf("open selected backup directory entry: %w", err)
			}
			var opened unix.Stat_t
			if err := unix.Fstat(child, &opened); err != nil {
				unix.Close(child)
				return fmt.Errorf("inspect opened backup directory entry: %w", err)
			}
			if !sameBackupIdentity(opened, stat) {
				unix.Close(child)
				return fmt.Errorf("selected backup directory changed during inspection")
			}
			if err := collectBackupDirectory(ctx, root, child, childSourcePath, childArchivePath, entries, sourceBytes); err != nil {
				return err
			}
		case unix.S_IFREG:
			if stat.Size < 0 || uint64(stat.Size) > math.MaxUint64-*sourceBytes {
				return fmt.Errorf("selected backup source size overflows")
			}
			*sourceBytes += uint64(stat.Size)
			*entries = append(*entries, backupSourceEntry{
				root: root, sourcePath: childSourcePath, archivePath: childArchivePath, stat: stat,
			})
		default:
			return fmt.Errorf("selected backup source contains a non-regular entry")
		}
	}
	return nil
}

func validateBackupSourceSnapshot(ctx context.Context, sources []backupSource, expected []backupSourceEntry, expectedBytes uint64) error {
	var current []backupSourceEntry
	var currentBytes uint64
	for _, source := range sources {
		if err := collectBackupSource(ctx, source.root, source.sourcePath, source.archivePath, &current, &currentBytes); err != nil {
			return fmt.Errorf("revalidate selected backup sources: %w", err)
		}
	}
	sort.Slice(current, func(i, j int) bool { return current[i].archivePath < current[j].archivePath })
	if currentBytes != expectedBytes || len(current) != len(expected) {
		return fmt.Errorf("selected backup sources changed after streaming")
	}
	for index := range expected {
		want := expected[index]
		got := current[index]
		if got.root != want.root || got.sourcePath != want.sourcePath || got.archivePath != want.archivePath ||
			got.isDirectory != want.isDirectory || !sameBackupSnapshot(got.stat, want.stat) {
			return fmt.Errorf("selected backup sources changed after streaming")
		}
		if got.isDirectory {
			continue
		}
		digest, err := hashBackupSourceEntry(ctx, got)
		if err != nil {
			return fmt.Errorf("rehash selected backup source: %w", err)
		}
		if digest != want.payloadHash {
			return fmt.Errorf("selected backup source payload changed after streaming")
		}
	}
	return nil
}

func hashBackupSourceEntry(ctx context.Context, entry backupSourceEntry) (string, error) {
	parent, name, err := openRelativeParent(entry.root, entry.sourcePath, false, nil)
	if err != nil {
		return "", err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), entry.archivePath)
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return "", err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || !sameBackupSnapshot(before, entry.stat) {
		return "", fmt.Errorf("selected backup file changed before rehash")
	}
	hasher := sha256.New()
	if _, err := io.CopyN(hasher, contextReader{ctx: ctx, reader: file}, before.Size); err != nil {
		return "", err
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return "", err
	}
	if !sameBackupSnapshot(after, before) {
		return "", fmt.Errorf("selected backup file changed while rehashing")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func openBackupDirectoryAt(root int, relativePath string) (int, error) {
	parent, name, err := openRelativeParent(root, relativePath, false, nil)
	if err != nil {
		return -1, err
	}
	defer unix.Close(parent)
	return unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}

func (m *BackupManager) requireSpace(root int, sourceBytes uint64) error {
	if sourceBytes > math.MaxUint64-archiveSafetyReserve {
		return fmt.Errorf("insufficient space in backup directory")
	}
	if m.availableSpace == nil {
		return requireFilesystemSpace(root, sourceBytes, "backup directory")
	}
	available, err := m.availableSpace(root)
	if err != nil {
		return fmt.Errorf("inspect free space in backup directory: %w", err)
	}
	needed := sourceBytes + archiveSafetyReserve
	if available < needed {
		return fmt.Errorf("insufficient space in backup directory: have %d bytes, need %d", available, needed)
	}
	return nil
}

func writeBackupArchive(ctx context.Context, target *os.File, createdAt time.Time, manifest []byte, entries []backupSourceEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gz := gzip.NewWriter(target)
	gz.Header.ModTime = createdAt
	gz.Header.OS = 255
	tarWriter := tar.NewWriter(gz)
	manifestHeader := &tar.Header{
		Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg,
		ModTime: createdAt, Format: tar.FormatPAX,
	}
	if err := tarWriter.WriteHeader(manifestHeader); err != nil {
		return fmt.Errorf("write backup manifest header: %w", err)
	}
	if _, err := tarWriter.Write(manifest); err != nil {
		return fmt.Errorf("write backup manifest: %w", err)
	}
	for index := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeBackupSourceEntry(ctx, tarWriter, &entries[index]); err != nil {
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("close backup tar stream: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close backup gzip stream: %w", err)
	}
	return nil
}

func writeBackupSourceEntry(ctx context.Context, writer *tar.Writer, entry *backupSourceEntry) error {
	header := &tar.Header{
		Name: entry.archivePath, Mode: int64(entry.stat.Mode & 0o7777),
		ModTime: time.Unix(entry.stat.Mtim.Sec, entry.stat.Mtim.Nsec).UTC(), Format: tar.FormatPAX,
	}
	if entry.isDirectory {
		directory, err := openBackupDirectoryAt(entry.root, entry.sourcePath)
		if err != nil {
			return fmt.Errorf("reopen selected backup directory: %w", err)
		}
		defer unix.Close(directory)
		var stat unix.Stat_t
		if err := unix.Fstat(directory, &stat); err != nil {
			return fmt.Errorf("inspect reopened backup directory: %w", err)
		}
		if !sameBackupSnapshot(stat, entry.stat) {
			return fmt.Errorf("selected backup directory changed before archiving")
		}
		header.Typeflag = tar.TypeDir
		return writer.WriteHeader(header)
	}

	parent, name, err := openRelativeParent(entry.root, entry.sourcePath, false, nil)
	if err != nil {
		return fmt.Errorf("reopen selected backup file parent: %w", err)
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("reopen selected backup file: %w", err)
	}
	file := os.NewFile(uintptr(fd), entry.archivePath)
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return fmt.Errorf("inspect reopened backup file: %w", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || !sameBackupSnapshot(before, entry.stat) {
		return fmt.Errorf("selected backup file changed before archiving")
	}
	header.Typeflag = tar.TypeReg
	header.Size = before.Size
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write backup file header: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(writer, hasher), contextReader{ctx: ctx, reader: file}, before.Size); err != nil {
		return fmt.Errorf("stream backup file: %w", err)
	}
	entry.payloadHash = hex.EncodeToString(hasher.Sum(nil))
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return fmt.Errorf("reinspect backup file: %w", err)
	}
	if !sameBackupSnapshot(after, before) {
		return fmt.Errorf("selected backup file changed while archiving")
	}
	return nil
}

func expectedBackupMembers(manifest []byte, entries []backupSourceEntry) []backupExpectedMember {
	expected := []backupExpectedMember{{name: "manifest.json", typeflag: tar.TypeReg, size: int64(len(manifest)), body: manifest}}
	for _, entry := range entries {
		typeflag := byte(tar.TypeReg)
		size := entry.stat.Size
		if entry.isDirectory {
			typeflag = tar.TypeDir
			size = 0
		}
		expected = append(expected, backupExpectedMember{
			name: entry.archivePath, typeflag: typeflag, size: size, payloadHash: entry.payloadHash,
		})
	}
	return expected
}

func verifyBackupArchive(ctx context.Context, file *os.File, expected []backupExpectedMember) (string, int64, error) {
	return verifyBackupArchiveWithHook(ctx, file, expected, nil)
}

func verifyBackupArchiveWithHook(ctx context.Context, file *os.File, expected []backupExpectedMember, beforeMember func(string)) (string, int64, error) {
	var before unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return "", 0, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", 0, fmt.Errorf("backup descriptor is not a regular file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, contextReader{ctx: ctx, reader: file})
	if err != nil {
		return "", 0, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	buffered := bufio.NewReader(contextReader{ctx: ctx, reader: file})
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return "", 0, err
	}
	gz.Multistream(false)
	tarReader := tar.NewReader(gz)
	for index, want := range expected {
		if err := ctx.Err(); err != nil {
			gz.Close()
			return "", 0, err
		}
		header, err := tarReader.Next()
		if err != nil {
			gz.Close()
			return "", 0, fmt.Errorf("read member %d: %w", index, err)
		}
		if err := validateBackupMemberName(header.Name); err != nil {
			gz.Close()
			return "", 0, err
		}
		if beforeMember != nil {
			beforeMember(header.Name)
		}
		if err := ctx.Err(); err != nil {
			gz.Close()
			return "", 0, err
		}
		if header.Name != want.name || header.Typeflag != want.typeflag || header.Size != want.size || header.Linkname != "" {
			gz.Close()
			return "", 0, fmt.Errorf("backup member list or type does not match manifest")
		}
		if want.body != nil {
			body, err := io.ReadAll(contextReader{ctx: ctx, reader: tarReader})
			if err != nil {
				gz.Close()
				return "", 0, err
			}
			if !bytes.Equal(body, want.body) {
				gz.Close()
				return "", 0, fmt.Errorf("backup manifest bytes do not match")
			}
			continue
		}
		payloadHasher := sha256.New()
		written, err := io.Copy(payloadHasher, contextReader{ctx: ctx, reader: tarReader})
		if err != nil || written != want.size {
			gz.Close()
			return "", 0, fmt.Errorf("read complete backup member: copied %d of %d bytes: %w", written, want.size, err)
		}
		if want.payloadHash != "" && hex.EncodeToString(payloadHasher.Sum(nil)) != want.payloadHash {
			gz.Close()
			return "", 0, fmt.Errorf("backup member payload digest does not match source")
		}
	}
	if header, err := tarReader.Next(); err != io.EOF {
		gz.Close()
		if err == nil {
			return "", 0, fmt.Errorf("unexpected backup member %q", header.Name)
		}
		return "", 0, err
	}
	trailingTarBytes, err := io.Copy(io.Discard, contextReader{ctx: ctx, reader: gz})
	if err != nil {
		gz.Close()
		return "", 0, err
	}
	if trailingTarBytes != 0 {
		gz.Close()
		return "", 0, fmt.Errorf("backup contains trailing tar payload")
	}
	if err := gz.Close(); err != nil {
		return "", 0, err
	}
	if _, err := buffered.ReadByte(); err != io.EOF {
		if err == nil {
			return "", 0, fmt.Errorf("backup contains trailing compressed bytes")
		}
		return "", 0, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return "", 0, err
	}
	if !sameBackupSnapshot(after, before) {
		return "", 0, fmt.Errorf("backup changed during verification")
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func validateBackupMemberName(name string) error {
	if name == "" || strings.Contains(name, `\`) || path.IsAbs(name) || path.Clean(name) != name {
		return fmt.Errorf("backup contains an unsafe member name")
	}
	if _, err := relativePathParts(name); err != nil {
		return fmt.Errorf("backup contains an unsafe member name")
	}
	return nil
}

func openBackupRegularAt(root int, name string) (*os.File, error) {
	if _, err := relativePathParts(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(root, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, fmt.Errorf("is not a regular file")
	}
	return os.NewFile(uintptr(fd), name), nil
}

func hashStableBackupFile(file *os.File) (string, unix.Stat_t, error) {
	var before unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return "", unix.Stat_t{}, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", unix.Stat_t{}, fmt.Errorf("is not a regular file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", unix.Stat_t{}, err
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", unix.Stat_t{}, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return "", unix.Stat_t{}, err
	}
	if !sameBackupSnapshot(after, before) {
		return "", unix.Stat_t{}, fmt.Errorf("file changed while hashing")
	}
	return hex.EncodeToString(hasher.Sum(nil)), after, nil
}

func verifyBackupDescriptorIdentity(left, right *os.File) error {
	var leftStat unix.Stat_t
	if err := unix.Fstat(int(left.Fd()), &leftStat); err != nil {
		return err
	}
	var rightStat unix.Stat_t
	if err := unix.Fstat(int(right.Fd()), &rightStat); err != nil {
		return err
	}
	if leftStat.Mode&unix.S_IFMT != unix.S_IFREG || rightStat.Mode&unix.S_IFMT != unix.S_IFREG ||
		!sameBackupIdentity(leftStat, rightStat) {
		return fmt.Errorf("backup descriptors identify different files")
	}
	return nil
}

func unlinkBackupNameIfIdentity(root int, name string, expected *os.File) (bool, error) {
	var held unix.Stat_t
	if err := unix.Fstat(int(expected.Fd()), &held); err != nil {
		return false, err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(root, name, &named, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if held.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&unix.S_IFMT != unix.S_IFREG || !sameBackupIdentity(held, named) {
		return false, nil
	}
	if err := unix.Unlinkat(root, name, 0); err != nil {
		return false, err
	}
	return true, nil
}

func (m *BackupManager) removePublishedBackupIfIdentity(root int, name string, expected *os.File) error {
	removed, err := unlinkBackupNameIfIdentity(root, name, expected)
	if err != nil || !removed {
		return err
	}
	if err := m.syncBackupDirectory(root); err != nil {
		return fmt.Errorf("sync backup directory after cleanup: %w", err)
	}
	return nil
}

func backupDirectoryNames(root int) ([]string, error) {
	copyFD, err := unix.Dup(root)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(copyFD), "backups")
	defer directory.Close()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func completedBackupTime(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, backupNamePrefix) || !strings.HasSuffix(name, backupNameSuffix) {
		return time.Time{}, false
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(name, backupNamePrefix), backupNameSuffix)
	createdAt, err := time.Parse(backupTimeLayout, encoded)
	if err != nil || backupNamePrefix+createdAt.Format(backupTimeLayout)+backupNameSuffix != name {
		return time.Time{}, false
	}
	return createdAt, true
}

func isCompletedBackupName(name string) bool {
	_, ok := completedBackupTime(name)
	return ok
}

func sameBackupIdentity(left, right unix.Stat_t) bool {
	return left.Mode&unix.S_IFMT == right.Mode&unix.S_IFMT && left.Dev == right.Dev && left.Ino == right.Ino
}

func sameBackupSnapshot(left, right unix.Stat_t) bool {
	return sameBackupIdentity(left, right) && left.Size == right.Size && left.Mode == right.Mode &&
		left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func (m *BackupManager) syncBackupDirectory(fd int) error {
	if m.syncDirectory != nil {
		return m.syncDirectory(fd)
	}
	return unix.Fsync(fd)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
