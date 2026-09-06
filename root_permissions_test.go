package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestManagedFileModePolicy(t *testing.T) {
	for _, staged := range []bool{false, true} {
		fixture := newLiveInstallationFixture(t)
		file := fixture.manifest.Files[0]
		path := liveManagedPath(fixture, file)
		if staged {
			path = filepath.Join(fixture.config.StateDir, "generations", fixture.staged.Record.ID, overlayDirectory, file.RelativePath)
		}
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
		err := ValidateLiveInstallation(t.Context(), fixture.config)
		if os.Geteuid() == 0 && err != nil {
			t.Fatalf("root rejected mode drift (staged=%t): %v", staged, err)
		}
		if os.Geteuid() != 0 && err == nil {
			t.Fatalf("non-root accepted mode drift (staged=%t)", staged)
		}
	}
}

func TestRootReadsLegacyPrivateNamespaces(t *testing.T) {
	requireRoot(t)
	for _, mode := range []os.FileMode{0o700, 0o755, 0o777} {
		fixture := newLiveInstallationFixture(t)
		if err := filepath.Walk(fixture.config.StateDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if err := os.Lchown(path, testRuntimeUID, testRuntimeGID); err != nil {
				return err
			}
			if info.IsDir() {
				return os.Chmod(path, mode)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if code := verifyCommand(&output, fixture.config); code != 0 {
			t.Fatalf("root verification rejected legacy metadata: %s", &output)
		}
		fd, err := unix.Open(fixture.config.StateDir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range []func(int) error{validateArchiveCacheRoot, func(fd int) error {
			return validatePrivateBackupDirectory(fd, "legacy backups")
		}} {
			if err := check(fd); err != nil {
				t.Errorf("root rejected legacy namespace: %v", err)
			}
		}
		unix.Close(fd)
		assertPathOwner(t, fixture.config.StateDir, testRuntimeUID, testRuntimeGID)
	}
}

func TestRootRejectsDockerUserOverride(t *testing.T) {
	if os.Getenv("VRISING_TEST_NONROOT_START") == "1" {
		var output bytes.Buffer
		if code := runCommand(&output, Config{}); code != exitPreflight || !strings.Contains(output.String(), "remove Docker user/--user") {
			t.Fatalf("non-root bootstrap was not rejected first: code=%d output=%q", code, &output)
		}
		return
	}
	requireRoot(t)
	// Give the non-root subprocess access to a disposable copy of the test
	// executable without altering permissions on Go's own build directory.
	dir, err := os.MkdirTemp("", "vrising-root-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-test.run=^(TestRootRejectsDockerUserOverride|TestStoreRejectsPermissiveStateDirectory|TestRecoverRejectsPermissiveTransactionArtifactNamespace|TestArchiveCacheRejectsPermissiveCacheRoot|TestBackupRequiresPrivateRuntimeStateDirectory|TestPromotionFinalSchemaValidatesBeforeAnyWrite|TestManagedFileModePolicy)$")
	cmd.Env = append(os.Environ(), "VRISING_TEST_NONROOT_START=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: testRuntimeUID, Gid: testRuntimeGID}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("non-root startup probe: %v\n%s", err, output)
	}
}

func TestRootPreparationHoldsLifetimeLock(t *testing.T) {
	fixture := newRunFixture(t)
	fixture.app.prepareRuntime = func() error {
		if !fixture.store.held {
			t.Fatal("permission repair ran without the lifetime lock")
		}
		fixture.recorder.add("repair")
		return nil
	}
	if err := fixture.app.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertEventBefore(t, fixture.recorder.events, "lock", "repair")
	assertEventBefore(t, fixture.recorder.events, "repair", "recover")
	fixture.store.held = true
	fixture.app.prepareRuntime = func() error {
		t.Fatal("competing startup changed permissions")
		return nil
	}
	if err := fixture.app.Run(t.Context()); err == nil {
		t.Fatal("competing startup acquired an occupied lock")
	}
}

func TestRootCompetingStartupLeavesPermissionsUntouched(t *testing.T) {
	requireRoot(t)
	cfg := newIdentityTestConfig(t)
	store := &Store{StateDir: cfg.StateDir}
	lock, err := store.OpenLifetimeLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	chownTestPath(t, cfg.StateDir, testRuntimeUID, testRuntimeGID)
	if err := os.Chmod(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := runCommand(&output, cfg); code != exitPreflight || !strings.Contains(output.String(), "acquire lifetime lock") {
		t.Fatalf("competing startup: code=%d output=%s", code, &output)
	}
	assertPathOwner(t, cfg.StateDir, testRuntimeUID, testRuntimeGID)
	info, err := os.Stat(cfg.StateDir)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("competing startup changed state-directory permissions: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cfg.StateDir, "wineprefix")); !os.IsNotExist(err) {
		t.Fatalf("competing startup prepared Wine prefix: %v", err)
	}
}

func TestRootSharedSavesRejectDifferentServerMount(t *testing.T) {
	requireRoot(t)
	first := newIdentityTestConfig(t)
	second := newIdentityTestConfig(t)
	second.DataDir = first.DataDir
	store := &Store{StateDir: first.StateDir, DataDir: first.DataDir}
	lock, err := store.OpenLifetimeLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	chownTestPath(t, first.DataDir, testRuntimeUID, testRuntimeGID)
	second.PUID, second.PGID = intPointer(testRuntimeUID+1), intPointer(testRuntimeGID+1)
	var output bytes.Buffer
	if code := runCommand(&output, second); code != exitPreflight || !strings.Contains(output.String(), "persistent-data lifetime lock") {
		t.Fatalf("shared-save startup: code=%d output=%s", code, &output)
	}
	assertPathOwner(t, first.DataDir, testRuntimeUID, testRuntimeGID)
	if _, err := os.Lstat(filepath.Join(second.StateDir, "wineprefix")); !os.IsNotExist(err) {
		t.Fatalf("shared-save startup prepared Wine: %v", err)
	}
}
