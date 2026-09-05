package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ProcInspector interface {
	Identity(int) (ProcessIdentity, error)
}

type procFSInspector struct {
	Root string
}

func (p procFSInspector) Identity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("PID must be positive")
	}
	root := p.Root
	if root == "" {
		root = "/proc"
	}
	content, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return ProcessIdentity{}, err
	}
	stat := strings.TrimSpace(string(content))
	commandEnd := strings.LastIndexByte(stat, ')')
	if commandEnd < 0 || commandEnd+1 >= len(stat) {
		return ProcessIdentity{}, fmt.Errorf("malformed process stat")
	}
	fields := strings.Fields(stat[commandEnd+1:])
	const startTimeIndexAfterCommand = 19
	if len(fields) <= startTimeIndexAfterCommand {
		return ProcessIdentity{}, fmt.Errorf("malformed process stat")
	}
	startTicks, err := strconv.ParseUint(fields[startTimeIndexAfterCommand], 10, 64)
	if err != nil || startTicks == 0 {
		return ProcessIdentity{}, fmt.Errorf("malformed process start ticks")
	}
	return ProcessIdentity{PID: pid, StartTicks: startTicks}, nil
}

func CheckHealth(state State, proc ProcInspector) error {
	if state.SchemaVersion != schemaVersion {
		return fmt.Errorf("invalid state")
	}
	runtime := state.Runtime
	if !runtime.Ready {
		if runtime.Server.PID <= 0 {
			return fmt.Errorf("not running")
		}
		return fmt.Errorf("not ready")
	}
	if runtime.Phase != "ready" || runtime.Server.PID <= 0 || runtime.Server.StartTicks == 0 {
		return fmt.Errorf("runtime state is incoherent")
	}
	if proc == nil {
		return fmt.Errorf("process inspector is unavailable")
	}
	live, err := proc.Identity(runtime.Server.PID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("server process is not running")
		}
		return fmt.Errorf("inspect server process: %w", err)
	}
	if !runtime.Server.Matches(live) {
		return fmt.Errorf("server process identity changed")
	}
	return nil
}
