package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var (
	bepInExPackage = PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"}
	vcfPackage     = PackageRef{Namespace: "deca", Name: "VampireCommandFramework", Version: "0.10.4"}
	kindredPackage = PackageRef{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8"}
)

func TestStageMapsBepInExAndPluginFiles(t *testing.T) {
	manager := newTestModManager(t)
	lock, archives := newManagedArchiveSet(t, managedArchiveContents{
		bepInEx: []zipEntry{
			{name: "BepInExPack_V_Rising/.doorstop_version", body: "6.0.0"},
			{name: "BepInExPack_V_Rising/doorstop_config.ini", body: "[UnityDoorstop]"},
			{name: "BepInExPack_V_Rising/winhttp.dll", body: "proxy"},
			{name: "BepInExPack_V_Rising/dotnet/runtime.dll", body: "runtime"},
			{name: "BepInExPack_V_Rising/BepInEx/core/core.dll", body: "core"},
			{name: "BepInExPack_V_Rising/BepInEx/patchers/patcher.dll", body: "patcher"},
			{name: "BepInExPack_V_Rising/BepInEx/config/BepInEx.cfg", body: "[Logging.Console]\nEnabled = true\n"},
			{name: "BepInExPack_V_Rising/changelog.txt", body: "ignored"},
		},
		vcf: []zipEntry{{name: "VampireCommandFramework.dll", body: "vcf"}},
		kindred: []zipEntry{
			{name: "KindredCommands.dll", body: "kindred"},
			{name: "NetTopologySuite.dll", body: "topology"},
		},
	})

	staged, err := manager.Stage(t.Context(), lock, archives)
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}

	wantFiles := map[string]string{
		".doorstop_version":            "6.0.0",
		"doorstop_config.ini":          "[UnityDoorstop]",
		"winhttp.dll":                  "proxy",
		"dotnet/runtime.dll":           "runtime",
		"BepInEx/core/core.dll":        "core",
		"BepInEx/patchers/patcher.dll": "patcher",
		"BepInEx/plugins/saltydk-managed/VampireCommandFramework.dll": "vcf",
		"BepInEx/plugins/saltydk-managed/KindredCommands.dll":         "kindred",
		"BepInEx/plugins/saltydk-managed/NetTopologySuite.dll":        "topology",
	}
	if got := manifestPaths(staged.Manifest); !reflect.DeepEqual(got, sortedMapKeys(wantFiles)) {
		t.Fatalf("staged manifest paths = %v, want %v", got, sortedMapKeys(wantFiles))
	}
	for relativePath, want := range wantFiles {
		if got := readTestFile(t, filepath.Join(staged.Dir, filepath.FromSlash(relativePath))); got != want {
			t.Fatalf("staged %s = %q, want %q", relativePath, got, want)
		}
	}
	for _, excluded := range []string{"BepInEx/config/BepInEx.cfg", "changelog.txt"} {
		if _, err := os.Lstat(filepath.Join(staged.Dir, filepath.FromSlash(excluded))); !os.IsNotExist(err) {
			t.Fatalf("excluded staged path %s stat error = %v, want not exist", excluded, err)
		}
	}
	if staged.Record.ID == "" || staged.Record.ID != staged.Manifest.GenerationID || staged.Record.LockDigest != lock.Digest {
		t.Fatalf("staged record = %#v, manifest = %#v, lock digest = %q", staged.Record, staged.Manifest, lock.Digest)
	}
	if !reflect.DeepEqual(staged.Lock, lock) {
		t.Fatalf("staged lock = %#v, want %#v", staged.Lock, lock)
	}
}

func TestStageAlwaysIncludesNetTopologySuite(t *testing.T) {
	manager := newTestModManager(t)
	lock, archives := newManagedArchiveSet(t, managedArchiveContents{
		vcf:     []zipEntry{{name: "VampireCommandFramework.dll", body: "vcf"}},
		kindred: []zipEntry{{name: "KindredCommands.dll", body: "kindred"}},
	})

	_, err := manager.Stage(t.Context(), lock, archives)
	if err == nil || !strings.Contains(err.Error(), "NetTopologySuite.dll") {
		t.Fatalf("Stage() error = %v, want missing NetTopologySuite.dll rejection", err)
	}
}

func TestStageRequiresCompleteBepInExRuntime(t *testing.T) {
	for _, missing := range []string{
		"BepInExPack_V_Rising/.doorstop_version",
		"BepInExPack_V_Rising/doorstop_config.ini",
		"BepInExPack_V_Rising/winhttp.dll",
		"BepInExPack_V_Rising/dotnet/runtime.dll",
		"BepInExPack_V_Rising/BepInEx/core/core.dll",
		"BepInExPack_V_Rising/BepInEx/patchers/patcher.dll",
	} {
		t.Run(filepath.Base(missing), func(t *testing.T) {
			manager := newTestModManager(t)
			entries := defaultBepInExEntries("")
			for i := range entries {
				if entries[i].name == missing {
					entries = slices.Delete(entries, i, i+1)
					break
				}
			}
			lock, archives := newManagedArchiveSet(t, managedArchiveContents{bepInEx: entries})

			if _, err := manager.Stage(t.Context(), lock, archives); err == nil {
				t.Fatalf("Stage() succeeded without required BepInEx runtime path %s", missing)
			}
		})
	}
}

