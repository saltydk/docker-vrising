package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	steamTestBuildID       = "24686592"
	steamTestDepotManifest = "1049007059193047563"
)

func TestParseInstalledBuild(t *testing.T) {
	client := newTestSteamClient(t, nil)
	installSteamFixture(t, client, "public", true, "1829350")

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

func TestRemoteBuildSelectsConfiguredBranch(t *testing.T) {
	output := strings.ReplaceAll(
		string(readSteamFixture(t, "app-info-public.txt")),
		`"public"`,
		`"legacy-1.1"`,
	)
	runner := &recordingCommandRunner{results: []commandRun{{result: CommandResult{
		Stdout: []byte(output),
	}}}}
	client := newTestSteamClient(t, runner)
	client.Branch = "legacy-1.1"

	got, err := client.RemoteBuild(t.Context())
	if err != nil {
		t.Fatalf("RemoteBuild() error = %v", err)
	}
	want := SteamBuild{
		BuildID:       steamTestBuildID,
		DepotManifest: steamTestDepotManifest,
		Branch:        "legacy-1.1",
	}
	if got != want {
		t.Fatalf("RemoteBuild() = %#v, want %#v", got, want)
	}
	assertCommandSpecs(t, runner.specs, remoteSteamSpec(client))
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

func TestSteamUpdateUsesLiteralArgumentSlice(t *testing.T) {
	runner := &recordingCommandRunner{results: []commandRun{{}}}
	client := newTestSteamClient(t, runner)
	client.Branch = "legacy-1.1"
	installSteamFixture(t, client, "legacy-1.1", true, "1829350")
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
	installSteamFixture(t, client, "public", true, "1829350")
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
	installSteamFixture(t, client, "public", false, "1829350")

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
			appid:  "1829350",
			target: SteamBuild{BuildID: "99999999", DepotManifest: steamTestDepotManifest, Branch: "public"},
		},
		{
			name:   "depot manifest",
			branch: "public",
			appid:  "1829350",
			target: SteamBuild{BuildID: steamTestBuildID, DepotManifest: "9999999999999999999", Branch: "public"},
		},
		{
			name:   "steam app id file",
			branch: "public",
			appid:  "1604030",
			target: testSteamBuild("public"),
		},
		{
			name:   "manifest app id",
			branch: "public",
			appid:  "1829350",
			target: testSteamBuild("public"),
			transform: func(manifest string) string {
				return strings.Replace(manifest, "\"appid\"\t\t\"1829350\"", "\"appid\"\t\t\"1604030\"", 1)
			},
		},
		{
			name:   "branch",
			branch: "legacy-1.1",
			appid:  "1829350",
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
	installSteamFixture(t, client, "public", true, "1829350")

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
		Env: []string{"HOME=" + client.HomeDir},
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
		Env: []string{"HOME=" + client.HomeDir},
		Dir: client.HomeDir,
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
		Env: []string{"HOME=" + client.HomeDir},
		Dir: client.HomeDir,
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
