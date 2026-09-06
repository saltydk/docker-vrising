package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	steamTestBuildID       = "24686592"
	steamTestDepotManifest = "1049007059193047563"
)

func TestParseInstalledBuild(t *testing.T) {
	client := newTestSteamClient(t, nil)
	installSteamFixture(t, client, "public", true, "1604030")

	got, err := client.InstalledBuild()
	if err != nil {
		t.Fatalf("InstalledBuild() error = %v", err)
	}
	want := SteamBuild{
		BuildID:       steamTestBuildID,
		DepotManifest: steamTestDepotManifest,
		Branch:        "public",
	}
	if got != want {
		t.Fatalf("InstalledBuild() = %#v, want %#v", got, want)
	}
}

func TestValidateInstalledRequiresCompleteRuntime(t *testing.T) {
	tests := []struct {
		name       string
		executable bool
		appID      string
		expected   SteamBuild
		wantErr    bool
	}{
		{name: "valid", executable: true, appID: steamRuntimeAppID, expected: testSteamBuild("public")},
		{name: "missing executable", appID: steamRuntimeAppID, expected: testSteamBuild("public"), wantErr: true},
		{name: "wrong runtime app ID", executable: true, appID: steamAppID, expected: testSteamBuild("public"), wantErr: true},
		{
			name:       "recorded build mismatch",
			executable: true,
			appID:      steamRuntimeAppID,
			expected:   SteamBuild{BuildID: "other", DepotManifest: steamTestDepotManifest, Branch: publicBranch},
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestSteamClient(t, nil)
			installSteamFixture(t, client, publicBranch, tt.executable, tt.appID)

			got, err := client.ValidateInstalled(tt.expected)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateInstalled() = %#v, %v, wantErr %t", got, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.expected {
				t.Fatalf("ValidateInstalled() = %#v, want %#v", got, tt.expected)
			}
		})
	}
}

func TestSteamInstalledManifestRejectsAmbiguousVDF(t *testing.T) {
	fixture := string(readSteamFixture(t, "appmanifest-installed.acf"))
	tests := []struct {
		name     string
		manifest string
	}{
		{
			name: "duplicate value case variant",
			manifest: strings.Replace(
				fixture,
				"\t\"buildid\"\t\t\"24686592\"",
				"\t\"buildid\"\t\t\"24686592\"\n\t\"BUILDID\"\t\t\"24686592\"",
				1,
			),
		},
		{
			name: "duplicate child case variant",
			manifest: strings.Replace(
				fixture,
				"\t\"MountedConfig\"",
				"\t\"userconfig\"\n\t{\n\t}\n\t\"MountedConfig\"",
				1,
			),
		},
		{name: "leading unquoted junk", manifest: "junk\n" + fixture},
		{name: "unquoted structural junk", manifest: strings.Replace(fixture, "\"AppState\"\n", "\"AppState\" junk\n", 1)},
		{name: "trailing structural token", manifest: fixture + "\n}\n"},
		{name: "extra root", manifest: fixture + "\n" + fixture},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestSteamClient(t, nil)
			writeInstalledManifest(t, client, []byte(tt.manifest))

			if _, err := client.InstalledBuild(); err == nil {
				t.Fatal("InstalledBuild() accepted ambiguous or malformed VDF")
			}
		})
	}
}

func TestParseRemotePublicBuildAndDepot(t *testing.T) {
	runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{
		Stdout: readSteamFixture(t, "app-info-public.txt"),
	}}}}
	client := newTestSteamClient(t, runner)

	got, err := client.RemoteBuild(t.Context())
	if err != nil {
		t.Fatalf("RemoteBuild() error = %v", err)
	}
	want := SteamBuild{
		BuildID:       steamTestBuildID,
		DepotManifest: steamTestDepotManifest,
		Branch:        "public",
	}
	if got != want {
		t.Fatalf("RemoteBuild() = %#v, want %#v", got, want)
	}
	assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
}

