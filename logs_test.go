package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadinessRequiresServerBepInExVCFKindredAndSatisvampory(t *testing.T) {
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
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.85"))

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
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.85"))
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	appendLog(t, bepInExLog, readLogFixture(t, "bepinex-fatal.log"))

	err := <-result
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fatal") {
		t.Fatalf("Wait() error = %v, want fatal readiness error", err)
	}
}

func TestReadinessFinalBarrierRejectsCrossFileFatalAfterReadyEvidence(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	writeLog(t, serverLog, "prior run\n")
	writeLog(t, bepInExLog, "prior run\n")

	observed := newReadinessOutput()
	output := &injectingReadinessOutput{
		Writer: observed,
		match:  "[Server] Startup Completed",
		inject: func() error {
			return appendLogError(bepInExLog, "[Fatal : BepInEx] startup aborted\n")
		},
	}
	monitor := ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.85"))
	appendUntilObserved(t, serverLog, observed.server, result)
	appendUntilObserved(t, bepInExLog, observed.bepinex, result)
	appendLog(t, bepInExLog, readLogFixture(t, "bepinex-ready.log"))
	waitForOutput(t, observed, "Plugin aa.odjit.KindredCommands version 2.5.8 is loaded!", result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))

	err := <-result
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fatal") {
		t.Fatalf("Wait() error = %v, want cross-file fatal from final drain; output = %q", err, observed.String())
	}
	if output.Err() != nil {
		t.Fatalf("fatal injection error = %v", output.Err())
	}
}

func TestReadinessRequiresTwoFreshQuietRoundsAfterBenignOutput(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "prior run\n")
	observed := newReadinessOutput()
	injectedAt := make(chan time.Time, 1)
	output := &injectingReadinessOutput{
		Writer: observed,
		match:  "[Server] Startup Completed",
		inject: func() error {
			injectedAt <- time.Now()
			return appendLogError(serverLog, "benign confirmation output\n")
		},
	}
	const pollEvery = 30 * time.Millisecond
	monitor := ReadinessMonitor{ServerLog: serverLog, Output: output, PollEvery: pollEvery}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, observed.server, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	started := <-injectedAt
	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < 2*pollEvery-10*time.Millisecond {
		t.Fatalf("quiet confirmation elapsed = %v, want at least two fresh poll intervals", elapsed)
	}
	if output.Err() != nil {
		t.Fatalf("benign injection error = %v", output.Err())
	}
}

func TestReadinessFinalBarrierDrainsBoundedBacklogBeforeReady(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	writeLog(t, serverLog, "prior run\n")
	writeLog(t, bepInExLog, "prior run\n")
	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.85"))
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	if err := os.Rename(bepInExLog, bepInExLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, bepInExLog,
		readLogFixture(t, "bepinex-ready.log")+
			strings.Repeat("bounded backlog line\n", 125000)+
			"[Fatal : BepInEx] trailing startup failure\n")

	err := <-result
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fatal") {
		t.Fatalf("Wait() error = %v, want fatal beyond bounded batches", err)
	}
}

func TestReadinessDrainsRotatedOldBacklogBeforeAdoptingReplacement(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	writeLog(t, serverLog, "prior run\n")
	writeLog(t, bepInExLog, "prior run\n")
	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.85"))
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	appendLog(t, bepInExLog,
		readLogFixture(t, "bepinex-ready.log")+
			strings.Repeat("old descriptor backlog\n", 125000)+
			"[Fatal : BepInEx] fatal at old descriptor tail\n")
	if err := os.Rename(bepInExLog, bepInExLog+".previous"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, bepInExLog, "new generation\n")

	err := <-result
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fatal") {
		t.Fatalf("Wait() error = %v, want fatal from rotated old backlog", err)
	}
}

