//go:build fixture

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFixtureApplyBarrierPublishesTokenAndWaitsForRelease(t *testing.T) {
	recordDir := t.TempDir()
	const token = "fixture-barrier-test"
	t.Setenv("FIXTURE_APPLY_BARRIER", "true")
	t.Setenv("FIXTURE_RECORD_DIR", recordDir)
	t.Setenv("FIXTURE_RUN_TOKEN", token)

	result := make(chan error, 1)
	go func() {
		result <- fixtureAfterManagedPublication(t.Context(), "BepInEx/plugins/saltydk-managed/KindredCommands.dll")
	}()

	readyPath := filepath.Join(recordDir, "apply-barrier."+token+".ready")
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(readyPath)
		if err == nil {
			if string(data) != token+"\n" {
				t.Fatalf("ready marker = %q, want exact token", data)
			}
			break
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture barrier did not publish its ready marker")
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case err := <-result:
		t.Fatalf("fixture barrier returned before release: %v", err)
	default:
	}
	releasePath := filepath.Join(recordDir, "apply-barrier."+token+".release")
	if err := os.WriteFile(releasePath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("fixture barrier release error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fixture barrier did not observe release")
	}
}

func TestFixtureApplyBarrierIgnoresOtherManagedFiles(t *testing.T) {
	recordDir := t.TempDir()
	t.Setenv("FIXTURE_APPLY_BARRIER", "true")
	t.Setenv("FIXTURE_RECORD_DIR", recordDir)
	t.Setenv("FIXTURE_RUN_TOKEN", "wrong-path")

	if err := fixtureAfterManagedPublication(t.Context(), "BepInEx/plugins/saltydk-managed/HookDOTS.API.dll"); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(recordDir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("non-target publication created fixture artifacts: %v", entries)
	}
}

func TestFixtureApplyBarrierRejectsMismatchedReleaseToken(t *testing.T) {
	recordDir := t.TempDir()
	const token = "fixture-release-token"
	t.Setenv("FIXTURE_APPLY_BARRIER", "true")
	t.Setenv("FIXTURE_RECORD_DIR", recordDir)
	t.Setenv("FIXTURE_RUN_TOKEN", token)

	if err := os.WriteFile(filepath.Join(recordDir, "apply-barrier."+token+".release"), []byte("wrong-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	err := fixtureAfterManagedPublication(ctx, "BepInEx/plugins/saltydk-managed/KindredCommands.dll")
	if err == nil {
		t.Fatal("fixture barrier accepted a mismatched release token")
	}
}
