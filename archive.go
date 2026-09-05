package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	archiveSafetyReserve = uint64(512 << 20)
	maxUnknownArchive    = int64(1 << 30)
)

type ArchiveCache struct {
	Dir      string
	Client   HTTPDoer
	Attempts int
	Timeout  time.Duration
	Backoff  func(context.Context, time.Duration) error
}

type ExtractPolicy int

const (
	ExtractBepInEx ExtractPolicy = iota
	ExtractPlugin
)

type ExtractedArchive struct {
	Manifest PackageRef
	Files    []string
}

type archiveManifest struct {
	Name         string              `json:"name"`
	Namespace    string              `json:"namespace"`
	FullName     string              `json:"FullName"`
	Version      string              `json:"version_number"`
	Dependencies packageDependencies `json:"dependencies"`
}

type archiveEntry struct {
	file        *zip.File
	archivePath string
	isDirectory bool
	outputPath  string
}

func (c *ArchiveCache) Fetch(ctx context.Context, pkg ResolvedPackage, prior LockedPackage) (LockedPackage, string, error) {
	if err := validateResolvedArchivePackage(pkg); err != nil {
		return LockedPackage{}, "", err
	}
	if err := validatePriorLockedPackage(pkg, prior); err != nil {
		return LockedPackage{}, "", err
	}

	cacheRoot, err := openDirectoryPath(c.Dir, true)
	if err != nil {
		return LockedPackage{}, "", fmt.Errorf("open archive cache: %w", err)
	}
	defer unix.Close(cacheRoot)

	archiveName := pkg.FullName + ".zip"
	if _, err := relativePathParts(archiveName); err != nil {
		return LockedPackage{}, "", fmt.Errorf("invalid cache archive name: %w", err)
	}
	archivePath := filepath.Join(c.Dir, archiveName)

	locked, found, err := cachedArchive(cacheRoot, archiveName, pkg, prior)
	if err != nil {
		return LockedPackage{}, "", err
	}
	if found {
		return locked, archivePath, nil
	}

	requiredSize := uint64(maxUnknownArchive)
	if pkg.FileSize > 0 {
		requiredSize = uint64(pkg.FileSize)
	}
	if err := requireFilesystemSpace(cacheRoot, requiredSize, "archive cache"); err != nil {
		return LockedPackage{}, "", err
	}

	temporaryName, temporaryFD, err := createTemporaryFileAt(cacheRoot, archiveName, 0o600)
	if err != nil {
		return LockedPackage{}, "", fmt.Errorf("create archive cache temporary file: %w", err)
	}
	temporary := os.NewFile(uintptr(temporaryFD), temporaryName)
	removeTemporary := true
	defer func() {
		temporary.Close()
		if removeTemporary {
			_ = unix.Unlinkat(cacheRoot, temporaryName, 0)
		}
	}()

	actualSize, digest, err := c.download(ctx, temporary, pkg)
	if err != nil {
		return LockedPackage{}, "", err
	}
	if err := validateLockedBytes(prior, actualSize, digest); err != nil {
		return LockedPackage{}, "", err
	}
	if err := temporary.Sync(); err != nil {
		return LockedPackage{}, "", fmt.Errorf("sync downloaded archive: %w", err)
	}
	validationFD, err := unix.Openat(cacheRoot, temporaryName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return LockedPackage{}, "", fmt.Errorf("open downloaded archive for validation: %w", err)
	}
	validationFile := os.NewFile(uintptr(validationFD), temporaryName)
	validationErr := validatePackageZIP(validationFile, actualSize, pkg.Ref, pkg.Dependencies)
	closeValidationErr := validationFile.Close()
	if validationErr != nil {
		return LockedPackage{}, "", fmt.Errorf("validate downloaded archive: %w", validationErr)
	}
	if closeValidationErr != nil {
		return LockedPackage{}, "", fmt.Errorf("close downloaded archive validation file: %w", closeValidationErr)
	}
	if err := temporary.Close(); err != nil {
		return LockedPackage{}, "", fmt.Errorf("close downloaded archive: %w", err)
	}
	if err := unix.Renameat(cacheRoot, temporaryName, cacheRoot, archiveName); err != nil {
		return LockedPackage{}, "", fmt.Errorf("rename downloaded archive: %w", err)
	}
	removeTemporary = false
	if err := unix.Fsync(cacheRoot); err != nil {
		return LockedPackage{}, "", fmt.Errorf("sync archive cache: %w", err)
	}

	return lockedPackageForArchive(pkg, actualSize, digest), archivePath, nil
}

