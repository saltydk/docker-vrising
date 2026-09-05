package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	exitUsage     = 2
	exitPreflight = 10
	exitModUpdate = 20
	exitBackup    = 21
	exitSteam     = 22
	exitReadiness = 30
	exitShutdown  = 31
)

var buildVersion = "dev"

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout))
}

func dispatch(args []string, output io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(output, "usage: vrisingctl {run|health|verify|version}")
		return exitUsage
	}

	switch args[0] {
	case "run":
		return runCommand(output, Config{})
	case "health":
		return healthCommand(output)
	case "verify":
		return verifyCommand(output, Config{})
	case "version":
		fmt.Fprintln(output, buildVersion)
		return 0
	default:
		fmt.Fprintln(output, "usage: vrisingctl {run|health|verify|version}")
		return exitUsage
	}
}

func runCommand(output io.Writer, cfg Config) int {
	if cfg.ServerDir == "" && cfg.DataDir == "" && cfg.StateDir == "" {
		loaded, warnings, err := LoadConfig(EnvironmentMap(os.Environ()))
		for _, warning := range warnings {
			fmt.Fprintln(output, warning)
		}
		if err != nil {
			fmt.Fprintln(output, err)
			return exitPreflight
		}
		cfg = loaded
	}

	identity, err := ResolveIdentity(cfg)
	if err == nil {
		err = PrepareOwnership(cfg, identity)
	}
	if err == nil {
		err = DropPrivileges(identity)
	}
	if err == nil {
		err = identity.VerifyWritable(cfg)
	}
	if err != nil {
		fmt.Fprintln(output, err)
		return exitPreflight
	}

	app := newApplication(cfg, identity)
	configureApplicationOutput(app, output)
	err = app.Run(context.Background())
	if err == nil {
		return 0
	}
	fmt.Fprintln(output, err)
	var runErr *runError
	if errors.As(err, &runErr) {
		return runErr.Code
	}
	return exitPreflight
}

func configureApplicationOutput(app *Application, output io.Writer) {
	supervisor, ok := app.Supervisor.(*Supervisor)
	if !ok {
		return
	}
	if supervisor.Readiness != nil {
		supervisor.Readiness.Output = output
	}
	if processes, ok := supervisor.Processes.(ExecProcessFactory); ok {
		processes.Stdout = output
		processes.Stderr = output
		supervisor.Processes = processes
	}
}

func healthCommand(output io.Writer) int {
	cfg, _, err := LoadConfig(EnvironmentMap(os.Environ()))
	if err == nil {
		var state State
		state, err = (&Store{StateDir: cfg.StateDir}).Load()
		if err == nil {
			err = CheckHealth(state, procFSInspector{})
		}
	}
	if err != nil {
		fmt.Fprintln(output, err)
		return 1
	}
	fmt.Fprintln(output, "healthy")
	return 0
}
