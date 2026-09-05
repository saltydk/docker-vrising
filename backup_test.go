package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type backupTestManifest struct {
	InstalledBuild string    `json:"installed_build"`
	TargetBuild    string    `json:"target_build"`
	PackageDigest  string    `json:"package_digest"`
	CreatedAt      time.Time `json:"created_at"`
	Members        []string  `json:"members"`
}

type backupTestEntry struct {
	name     string
	typeflag byte
	body     []byte
}

func TestBackupIncludesSavesSettingsListsAndModConfig(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Settings", "ServerGameSettings.json"), "settings")
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Settings", "adminlist.txt"), "admin")
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Settings", "banlist.txt"), "ban")
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "v4", "world.save"), "save")
	writeBackupTestFile(t, filepath.Join(manager.ServerDir, "BepInEx", "config", "KindredCommands.cfg"), "mod-config")

	request := BackupRequest{
		InstalledBuild: "100",
		TargetBuild:    "200",
		PackageDigest:  strings.Repeat("a", 64),
	}
	record, err := manager.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}

	entries := readBackupTestArchive(t, record.Path)
	wantNames := []string{
		"manifest.json",
		"persistentdata/Saves",
		"persistentdata/Saves/v4",
		"persistentdata/Saves/v4/world.save",
		"persistentdata/Settings",
		"persistentdata/Settings/ServerGameSettings.json",
		"persistentdata/Settings/adminlist.txt",
		"persistentdata/Settings/banlist.txt",
		"server/BepInEx/config",
		"server/BepInEx/config/KindredCommands.cfg",
	}
	if got := backupTestEntryNames(entries); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("archive members = %#v, want %#v", got, wantNames)
	}

	wantBodies := map[string]string{
		"persistentdata/Saves/v4/world.save":              "save",
		"persistentdata/Settings/ServerGameSettings.json": "settings",
		"persistentdata/Settings/adminlist.txt":           "admin",
		"persistentdata/Settings/banlist.txt":             "ban",
		"server/BepInEx/config/KindredCommands.cfg":       "mod-config",
	}
	for _, entry := range entries {
		if want, ok := wantBodies[entry.name]; ok && string(entry.body) != want {
			t.Fatalf("member %s body = %q, want %q", entry.name, entry.body, want)
		}
	}

	var manifest backupTestManifest
	if err := json.Unmarshal(entries[0].body, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.InstalledBuild != request.InstalledBuild ||
		manifest.TargetBuild != request.TargetBuild ||
		manifest.PackageDigest != request.PackageDigest ||
		!manifest.CreatedAt.Equal(record.CreatedAt) ||
		!reflect.DeepEqual(manifest.Members, wantNames[1:]) {
		t.Fatalf("manifest = %#v, want request, creation time, and exact source members", manifest)
	}
	if record.Size <= 0 {
		t.Fatalf("record size = %d, want positive", record.Size)
	}
	archiveBytes, err := os.ReadFile(record.Path)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(archiveBytes)
	if record.SHA256 != hex.EncodeToString(wantDigest[:]) || record.Size != int64(len(archiveBytes)) {
		t.Fatalf("record = %#v, want actual archive size and SHA-256", record)
	}
	if filepath.Dir(record.Path) != manager.BackupDir {
		t.Fatalf("record path = %q, want child of backup directory", record.Path)
	}
}

func TestBackupExcludesLogsGameFilesAndCache(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Settings", "ServerHostSettings.json"), "included")
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Logs", "server.log"), "excluded")
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "cache", "temporary.bin"), "excluded")
	writeBackupTestFile(t, filepath.Join(manager.ServerDir, "VRisingServer.exe"), "excluded")
	writeBackupTestFile(t, filepath.Join(manager.ServerDir, "BepInEx", "LogOutput.log"), "excluded")
	writeBackupTestFile(t, filepath.Join(manager.ServerDir, "BepInEx", "cache", "metadata"), "excluded")
	writeBackupTestFile(t, filepath.Join(manager.ServerDir, ".docker-vrising", "backups", "old.tar.gz"), "excluded")

	record, err := manager.Create(t.Context(), BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := backupTestEntryNames(readBackupTestArchive(t, record.Path))
	want := []string{
		"manifest.json",
		"persistentdata/Settings",
		"persistentdata/Settings/ServerHostSettings.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("archive members = %#v, want only selected persistent files %#v", got, want)
	}
}