func TestStageRejectsUnmappedPluginDLL(t *testing.T) {
	manager := newTestModManager(t)
	lock, archives := newManagedArchiveSet(t, managedArchiveContents{
		vcf: []zipEntry{
			{name: "VampireCommandFramework.dll", body: "vcf"},
			{name: "Unexpected.dll", body: "unexpected"},
		},
	})

	_, err := manager.Stage(t.Context(), lock, archives)
	if err == nil || !strings.Contains(err.Error(), "Unexpected.dll") {
		t.Fatalf("Stage() error = %v, want unmapped plugin DLL rejection", err)
	}
}

func TestStageBorrowsValidatedArchivesWithoutClosingThem(t *testing.T) {
	manager := newTestModManager(t)
	lock, archives := newManagedArchiveSet(t, managedArchiveContents{})
	staged, err := manager.Stage(t.Context(), lock, archives)
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}

	if _, err := ExtractArchive(archives[vcfPackage], t.TempDir(), ExtractPlugin); err != nil {
		t.Fatalf("ExtractArchive() after Stage error = %v, want caller-owned archive still open", err)
	}
	if err := archives[vcfPackage].Close(); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(staged.Dir, "BepInEx", "plugins", "saltydk-managed", "VampireCommandFramework.dll")); got != "vcf" {
		t.Fatalf("staged VCF after caller close = %q, want vcf", got)
	}
}

func TestApplyPreservesMutableBepInExDirectories(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	mutable := map[string]string{
		"BepInEx/cache/cache.bin":            "cache",
		"BepInEx/interop/interop.dll":        "interop",
		"BepInEx/unity-libs/unity.dll":       "unity",
		"BepInEx/LogOutput.log":              "log",
		"BepInEx/config/KindredCommands.cfg": "kindred config",
	}
	for relativePath, content := range mutable {
		writeTestFile(t, filepath.Join(manager.ServerDir, filepath.FromSlash(relativePath)), content)
	}

	if err := manager.Apply(t.Context(), staged); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	for relativePath, want := range mutable {
		if got := readTestFile(t, filepath.Join(manager.ServerDir, filepath.FromSlash(relativePath))); got != want {
			t.Fatalf("mutable path %s = %q, want %q", relativePath, got, want)
		}
	}
}

func TestApplyPreservesManualPlugins(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	manualPlugin := filepath.Join(manager.ServerDir, "BepInEx", "plugins", "ManualPlugin.dll")
	manualDirectoryPlugin := filepath.Join(manager.ServerDir, "BepInEx", "plugins", "custom", "Nested.dll")
	writeTestFile(t, manualPlugin, "manual")
	writeTestFile(t, manualDirectoryPlugin, "nested manual")

	if err := manager.Apply(t.Context(), staged); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := readTestFile(t, manualPlugin); got != "manual" {
		t.Fatalf("manual plugin = %q, want unchanged", got)
	}
	if got := readTestFile(t, manualDirectoryPlugin); got != "nested manual" {
		t.Fatalf("nested manual plugin = %q, want unchanged", got)
	}
}

func TestApplyRefusesUnmanagedFileCollision(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	collision := filepath.Join(manager.ServerDir, "winhttp.dll")
	writeTestFile(t, collision, "manual proxy")

	err := manager.Apply(t.Context(), staged)
	if err == nil || !strings.Contains(err.Error(), "not in the prior managed inventory") {
		t.Fatalf("Apply() error = %v, want unmanaged collision rejection", err)
	}
	if got := readTestFile(t, collision); got != "manual proxy" {
		t.Fatalf("colliding file = %q, want unchanged", got)
	}
}

func TestBepInExConsoleLoggingIsDisabled(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	configPath := filepath.Join(manager.ServerDir, "BepInEx", "config", "BepInEx.cfg")
	want := "# header\r\n[Logging.Console]\r\nEnabled = false # keep comment\r\nOther = yes\r\n[Logging.Disk]\r\nEnabled = true\r\n"
	writeTestFile(t, configPath, "# header\r\n[Logging.Console]\r\nEnabled = true # keep comment\r\nOther = yes\r\n[Logging.Disk]\r\nEnabled = true\r\n")

	if err := manager.Apply(t.Context(), staged); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := readTestFile(t, configPath); got != want {
		t.Fatalf("BepInEx.cfg = %q, want narrow edit %q", got, want)
	}
}

func TestBepInExConsoleLoggingIsDisabledWhenConfigIsAbsent(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})

	if err := manager.Apply(t.Context(), staged); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	configPath := filepath.Join(manager.ServerDir, "BepInEx", "config", "BepInEx.cfg")
	if got := readTestFile(t, configPath); got != "[Logging.Console]\nEnabled = false\n" {
		t.Fatalf("created BepInEx.cfg = %q, want console disabled template", got)
	}
}

func TestBepInExConsoleLoggingAddsMissingEnabledKeyAtEOF(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	configPath := filepath.Join(manager.ServerDir, "BepInEx", "config", "BepInEx.cfg")
	writeTestFile(t, configPath, "[Logging.Console]")

	if err := manager.Apply(t.Context(), staged); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := readTestFile(t, configPath); got != "[Logging.Console]\nEnabled = false\n" {
		t.Fatalf("BepInEx.cfg = %q, want missing key appended on its own line", got)
	}
}

