package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const defaultLogPollInterval = 100 * time.Millisecond
const logContinuityBytes = 64
const logReadChunkBytes = 64 << 10
const maxReadinessInputBytes = 1 << 20
const maxPendingLogGenerations = 16

type ExpectedReadiness struct {
	KindredVersion      string
	SatisvamporyVersion string
	RequireMods         bool
}

type ReadinessMonitor struct {
	ServerLog  string
	BepInExLog string
	Output     io.Writer
	PollEvery  time.Duration
	testHooks  *readinessTestHooks
}

type readinessTestHooks struct {
	replacementDetected func(string)
	generationAdopted   func(string)
	afterReadChunk      func(string)
	readChunk           func(string, int)
}

type readinessEvidence struct {
	serverStartup bool
	chainloader   bool
	vcf           bool
	kindred       bool
	satisvampory  bool
}

type logBatch struct {
	round    uint64
	source   string
	lines    []string
	more     bool
	pending  bool
	reset    bool
	activity bool
	err      error
}

type logFollower struct {
	path                string
	source              string
	file                *os.File
	identity            os.FileInfo
	currentGeneration   bool
	seen                map[logInode]struct{}
	offset              int64
	remainder           []byte
	anchor              []byte
	drain               chan uint64
	pendingGenerations  []*logFollower
	pollEvery           time.Duration
	observation         uint64
	detectedObservation uint64
	notBefore           time.Time
	testHooks           *readinessTestHooks
	more                bool
	resetEvidence       bool
	activity            bool
}

type logInode struct {
	device uint64
	inode  uint64
}

