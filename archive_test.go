package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	cacheDir := tempCacheDir(t)

	archive, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	locked := archive.LockedPackage()
	archivePath := filepath.Join(cacheDir, "acme-ExamplePlugin-1.2.3.zip")
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

		archive, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
		if err != nil {
			t.Fatal(err)
		}
		defer archive.Close()
		locked := archive.LockedPackage()
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

		archive, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
		if err != nil {
			t.Fatal(err)
		}
		defer archive.Close()
		locked := archive.LockedPackage()
		if locked.FileSize != int64(len(body)) {
			t.Fatalf("FileSize = %d, want actual size %d", locked.FileSize, len(body))
		}
	})
}

func TestArchiveCacheReusesMatchingFile(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	cacheDir := tempCacheDir(t)
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

	archive, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), pkg, prior)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	locked := archive.LockedPackage()
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0 for matching cache", requests)
	}
	if !reflect.DeepEqual(locked, prior) {
		t.Fatalf("Fetch() lock = %#v, want prior lock %#v", locked, prior)
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

			_, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
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

	cacheDir := tempCacheDir(t)
	_, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), pkg, prior)
	if err == nil || !strings.Contains(err.Error(), "locked SHA-256") {
		t.Fatalf("Fetch() error = %v, want changed locked bytes rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "acme-ExamplePlugin-1.2.3.zip")); !os.IsNotExist(statErr) {
		t.Fatalf("cache publication stat error = %v, want not exist", statErr)
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

	_, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(t.Context(), pkg, prior)
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

	_, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
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
		Dir:    tempCacheDir(t),
		Client: server.Client(),
		Backoff: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}

	_, err := cache.Fetch(t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{})
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

func TestArchiveCacheRetriesTruncatedContentLength(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if requests < 3 {
			_, _ = w.Write(body[:len(body)-1])
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	var delays []time.Duration
	archive, err := (&ArchiveCache{
		Dir:    tempCacheDir(t),
		Client: server.Client(),
		Backoff: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}).Fetch(t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !reflect.DeepEqual(delays, want) {
		t.Fatalf("backoff delays = %v, want %v", delays, want)
	}
}

func TestArchiveCacheTruncationExhaustsThreeAttempts(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body[:len(body)-1])
	}))
	defer server.Close()
	var delays []time.Duration
	_, err := (&ArchiveCache{
		Dir:    tempCacheDir(t),
		Client: server.Client(),
		Backoff: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}).Fetch(t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{})
	if err == nil {
		t.Fatal("Fetch() accepted an exhausted truncated response")
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !reflect.DeepEqual(delays, want) {
		t.Fatalf("backoff delays = %v, want %v", delays, want)
	}
}

func TestArchiveCacheDoesNotRetryLocalValidationFailure(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	body = corruptZIPEntry(t, body, "ExamplePlugin.dll")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(body)
	}))
	defer server.Close()

	if _, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(
		t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{},
	); err == nil {
		t.Fatal("Fetch() accepted corrupt ZIP")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 for local validation failure", requests)
	}
}

func TestArchiveCacheDoesNotRetryPermanentClientError(t *testing.T) {
	requests := 0
	client := httpDoerFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("permanent client error")
	})
	pkg := resolvedArchivePackage("http://example.invalid/archive.zip", []byte("x"))

	if _, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: client, Backoff: noBackoff}).Fetch(
		t.Context(), pkg, LockedPackage{},
	); err == nil {
		t.Fatal("Fetch() accepted permanent client error")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 for permanent client error", requests)
	}
}

func TestArchiveCacheRetriesTransientTransportError(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	requests := 0
	client := httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if requests < 3 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		return server.Client().Do(request)
	})

	archive, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: client, Backoff: noBackoff}).Fetch(
		t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

func TestArchiveCacheDoesNotRetryPermissionNetOpError(t *testing.T) {
	requests := 0
	client := httpDoerFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, &net.OpError{Op: "open", Net: "tcp", Err: syscall.EACCES}
	})
	pkg := resolvedArchivePackage("http://example.invalid/archive.zip", []byte("x"))

	if _, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: client, Backoff: noBackoff}).Fetch(
		t.Context(), pkg, LockedPackage{},
	); err == nil {
		t.Fatal("Fetch() accepted permission-denied net.OpError")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 for permanent net.OpError", requests)
	}
}