func TestApplyRejectsSymlinkedBepInExConfig(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	externalConfig := filepath.Join(t.TempDir(), "BepInEx.cfg")
	writeTestFile(t, externalConfig, "[Logging.Console]\nEnabled = true\n")
	configDir := filepath.Join(manager.ServerDir, "BepInEx", "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalConfig, filepath.Join(configDir, "BepInEx.cfg")); err != nil {
		t.Fatal(err)
	}

	if err := manager.Apply(t.Context(), staged); err == nil {
		t.Fatal("Apply() succeeded with a symlinked BepInEx config")
	}
	if got := readTestFile(t, externalConfig); got != "[Logging.Console]\nEnabled = true\n" {
		t.Fatalf("external config = %q, want unchanged", got)
	}
}

func TestApplyRemovesOnlyStaleManagedFiles(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: append(defaultBepInExEntries("first"), zipEntry{
			name: "BepInExPack_V_Rising/BepInEx/core/stale.dll", body: "stale",
		}),
	})
	applyAndPromote(t, manager, first)
	manualPlugin := filepath.Join(manager.ServerDir, "BepInEx", "plugins", "manual.dll")
	writeTestFile(t, manualPlugin, "manual")

	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	if err := manager.Apply(t.Context(), second); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(manager.ServerDir, "BepInEx", "core", "stale.dll")); !os.IsNotExist(err) {
		t.Fatalf("stale managed file stat error = %v, want not exist", err)
	}
	if got := readTestFile(t, manualPlugin); got != "manual" {
		t.Fatalf("manual plugin = %q, want unchanged", got)
	}
}

func TestApplyRefusesEditedManagedFile(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	editedPath := filepath.Join(manager.ServerDir, "BepInEx", "core", "core.dll")
	writeTestFile(t, editedPath, "operator edit")
	untouchedPath := filepath.Join(manager.ServerDir, "winhttp.dll")
	wantUntouched := readTestFile(t, untouchedPath)

	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	err := manager.Apply(t.Context(), second)
	if err == nil || !strings.Contains(err.Error(), "differs from the prior inventory") {
		t.Fatalf("Apply() error = %v, want edited managed-file rejection", err)
	}
	if got := readTestFile(t, editedPath); got != "operator edit" {
		t.Fatalf("edited managed file = %q, want operator edit preserved", got)
	}
	if got := readTestFile(t, untouchedPath); got != wantUntouched {
		t.Fatalf("other managed file = %q, want unchanged %q", got, wantUntouched)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Transaction != nil || state.Candidate != nil {
		t.Fatalf("state after rejected apply = %#v, want no candidate transaction", state)
	}
}

func TestApplyPreservesEditImmediatelyBeforeManagedInstall(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	target := filepath.Join(manager.ServerDir, "winhttp.dll")
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	manager.beforeManagedMutation = func(relativePath string) error {
		if relativePath == "winhttp.dll" {
			writeTestFile(t, target, "operator edit before install")
		}
		return nil
	}

	if err := manager.Apply(t.Context(), second); err == nil || !strings.Contains(err.Error(), "changed before install") {
		t.Fatalf("Apply() error = %v, want concurrent managed edit rejection", err)
	}
	if got := readTestFile(t, target); got != "operator edit before install" {
		t.Fatalf("concurrently edited managed file = %q, want preserved operator edit", got)
	}
}

func TestApplyPreservesCollisionImmediatelyBeforeNewInstall(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	target := filepath.Join(manager.ServerDir, ".doorstop_version")
	manager.beforeManagedMutation = func(relativePath string) error {
		if relativePath == ".doorstop_version" {
			writeTestFile(t, target, "operator collision")
		}
		return nil
	}

	if err := manager.Apply(t.Context(), staged); err == nil || !strings.Contains(err.Error(), "appeared before install") {
		t.Fatalf("Apply() error = %v, want concurrent collision rejection", err)
	}
	if got := readTestFile(t, target); got != "operator collision" {
		t.Fatalf("concurrent collision = %q, want preserved operator file", got)
	}
}

func TestApplyPreservesEditImmediatelyBeforeStaleDelete(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: append(defaultBepInExEntries("first"), zipEntry{
			name: "BepInExPack_V_Rising/BepInEx/core/stale.dll", body: "stale original",
		}),
	})
	applyAndPromote(t, manager, first)
	target := filepath.Join(manager.ServerDir, "BepInEx", "core", "stale.dll")
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	manager.beforeManagedMutation = func(relativePath string) error {
		if relativePath == "BepInEx/core/stale.dll" {
			writeTestFile(t, target, "operator edit before delete")
		}
		return nil
	}

	if err := manager.Apply(t.Context(), second); err == nil || !strings.Contains(err.Error(), "changed before delete") {
		t.Fatalf("Apply() error = %v, want concurrent stale edit rejection", err)
	}
	if got := readTestFile(t, target); got != "operator edit before delete" {
		t.Fatalf("concurrently edited stale file = %q, want preserved operator edit", got)
	}
}

