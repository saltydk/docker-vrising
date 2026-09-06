package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	cacheFilesystemHook func(archiveCacheHookStage, int, string, string) error
}

type archiveCacheHookStage uint8

const (
	archiveCacheBeforeValidation archiveCacheHookStage = iota
	archiveCacheBeforePublication
)

type ExtractPolicy int

const (
	ExtractBepInEx ExtractPolicy = iota
	ExtractPlugin
)

type ExtractedArchive struct {
	Manifest PackageRef
	Files    []string
}

// ValidatedArchive owns the verified archive descriptor returned by Fetch.
// The caller must close it after the last extraction attempt.
type ValidatedArchive struct {
	mu     sync.Mutex
	file   *os.File
	locked LockedPackage
}

func (a *ValidatedArchive) LockedPackage() LockedPackage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneLockedPackage(a.locked)
}

func (a *ValidatedArchive) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.file = nil
	return err
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

type transientArchiveDownloadError struct {
	err error
}

func (e *transientArchiveDownloadError) Error() string {
	return e.err.Error()
}

func (e *transientArchiveDownloadError) Unwrap() error {
	return e.err
}

func (c *ArchiveCache) Fetch(ctx context.Context, pkg ResolvedPackage, prior LockedPackage) (*ValidatedArchive, error) {
	if err := validateResolvedArchivePackage(pkg); err != nil {
		return nil, err
	}
	if err := validatePriorLockedPackage(pkg, prior); err != nil {
		return nil, err
	}

	cacheRoot, err := openDirectoryPath(c.Dir, true)
	if err != nil {
		return nil, fmt.Errorf("open archive cache: %w", err)
	}
	defer unix.Close(cacheRoot)
	if err := validateArchiveCacheRoot(cacheRoot); err != nil {
		return nil, err
	}

	archiveName := packageVersionFullName(pkg.Ref) + ".zip"
	if _, err := relativePathParts(archiveName); err != nil {
		return nil, fmt.Errorf("invalid cache archive name: %w", err)
	}

	archive, found, err := cachedArchive(cacheRoot, archiveName, pkg, prior)
	if err != nil {
		return nil, err
	}
	if found {
		return archive, nil
	}

	requiredSize := uint64(maxUnknownArchive)
	if pkg.FileSize > 0 {
		requiredSize = uint64(pkg.FileSize)
	}
	if err := requireFilesystemSpace(cacheRoot, requiredSize, "archive cache"); err != nil {
		return nil, err
	}

	temporaryName, temporaryFD, err := createArchiveTemporaryFileAt(cacheRoot, archiveName)
	if err != nil {
		return nil, fmt.Errorf("create archive cache temporary file: %w", err)
	}
	temporary := os.NewFile(uintptr(temporaryFD), temporaryName)
	removeTemporary := true
	transferOwnership := false
	defer func() {
		if !transferOwnership {
			temporary.Close()
		}
		if removeTemporary {
			_ = unix.Unlinkat(cacheRoot, temporaryName, 0)
		}
	}()

	actualSize, digest, err := c.download(ctx, temporary, pkg)
	if err != nil {
		return nil, err
	}
	if err := validateLockedBytes(prior, actualSize, digest); err != nil {
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		return nil, fmt.Errorf("sync downloaded archive: %w", err)
	}
	if c.cacheFilesystemHook != nil {
		if err := c.cacheFilesystemHook(archiveCacheBeforeValidation, cacheRoot, temporaryName, archiveName); err != nil {
			return nil, fmt.Errorf("archive cache validation hook: %w", err)
		}
	}
	if err := validatePackageZIP(temporary, actualSize, pkg.Ref, pkg.Dependencies); err != nil {
		return nil, fmt.Errorf("validate downloaded archive: %w", err)
	}
	if c.cacheFilesystemHook != nil {
		if err := c.cacheFilesystemHook(archiveCacheBeforePublication, cacheRoot, temporaryName, archiveName); err != nil {
			return nil, fmt.Errorf("archive cache publication hook: %w", err)
		}
	}
	if err := verifyNamedArchiveIdentity(cacheRoot, temporaryName, temporary); err != nil {
		return nil, fmt.Errorf("verify archive temporary identity: %w", err)
	}
	if err := unix.Renameat2(cacheRoot, temporaryName, cacheRoot, archiveName, unix.RENAME_NOREPLACE); err != nil {
		return nil, fmt.Errorf("rename downloaded archive: %w", err)
	}
	removeTemporary = false
	if err := verifyNamedArchiveIdentity(cacheRoot, archiveName, temporary); err != nil {
		_ = unix.Unlinkat(cacheRoot, archiveName, 0)
		_ = unix.Fsync(cacheRoot)
		return nil, fmt.Errorf("verify published archive identity: %w", err)
	}
	if err := unix.Fsync(cacheRoot); err != nil {
		return nil, fmt.Errorf("sync archive cache: %w", err)
	}

	transferOwnership = true
	return newValidatedArchive(temporary, lockedPackageForArchive(pkg, actualSize, digest)), nil
}