func TestArchiveCacheDoesNotRetryPermanentBodyReadError(t *testing.T) {
	requests := 0
	client := httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Length": []string{"1"}},
			Body:          io.NopCloser(errorReader{err: errors.New("permanent body read error")}),
			ContentLength: 1,
			Request:       request,
		}, nil
	})
	pkg := resolvedArchivePackage("http://example.invalid/archive.zip", []byte("x"))

	if _, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: client, Backoff: noBackoff}).Fetch(
		t.Context(), pkg, LockedPackage{},
	); err == nil {
		t.Fatal("Fetch() accepted permanent body read error")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 for permanent body read error", requests)
	}
}

func TestArchiveCacheDoesNotRetryBodyLongerThanContentLength(t *testing.T) {
	requests := 0
	client := httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Length": []string{"1"}},
			Body:          io.NopCloser(strings.NewReader("xx")),
			ContentLength: 1,
			Request:       request,
		}, nil
	})
	pkg := resolvedArchivePackage("http://example.invalid/archive.zip", nil)

	if _, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: client, Backoff: noBackoff}).Fetch(
		t.Context(), pkg, LockedPackage{},
	); err == nil {
		t.Fatal("Fetch() accepted body longer than Content-Length")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 for excess body bytes", requests)
	}
}

func TestArchiveCacheRetriesPerAttemptTimeoutThenSucceeds(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			<-request.Context().Done()
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()

	archive, err := (&ArchiveCache{
		Dir:     tempCacheDir(t),
		Client:  server.Client(),
		Timeout: 25 * time.Millisecond,
		Backoff: noBackoff,
	}).Fetch(t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 after one attempt timeout", got)
	}
}

func TestArchiveCachePerAttemptTimeoutExhaustsThreeAttempts(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		<-request.Context().Done()
	}))
	defer server.Close()
	pkg := resolvedArchivePackage(server.URL, []byte("x"))

	_, err := (&ArchiveCache{
		Dir:     tempCacheDir(t),
		Client:  server.Client(),
		Timeout: 25 * time.Millisecond,
		Backoff: noBackoff,
	}).Fetch(t.Context(), pkg, LockedPackage{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch() error = %v, want attempt deadline", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3 exhausted attempt timeouts", got)
	}
}

func TestArchiveCacheParentCancellationStopsAfterOneAttempt(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		cancel()
		<-request.Context().Done()
	}))
	defer server.Close()
	pkg := resolvedArchivePackage(server.URL, []byte("x"))

	_, err := (&ArchiveCache{
		Dir:     tempCacheDir(t),
		Client:  server.Client(),
		Timeout: time.Second,
		Backoff: noBackoff,
	}).Fetch(parent, pkg, LockedPackage{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch() error = %v, want parent cancellation", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 after parent cancellation", got)
	}
}

func TestArchiveCacheDoesNotRetryContextCancellation(t *testing.T) {
	requests := 0
	client := httpDoerFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, context.Canceled
	})
	pkg := resolvedArchivePackage("http://example.invalid/archive.zip", []byte("x"))

	_, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: client, Backoff: noBackoff}).Fetch(
		t.Context(), pkg, LockedPackage{},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch() error = %v, want context cancellation", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 for context cancellation", requests)
	}
}

func TestArchiveCacheDoesNotRetryOtherHTTPStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, 600} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(status)
			}))
			defer server.Close()
			pkg := resolvedArchivePackage(server.URL, []byte("x"))

			if _, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client(), Backoff: noBackoff}).Fetch(
				t.Context(), pkg, LockedPackage{},
			); err == nil {
				t.Fatalf("Fetch() accepted HTTP status %d", status)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1 for HTTP status %d", requests, status)
			}
		})
	}
}

func TestArchiveCacheRejectsInsufficientSpace(t *testing.T) {
	cacheDir := tempCacheDir(t)
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

	_, err := (&ArchiveCache{Dir: cacheDir}).Fetch(t.Context(), pkg, LockedPackage{})
	if err == nil || !strings.Contains(err.Error(), "space") {
		t.Fatalf("Fetch() error = %v, want insufficient space", err)
	}
}