func (m ReadinessMonitor) Wait(ctx context.Context, expected ExpectedReadiness) error {
	if m.ServerLog == "" {
		return fmt.Errorf("server log path is required")
	}
	if expected.RequireMods {
		if m.BepInExLog == "" {
			return fmt.Errorf("BepInEx log path is required")
		}
		if expected.KindredVersion == "" || expected.KindredVersion == "latest" || !semanticVersion.MatchString(expected.KindredVersion) {
			return fmt.Errorf("exact Kindred version is required")
		}
		if expected.SatisvamporyVersion == "" || expected.SatisvamporyVersion == "latest" || !semanticVersion.MatchString(expected.SatisvamporyVersion) {
			return fmt.Errorf("exact Satisvampory version is required")
		}
	}
	pollEvery := m.PollEvery
	if pollEvery <= 0 {
		pollEvery = defaultLogPollInterval
	}
	output := m.Output
	if output == nil {
		output = io.Discard
	}

	followers := make([]*logFollower, 0, 2)
	server, err := newLogFollower(m.ServerLog, "server")
	if err != nil {
		return fmt.Errorf("initialize server log follower: %w", err)
	}
	server.testHooks = m.testHooks
	server.pollEvery = pollEvery
	followers = append(followers, server)
	if expected.RequireMods {
		bepInEx, err := newLogFollower(m.BepInExLog, "bepinex")
		if err != nil {
			server.close()
			return fmt.Errorf("initialize BepInEx log follower: %w", err)
		}
		bepInEx.testHooks = m.testHooks
		bepInEx.pollEvery = pollEvery
		followers = append(followers, bepInEx)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	events := make(chan logBatch, len(followers))
	var watchers sync.WaitGroup
	for _, follower := range followers {
		follower.drain = make(chan uint64)
		watchers.Add(1)
		go func() {
			defer watchers.Done()
			follower.watch(watchCtx, events)
		}()
	}
	defer func() {
		cancel()
		watchers.Wait()
	}()

	var evidence readinessEvidence
	var round uint64
	quietRounds := 0
	for {
		round++
		ready, more, pending, activity, err := observeReadinessRound(ctx, followers, events, round, output, &evidence, expected)
		if err != nil {
			return err
		}
		if ready && !more && !pending && !activity {
			quietRounds++
		} else {
			quietRounds = 0
		}
		if quietRounds == 2 {
			return nil
		}
		if more && !pending {
			continue
		}
		if err := waitReadinessPoll(ctx, pollEvery); err != nil {
			return err
		}
	}
}

func waitReadinessPoll(ctx context.Context, pollEvery time.Duration) error {
	timer := time.NewTimer(pollEvery)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("readiness: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func observeReadinessRound(
	ctx context.Context,
	followers []*logFollower,
	events <-chan logBatch,
	round uint64,
	output io.Writer,
	evidence *readinessEvidence,
	expected ExpectedReadiness,
) (bool, bool, bool, bool, error) {
	for _, follower := range followers {
		select {
		case follower.drain <- round:
		case <-ctx.Done():
			return false, false, false, false, fmt.Errorf("readiness: %w", ctx.Err())
		}
	}
	batches := make([]logBatch, 0, len(followers))
	for len(batches) < len(followers) {
		select {
		case batch := <-events:
			if batch.round != round {
				return false, false, false, false, fmt.Errorf("log follower acknowledged an unexpected observation round")
			}
			batches = append(batches, batch)
		case <-ctx.Done():
			return false, false, false, false, fmt.Errorf("readiness: %w", ctx.Err())
		}
	}
	for _, batch := range batches {
		if batch.err != nil {
			return false, false, false, false, fmt.Errorf("follow %s log: %w", batch.source, batch.err)
		}
		if batch.reset {
			evidence.reset(batch.source)
		}
		for _, line := range batch.lines {
			if _, err := fmt.Fprintf(output, "[%s] %s\n", batch.source, line); err != nil {
				return false, false, false, false, fmt.Errorf("write %s log output: %w", batch.source, err)
			}
		}
	}
	for _, batch := range batches {
		for _, line := range batch.lines {
			if fatalReadinessLine(line) {
				return false, false, false, false, fmt.Errorf("fatal startup output: %s", shortLogReason(line))
			}
		}
	}
	for _, batch := range batches {
		for _, line := range batch.lines {
			evidence.observe(line, expected)
		}
	}
	more := false
	pending := false
	activity := false
	for _, batch := range batches {
		more = more || batch.more
		pending = pending || batch.pending
		activity = activity || batch.activity
	}
	return evidence.complete(expected.RequireMods), more, pending, activity, nil
}

func newLogFollower(path, source string) (*logFollower, error) {
	follower := &logFollower{path: path, source: source, seen: make(map[logInode]struct{})}
	file, info, err := openLogPath(path)
	if errors.Is(err, os.ErrNotExist) {
		return follower, nil
	}
	if err != nil {
		return nil, err
	}
	follower.file = file
	follower.identity = info
	inode, err := inodeOf(info)
	if err != nil {
		follower.close()
		return nil, err
	}
	follower.seen[inode] = struct{}{}
	follower.offset = info.Size()
	if err := follower.refreshAnchor(); err != nil {
		if errors.Is(err, io.EOF) {
			follower.close()
			return follower, nil
		}
		follower.close()
		return nil, err
	}
	return follower, nil
}

func (f *logFollower) watch(ctx context.Context, events chan<- logBatch) {
	defer f.close()
	for {
		select {
		case <-ctx.Done():
			return
		case round := <-f.drain:
			lines, err := f.readAvailableContext(ctx)
			select {
			case events <- logBatch{round: round, source: f.source, lines: lines, more: f.more, pending: len(f.pendingGenerations) != 0, reset: f.resetEvidence, activity: f.activity, err: err}:
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

func (f *logFollower) readAvailable() ([]string, error) {
	return f.readAvailableContext(context.Background())
}

func (f *logFollower) readAvailableContext(ctx context.Context) ([]string, error) {
	f.observation++
	f.more = false
	f.resetEvidence = false
	f.activity = false
	remaining := maxReadinessInputBytes
	var lines []string
	current, err := f.readCurrent(ctx, &remaining)
	if err != nil {
		return nil, err
	}
	lines = append(lines, current...)
	if f.resetEvidence {
		lines = nil
	}

	replacement, info, err := openLogPath(f.path)
	if errors.Is(err, os.ErrNotExist) {
		replacement = nil
		info = nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if replacement != nil && (f.file == nil || !os.SameFile(f.identity, info)) {
		inode, identityErr := inodeOf(info)
		if identityErr != nil {
			replacement.Close()
			return nil, identityErr
		}
		if _, alreadySeen := f.seen[inode]; alreadySeen {
			replacement.Close()
			f.activity = true
		} else {
			if len(f.pendingGenerations) >= maxPendingLogGenerations {
				replacement.Close()
				return nil, fmt.Errorf("more than %d pending log generations", maxPendingLogGenerations)
			}
			detectedAt := time.Now()
			queued := &logFollower{
				source:              f.source,
				file:                replacement,
				identity:            info,
				currentGeneration:   true,
				offset:              0,
				testHooks:           f.testHooks,
				detectedObservation: f.observation,
				notBefore:           detectedAt.Add(f.pollEvery),
			}
			f.seen[inode] = struct{}{}
			f.pendingGenerations = append(f.pendingGenerations, queued)
			f.activity = true
			if f.testHooks != nil && f.testHooks.replacementDetected != nil {
				f.testHooks.replacementDetected(f.source)
			}
		}
	} else if replacement != nil {
		replacement.Close()
	}

	if f.file != nil {
		current, readErr := f.readCurrent(ctx, &remaining)
		if readErr != nil {
			return nil, readErr
		}
		lines = append(lines, current...)
		if f.resetEvidence {
			lines = nil
		}
	}

	queuedLinesStart := len(lines)
	for _, queued := range f.pendingGenerations {
		queued.more = false
		queued.activity = false
		current, readErr := queued.readCurrent(ctx, &remaining)
		if readErr != nil {
			return nil, readErr
		}
		lines = append(lines, current...)
		f.activity = f.activity || queued.activity
	}

	retiringChanged := false
	if len(f.pendingGenerations) != 0 && f.file != nil {
		current, readErr := f.readCurrent(ctx, &remaining)
		if readErr != nil {
			return nil, readErr
		}
		if f.resetEvidence {
			queuedLines := append([]string(nil), lines[queuedLinesStart:]...)
			lines = queuedLines
			retiringChanged = true
		} else {
			lines = append(lines, current...)
		}
	}

	if len(f.pendingGenerations) != 0 {
		head := f.pendingGenerations[0]
		activeMore := false
		if f.file != nil {
			activeMore = f.more
		}
		if !retiringChanged && !activeMore && !head.more && f.observation > head.detectedObservation && !time.Now().Before(head.notBefore) {
			f.closeActive()
			f.file = head.file
			head.file = nil
			f.identity = head.identity
			f.currentGeneration = true
			f.offset = head.offset
			f.remainder = head.remainder
			f.anchor = head.anchor
			f.pendingGenerations = f.pendingGenerations[1:]
			f.activity = true
			if f.testHooks != nil && f.testHooks.generationAdopted != nil {
				f.testHooks.generationAdopted(f.source)
			}
		}
	}

	more := f.more
	for _, queued := range f.pendingGenerations {
		more = more || queued.more
	}
	f.more = more
	return lines, nil
}

func openLogPath(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		return nil, nil, fmt.Errorf("log is not a regular file")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func (f *logFollower) readCurrent(ctx context.Context, remaining *int) ([]string, error) {
	if f.file == nil {
		f.more = false
		return nil, nil
	}
	info, err := f.file.Stat()
	if err != nil {
		return nil, err
	}
	continuous, err := f.continuousAtOffset(info.Size())
	if err != nil {
		return nil, err
	}
	if !continuous {
		f.handleReadTransition()
		return nil, nil
	}
	if info.Size() == f.offset || *remaining == 0 {
		f.more = info.Size() > f.offset
		return nil, nil
	}
	var lines []string
	for f.offset < info.Size() && *remaining > 0 {
		select {
		case <-ctx.Done():
			return lines, ctx.Err()
		default:
		}
		readSize := min(int64(logReadChunkBytes), info.Size()-f.offset, int64(*remaining))
		chunk := make([]byte, int(readSize))
		priorOffset := f.offset
		priorAnchor := append([]byte(nil), f.anchor...)
		n, readErr := f.file.ReadAt(chunk, f.offset)
		if n > 0 {
			f.activity = true
			f.offset += int64(n)
			*remaining -= n
			if f.testHooks != nil && f.testHooks.readChunk != nil {
				f.testHooks.readChunk(f.source, n)
			}
			if f.testHooks != nil && f.testHooks.afterReadChunk != nil {
				f.testHooks.afterReadChunk(f.source)
			}
		}
		after, statErr := f.file.Stat()
		if statErr != nil {
			return nil, statErr
		}
		continuous, continuityErr := f.matchesAnchor(after.Size(), priorOffset, priorAnchor)
		if continuityErr != nil {
			return nil, continuityErr
		}
		if readErr != nil || !continuous {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			baselined := !f.currentGeneration
			f.handleReadTransition()
			if baselined {
				return nil, nil
			}
			return lines, nil
		}
		if n > 0 {
			current, err := f.completeLines(chunk[:n])
			if err != nil {
				return nil, err
			}
			lines = append(lines, current...)
		}
		if err := f.refreshAnchor(); err != nil {
			if errors.Is(err, io.EOF) {
				baselined := !f.currentGeneration
				f.handleReadTransition()
				if baselined {
					return nil, nil
				}
				return lines, nil
			}
			return nil, err
		}
		info = after
	}
	f.more = f.offset < info.Size()
	return lines, nil
}

func (f *logFollower) handleReadTransition() {
	f.activity = true
	if !f.currentGeneration {
		f.resetEvidence = true
		f.closeActive()
		return
	}
	f.offset = 0
	f.remainder = nil
	f.anchor = nil
	f.more = true
}

func (f *logFollower) continuousAtOffset(size int64) (bool, error) {
	return f.matchesAnchor(size, f.offset, f.anchor)
}

func (f *logFollower) matchesAnchor(size, offset int64, anchor []byte) (bool, error) {
	if size < offset {
		return false, nil
	}
	if offset == 0 || len(anchor) == 0 {
		return true, nil
	}
	current := make([]byte, len(anchor))
	if _, err := f.file.ReadAt(current, offset-int64(len(current))); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(current, anchor), nil
}

func (f *logFollower) refreshAnchor() error {
	length := min(int64(logContinuityBytes), f.offset)
	if length == 0 {
		f.anchor = nil
		return nil
	}
	anchor := make([]byte, int(length))
	if _, err := f.file.ReadAt(anchor, f.offset-length); err != nil {
		return err
	}
	f.anchor = anchor
	return nil
}

func (f *logFollower) close() {
	f.closeActive()
	for _, queued := range f.pendingGenerations {
		queued.closeActive()
	}
	f.pendingGenerations = nil
}

func (f *logFollower) closeActive() {
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	f.identity = nil
	f.currentGeneration = false
	f.anchor = nil
}

func inodeOf(info os.FileInfo) (logInode, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return logInode{}, fmt.Errorf("log has unsupported file identity")
	}
	return logInode{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func (f *logFollower) completeLines(content []byte) ([]string, error) {
	var lines []string
	for len(content) > 0 {
		newline := bytes.IndexByte(content, '\n')
		if newline < 0 {
			if len(f.remainder)+len(content) > maxReadinessInputBytes {
				return nil, fmt.Errorf("log line exceeds %d bytes", maxReadinessInputBytes)
			}
			f.remainder = append(f.remainder, content...)
			break
		}
		if len(f.remainder)+newline > maxReadinessInputBytes {
			return nil, fmt.Errorf("log line exceeds %d bytes", maxReadinessInputBytes)
		}
		f.remainder = append(f.remainder, content[:newline]...)
		f.remainder = bytes.TrimSuffix(f.remainder, []byte{'\r'})
		lines = append(lines, string(f.remainder))
		f.remainder = nil
		content = content[newline+1:]
	}
	return lines, nil
}

func (e *readinessEvidence) observe(line string, expected ExpectedReadiness) {
	lower := strings.ToLower(line)
	if strings.Contains(line, "[Server] Startup Completed") {
		e.serverStartup = true
	}
	if strings.Contains(lower, "chainloader") &&
		(strings.Contains(lower, "startup complete") || strings.Contains(lower, "startup finished")) {
		e.chainloader = true
	}
	if strings.Contains(lower, "vampirecommandframework") && strings.Contains(lower, "is loaded!") {
		e.vcf = true
	}
	if expected.KindredVersion != "" && strings.Contains(line,
		"Plugin aa.odjit.KindredCommands version "+expected.KindredVersion+" is loaded!") {
		e.kindred = true
	}
	if expected.SatisvamporyVersion != "" && strings.Contains(line,
		"Satisvampory "+expected.SatisvamporyVersion+" (Satisvampory) ready.") {
		e.satisvampory = true
	}
}

func (e *readinessEvidence) reset(source string) {
	if source == "server" {
		e.serverStartup = false
		return
	}
	e.chainloader = false
	e.vcf = false
	e.kindred = false
	e.satisvampory = false
}

func (e readinessEvidence) complete(requireMods bool) bool {
	if !e.serverStartup {
		return false
	}
	return !requireMods || e.chainloader && e.vcf && e.kindred && e.satisvampory
}

func fatalReadinessLine(line string) bool {
	lower := strings.ToLower(line)
	for _, signature := range []string{
		"error loading [",
		"failed to load plugin",
		"failed loading plugin",
		"could not load plugin",
		"failed to initialize bepinex",
		"bepinex initialization failed",
		"failed to load coreclr",
		"failed to start coreclr",
		"coreclr initialization failed",
		"coreclr_initialize failed",
		"unable to execute doorstop target assembly",
		"failed to load doorstop",
	} {
		if strings.Contains(lower, signature) {
			return true
		}
	}
	if strings.Contains(lower, "fatal") &&
		(strings.Contains(lower, "bepinex") || strings.Contains(lower, "doorstop") ||
			strings.Contains(lower, "coreclr") || strings.Contains(lower, "plugin")) {
		return true
	}
	return strings.Contains(lower, "doorstop") &&
		(strings.Contains(lower, " error") || strings.Contains(lower, ":error") || strings.Contains(lower, " failed"))
}

func shortLogReason(line string) string {
	line = strings.TrimSpace(line)
	const maximum = 160
	if len(line) <= maximum {
		return line
	}
	return line[:maximum] + "..."
}

func PruneLogs(logDir string, days int, now time.Time) error {
	return pruneLogsWithHooks(logDir, days, now, pruneTestHooks{})
}

type pruneTestHooks struct {
	beforeReopen  func(string)
	syncDirectory func(int) error
}

func pruneLogsWithHooks(logDir string, days int, now time.Time, hooks pruneTestHooks) error {
	if days < 1 || days > maxLogDays {
		return fmt.Errorf("log retention must be between 1 and %d days", maxLogDays)
	}
	root, err := openDirectoryPath(logDir, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open log directory: %w", err)
	}
	directory := os.NewFile(uintptr(root), logDir)
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	cutoff := now.AddDate(0, 0, -days)
	syncDirectory := hooks.syncDirectory
	if syncDirectory == nil {
		syncDirectory = unix.Fsync
	}
	for _, entry := range entries {
		name := entry.Name()
		if !ownedServerLogName(name) {
			continue
		}
		fd, opened, err := openPruneLog(root, name)
		if errors.Is(err, unix.ELOOP) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open server log %s: %w", name, err)
		}
		unix.Close(fd)
		if opened.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		modified := time.Unix(opened.Mtim.Sec, opened.Mtim.Nsec)
		if !modified.Before(cutoff) {
			continue
		}
		if hooks.beforeReopen != nil {
			hooks.beforeReopen(name)
		}
		finalFD, final, err := openPruneLog(root, name)
		if err != nil {
			return fmt.Errorf("server log %s changed during pruning: %w", name, err)
		}
		finalModified := time.Unix(final.Mtim.Sec, final.Mtim.Nsec)
		if final.Mode&unix.S_IFMT != unix.S_IFREG || final.Dev != opened.Dev || final.Ino != opened.Ino || final.Size != opened.Size ||
			final.Mtim != opened.Mtim || !finalModified.Before(cutoff) {
			unix.Close(finalFD)
			return fmt.Errorf("server log %s changed during pruning", name)
		}
		if err := unix.Unlinkat(root, name, 0); err != nil {
			unix.Close(finalFD)
			return fmt.Errorf("remove server log %s: %w", name, err)
		}
		unix.Close(finalFD)
		if err := syncDirectory(root); err != nil {
			return fmt.Errorf("sync log directory: %w", err)
		}
	}
	return nil
}

func openPruneLog(root int, name string) (int, unix.Stat_t, error) {
	fd, err := unix.Openat(root, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, unix.Stat_t{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, unix.Stat_t{}, err
	}
	return fd, stat, nil
}

func ownedServerLogName(name string) bool {
	for _, layout := range []string{
		"20060102-1504-VRisingServer.log",
		"20060102-150405-VRisingServer.log",
	} {
		if _, err := time.ParseInLocation(layout, name, time.UTC); err == nil {
			return true
		}
	}
	const prefix = "VRisingServer-"
	const suffix = ".log"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	identity := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	separator := strings.LastIndexByte(identity, '-')
	if separator < 0 {
		return false
	}
	timestamp := identity[:separator]
	sequence := identity[separator+1:]
	if len(sequence) < 6 {
		return false
	}
	for _, digit := range sequence {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	_, err := time.ParseInLocation("20060102T150405.000000000Z", timestamp, time.UTC)
	return err == nil
}