func TestApplyPreservesConfigReplacementImmediatelyBeforeEdit(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	target := filepath.Join(manager.ServerDir, "BepInEx", "config", "BepInEx.cfg")
	writeTestFile(t, target, "[Logging.Console]\nEnabled = true\n")
	manager.beforeConfigMutation = func() error {
		writeTestFile(t, target, "[Logging.Console]\nEnabled = true\nOperator = changed\n")
		return nil
	}

	if err := manager.Apply(t.Context(), staged); err == nil || !strings.Contains(err.Error(), "config changed before edit") {
		t.Fatalf("Apply() error = %v, want concurrent config replacement rejection", err)
	}
	if got := readTestFile(t, target); got != "[Logging.Console]\nEnabled = true\nOperator = changed\n" {
		t.Fatalf("concurrently replaced config = %q, want preserved operator config", got)
	}
}

func TestApplyPreservesDisplacedOperatorFileWhenRestoreIsBlocked(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("first")})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("second")})
	target := filepath.Join(manager.ServerDir, "winhttp.dll")
	manager.beforeManagedMutation = func(relativePath string) error {
		if relativePath == "winhttp.dll" {
			writeTestFile(t, target, "displaced operator edit")
		}
		return nil
	}
	manager.namespaceHook = func(stage, relativePath string) error {
		if stage != "quarantined" || relativePath != "winhttp.dll" {
			return nil
		}
		state, err := manager.Store.Load()
		if err != nil {
			return err
		}
		entry := journalEntryForPath(t, state, relativePath)
		if entry.QuarantinePath == "" {
			t.Fatal("quarantine path was not journaled before namespace mutation")
		}
		if got := readTestFile(t, filepath.Join(manager.Store.StateDir, filepath.FromSlash(entry.QuarantinePath))); got != "displaced operator edit" {
			t.Fatalf("quarantined file = %q, want displaced operator edit", got)
		}
		writeTestFile(t, target, "concurrent live file")
		return nil
	}

	if err := manager.Apply(t.Context(), second); err == nil {
		t.Fatal("Apply() succeeded when displaced operator restoration was blocked")
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry := journalEntryForPath(t, state, "winhttp.dll")
	if got := readTestFile(t, target); got != "concurrent live file" {
		t.Fatalf("live file = %q, want concurrent live file preserved", got)
	}
	if got := readTestFile(t, filepath.Join(manager.Store.StateDir, filepath.FromSlash(entry.QuarantinePath))); got != "displaced operator edit" {
		t.Fatalf("displaced file = %q, want operator edit preserved in quarantine", got)
	}
}

func TestApplyStaleDeletePreservesConcurrentLiveReplacement(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: append(defaultBepInExEntries("first"), zipEntry{
			name: "BepInExPack_V_Rising/BepInEx/core/stale.dll", body: "stale original",
		}),
	})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("second")})
	target := filepath.Join(manager.ServerDir, "BepInEx", "core", "stale.dll")
	manager.namespaceHook = func(stage, relativePath string) error {
		if stage == "quarantined" && relativePath == "BepInEx/core/stale.dll" {
			writeTestFile(t, target, "concurrent replacement")
		}
		return nil
	}

	if err := manager.Apply(t.Context(), second); err == nil {
		t.Fatal("Apply() succeeded after a concurrent stale-path replacement")
	}
	if got := readTestFile(t, target); got != "concurrent replacement" {
		t.Fatalf("concurrent stale-path replacement = %q, want preserved", got)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry := journalEntryForPath(t, state, "BepInEx/core/stale.dll")
	if got := readTestFile(t, filepath.Join(manager.Store.StateDir, filepath.FromSlash(entry.QuarantinePath))); got != "stale original" {
		t.Fatalf("quarantined stale file = %q, want original", got)
	}
}