func TestArchiveCacheRejectsPackageIdentifiersOutsideFormatAlphabet(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(body)
	}))
	defer server.Close()
	tests := []PackageRef{
		{Namespace: "bad-namespace", Name: "ExamplePlugin", Version: "1.2.3"},
		{Namespace: "acme", Name: "bad.name", Version: "1.2.3"},
		{Namespace: "acme", Name: "nested/ExamplePlugin", Version: "1.2.3"},
	}
	for _, ref := range tests {
		t.Run(packageVersionFullName(ref), func(t *testing.T) {
			pkg := ResolvedPackage{
				Ref:         ref,
				FullName:    packageVersionFullName(ref),
				DownloadURL: server.URL,
				FileSize:    int64(len(body)),
				IsActive:    true,
			}

			if _, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{}); err == nil {
				t.Fatalf("Fetch() accepted package reference %#v", ref)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0 for invalid identifiers", requests)
	}
}

func TestArchiveCacheRejectsIdentifierSymlinkReadEscape(t *testing.T) {
	ref := PackageRef{Namespace: "acme", Name: "linked/ExamplePlugin", Version: "1.2.3"}
	body := packageZIP(t, ref, nil, zipEntry{name: "ExamplePlugin.dll", body: "external"})
	cacheDir := tempCacheDir(t)
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "ExamplePlugin-1.2.3.zip"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(cacheDir, "acme-linked")); err != nil {
		t.Fatal(err)
	}
	requests := 0
	pkg := ResolvedPackage{
		Ref:         ref,
		FullName:    packageVersionFullName(ref),
		DownloadURL: "http://127.0.0.1:1/archive.zip",
		FileSize:    int64(len(body)),
		IsActive:    true,
	}

	if _, err := (&ArchiveCache{Dir: cacheDir}).Fetch(t.Context(), pkg, LockedPackage{}); err == nil {
		t.Fatal("Fetch() reused an archive through an identifier-controlled symlink")
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
	}
}

func TestArchiveCacheRejectsIdentifierSymlinkWriteEscape(t *testing.T) {
	ref := PackageRef{Namespace: "acme", Name: "linked/ExamplePlugin", Version: "1.2.3"}
	body := packageZIP(t, ref, nil, zipEntry{name: "ExamplePlugin.dll", body: "download"})
	cacheDir := tempCacheDir(t)
	external := t.TempDir()
	for _, component := range []string{"acme-linked", ".acme-linked"} {
		if err := os.Symlink(external, filepath.Join(cacheDir, component)); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	pkg := ResolvedPackage{
		Ref:         ref,
		FullName:    packageVersionFullName(ref),
		DownloadURL: server.URL,
		FileSize:    int64(len(body)),
		IsActive:    true,
	}

	if _, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{}); err == nil {
		t.Fatal("Fetch() published an archive through identifier-controlled symlinks")
	}
	assertDirectoryEmpty(t, external)
}

func TestArchiveCacheRejectsSymlinkCacheRoot(t *testing.T) {
	external := t.TempDir()
	parent := t.TempDir()
	cacheDir := filepath.Join(parent, "cache")
	if err := os.Symlink(external, cacheDir); err != nil {
		t.Fatal(err)
	}

	if _, err := (&ArchiveCache{Dir: cacheDir}).Fetch(t.Context(), ResolvedPackage{
		Ref:         archiveTestRef,
		FullName:    packageVersionFullName(archiveTestRef),
		DownloadURL: "http://127.0.0.1:1/archive.zip",
		FileSize:    1,
		IsActive:    true,
	}, LockedPackage{}); err == nil {
		t.Fatal("Fetch() accepted a symlink cache root")
	}
	assertDirectoryEmpty(t, external)
}

func TestArchiveCacheRejectsPermissiveCacheRoot(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	cacheDir := tempCacheDir(t)
	if err := os.Chmod(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(body)
	}))
	defer server.Close()

	if _, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(
		t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{},
	); err == nil {
		t.Fatal("Fetch() accepted a cache root not restricted to mode 0700")
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
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
	cacheDir := tempCacheDir(t)

	_, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), pkg, LockedPackage{})
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

	_, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(
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
	cacheDir := tempCacheDir(t)

	_, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{})
	if err == nil {
		t.Fatal("Fetch() accepted a ZIP with a corrupt plugin entry")
	}
	assertDirectoryEmpty(t, cacheDir)
}

func TestValidatedArchiveKeepsFetchedInodeAcrossPathReplacement(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "verified"})
	substitute := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "substitute"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	cacheDir := tempCacheDir(t)

	archive, err := (&ArchiveCache{Dir: cacheDir, Client: server.Client()}).Fetch(
		t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	locked := archive.LockedPackage()
	wantHash := sha256.Sum256(body)
	if locked.FileSize != int64(len(body)) || locked.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("LockedPackage() = %#v, want fetched size and digest", locked)
	}

	cachePath := filepath.Join(cacheDir, "acme-ExamplePlugin-1.2.3.zip")
	if err := os.Rename(cachePath, cachePath+".verified"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, substitute, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if _, err := ExtractArchive(archive, destination, ExtractPlugin); err != nil {
		t.Fatal(err)
	}
	extracted, err := os.ReadFile(filepath.Join(destination, "ExamplePlugin.dll"))
	if err != nil {
		t.Fatal(err)
	}
	if string(extracted) != "verified" {
		t.Fatalf("extracted bytes = %q, want held verified inode bytes", extracted)
	}
}