func validateResolvedArchivePackage(pkg ResolvedPackage) error {
	if pkg.Ref.Namespace == "" || pkg.Ref.Name == "" || !semanticVersion.MatchString(pkg.Ref.Version) {
		return fmt.Errorf("archive package reference is invalid")
	}
	if pkg.FullName != packageVersionFullName(pkg.Ref) {
		return fmt.Errorf("archive package full name does not match reference")
	}
	if pkg.DownloadURL == "" {
		return fmt.Errorf("archive package download URL is empty")
	}
	if pkg.FileSize < 0 {
		return fmt.Errorf("archive package size is negative")
	}
	return nil
}

func validatePriorLockedPackage(pkg ResolvedPackage, prior LockedPackage) error {
	if !hasLockedPackage(prior) {
		return nil
	}
	if prior.Ref != pkg.Ref {
		return fmt.Errorf("prior locked package reference does not match %s", pkg.FullName)
	}
	if prior.FileSize <= 0 {
		return fmt.Errorf("prior locked size must be positive")
	}
	digest, err := hex.DecodeString(prior.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("prior locked SHA-256 is invalid")
	}
	if pkg.FileSize > 0 && prior.FileSize != pkg.FileSize {
		return fmt.Errorf("locked size %d does not match metadata size %d", prior.FileSize, pkg.FileSize)
	}
	return nil
}

func hasLockedPackage(pkg LockedPackage) bool {
	return pkg.Ref != (PackageRef{}) || pkg.FullName != "" || pkg.DownloadURL != "" || pkg.FileSize != 0 || pkg.SHA256 != "" || len(pkg.Dependencies) != 0
}

func cachedArchive(root int, name string, pkg ResolvedPackage, prior LockedPackage) (LockedPackage, bool, error) {
	fd, err := unix.Openat(root, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == unix.ENOENT {
		return LockedPackage{}, false, nil
	}
	if err != nil {
		return LockedPackage{}, false, fmt.Errorf("open cached archive: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return LockedPackage{}, false, fmt.Errorf("stat cached archive: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return LockedPackage{}, false, fmt.Errorf("cached archive is not a regular file")
	}
	if stat.Size <= 0 {
		return LockedPackage{}, false, fmt.Errorf("cached archive size must be positive")
	}
	if pkg.FileSize > 0 && stat.Size != pkg.FileSize {
		return LockedPackage{}, false, fmt.Errorf("cached archive size %d does not match metadata size %d", stat.Size, pkg.FileSize)
	}
	digest, err := hashFile(file)
	if err != nil {
		return LockedPackage{}, false, fmt.Errorf("hash cached archive: %w", err)
	}
	if err := validateLockedBytes(prior, stat.Size, digest); err != nil {
		return LockedPackage{}, false, err
	}
	if err := validatePackageZIP(file, stat.Size, pkg.Ref, pkg.Dependencies); err != nil {
		return LockedPackage{}, false, fmt.Errorf("validate cached archive: %w", err)
	}
	return lockedPackageForArchive(pkg, stat.Size, digest), true, nil
}

func hashFile(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (c *ArchiveCache) download(ctx context.Context, target *os.File, pkg ResolvedPackage) (int64, string, error) {
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := c.Client
	if client == nil {
		client = &http.Client{}
	}

	var lastErr error
	for attempt := range attempts {
		if err := resetTemporaryFile(target); err != nil {
			return 0, "", err
		}
		requestContext, cancel := context.WithTimeout(ctx, timeout)
		request, err := http.NewRequestWithContext(requestContext, http.MethodGet, pkg.DownloadURL, nil)
		if err != nil {
			cancel()
			return 0, "", fmt.Errorf("create archive request: %w", err)
		}
		response, err := client.Do(request)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("download archive: %w", err)
		} else if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
			_ = response.Body.Close()
			cancel()
			lastErr = fmt.Errorf("archive response status %s", response.Status)
		} else if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			cancel()
			return 0, "", fmt.Errorf("archive response status %s", response.Status)
		} else {
			actualSize, digest, readErr := readArchiveResponse(response, target, pkg.FileSize)
			cancel()
			if readErr == nil {
				return actualSize, digest, nil
			}
			if !isRetryableReadError(readErr) {
				return 0, "", readErr
			}
			lastErr = readErr
		}

		if attempt == attempts-1 {
			return 0, "", lastErr
		}
		if err := c.wait(ctx, retryDelay(attempt)); err != nil {
			return 0, "", err
		}
	}
	return 0, "", lastErr
}