func createArchiveTemporaryFileAt(parent int, name string) (string, int, error) {
	for range 16 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", -1, fmt.Errorf("create temporary suffix: %w", err)
		}
		temporaryName := "." + name + ".tmp-" + hex.EncodeToString(suffix[:])
		fd, err := unix.Openat(parent, temporaryName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err == unix.EEXIST {
			continue
		}
		if err != nil {
			return "", -1, err
		}
		if err := unix.Fchmod(fd, 0o600); err != nil {
			unix.Close(fd)
			unix.Unlinkat(parent, temporaryName, 0)
			return "", -1, err
		}
		return temporaryName, fd, nil
	}
	return "", -1, fmt.Errorf("create temporary file: too many collisions")
}

func validateResolvedArchivePackage(pkg ResolvedPackage) error {
	if !validPackageIdentifier(pkg.Ref.Namespace, false) ||
		!validPackageIdentifier(pkg.Ref.Name, true) ||
		!semanticVersion.MatchString(pkg.Ref.Version) {
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

func validPackageIdentifier(value string, allowHyphen bool) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '_' || allowHyphen && character == '-' {
			continue
		}
		return false
	}
	return true
}

func validateArchiveCacheRoot(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat archive cache: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("archive cache is not a directory")
	}
	if os.Geteuid() != 0 && stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("archive cache is not owned by runtime user")
	}
	if os.Geteuid() != 0 && stat.Mode&0o7777 != 0o700 {
		return fmt.Errorf("archive cache mode is %04o, want 0700", stat.Mode&0o7777)
	}
	return nil
}