func TestValidatedArchiveCloseEndsExtractionOwnership(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	archive, err := (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(
		t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want idempotent close", err)
	}

	if _, err := ExtractArchive(archive, t.TempDir(), ExtractPlugin); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("ExtractArchive() error = %v, want closed archive rejection", err)
	}
}

func TestArchiveCacheValidatesHeldDescriptorAfterNameSubstitution(t *testing.T) {
	valid := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin bytes"})
	corrupt := corruptZIPEntry(t, valid, "ExamplePlugin.dll")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(corrupt)
	}))
	defer server.Close()
	cacheDir := tempCacheDir(t)
	cache := &ArchiveCache{
		Dir:    cacheDir,
		Client: server.Client(),
		cacheFilesystemHook: func(stage archiveCacheHookStage, root int, temporaryName, _ string) error {
			if stage != archiveCacheBeforeValidation {
				return nil
			}
			return replaceArchiveName(root, temporaryName, valid)
		},
	}

	if archive, err := cache.Fetch(t.Context(), resolvedArchivePackage(server.URL, corrupt), LockedPackage{}); err == nil {
		archive.Close()
		t.Fatal("Fetch() validated substituted pathname bytes instead of the held descriptor")
	}
	assertPathDoesNotExist(t, filepath.Join(cacheDir, "acme-ExamplePlugin-1.2.3.zip"))
}

func TestArchiveCacheRejectsTemporarySubstitutionBeforePublication(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "verified"})
	substitute := packageZIP(t, archiveTestRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "substitute"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	cacheDir := tempCacheDir(t)
	cache := &ArchiveCache{
		Dir:    cacheDir,
		Client: server.Client(),
		cacheFilesystemHook: func(stage archiveCacheHookStage, root int, temporaryName, _ string) error {
			if stage != archiveCacheBeforePublication {
				return nil
			}
			return replaceArchiveName(root, temporaryName, substitute)
		},
	}

	if archive, err := cache.Fetch(t.Context(), resolvedArchivePackage(server.URL, body), LockedPackage{}); err == nil {
		archive.Close()
		t.Fatal("Fetch() published a pathname substituted after descriptor validation")
	}
	assertPathDoesNotExist(t, filepath.Join(cacheDir, "acme-ExamplePlugin-1.2.3.zip"))
}

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: "../escape.dll", body: "escape"},
	)
	destination := t.TempDir()

	_, err := fetchArchiveBytes(t, body, archiveTestRef, nil)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("ExtractArchive() error = %v, want traversal rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(destination), "escape.dll")); !os.IsNotExist(statErr) {
		t.Fatalf("outside file stat error = %v, want not exist", statErr)
	}
}

func TestExtractArchiveRejectsAbsolutePath(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: "/escape.dll", body: "escape"},
	)

	_, err := fetchArchiveBytes(t, body, archiveTestRef, nil)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("ExtractArchive() error = %v, want absolute path rejection", err)
	}
}

func TestExtractArchiveRejectsBackslashPath(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: `nested\escape.dll`, body: "escape"},
	)

	_, err := fetchArchiveBytes(t, body, archiveTestRef, nil)
	if err == nil || !strings.Contains(err.Error(), "backslash") {
		t.Fatalf("ExtractArchive() error = %v, want backslash rejection", err)
	}
}

func TestExtractArchiveRejectsSymlink(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: "link.dll", body: "target", mode: os.ModeSymlink | 0o777},
	)

	_, err := fetchArchiveBytes(t, body, archiveTestRef, nil)
	if err == nil || !strings.Contains(err.Error(), "regular file or directory") {
		t.Fatalf("ExtractArchive() error = %v, want symlink rejection", err)
	}
}

func TestExtractArchiveRejectsDuplicateDestination(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "first"},
		zipEntry{name: "ExamplePlugin.dll", body: "second"},
	)

	_, err := fetchArchiveBytes(t, body, archiveTestRef, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("ExtractArchive() error = %v, want duplicate destination rejection", err)
	}
}

