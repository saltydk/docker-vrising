//go:build !fixture

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureApplyBarrierIsNoopInProductionBuild(t *testing.T) {
	recordDir := t.TempDir()
	t.Setenv("FIXTURE_APPLY_BARRIER", "true")
	t.Setenv("FIXTURE_RECORD_DIR", recordDir)
	t.Setenv("FIXTURE_RUN_TOKEN", "production-noop")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fixtureAfterManagedPublication(ctx, "BepInEx/plugins/saltydk-managed/KindredCommands.dll"); err != nil {
		t.Fatalf("fixtureAfterManagedPublication() error = %v, want nil production no-op", err)
	}
	if entries, err := os.ReadDir(recordDir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("production no-op created fixture artifacts: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(recordDir, "apply-barrier.production-noop.ready")); !os.IsNotExist(err) {
		t.Fatalf("production no-op marker stat error = %v, want not exist", err)
	}
}
