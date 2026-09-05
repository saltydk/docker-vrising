package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const thunderstoreAPIPath = "/api/experimental/package/"

func TestResolveLatestKindredUsesExactDeclaredDependencies(t *testing.T) {
	server := newThunderstoreServer(t, fixtureResponses(t))
	defer server.Close()

	graph, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit",
		Name:      "KindredCommands",
		Version:   "latest",
	})
	if err != nil {
		t.Fatal(err)
	}

	wantRoot := PackageRef{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8"}
	if graph.Root != wantRoot {
		t.Fatalf("root = %#v, want %#v", graph.Root, wantRoot)
	}
	assertPackageOrder(t, graph.Packages,
		PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"},
		PackageRef{Namespace: "deca", Name: "VampireCommandFramework", Version: "0.10.4"},
		wantRoot,
	)
	if got := server.requests(); containsRequest(got, thunderstoreAPIPath+"deca/VampireCommandFramework/") {
		t.Fatalf("resolver requested VCF latest endpoint: %v", got)
	}
	if got, want := server.requests(), []string{
		thunderstoreAPIPath + "odjit/KindredCommands/",
		thunderstoreAPIPath + "odjit/KindredCommands/2.5.8/",
		thunderstoreAPIPath + "BepInEx/BepInExPack_V_Rising/1.733.2/",
		thunderstoreAPIPath + "deca/VampireCommandFramework/0.10.4/",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestResolvePinnedKindredSkipsLatestEndpoint(t *testing.T) {
	server := newThunderstoreServer(t, fixtureResponses(t))
	defer server.Close()

	_, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit",
		Name:      "KindredCommands",
		Version:   "2.5.8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := server.requests(); containsRequest(got, thunderstoreAPIPath+"odjit/KindredCommands/") {
		t.Fatalf("resolver requested root latest endpoint: %v", got)
	}
}

func TestResolveDeduplicatesBepInEx(t *testing.T) {
	server := newThunderstoreServer(t, fixtureResponses(t))
	defer server.Close()

	graph, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Packages) != 3 {
		t.Fatalf("resolved %d packages, want 3", len(graph.Packages))
	}
	if got := countRequest(server.requests(), thunderstoreAPIPath+"BepInEx/BepInExPack_V_Rising/1.733.2/"); got != 1 {
		t.Fatalf("BepInEx exact metadata requests = %d, want 1", got)
	}
}

func TestResolveAcceptsMetadataWithoutFileSize(t *testing.T) {
	server := newThunderstoreServer(t, fixtureResponses(t))
	defer server.Close()

	graph, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range graph.Packages {
		if pkg.FileSize != 0 {
			t.Fatalf("%s FileSize = %d, want 0 when metadata omits file_size", pkg.FullName, pkg.FileSize)
		}
	}
}

func TestResolvePreservesReportedMetadataFileSize(t *testing.T) {
	responses := fixtureResponses(t)
	rootPath := thunderstoreAPIPath + "odjit/KindredCommands/2.5.8/"
	responses[rootPath] = changeMetadata(t, responses[rootPath], func(metadata map[string]any) {
		metadata["file_size"] = 1384283
	})
	server := newThunderstoreServer(t, responses)
	defer server.Close()

	graph, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := graph.Packages[2].FileSize; got != 1384283 {
		t.Fatalf("KindredCommands FileSize = %d, want 1384283", got)
	}
}

func TestResolveRejectsNegativeFileSize(t *testing.T) {
	responses := fixtureResponses(t)
	rootPath := thunderstoreAPIPath + "odjit/KindredCommands/2.5.8/"
	responses[rootPath] = changeMetadata(t, responses[rootPath], func(metadata map[string]any) {
		metadata["file_size"] = -1
	})
	server := newThunderstoreServer(t, responses)
	defer server.Close()

	_, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8",
	})
	if err == nil || !strings.Contains(err.Error(), "negative file size") {
		t.Fatalf("Resolve() error = %v, want negative file size", err)
	}
}

func TestResolveRejectsConflictingExactVersions(t *testing.T) {
	responses := fixtureResponses(t)
	responses[thunderstoreAPIPath+"odjit/KindredCommands/2.5.8/"] = changeMetadata(t, responses[thunderstoreAPIPath+"odjit/KindredCommands/2.5.8/"], func(metadata map[string]any) {
		metadata["dependencies"] = []string{
			"deca-VampireCommandFramework-0.10.4",
			"deca-VampireCommandFramework-0.11.0",
		}
	})
	server := newThunderstoreServer(t, responses)
	defer server.Close()

	_, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8",
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting versions") {
		t.Fatalf("Resolve() error = %v, want conflicting versions", err)
	}
}

func TestResolveRejectsDependencyCycle(t *testing.T) {
	responses := fixtureResponses(t)
	vcfPath := thunderstoreAPIPath + "deca/VampireCommandFramework/0.10.4/"
	responses[vcfPath] = changeMetadata(t, responses[vcfPath], func(metadata map[string]any) {
		metadata["dependencies"] = []string{"odjit-KindredCommands-2.5.8"}
	})
	server := newThunderstoreServer(t, responses)
	defer server.Close()

	_, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8",
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Resolve() error = %v, want dependency cycle", err)
	}
}