func TestExtractArchiveRejectsManifestMismatch(t *testing.T) {
	wrongRef := PackageRef{Namespace: archiveTestRef.Namespace, Name: "WrongPlugin", Version: archiveTestRef.Version}
	body := packageZIP(t, wrongRef, nil, zipEntry{name: "ExamplePlugin.dll", body: "plugin"})
	destination := t.TempDir()

	_, err := fetchArchiveBytes(t, body, archiveTestRef, nil)
	if err == nil || !strings.Contains(err.Error(), "manifest identity") {
		t.Fatalf("ExtractArchive() error = %v, want manifest identity rejection", err)
	}
	assertDirectoryEmpty(t, destination)
}

func TestExtractArchiveRejectsSymlinkedDestinationComponent(t *testing.T) {
	bepRef := PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"}
	body := packageZIP(t, bepRef, nil,
		zipEntry{name: "BepInExPack_V_Rising/BepInEx/core/core.dll", body: "core"},
	)
	archive, err := fetchArchiveBytes(t, body, bepRef, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	destination := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(destination, "BepInEx")); err != nil {
		t.Fatal(err)
	}

	_, err = ExtractArchive(archive, destination, ExtractBepInEx)
	if err == nil {
		t.Fatal("ExtractArchive() succeeded through symlinked destination component")
	}
	assertDirectoryEmpty(t, external)
}

func TestExtractArchiveExtractsRootPluginDLLsAt0644(t *testing.T) {
	body := packageZIP(t, archiveTestRef, nil,
		zipEntry{name: "ExamplePlugin.dll", body: "plugin"},
		zipEntry{name: "Helper.DLL", body: "helper"},
		zipEntry{name: "README.md", body: "docs"},
		zipEntry{name: "nested/Hidden.dll", body: "hidden"},
	)
	archive, err := fetchArchiveBytes(t, body, archiveTestRef, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	destination := filepath.Join(t.TempDir(), "new", "extract")

	extracted, err := ExtractArchive(archive, destination, ExtractPlugin)
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
			body := packageZIP(t, bepRef, nil, tt.entries...)
			archive, err := fetchArchiveBytes(t, body, bepRef, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			destination := t.TempDir()

			_, err = ExtractArchive(archive, destination, ExtractBepInEx)
			if err == nil || !strings.Contains(err.Error(), "single BepInExPack_V_Rising wrapper") {
				t.Fatalf("ExtractArchive() error = %v, want wrapper rejection", err)
			}
			assertDirectoryEmpty(t, destination)
		})
	}
}

func TestExtractBepInExStripsWrapper(t *testing.T) {
	bepRef := PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"}
	body := packageZIP(t, bepRef, nil,
		zipEntry{name: "README.md", body: "package docs"},
		zipEntry{name: "BepInExPack_V_Rising/", mode: os.ModeDir | 0o755},
		zipEntry{name: "BepInExPack_V_Rising/winhttp.dll", body: "dll"},
		zipEntry{name: "BepInExPack_V_Rising/BepInEx/core/core.dll", body: "core"},
	)
	archive, err := fetchArchiveBytes(t, body, bepRef, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	destination := t.TempDir()

	extracted, err := ExtractArchive(archive, destination, ExtractBepInEx)
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

	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	archive := newValidatedArchive(file, LockedPackage{
		Ref:          archiveTestRef,
		FullName:     packageVersionFullName(archiveTestRef),
		FileSize:     int64(len(body)),
		SHA256:       hex.EncodeToString(digest[:]),
		Dependencies: []PackageRef{},
	})
	defer archive.Close()
	_, err = ExtractArchive(archive, destination, ExtractPlugin)
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

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (f httpDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func noBackoff(context.Context, time.Duration) error {
	return nil
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
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

func fetchArchiveBytes(t *testing.T, body []byte, ref PackageRef, dependencies []PackageRef) (*ValidatedArchive, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	return (&ArchiveCache{Dir: tempCacheDir(t), Client: server.Client()}).Fetch(t.Context(), ResolvedPackage{
		Ref:          ref,
		FullName:     packageVersionFullName(ref),
		DownloadURL:  server.URL,
		FileSize:     int64(len(body)),
		IsActive:     true,
		Dependencies: append([]PackageRef(nil), dependencies...),
	}, LockedPackage{})
}

func tempCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
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

func replaceArchiveName(root int, name string, replacement []byte) error {
	if err := unix.Renameat(root, name, root, name+".held"); err != nil {
		return err
	}
	fd, err := unix.Openat(root, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := file.Write(replacement); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("path %s stat error = %v, want not exist", path, err)
	}
}
