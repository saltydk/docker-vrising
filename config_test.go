package main

import (
	"bytes"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, warnings, err := LoadConfig(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if cfg.ServerDir != "/mnt/vrising/server" ||
		cfg.DataDir != "/mnt/vrising/persistentdata" ||
		!cfg.UpdateGame || !cfg.UpdateMods || !cfg.ModsEnabled ||
		cfg.KindredVersion != "latest" ||
		cfg.BackupRetention != 3 ||
		cfg.StartupTimeout != 30*time.Minute ||
		cfg.ShutdownTimeout != 120*time.Second ||
		cfg.LogDays != 30 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
}

func TestLoadConfigNativeValuesWinOverLegacyAliases(t *testing.T) {
	cfg, warnings, err := LoadConfig(map[string]string{
		"SERVERNAME":     "legacy",
		"VR_SERVER_NAME": "native",
		"WORLDNAME":      "legacy-world",
		"VR_SAVE_NAME":   "native-world",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GameEnv["VR_SERVER_NAME"] != "native" ||
		cfg.GameEnv["VR_SAVE_NAME"] != "native-world" ||
		len(warnings) != 2 {
		t.Fatalf("cfg=%#v warnings=%v", cfg, warnings)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "boolean", env: map[string]string{"UPDATE_GAME": "yes"}},
		{name: "zero duration", env: map[string]string{"STARTUP_TIMEOUT": "0s"}},
		{name: "negative duration", env: map[string]string{"SHUTDOWN_TIMEOUT": "-1s"}},
		{name: "backup retention", env: map[string]string{"BACKUP_RETENTION": "0"}},
		{name: "log retention too large", env: map[string]string{"LOGDAYS": "365001"}},
		{name: "only puid", env: map[string]string{"PUID": "1000"}},
		{name: "non-numeric ids", env: map[string]string{"PUID": "user", "PGID": "1000"}},
		{name: "kindred version", env: map[string]string{"KINDRED_COMMANDS_VERSION": "v1.2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := LoadConfig(tt.env); err == nil {
				t.Fatal("LoadConfig succeeded for invalid environment")
			}
		})
	}
}

func TestLoadConfigAcceptsMaximumLogRetention(t *testing.T) {
	cfg, _, err := LoadConfig(map[string]string{"LOGDAYS": "365000"})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.LogDays != 365000 {
		t.Fatalf("LogDays = %d, want 365000", cfg.LogDays)
	}
}

func TestEnvironmentMapParsesEnvironmentEntries(t *testing.T) {
	got := EnvironmentMap([]string{"VR_SERVER_NAME=castle", "TZ=UTC", "EMPTY="})
	want := map[string]string{"VR_SERVER_NAME": "castle", "TZ": "UTC", "EMPTY": ""}
	if len(got) != len(want) {
		t.Fatalf("environment map length = %d, want %d", len(got), len(want))
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Errorf("environment map[%q] = %q, want %q", key, got[key], wantValue)
		}
	}
}

func TestLoadConfigParsesValuesAndPreservesGameEnvironment(t *testing.T) {
	cfg, warnings, err := LoadConfig(map[string]string{
		"UPDATE_GAME":              "false",
		"UPDATE_MODS":              "false",
		"MODS_ENABLED":             "false",
		"KINDRED_COMMANDS_VERSION": "1.2.3",
		"BACKUP_RETENTION":         "5",
		"STARTUP_TIMEOUT":          "45s",
		"SHUTDOWN_TIMEOUT":         "3m",
		"LOGDAYS":                  "7",
		"BRANCH":                   "experimental",
		"PUID":                     "1000",
		"PGID":                     "1001",
		"VR_SERVER_NAME":           "castle",
		"VR_CUSTOM_SETTING":        "preserve-me",
		"TZ":                       "UTC",
		"WINEDEBUG":                "-all",
		"SERVERNAME":               "legacy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || cfg.UpdateGame || cfg.UpdateMods || cfg.ModsEnabled ||
		cfg.KindredVersion != "1.2.3" || cfg.BackupRetention != 5 ||
		cfg.StartupTimeout != 45*time.Second || cfg.ShutdownTimeout != 3*time.Minute ||
		cfg.LogDays != 7 || cfg.Branch != "experimental" || cfg.PUID == nil ||
		*cfg.PUID != 1000 || cfg.PGID == nil || *cfg.PGID != 1001 {
		t.Fatalf("unexpected parsed config: %#v", cfg)
	}
	for key, want := range map[string]string{
		"VR_SERVER_NAME":    "castle",
		"VR_CUSTOM_SETTING": "preserve-me",
		"TZ":                "UTC",
		"WINEDEBUG":         "-all",
	} {
		if cfg.GameEnv[key] != want {
			t.Errorf("GameEnv[%q] = %q, want %q", key, cfg.GameEnv[key], want)
		}
	}
	if cfg.GameEnv["VR_SAVE_NAME"] != "" {
		t.Fatalf("unexpected unmapped alias: %#v", cfg.GameEnv)
	}
}

func TestCommandDispatchProvidesCompileSafeStubs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantOut string
		want    int
	}{
		{name: "run", args: []string{"run"}, wantOut: "runtime not implemented\n", want: exitPreflight},
		{name: "health", args: []string{"health"}, wantOut: "not running\n", want: 1},
		{name: "version", args: []string{"version"}, wantOut: "dev\n", want: 0},
		{name: "unknown", args: []string{"wat"}, wantOut: "usage: vrisingctl {run|health|version}\n", want: exitUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			if got := dispatch(tt.args, &output); got != tt.want {
				t.Fatalf("dispatch exit code = %d, want %d", got, tt.want)
			}
			if output.String() != tt.wantOut {
				t.Fatalf("dispatch output = %q, want %q", output.String(), tt.wantOut)
			}
		})
	}
}
