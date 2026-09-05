package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadinessRequiresServerBepInExVCFAndKindred(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	serverReady := readLogFixture(t, "server-ready.log")
	bepInExReady := readLogFixture(t, "bepinex-ready.log")

	// These complete markers belong to a prior run and must not satisfy readiness.
	writeLog(t, serverLog, serverReady)
	writeLog(t, bepInExLog, bepInExReady)

	output := newReadinessOutput()
	monitor := ReadinessMonitor{
		ServerLog:  serverLog,
		BepInExLog: bepInExLog,
		Output:     output,
		PollEvery:  2 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{RequireMods: true, KindredVersion: "2.5.8"})

	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)
	appendLog(t, serverLog, serverReady)
	appendLog(t, bepInExLog, bepInExReady)

	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	got := output.String()
	if !strings.Contains(got, "[server] ") || !strings.Contains(got, "[bepinex] ") {
		t.Fatalf("prefixed output = %q", got)
	}
}

func TestReadinessRejectsFatalBepInExOutput(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	writeLog(t, serverLog, "prior run\n")
	writeLog(t, bepInExLog, "prior run\n")

	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{RequireMods: true, KindredVersion: "2.5.8"})
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	appendLog(t, bepInExLog, readLogFixture(t, "bepinex-fatal.log"))

	err := <-result
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fatal") {
		t.Fatalf("Wait() error = %v, want fatal readiness error", err)
	}
}

func TestReadinessRequiresExpectedKindredVersion(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	writeLog(t, serverLog, "prior run\n")
	writeLog(t, bepInExLog, "prior run\n")

	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{RequireMods: true, KindredVersion: "2.5.9"})
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	appendLog(t, bepInExLog, readLogFixture(t, "bepinex-ready.log"))

	select {
	case err := <-result:
		t.Fatalf("Wait() returned for the wrong Kindred version: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	appendLog(t, bepInExLog, "[Info :KindredCommands] Plugin aa.odjit.KindredCommands version 2.5.9 is loaded!\n")
	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v; output = %q", err, output.String())
	}
}

func TestReadinessTimesOut(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := (ReadinessMonitor{
		ServerLog:  filepath.Join(root, "server.log"),
		BepInExLog: filepath.Join(root, "bepinex.log"),
		Output:     io.Discard,
		PollEvery:  2 * time.Millisecond,
	}).Wait(ctx, ExpectedReadiness{RequireMods: true, KindredVersion: "2.5.8"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() error = %v, want deadline exceeded", err)
	}
}

func TestReadinessClosesInitializedLogsWhenSetupFails(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "prior run\n")
	bepInExDirectory := filepath.Join(root, "bepinex-directory")
	if err := os.Mkdir(bepInExDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	before := countOpenPath(t, serverLog)
	err := (ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExDirectory}).Wait(
		t.Context(),
		ExpectedReadiness{RequireMods: true, KindredVersion: "2.5.8"},
	)
	if err == nil {
		t.Fatal("Wait() succeeded with a directory as the BepInEx log")
	}
	if after := countOpenPath(t, serverLog); after != before {
		t.Fatalf("open server log descriptors = %d, want %d after setup failure", after, before)
	}
}

func TestReadinessFollowsFilesCreatedAfterStartWithModsDisabled(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestReadinessHandlesTruncationAndRotation(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	writeLog(t, serverLog, strings.Repeat("stale server output\n", 256))
	writeLog(t, bepInExLog, strings.Repeat("stale BepInEx output\n", 256))

	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{RequireMods: true, KindredVersion: "2.5.8"})
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)

	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	appendLog(t, bepInExLog, "[Message: BepInEx] Chainloader startup complete\n")
	appendLog(t, bepInExLog, "Plugin gg.deca.VampireCommandFramework version 0.10.4 is loaded!\n")
	if err := os.Rename(bepInExLog, bepInExLog+".previous"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, bepInExLog, "Plugin aa.odjit.KindredCommands version 2.5.8 is loaded!\n")

	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v; output = %q", err, output.String())
	}
}

func TestReadinessDetectsTruncateAndRegrowPastPreviousOffset(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, strings.Repeat("x", 30)+"\n")
	follower, err := newLogFollower(serverLog, "server")
	if err != nil {
		t.Fatal(err)
	}
	defer follower.close()
	appendLog(t, serverLog, "follower probe\n")
	if lines, err := follower.readAvailable(); err != nil || len(lines) != 1 || lines[0] != "follower probe" {
		t.Fatalf("initial append = %q, %v", lines, err)
	}

	ready := strings.TrimSuffix(readLogFixture(t, "server-ready.log"), "\n")
	writeLog(t, serverLog, ready+"\n")
	lines, err := follower.readAvailable()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != ready {
		t.Fatalf("lines after truncate and regrow = %q, want complete current log line", lines)
	}
}