func TestReadinessRevalidatesRetiringActiveAfterQueuedRead(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	activeLog := filepath.Join(root, "generation-A.log")
	writeLog(t, serverLog, "generation A\n")
	output := newReadinessOutput()
	detected := make(chan struct{})
	var detectOnce sync.Once
	var armed atomic.Bool
	var appendOnce sync.Once
	var appendErr error
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: 5 * time.Millisecond,
		testHooks: &readinessTestHooks{
			replacementDetected: func(string) { detectOnce.Do(func() { close(detected) }) },
			afterReadChunk: func(string) {
				if armed.Load() {
					appendOnce.Do(func() {
						appendErr = appendLogError(activeLog, "[Fatal : BepInEx] active A failed during B read\n")
					})
				}
			},
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	waitForOutput(t, output, "[Server] Startup Completed", result)
	if err := os.Rename(serverLog, activeLog); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, "")
	select {
	case <-detected:
	case err := <-result:
		t.Fatalf("Wait() returned before B detection: %v", err)
	case <-time.After(time.Second):
		t.Fatal("generation B was not detected")
	}
	armed.Store(true)
	appendLog(t, serverLog, "generation B adoption data\n")

	err := <-result
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fatal") {
		t.Fatalf("Wait() error = %v, want fatal appended to retiring A; output = %q", err, output.String())
	}
	if appendErr != nil {
		t.Fatalf("append A fatal error = %v", appendErr)
	}
}

func TestReadinessRevalidationClearsRetiringActiveEvidenceOnTruncate(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	activeLog := filepath.Join(root, "generation-A.log")
	writeLog(t, serverLog, "generation A\n")
	output := newReadinessOutput()
	detected := make(chan struct{})
	var detectOnce sync.Once
	var armed atomic.Bool
	var truncateOnce sync.Once
	var truncateErr error
	truncated := make(chan struct{})
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: 5 * time.Millisecond,
		testHooks: &readinessTestHooks{
			replacementDetected: func(string) { detectOnce.Do(func() { close(detected) }) },
			afterReadChunk: func(string) {
				if armed.Load() {
					truncateOnce.Do(func() {
						truncateErr = os.WriteFile(activeLog, []byte("truncated retiring A\n"), 0o600)
						close(truncated)
					})
				}
			},
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	waitForOutput(t, output, "[Server] Startup Completed", result)
	if err := os.Rename(serverLog, activeLog); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, "")
	select {
	case <-detected:
	case err := <-result:
		t.Fatalf("Wait() returned before B detection: %v", err)
	case <-time.After(time.Second):
		t.Fatal("generation B was not detected")
	}
	armed.Store(true)
	appendLog(t, serverLog, "generation B adoption data\n")
	select {
	case <-truncated:
	case err := <-result:
		t.Fatalf("Wait() returned before retiring A truncate hook: %v", err)
	case <-time.After(time.Second):
		t.Fatal("retiring A truncate hook did not run")
	}
	select {
	case err := <-result:
		t.Fatalf("Wait() retained readiness from truncated retiring A: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if truncateErr != nil {
		t.Fatalf("truncate A error = %v", truncateErr)
	}
	if err := os.Rename(serverLog, serverLog+".generation-B"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	if err := <-result; err != nil {
		t.Fatalf("Wait() error after generation C readiness = %v", err)
	}
}

func TestReadinessFinalBarrierRejectsServerFatalAfterModReadyEvidence(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	writeLog(t, serverLog, "prior run\n")
	writeLog(t, bepInExLog, "prior run\n")
	observed := newReadinessOutput()
	output := &injectingReadinessOutput{
		Writer: observed,
		match:  "Plugin aa.odjit.KindredCommands version 2.5.8 is loaded!",
		inject: func() error {
			return appendLogError(serverLog, "[Fatal : Doorstop] server-side startup failure\n")
		},
	}
	monitor := ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.85"))
	appendUntilObserved(t, serverLog, observed.server, result)
	appendUntilObserved(t, bepInExLog, observed.bepinex, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	waitForOutput(t, observed, "[Server] Startup Completed", result)
	appendLog(t, bepInExLog, readLogFixture(t, "bepinex-ready.log"))

	err := <-result
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fatal") {
		t.Fatalf("Wait() error = %v, want server fatal from final drain", err)
	}
	if output.Err() != nil {
		t.Fatalf("fatal injection error = %v", output.Err())
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
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.9", "1.0.85"))
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

func TestReadinessRequiresExpectedSatisvamporyVersion(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	writeLog(t, serverLog, "prior run\n")
	writeLog(t, bepInExLog, "prior run\n")

	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, BepInExLog: bepInExLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.86"))
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	appendLog(t, bepInExLog, readLogFixture(t, "bepinex-ready.log"))

	select {
	case err := <-result:
		t.Fatalf("Wait() returned for the wrong Satisvampory version: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	appendLog(t, bepInExLog, "[Info :Satisvampory] Satisvampory 1.0.86 (Satisvampory) ready.\n")
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
	}).Wait(ctx, expectedModReadiness("2.5.8", "1.0.85"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() error = %v, want deadline exceeded", err)
	}
}

func TestReadinessRejectsOversizedUnterminatedLine(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "prior run\n")
	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, strings.Repeat("x", (1<<20)+1))

	err := <-result
	if err == nil || !strings.Contains(err.Error(), "line exceeds") {
		t.Fatalf("Wait() error = %v, want oversized line rejection", err)
	}
}

func TestReadinessChecksCancellationBetweenBoundedChunks(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "prior run\n")
	output := newReadinessOutput()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var armed atomic.Bool
	var chunks atomic.Int32
	var largest atomic.Int64
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: 2 * time.Millisecond,
		testHooks: &readinessTestHooks{readChunk: func(_ string, size int) {
			if !armed.Load() {
				return
			}
			chunks.Add(1)
			largest.Store(max(largest.Load(), int64(size)))
			cancel()
		}},
	}
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	writeLog(t, serverLog, strings.Repeat("noise line\n", 400000))

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context cancellation", err)
	}
	if got := chunks.Load(); got != 1 {
		t.Fatalf("chunks read after cancellation = %d, want 1", got)
	}
	if got := largest.Load(); got > 64<<10 {
		t.Fatalf("largest read chunk = %d, want at most 65536", got)
	}
}

