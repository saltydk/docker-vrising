package main

import (
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
		fmt.Fprintln(output, "usage: vrisingctl {run|health|version}")
		return exitUsage
	}

	switch args[0] {
	case "run":
		return runCommand(output, Config{})
	case "health":
		return healthCommand(output)
	case "version":
		fmt.Fprintln(output, buildVersion)
		return 0
	default:
		fmt.Fprintln(output, "usage: vrisingctl {run|health|version}")
		return exitUsage
	}
}

func runCommand(output io.Writer, _ Config) int {
	fmt.Fprintln(output, "runtime not implemented")
	return exitPreflight
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
