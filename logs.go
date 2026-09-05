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
	"time"

	"golang.org/x/sys/unix"
)

const defaultLogPollInterval = 100 * time.Millisecond
const logContinuityBytes = 64

type ExpectedReadiness struct {
	KindredVersion string
	RequireMods    bool
}

type ReadinessMonitor struct {
	ServerLog  string
	BepInExLog string
	Output     io.Writer
	PollEvery  time.Duration
}

type readinessEvidence struct {
	serverStartup bool
	chainloader   bool
	vcf           bool
	kindred       bool
}

type logBatch struct {
	source string
	lines  []string
	err    error
}

type logFollower struct {
	path      string
	source    string
	file      *os.File
	identity  os.FileInfo
	offset    int64
	remainder string
	anchor    []byte
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
	followers = append(followers, server)
	if expected.RequireMods {
		bepInEx, err := newLogFollower(m.BepInExLog, "bepinex")
		if err != nil {
			server.close()
			return fmt.Errorf("initialize BepInEx log follower: %w", err)
		}
		followers = append(followers, bepInEx)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	events := make(chan logBatch, len(followers))
	var watchers sync.WaitGroup
	for _, follower := range followers {
		watchers.Add(1)
		go func() {
			defer watchers.Done()
			follower.watch(watchCtx, pollEvery, events)
		}()
	}
	defer func() {
		cancel()
		watchers.Wait()
	}()

	var evidence readinessEvidence
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("readiness: %w", ctx.Err())
		case batch := <-events:
			if batch.err != nil {
				return fmt.Errorf("follow %s log: %w", batch.source, batch.err)
			}
			for _, line := range batch.lines {
				if _, err := fmt.Fprintf(output, "[%s] %s\n", batch.source, line); err != nil {
					return fmt.Errorf("write %s log output: %w", batch.source, err)
				}
			}
			for _, line := range batch.lines {
				if fatalReadinessLine(line) {
					return fmt.Errorf("fatal startup output: %s", shortLogReason(line))
				}
			}
			for _, line := range batch.lines {
				evidence.observe(line, expected)
			}
			if evidence.complete(expected.RequireMods) {
				return nil
			}
		}
	}
}

func newLogFollower(path, source string) (*logFollower, error) {
	follower := &logFollower{path: path, source: source}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return follower, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("log is not a regular file")
	}
	follower.file = file
	follower.identity = info
	follower.offset = info.Size()
	if err := follower.refreshAnchor(); err != nil {
		follower.close()
		return nil, err
	}
	return follower, nil
}

func (f *logFollower) watch(ctx context.Context, pollEvery time.Duration, events chan<- logBatch) {
	defer f.close()
	poll := func() bool {
		lines, err := f.readAvailable()
		if err == nil && len(lines) == 0 {
			return true
		}
		select {
		case events <- logBatch{source: f.source, lines: lines, err: err}:
			return err == nil
		case <-ctx.Done():
			return false
		}
	}
	if !poll() {
		return
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !poll() {
				return
			}
		}
	}
}

func (f *logFollower) readAvailable() ([]string, error) {
	var lines []string
	if f.file != nil {
		current, err := f.readCurrent()
		if err != nil {
			return nil, err
		}
		lines = append(lines, current...)
	}

	replacement, info, err := openLogPath(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return lines, nil
	}
	if err != nil {
		return nil, err
	}
	if f.file != nil && os.SameFile(f.identity, info) {
		replacement.Close()
		return lines, nil
	}
	f.close()
	f.file = replacement
	f.identity = info
	f.offset = 0
	f.remainder = ""
	f.anchor = nil
	current, err := f.readCurrent()
	if err != nil {
		return nil, err
	}
	return append(lines, current...), nil
}

func openLogPath(path string) (*os.File, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, fmt.Errorf("log is not a regular file")
	}
	return file, info, nil
}

func (f *logFollower) readCurrent() ([]string, error) {
	info, err := f.file.Stat()
	if err != nil {
		return nil, err
	}
	continuous, err := f.continuousAtOffset(info.Size())
	if err != nil {
		return nil, err
	}
	if !continuous {
		f.offset = 0
		f.remainder = ""
		f.anchor = nil
	}
	if info.Size() == f.offset {
		return nil, nil
	}
	content, err := io.ReadAll(io.NewSectionReader(f.file, f.offset, info.Size()-f.offset))
	if err != nil {
		return nil, err
	}
	f.offset += int64(len(content))
	if err := f.refreshAnchor(); err != nil {
		return nil, err
	}
	return f.completeLines(string(content)), nil
}

func (f *logFollower) continuousAtOffset(size int64) (bool, error) {
	if size < f.offset {
		return false, nil
	}
	if f.offset == 0 || len(f.anchor) == 0 {
		return true, nil
	}
	current := make([]byte, len(f.anchor))
	if _, err := f.file.ReadAt(current, f.offset-int64(len(current))); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(current, f.anchor), nil
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
	f.anchor = nil
}

func (f *logFollower) completeLines(content string) []string {
	content = f.remainder + content
	parts := strings.Split(content, "\n")
	f.remainder = parts[len(parts)-1]
	parts = parts[:len(parts)-1]
	for i := range parts {
		parts[i] = strings.TrimSuffix(parts[i], "\r")
	}
	return parts
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
	if days < 1 {
		return fmt.Errorf("log retention must be at least one day")
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
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	for _, entry := range entries {
		name := entry.Name()
		if !ownedServerLogName(name) {
			continue
		}
		fd, err := unix.Openat(root, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ELOOP) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open server log %s: %w", name, err)
		}
		var opened unix.Stat_t
		statErr := unix.Fstat(fd, &opened)
		unix.Close(fd)
		if statErr != nil {
			return fmt.Errorf("stat server log %s: %w", name, statErr)
		}
		if opened.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		modified := time.Unix(opened.Mtim.Sec, opened.Mtim.Nsec)
		if !modified.Before(cutoff) {
			continue
		}
		var named unix.Stat_t
		if err := unix.Fstatat(root, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("verify server log %s: %w", name, err)
		}
		if named.Mode&unix.S_IFMT != unix.S_IFREG || named.Dev != opened.Dev || named.Ino != opened.Ino {
			return fmt.Errorf("server log %s changed during pruning", name)
		}
		if err := unix.Unlinkat(root, name, 0); err != nil {
			return fmt.Errorf("remove server log %s: %w", name, err)
		}
	}
	return nil
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