func TestRemoteBuildAcceptsCurrentSteamConsoleWrapper(t *testing.T) {
	output := string(readSteamFixture(t, "app-info-public.txt"))
	output = strings.Replace(output,
		"[----] Verifying installation...\n",
		"[----] Verifying installation...\nUpdateUI: skip show logo\n",
		1,
	)
	output = strings.Replace(output,
		"Loading Steam API...OK\n\nAppID :",
		"Loading Steam API...\x1b[0mOK\n\x1b[0m\"@sSteamCmdForcePlatformType\" = \"windows\"\n\x1b[0m\n"+
			"Connecting anonymously to Steam Public...\x1b[0mOK\n"+
			"\x1b[0mWaiting for client config...\x1b[0mOK\n"+
			"\x1b[0mWaiting for user info...\x1b[0mOK\n\x1b[0mAppID :",
		1,
	)
	output = strings.Replace(output, "\nSteam>\n", "\nUnloading Steam API...\x1b[0mOK\n\x1b[0m\n", 1)
	runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{Stdout: []byte(output)}}}}
	client := newTestSteamClient(t, runner)

	got, err := client.RemoteBuild(t.Context())
	if err != nil {
		t.Fatalf("RemoteBuild() rejected the current SteamCMD console wrapper: %v", err)
	}
	if got != testSteamBuild("public") {
		t.Fatalf("RemoteBuild() = %#v, want public build", got)
	}
	assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
}

func TestRemoteBuildSelectsConfiguredBranch(t *testing.T) {
	output := strings.ReplaceAll(
		string(readSteamFixture(t, "app-info-public.txt")),
		`"public"`,
		`"Legacy-1.1"`,
	)
	runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{
		Stdout: []byte(output),
	}}}}
	client := newTestSteamClient(t, runner)
	client.Branch = "Legacy-1.1"

	got, err := client.RemoteBuild(t.Context())
	if err != nil {
		t.Fatalf("RemoteBuild() error = %v", err)
	}
	want := SteamBuild{
		BuildID:       steamTestBuildID,
		DepotManifest: steamTestDepotManifest,
		Branch:        "Legacy-1.1",
	}
	if got != want {
		t.Fatalf("RemoteBuild() = %#v, want %#v", got, want)
	}
	assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
}

func TestSteamRemoteBranchLookupIsCaseSensitive(t *testing.T) {
	output := strings.ReplaceAll(
		string(readSteamFixture(t, "app-info-public.txt")),
		`"public"`,
		`"Legacy-1.1"`,
	)
	runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{Stdout: []byte(output)}}}}
	client := newTestSteamClient(t, runner)
	client.Branch = "legacy-1.1"

	if _, err := client.RemoteBuild(t.Context()); err == nil {
		t.Fatal("RemoteBuild() matched a branch with different case")
	}
	assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
}

func TestSteamRejectsUppercasePublicBranch(t *testing.T) {
	t.Run("remote selection", func(t *testing.T) {
		runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{
			Stdout: readSteamFixture(t, "app-info-public.txt"),
		}}}}
		client := newTestSteamClient(t, runner)
		client.Branch = "PUBLIC"

		if _, err := client.RemoteBuild(t.Context()); err == nil {
			t.Fatal("RemoteBuild() treated configured PUBLIC as canonical public")
		}
		assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
	})

	t.Run("update before mutation", func(t *testing.T) {
		runner := &recordingCommandRunner{}
		client := newTestSteamClient(t, runner)
		client.Branch = "PUBLIC"

		_, err := client.Update(t.Context(), testSteamBuild("PUBLIC"))
		if err == nil {
			t.Fatal("Update() accepted configured PUBLIC")
		}
		var preMutation *SteamPreMutationError
		if !errors.As(err, &preMutation) {
			t.Fatalf("Update() error = %T %v, want *SteamPreMutationError", err, err)
		}
		if len(runner.specs) != 0 {
			t.Fatalf("Update() ran commands for configured PUBLIC: %#v", runner.specs)
		}
	})
}