func TestResolveRejectsInactivePackage(t *testing.T) {
	responses := fixtureResponses(t)
	vcfPath := thunderstoreAPIPath + "deca/VampireCommandFramework/0.10.4/"
	responses[vcfPath] = changeMetadata(t, responses[vcfPath], func(metadata map[string]any) {
		metadata["is_active"] = false
	})
	server := newThunderstoreServer(t, responses)
	defer server.Close()

	_, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8",
	})
	if err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("Resolve() error = %v, want inactive package", err)
	}
}

func TestResolveRetriesTransientMetadataFailure(t *testing.T) {
	responses := fixtureResponses(t)
	rootPath := thunderstoreAPIPath + "odjit/KindredCommands/2.5.8/"
	server := newThunderstoreServer(t, responses)
	server.failures[rootPath] = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}
	defer server.Close()

	graph, err := (&Thunderstore{BaseURL: server.URL, Client: server.Client()}).Resolve(t.Context(), RootSelection{
		Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Packages) != 3 {
		t.Fatalf("resolved %d packages, want 3", len(graph.Packages))
	}
	if got := countRequest(server.requests(), rootPath); got != 3 {
		t.Fatalf("root metadata requests = %d, want 3", got)
	}
}

func TestPackageLockDigestIsStableAcrossResponseOrder(t *testing.T) {
	bepinex := LockedPackage{Ref: PackageRef{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2"}, FullName: "BepInEx-BepInExPack_V_Rising-1.733.2", DownloadURL: "https://thunderstore.io/package/download/BepInEx/BepInExPack_V_Rising/1.733.2/", FileSize: 33503624}
	vcf := LockedPackage{Ref: PackageRef{Namespace: "deca", Name: "VampireCommandFramework", Version: "0.10.4"}, FullName: "deca-VampireCommandFramework-0.10.4", DownloadURL: "https://thunderstore.io/package/download/deca/VampireCommandFramework/0.10.4/", FileSize: 45335, Dependencies: []PackageRef{bepinex.Ref}}
	kindred := LockedPackage{Ref: PackageRef{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8"}, FullName: "odjit-KindredCommands-2.5.8", DownloadURL: "https://thunderstore.io/package/download/odjit/KindredCommands/2.5.8/", FileSize: 1384283, Dependencies: []PackageRef{vcf.Ref, bepinex.Ref}}

	first := PackageLock{SchemaVersion: schemaVersion, Root: kindred.Ref, Packages: []LockedPackage{bepinex, vcf, kindred}}
	second := PackageLock{SchemaVersion: schemaVersion, Root: kindred.Ref, Packages: []LockedPackage{kindred, bepinex, vcf}}
	second.Packages[0].Dependencies = []PackageRef{bepinex.Ref, vcf.Ref}

	if got, want := PackageLockDigest(first), PackageLockDigest(second); got != want {
		t.Fatalf("PackageLockDigest() = %q, want stable digest %q", got, want)
	}
}

func TestParseDependencyRejectsMalformedReference(t *testing.T) {
	for _, dependency := range []string{
		"", "namespace-name", "namespace-name-1.2", "namespace-name-1.2.3.4",
		"namespace-name-v1.2.3", "namespace--1.2.3", "namespace-name-1.2.3-extra",
	} {
		t.Run(dependency, func(t *testing.T) {
			if _, err := parseDependency(dependency); err == nil {
				t.Fatalf("parseDependency(%q) succeeded", dependency)
			}
		})
	}
}

type thunderstoreServer struct {
	*httptest.Server
	mu        sync.Mutex
	paths     []string
	responses map[string][]byte
	failures  map[string][]int
}

func newThunderstoreServer(t *testing.T, responses map[string][]byte) *thunderstoreServer {
	t.Helper()
	server := &thunderstoreServer{responses: responses, failures: make(map[string][]int)}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.mu.Lock()
		server.paths = append(server.paths, r.URL.Path)
		if failures := server.failures[r.URL.Path]; len(failures) > 0 {
			status := failures[0]
			server.failures[r.URL.Path] = failures[1:]
			server.mu.Unlock()
			http.Error(w, http.StatusText(status), status)
			return
		}
		body, ok := server.responses[r.URL.Path]
		server.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	return server
}

func (s *thunderstoreServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func fixtureResponses(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		thunderstoreAPIPath + "odjit/KindredCommands/":                readFixture(t, "kindred-package.json"),
		thunderstoreAPIPath + "odjit/KindredCommands/2.5.8/":          readFixture(t, "kindred-2.5.8.json"),
		thunderstoreAPIPath + "deca/VampireCommandFramework/0.10.4/":  readFixture(t, "vcf-0.10.4.json"),
		thunderstoreAPIPath + "BepInEx/BepInExPack_V_Rising/1.733.2/": readFixture(t, "bepinex-1.733.2.json"),
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "thunderstore", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func changeMetadata(t *testing.T, source []byte, change func(map[string]any)) []byte {
	t.Helper()
	var metadata map[string]any
	if err := json.Unmarshal(source, &metadata); err != nil {
		t.Fatal(err)
	}
	change(metadata)
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertPackageOrder(t *testing.T, packages []ResolvedPackage, want ...PackageRef) {
	t.Helper()
	got := make([]PackageRef, len(packages))
	for i, pkg := range packages {
		got[i] = pkg.Ref
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("package order = %#v, want %#v", got, want)
	}
}

func containsRequest(paths []string, want string) bool {
	return countRequest(paths, want) > 0
}

func countRequest(paths []string, want string) int {
	count := 0
	for _, path := range paths {
		if path == want {
			count++
		}
	}
	return count
}
