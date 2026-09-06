package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	steamAppID        = "1829350"
	steamDepotID      = "1829351"
	steamRuntimeAppID = "1604030"
	publicBranch      = "public"
	updateTries       = 3
)

type CommandSpec struct {
	Path         string
	Args         []string
	Env          []string
	Dir          string
	StreamOutput bool
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

func (s *SteamClient) ValidateInstalled(expected SteamBuild) (SteamBuild, error) {
	expected.Branch = selectedSteamBranch(expected.Branch)
	installed, err := s.validateUpdatedInstallation(expected)
	if err != nil {
		return SteamBuild{}, preMutationSteamError(fmt.Errorf("validate installed runtime: %w", err))
	}
	return installed, nil
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
	if s.Branch != publicBranch && strings.EqualFold(s.Branch, publicBranch) {
		return SteamBuild{}, preMutationSteamError(fmt.Errorf("configured public branch must be spelled %q", publicBranch))
	}
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
		Env: steamCommandEnvironment(s.HomeDir),
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
		Path:         s.SteamCMD,
		Args:         args,
		Env:          steamCommandEnvironment(s.HomeDir),
		Dir:          s.HomeDir,
		StreamOutput: true,
	}
}

func steamCommandEnvironment(home string) []string {
	return []string{
		"HOME=" + home,
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
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
	root, err := parseVDFDocument(manifest, "AppState")
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
	root, err := parseRemoteVDF(output, steamAppID)
	if err != nil {
		return SteamBuild{}, err
	}
	branch = selectedSteamBranch(branch)
	depots, ok := root.child("depots")
	if !ok {
		return SteamBuild{}, fmt.Errorf("remote depots metadata is missing")
	}
	branches, ok := depots.child("branches")
	if !ok {
		return SteamBuild{}, fmt.Errorf("remote branches metadata is missing")
	}
	selected, ok := branches.childExact(branch)
	if !ok {
		return SteamBuild{}, fmt.Errorf("remote branch %q is missing", branch)
	}
	buildID, ok := selected.value("buildid")
	if !ok || buildID == "" {
		return SteamBuild{}, fmt.Errorf("remote branch %q buildid is missing", branch)
	}
	depot, ok := depots.child(steamDepotID)
	if !ok {
		return SteamBuild{}, fmt.Errorf("remote depot %s metadata is missing", steamDepotID)
	}
	manifests, ok := depot.child("manifests")
	if !ok {
		return SteamBuild{}, fmt.Errorf("remote depot %s manifests are missing", steamDepotID)
	}
	manifest, ok := manifests.childExact(branch)
	if !ok {
		return SteamBuild{}, fmt.Errorf("remote branch %q depot %s manifest is missing", branch, steamDepotID)
	}
	depotManifest, ok := manifest.value("gid")
	if !ok || depotManifest == "" {
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
	if !validRuntimeSteamAppID(appID) {
		return SteamBuild{}, fmt.Errorf("steam_appid.txt does not contain the exact runtime app ID %s", steamRuntimeAppID)
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

func validRuntimeSteamAppID(content []byte) bool {
	switch string(content) {
	case steamRuntimeAppID, steamRuntimeAppID + "\n", steamRuntimeAppID + "\r\n":
		return true
	default:
		return false
	}
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
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
	return &SteamPreMutationError{Err: phaseNeutralSteamError(err)}
}

func postMutationSteamError(err error) error {
	if err == nil {
		err = fmt.Errorf("app_update failed")
	}
	return &SteamPostMutationError{Err: phaseNeutralSteamError(err)}
}

type opaqueSteamError struct {
	message string
	cause   error
}

func (e opaqueSteamError) Error() string {
	return e.message
}

func (e opaqueSteamError) Is(target error) bool {
	switch target.(type) {
	case *SteamPreMutationError, *SteamPostMutationError:
		return false
	default:
		return errors.Is(e.cause, target)
	}
}

func phaseNeutralSteamError(err error) error {
	if err == nil {
		return opaqueSteamError{message: "unknown Steam failure"}
	}
	var preMutation *SteamPreMutationError
	var postMutation *SteamPostMutationError
	if errors.As(err, &preMutation) || errors.As(err, &postMutation) {
		return opaqueSteamError{message: err.Error(), cause: err}
	}
	return err
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

func (n *vdfNode) childExact(key string) (*vdfNode, bool) {
	child, ok := n.children[key]
	return child, ok
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

type vdfParser struct {
	data     []byte
	position int
}

func parseVDFDocument(data []byte, rootName string) (*vdfNode, error) {
	root, consumed, err := parseVDFPrefix(data, rootName)
	if err != nil {
		return nil, err
	}
	if consumed != len(data) {
		return nil, fmt.Errorf("unexpected data after VDF root %q", rootName)
	}
	return root, nil
}

func parseVDFPrefix(data []byte, rootName string) (*vdfNode, int, error) {
	parser := &vdfParser{data: data}
	parser.skipWhitespace()
	root, err := parser.readString()
	if err != nil {
		return nil, 0, fmt.Errorf("read VDF root: %w", err)
	}
	if root != rootName {
		return nil, 0, fmt.Errorf("VDF root is %q, want %q", root, rootName)
	}
	parser.skipWhitespace()
	node, err := parser.readObject()
	if err != nil {
		return nil, 0, err
	}
	parser.skipWhitespace()
	return node, parser.position, nil
}

func parseRemoteVDF(output []byte, rootName string) (*vdfNode, error) {
	rootStart, count := remoteVDFRootLine(output, rootName)
	if count != 1 {
		return nil, fmt.Errorf("SteamCMD output contains %d VDF roots for app %s", count, rootName)
	}
	if err := validateSteamWrapper(output[:rootStart], true); err != nil {
		return nil, err
	}
	root, consumed, err := parseVDFPrefix(output[rootStart:], rootName)
	if err != nil {
		return nil, err
	}
	if err := validateSteamWrapper(output[rootStart+consumed:], false); err != nil {
		return nil, err
	}
	return root, nil
}

func remoteVDFRootLine(output []byte, rootName string) (int, int) {
	want := `"` + rootName + `"`
	rootStart := -1
	count := 0
	for lineStart := 0; lineStart <= len(output); {
		lineEnd := len(output)
		if newline := bytes.IndexByte(output[lineStart:], '\n'); newline >= 0 {
			lineEnd = lineStart + newline
		}
		line := strings.TrimSuffix(string(output[lineStart:lineEnd]), "\r")
		if strings.Trim(line, " \t") == want {
			rootStart = lineStart
			count++
		}
		if lineEnd == len(output) {
			break
		}
		lineStart = lineEnd + 1
	}
	return rootStart, count
}

func validateSteamWrapper(data []byte, beforeRoot bool) error {
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		line = strings.ReplaceAll(line, "\x1b[0m", "")
		if strings.ContainsRune(line, '\x1b') {
			return fmt.Errorf("unrecognized ANSI sequence in SteamCMD console output")
		}
		if strings.Trim(line, " \t") == "" {
			continue
		}
		if beforeRoot && isSteamPrefixLine(line) || !beforeRoot && isSteamSuffixLine(line) {
			continue
		}
		return fmt.Errorf("unrecognized SteamCMD console output %q", line)
	}
	return nil
}

func isSteamPrefixLine(line string) bool {
	switch line {
	case "[  0%] Checking for available updates...",
		"[----] Verifying installation...",
		"UpdateUI: skip show logo",
		"-- type 'quit' to exit --",
		"Loading Steam API...OK",
		`"@sSteamCmdForcePlatformType" = "windows"`,
		"Connecting anonymously to Steam Public...OK",
		"Waiting for client config...OK",
		"Waiting for user info...OK":
		return true
	}
	if quotedConsolePath(line, "Redirecting stderr to ") || quotedConsolePath(line, "Logging directory: ") {
		return true
	}
	if version, ok := strings.CutPrefix(line, "Steam Console Client (c) Valve Corporation - version "); ok {
		return asciiDigits(version)
	}
	const appInfoPrefix = "AppID : " + steamAppID + ", change number : "
	changeAndTime, ok := strings.CutPrefix(line, appInfoPrefix)
	if !ok {
		return false
	}
	change, changedAt, ok := strings.Cut(changeAndTime, ", last change : ")
	left, right, slash := strings.Cut(change, "/")
	return ok && slash && asciiDigits(left) && asciiDigits(right) && safeConsoleText(changedAt)
}

func isSteamSuffixLine(line string) bool {
	return line == "Steam>" || line == "Unloading Steam API...OK"
}

func quotedConsolePath(line, prefix string) bool {
	value, ok := strings.CutPrefix(line, prefix)
	if !ok || len(value) < 3 || value[0] != '\'' || value[len(value)-1] != '\'' {
		return false
	}
	return safeConsoleText(value[1:len(value)-1]) && !strings.ContainsRune(value[1:len(value)-1], '\'')
}

func asciiDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func safeConsoleText(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || strings.ContainsRune(`"{}`, character) {
			return false
		}
	}
	return true
}

func (p *vdfParser) skipWhitespace() {
	for p.position < len(p.data) {
		switch p.data[p.position] {
		case ' ', '\t', '\r', '\n':
			p.position++
		default:
			return
		}
	}
}

func (p *vdfParser) readString() (string, error) {
	if p.position >= len(p.data) || p.data[p.position] != '"' {
		return "", fmt.Errorf("expected quoted string at byte %d", p.position)
	}
	p.position++
	var value strings.Builder
	for p.position < len(p.data) {
		character := p.data[p.position]
		p.position++
		if character == '"' {
			return value.String(), nil
		}
		if character < 0x20 {
			return "", fmt.Errorf("unescaped control byte in VDF string")
		}
		if character != '\\' {
			value.WriteByte(character)
			continue
		}
		if p.position >= len(p.data) {
			return "", fmt.Errorf("unterminated VDF escape")
		}
		escaped := p.data[p.position]
		p.position++
		if escaped < 0x20 {
			return "", fmt.Errorf("raw control byte after VDF escape")
		}
		switch escaped {
		case '"', '\\':
			value.WriteByte(escaped)
		case 'n':
			value.WriteByte('\n')
		case 't':
			value.WriteByte('\t')
		default:
			value.WriteByte('\\')
			value.WriteByte(escaped)
		}
	}
	return "", fmt.Errorf("unterminated VDF string")
}

func (p *vdfParser) readObject() (*vdfNode, error) {
	if p.position >= len(p.data) || p.data[p.position] != '{' {
		return nil, fmt.Errorf("expected VDF object at byte %d", p.position)
	}
	p.position++
	node := &vdfNode{values: make(map[string]string), children: make(map[string]*vdfNode)}
	for {
		p.skipWhitespace()
		if p.position >= len(p.data) {
			return nil, fmt.Errorf("VDF object closing brace is missing")
		}
		if p.data[p.position] == '}' {
			p.position++
			return node, nil
		}
		key, err := p.readString()
		if err != nil {
			return nil, fmt.Errorf("read VDF object key: %w", err)
		}
		if node.hasKey(key) {
			return nil, fmt.Errorf("duplicate VDF key %q", key)
		}
		p.skipWhitespace()
		if p.position >= len(p.data) {
			return nil, fmt.Errorf("VDF value for %q is missing", key)
		}
		if p.data[p.position] == '"' {
			value, err := p.readString()
			if err != nil {
				return nil, fmt.Errorf("read VDF value for %q: %w", key, err)
			}
			node.values[key] = value
			continue
		}
		if p.data[p.position] == '{' {
			child, err := p.readObject()
			if err != nil {
				return nil, err
			}
			node.children[key] = child
			continue
		}
		return nil, fmt.Errorf("VDF value for %q is malformed", key)
	}
}

func (n *vdfNode) hasKey(key string) bool {
	for candidate := range n.values {
		if strings.EqualFold(candidate, key) {
			return true
		}
	}
	for candidate := range n.children {
		if strings.EqualFold(candidate, key) {
			return true
		}
	}
	return false
}