func TestSteamRemoteMetadataRejectsAmbiguousVDF(t *testing.T) {
	fixture := string(readSteamFixture(t, "app-info-public.txt"))
	duplicateBranch := strings.Replace(
		fixture,
		"\t\t\t\"public\"\n\t\t\t{\n\t\t\t\t\"buildid\"\t\t\"24686592\"\n\t\t\t\t\"timeupdated\"\t\t\"1786515518\"\n\t\t\t}",
		"\t\t\t\"public\"\n\t\t\t{\n\t\t\t\t\"buildid\"\t\t\"24686592\"\n\t\t\t\t\"timeupdated\"\t\t\"1786515518\"\n\t\t\t}\n\t\t\t\"PUBLIC\"\n\t\t\t{\n\t\t\t\t\"buildid\"\t\t\"99999999\"\n\t\t\t}",
		1,
	)
	tests := []struct {
		name   string
		output string
	}{
		{name: "duplicate branch case variant", output: duplicateBranch},
		{name: "unknown wrapper", output: "Injected console line\n" + fixture},
		{name: "unsupported ANSI wrapper", output: "Loading Steam API...\x1b[31mOK\n" + fixture},
		{name: "unquoted structural junk", output: strings.Replace(fixture, "\"1829350\"\n{", "\"1829350\"\njunk\n{", 1)},
		{name: "trailing structural token", output: strings.Replace(fixture, "\nSteam>\n", "\n}\nSteam>\n", 1)},
		{name: "extra root", output: strings.Replace(fixture, "\nSteam>\n", "\n\"1829350\"\n{\n}\nSteam>\n", 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{Stdout: []byte(tt.output)}}}}
			client := newTestSteamClient(t, runner)

			if _, err := client.RemoteBuild(t.Context()); err == nil {
				t.Fatal("RemoteBuild() accepted ambiguous or malformed VDF output")
			}
			assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
		})
	}
}

func TestSteamRemoteMetadataRejectsEscapedRawControlBytes(t *testing.T) {
	controls := []struct {
		name  string
		value byte
	}{
		{name: "nul", value: 0x00},
		{name: "tab", value: '\t'},
		{name: "line feed", value: '\n'},
		{name: "carriage return", value: '\r'},
		{name: "unit separator", value: 0x1f},
	}

	for _, control := range controls {
		t.Run(control.name, func(t *testing.T) {
			fixture := readSteamFixture(t, "app-info-public.txt")
			replacement := append([]byte("V Rising\\"), control.value)
			replacement = append(replacement, []byte(" Dedicated Server")...)
			output := bytes.Replace(fixture, []byte("V Rising Dedicated Server"), replacement, 1)
			runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{Stdout: output}}}}
			client := newTestSteamClient(t, runner)

			if _, err := client.RemoteBuild(t.Context()); err == nil {
				t.Fatalf("RemoteBuild() accepted escaped raw control byte 0x%02x", control.value)
			}
			assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
		})
	}
}

func TestSteamRemoteMetadataAcceptsSupportedTextualEscapes(t *testing.T) {
	for _, replacement := range [][]byte{
		[]byte("V Rising \\\\ Dedicated Server"),
		[]byte("V Rising \\\"Dedicated\\\" Server"),
	} {
		fixture := readSteamFixture(t, "app-info-public.txt")
		output := bytes.Replace(fixture, []byte("V Rising Dedicated Server"), replacement, 1)
		runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{Stdout: output}}}}
		client := newTestSteamClient(t, runner)

		got, err := client.RemoteBuild(t.Context())
		if err != nil {
			t.Fatalf("RemoteBuild() error = %v", err)
		}
		if got != testSteamBuild("public") {
			t.Fatalf("RemoteBuild() = %#v, want public build", got)
		}
		assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
	}
}

func TestSteamMetadataFailureIsPreMutation(t *testing.T) {
	runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{
		ExitCode: 7,
		Stderr:   []byte("failed to request app info"),
	}}}}
	client := newTestSteamClient(t, runner)

	got, err := client.RemoteBuild(t.Context())
	if err == nil {
		t.Fatal("RemoteBuild() succeeded after SteamCMD metadata failure")
	}
	if got != (SteamBuild{}) {
		t.Fatalf("RemoteBuild() = %#v after failure, want zero result", got)
	}
	var preMutation *SteamPreMutationError
	if !errors.As(err, &preMutation) {
		t.Fatalf("RemoteBuild() error = %T %v, want *SteamPreMutationError", err, err)
	}
	var postMutation *SteamPostMutationError
	if errors.As(err, &postMutation) {
		t.Fatalf("RemoteBuild() error = %v, unexpectedly classifies as post-mutation", err)
	}
	assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
}

