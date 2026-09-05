package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	steamAppID   = "1829350"
	steamDepotID = "1829351"
	publicBranch = "public"
	updateTries  = 3
)

type CommandSpec struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

type CommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

type CommandRunner interface {
	Run(context.Context, CommandSpec) (CommandResult, error)
}

type SteamBuild struct {
	BuildID       string
	DepotManifest string
	Branch        string
}

type SteamClient struct {
	Runner    CommandRunner
	SteamCMD  string
	ServerDir string
	HomeDir   string
	Branch    string
}

// SteamPreMutationError reports a Steam failure known to have occurred before
// app_update was invoked, so callers may safely consider an installed fallback.
type SteamPreMutationError struct {
	Err error
}

func (e *SteamPreMutationError) Error() string {
	return "steam failure before mutation: " + e.Err.Error()
}

func (e *SteamPreMutationError) Unwrap() error {
	return e.Err
}

// SteamPostMutationError reports a terminal failure after app_update was first
// invoked, so callers must not trust the live installation as a fallback.
type SteamPostMutationError struct {
	Err error
}

func (e *SteamPostMutationError) Error() string {
	return "steam failure after mutation began: " + e.Err.Error()
}

func (e *SteamPostMutationError) Unwrap() error {
	return e.Err
}

func (s *SteamClient) InstalledBuild() (SteamBuild, error) {
	build, err := s.readInstalledBuild()
	if err != nil {
		return SteamBuild{}, preMutationSteamError(fmt.Errorf("read installed build: %w", err))
	}
	return build, nil
}

func (s *SteamClient) RemoteBuild(ctx context.Context) (SteamBuild, error) {
	if err := s.validateCommandConfiguration(ctx); err != nil {
		return SteamBuild{}, preMutationSteamError(err)
	}
	result, err := s.Runner.Run(ctx, s.remoteBuildCommand())
	if err := steamCommandError(result, err); err != nil {
		return SteamBuild{}, preMutationSteamError(fmt.Errorf("query remote build: %w", err))
	}
	build, err := parseRemoteBuild(result.Stdout, s.selectedBranch())
	if err != nil {
		return SteamBuild{}, preMutationSteamError(fmt.Errorf("parse remote build: %w", err))
	}
	return build, nil
}

func (s *SteamClient) Update(ctx context.Context, target SteamBuild) (SteamBuild, error) {
	if err := s.validateCommandConfiguration(ctx); err != nil {
		return SteamBuild{}, preMutationSteamError(err)
	}
	target.Branch = selectedSteamBranch(target.Branch)
	if target.BuildID == "" || target.DepotManifest == "" {
		return SteamBuild{}, preMutationSteamError(fmt.Errorf("target Steam build metadata is incomplete"))
	}
	if target.Branch != s.selectedBranch() {
		return SteamBuild{}, preMutationSteamError(fmt.Errorf(
			"target branch %q does not match configured branch %q",
			target.Branch,
			s.selectedBranch(),
		))
	}

	spec := s.updateCommand(target.Branch)
	var lastErr error
	for attempt := range updateTries {
		result, runErr := s.Runner.Run(ctx, spec)
		if err := steamCommandError(result, runErr); err != nil {
			lastErr = fmt.Errorf("app_update attempt %d of %d: %w", attempt+1, updateTries, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}

		installed, err := s.validateUpdatedInstallation(target)
		if err != nil {
			return SteamBuild{}, postMutationSteamError(err)
		}
		return installed, nil
	}
	return SteamBuild{}, postMutationSteamError(lastErr)
}

func (s *SteamClient) validateCommandConfiguration(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Runner == nil {
		return fmt.Errorf("Steam command runner is nil")
	}
	if s.SteamCMD == "" {
		return fmt.Errorf("SteamCMD path is empty")
	}
	if s.ServerDir == "" {
		return fmt.Errorf("server directory is empty")
	}
	if s.HomeDir == "" {
		return fmt.Errorf("Steam home directory is empty")
	}
	return nil
}

func (s *SteamClient) remoteBuildCommand() CommandSpec {
	return CommandSpec{
		Path: s.SteamCMD,
		Args: []string{
			"+@sSteamCmdForcePlatformType", "windows",
			"+login", "anonymous",
			"+app_info_update", "1",
			"+app_info_print", steamAppID,
			"+quit",
		},
		Env: []string{"HOME=" + s.HomeDir},
		Dir: s.HomeDir,
	}
}

func (s *SteamClient) updateCommand(branch string) CommandSpec {
	args := []string{
		"+@sSteamCmdForcePlatformType", "windows",
		"+force_install_dir", s.ServerDir,
		"+login", "anonymous",
		"+app_update", steamAppID,
	}
	if branch != publicBranch {
		args = append(args, "-beta", branch)
	}
	args = append(args, "validate", "+quit")
	return CommandSpec{
		Path: s.SteamCMD,
		Args: args,
		Env:  []string{"HOME=" + s.HomeDir},
		Dir:  s.HomeDir,
	}
}

func (s *SteamClient) selectedBranch() string {
	return selectedSteamBranch(s.Branch)
}

func selectedSteamBranch(branch string) string {
	if branch == "" {
		return publicBranch
	}
	return branch
}

func (s *SteamClient) readInstalledBuild() (SteamBuild, error) {
	manifestPath := filepath.Join(s.ServerDir, "steamapps", "appmanifest_"+steamAppID+".acf")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return SteamBuild{}, err
	}
	root, err := parseVDFRoot(manifest, "AppState")
	if err != nil {
		return SteamBuild{}, err
	}
	appID, ok := root.value("appid")
	if !ok || appID != steamAppID {
		return SteamBuild{}, fmt.Errorf("installed manifest appid is %q, want %s", appID, steamAppID)
	}
	buildID, ok := root.value("buildid")
	if !ok || buildID == "" {
		return SteamBuild{}, fmt.Errorf("installed manifest buildid is missing")
	}
	if targetBuildID, ok := root.value("TargetBuildID"); ok && targetBuildID != "" && targetBuildID != buildID {
		return SteamBuild{}, fmt.Errorf("installed buildid %s does not match manifest target buildid %s", buildID, targetBuildID)
	}
	depotManifest, err := root.pathValue("InstalledDepots", steamDepotID, "manifest")
	if err != nil || depotManifest == "" {
		return SteamBuild{}, fmt.Errorf("installed depot %s manifest is missing", steamDepotID)
	}
	branch := publicBranch
	if userConfig, ok := root.child("UserConfig"); ok {
		if betaKey, ok := userConfig.value("BetaKey"); ok && betaKey != "" {
			branch = betaKey
		}
	}
	return SteamBuild{BuildID: buildID, DepotManifest: depotManifest, Branch: branch}, nil
}