func TestRecoverInterruptedStaleQuarantineTransition(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: append(defaultBepInExEntries("first"), zipEntry{
			name: "BepInExPack_V_Rising/BepInEx/core/stale.dll", body: "stale original",
		}),
	})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("second")})
	target := filepath.Join(manager.ServerDir, "BepInEx", "core", "stale.dll")
	manager.namespaceHook = func(stage, relativePath string) error {
		if stage == "quarantined" && relativePath == "BepInEx/core/stale.dll" {
			return errors.New("crash after stale quarantine")
		}
		return nil
	}

	if err := manager.Apply(t.Context(), second); err == nil || !strings.Contains(err.Error(), "crash after stale quarantine") {
		t.Fatalf("Apply() error = %v, want quarantine crash", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("stale live path after crash stat error = %v, want not exist", err)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry := journalEntryForPath(t, state, "BepInEx/core/stale.dll")
	if got := readTestFile(t, filepath.Join(manager.Store.StateDir, filepath.FromSlash(entry.QuarantinePath))); got != "stale original" {
		t.Fatalf("quarantined stale file = %q, want original", got)
	}

	restarted := &ModManager{ServerDir: manager.ServerDir, GenerationsDir: manager.GenerationsDir, Store: manager.Store}
	if err := restarted.Rollback(t.Context()); err != nil {
		t.Fatalf("Rollback() after quarantine crash error = %v", err)
	}
	if got := readTestFile(t, target); got != "stale original" {
		t.Fatalf("recovered stale file = %q, want original", got)
	}
}

func TestRollbackRecoversInterruptedCandidateTombstoneTransition(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("first")})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("second")})
	if err := manager.Apply(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	manager.Store.recoveryHook = func(stage, relativePath string) error {
		if stage == "tombstoned" && relativePath == "winhttp.dll" {
			return errors.New("crash after candidate tombstone")
		}
		return nil
	}

	if err := manager.Rollback(t.Context()); err == nil || !strings.Contains(err.Error(), "crash after candidate tombstone") {
		t.Fatalf("Rollback() error = %v, want tombstone crash", err)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry := journalEntryForPath(t, state, "winhttp.dll")
	if _, err := os.Lstat(filepath.Join(manager.ServerDir, "winhttp.dll")); !os.IsNotExist(err) {
		t.Fatalf("live candidate after tombstone crash stat error = %v, want not exist", err)
	}
	if got := readTestFile(t, filepath.Join(manager.Store.StateDir, filepath.FromSlash(entry.QuarantinePath))); got != "proxy-first" {
		t.Fatalf("quarantined original = %q, want first generation", got)
	}
	if got := readTestFile(t, filepath.Join(manager.Store.StateDir, filepath.FromSlash(entry.TombstonePath))); got != "proxy-second" {
		t.Fatalf("candidate tombstone = %q, want second generation", got)
	}

	manager.Store.recoveryHook = nil
	if err := manager.Rollback(t.Context()); err != nil {
		t.Fatalf("Rollback() retry error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(manager.ServerDir, "winhttp.dll")); got != "proxy-first" {
		t.Fatalf("recovered live file = %q, want first generation", got)
	}
	for _, artifact := range []string{entry.QuarantinePath, entry.TombstonePath} {
		if _, err := os.Lstat(filepath.Join(manager.Store.StateDir, filepath.FromSlash(artifact))); !os.IsNotExist(err) {
			t.Fatalf("recovered artifact %s stat error = %v, want not exist", artifact, err)
		}
	}
}

func TestRollbackRestoresPreviousGeneration(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	if err := manager.Apply(t.Context(), second); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if err := manager.Rollback(t.Context()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(manager.ServerDir, "winhttp.dll")); got != "proxy-first" {
		t.Fatalf("rolled-back winhttp.dll = %q, want first generation", got)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.ID != first.Record.ID || state.Candidate != nil || state.Transaction != nil {
		t.Fatalf("rolled-back state = %#v, want first active and no candidate transaction", state)
	}
	if state.Failed == nil || state.Failed.ID != second.Record.ID || state.Failed.Status != "failed" {
		t.Fatalf("failed generation = %#v, want second generation failure evidence", state.Failed)
	}
	if err := manager.Rollback(t.Context()); err != nil {
		t.Fatalf("second Rollback() error = %v", err)
	}
}

func TestRecoverAfterInterruptedApply(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	mutations := 0
	manager.applyFileHook = func(string) error {
		mutations++
		if mutations == 1 {
			return errors.New("simulated interruption")
		}
		return nil
	}
	if err := manager.Apply(t.Context(), second); err == nil || !strings.Contains(err.Error(), "simulated interruption") {
		t.Fatalf("Apply() error = %v, want simulated interruption", err)
	}

	restarted := &ModManager{ServerDir: manager.ServerDir, GenerationsDir: manager.GenerationsDir, Store: manager.Store}
	if err := restarted.Rollback(t.Context()); err != nil {
		t.Fatalf("Rollback() after restart error = %v", err)
	}
	for _, file := range first.Manifest.Files {
		got := readTestFile(t, filepath.Join(manager.ServerDir, filepath.FromSlash(file.RelativePath)))
		want := readTestFile(t, filepath.Join(first.Dir, filepath.FromSlash(file.RelativePath)))
		if got != want {
			t.Fatalf("recovered %s = %q, want %q", file.RelativePath, got, want)
		}
	}
}

func TestRecoverAfterInterruptedFirstApply(t *testing.T) {
	manager := newTestModManager(t)
	staged := stageTestGeneration(t, manager, managedArchiveContents{})
	manager.applyFileHook = func(string) error {
		return errors.New("simulated first-install interruption")
	}
	if err := manager.Apply(t.Context(), staged); err == nil || !strings.Contains(err.Error(), "simulated first-install interruption") {
		t.Fatalf("Apply() error = %v, want first-install interruption", err)
	}

	restarted := &ModManager{ServerDir: manager.ServerDir, GenerationsDir: manager.GenerationsDir, Store: manager.Store}
	if err := restarted.Rollback(t.Context()); err != nil {
		t.Fatalf("Rollback() after first-install interruption error = %v", err)
	}
	for _, file := range staged.Manifest.Files {
		if _, err := os.Lstat(filepath.Join(manager.ServerDir, filepath.FromSlash(file.RelativePath))); !os.IsNotExist(err) {
			t.Fatalf("rolled-back first-install path %s stat error = %v, want not exist", file.RelativePath, err)
		}
	}
}

func TestDisableModsPreservesGenerationsAndConfig(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("candidate"),
	})
	if err := manager.Apply(t.Context(), second); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := manager.Rollback(t.Context()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}

	for _, generation := range []StagedGeneration{first, second} {
		if info, err := os.Stat(generation.Dir); err != nil || !info.IsDir() {
			t.Fatalf("preserved generation %s: info=%v err=%v", generation.Record.ID, info, err)
		}
	}
	config := readTestFile(t, filepath.Join(manager.ServerDir, "BepInEx", "config", "BepInEx.cfg"))
	if !strings.Contains(config, "Enabled = false") {
		t.Fatalf("preserved config = %q, want disabled console setting", config)
	}
}

func TestPromoteAdvancesActiveAndPreviousGeneration(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	applyAndPromote(t, manager, second)

	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.ID != second.Record.ID || state.Active.Status != "active" {
		t.Fatalf("active generation = %#v, want second active", state.Active)
	}
	if state.Previous == nil || state.Previous.ID != first.Record.ID || state.Previous.Status != "previous" {
		t.Fatalf("previous generation = %#v, want first previous", state.Previous)
	}
	if state.Candidate != nil || state.Transaction != nil {
		t.Fatalf("promoted state retained candidate transaction: %#v", state)
	}
	lock, err := manager.Store.LoadPackageLock()
	if err != nil {
		t.Fatal(err)
	}
	if !samePackageLock(lock, second.Lock) {
		t.Fatalf("active package lock = %#v, want second lock %#v", lock, second.Lock)
	}
}

func TestPromotionRetainsFailedEvidenceUntilLaterSuccess(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	failed := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("failed"),
	})
	if err := manager.Apply(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	if err := manager.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(failed.Dir); err != nil {
		t.Fatalf("failed generation was not retained: %v", err)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Failed == nil || state.Failed.ID != failed.Record.ID {
		t.Fatalf("failed evidence = %#v, want %s", state.Failed, failed.Record.ID)
	}

	success := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("success"),
	})
	applyAndPromote(t, manager, success)
	state, err = manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Failed != nil {
		t.Fatalf("failed evidence after later promotion = %#v, want nil", state.Failed)
	}
	if _, err := os.Stat(failed.Dir); !os.IsNotExist(err) {
		t.Fatalf("obsolete failed generation stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(first.Dir); err != nil {
		t.Fatalf("previous generation was not retained: %v", err)
	}
}