func TestReadinessCapsEachFollowerDrainAtOneMiB(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	follower, err := newLogFollower(serverLog, "server")
	if err != nil {
		t.Fatal(err)
	}
	defer follower.close()
	writeLog(t, serverLog, strings.Repeat("bounded line\n", 200000))
	lines, err := follower.readAvailable()
	if err != nil {
		t.Fatal(err)
	}
	readBytes := 0
	for _, line := range lines {
		readBytes += len(line) + 1
	}
	if readBytes == 0 || readBytes > 1<<20 {
		t.Fatalf("first drain input = %d bytes, want 1..1048576", readBytes)
	}
	if !follower.more {
		t.Fatal("first bounded drain did not report remaining input")
	}
}

func TestReadinessRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "server.log")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	result := waitForReadiness(ReadinessMonitor{ServerLog: fifo}, t.Context(), ExpectedReadiness{})
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("Wait() error = %v, want non-regular log rejection", err)
		}
	case <-time.After(100 * time.Millisecond):
		unblockFIFO(t, fifo)
		<-result
		t.Fatal("Wait() blocked opening a FIFO")
	}
}

func TestReadinessRejectsLateFIFOAndJoinsWatchers(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "prior run\n")
	output := newReadinessOutput()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := waitForReadiness(ReadinessMonitor{ServerLog: serverLog, Output: output, PollEvery: 2 * time.Millisecond}, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(serverLog, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("Wait() error = %v, want non-regular late log rejection", err)
		}
	case <-time.After(100 * time.Millisecond):
		unblockFIFO(t, serverLog)
		<-result
		t.Fatal("Wait() left a watcher blocked opening a late FIFO")
	}
}

func TestReadinessRejectsSymlinkedLog(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.log")
	writeLog(t, target, "log\n")
	serverLog := filepath.Join(root, "server.log")
	if err := os.Symlink(target, serverLog); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := (ReadinessMonitor{ServerLog: serverLog}).Wait(ctx, ExpectedReadiness{})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() error = %v, want immediate symlink rejection", err)
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
		expectedModReadiness("2.5.8", "1.0.85"),
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
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.85"))
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)

	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, "current server generation\n")
	waitForOutput(t, output, "current server generation", result)
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	appendLog(t, bepInExLog, "[Message: BepInEx] Chainloader startup complete\n")
	appendLog(t, bepInExLog, "Plugin gg.deca.VampireCommandFramework version 0.10.4 is loaded!\n")
	if err := os.Rename(bepInExLog, bepInExLog+".previous"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, bepInExLog,
		"Plugin aa.odjit.KindredCommands version 2.5.8 is loaded!\n"+
			"Satisvampory 1.0.85 (Satisvampory) ready.\n")

	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v; output = %q", err, output.String())
	}
}