func TestSteamMetadataPhaseOverridesNestedPostMutationError(t *testing.T) {
	adversarial := fmt.Errorf(
		"outer runner wrapper: %w",
		fmt.Errorf("inner runner wrapper: %w", &SteamPostMutationError{
			Err: fmt.Errorf("adversarial post phase: %w", context.Canceled),
		}),
	)
	runner := &recordingCommandRunner{results: []commandRun{{err: adversarial}}}
	client := newTestSteamClient(t, runner)

	_, err := client.RemoteBuild(t.Context())
	if err == nil {
		t.Fatal("RemoteBuild() succeeded after adversarial runner failure")
	}
	var preMutation *SteamPreMutationError
	if !errors.As(err, &preMutation) {
		t.Fatalf("RemoteBuild() error = %T %v, want *SteamPreMutationError", err, err)
	}
	var postMutation *SteamPostMutationError
	if errors.As(err, &postMutation) {
		t.Fatalf("RemoteBuild() error = %v, also exposes *SteamPostMutationError", err)
	}
	if !strings.Contains(err.Error(), "outer runner wrapper") || !strings.Contains(err.Error(), "adversarial post phase") {
		t.Fatalf("RemoteBuild() error lost diagnostic text: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RemoteBuild() error = %v, want context cancellation preserved", err)
	}
	assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
}

func TestSteamUpdateUsesLiteralArgumentSlice(t *testing.T) {
	runner := &recordingCommandRunner{results: []commandRun{{}}}
	client := newTestSteamClient(t, runner)
	client.Branch = "legacy-1.1"
	installSteamFixture(t, client, "legacy-1.1", true, "1604030")
	target := SteamBuild{
		BuildID:       steamTestBuildID,
		DepotManifest: steamTestDepotManifest,
		Branch:        "legacy-1.1",
	}

	got, err := client.Update(t.Context(), target)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got != target {
		t.Fatalf("Update() = %#v, want %#v", got, target)
	}
	assertCommandSpecs(t, runner.specs, betaUpdateSteamSpec(client, "legacy-1.1"))
}

func TestSteamUpdateRetriesThreeTimes(t *testing.T) {
	runner := &recordingCommandRunner{results: []commandRun{
		{result: CommandResult{ExitCode: 5, Stderr: []byte("network unavailable")}},
		{err: errors.New("steamcmd process failed")},
		{},
	}}
	client := newTestSteamClient(t, runner)
	installSteamFixture(t, client, "public", true, "1604030")
	target := testSteamBuild("public")

	got, err := client.Update(t.Context(), target)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got != target {
		t.Fatalf("Update() = %#v, want %#v", got, target)
	}
	wantSpec := publicUpdateSteamSpec(client)
	assertCommandSpecs(t, runner.specs, wantSpec, wantSpec, wantSpec)
	if _, err := os.Stat(filepath.Join(client.ServerDir, "steamapps", "appmanifest_1829350.acf")); err != nil {
		t.Fatalf("Steam metadata after retries: %v", err)
	}
}

func TestSteamUpdateRejectsMissingExecutable(t *testing.T) {
	runner := &recordingCommandRunner{results: []commandRun{{}}}
	client := newTestSteamClient(t, runner)
	installSteamFixture(t, client, "public", false, "1604030")

	got, err := client.Update(t.Context(), testSteamBuild("public"))
	if err == nil {
		t.Fatal("Update() succeeded without VRisingServer.exe")
	}
	if got != (SteamBuild{}) {
		t.Fatalf("Update() = %#v after failure, want zero result", got)
	}
	assertPostMutationError(t, err)
	assertCommandSpecs(t, runner.specs, publicUpdateSteamSpec(client))
}