func TestPromotionReconcilesLockWrittenBeforeState(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	if err := manager.Apply(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	manager.promotionHook = func(stage string) error {
		if stage == "lock-written" {
			return errors.New("crash after lock write")
		}
		return nil
	}

	if err := manager.Promote(t.Context(), second); err == nil || !strings.Contains(err.Error(), "crash after lock write") {
		t.Fatalf("Promote() error = %v, want lock-write crash", err)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.ID != first.Record.ID || state.Candidate == nil || state.Candidate.ID != second.Record.ID {
		t.Fatalf("state at lock-write crash = %#v, want first active and second candidate", state)
	}
	if state.Promotion == nil || state.Promotion.GenerationID != second.Record.ID {
		t.Fatalf("promotion intent = %#v, want second generation", state.Promotion)
	}
	lock, err := manager.Store.LoadPackageLock()
	if err != nil {
		t.Fatal(err)
	}
	if !samePackageLock(lock, second.Lock) {
		t.Fatalf("lock at crash = %#v, want second lock", lock)
	}
	if err := manager.Store.RecoverInterruptedTransaction(); err == nil {
		t.Fatal("Store recovery rolled back an overlay with pending promotion intent")
	} else {
		var pending *PendingPromotionError
		if !errors.As(err, &pending) || pending.GenerationID != second.Record.ID {
			t.Fatalf("Store recovery error = %v, want typed pending promotion for %s", err, second.Record.ID)
		}
	}
	if got := readTestFile(t, filepath.Join(manager.ServerDir, "winhttp.dll")); got != "proxy-second" {
		t.Fatalf("overlay after blocked Store recovery = %q, want promoted candidate preserved", got)
	}

	restarted := &ModManager{ServerDir: manager.ServerDir, GenerationsDir: manager.GenerationsDir, Store: manager.Store}
	if err := restarted.Rollback(t.Context()); err != nil {
		t.Fatalf("restart reconciliation error = %v", err)
	}
	state, err = manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.ID != second.Record.ID || state.Previous == nil || state.Previous.ID != first.Record.ID ||
		state.Candidate != nil || state.Transaction != nil || state.Promotion != nil {
		t.Fatalf("reconciled promotion state = %#v", state)
	}
	lock, err = manager.Store.LoadPackageLock()
	if err != nil {
		t.Fatal(err)
	}
	if !samePackageLock(lock, second.Lock) {
		t.Fatalf("reconciled lock = %#v, want second lock", lock)
	}
}

func TestPromotionRestartCompletesPendingCleanup(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})
	applyAndPromote(t, manager, second)
	third := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("third"),
	})
	if err := manager.Apply(t.Context(), third); err != nil {
		t.Fatal(err)
	}
	manager.promotionHook = func(stage string) error {
		if stage == "state-written" {
			return errors.New("crash before cleanup")
		}
		return nil
	}

	if err := manager.Promote(t.Context(), third); err == nil || !strings.Contains(err.Error(), "crash before cleanup") {
		t.Fatalf("Promote() error = %v, want post-state crash", err)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.ID != third.Record.ID || state.Promotion == nil ||
		!reflect.DeepEqual(state.PendingCleanup, []string{first.Record.ID}) {
		t.Fatalf("state before cleanup = %#v, want third active and first pending", state)
	}
	if _, err := os.Stat(first.Dir); err != nil {
		t.Fatalf("pending cleanup generation was removed before cleanup: %v", err)
	}

	restarted := &ModManager{ServerDir: manager.ServerDir, GenerationsDir: manager.GenerationsDir, Store: manager.Store}
	if err := restarted.Rollback(t.Context()); err != nil {
		t.Fatalf("restart cleanup error = %v", err)
	}
	if _, err := os.Stat(first.Dir); !os.IsNotExist(err) {
		t.Fatalf("cleaned generation stat error = %v, want not exist", err)
	}
	state, err = manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Promotion != nil || len(state.PendingCleanup) != 0 || state.Active == nil || state.Active.ID != third.Record.ID ||
		state.Previous == nil || state.Previous.ID != second.Record.ID {
		t.Fatalf("state after restart cleanup = %#v", state)
	}
}

