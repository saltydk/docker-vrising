package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var archiveTestRef = PackageRef{Namespace: "acme", Name: "ExamplePlugin", Version: "1.2.3"}

func TestArchiveCacheDownloadsAndRecordsSHA256(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	pkg := resolvedArchivePackage(server.URL, body)

	locked, archivePath, err := (&ArchiveCache{Dir: t.TempDir(), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(body)
	if locked.Ref != pkg.Ref || locked.FullName != pkg.FullName || locked.DownloadURL != pkg.DownloadURL ||
		locked.FileSize != int64(len(body)) || locked.SHA256 != hex.EncodeToString(wantHash[:]) ||
		!reflect.DeepEqual(locked.Dependencies, pkg.Dependencies) {
		t.Fatalf("Fetch() lock = %#v, want package identity, size, dependencies, and SHA-256", locked)
	}
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("cached archive bytes differ from response")
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cached archive mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestArchiveCacheDerivesOmittedSize(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})

	t.Run("positive Content-Length", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write(body)
		}))
		defer server.Close()
		pkg := resolvedArchivePackage(server.URL, nil)

		locked, _, err := (&ArchiveCache{Dir: t.TempDir(), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
		if err != nil {
			t.Fatal(err)
		}
		if locked.FileSize != int64(len(body)) {
			t.Fatalf("FileSize = %d, want actual size %d", locked.FileSize, len(body))
		}
	})

	t.Run("no Content-Length", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Trailer", "X-End")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			w.Header().Set("X-End", "yes")
		}))
		defer server.Close()
		pkg := resolvedArchivePackage(server.URL, nil)

		locked, _, err := (&ArchiveCache{Dir: t.TempDir(), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
		if err != nil {
			t.Fatal(err)
		}
		if locked.FileSize != int64(len(body)) {
			t.Fatalf("FileSize = %d, want actual size %d", locked.FileSize, len(body))
		}
	})
}

func TestArchiveCacheReusesMatchingFile(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, packageVersionFullName(archiveTestRef)+".zip")
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	pkg := resolvedArchivePackage(server.URL, body)
	wantHash := sha256.Sum256(body)
	prior := lockedArchivePackage(pkg, int64(len(body)), hex.EncodeToString(wantHash[:]))

	locked, gotPath, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), pkg, prior)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0 for matching cache", requests)
	}
	if gotPath != cachePath || !reflect.DeepEqual(locked, prior) {
		t.Fatalf("Fetch() = (%#v, %q), want prior lock and %q", locked, gotPath, cachePath)
	}
}

func TestArchiveCacheRejectsSizeMismatch(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	tests := []struct {
		name          string
		metadataSize  int64
		contentLength int
	}{
		{name: "metadata and Content-Length", metadataSize: int64(len(body) + 1), contentLength: len(body)},
		{name: "metadata and actual", metadataSize: int64(len(body) + 1), contentLength: -1},
		{name: "Content-Length and actual", metadataSize: 0, contentLength: len(body) + 1},
		{name: "zero Content-Length", metadataSize: 0, contentLength: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.contentLength >= 0 {
					w.Header().Set("Content-Length", fmt.Sprint(tt.contentLength))
				} else {
					w.Header().Set("Trailer", "X-End")
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			pkg := resolvedArchivePackage(server.URL, nil)
			pkg.FileSize = tt.metadataSize

			_, _, err := (&ArchiveCache{Dir: t.TempDir(), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
			if err == nil || !strings.Contains(err.Error(), "size") {
				t.Fatalf("Fetch() error = %v, want size mismatch", err)
			}
		})
	}
}

func TestArchiveCacheRejectsChangedBytesForLockedVersion(t *testing.T) {
	priorBody := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "aaaaaa"})
	changedBody := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "bbbbbb"})
	if len(priorBody) != len(changedBody) {
		t.Fatalf("test ZIP lengths differ: %d and %d", len(priorBody), len(changedBody))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(changedBody)
	}))
	defer server.Close()
	pkg := resolvedArchivePackage(server.URL, changedBody)
	priorHash := sha256.Sum256(priorBody)
	prior := lockedArchivePackage(pkg, int64(len(priorBody)), hex.EncodeToString(priorHash[:]))

	_, archivePath, err := (&ArchiveCache{Dir: t.TempDir(), Client: server.Client()}).Fetch(t.Context(), pkg, prior)
	if err == nil || !strings.Contains(err.Error(), "locked SHA-256") {
		t.Fatalf("Fetch() error = %v, want changed locked bytes rejection", err)
	}
	if archivePath != "" {
		t.Fatalf("Fetch() path = %q after rejection, want empty", archivePath)
	}
}