func TestBackupDoesNotAppearUntilVerified(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	observed := false
	manager.beforeVerification = func(_ int, _ string) error {
		observed = true
		for _, name := range backupTestDirectoryNames(t, manager.BackupDir) {
			if isCompletedBackupName(name) {
				t.Fatalf("completed backup %q appeared before verification", name)
			}
		}
		return nil
	}

	record, err := manager.Create(t.Context(), BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("verification boundary was not observed")
	}
	if _, err := os.Stat(record.Path); err != nil {
		t.Fatalf("verified backup not published: %v", err)
	}
}

func TestBackupFailureLeavesExistingBackupsUntouched(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "original")
	first, err := manager.Create(t.Context(), BackupRequest{InstalledBuild: "100", TargetBuild: "200"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}

	manager.Now = func() time.Time { return time.Date(2026, 9, 5, 12, 1, 0, 0, time.UTC) }
	manager.beforeVerification = func(root int, name string) error {
		fd, err := unix.Openat(root, name, unix.O_WRONLY|unix.O_TRUNC|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), name)
		if _, err := file.Write([]byte("not a gzip archive")); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		return file.Close()
	}
	if _, err := manager.Create(t.Context(), BackupRequest{InstalledBuild: "200", TargetBuild: "300"}); err == nil {
		t.Fatal("Create() accepted a corrupted temporary archive")
	}

	after, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("read prior backup after failure: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("failed backup changed prior completed backup")
	}
	names := backupTestDirectoryNames(t, manager.BackupDir)
	if len(names) != 1 || names[0] != filepath.Base(first.Path) {
		t.Fatalf("backup directory after failure = %#v, want only prior backup", names)
	}
}

func TestBackupRejectsValidSameSizePayloadSubstitution(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "original")
	manager.beforeVerification = func(root int, name string) error {
		return rewriteBackupTestMember(root, name, "persistentdata/Saves/world.save", []byte("attacker"))
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil {
		t.Fatal("Create() accepted a valid archive whose payload changed at the same size")
	}
	assertNoCompletedBackup(t, manager.BackupDir)
}