func resetTemporaryFile(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate archive temporary file: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek archive temporary file: %w", err)
	}
	return nil
}

func readArchiveResponse(response *http.Response, target *os.File, metadataSize int64) (int64, string, error) {
	defer response.Body.Close()

	hasContentLength := response.Header.Get("Content-Length") != "" || response.ContentLength > 0
	contentLength := response.ContentLength
	if header := response.Header.Get("Content-Length"); header != "" {
		parsed, err := strconv.ParseInt(header, 10, 64)
		if err != nil {
			return 0, "", fmt.Errorf("archive Content-Length is invalid: %w", err)
		}
		contentLength = parsed
	}
	if hasContentLength && contentLength <= 0 {
		return 0, "", fmt.Errorf("archive Content-Length size must be positive")
	}
	if metadataSize > 0 && hasContentLength && contentLength != metadataSize {
		return 0, "", fmt.Errorf("archive Content-Length size %d does not match metadata size %d", contentLength, metadataSize)
	}
	if metadataSize == 0 && hasContentLength && contentLength > maxUnknownArchive {
		return 0, "", fmt.Errorf("archive Content-Length exceeds unknown-size limit of %d bytes", maxUnknownArchive)
	}

	limit := maxUnknownArchive
	if metadataSize > 0 {
		limit = metadataSize
	}
	readLimit := limit
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	hash := sha256.New()
	actualSize, err := io.Copy(io.MultiWriter(target, hash), io.LimitReader(response.Body, readLimit))
	if err != nil {
		if hasContentLength && actualSize != contentLength {
			return 0, "", fmt.Errorf("archive actual size %d does not match Content-Length size %d: %w", actualSize, contentLength, err)
		}
		return 0, "", fmt.Errorf("read archive response: %w", err)
	}
	if actualSize > limit {
		if metadataSize == 0 {
			return 0, "", fmt.Errorf("archive exceeds unknown-size limit of %d bytes", maxUnknownArchive)
		}
		return 0, "", fmt.Errorf("archive actual size exceeds metadata size %d", metadataSize)
	}
	if actualSize <= 0 {
		return 0, "", fmt.Errorf("archive actual size must be positive")
	}
	if metadataSize > 0 && actualSize != metadataSize {
		return 0, "", fmt.Errorf("archive actual size %d does not match metadata size %d", actualSize, metadataSize)
	}
	if hasContentLength && actualSize != contentLength {
		return 0, "", fmt.Errorf("archive actual size %d does not match Content-Length size %d", actualSize, contentLength)
	}
	return actualSize, hex.EncodeToString(hash.Sum(nil)), nil
}

func isRetryableReadError(err error) bool {
	return strings.Contains(err.Error(), "read archive response")
}

func retryDelay(attempt int) time.Duration {
	if attempt == 0 {
		return time.Second
	}
	return 2 * time.Second
}

