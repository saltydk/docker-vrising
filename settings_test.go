package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestServerSettings(t *testing.T, serverDir string) {
	t.Helper()
	for _, name := range []string{"ServerHostSettings.json", "ServerGameSettings.json"} {
		writeTestFile(t, filepath.Join(serverDir, "VRisingServer_Data", "StreamingAssets", "Settings", name), "{}\n")
	}
}

func TestSeedServerSettingsPreservesExistingOverrides(t *testing.T) {
	server, data := t.TempDir(), t.TempDir()
	writeTestServerSettings(t, server)
	custom := "{\"Name\":\"Salty\",\"SaveName\":\"existing-world\"}\n"
	host := filepath.Join(data, "Settings", "ServerHostSettings.json")
	writeTestFile(t, host, custom)
	if err := seedServerSettings(server, data); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(host); err != nil || string(got) != custom {
		t.Fatalf("existing settings changed: %q, %v", got, err)
	}
	game := filepath.Join(data, "Settings", "ServerGameSettings.json")
	if got, err := os.ReadFile(game); err != nil || string(got) != "{}\n" {
		t.Fatalf("missing settings were not seeded: %q, %v", got, err)
	}
	writeTestFile(t, filepath.Join(server, "VRisingServer_Data", "StreamingAssets", "Settings", "ServerGameSettings.json"), "{\"changed\":true}\n")
	if err := seedServerSettings(server, data); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(game); string(got) != "{}\n" {
		t.Fatal("restart replaced existing settings with updated defaults")
	}
}

func TestSeedServerSettingsRejectsSymlinkDirectory(t *testing.T) {
	server, data, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeTestServerSettings(t, server)
	if err := os.Symlink(outside, filepath.Join(data, "Settings")); err != nil {
		t.Fatal(err)
	}
	if err := seedServerSettings(server, data); err == nil {
		t.Fatal("followed symlinked Settings directory")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote outside the data mount")
	}
}