func verifyNamedArchiveIdentity(root int, name string, file *os.File) error {
	var held unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &held); err != nil {
		return err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(root, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if held.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("archive identity is not a regular file")
	}
	if held.Dev != named.Dev || held.Ino != named.Ino {
		return fmt.Errorf("archive pathname does not reference held inode")
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

func cachedArchive(root int, name string, pkg ResolvedPackage, prior LockedPackage) (*ValidatedArchive, bool, error) {
	fd, err := unix.Openat(root, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == unix.ENOENT {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open cached archive: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	transferOwnership := false
	defer func() {
		if !transferOwnership {
			file.Close()
		}
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, false, fmt.Errorf("stat cached archive: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, false, fmt.Errorf("cached archive is not a regular file")
	}
	if stat.Size <= 0 {
		return nil, false, fmt.Errorf("cached archive size must be positive")
	}
	if pkg.FileSize > 0 && stat.Size != pkg.FileSize {
		return nil, false, fmt.Errorf("cached archive size %d does not match metadata size %d", stat.Size, pkg.FileSize)
	}
	digest, err := hashFile(file)
	if err != nil {
		return nil, false, fmt.Errorf("hash cached archive: %w", err)
	}
	if err := validateLockedBytes(prior, stat.Size, digest); err != nil {
		return nil, false, err
	}
	if err := validatePackageZIP(file, stat.Size, pkg.Ref, pkg.Dependencies); err != nil {
		return nil, false, fmt.Errorf("validate cached archive: %w", err)
	}
	if err := verifyNamedArchiveIdentity(root, name, file); err != nil {
		return nil, false, fmt.Errorf("verify cached archive identity: %w", err)
	}
	transferOwnership = true
	return newValidatedArchive(file, lockedPackageForArchive(pkg, stat.Size, digest)), true, nil
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
			requestErr := classifyArchiveTransferError(ctx, requestContext, "download archive", err)
			cancel()
			if !isTransientArchiveDownload(requestErr) {
				return 0, "", requestErr
			}
			lastErr = requestErr
		} else if response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError && response.StatusCode <= 599 {
			_ = response.Body.Close()
			cancel()
			lastErr = transientArchiveDownload(fmt.Errorf("archive response status %s", response.Status))
		} else if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			cancel()
			return 0, "", fmt.Errorf("archive response status %s", response.Status)
		} else {
			actualSize, digest, readErr := readArchiveResponse(ctx, requestContext, response, target, pkg.FileSize)
			cancel()
			if readErr == nil {
				return actualSize, digest, nil
			}
			if !isTransientArchiveDownload(readErr) {
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

func classifyArchiveTransferError(parent, attempt context.Context, operation string, err error) error {
	if parentErr := parent.Err(); parentErr != nil {
		return fmt.Errorf("%s: %w", operation, parentErr)
	}
	if attemptErr := attempt.Err(); attemptErr != nil {
		classified := fmt.Errorf("%s: %w", operation, attemptErr)
		if errors.Is(attemptErr, context.DeadlineExceeded) {
			return transientArchiveDownload(classified)
		}
		return classified
	}
	classified := fmt.Errorf("%s: %w", operation, err)
	if errors.Is(err, context.Canceled) {
		return classified
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		isTransientArchiveNetworkError(err) {
		return transientArchiveDownload(classified)
	}
	return classified
}

func isTransientArchiveNetworkError(err error) bool {
	for _, permanent := range []error{
		unix.EACCES, unix.EPERM, unix.ENOENT, unix.ENOTDIR,
		unix.EMFILE, unix.ENFILE, unix.ENOMEM,
	} {
		if errors.Is(err, permanent) {
			return false
		}
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return dnsError.IsTimeout || dnsError.IsTemporary
	}
	for _, transient := range []error{unix.ECONNRESET, unix.ECONNREFUSED, unix.ECONNABORTED} {
		if errors.Is(err, transient) {
			return true
		}
	}
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}

func transientArchiveDownload(err error) error {
	return &transientArchiveDownloadError{err: err}
}

func isTransientArchiveDownload(err error) bool {
	var transient *transientArchiveDownloadError
	return errors.As(err, &transient)
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

func readArchiveResponse(parent, attempt context.Context, response *http.Response, target *os.File, metadataSize int64) (int64, string, error) {
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
	var actualSize int64
	reader := io.LimitReader(response.Body, readLimit)
	buffer := make([]byte, 32*1024)
	for {
		read, readErr := reader.Read(buffer)
		if read > 0 {
			written, writeErr := target.Write(buffer[:read])
			if writeErr != nil {
				return 0, "", fmt.Errorf("write archive temporary file: %w", writeErr)
			}
			if written != read {
				return 0, "", fmt.Errorf("write archive temporary file: %w", io.ErrShortWrite)
			}
			_, _ = hash.Write(buffer[:read])
			actualSize += int64(read)
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		failure := error(readErr)
		if hasContentLength && actualSize != contentLength {
			failure = fmt.Errorf("archive actual size %d does not match Content-Length size %d: %w", actualSize, contentLength, readErr)
		}
		return 0, "", classifyArchiveTransferError(parent, attempt, "read archive response", failure)
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
		if actualSize < contentLength {
			failure := fmt.Errorf("archive actual size %d does not match Content-Length size %d: %w", actualSize, contentLength, io.ErrUnexpectedEOF)
			return 0, "", classifyArchiveTransferError(parent, attempt, "read archive response", failure)
		}
		return 0, "", fmt.Errorf("archive actual size %d does not match Content-Length size %d", actualSize, contentLength)
	}
	return actualSize, hex.EncodeToString(hash.Sum(nil)), nil
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

func newValidatedArchive(file *os.File, locked LockedPackage) *ValidatedArchive {
	return &ValidatedArchive{file: file, locked: cloneLockedPackage(locked)}
}

func cloneLockedPackage(pkg LockedPackage) LockedPackage {
	pkg.Dependencies = append([]PackageRef(nil), pkg.Dependencies...)
	return pkg
}

func ExtractArchive(archive *ValidatedArchive, destination string, policy ExtractPolicy) (ExtractedArchive, error) {
	if archive == nil {
		return ExtractedArchive{}, fmt.Errorf("validated archive is nil")
	}
	archive.mu.Lock()
	defer archive.mu.Unlock()
	if archive.file == nil {
		return ExtractedArchive{}, fmt.Errorf("validated archive is closed")
	}
	file := archive.file
	locked := cloneLockedPackage(archive.locked)
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return ExtractedArchive{}, fmt.Errorf("stat validated archive: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size <= 0 {
		return ExtractedArchive{}, fmt.Errorf("validated archive is not a non-empty regular file")
	}
	digest, err := hashFile(file)
	if err != nil {
		return ExtractedArchive{}, fmt.Errorf("hash validated archive: %w", err)
	}
	if err := validateLockedBytes(locked, stat.Size, digest); err != nil {
		return ExtractedArchive{}, err
	}

	reader, err := zip.NewReader(file, stat.Size)
	if err != nil {
		return ExtractedArchive{}, fmt.Errorf("open ZIP archive: %w", err)
	}
	entries, manifestFile, uncompressedSize, err := inspectZIP(reader.File)
	if err != nil {
		return ExtractedArchive{}, err
	}
	manifest, _, err := readAndValidateManifest(manifestFile, locked.Ref, locked.Dependencies, true)
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
		seenDestinations := make(map[string]bool)
		for _, entry := range entries {
			if entry.archivePath == "manifest.json" {
				continue
			}
			if !strings.Contains(entry.archivePath, "/") {
				if entry.isDirectory {
					if entry.archivePath == "plugins" {
						continue
					}
					return nil, fmt.Errorf("plugin archive contains unexpected directory %q", entry.archivePath)
				}
				if entry.archivePath == "plugins" {
					return nil, fmt.Errorf("plugin archive plugins entry is not a directory")
				}
				if !strings.EqualFold(path.Ext(entry.archivePath), ".dll") {
					continue
				}
				entry.outputPath = entry.archivePath
			} else {
				if !strings.HasPrefix(entry.archivePath, "plugins/") {
					return nil, fmt.Errorf("plugin archive contains unexpected layout %q", entry.archivePath)
				}
				name := strings.TrimPrefix(entry.archivePath, "plugins/")
				if entry.isDirectory || name == "" || strings.Contains(name, "/") {
					return nil, fmt.Errorf("plugin archive contains unexpected plugins layout %q", entry.archivePath)
				}
				if !strings.EqualFold(path.Ext(name), ".dll") {
					return nil, fmt.Errorf("plugin archive contains non-DLL plugins payload %q", entry.archivePath)
				}
				entry.outputPath = name
			}
			destinationKey := strings.ToLower(entry.outputPath)
			if seenDestinations[destinationKey] {
				return nil, fmt.Errorf("duplicate plugin destination %q", entry.outputPath)
			}
			seenDestinations[destinationKey] = true
			selected = append(selected, entry)
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("plugin archive contains no DLLs at root or directly below plugins")
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