func TestPromotionFinalSchemaValidatesBeforeAnyWrite(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for _, invalid := range []string{"late noncanonical artifact", "missing tombstone hash", "permissive namespace", "changed artifact"} {
			t.Run(fmt.Sprintf("restart=%t/%s", restart, invalid), func(t *testing.T) {
				manager := newTestModManager(t)
				first := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("first")})
				applyAndPromote(t, manager, first)
				second := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("second")})
				if err := manager.Apply(t.Context(), second); err != nil {
					t.Fatal(err)
				}
				state, err := manager.Store.Load()
				if err != nil {
					t.Fatal(err)
				}
				entry := &state.Transaction.Entries[len(state.Transaction.Entries)-1]
				switch invalid {
				case "late noncanonical artifact":
					entry.QuarantinePath = "operator.displaced"
				case "missing tombstone hash":
					entry.TombstoneSHA256 = ""
				case "permissive namespace":
					if err := os.Chmod(filepath.Dir(filepath.Join(manager.Store.StateDir, entry.QuarantinePath)), 0o755); err != nil {
						t.Fatal(err)
					}
				case "changed artifact":
					writeTestFile(t, filepath.Join(manager.Store.StateDir, entry.QuarantinePath), "operator edit")
				}
				if restart {
					state.Promotion = &PromotionJournal{GenerationID: second.Record.ID, Lock: second.Lock}
				}
				if err := manager.Store.saveJSON("state.json", state); err != nil {
					t.Fatal(err)
				}
				before := transactionTree(t, manager.ServerDir)
				if restart {
					err = manager.Rollback(t.Context())
				} else {
					err = manager.Promote(t.Context(), second)
				}
				if err == nil {
					t.Error("promotion accepted invalid transaction")
				}
				if after := transactionTree(t, manager.ServerDir); !reflect.DeepEqual(before, after) {
					t.Error("promotion changed package lock, state, or artifacts before rejecting the complete journal")
				}
			})
		}
	}
}

func TestPromotionCleanupRechecksArtifactImmediatelyBeforeDelete(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("first")})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("second")})
	if err := manager.Apply(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry := journalEntryForPath(t, state, "winhttp.dll")
	quarantine := filepath.Join(manager.Store.StateDir, filepath.FromSlash(entry.QuarantinePath))
	manager.Store.artifactCleanupHook = func(relativePath string) error {
		if relativePath == entry.QuarantinePath {
			writeTestFile(t, quarantine, "operator changed quarantine")
		}
		return nil
	}

	if err := manager.Promote(t.Context(), second); err == nil {
		t.Fatal("Promote() deleted an artifact changed immediately before cleanup")
	}
	if got := readTestFile(t, quarantine); got != "operator changed quarantine" {
		t.Fatalf("changed quarantine = %q, want preserved operator content", got)
	}
	state, err = manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Promotion == nil || state.Transaction == nil {
		t.Fatalf("state after rejected artifact cleanup = %#v, want durable promotion and transaction", state)
	}
}

func TestApplyCompletesPendingPromotionCleanupBeforeNewCandidate(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("first")})
	applyAndPromote(t, manager, first)
	second := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("second")})
	applyAndPromote(t, manager, second)
	third := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("third")})
	if err := manager.Apply(t.Context(), third); err != nil {
		t.Fatal(err)
	}
	manager.promotionHook = func(stage string) error {
		if stage == "state-written" {
			return errors.New("crash before cleanup")
		}
		return nil
	}
	if err := manager.Promote(t.Context(), third); err == nil {
		t.Fatal("Promote() succeeded despite injected cleanup crash")
	}
	fourth := stageTestGeneration(t, manager, managedArchiveContents{bepInEx: defaultBepInExEntries("fourth")})
	restarted := &ModManager{ServerDir: manager.ServerDir, GenerationsDir: manager.GenerationsDir, Store: manager.Store}

	if err := restarted.Apply(t.Context(), fourth); err != nil {
		t.Fatalf("Apply() after pending promotion error = %v", err)
	}
	if _, err := os.Stat(first.Dir); !os.IsNotExist(err) {
		t.Fatalf("pending cleanup generation stat error = %v, want not exist", err)
	}
	state, err := manager.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Promotion != nil || len(state.PendingCleanup) != 0 || state.Candidate == nil || state.Candidate.ID != fourth.Record.ID {
		t.Fatalf("state after gated Apply = %#v, want completed promotion and fourth candidate", state)
	}
}