func TestArchiveCacheRejectsInvalidPriorSize(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	pkg := resolvedArchivePackage(server.URL, body)
	wantHash := sha256.Sum256(body)
	prior := lockedArchivePackage(pkg, int64(len(body)+1), hex.EncodeToString(wantHash[:]))

	_, _, err := (&ArchiveCache{Dir: t.TempDir(), Client: server.Client()}).Fetch(t.Context(), pkg, prior)
	if err == nil || !strings.Contains(err.Error(), "locked size") {
		t.Fatalf("Fetch() error = %v, want prior locked size rejection", err)
	}
}

func TestArchiveCacheRejectsOversizedUnknownContentLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(maxUnknownArchive+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	pkg := resolvedArchivePackage(server.URL, nil)

	_, _, err := (&ArchiveCache{Dir: t.TempDir(), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
	if err == nil || !strings.Contains(err.Error(), "unknown-size limit") {
		t.Fatalf("Fetch() error = %v, want unknown-size limit rejection", err)
	}
}

func TestArchiveCacheRetries503ThreeTimes(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var delays []time.Duration
	cache := &ArchiveCache{
		Dir:    t.TempDir(),
		Client: server.Client(),
		Backoff: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}

	_, _, err := cache.Fetch(t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Fetch() error = %v, want final 503", err)
	}
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 3 {
		t.Fatalf("requests = %d, want 3", gotRequests)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !reflect.DeepEqual(delays, want) {
		t.Fatalf("backoff delays = %v, want %v", delays, want)
	}
}

func TestArchiveCacheRejectsInsufficientSpace(t *testing.T) {
	cacheDir := t.TempDir()
	var stat unix.Statfs_t
	if err := unix.Statfs(cacheDir, &stat); err != nil {
		t.Fatal(err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	pkg := ResolvedPackage{
		Ref:         archiveTestRef,
		FullName:    packageVersionFullName(archiveTestRef),
		DownloadURL: "http://127.0.0.1:1/archive.zip",
		FileSize:    available,
		IsActive:    true,
	}

	_, _, err := (&ArchiveCache{Dir: cacheDir}).Fetch(t.Context(), pkg, LockedPackage{})
	if err == nil || !strings.Contains(err.Error(), "space") {
		t.Fatalf("Fetch() error = %v, want insufficient space", err)
	}
}

func TestArchiveCacheRejectsManifestDependencyMismatch(t *testing.T) {
	declared := []PackageRef{{Namespace: "dep", Name: "Library", Version: "2.0.0"}}
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	pkg := resolvedArchivePackage(server.URL, body)
	pkg.Dependencies = declared
	cacheDir := t.TempDir()

	_, _, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
	if err == nil || !strings.Contains(err.Error(), "dependencies") {
		t.Fatalf("Fetch() error = %v, want exact dependency mismatch", err)
	}
	if entries, readErr := os.ReadDir(cacheDir); readErr != nil || len(entries) != 0 {
		t.Fatalf("cache after rejection = %v, error %v; want empty", entries, readErr)
	}
}

func TestArchiveCacheRejectsUnexpectedDependencyForPackageWithNone(t *testing.T) {
	manifestDependencies := []PackageRef{{Namespace: "dep", Name: "Library", Version: "2.0.0"}}
	body := packageZIP(t, archiveTestRef, manifestDependencies, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()

	_, _, err := (&ArchiveCache{Dir: t.TempDir(), Client: server.Client()}).Fetch(
		t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{},
	)
	if err == nil || !strings.Contains(err.Error(), "dependencies") {
		t.Fatalf("Fetch() error = %v, want unexpected dependency rejection", err)
	}
}

func TestArchiveCacheRejectsCorruptZIPEntry(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin bytes"})
	body = corruptZIPEntry(t, body, "ExamplePlugin.dll")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	cacheDir := t.TempDir()

	_, _, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{})
	if err == nil {
		t.Fatal("Fetch() accepted a ZIP with a corrupt plugin entry")
	}
	assertDirectoryEmpty(t, cacheDir)
}

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	archivePath := writePackageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: "../escape.dll", body: "escape"},
	)
	destination := t.TempDir()

	_, err := ExtractArchive(archivePath, destination, ExtractPlugin)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("ExtractArchive() error = %v, want traversal rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(destination), "escape.dll")); !os.IsNotExist(statErr) {
		t.Fatalf("outside file stat error = %v, want not exist", statErr)
	}
}

func TestExtractArchiveRejectsAbsolutePath(t *testing.T) {
	archivePath := writePackageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: "/escape.dll", body: "escape"},
	)

	_, err := ExtractArchive(archivePath, t.TempDir(), ExtractPlugin)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("ExtractArchive() error = %v, want absolute path rejection", err)
	}
}