func TestPruneLogsDeletesOnlyOwnedExpiredServerLogs(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)
	recent := now.Add(-6 * 24 * time.Hour)

	for _, name := range []string{
		"20260828-1200-VRisingServer.log",
		"20260828-120000-VRisingServer.log",
	} {
		writeDatedLog(t, root, name, old)
	}
	for _, name := range []string{
		"20260901-1200-VRisingServer.log",
		"20261340-9999-VRisingServer.log",
		"20260828-1200-user.log",
		"BepInEx.log",
		"backup-20260828-1200-VRisingServer.log",
	} {
		writeDatedLog(t, root, name, recent)
	}
	external := filepath.Join(t.TempDir(), "external.log")
	writeLog(t, external, "preserve\n")
	symlink := filepath.Join(root, "20260828-1300-VRisingServer.log")
	if err := os.Symlink(external, symlink); err != nil {
		t.Fatal(err)
	}

	if err := PruneLogs(root, 7, now); err != nil {
		t.Fatalf("PruneLogs() error = %v", err)
	}
	for _, name := range []string{"20260828-1200-VRisingServer.log", "20260828-120000-VRisingServer.log"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("expired owned log %q stat error = %v, want not exist", name, err)
		}
	}
	for _, name := range []string{
		"20260901-1200-VRisingServer.log",
		"20261340-9999-VRisingServer.log",
		"20260828-1200-user.log",
		"BepInEx.log",
		"backup-20260828-1200-VRisingServer.log",
		"20260828-1300-VRisingServer.log",
	} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Errorf("preserved path %q stat error = %v", name, err)
		}
	}
	if got, err := os.ReadFile(external); err != nil || string(got) != "preserve\n" {
		t.Fatalf("external symlink target = %q, %v", got, err)
	}
}

func TestPruneLogsNeverTraversesSubdirectories(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-90 * 24 * time.Hour)
	for _, directory := range []string{"BepInEx", "backups", "Settings", "Saves", "user"} {
		path := filepath.Join(root, directory)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		writeDatedLog(t, path, "20260101-0000-VRisingServer.log", old)
	}
	ownedLookingDirectory := filepath.Join(root, "20260101-0000-VRisingServer.log")
	if err := os.Mkdir(ownedLookingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDatedLog(t, ownedLookingDirectory, "nested.log", old)

	if err := PruneLogs(root, 7, now); err != nil {
		t.Fatalf("PruneLogs() error = %v", err)
	}
	for _, path := range []string{
		filepath.Join(root, "BepInEx", "20260101-0000-VRisingServer.log"),
		filepath.Join(root, "backups", "20260101-0000-VRisingServer.log"),
		filepath.Join(root, "Settings", "20260101-0000-VRisingServer.log"),
		filepath.Join(root, "Saves", "20260101-0000-VRisingServer.log"),
		filepath.Join(root, "user", "20260101-0000-VRisingServer.log"),
		filepath.Join(ownedLookingDirectory, "nested.log"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("nested path %q stat error = %v", path, err)
		}
	}
}

type readinessOutput struct {
	mu          sync.Mutex
	buffer      bytes.Buffer
	server      chan struct{}
	bepinex     chan struct{}
	serverOnce  sync.Once
	bepinexOnce sync.Once
}

func newReadinessOutput() *readinessOutput {
	return &readinessOutput{server: make(chan struct{}), bepinex: make(chan struct{})}
}

func (o *readinessOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if bytes.Contains(p, []byte("[server] ")) {
		o.serverOnce.Do(func() { close(o.server) })
	}
	if bytes.Contains(p, []byte("[bepinex] ")) {
		o.bepinexOnce.Do(func() { close(o.bepinex) })
	}
	return o.buffer.Write(p)
}

func (o *readinessOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.String()
}

func waitForReadiness(monitor ReadinessMonitor, ctx context.Context, expected ExpectedReadiness) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- monitor.Wait(ctx, expected)
	}()
	return result
}

func appendUntilObserved(t *testing.T, path string, observed <-chan struct{}, result <-chan error) {
	t.Helper()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-observed:
			return
		case err := <-result:
			t.Fatalf("Wait() returned before follower observed %s: %v", path, err)
		case <-ticker.C:
			appendLog(t, path, "follower probe\n")
		case <-timeout.C:
			t.Fatalf("follower did not emit %s", path)
		}
	}
}

func readLogFixture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "logs", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendLog(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, content); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeDatedLog(t *testing.T, root, name string, modified time.Time) {
	t.Helper()
	path := filepath.Join(root, name)
	writeLog(t, path, "log\n")
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func countOpenPath(t *testing.T, path string) int {
	t.Helper()
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && target == want {
			count++
		}
	}
	return count
}
