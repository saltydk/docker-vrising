package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var semanticVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

const maxLogDays = 365000

type Config struct {
	ServerDir           string
	DataDir             string
	StateDir            string
	UpdateGame          bool
	UpdateMods          bool
	ModsEnabled         bool
	KindredVersion      string
	SatisvamporyVersion string
	BackupRetention     int
	StartupTimeout      time.Duration
	ShutdownTimeout     time.Duration
	LogDays             int
	Branch              string
	PUID                *int
	PGID                *int
	GameEnv             map[string]string
	BaseEnv             []string
}

func LoadConfig(env map[string]string) (Config, []string, error) {
	cfg := Config{
		ServerDir:           "/mnt/vrising/server",
		DataDir:             "/mnt/vrising/persistentdata",
		StateDir:            "/mnt/vrising/server/.docker-vrising",
		UpdateGame:          true,
		UpdateMods:          true,
		ModsEnabled:         true,
		KindredVersion:      "latest",
		SatisvamporyVersion: "latest",
		BackupRetention:     3,
		StartupTimeout:      30 * time.Minute,
		ShutdownTimeout:     120 * time.Second,
		LogDays:             30,
		GameEnv:             make(map[string]string),
	}
	warnings := []string{}

	var err error
	if cfg.UpdateGame, err = boolValue(env, "UPDATE_GAME", cfg.UpdateGame); err != nil {
		return Config{}, nil, err
	}
	if cfg.UpdateMods, err = boolValue(env, "UPDATE_MODS", cfg.UpdateMods); err != nil {
		return Config{}, nil, err
	}
	if cfg.ModsEnabled, err = boolValue(env, "MODS_ENABLED", cfg.ModsEnabled); err != nil {
		return Config{}, nil, err
	}
	if cfg.BackupRetention, err = intValue(env, "BACKUP_RETENTION", cfg.BackupRetention, 1); err != nil {
		return Config{}, nil, err
	}
	if cfg.LogDays, err = intValue(env, "LOGDAYS", cfg.LogDays, 1); err != nil {
		return Config{}, nil, err
	}
	if cfg.LogDays > maxLogDays {
		return Config{}, nil, fmt.Errorf("LOGDAYS must be at most %d", maxLogDays)
	}
	if cfg.StartupTimeout, err = durationValue(env, "STARTUP_TIMEOUT", cfg.StartupTimeout); err != nil {
		return Config{}, nil, err
	}
	if cfg.ShutdownTimeout, err = durationValue(env, "SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, nil, err
	}
	if cfg.KindredVersion, err = versionValue(env, "KINDRED_COMMANDS_VERSION", cfg.KindredVersion); err != nil {
		return Config{}, nil, err
	}
	if cfg.SatisvamporyVersion, err = versionValue(env, "SATISVAMPORY_VERSION", cfg.SatisvamporyVersion); err != nil {
		return Config{}, nil, err
	}
	cfg.Branch = env["BRANCH"]
	if cfg.PUID, cfg.PGID, err = ownerValues(env); err != nil {
		return Config{}, nil, err
	}

	for key, value := range env {
		if strings.HasPrefix(key, "VR_") || key == "TZ" || key == "WINEDEBUG" {
			cfg.GameEnv[key] = value
		}
	}
	for legacy, native := range map[string]string{
		"SERVERNAME": "VR_SERVER_NAME",
		"WORLDNAME":  "VR_SAVE_NAME",
		"GAMEPORT":   "VR_GAME_PORT",
		"QUERYPORT":  "VR_QUERY_PORT",
	} {
		legacyValue, legacyPresent := env[legacy]
		if !legacyPresent {
			continue
		}
		if _, nativePresent := env[native]; nativePresent {
			warnings = append(warnings, fmt.Sprintf("%s is shadowed by %s", legacy, native))
			continue
		}
		cfg.GameEnv[native] = legacyValue
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cfg.BaseEnv = append(cfg.BaseEnv, key+"="+env[key])
	}
	sort.Strings(warnings)
	return cfg, warnings, nil
}

func versionValue(env map[string]string, name, defaultValue string) (string, error) {
	value, ok := env[name]
	if !ok {
		return defaultValue, nil
	}
	if value != "latest" && !semanticVersion.MatchString(value) {
		return "", fmt.Errorf("%s must be latest or a semantic version", name)
	}
	return value, nil
}

func boolValue(env map[string]string, name string, defaultValue bool) (bool, error) {
	value, ok := env[name]
	if !ok {
		return defaultValue, nil
	}
	if value != "true" && value != "false" {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value == "true", nil
}

func intValue(env map[string]string, name string, defaultValue, minimum int) (int, error) {
	value, ok := env[name]
	if !ok {
		return defaultValue, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum {
		return 0, fmt.Errorf("%s must be an integer of at least %d", name, minimum)
	}
	return parsed, nil
}

func durationValue(env map[string]string, name string, defaultValue time.Duration) (time.Duration, error) {
	value, ok := env[name]
	if !ok {
		return defaultValue, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func ownerValues(env map[string]string) (*int, *int, error) {
	puid, hasPUID := env["PUID"]
	pgid, hasPGID := env["PGID"]
	if hasPUID != hasPGID {
		return nil, nil, fmt.Errorf("PUID and PGID must be supplied together")
	}
	if !hasPUID {
		return nil, nil, nil
	}
	puidValue, err := strconv.Atoi(puid)
	if err != nil || puidValue <= 0 {
		return nil, nil, fmt.Errorf("PUID must be a positive integer")
	}
	pgidValue, err := strconv.Atoi(pgid)
	if err != nil || pgidValue <= 0 {
		return nil, nil, fmt.Errorf("PGID must be a positive integer")
	}
	return &puidValue, &pgidValue, nil
}

func EnvironmentMap(entries []string) map[string]string {
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}
