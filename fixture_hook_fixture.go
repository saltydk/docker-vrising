//go:build fixture

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const fixtureBarrierManagedPath = "BepInEx/plugins/saltydk-managed/KindredCommands.dll"

var fixtureTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func fixtureAfterManagedPublication(ctx context.Context, relativePath string) error {
	if os.Getenv("FIXTURE_APPLY_BARRIER") != "true" || relativePath != fixtureBarrierManagedPath {
		return nil
	}
	recordDir := os.Getenv("FIXTURE_RECORD_DIR")
	token := os.Getenv("FIXTURE_RUN_TOKEN")
	if !filepath.IsAbs(recordDir) || !fixtureTokenPattern.MatchString(token) {
		return fmt.Errorf("fixture apply barrier configuration is invalid")
	}
	info, err := os.Lstat(recordDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("fixture apply barrier record directory is invalid")
	}
	readyPath := filepath.Join(recordDir, "apply-barrier."+token+".ready")
	if err := os.WriteFile(readyPath, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write fixture apply barrier marker: %w", err)
	}
	releasePath := filepath.Join(recordDir, "apply-barrier."+token+".release")
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(releasePath)
		switch {
		case err == nil && string(data) == token+"\n":
			return nil
		case err == nil:
			return fmt.Errorf("fixture apply barrier release token does not match")
		case !os.IsNotExist(err):
			return fmt.Errorf("read fixture apply barrier release: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for fixture apply barrier release: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