func parseRemoteBuild(output []byte, branch string) (SteamBuild, error) {
	root, err := parseVDFRoot(output, steamAppID)
	if err != nil {
		return SteamBuild{}, err
	}
	branch = selectedSteamBranch(branch)
	buildID, err := root.pathValue("depots", "branches", branch, "buildid")
	if err != nil || buildID == "" {
		return SteamBuild{}, fmt.Errorf("remote branch %q buildid is missing", branch)
	}
	depotManifest, err := root.pathValue("depots", steamDepotID, "manifests", branch, "gid")
	if err != nil || depotManifest == "" {
		return SteamBuild{}, fmt.Errorf("remote branch %q depot %s manifest is missing", branch, steamDepotID)
	}
	return SteamBuild{BuildID: buildID, DepotManifest: depotManifest, Branch: branch}, nil
}

func (s *SteamClient) validateUpdatedInstallation(target SteamBuild) (SteamBuild, error) {
	if err := requireRegularFile(filepath.Join(s.ServerDir, "VRisingServer.exe")); err != nil {
		return SteamBuild{}, fmt.Errorf("validate V Rising executable: %w", err)
	}
	appID, err := os.ReadFile(filepath.Join(s.ServerDir, "steam_appid.txt"))
	if err != nil {
		return SteamBuild{}, fmt.Errorf("read steam_appid.txt: %w", err)
	}
	if strings.TrimSpace(string(appID)) != steamAppID {
		return SteamBuild{}, fmt.Errorf("steam_appid.txt does not contain %s", steamAppID)
	}
	installed, err := s.readInstalledBuild()
	if err != nil {
		return SteamBuild{}, fmt.Errorf("read updated appmanifest: %w", err)
	}
	if installed.Branch != target.Branch {
		return SteamBuild{}, fmt.Errorf("installed branch %q does not match target branch %q", installed.Branch, target.Branch)
	}
	if installed.BuildID != target.BuildID {
		return SteamBuild{}, fmt.Errorf("installed build %s does not match target build %s", installed.BuildID, target.BuildID)
	}
	if installed.DepotManifest != target.DepotManifest {
		return SteamBuild{}, fmt.Errorf(
			"installed depot manifest %s does not match target manifest %s",
			installed.DepotManifest,
			target.DepotManifest,
		)
	}
	return installed, nil
}

func requireRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

