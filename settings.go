package main

import (
	"fmt"
	"path/filepath"
)

// seedServerSettings copies the game's defaults only when a persistent override
// is absent, matching the upstream container without overwriting user settings.
func seedServerSettings(serverDir, dataDir string) error {
	for _, name := range []string{"ServerHostSettings.json", "ServerGameSettings.json"} {
		target := "Settings/" + name
		if _, exists, err := snapshotFileBelow(dataDir, target); err != nil {
			return fmt.Errorf("inspect persistent %s: %w", name, err)
		} else if exists {
			continue
		}
		data, mode, err := readFileBelow(serverDir, filepath.Join("VRisingServer_Data", "StreamingAssets", "Settings", name))
		if err != nil {
			return fmt.Errorf("read default %s: %w", name, err)
		}
		if err := publishServerFile(dataDir, target, data, mode.Perm(), "persistent settings appeared during initialization"); err != nil {
			return fmt.Errorf("initialize %s: %w", name, err)
		}
	}
	return nil
}