func TestSteamUpdateRejectsNonRegularExecutable(t *testing.T) {
	tests := []struct {
		name   string
		create func(*testing.T, string) func()
	}{
		{
			name: "symlink",
			create: func(t *testing.T, path string) func() {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "real-server.exe")
				if err := os.WriteFile(target, []byte("server"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "directory",
			create: func(t *testing.T, path string) func() {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "fifo",
			create: func(t *testing.T, path string) func() {
				t.Helper()
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "socket",
			create: func(t *testing.T, path string) func() {
				t.Helper()
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				return func() {
					if err := listener.Close(); err != nil {
						t.Errorf("close Unix listener: %v", err)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingCommandRunner{results: []commandRun{{}}}
			client := newTestSteamClient(t, runner)
			installSteamFixture(t, client, "public", false, "1604030")
			cleanup := tt.create(t, filepath.Join(client.ServerDir, "VRisingServer.exe"))
			defer cleanup()

			if _, err := client.Update(t.Context(), testSteamBuild("public")); err == nil {
				t.Fatal("Update() accepted a non-regular VRisingServer.exe")
			} else {
				assertPostMutationError(t, err)
			}
			assertCommandSpecs(t, runner.specs, publicUpdateSteamSpec(client))
		})
	}
}

func TestSteamUpdateAcceptsExactRuntimeAppIDFormats(t *testing.T) {
	for _, content := range []string{"1604030", "1604030\n", "1604030\r\n"} {
		t.Run(strings.ReplaceAll(content, "\n", `\n`), func(t *testing.T) {
			runner := &recordingCommandRunner{results: []commandRun{{}}}
			client := newTestSteamClient(t, runner)
			installSteamFixture(t, client, "public", true, "1604030")
			writeSteamAppID(t, client, []byte(content))

			got, err := client.Update(t.Context(), testSteamBuild("public"))
			if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if got != testSteamBuild("public") {
				t.Fatalf("Update() = %#v, want exact target", got)
			}
			assertCommandSpecs(t, runner.specs, publicUpdateSteamSpec(client))
		})
	}
}

func TestSteamUpdateRejectsMalformedRuntimeAppID(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "leading space", content: " 1604030"},
		{name: "trailing space", content: "1604030 "},
		{name: "blank", content: ""},
		{name: "blank line", content: "1604030\n\n"},
		{name: "repeated line", content: "1604030\n1604030\n"},
		{name: "unicode whitespace", content: "1604030\u00a0"},
		{name: "trailing data", content: "1604030\nextra"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingCommandRunner{results: []commandRun{{}}}
			client := newTestSteamClient(t, runner)
			installSteamFixture(t, client, "public", true, "1604030")
			writeSteamAppID(t, client, []byte(tt.content))

			if _, err := client.Update(t.Context(), testSteamBuild("public")); err == nil {
				t.Fatal("Update() accepted malformed steam_appid.txt bytes")
			} else {
				assertPostMutationError(t, err)
			}
			assertCommandSpecs(t, runner.specs, publicUpdateSteamSpec(client))
		})
	}
}

func TestSteamUpdateRejectsUnexpectedBuild(t *testing.T) {
	tests := []struct {
		name      string
		branch    string
		appid     string
		target    SteamBuild
		transform func(string) string
	}{
		{
			name:   "build id",
			branch: "public",
			appid:  "1604030",
			target: SteamBuild{BuildID: "99999999", DepotManifest: steamTestDepotManifest, Branch: "public"},
		},
		{
			name:   "depot manifest",
			branch: "public",
			appid:  "1604030",
			target: SteamBuild{BuildID: steamTestBuildID, DepotManifest: "9999999999999999999", Branch: "public"},
		},
		{
			name:   "steam app id file",
			branch: "public",
			appid:  "1829350",
			target: testSteamBuild("public"),
		},
		{
			name:   "manifest app id",
			branch: "public",
			appid:  "1604030",
			target: testSteamBuild("public"),
			transform: func(manifest string) string {
				return strings.Replace(manifest, "\"appid\"\t\t\"1829350\"", "\"appid\"\t\t\"1604030\"", 1)
			},
		},
		{
			name:   "branch",
			branch: "legacy-1.1",
			appid:  "1604030",
			target: testSteamBuild("legacy-1.1"),
			transform: func(manifest string) string {
				return strings.Replace(manifest, "\"BetaKey\"\t\t\"legacy-1.1\"", "\"BetaKey\"\t\t\"other\"", 1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingCommandRunner{results: []commandRun{{}}}
			client := newTestSteamClient(t, runner)
			client.Branch = tt.branch
			installSteamFixture(t, client, tt.branch, true, tt.appid)
			if tt.transform != nil {
				manifestPath := filepath.Join(client.ServerDir, "steamapps", "appmanifest_1829350.acf")
				manifest, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manifestPath, []byte(tt.transform(string(manifest))), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			got, err := client.Update(t.Context(), tt.target)
			if err == nil {
				t.Fatal("Update() accepted installation metadata that disagrees with target")
			}
			if got != (SteamBuild{}) {
				t.Fatalf("Update() = %#v after failure, want zero result", got)
			}
			assertPostMutationError(t, err)
			wantSpec := publicUpdateSteamSpec(client)
			if tt.branch != "public" {
				wantSpec = betaUpdateSteamSpec(client, tt.branch)
			}
			assertCommandSpecs(t, runner.specs, wantSpec)
		})
	}
}

func TestSteamFailureAfterMutationIsFatal(t *testing.T) {
	runner := &recordingCommandRunner{results: []commandRun{
		{result: CommandResult{ExitCode: 8, Stderr: []byte("download failed")}},
		{result: CommandResult{ExitCode: 8, Stderr: []byte("download failed")}},
		{result: CommandResult{ExitCode: 8, Stderr: []byte("download failed")}},
	}}
	client := newTestSteamClient(t, runner)
	installSteamFixture(t, client, "public", true, "1604030")

	got, err := client.Update(t.Context(), testSteamBuild("public"))
	if err == nil {
		t.Fatal("Update() succeeded after three SteamCMD failures")
	}
	if got != (SteamBuild{}) {
		t.Fatalf("Update() = %#v after failure, want zero result", got)
	}
	assertPostMutationError(t, err)
	var preMutation *SteamPreMutationError
	if errors.As(err, &preMutation) {
		t.Fatalf("Update() error = %v, unexpectedly classifies as pre-mutation", err)
	}
	wantSpec := publicUpdateSteamSpec(client)
	assertCommandSpecs(t, runner.specs, wantSpec, wantSpec, wantSpec)
}

func TestSteamUpdatePhaseOverridesNestedPreMutationError(t *testing.T) {
	adversarial := fmt.Errorf(
		"outer runner wrapper: %w",
		fmt.Errorf("inner runner wrapper: %w", &SteamPreMutationError{
			Err: fmt.Errorf("adversarial pre phase: %w", context.Canceled),
		}),
	)
	runner := &recordingCommandRunner{results: []commandRun{
		{err: adversarial},
		{err: adversarial},
		{err: adversarial},
	}}
	client := newTestSteamClient(t, runner)
	installSteamFixture(t, client, "public", true, "1604030")

	_, err := client.Update(t.Context(), testSteamBuild("public"))
	if err == nil {
		t.Fatal("Update() succeeded after adversarial runner failures")
	}
	var postMutation *SteamPostMutationError
	if !errors.As(err, &postMutation) {
		t.Fatalf("Update() error = %T %v, want *SteamPostMutationError", err, err)
	}
	var preMutation *SteamPreMutationError
	if errors.As(err, &preMutation) {
		t.Fatalf("Update() error = %v, also exposes *SteamPreMutationError", err)
	}
	if !strings.Contains(err.Error(), "outer runner wrapper") || !strings.Contains(err.Error(), "adversarial pre phase") {
		t.Fatalf("Update() error lost diagnostic text: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Update() error = %v, want context cancellation preserved", err)
	}
	wantSpec := publicUpdateSteamSpec(client)
	assertCommandSpecs(t, runner.specs, wantSpec, wantSpec, wantSpec)
}

func TestSteamRejectsTargetBranchBeforeMutation(t *testing.T) {
	runner := &recordingCommandRunner{}
	client := newTestSteamClient(t, runner)
	client.Branch = "legacy-1.1"

	got, err := client.Update(t.Context(), testSteamBuild("public"))
	if err == nil {
		t.Fatal("Update() accepted a target for a different branch")
	}
	if got != (SteamBuild{}) {
		t.Fatalf("Update() = %#v after failure, want zero result", got)
	}
	var preMutation *SteamPreMutationError
	if !errors.As(err, &preMutation) {
		t.Fatalf("Update() error = %T %v, want *SteamPreMutationError", err, err)
	}
	if len(runner.specs) != 0 {
		t.Fatalf("Update() ran %d commands before rejecting target branch", len(runner.specs))
	}
}

type commandRun struct {
	result CommandResult
	err    error
}

type recordingCommandRunner struct {
	results []commandRun
	specs   []CommandSpec
}

func (r *recordingCommandRunner) Run(_ context.Context, spec CommandSpec) (CommandResult, error) {
	r.specs = append(r.specs, cloneCommandSpec(spec))
	if len(r.results) == 0 {
		return CommandResult{}, errors.New("unexpected command")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result.result, result.err
}

func cloneCommandSpec(spec CommandSpec) CommandSpec {
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = append([]string(nil), spec.Env...)
	return spec
}

func newTestSteamClient(t *testing.T, runner CommandRunner) *SteamClient {
	t.Helper()
	root := t.TempDir()
	return &SteamClient{
		Runner:    runner,
		SteamCMD:  "/usr/games/steamcmd",
		ServerDir: filepath.Join(root, "server"),
		HomeDir:   filepath.Join(root, "state", "home"),
	}
}

func installSteamFixture(t *testing.T, client *SteamClient, branch string, executable bool, appID string) {
	t.Helper()
	manifest := string(readSteamFixture(t, "appmanifest-installed.acf"))
	if branch != "" && branch != "public" {
		manifest = strings.Replace(
			manifest,
			"\"language\"\t\t\"english\"",
			"\"language\"\t\t\"english\"\n\t\t\"BetaKey\"\t\t\""+branch+"\"",
			1,
		)
	}
	manifestPath := filepath.Join(client.ServerDir, "steamapps", "appmanifest_1829350.acf")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if executable {
		if err := os.WriteFile(filepath.Join(client.ServerDir, "VRisingServer.exe"), []byte("server"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(client.ServerDir, "steam_appid.txt"), []byte(appID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSteamFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "steam", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeInstalledManifest(t *testing.T, client *SteamClient, manifest []byte) {
	t.Helper()
	manifestPath := filepath.Join(client.ServerDir, "steamapps", "appmanifest_1829350.acf")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSteamAppID(t *testing.T, client *SteamClient, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(client.ServerDir, "steam_appid.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testSteamBuild(branch string) SteamBuild {
	return SteamBuild{
		BuildID:       steamTestBuildID,
		DepotManifest: steamTestDepotManifest,
		Branch:        branch,
	}
}

func remoteSteamSpec(client *SteamClient) CommandSpec {
	return CommandSpec{
		Path: client.SteamCMD,
		Args: []string{
			"+@sSteamCmdForcePlatformType", "windows",
			"+login", "anonymous",
			"+app_info_update", "1",
			"+app_info_print", "1829350",
			"+quit",
		},
		Env: steamCommandEnvironment(client.HomeDir),
		Dir: client.HomeDir,
	}
}

func publicUpdateSteamSpec(client *SteamClient) CommandSpec {
	return CommandSpec{
		Path: client.SteamCMD,
		Args: []string{
			"+@sSteamCmdForcePlatformType", "windows",
			"+force_install_dir", client.ServerDir,
			"+login", "anonymous",
			"+app_update", "1829350",
			"validate", "+quit",
		},
		Env:          steamCommandEnvironment(client.HomeDir),
		Dir:          client.HomeDir,
		StreamOutput: true,
	}
}

func betaUpdateSteamSpec(client *SteamClient, branch string) CommandSpec {
	return CommandSpec{
		Path: client.SteamCMD,
		Args: []string{
			"+@sSteamCmdForcePlatformType", "windows",
			"+force_install_dir", client.ServerDir,
			"+login", "anonymous",
			"+app_update", "1829350",
			"-beta", branch,
			"validate", "+quit",
		},
		Env:          steamCommandEnvironment(client.HomeDir),
		Dir:          client.HomeDir,
		StreamOutput: true,
	}
}

func assertCommandSpecs(t *testing.T, got []CommandSpec, want ...CommandSpec) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command specs = %#v, want %#v", got, want)
	}
}

func assertPostMutationError(t *testing.T, err error) {
	t.Helper()
	var postMutation *SteamPostMutationError
	if !errors.As(err, &postMutation) {
		t.Fatalf("error = %T %v, want *SteamPostMutationError", err, err)
	}
}