func steamCommandError(result CommandResult, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if result.ExitCode == 0 {
		return nil
	}
	detail := strings.TrimSpace(string(result.Stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(result.Stdout))
	}
	if detail == "" {
		return fmt.Errorf("SteamCMD exited with status %d", result.ExitCode)
	}
	return fmt.Errorf("SteamCMD exited with status %d: %s", result.ExitCode, detail)
}

func preMutationSteamError(err error) error {
	return &SteamPreMutationError{Err: err}
}

func postMutationSteamError(err error) error {
	if err == nil {
		err = fmt.Errorf("app_update failed")
	}
	return &SteamPostMutationError{Err: err}
}

type vdfNode struct {
	values   map[string]string
	children map[string]*vdfNode
}

func (n *vdfNode) value(key string) (string, bool) {
	for candidate, value := range n.values {
		if strings.EqualFold(candidate, key) {
			return value, true
		}
	}
	return "", false
}

func (n *vdfNode) child(key string) (*vdfNode, bool) {
	for candidate, child := range n.children {
		if strings.EqualFold(candidate, key) {
			return child, true
		}
	}
	return nil, false
}

func (n *vdfNode) pathValue(path ...string) (string, error) {
	if len(path) == 0 {
		return "", fmt.Errorf("empty VDF path")
	}
	current := n
	for _, segment := range path[:len(path)-1] {
		child, ok := current.child(segment)
		if !ok {
			return "", fmt.Errorf("VDF object %q is missing", segment)
		}
		current = child
	}
	value, ok := current.value(path[len(path)-1])
	if !ok {
		return "", fmt.Errorf("VDF value %q is missing", path[len(path)-1])
	}
	return value, nil
}

type vdfTokenKind uint8

const (
	vdfString vdfTokenKind = iota
	vdfOpen
	vdfClose
)

type vdfToken struct {
	kind  vdfTokenKind
	value string
}

func parseVDFRoot(data []byte, rootName string) (*vdfNode, error) {
	tokens, err := tokenizeVDF(data)
	if err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].kind != vdfString || tokens[i].value != rootName || tokens[i+1].kind != vdfOpen {
			continue
		}
		position := i + 1
		return parseVDFObject(tokens, &position)
	}
	return nil, fmt.Errorf("VDF root %q is missing", rootName)
}

func tokenizeVDF(data []byte) ([]vdfToken, error) {
	tokens := make([]vdfToken, 0)
	for i := 0; i < len(data); {
		switch data[i] {
		case '{':
			tokens = append(tokens, vdfToken{kind: vdfOpen})
			i++
		case '}':
			tokens = append(tokens, vdfToken{kind: vdfClose})
			i++
		case '"':
			value, next, err := readVDFString(data, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, vdfToken{kind: vdfString, value: value})
			i = next
		default:
			i++
		}
	}
	return tokens, nil
}

func readVDFString(data []byte, start int) (string, int, error) {
	var value strings.Builder
	for i := start + 1; i < len(data); i++ {
		if data[i] == '"' {
			return value.String(), i + 1, nil
		}
		if data[i] != '\\' {
			value.WriteByte(data[i])
			continue
		}
		if i+1 >= len(data) {
			return "", 0, fmt.Errorf("unterminated VDF escape")
		}
		i++
		switch data[i] {
		case '"', '\\':
			value.WriteByte(data[i])
		case 'n':
			value.WriteByte('\n')
		case 't':
			value.WriteByte('\t')
		default:
			value.WriteByte('\\')
			value.WriteByte(data[i])
		}
	}
	return "", 0, fmt.Errorf("unterminated VDF string")
}

func parseVDFObject(tokens []vdfToken, position *int) (*vdfNode, error) {
	if *position >= len(tokens) || tokens[*position].kind != vdfOpen {
		return nil, fmt.Errorf("VDF object opening brace is missing")
	}
	*position++
	node := &vdfNode{values: make(map[string]string), children: make(map[string]*vdfNode)}
	for *position < len(tokens) {
		if tokens[*position].kind == vdfClose {
			*position++
			return node, nil
		}
		if tokens[*position].kind != vdfString {
			return nil, fmt.Errorf("VDF object key is malformed")
		}
		key := tokens[*position].value
		*position++
		if *position >= len(tokens) {
			return nil, fmt.Errorf("VDF value for %q is missing", key)
		}
		switch tokens[*position].kind {
		case vdfString:
			node.values[key] = tokens[*position].value
			*position++
		case vdfOpen:
			child, err := parseVDFObject(tokens, position)
			if err != nil {
				return nil, err
			}
			node.children[key] = child
		case vdfClose:
			return nil, fmt.Errorf("VDF value for %q is missing", key)
		}
	}
	return nil, fmt.Errorf("VDF object closing brace is missing")
}
