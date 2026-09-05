package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const defaultThunderstoreBaseURL = "https://thunderstore.io"

var dependencyReference = regexp.MustCompile(`^([A-Za-z0-9_]+)-([A-Za-z0-9_-]+)-(\d+\.\d+\.\d+)$`)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type RootSelection struct {
	Namespace string
	Name      string
	Version   string
}

type ResolvedPackage struct {
	Ref          PackageRef
	FullName     string
	DownloadURL  string
	FileSize     int64
	IsActive     bool
	Dependencies []PackageRef
}

type ResolvedGraph struct {
	Root     PackageRef
	Packages []ResolvedPackage
}

type Thunderstore struct {
	BaseURL string
	Client  HTTPDoer
}

type packageMetadata struct {
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	Version      string   `json:"version_number"`
	FullName     string   `json:"full_name"`
	Dependencies []string `json:"dependencies"`
	DownloadURL  string   `json:"download_url"`
	FileSize     int64    `json:"file_size"`
	IsActive     bool     `json:"is_active"`
}

type packageResponse struct {
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	FullName  string          `json:"full_name"`
	Latest    packageMetadata `json:"latest"`
}

func (t *Thunderstore) Resolve(ctx context.Context, selection RootSelection) (ResolvedGraph, error) {
	if err := validateRootSelection(selection); err != nil {
		return ResolvedGraph{}, err
	}

	root, err := t.resolveRoot(ctx, selection)
	if err != nil {
		return ResolvedGraph{}, err
	}

	resolver := graphResolver{
		thunderstore: t,
		visiting:     make(map[PackageRef]bool),
		visited:      make(map[PackageRef]bool),
		versions:     make(map[string]string),
	}
	if err := resolver.visit(ctx, root); err != nil {
		return ResolvedGraph{}, err
	}
	return ResolvedGraph{Root: root, Packages: resolver.packages}, nil
}

func validateRootSelection(selection RootSelection) error {
	if selection.Namespace == "" || selection.Name == "" {
		return fmt.Errorf("root namespace and name are required")
	}
	if selection.Version != "latest" && !semanticVersion.MatchString(selection.Version) {
		return fmt.Errorf("root version must be latest or a semantic version")
	}
	return nil
}

func (t *Thunderstore) resolveRoot(ctx context.Context, selection RootSelection) (PackageRef, error) {
	if selection.Version != "latest" {
		return PackageRef{Namespace: selection.Namespace, Name: selection.Name, Version: selection.Version}, nil
	}

	var response packageResponse
	if err := t.getJSON(ctx, t.packageURL(selection.Namespace, selection.Name), &response); err != nil {
		return PackageRef{}, fmt.Errorf("load latest root metadata: %w", err)
	}
	if response.Namespace != selection.Namespace || response.Name != selection.Name ||
		response.FullName != packageFullName(selection.Namespace, selection.Name) {
		return PackageRef{}, fmt.Errorf("latest root metadata identity does not match %s/%s", selection.Namespace, selection.Name)
	}
	if response.Latest.Namespace != selection.Namespace || response.Latest.Name != selection.Name ||
		!semanticVersion.MatchString(response.Latest.Version) ||
		response.Latest.FullName != packageVersionFullName(PackageRef{Namespace: selection.Namespace, Name: selection.Name, Version: response.Latest.Version}) {
		return PackageRef{}, fmt.Errorf("latest root version identity does not match %s/%s", selection.Namespace, selection.Name)
	}
	return PackageRef{Namespace: selection.Namespace, Name: selection.Name, Version: response.Latest.Version}, nil
}

type graphResolver struct {
	thunderstore *Thunderstore
	visiting     map[PackageRef]bool
	visited      map[PackageRef]bool
	versions     map[string]string
	packages     []ResolvedPackage
}

func (r *graphResolver) visit(ctx context.Context, ref PackageRef) error {
	key := ref.Namespace + "\x00" + ref.Name
	if version, ok := r.versions[key]; ok && version != ref.Version {
		return fmt.Errorf("conflicting versions for %s/%s: %s and %s", ref.Namespace, ref.Name, version, ref.Version)
	}
	r.versions[key] = ref.Version
	if r.visiting[ref] {
		return fmt.Errorf("dependency cycle at %s", packageVersionFullName(ref))
	}
	if r.visited[ref] {
		return nil
	}

	r.visiting[ref] = true
	defer delete(r.visiting, ref)

	var metadata packageMetadata
	if err := r.thunderstore.getJSON(ctx, r.thunderstore.versionURL(ref), &metadata); err != nil {
		return fmt.Errorf("load metadata for %s: %w", packageVersionFullName(ref), err)
	}
	if err := validateMetadata(ref, metadata); err != nil {
		return err
	}

	dependencies := make([]PackageRef, 0, len(metadata.Dependencies))
	for _, dependency := range metadata.Dependencies {
		parsed, err := parseDependency(dependency)
		if err != nil {
			return fmt.Errorf("parse dependency for %s: %w", packageVersionFullName(ref), err)
		}
		dependencies = append(dependencies, parsed)
	}
	sortPackageRefs(dependencies)
	for _, dependency := range dependencies {
		if err := r.visit(ctx, dependency); err != nil {
			return err
		}
	}

	r.visited[ref] = true
	r.packages = append(r.packages, ResolvedPackage{
		Ref:          ref,
		FullName:     metadata.FullName,
		DownloadURL:  metadata.DownloadURL,
		FileSize:     metadata.FileSize,
		IsActive:     metadata.IsActive,
		Dependencies: dependencies,
	})
	return nil
}