func TestApplyDoesNotFollowManagedDirectorySymlink(t *testing.T) {
	manager := newTestModManager(t)
	first := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("first"),
	})
	applyAndPromote(t, manager, first)
	coreDir := filepath.Join(manager.ServerDir, "BepInEx", "core")
	if err := os.Rename(coreDir, coreDir+".held"); err != nil {
		t.Fatal(err)
	}
	externalDir := t.TempDir()
	externalCore := filepath.Join(externalDir, "core.dll")
	writeTestFile(t, externalCore, "external")
	if err := os.Symlink(externalDir, coreDir); err != nil {
		t.Fatal(err)
	}
	second := stageTestGeneration(t, manager, managedArchiveContents{
		bepInEx: defaultBepInExEntries("second"),
	})

	if err := manager.Apply(t.Context(), second); err == nil {
		t.Fatal("Apply() succeeded through a managed directory symlink")
	}
	if got := readTestFile(t, externalCore); got != "external" {
		t.Fatalf("external core = %q, want unchanged", got)
	}
}

type managedArchiveContents struct {
	bepInEx []zipEntry
	vcf     []zipEntry
	kindred []zipEntry
}

func newManagedArchiveSet(t *testing.T, contents managedArchiveContents) (PackageLock, map[PackageRef]*ValidatedArchive) {
	t.Helper()
	if contents.bepInEx == nil {
		contents.bepInEx = defaultBepInExEntries("")
	}
	if contents.vcf == nil {
		contents.vcf = []zipEntry{{name: "VampireCommandFramework.dll", body: "vcf"}}
	}
	if contents.kindred == nil {
		contents.kindred = []zipEntry{
			{name: "KindredCommands.dll", body: "kindred"},
			{name: "NetTopologySuite.dll", body: "topology"},
		}
	}

	bepInEx := fetchArchiveBytesForMods(t, bepInExPackage, nil, contents.bepInEx)
	vcf := fetchArchiveBytesForMods(t, vcfPackage, []PackageRef{bepInExPackage}, contents.vcf)
	kindred := fetchArchiveBytesForMods(t, kindredPackage, []PackageRef{bepInExPackage, vcfPackage}, contents.kindred)
	archives := map[PackageRef]*ValidatedArchive{
		bepInExPackage: bepInEx,
		vcfPackage:     vcf,
		kindredPackage: kindred,
	}
	for _, archive := range archives {
		t.Cleanup(func() { _ = archive.Close() })
	}
	lock := PackageLock{
		SchemaVersion: schemaVersion,
		Root:          kindredPackage,
		Packages: []LockedPackage{
			bepInEx.LockedPackage(),
			vcf.LockedPackage(),
			kindred.LockedPackage(),
		},
	}
	lock.Digest = PackageLockDigest(lock)
	return lock, archives
}

func defaultBepInExEntries(suffix string) []zipEntry {
	return []zipEntry{
		{name: "BepInExPack_V_Rising/.doorstop_version", body: "6.0.0" + suffix},
		{name: "BepInExPack_V_Rising/doorstop_config.ini", body: "[UnityDoorstop]" + suffix},
		{name: "BepInExPack_V_Rising/winhttp.dll", body: "proxy-" + suffix},
		{name: "BepInExPack_V_Rising/dotnet/runtime.dll", body: "runtime-" + suffix},
		{name: "BepInExPack_V_Rising/BepInEx/core/core.dll", body: "core-" + suffix},
		{name: "BepInExPack_V_Rising/BepInEx/patchers/patcher.dll", body: "patcher-" + suffix},
		{name: "BepInExPack_V_Rising/BepInEx/config/BepInEx.cfg", body: "[Logging.Console]\nEnabled = true\n"},
	}
}

func fetchArchiveBytesForMods(t *testing.T, ref PackageRef, dependencies []PackageRef, entries []zipEntry) *ValidatedArchive {
	t.Helper()
	body := packageZIP(t, ref, dependencies, entries...)
	archive, err := fetchArchiveBytes(t, body, ref, dependencies)
	if err != nil {
		t.Fatalf("fetch validated archive %s: %v", packageVersionFullName(ref), err)
	}
	return archive
}

func newTestModManager(t *testing.T) *ModManager {
	t.Helper()
	serverDir := t.TempDir()
	stateDir := filepath.Join(serverDir, ".docker-vrising")
	return &ModManager{
		ServerDir:      serverDir,
		GenerationsDir: filepath.Join(stateDir, "generations"),
		Store:          &Store{StateDir: stateDir},
	}
}

func stageTestGeneration(t *testing.T, manager *ModManager, contents managedArchiveContents) StagedGeneration {
	t.Helper()
	lock, archives := newManagedArchiveSet(t, contents)
	staged, err := manager.Stage(t.Context(), lock, archives)
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	return staged
}

func applyAndPromote(t *testing.T, manager *ModManager, staged StagedGeneration) {
	t.Helper()
	if err := manager.Apply(t.Context(), staged); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := manager.Promote(t.Context(), staged); err != nil {
		t.Fatalf("Promote() error = %v", err)
	}
}

func journalEntryForPath(t *testing.T, state State, relativePath string) JournalEntry {
	t.Helper()
	if state.Transaction == nil {
		t.Fatal("transaction journal is nil")
	}
	for _, entry := range state.Transaction.Entries {
		if entry.RelativePath == relativePath {
			return entry
		}
	}
	t.Fatalf("transaction journal has no entry for %s", relativePath)
	return JournalEntry{}
}

func manifestPaths(manifest ManagedManifest) []string {
	paths := make([]string, len(manifest.Files))
	for i, file := range manifest.Files {
		paths[i] = file.RelativePath
	}
	return paths
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