func (c *ArchiveCache) wait(ctx context.Context, delay time.Duration) error {
	if c.Backoff != nil {
		return c.Backoff(ctx, delay)
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

func validateLockedBytes(prior LockedPackage, actualSize int64, digest string) error {
	if !hasLockedPackage(prior) {
		return nil
	}
	if prior.FileSize != actualSize {
		return fmt.Errorf("locked size %d does not match archive size %d", prior.FileSize, actualSize)
	}
	if !strings.EqualFold(prior.SHA256, digest) {
		return fmt.Errorf("locked SHA-256 does not match archive bytes")
	}
	return nil
}

func lockedPackageForArchive(pkg ResolvedPackage, size int64, digest string) LockedPackage {
	return LockedPackage{
		Ref:          pkg.Ref,
		FullName:     pkg.FullName,
		DownloadURL:  pkg.DownloadURL,
		FileSize:     size,
		SHA256:       digest,
		Dependencies: append([]PackageRef(nil), pkg.Dependencies...),
	}
}

func ExtractArchive(archivePath, destination string, policy ExtractPolicy) (ExtractedArchive, error) {
	file, size, err := openArchiveFile(archivePath)
	if err != nil {
		return ExtractedArchive{}, err
	}
	defer file.Close()

	reader, err := zip.NewReader(file, size)
	if err != nil {
		return ExtractedArchive{}, fmt.Errorf("open ZIP archive: %w", err)
	}
	entries, manifestFile, uncompressedSize, err := inspectZIP(reader.File)
	if err != nil {
		return ExtractedArchive{}, err
	}
	expected, err := archiveReferenceFromPath(archivePath)
	if err != nil {
		return ExtractedArchive{}, err
	}
	manifest, _, err := readAndValidateManifest(manifestFile, expected, nil, false)
	if err != nil {
		return ExtractedArchive{}, err
	}

	selected, err := selectArchiveEntries(entries, policy)
	if err != nil {
		return ExtractedArchive{}, err
	}
	destinationRoot, err := openDirectoryPath(destination, true)
	if err != nil {
		return ExtractedArchive{}, fmt.Errorf("open extraction destination: %w", err)
	}
	defer unix.Close(destinationRoot)
	if err := requireFilesystemSpace(destinationRoot, uncompressedSize, "extraction destination"); err != nil {
		return ExtractedArchive{}, err
	}
	if err := verifyZIPContents(entries); err != nil {
		return ExtractedArchive{}, err
	}

	files := make([]string, 0, len(selected))
	for _, entry := range selected {
		if entry.isDirectory {
			if err := ensureArchiveDirectory(destinationRoot, entry.outputPath); err != nil {
				return ExtractedArchive{}, fmt.Errorf("create archive directory %s: %w", entry.outputPath, err)
			}
			continue
		}
		if err := extractArchiveFile(destinationRoot, entry); err != nil {
			return ExtractedArchive{}, fmt.Errorf("extract archive file %s: %w", entry.outputPath, err)
		}
		files = append(files, entry.outputPath)
	}
	sort.Strings(files)
	return ExtractedArchive{Manifest: manifest, Files: files}, nil
}

func openArchiveFile(archivePath string) (*os.File, int64, error) {
	if !filepath.IsAbs(archivePath) {
		return nil, 0, fmt.Errorf("archive path must be absolute")
	}
	parent, err := openDirectoryPath(filepath.Dir(archivePath), false)
	if err != nil {
		return nil, 0, fmt.Errorf("open archive parent: %w", err)
	}
	defer unix.Close(parent)
	name := filepath.Base(archivePath)
	if _, err := relativePathParts(name); err != nil {
		return nil, 0, fmt.Errorf("invalid archive filename: %w", err)
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open archive: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, 0, fmt.Errorf("stat archive: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, 0, fmt.Errorf("archive is not a regular file")
	}
	return os.NewFile(uintptr(fd), name), stat.Size, nil
}

func validatePackageZIP(file *os.File, size int64, expected PackageRef, dependencies []PackageRef) error {
	reader, err := zip.NewReader(file, size)
	if err != nil {
		return fmt.Errorf("open ZIP archive: %w", err)
	}
	entries, manifestFile, _, err := inspectZIP(reader.File)
	if err != nil {
		return err
	}
	if _, _, err := readAndValidateManifest(manifestFile, expected, dependencies, true); err != nil {
		return err
	}
	return verifyZIPContents(entries)
}

func inspectZIP(files []*zip.File) ([]archiveEntry, *zip.File, uint64, error) {
	entries := make([]archiveEntry, 0, len(files))
	seen := make(map[string]bool, len(files))
	kinds := make(map[string]bool, len(files))
	var manifestFile *zip.File
	var total uint64
	for _, file := range files {
		archivePath, isDirectory, err := safeArchivePath(file)
		if err != nil {
			return nil, nil, 0, err
		}
		if seen[archivePath] {
			return nil, nil, 0, fmt.Errorf("duplicate archive destination %q", archivePath)
		}
		seen[archivePath] = true
		kinds[archivePath] = isDirectory
		if !isDirectory {
			if math.MaxUint64-total < file.UncompressedSize64 {
				return nil, nil, 0, fmt.Errorf("archive uncompressed size overflows")
			}
			total += file.UncompressedSize64
		}
		if archivePath == "manifest.json" {
			if isDirectory {
				return nil, nil, 0, fmt.Errorf("archive manifest is not a regular file")
			}
			manifestFile = file
		}
		entries = append(entries, archiveEntry{file: file, archivePath: archivePath, isDirectory: isDirectory})
	}
	for archivePath, isDirectory := range kinds {
		parts := strings.Split(archivePath, "/")
		for i := 1; i < len(parts); i++ {
			ancestor := strings.Join(parts[:i], "/")
			if ancestorIsDirectory, ok := kinds[ancestor]; ok && !ancestorIsDirectory {
				return nil, nil, 0, fmt.Errorf("archive destination %q has file parent %q", archivePath, ancestor)
			}
		}
		if !isDirectory {
			prefix := archivePath + "/"
			for other := range kinds {
				if strings.HasPrefix(other, prefix) {
					return nil, nil, 0, fmt.Errorf("archive file destination %q contains another entry", archivePath)
				}
			}
		}
	}
	if manifestFile == nil {
		return nil, nil, 0, fmt.Errorf("archive is missing root manifest.json")
	}
	return entries, manifestFile, total, nil
}

func safeArchivePath(file *zip.File) (string, bool, error) {
	name := file.Name
	if name == "" {
		return "", false, fmt.Errorf("archive path is empty")
	}
	if strings.Contains(name, `\`) {
		return "", false, fmt.Errorf("archive path %q contains a backslash", name)
	}
	if path.IsAbs(name) || hasWindowsVolumePrefix(name) {
		return "", false, fmt.Errorf("archive path %q is absolute", name)
	}
	isDirectory := file.Mode().IsDir()
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" {
		return "", false, fmt.Errorf("archive path %q is empty", name)
	}
	parts := strings.Split(trimmed, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", false, fmt.Errorf("archive path %q does not remain safely below its root", name)
		}
	}
	cleaned := path.Clean(trimmed)
	mode := file.Mode()
	if !mode.IsRegular() && !isDirectory {
		return "", false, fmt.Errorf("archive path %q is not a regular file or directory", name)
	}
	return cleaned, isDirectory, nil
}

func verifyZIPContents(entries []archiveEntry) error {
	for _, entry := range entries {
		if entry.isDirectory {
			continue
		}
		reader, err := entry.file.Open()
		if err != nil {
			return fmt.Errorf("open archive entry %q: %w", entry.archivePath, err)
		}
		read, readErr := io.Copy(io.Discard, reader)
		closeErr := reader.Close()
		if readErr != nil {
			return fmt.Errorf("verify archive entry %q: %w", entry.archivePath, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close archive entry %q: %w", entry.archivePath, closeErr)
		}
		if uint64(read) != entry.file.UncompressedSize64 {
			return fmt.Errorf("verify archive entry %q: read %d bytes, want %d", entry.archivePath, read, entry.file.UncompressedSize64)
		}
	}
	return nil
}

func hasWindowsVolumePrefix(name string) bool {
	return len(name) >= 2 && ((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z')) && name[1] == ':'
}

func archiveReferenceFromPath(archivePath string) (PackageRef, error) {
	name := filepath.Base(archivePath)
	if filepath.Ext(name) != ".zip" {
		return PackageRef{}, fmt.Errorf("archive filename must end in .zip")
	}
	ref, err := parseDependency(strings.TrimSuffix(name, ".zip"))
	if err != nil {
		return PackageRef{}, fmt.Errorf("archive filename does not identify a package: %w", err)
	}
	return ref, nil
}

func readAndValidateManifest(file *zip.File, expected PackageRef, expectedDependencies []PackageRef, compareDependencies bool) (PackageRef, []PackageRef, error) {
	reader, err := file.Open()
	if err != nil {
		return PackageRef{}, nil, fmt.Errorf("open archive manifest: %w", err)
	}
	defer reader.Close()
	var manifest archiveManifest
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&manifest); err != nil {
		return PackageRef{}, nil, fmt.Errorf("decode archive manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return PackageRef{}, nil, fmt.Errorf("archive manifest contains multiple JSON values")
		}
		return PackageRef{}, nil, fmt.Errorf("read archive manifest: %w", err)
	}
	if manifest.Name != expected.Name || manifest.Version != expected.Version ||
		(manifest.Namespace != "" && manifest.Namespace != expected.Namespace) ||
		(manifest.FullName != "" && manifest.FullName != packageFullName(expected.Namespace, expected.Name)) {
		return PackageRef{}, nil, fmt.Errorf("archive manifest identity does not match %s", packageVersionFullName(expected))
	}
	if !manifest.Dependencies.present || manifest.Dependencies.null {
		return PackageRef{}, nil, fmt.Errorf("archive manifest dependencies must be a non-null JSON array")
	}

	dependencies := make([]PackageRef, 0, len(manifest.Dependencies.values))
	seen := make(map[PackageRef]bool, len(manifest.Dependencies.values))
	for _, dependency := range manifest.Dependencies.values {
		ref, err := parseDependency(dependency)
		if err != nil {
			return PackageRef{}, nil, fmt.Errorf("archive manifest dependency: %w", err)
		}
		if seen[ref] {
			return PackageRef{}, nil, fmt.Errorf("archive manifest contains duplicate dependency %q", dependency)
		}
		seen[ref] = true
		dependencies = append(dependencies, ref)
	}
	sortPackageRefs(dependencies)
	if compareDependencies {
		want := append([]PackageRef(nil), expectedDependencies...)
		sortPackageRefs(want)
		if !samePackageRefs(dependencies, want) {
			return PackageRef{}, nil, fmt.Errorf("archive manifest dependencies do not match resolved dependencies")
		}
	}
	return expected, dependencies, nil
}

func samePackageRefs(left, right []PackageRef) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func selectArchiveEntries(entries []archiveEntry, policy ExtractPolicy) ([]archiveEntry, error) {
	selected := make([]archiveEntry, 0, len(entries))
	switch policy {
	case ExtractPlugin:
		for _, entry := range entries {
			if entry.isDirectory || strings.Contains(entry.archivePath, "/") || !strings.EqualFold(path.Ext(entry.archivePath), ".dll") {
				continue
			}
			entry.outputPath = entry.archivePath
			selected = append(selected, entry)
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("plugin archive contains no root DLLs")
		}
	case ExtractBepInEx:
		const wrapper = "BepInExPack_V_Rising"
		wrapperPrefix := wrapper + "/"
		foundWrapperContent := false
		for _, entry := range entries {
			if !strings.Contains(entry.archivePath, "/") {
				if entry.isDirectory && entry.archivePath != wrapper {
					return nil, fmt.Errorf("BepInEx archive must contain a single BepInExPack_V_Rising wrapper directory")
				}
				continue
			}
			if !strings.HasPrefix(entry.archivePath, wrapperPrefix) {
				return nil, fmt.Errorf("BepInEx archive must contain a single BepInExPack_V_Rising wrapper directory")
			}
			outputPath := strings.TrimPrefix(entry.archivePath, wrapperPrefix)
			if outputPath == "" {
				continue
			}
			foundWrapperContent = true
			entry.outputPath = outputPath
			selected = append(selected, entry)
		}
		if !foundWrapperContent {
			return nil, fmt.Errorf("BepInEx archive must contain a single BepInExPack_V_Rising wrapper directory")
		}
	default:
		return nil, fmt.Errorf("unknown archive extraction policy %d", policy)
	}
	return selected, nil
}

func ensureArchiveDirectory(root int, relativePath string) error {
	parent, _, err := openRelativeParent(root, relativePath+"/.archive-entry", true, unix.Fsync)
	if err != nil {
		return err
	}
	return unix.Close(parent)
}

func extractArchiveFile(root int, entry archiveEntry) error {
	parent, name, err := openRelativeParent(root, entry.outputPath, true, unix.Fsync)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	target := os.NewFile(uintptr(fd), name)
	keep := false
	defer func() {
		target.Close()
		if !keep {
			_ = unix.Unlinkat(parent, name, 0)
		}
	}()

	source, err := entry.file.Open()
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(target, source)
	closeErr := source.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if uint64(written) != entry.file.UncompressedSize64 {
		return fmt.Errorf("wrote %d bytes, want %d", written, entry.file.UncompressedSize64)
	}
	if err := unix.Fchmod(fd, 0o644); err != nil {
		return err
	}
	if err := target.Sync(); err != nil {
		return err
	}
	if err := target.Close(); err != nil {
		return err
	}
	if err := unix.Fsync(parent); err != nil {
		return err
	}
	keep = true
	return nil
}

func requireFilesystemSpace(fd int, contentSize uint64, label string) error {
	if math.MaxUint64-contentSize < archiveSafetyReserve {
		return fmt.Errorf("insufficient space in %s", label)
	}
	required := contentSize + archiveSafetyReserve
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return fmt.Errorf("inspect free space in %s: %w", label, err)
	}
	available := uint64(stat.Bavail)
	blockSize := uint64(stat.Bsize)
	if blockSize != 0 && available > math.MaxUint64/blockSize {
		available = math.MaxUint64
	} else {
		available *= blockSize
	}
	if available < required {
		return fmt.Errorf("insufficient space in %s: have %d bytes, need %d", label, available, required)
	}
	return nil
}