func TestReadinessDrainsAppendDuringRotationHandoff(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	bepInExLog := filepath.Join(root, "bepinex.log")
	previousBepInExLog := bepInExLog + ".previous"
	writeLog(t, serverLog, "prior run\n")
	writeLog(t, bepInExLog, "prior run\n")

	output := newReadinessOutput()
	var injectOnce sync.Once
	var injectErr error
	monitor := ReadinessMonitor{
		ServerLog:  serverLog,
		BepInExLog: bepInExLog,
		Output:     output,
		PollEvery:  2 * time.Millisecond,
		testHooks: &readinessTestHooks{replacementDetected: func(source string) {
			if source == "bepinex" {
				injectOnce.Do(func() {
					injectErr = appendLogError(previousBepInExLog,
						"[Message: BepInEx] Chainloader startup complete\n"+
							"Plugin gg.deca.VampireCommandFramework version 0.10.4 is loaded!\n")
				})
			}
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, expectedModReadiness("2.5.8", "1.0.85"))
	appendUntilObserved(t, serverLog, output.server, result)
	appendUntilObserved(t, bepInExLog, output.bepinex, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	if err := os.Rename(bepInExLog, previousBepInExLog); err != nil {
		t.Fatal(err)
	}
	writeLog(t, bepInExLog,
		"Plugin aa.odjit.KindredCommands version 2.5.8 is loaded!\n"+
			"Satisvampory 1.0.85 (Satisvampory) ready.\n")

	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v; output = %q", err, output.String())
	}
	if injectErr != nil {
		t.Fatalf("handoff append error = %v", injectErr)
	}
}

func TestReadinessRetainsIntermediateReplacementWithFatal(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "generation A\n")
	output := newReadinessOutput()
	var replaceOnce sync.Once
	var replaceErr error
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: 2 * time.Millisecond,
		testHooks: &readinessTestHooks{replacementDetected: func(source string) {
			replaceOnce.Do(func() {
				replaceErr = os.Rename(serverLog, filepath.Join(root, "generation-B.log"))
				if replaceErr == nil {
					replaceErr = os.WriteFile(serverLog, []byte("generation C\n"), 0o600)
				}
			})
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	waitForOutput(t, output, "[Server] Startup Completed", result)
	if err := os.Rename(serverLog, filepath.Join(root, "generation-A.log")); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, "[Fatal : BepInEx] generation B failed\n")

	err := <-result
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fatal") {
		t.Fatalf("Wait() error = %v, want fatal retained from generation B; output = %q", err, output.String())
	}
	if replaceErr != nil {
		t.Fatalf("B-to-C replacement error = %v", replaceErr)
	}
}

func TestReadinessRejectsMoreThanSixteenPendingGenerations(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "baseline\n")
	output := newReadinessOutput()
	generation := 0
	var replacementErr error
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: time.Millisecond,
		testHooks: &readinessTestHooks{replacementDetected: func(string) {
			if replacementErr != nil || generation >= 16 {
				return
			}
			replacementErr = os.Rename(serverLog, filepath.Join(root, fmt.Sprintf("queued-%02d.log", generation)))
			generation++
			if replacementErr == nil {
				replacementErr = os.WriteFile(serverLog, []byte(fmt.Sprintf("generation %d\n", generation)), 0o600)
			}
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	output.discard.Store(true)
	if err := os.Rename(serverLog, filepath.Join(root, "baseline.log")); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, strings.Repeat("generation zero backlog\n", 800000))

	err := <-result
	if err == nil || !strings.Contains(err.Error(), "pending log generations") {
		t.Fatalf("Wait() error = %v, want pending-generation bound", err)
	}
	if replacementErr != nil {
		t.Fatalf("replacement error = %v", replacementErr)
	}
}

func TestReadinessRetainsOldDescriptorThroughStableGracePoll(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "prior run\n")
	output := newReadinessOutput()
	detected := make(chan struct{})
	adopted := make(chan struct{})
	var detectOnce sync.Once
	var adoptOnce sync.Once
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: 50 * time.Millisecond,
		testHooks: &readinessTestHooks{
			replacementDetected: func(string) { detectOnce.Do(func() { close(detected) }) },
			generationAdopted:   func(string) { adoptOnce.Do(func() { close(adopted) }) },
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	select {
	case <-detected:
	case err := <-result:
		t.Fatalf("Wait() returned before replacement detection: %v", err)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("replacement was not detected")
	}
	select {
	case <-adopted:
		t.Fatal("replacement was adopted without a stable grace poll")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-adopted:
	case err := <-result:
		t.Fatalf("Wait() returned before replacement adoption: %v", err)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("replacement was not adopted after the grace poll")
	}
	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestReadinessReplacementGraceStartsAtDetectionAfterSlowRead(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	oldLog := serverLog + ".old"
	writeLog(t, serverLog, "baseline\n")
	output := newReadinessOutput()
	const pollEvery = 60 * time.Millisecond
	var armed atomic.Bool
	var slowOnce sync.Once
	detected := make(chan time.Time, 1)
	adopted := make(chan time.Time, 1)
	var detectOnce sync.Once
	var adoptOnce sync.Once
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: pollEvery,
		testHooks: &readinessTestHooks{
			afterReadChunk: func(string) {
				if armed.Load() {
					slowOnce.Do(func() {
						timer := time.NewTimer(pollEvery + 20*time.Millisecond)
						defer timer.Stop()
						<-timer.C
					})
				}
			},
			replacementDetected: func(string) { detectOnce.Do(func() { detected <- time.Now() }) },
			generationAdopted:   func(string) { adoptOnce.Do(func() { adopted <- time.Now() }) },
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	armed.Store(true)
	if err := os.Rename(serverLog, oldLog); err != nil {
		t.Fatal(err)
	}
	appendLog(t, oldLog, "slow old descriptor tail\n")
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	started := <-detected
	finished := <-adopted
	if elapsed := finished.Sub(started); elapsed < pollEvery-10*time.Millisecond {
		t.Fatalf("replacement grace = %v, want detection-relative PollEvery", elapsed)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error after test cancellation = %v", err)
	}
}

func TestReadinessDetectsCurrentGenerationTruncateAndRegrowPastPreviousOffset(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, strings.Repeat("x", 30)+"\n")
	follower, err := newLogFollower(serverLog, "server")
	if err != nil {
		t.Fatal(err)
	}
	defer follower.close()
	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, strings.Repeat("y", 30)+"\n")
	if lines, err := follower.readAvailable(); err != nil || len(lines) != 1 || lines[0] != strings.Repeat("y", 30) {
		t.Fatalf("current generation = %q, %v", lines, err)
	}
	if lines, err := follower.readAvailable(); err != nil || len(lines) != 0 {
		t.Fatalf("current generation adoption = %q, %v", lines, err)
	}
	appendLog(t, serverLog, "follower probe\n")
	if lines, err := follower.readAvailable(); err != nil || len(lines) != 1 || lines[0] != "follower probe" {
		t.Fatalf("current generation append = %q, %v", lines, err)
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

func TestReadinessTreatsTruncateBetweenReadAndAnchorAsTransition(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	follower, err := newLogFollower(serverLog, "server")
	if err != nil {
		t.Fatal(err)
	}
	defer follower.close()
	writeLog(t, serverLog, "current generation\n")
	if lines, err := follower.readAvailable(); err != nil || len(lines) != 1 || lines[0] != "current generation" {
		t.Fatalf("current generation = %q, %v", lines, err)
	}
	if lines, err := follower.readAvailable(); err != nil || len(lines) != 0 {
		t.Fatalf("current generation adoption = %q, %v", lines, err)
	}

	ready := readLogFixture(t, "server-ready.log")
	var truncateOnce sync.Once
	var truncateErr error
	follower.testHooks = &readinessTestHooks{afterReadChunk: func(source string) {
		truncateOnce.Do(func() {
			truncateErr = os.WriteFile(serverLog, []byte(ready), 0o600)
		})
	}}
	appendLog(t, serverLog, "line read before truncate\n")
	lines, err := follower.readAvailable()
	if err != nil {
		t.Fatalf("read across concurrent truncate: %v", err)
	}
	if truncateErr != nil {
		t.Fatalf("truncate error = %v", truncateErr)
	}
	if len(lines) != 1 || lines[0] != strings.TrimSuffix(ready, "\n") {
		t.Fatalf("lines after truncate transition = %q", lines)
	}
}

func TestReadinessDoesNotReplayTruncatedBaselinedInode(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log")+strings.Repeat("stale\n", 64))
	output := newReadinessOutput()
	monitor := ReadinessMonitor{ServerLog: serverLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)

	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	select {
	case err := <-result:
		t.Fatalf("Wait() replayed a truncated baselined inode: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	if err := <-result; err != nil {
		t.Fatalf("Wait() error for new current inode = %v", err)
	}
}

func TestReadinessInvalidatesBaselinedEvidenceTruncatedDuringRead(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "prior run\n")
	output := newReadinessOutput()
	var armed atomic.Bool
	var truncateOnce sync.Once
	var truncateErr error
	truncated := make(chan struct{})
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: 2 * time.Millisecond,
		testHooks: &readinessTestHooks{afterReadChunk: func(string) {
			if armed.Load() {
				truncateOnce.Do(func() {
					truncateErr = os.WriteFile(serverLog, []byte("truncated baseline\n"), 0o600)
					close(truncated)
				})
			}
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	armed.Store(true)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	select {
	case <-truncated:
	case err := <-result:
		t.Fatalf("Wait() returned before baseline truncate hook: %v", err)
	case <-time.After(time.Second):
		t.Fatal("baseline truncate hook did not run")
	}
	select {
	case err := <-result:
		t.Fatalf("Wait() retained evidence from a truncated baseline: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if truncateErr != nil {
		t.Fatalf("truncate error = %v", truncateErr)
	}
	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	if err := <-result; err != nil {
		t.Fatalf("Wait() error for new inode = %v", err)
	}
}

func TestReadinessInvalidatesBaselinedEvidenceTruncatedImmediatelyAfterMarker(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	writeLog(t, serverLog, "prior run\n")
	observed := newReadinessOutput()
	markerWritten := make(chan struct{})
	var truncateErr error
	output := &injectingReadinessOutput{
		Writer: observed,
		match:  "[Server] Startup Completed",
		inject: func() error {
			truncateErr = os.WriteFile(serverLog, []byte("truncated immediately after marker\n"), 0o600)
			close(markerWritten)
			return nil
		},
	}
	monitor := ReadinessMonitor{ServerLog: serverLog, Output: output, PollEvery: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, observed.server, result)
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	<-markerWritten
	select {
	case err := <-result:
		t.Fatalf("Wait() retained marker from immediately truncated baseline: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if truncateErr != nil {
		t.Fatalf("truncate error = %v", truncateErr)
	}
	if err := os.Rename(serverLog, serverLog+".baseline"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	if err := <-result; err != nil {
		t.Fatalf("Wait() error for replacement inode = %v", err)
	}
}

func TestReadinessNeverSwitchesBackToBaselinedInode(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	baselineLog := filepath.Join(root, "baseline.log")
	writeLog(t, serverLog, "baseline\n")
	output := newReadinessOutput()
	adopted := make(chan struct{})
	var adoptOnce sync.Once
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: 2 * time.Millisecond,
		testHooks: &readinessTestHooks{generationAdopted: func(string) {
			adoptOnce.Do(func() { close(adopted) })
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)

	if err := os.Rename(serverLog, baselineLog); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, "current generation probe\n")
	waitForOutput(t, output, "current generation probe", result)
	select {
	case <-adopted:
	case err := <-result:
		t.Fatalf("Wait() returned before current generation adoption: %v", err)
	case <-time.After(time.Second):
		t.Fatal("current generation was not adopted")
	}
	if err := os.Remove(serverLog); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(baselineLog, serverLog); err != nil {
		t.Fatal(err)
	}
	appendLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	select {
	case err := <-result:
		t.Fatalf("Wait() switched back to the baselined inode: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := os.Rename(serverLog, baselineLog); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	if err := <-result; err != nil {
		t.Fatalf("Wait() error for later current inode = %v", err)
	}
}

func TestReadinessSeenInodeReappearanceIsNotAQuietEvent(t *testing.T) {
	root := t.TempDir()
	serverLog := filepath.Join(root, "server.log")
	baselineLog := filepath.Join(root, "baseline.log")
	writeLog(t, serverLog, "generation A\n")
	output := newReadinessOutput()
	adopted := make(chan struct{}, 2)
	monitor := ReadinessMonitor{
		ServerLog: serverLog,
		Output:    output,
		PollEvery: 2 * time.Millisecond,
		testHooks: &readinessTestHooks{generationAdopted: func(string) { adopted <- struct{}{} }},
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := waitForReadiness(monitor, ctx, ExpectedReadiness{})
	appendUntilObserved(t, serverLog, output.server, result)
	if err := os.Rename(serverLog, baselineLog); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, readLogFixture(t, "server-ready.log"))
	select {
	case <-adopted:
	case err := <-result:
		t.Fatalf("Wait() returned before generation B adoption: %v", err)
	case <-time.After(time.Second):
		t.Fatal("generation B was not adopted")
	}
	if err := os.Remove(serverLog); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(baselineLog, serverLog); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		t.Fatalf("Wait() counted seen-inode reappearance as quiet: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.Rename(serverLog, baselineLog); err != nil {
		t.Fatal(err)
	}
	writeLog(t, serverLog, "generation C\n")
	select {
	case <-adopted:
	case err := <-result:
		t.Fatalf("Wait() returned before generation C adoption: %v", err)
	case <-time.After(time.Second):
		t.Fatal("generation C was not adopted")
	}
	if err := <-result; err != nil {
		t.Fatalf("Wait() error = %v", err)
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
		"VRisingServer-20260828T120000.000000000Z-000001.log",
	} {
		writeDatedLog(t, root, name, old)
	}
	for _, name := range []string{
		"20260901-1200-VRisingServer.log",
		"20261340-9999-VRisingServer.log",
		"20260828-1200-user.log",
		"BepInEx.log",
		"backup-20260828-1200-VRisingServer.log",
		"VRisingServer-20260901T120000.000000000Z-000002.log",
		"VRisingServer-20260828T120000.000000000Z-00001.log",
		"VRisingServer-20260828T120000.000000000Z-000003.log.extra",
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
	for _, name := range []string{
		"20260828-1200-VRisingServer.log",
		"20260828-120000-VRisingServer.log",
		"VRisingServer-20260828T120000.000000000Z-000001.log",
	} {
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
		"VRisingServer-20260901T120000.000000000Z-000002.log",
		"VRisingServer-20260828T120000.000000000Z-00001.log",
		"VRisingServer-20260828T120000.000000000Z-000003.log.extra",
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

func TestPruneLogsMissingDirectoryIsNoop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

	if err := PruneLogs(root, 7, now); err != nil {
		t.Fatalf("PruneLogs() missing directory error = %v", err)
	}
}

func TestPruneServerLogsPrunesLegacyRootAndCurrentSubdirectory(t *testing.T) {
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	rootSubdir := filepath.Join(dataDir, "Settings")
	logSubdir := filepath.Join(logDir, "archive")
	for _, dir := range []string{logDir, rootSubdir, logSubdir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)
	legacy := "20260828-1200-VRisingServer.log"
	current := "VRisingServer-20260828T120000.000000000Z-000001.log"

	for dir, names := range map[string][]string{
		dataDir:    {legacy, current, "operator.log"},
		logDir:     {legacy, current, "operator.log"},
		rootSubdir: {legacy},
		logSubdir:  {current},
	} {
		for _, name := range names {
			writeDatedLog(t, dir, name, old)
		}
	}

	if err := PruneServerLogs(dataDir, 7, now); err != nil {
		t.Fatalf("PruneServerLogs() error = %v", err)
	}
	for _, path := range []string{
		filepath.Join(dataDir, legacy),
		filepath.Join(logDir, current),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("expired owned log %q stat error = %v, want not exist", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(dataDir, current),
		filepath.Join(dataDir, "operator.log"),
		filepath.Join(logDir, legacy),
		filepath.Join(logDir, "operator.log"),
		filepath.Join(rootSubdir, legacy),
		filepath.Join(logSubdir, current),
	} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("preserved path %q stat error = %v", path, err)
		}
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

func TestPruneLogsPreservesFileChangedBeforeFinalReopen(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)
	tests := []struct {
		name   string
		change func(t *testing.T, path string)
	}{
		{
			name: "append",
			change: func(t *testing.T, path string) {
				appendLog(t, path, "changed\n")
			},
		},
		{
			name: "replace",
			change: func(t *testing.T, path string) {
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				writeLog(t, path, "replacement\n")
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "refresh mtime",
			change: func(t *testing.T, path string) {
				if err := os.Chtimes(path, now, now); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			name := "20260828-1200-VRisingServer.log"
			path := filepath.Join(root, name)
			writeDatedLog(t, root, name, old)
			changed := false
			err := pruneLogsWithHooks(root, 7, now, pruneTestHooks{
				beforeReopen: func(got string) {
					if got == name && !changed {
						changed = true
						tt.change(t, path)
					}
				},
			})
			if err == nil || !strings.Contains(err.Error(), "changed during pruning") {
				t.Fatalf("pruneLogsWithHooks() error = %v, want changed-file rejection", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("changed log was not preserved: %v", err)
			}
		})
	}
}

func TestPruneLogsFinalReopenRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	name := "20260828-1200-VRisingServer.log"
	path := filepath.Join(root, name)
	writeDatedLog(t, root, name, now.Add(-8*24*time.Hour))
	var hookErr error
	result := make(chan error, 1)
	go func() {
		result <- pruneLogsWithHooks(root, 7, now, pruneTestHooks{beforeReopen: func(string) {
			hookErr = os.Rename(path, path+".original")
			if hookErr == nil {
				hookErr = unix.Mkfifo(path, 0o600)
			}
		}})
	}()
	select {
	case err := <-result:
		if hookErr != nil {
			t.Fatalf("replacement hook error = %v", hookErr)
		}
		if err == nil || !strings.Contains(err.Error(), "changed during pruning") {
			t.Fatalf("pruneLogsWithHooks() error = %v, want FIFO replacement rejection", err)
		}
	case <-time.After(100 * time.Millisecond):
		unblockFIFO(t, path)
		<-result
		t.Fatal("prune final reopen blocked on a FIFO")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("replacement FIFO info = %v, %v", info, err)
	}
}

func TestPruneLogsFsyncsDataDirectoryAfterUnlink(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	writeDatedLog(t, root, "20260828-1200-VRisingServer.log", now.Add(-8*24*time.Hour))
	syncs := 0
	err := pruneLogsWithHooks(root, 7, now, pruneTestHooks{syncDirectory: func(int) error {
		syncs++
		return nil
	}})
	if err != nil {
		t.Fatalf("pruneLogsWithHooks() error = %v", err)
	}
	if syncs != 1 {
		t.Fatalf("DataDir fsync calls = %d, want 1", syncs)
	}
}

func TestPruneLogsReportsDataDirectoryFsyncFailure(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	name := "20260828-1200-VRisingServer.log"
	writeDatedLog(t, root, name, now.Add(-8*24*time.Hour))
	want := errors.New("sync failed")
	err := pruneLogsWithHooks(root, 7, now, pruneTestHooks{syncDirectory: func(int) error { return want }})
	if !errors.Is(err, want) {
		t.Fatalf("pruneLogsWithHooks() error = %v, want sync failure", err)
	}
	if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
		t.Fatalf("unlinked log stat error = %v, want not exist", err)
	}
}

func TestPruneLogsRejectsExcessiveRetentionBeforeDeletion(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	name := "20200101-0000-VRisingServer.log"
	writeDatedLog(t, root, name, now.Add(-2000*24*time.Hour))
	if err := PruneLogs(root, 365001, now); err == nil {
		t.Fatal("PruneLogs() accepted retention above 1000 years")
	}
	if _, err := os.Stat(filepath.Join(root, name)); err != nil {
		t.Fatalf("log changed before retention validation: %v", err)
	}
}

type readinessOutput struct {
	mu          sync.Mutex
	buffer      bytes.Buffer
	server      chan struct{}
	bepinex     chan struct{}
	serverOnce  sync.Once
	bepinexOnce sync.Once
	discard     atomic.Bool
}

type injectingReadinessOutput struct {
	io.Writer
	match  string
	inject func() error
	once   sync.Once
	mu     sync.Mutex
	err    error
}

func (o *injectingReadinessOutput) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(o.match)) {
		o.once.Do(func() {
			o.mu.Lock()
			o.err = o.inject()
			o.mu.Unlock()
		})
	}
	return o.Writer.Write(p)
}

func (o *injectingReadinessOutput) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
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
	if o.discard.Load() {
		return len(p), nil
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

func expectedModReadiness(kindredVersion, satisvamporyVersion string) ExpectedReadiness {
	return ExpectedReadiness{
		RequireMods:         true,
		KindredVersion:      kindredVersion,
		SatisvamporyVersion: satisvamporyVersion,
	}
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

func waitForOutput(t *testing.T, output *readinessOutput, substring string, result <-chan error) {
	t.Helper()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		if strings.Contains(output.String(), substring) {
			return
		}
		select {
		case err := <-result:
			t.Fatalf("Wait() returned before output contained %q: %v", substring, err)
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("output did not contain %q: %q", substring, output.String())
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
	if err := appendLogError(path, content); err != nil {
		t.Fatal(err)
	}
}

func appendLogError(path, content string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, content); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
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

func unblockFIFO(t *testing.T, path string) {
	t.Helper()
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}
}
