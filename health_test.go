package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthRejectsMissingProcess(t *testing.T) {
	state := healthyRuntimeState()
	proc := fakeProcInspector{err: os.ErrNotExist}
	if err := CheckHealth(state, proc); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("CheckHealth() error = %v, want missing process", err)
	}
}

func TestHealthRejectsReusedPID(t *testing.T) {
	state := healthyRuntimeState()
	proc := fakeProcInspector{identity: ProcessIdentity{PID: 4242, StartTicks: 999}}
	if err := CheckHealth(state, proc); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("CheckHealth() error = %v, want reused PID rejection", err)
	}
}

func TestHealthAcceptsDegradedKnownGoodServer(t *testing.T) {
	state := healthyRuntimeState()
	state.Runtime.Degraded = true
	state.Runtime.Reason = "remote metadata unavailable; using known-good server"
	proc := fakeProcInspector{identity: state.Runtime.Server}
	if err := CheckHealth(state, proc); err != nil {
		t.Fatalf("CheckHealth() error = %v", err)
	}
}

func TestHealthRejectsIncoherentRuntimeState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*State)
	}{
		{name: "schema", mutate: func(state *State) { state.SchemaVersion++ }},
		{name: "not ready", mutate: func(state *State) { state.Runtime.Ready = false }},
		{name: "phase", mutate: func(state *State) { state.Runtime.Phase = "starting" }},
		{name: "pid", mutate: func(state *State) { state.Runtime.Server.PID = 0 }},
		{name: "start ticks", mutate: func(state *State) { state.Runtime.Server.StartTicks = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := healthyRuntimeState()
			tt.mutate(&state)
			if err := CheckHealth(state, fakeProcInspector{identity: state.Runtime.Server}); err == nil {
				t.Fatal("CheckHealth() accepted incoherent runtime state")
			}
		})
	}
}

func TestProcInspectorReadsCurrentProcessIdentity(t *testing.T) {
	identity, err := (procFSInspector{}).Identity(os.Getpid())
	if err != nil {
		t.Fatalf("Identity() error = %v", err)
	}
	if identity.PID != os.Getpid() || identity.StartTicks == 0 {
		t.Fatalf("Identity() = %#v", identity)
	}
}

func TestProcInspectorRejectsMalformedStat(t *testing.T) {
	root := t.TempDir()
	pidDir := root + "/42"
	if err := os.Mkdir(pidDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidDir+"/stat", []byte("42 (broken) S 1 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (procFSInspector{Root: root}).Identity(42)
	if err == nil {
		t.Fatal("Identity() accepted malformed stat data")
	}
}

func TestHealthRejectsZombieAndDeadProcessStates(t *testing.T) {
	for _, processState := range []string{"Z", "X", "x"} {
		t.Run(processState, func(t *testing.T) {
			state := healthyRuntimeState()
			root := t.TempDir()
			pidDir := filepath.Join(root, fmt.Sprint(state.Runtime.Server.PID))
			if err := os.Mkdir(pidDir, 0o700); err != nil {
				t.Fatal(err)
			}
			fields := []string{processState}
			for range 18 {
				fields = append(fields, "1")
			}
			fields = append(fields, fmt.Sprint(state.Runtime.Server.StartTicks))
			stat := fmt.Sprintf("%d (server worker) %s\n", state.Runtime.Server.PID, strings.Join(fields, " "))
			if err := os.WriteFile(filepath.Join(pidDir, "stat"), []byte(stat), 0o600); err != nil {
				t.Fatal(err)
			}

			err := CheckHealth(state, procFSInspector{Root: root})
			if err == nil || !strings.Contains(err.Error(), "not running") {
				t.Fatalf("CheckHealth() error = %v, want dead process rejection", err)
			}
		})
	}
}

func healthyRuntimeState() State {
	return State{
		SchemaVersion: schemaVersion,
		Runtime: RuntimeState{
			Phase:  "ready",
			Ready:  true,
			Server: ProcessIdentity{PID: 4242, StartTicks: 123456},
		},
	}
}

type fakeProcInspector struct {
	identity ProcessIdentity
	err      error
}

func (f fakeProcInspector) Identity(int) (ProcessIdentity, error) {
	return f.identity, f.err
}