func parseDependency(dependency string) (PackageRef, error) {
	matches := dependencyReference.FindStringSubmatch(dependency)
	if matches == nil {
		return PackageRef{}, fmt.Errorf("malformed dependency reference %q", dependency)
	}
	return PackageRef{Namespace: matches[1], Name: matches[2], Version: matches[3]}, nil
}

func validateMetadata(ref PackageRef, metadata packageMetadata) error {
	if metadata.Namespace != ref.Namespace || metadata.Name != ref.Name || metadata.Version != ref.Version ||
		metadata.FullName != packageVersionFullName(ref) {
		return fmt.Errorf("metadata identity does not match %s", packageVersionFullName(ref))
	}
	if !metadata.IsActive {
		return fmt.Errorf("package %s is inactive", packageVersionFullName(ref))
	}
	if metadata.FileSize <= 0 {
		return fmt.Errorf("package %s has non-positive file size", packageVersionFullName(ref))
	}
	downloadURL, err := url.Parse(metadata.DownloadURL)
	if err != nil || downloadURL.Scheme != "https" || downloadURL.Host == "" {
		return fmt.Errorf("package %s has non-HTTPS download URL", packageVersionFullName(ref))
	}
	return nil
}

func (t *Thunderstore) getJSON(ctx context.Context, endpoint string, target any) error {
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	for attempt := range 3 {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return fmt.Errorf("create metadata request: %w", err)
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("request metadata: %w", err)
		}

		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
			response.Body.Close()
			if attempt < 2 {
				if err := waitForRetry(ctx, attempt); err != nil {
					return err
				}
				continue
			}
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("metadata response status %s", response.Status)
		}
		if err := decodeJSONResponse(response, target); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("metadata retry attempts exhausted")
}

func waitForRetry(ctx context.Context, attempt int) error {
	delay := time.Second
	if attempt == 1 {
		delay = 2 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeJSONResponse(response *http.Response, target any) error {
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf("metadata response is not JSON")
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode metadata JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("metadata response contains multiple JSON values")
		}
		return fmt.Errorf("read metadata JSON: %w", err)
	}
	return nil
}

func (t *Thunderstore) packageURL(namespace, name string) string {
	return strings.TrimRight(t.baseURL(), "/") + "/api/experimental/package/" + url.PathEscape(namespace) + "/" + url.PathEscape(name) + "/"
}

func (t *Thunderstore) versionURL(ref PackageRef) string {
	return t.packageURL(ref.Namespace, ref.Name) + url.PathEscape(ref.Version) + "/"
}

func (t *Thunderstore) baseURL() string {
	if t.BaseURL == "" {
		return defaultThunderstoreBaseURL
	}
	return t.BaseURL
}

func packageFullName(namespace, name string) string {
	return namespace + "-" + name
}

func packageVersionFullName(ref PackageRef) string {
	return packageFullName(ref.Namespace, ref.Name) + "-" + ref.Version
}

func sortPackageRefs(refs []PackageRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Namespace != refs[j].Namespace {
			return refs[i].Namespace < refs[j].Namespace
		}
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].Version < refs[j].Version
	})
}

func PackageLockDigest(lock PackageLock) string {
	packages := make([]canonicalLockedPackage, len(lock.Packages))
	for i, pkg := range lock.Packages {
		dependencies := append([]PackageRef(nil), pkg.Dependencies...)
		sortPackageRefs(dependencies)
		packages[i] = canonicalLockedPackage{
			Ref:          pkg.Ref,
			FullName:     pkg.FullName,
			DownloadURL:  pkg.DownloadURL,
			FileSize:     pkg.FileSize,
			SHA256:       pkg.SHA256,
			Dependencies: dependencies,
		}
	}
	sort.Slice(packages, func(i, j int) bool {
		return comparePackageRefs(packages[i].Ref, packages[j].Ref) < 0
	})
	canonical, err := json.Marshal(canonicalPackageLock{
		SchemaVersion: lock.SchemaVersion,
		Root:          lock.Root,
		Packages:      packages,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal canonical package lock: %v", err))
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

type canonicalPackageLock struct {
	SchemaVersion int                      `json:"schema_version"`
	Root          PackageRef               `json:"root"`
	Packages      []canonicalLockedPackage `json:"packages"`
}

type canonicalLockedPackage struct {
	Ref          PackageRef   `json:"ref"`
	FullName     string       `json:"full_name"`
	DownloadURL  string       `json:"download_url"`
	FileSize     int64        `json:"file_size"`
	SHA256       string       `json:"sha256"`
	Dependencies []PackageRef `json:"dependencies"`
}

func comparePackageRefs(left, right PackageRef) int {
	if left.Namespace != right.Namespace {
		return strings.Compare(left.Namespace, right.Namespace)
	}
	if left.Name != right.Name {
		return strings.Compare(left.Name, right.Name)
	}
	return strings.Compare(left.Version, right.Version)
}