func TestBackupRejectsTemporaryNameSubstitution(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	var replacementName string
	var heldName string
	manager.beforeVerification = func(root int, name string) error {
		replacementName = name
		heldName = name + ".held"
		if err := unix.Renameat(root, name, root, heldName); err != nil {
			return err
		}
		sourceFD, err := unix.Openat(root, heldName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		source := os.NewFile(uintptr(sourceFD), heldName)
		defer source.Close()
		replacementFD, err := unix.Openat(root, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		replacement := os.NewFile(uintptr(replacementFD), name)
		if _, err := io.Copy(replacement, source); err != nil {
			replacement.Close()
			return err
		}
		if err := replacement.Sync(); err != nil {
			replacement.Close()
			return err
		}
		return replacement.Close()
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil {
		t.Fatal("Create() accepted a substituted temporary backup name")
	}
	assertNoCompletedBackup(t, manager.BackupDir)
	for _, name := range []string{replacementName, heldName} {
		if _, err := os.Stat(filepath.Join(manager.BackupDir, name)); err != nil {
			t.Fatalf("substitution artifact %q was not preserved: %v", name, err)
		}
	}
}

func TestBackupRejectsNewSourceEntryAddedAfterStreaming(t *testing.T) {
	manager := newBackupTestManager(t)
	settings := filepath.Join(manager.DataDir, "Settings")
	writeBackupTestFile(t, filepath.Join(settings, "ServerGameSettings.json"), "settings")
	manager.beforeVerification = func(int, string) error {
		return os.WriteFile(filepath.Join(settings, "late.txt"), []byte("late"), 0o600)
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil {
		t.Fatal("Create() published a snapshot after a new selected source entry appeared")
	}
	assertNoCompletedBackup(t, manager.BackupDir)
}

func TestBackupRejectsInitiallyAbsentSourceAppearingAfterStreaming(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Settings", "ServerGameSettings.json"), "settings")
	manager.beforeVerification = func(int, string) error {
		writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "late.save"), "late")
		return nil
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil {
		t.Fatal("Create() published a snapshot after an initially absent selected root appeared")
	}
	assertNoCompletedBackup(t, manager.BackupDir)
}

func TestBackupPreservesUnexpectedFinalNameReplacement(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	const replacementBody = "operator replacement"
	var finalName string
	var heldName string
	manager.afterPublication = func(root int, name string) error {
		finalName = name
		heldName = name + ".held"
		if err := unix.Renameat(root, name, root, heldName); err != nil {
			return err
		}
		fd, err := unix.Openat(root, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), name)
		if _, err := file.Write([]byte(replacementBody)); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		return file.Close()
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil {
		t.Fatal("Create() accepted a replacement at the published backup name")
	}
	got, err := os.ReadFile(filepath.Join(manager.BackupDir, finalName))
	if err != nil {
		t.Fatalf("read unexpected final-name replacement: %v", err)
	}
	if string(got) != replacementBody {
		t.Fatalf("unexpected final-name replacement body = %q, want preserved operator bytes", got)
	}
	if _, err := os.Stat(filepath.Join(manager.BackupDir, heldName)); err != nil {
		t.Fatalf("verified backup displaced by replacement was not preserved: %v", err)
	}
}

func TestBackupPruneKeepsNewestThree(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	var records []BackupRecord
	for minute := range 5 {
		createdAt := time.Date(2026, 9, 5, 12, minute, 0, 0, time.UTC)
		manager.Now = func() time.Time { return createdAt }
		record, err := manager.Create(t.Context(), BackupRequest{TargetBuild: string(rune('0' + minute))})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	writeBackupTestFile(t, filepath.Join(manager.BackupDir, "operator-notes.txt"), "keep")
	writeBackupTestFile(t, filepath.Join(manager.BackupDir, ".backup-in-progress.tmp"), "keep")

	if err := manager.Prune(3); err != nil {
		t.Fatal(err)
	}
	want := []string{
		".backup-in-progress.tmp",
		filepath.Base(records[2].Path),
		filepath.Base(records[3].Path),
		filepath.Base(records[4].Path),
		"operator-notes.txt",
	}
	sort.Strings(want)
	if got := backupTestDirectoryNames(t, manager.BackupDir); !reflect.DeepEqual(got, want) {
		t.Fatalf("backup directory after prune = %#v, want %#v", got, want)
	}
}

func TestBackupPruneSyncsAfterPartialFailureAndJoinsErrors(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	var records []BackupRecord
	for minute := range 3 {
		createdAt := time.Date(2026, 9, 5, 12, minute, 0, 0, time.UTC)
		manager.Now = func() time.Time { return createdAt }
		record, err := manager.Create(t.Context(), BackupRequest{})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}

	pruneErr := errors.New("injected second prune failure")
	syncErr := errors.New("injected partial prune sync failure")
	deleteAttempt := 0
	manager.beforePruneDelete = func(int, string) error {
		deleteAttempt++
		if deleteAttempt == 2 {
			return pruneErr
		}
		return nil
	}
	var backupDirectory unix.Stat_t
	if err := unix.Stat(manager.BackupDir, &backupDirectory); err != nil {
		t.Fatal(err)
	}
	manager.syncDirectory = func(fd int) error {
		var current unix.Stat_t
		if err := unix.Fstat(fd, &current); err != nil {
			return err
		}
		if current.Dev == backupDirectory.Dev && current.Ino == backupDirectory.Ino {
			return syncErr
		}
		return unix.Fsync(fd)
	}

	err := manager.Prune(1)
	if !errors.Is(err, pruneErr) || !errors.Is(err, syncErr) {
		t.Fatalf("Prune() error = %v, want operation and directory sync errors", err)
	}
	want := []string{filepath.Base(records[0].Path), filepath.Base(records[2].Path)}
	sort.Strings(want)
	if got := backupTestDirectoryNames(t, manager.BackupDir); !reflect.DeepEqual(got, want) {
		t.Fatalf("backup directory after partial prune = %#v, want first deletion durable and second preserved %#v", got, want)
	}
}

func TestBackupPrunePreservesBackupChangedBeforeUnlink(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	var records []BackupRecord
	for minute := range 2 {
		createdAt := time.Date(2026, 9, 5, 12, minute, 0, 0, time.UTC)
		manager.Now = func() time.Time { return createdAt }
		record, err := manager.Create(t.Context(), BackupRequest{})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	const replacement = "changed recognized backup"
	manager.beforePruneDelete = func(root int, name string) error {
		fd, err := unix.Openat(root, name, unix.O_WRONLY|unix.O_TRUNC|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), name)
		if _, err := file.Write([]byte(replacement)); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		return file.Close()
	}

	if err := manager.Prune(1); err == nil {
		t.Fatal("Prune() deleted a recognized backup whose content changed before unlink")
	}
	got, err := os.ReadFile(records[0].Path)
	if err != nil {
		t.Fatalf("read changed recognized backup: %v", err)
	}
	if string(got) != replacement {
		t.Fatalf("changed recognized backup body = %q, want preserved replacement", got)
	}
	if _, err := os.Stat(records[1].Path); err != nil {
		t.Fatalf("newest retained backup missing: %v", err)
	}
}

func TestBackupRejectsInsufficientSpace(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Settings", "one"), "abc")
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "two"), "de")
	manager.availableSpace = func(int) (uint64, error) {
		return uint64(512<<20) + 4, nil
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil || !strings.Contains(err.Error(), "insufficient space") {
		t.Fatalf("Create() error = %v, want insufficient space", err)
	}
	if got := backupTestDirectoryNames(t, manager.BackupDir); len(got) != 0 {
		t.Fatalf("backup directory after space rejection = %#v, want empty", got)
	}
}

func TestBackupDoesNotFollowSymlinks(t *testing.T) {
	tests := []struct {
		name  string
		plant func(*testing.T, BackupManager)
	}{
		{
			name: "file symlink",
			plant: func(t *testing.T, manager BackupManager) {
				outside := filepath.Join(t.TempDir(), "secret")
				writeBackupTestFile(t, outside, "secret")
				if err := os.MkdirAll(filepath.Join(manager.DataDir, "Settings"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(manager.DataDir, "Settings", "linked")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory symlink",
			plant: func(t *testing.T, manager BackupManager) {
				outside := t.TempDir()
				writeBackupTestFile(t, filepath.Join(outside, "secret"), "secret")
				if err := os.Symlink(outside, filepath.Join(manager.DataDir, "Saves")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non-regular socket",
			plant: func(t *testing.T, manager BackupManager) {
				directory := filepath.Join(manager.ServerDir, "BepInEx", "config")
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = unix.Close(fd) })
				if err := unix.Bind(fd, &unix.SockaddrUnix{Name: filepath.Join(directory, "control.sock")}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newBackupTestManager(t)
			tt.plant(t, manager)
			if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil {
				t.Fatal("Create() accepted a symlink or non-regular source entry")
			}
			if got := backupTestDirectoryNames(t, manager.BackupDir); len(got) != 0 {
				t.Fatalf("backup directory after unsafe source rejection = %#v, want empty", got)
			}
		})
	}
}

func TestBackupRejectsSymlinkAddedAfterSizeScan(t *testing.T) {
	manager := newBackupTestManager(t)
	settings := filepath.Join(manager.DataDir, "Settings")
	writeBackupTestFile(t, filepath.Join(settings, "ServerGameSettings.json"), "settings")
	outside := filepath.Join(t.TempDir(), "secret")
	writeBackupTestFile(t, outside, "secret")
	manager.availableSpace = func(int) (uint64, error) {
		if err := os.Symlink(outside, filepath.Join(settings, "late-link")); err != nil {
			return 0, err
		}
		return ^uint64(0), nil
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil {
		t.Fatal("Create() ignored a symlink inserted after the source size scan")
	}
	if got := backupTestDirectoryNames(t, manager.BackupDir); len(got) != 0 {
		t.Fatalf("backup directory after late symlink rejection = %#v, want empty", got)
	}
}

func TestBackupDirectorySyncFailureDoesNotPublish(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	if err := os.MkdirAll(manager.BackupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var backupDirectory unix.Stat_t
	if err := unix.Stat(manager.BackupDir, &backupDirectory); err != nil {
		t.Fatal(err)
	}
	manager.syncDirectory = func(fd int) error {
		var current unix.Stat_t
		if err := unix.Fstat(fd, &current); err != nil {
			return err
		}
		if current.Dev == backupDirectory.Dev && current.Ino == backupDirectory.Ino {
			return errors.New("injected backup directory sync failure")
		}
		return unix.Fsync(fd)
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil || !strings.Contains(err.Error(), "sync backup directory") {
		t.Fatalf("Create() error = %v, want backup directory sync failure", err)
	}
	if got := backupTestDirectoryNames(t, manager.BackupDir); len(got) != 0 {
		t.Fatalf("backup directory after publication sync failure = %#v, want empty", got)
	}
}

func TestBackupRequiresPrivateRuntimeStateDirectory(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	stateDir := filepath.Dir(manager.BackupDir)
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil || !strings.Contains(err.Error(), "runtime-owned mode 0700") {
		t.Fatalf("Create() error = %v, want private runtime state rejection", err)
	}
	if _, err := os.Stat(manager.BackupDir); !os.IsNotExist(err) {
		t.Fatalf("backup directory stat error = %v, want not exist", err)
	}
}

func TestBackupVerificationRejectsUnsafeMember(t *testing.T) {
	name := filepath.Join(t.TempDir(), "unsafe.tar.gz")
	file, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	body := []byte("escape")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}

	expected := []backupExpectedMember{{name: "../escape", typeflag: tar.TypeReg, size: int64(len(body))}}
	if _, _, err := verifyBackupArchive(t.Context(), file, expected); err == nil || !strings.Contains(err.Error(), "unsafe member") {
		t.Fatalf("verifyBackupArchive() error = %v, want unsafe member rejection", err)
	}
}

func TestBackupRejectsCancelledContextBeforeWriting(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := manager.Create(ctx, BackupRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create() error = %v, want context cancellation", err)
	}
	if got := backupTestDirectoryNames(t, manager.BackupDir); len(got) != 0 {
		t.Fatalf("backup directory after cancellation = %#v, want empty", got)
	}
}

func TestBackupCancellationAfterTemporaryCreationLeavesNoArchive(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), "save")
	ctx, cancel := context.WithCancel(t.Context())
	manager.afterTemporaryCreate = func() { cancel() }

	if _, err := manager.Create(ctx, BackupRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create() error = %v, want cancellation after temporary creation", err)
	}
	if got := backupTestDirectoryNames(t, manager.BackupDir); len(got) != 0 {
		t.Fatalf("backup directory after post-create cancellation = %#v, want empty", got)
	}
}

func TestBackupCancellationDuringFinalMemberVerificationLeavesNoArchive(t *testing.T) {
	manager := newBackupTestManager(t)
	writeBackupTestFile(t, filepath.Join(manager.DataDir, "Saves", "world.save"), strings.Repeat("save", 1024))
	ctx, cancel := context.WithCancel(t.Context())
	manager.beforeVerificationMember = func(name string) {
		if name == "persistentdata/Saves/world.save" {
			cancel()
		}
	}

	if _, err := manager.Create(ctx, BackupRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create() error = %v, want cancellation during final-member verification", err)
	}
	if got := backupTestDirectoryNames(t, manager.BackupDir); len(got) != 0 {
		t.Fatalf("backup directory after verification cancellation = %#v, want empty", got)
	}
}

func TestBackupRejectsSourceChangeDuringArchiveVerification(t *testing.T) {
	manager := newBackupTestManager(t)
	saves := filepath.Join(manager.DataDir, "Saves")
	writeBackupTestFile(t, filepath.Join(saves, "world.save"), "save")
	manager.beforeVerificationMember = func(name string) {
		if name == "persistentdata/Saves/world.save" {
			writeBackupTestFile(t, filepath.Join(saves, "late.save"), "late")
		}
	}

	if _, err := manager.Create(t.Context(), BackupRequest{}); err == nil {
		t.Fatal("Create() published after selected sources changed during archive verification")
	}
	assertNoCompletedBackup(t, manager.BackupDir)
}

func newBackupTestManager(t *testing.T) BackupManager {
	t.Helper()
	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	dataDir := filepath.Join(root, "persistentdata")
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	for _, directory := range []string{serverDir, dataDir, stateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return BackupManager{
		ServerDir: serverDir,
		DataDir:   dataDir,
		BackupDir: filepath.Join(stateDir, "backups"),
		Now: func() time.Time {
			return time.Date(2026, 9, 5, 12, 0, 0, 123456789, time.UTC)
		},
	}
}

func writeBackupTestFile(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readBackupTestArchive(t *testing.T, name string) []backupTestEntry {
	t.Helper()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var entries []backupTestEntry
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, backupTestEntry{name: header.Name, typeflag: header.Typeflag, body: body})
	}
	return entries
}

func rewriteBackupTestMember(root int, name, target string, replacement []byte) error {
	fd, err := unix.Openat(root, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	reader := tar.NewReader(gz)
	type storedEntry struct {
		header tar.Header
		body   []byte
	}
	var entries []storedEntry
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			gz.Close()
			return err
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			gz.Close()
			return err
		}
		if header.Name == target {
			if len(body) != len(replacement) {
				gz.Close()
				return fmt.Errorf("replacement size %d differs from member size %d", len(replacement), len(body))
			}
			body = append([]byte(nil), replacement...)
			found = true
		}
		entries = append(entries, storedEntry{header: *header, body: body})
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("member %q not found", target)
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gzWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzWriter)
	for _, entry := range entries {
		header := entry.header
		if err := tarWriter.WriteHeader(&header); err != nil {
			return err
		}
		if _, err := tarWriter.Write(entry.body); err != nil {
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gzWriter.Close(); err != nil {
		return err
	}
	return file.Sync()
}

func backupTestEntryNames(entries []backupTestEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.name)
	}
	return names
}

func backupTestDirectoryNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func assertNoCompletedBackup(t *testing.T, directory string) {
	t.Helper()
	for _, name := range backupTestDirectoryNames(t, directory) {
		if isCompletedBackupName(name) {
			t.Fatalf("completed backup %q exists after rejected snapshot", name)
		}
	}
}