func TestExtractArchiveRejectsBackslashPath(t *testing.T) {
	archivePath := writePackageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: `nested\escape.dll`, body: "escape"},
	)

	_, err := ExtractArchive(archivePath, t.TempDir(), ExtractPlugin)
	if err == nil || !strings.Contains(err.Error(), "backslash") {
		t.Fatalf("ExtractArchive() error = %v, want backslash rejection", err)
	}
}

func TestExtractArchiveRejectsSymlink(t *testing.T) {
	archivePath := writePackageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: "link.dll", body: "target", mode: os.ModeSymlink | 0o777},
	)

	_, err := ExtractArchive(archivePath, t.TempDir(), ExtractPlugin)
	if err == nil || !strings.Contains(err.Error(), "regular file or directory") {
		t.Fatalf("ExtractArchive() error = %v, want symlink rejection", err)
	}
}

func TestExtractArchiveRejectsDuplicateDestination(t *testing.T) {
	archivePath := writePackageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "first"},
		zipEntry{name: "ExamplePlugin.dll", body: "second"},
	)

	_, err := ExtractArchive(archivePath, t.TempDir(), ExtractPlugin)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("ExtractArchive() error = %v, want duplicate destination rejection", err)
	}
}

func TestExtractArchiveRejectsManifestMismatch(t *testing.T) {
	wrongRef := PackageRef{Namespace: archiveTestRef.Namespace, Name: "WrongPlugin", Version: archiveTestRef.Version}
	body := packageZIP(t, wrongRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	archivePath := filepath.Join(t.TempDir(), packageVersionFullName(archiveTestRef)+".zip")
	if err := os.WriteFile(archivePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()

	_, err := ExtractArchive(archivePath, destination, ExtractPlugin)
	if err == nil || !strings.Contains(err.Error(), "manifest identity") {
		t.Fatalf("ExtractArchive() error = %v, want manifest identity rejection", err)
	}
	assertDirectoryEmpty(t, destination)
}

func TestExtractArchiveRejectsSymlinkedDestinationComponent(t *testing.T) {
	bepRef := PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"}
	archivePath := writePackageZIP(t, bepRef, nil,
		zipEntry{name: "BepInExPack_V_Rising/BepInEx/core/core.dll", body: "core"},
	)
	destination := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(destination, "BepInEx")); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractArchive(archivePath, destination, ExtractBepInEx)
	if err == nil {
		t.Fatal("ExtractArchive() succeeded through symlinked destination component")
	}
	assertDirectoryEmpty(t, external)
}

func TestExtractArchiveExtractsRootPluginDLLsAt0644(t *testing.T) {
	archivePath := writePackageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: "Helper.DLL", body: "helper"},
		zipEntry{name: "README.md", body: "docs"},
		zipEntry{name: "nested/Hidden.dll", body: "hidden"},
	)
	destination := filepath.Join(t.TempDir(), "new", "extract")

	extracted, err := ExtractArchive(archivePath, destination, ExtractPlugin)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{"ExamplePlugin.dll", "Helper.DLL"}
	if extracted.Manifest != archiveTestRef || !reflect.DeepEqual(extracted.Files, wantFiles) {
		t.Fatalf("ExtractArchive() = %#v, want manifest %#v and files %v", extracted, archiveTestRef, wantFiles)
	}
	for _, name := range wantFiles {
		info, statErr := os.Stat(filepath.Join(destination, name))
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("%s mode = %04o, want 0644", name, info.Mode().Perm())
		}
	}
	for _, ignored := range []string{"manifest.json", "README.md", filepath.Join("nested", "Hidden.dll")} {
		if _, err := os.Stat(filepath.Join(destination, ignored)); !os.IsNotExist(err) {
			t.Fatalf("ignored path %q stat error = %v, want not exist", ignored, err)
		}
	}
}

