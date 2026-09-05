package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

type ExpectedReadiness struct {
	KindredVersion string
	RequireMods    bool
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
}

type logBatch struct {
	round   uint64
	source  string
	lines   []string
	more    bool
	pending bool
	reset   bool
	err     error
}

type logFollower struct {
	path              string
	source            string
	file              *os.File
	identity          os.FileInfo
	currentGeneration bool
	seen              map[logInode]struct{}
	offset            int64
	remainder         []byte
	anchor            []byte
	drain             chan uint64
	pending           *logInode
	testHooks         *readinessTestHooks
	more              bool
	resetEvidence     bool
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
	followers = append(followers, server)
	if expected.RequireMods {
		bepInEx, err := newLogFollower(m.BepInExLog, "bepinex")
		if err != nil {
			server.close()
			return fmt.Errorf("initialize BepInEx log follower: %w", err)
		}
		bepInEx.testHooks = m.testHooks
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

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	var evidence readinessEvidence
	var round uint64
	confirming := false
	for {
		round++
		ready, more, pending, err := observeReadinessRound(ctx, followers, events, round, output, &evidence, expected)
		if err != nil {
			return err
		}
		if ready && !more && !pending {
			if confirming {
				return nil
			}
			confirming = true
		} else {
			confirming = false
		}
		if more && !pending {
			continue
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("readiness: %w", ctx.Err())
		case <-ticker.C:
		}
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
) (bool, bool, bool, error) {
	for _, follower := range followers {
		select {
		case follower.drain <- round:
		case <-ctx.Done():
			return false, false, false, fmt.Errorf("readiness: %w", ctx.Err())
		}
	}
	batches := make([]logBatch, 0, len(followers))
	for len(batches) < len(followers) {
		select {
		case batch := <-events:
			if batch.round != round {
				return false, false, false, fmt.Errorf("log follower acknowledged an unexpected observation round")
			}
			batches = append(batches, batch)
		case <-ctx.Done():
			return false, false, false, fmt.Errorf("readiness: %w", ctx.Err())
		}
	}
	for _, batch := range batches {
		if batch.err != nil {
			return false, false, false, fmt.Errorf("follow %s log: %w", batch.source, batch.err)
		}
		if batch.reset {
			evidence.reset(batch.source)
		}
		for _, line := range batch.lines {
			if _, err := fmt.Fprintf(output, "[%s] %s\n", batch.source, line); err != nil {
				return false, false, false, fmt.Errorf("write %s log output: %w", batch.source, err)
			}
		}
	}
	for _, batch := range batches {
		for _, line := range batch.lines {
			if fatalReadinessLine(line) {
				return false, false, false, fmt.Errorf("fatal startup output: %s", shortLogReason(line))
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
	for _, batch := range batches {
		more = more || batch.more
		pending = pending || batch.pending
	}
	return evidence.complete(expected.RequireMods), more, pending, nil
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
			case events <- logBatch{round: round, source: f.source, lines: lines, more: f.more, pending: f.pending != nil, reset: f.resetEvidence, err: err}:
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
	f.more = false
	f.resetEvidence = false
	remaining := maxReadinessInputBytes
	var lines []string
	if f.file != nil {
		current, err := f.readCurrent(ctx, &remaining)
		if err != nil {
			return nil, err
		}
		lines = append(lines, current...)
		if f.resetEvidence {
			lines = nil
		}
	}

	replacement, info, err := openLogPath(f.path)
	if errors.Is(err, os.ErrNotExist) {
		f.pending = nil
		return lines, nil
	}
	if err != nil {
		return nil, err
	}
	if f.file != nil && os.SameFile(f.identity, info) {
		replacement.Close()
		f.pending = nil
		return lines, nil
	}
	inode, err := inodeOf(info)
	if err != nil {
		replacement.Close()
		return nil, err
	}
	if _, alreadySeen := f.seen[inode]; alreadySeen {
		replacement.Close()
		f.pending = nil
		return lines, nil
	}
	if f.testHooks != nil && f.testHooks.replacementDetected != nil {
		f.testHooks.replacementDetected(f.source)
	}
	if f.file != nil {
		current, err := f.readCurrent(ctx, &remaining)
		if err != nil {
			replacement.Close()
			return nil, err
		}
		lines = append(lines, current...)
		if f.resetEvidence {
			lines = nil
		}
	}
	if f.more {
		if f.pending == nil || *f.pending != inode {
			f.pending = &inode
		}
		replacement.Close()
		return lines, nil
	}
	if f.pending == nil || *f.pending != inode {
		f.pending = &inode
		replacement.Close()
		return lines, nil
	}
	f.close()
	f.file = replacement
	f.identity = info
	f.currentGeneration = true
	f.seen[inode] = struct{}{}
	f.offset = 0
	f.remainder = nil
	f.anchor = nil
	f.pending = nil
	if f.testHooks != nil && f.testHooks.generationAdopted != nil {
		f.testHooks.generationAdopted(f.source)
	}
	current, err := f.readCurrent(ctx, &remaining)
	if err != nil {
		return nil, err
	}
	return append(lines, current...), nil
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
	info, err := f.file.Stat()
	if err != nil {
		return nil, err
	}
	continuous, err := f.continuousAtOffset(info.Size())
	if err != nil {
		return nil, err
	}
	if !continuous {
		if !f.currentGeneration {
			f.close()
			return nil, nil
		}
		f.offset = 0
		f.remainder = nil
		f.anchor = nil
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
	if !f.currentGeneration {
		f.resetEvidence = true
		f.close()
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
}

func (e *readinessEvidence) reset(source string) {
	if source == "server" {
		e.serverStartup = false
		return
	}
	e.chainloader = false
	e.vcf = false
	e.kindred = false
}

func (e readinessEvidence) complete(requireMods bool) bool {
	if !e.serverStartup {
		return false
	}
	return !requireMods || e.chainloader && e.vcf && e.kindred
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

func PruneLogs(dataDir string, days int, now time.Time) error {
	return pruneLogsWithHooks(dataDir, days, now, pruneTestHooks{})
}

type pruneTestHooks struct {
	beforeReopen  func(string)
	syncDirectory func(int) error
}

func pruneLogsWithHooks(dataDir string, days int, now time.Time, hooks pruneTestHooks) error {
	if days < 1 || days > maxLogDays {
		return fmt.Errorf("log retention must be between 1 and %d days", maxLogDays)
	}
	root, err := openDirectoryPath(dataDir, false)
	if err != nil {
		return fmt.Errorf("open log directory: %w", err)
	}
	directory := os.NewFile(uintptr(root), dataDir)
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
	return false
}