func TestExtractBepInExRequiresSingleWrapperDirectory(t *testing.T) {
	bepRef := PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"}
	tests := []struct {
		name    string
		entries []zipEntry
	}{
		{name: "missing", entries: []zipEntry{{name: "winhttp.dll", body: "dll"}}},
		{name: "additional wrapper", entries: []zipEntry{
			{name: "BepInExPack_V_Rising/winhttp.dll", body: "dll"},
			{name: "OtherWrapper/file.dll", body: "other"},
		}},
		{name: "additional empty wrapper", entries: []zipEntry{
			{name: "BepInExPack_V_Rising/winhttp.dll", body: "dll"},
			{name: "OtherWrapper/", mode: os.ModeDir | 0o755},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archivePath := writePackageZIP(t, bepRef, nil, tt.entries...)
			destination := t.TempDir()

			_, err := ExtractArchive(archivePath, destination, ExtractBepInEx)
			if err == nil || !strings.Contains(err.Error(), "single BepInExPack_V_Rising wrapper") {
				t.Fatalf("ExtractArchive() error = %v, want wrapper rejection", err)
			}
			assertDirectoryEmpty(t, destination)
		})
	}
}

func TestExtractBepInExStripsWrapper(t *testing.T) {
	bepRef := PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"}
	archivePath := writePackageZIP(t, bepRef, nil,
		zipEntry{name: "README.md", body: "package docs"},
		zipEntry{name: "BepInExPack_V_Rising/", mode: os.ModeDir | 0o755},
		zipEntry{name: "BepInExPack_V_Rising/winhttp.dll", body: "dll"},
		zipEntry{name: "BepInExPack_V_Rising/BepInEx/core/core.dll", body: "core"},
	)
	destination := t.TempDir()

	extracted, err := ExtractArchive(archivePath, destination, ExtractBepInEx)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{"BepInEx/core/core.dll", "winhttp.dll"}
	if extracted.Manifest != bepRef || !reflect.DeepEqual(extracted.Files, wantFiles) {
		t.Fatalf("ExtractArchive() = %#v, want manifest %#v and files %v", extracted, bepRef, wantFiles)
	}
	for _, name := range wantFiles {
		if _, err := os.Stat(filepath.Join(destination, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "BepInExPack_V_Rising")); !os.IsNotExist(err) {
		t.Fatalf("wrapper stat error = %v, want stripped wrapper", err)
	}
}

func TestExtractArchiveRejectsInsufficientSpace(t *testing.T) {
	destination := t.TempDir()
	var stat unix.Statfs_t
	if err := unix.Statfs(destination, &stat); err != nil {
		t.Fatal(err)
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	body := packageZIPWithRawEntry(t, archiveTestRef, available+(512<<20)+1)
	archivePath := filepath.Join(t.TempDir(), packageVersionFullName(archiveTestRef)+".zip")
	if err := os.WriteFile(archivePath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractArchive(archivePath, destination, ExtractPlugin)
	if err == nil || !strings.Contains(err.Error(), "space") {
		t.Fatalf("ExtractArchive() error = %v, want insufficient space", err)
	}
	assertDirectoryEmpty(t, destination)
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

type testManifest struct {
	Name         string   `json:"name"`
	Namespace    string   `json:"namespace,omitempty"`
	FullName     string   `json:"FullName,omitempty"`
	Version      string   `json:"version_number"`
	Dependencies []string `json:"dependencies"`
}

func resolvedArchivePackage(downloadURL string, body []byte) ResolvedPackage {
	return ResolvedPackage{
		Ref:         archiveTestRef,
		FullName:    packageVersionFullName(archiveTestRef),
		DownloadURL: downloadURL,
		FileSize:    int64(len(body)),
		IsActive:    true,
	}
}

func lockedArchivePackage(pkg ResolvedPackage, size int64, hash string) LockedPackage {
	return LockedPackage{
		Ref:          pkg.Ref,
		FullName:     pkg.FullName,
		DownloadURL:  pkg.DownloadURL,
		FileSize:     size,
		SHA256:       hash,
		Dependencies: append([]PackageRef(nil), pkg.Dependencies...),
	}
}

func packageZIP(t *testing.T, ref PackageRef, dependencies []PackageRef, entries ...zipEntry) []byte {
	t.Helper()
	manifestDependencies := make([]string, len(dependencies))
	for i, dependency := range dependencies {
		manifestDependencies[i] = packageVersionFullName(dependency)
	}
	manifest, err := json.Marshal(testManifest{
		Name:         ref.Name,
		Namespace:    ref.Namespace,
		FullName:     packageFullName(ref.Namespace, ref.Name),
		Version:      ref.Version,
		Dependencies: manifestDependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	return makeZIP(t, append([]zipEntry{{name: "manifest.json", body: string(manifest)}}, entries...)...)
}

func makeZIP(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func packageZIPWithRawEntry(t *testing.T, ref PackageRef, uncompressedSize uint64) []byte {
	t.Helper()
	manifest := packageZIP(t, ref, nil)
	reader, err := zip.NewReader(bytes.NewReader(manifest), int64(len(manifest)))
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, file := range reader.File {
		source, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(source)
		source.Close()
		if err != nil {
			t.Fatal(err)
		}
		header := file.FileHeader
		target, err := writer.CreateHeader(&header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := target.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	header := &zip.FileHeader{
		Name:               "Huge.dll",
		Method:             zip.Store,
		CRC32:              crc32.ChecksumIEEE(nil),
		CompressedSize64:   0,
		UncompressedSize64: uncompressedSize,
	}
	header.SetMode(0o644)
	if _, err := writer.CreateRaw(header); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func corruptZIPEntry(t *testing.T, body []byte, name string) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		offset, err := file.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		corrupt := append([]byte(nil), body...)
		corrupt[offset] ^= 0xff
		return corrupt
	}
	t.Fatalf("ZIP entry %q not found", name)
	return nil
}

func writePackageZIP(t *testing.T, ref PackageRef, dependencies []PackageRef, entries ...zipEntry) string {
	t.Helper()
	archivePath := filepath.Join(t.TempDir(), packageVersionFullName(ref)+".zip")
	if err := os.WriteFile(archivePath, packageZIP(t, ref, dependencies, entries...), 0o600); err != nil {
		t.Fatal(err)
	}
	return archivePath
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory %s contains %v, want empty", path, entries)
	}
}
