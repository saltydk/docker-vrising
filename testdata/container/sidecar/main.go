package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
)

type fixtureFile struct {
	Name string
	Body string
}

type fixturePackage struct {
	Namespace    string
	Name         string
	Version      string
	Dependencies []string
	Files        []fixtureFile
}

var packages = []fixturePackage{
	{Namespace: "BepInEx", Name: "BepInExPack_V_Rising", Version: "1.733.2", Dependencies: []string{}, Files: []fixtureFile{
		{Name: "BepInExPack_V_Rising/.doorstop_version", Body: "6.0.0\n"},
		{Name: "BepInExPack_V_Rising/doorstop_config.ini", Body: "[UnityDoorstop]\n"},
		{Name: "BepInExPack_V_Rising/winhttp.dll", Body: "fixture proxy\n"},
		{Name: "BepInExPack_V_Rising/dotnet/runtime.dll", Body: "fixture runtime\n"},
		{Name: "BepInExPack_V_Rising/BepInEx/core/core.dll", Body: "fixture core\n"},
		{Name: "BepInExPack_V_Rising/BepInEx/patchers/patcher.dll", Body: "fixture patcher\n"},
		{Name: "BepInExPack_V_Rising/BepInEx/config/BepInEx.cfg", Body: "[Logging.Console]\nEnabled = true\n"},
	}},
	{Namespace: "deca", Name: "VampireCommandFramework", Version: "0.10.4", Dependencies: []string{"BepInEx-BepInExPack_V_Rising-1.733.2"}, Files: []fixtureFile{{Name: "VampireCommandFramework.dll", Body: "fixture vcf\n"}}},
	{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.8", Dependencies: []string{"BepInEx-BepInExPack_V_Rising-1.733.2", "deca-VampireCommandFramework-0.10.4"}, Files: []fixtureFile{{Name: "KindredCommands.dll", Body: "fixture kindred v1\n"}, {Name: "NetTopologySuite.dll", Body: "fixture topology\n"}}},
	{Namespace: "cheesasaurus", Name: "HookDOTS_API", Version: "1.1.1", Dependencies: []string{"BepInEx-BepInExPack_V_Rising-1.733.2"}, Files: []fixtureFile{{Name: "HookDOTS.API.dll", Body: "fixture hookdots\n"}}},
	{Namespace: "Team_GreenEye", Name: "Satisvampory", Version: "1.0.85", Dependencies: []string{"BepInEx-BepInExPack_V_Rising-1.733.2", "deca-VampireCommandFramework-0.10.4", "cheesasaurus-HookDOTS_API-1.1.1"}, Files: []fixtureFile{{Name: "plugins/Satisvampory.dll", Body: "fixture satisvampory v1\n"}}},
}

var updatedPackages = []fixturePackage{
	{Namespace: "odjit", Name: "KindredCommands", Version: "2.5.9", Dependencies: []string{"BepInEx-BepInExPack_V_Rising-1.733.2", "deca-VampireCommandFramework-0.10.4"}, Files: []fixtureFile{{Name: "KindredCommands.dll", Body: "fixture kindred v2\n"}, {Name: "NetTopologySuite.dll", Body: "fixture topology\n"}}},
	{Namespace: "Team_GreenEye", Name: "Satisvampory", Version: "1.0.86", Dependencies: []string{"BepInEx-BepInExPack_V_Rising-1.733.2", "deca-VampireCommandFramework-0.10.4", "cheesasaurus-HookDOTS_API-1.1.1"}, Files: []fixtureFile{{Name: "plugins/Satisvampory.dll", Body: "fixture satisvampory v2\n"}}},
}

func archive(pkg fixturePackage) []byte {
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	manifest, _ := zw.Create("manifest.json")
	_ = json.NewEncoder(manifest).Encode(map[string]any{"name": pkg.Name, "version_number": pkg.Version, "website_url": "", "description": "fixture", "dependencies": pkg.Dependencies})
	files := append([]fixtureFile(nil), pkg.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, fixture := range files {
		file, _ := zw.Create(fixture.Name)
		_, _ = file.Write([]byte(fixture.Body))
	}
	if pkg.Namespace == "BepInEx" {
		for i := 0; i < 64; i++ {
			name := fmt.Sprintf("BepInExPack_V_Rising/BepInEx/patchers/fixture-%02d.dll", i)
			file, _ := zw.Create(name)
			_, _ = fmt.Fprintf(file, "fixture patcher %02d\n", i)
		}
	}
	_ = zw.Close()
	return body.Bytes()
}

func main() {
	recordDir := os.Getenv("FIXTURE_RECORD_DIR")
	token := os.Getenv("FIXTURE_RUN_TOKEN")
	if len(os.Args) != 2 || os.Args[1] != "serve" || recordDir != "/fixture/records" || !validToken(token) {
		log.Fatal("invalid fixture sidecar invocation")
	}
	runDir := path.Join(recordDir, "runs", token)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path.Join(runDir, "sidecar.argv"), []byte(strings.Join(os.Args[1:], " ")+"\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	allPackages := append(append([]fixturePackage(nil), packages...), updatedPackages...)
	archives := make(map[string][]byte, len(allPackages))
	for _, pkg := range allPackages {
		archives[pkg.Namespace+"/"+pkg.Name+"/"+pkg.Version] = archive(pkg)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if recordDir != "" {
			file, err := os.OpenFile(path.Join(recordDir, "sidecar.requests"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = fmt.Fprintln(file, r.Method, r.URL.RequestURI())
				_ = file.Close()
			}
		}

		selectedRoots := []fixturePackage{packages[2], packages[4]}
		if _, err := os.Stat(path.Join(recordDir, "updated-packages")); err == nil {
			selectedRoots = updatedPackages
		}
		for _, pkg := range allPackages {
			key := pkg.Namespace + "/" + pkg.Name + "/" + pkg.Version
			body := archives[key]
			downloadURL := "https://thunderstore.io/package/download/" + key + "/"
			exactPath := "/api/experimental/package/" + key + "/"
			if r.URL.Path == exactPath {
				response := metadata(pkg, downloadURL, body)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
				return
			}
			if r.URL.Path == "/package/download/"+key+"/" {
				if _, err := os.Stat(path.Join(recordDir, "corrupt-downloads")); err == nil {
					body = []byte("corrupt fixture archive")
				}
				w.Header().Set("Content-Type", "application/zip")
				_, _ = w.Write(body)
				return
			}
		}
		for _, pkg := range selectedRoots {
			latestPath := "/api/experimental/package/" + pkg.Namespace + "/" + pkg.Name + "/"
			if r.URL.Path != latestPath {
				continue
			}
			key := pkg.Namespace + "/" + pkg.Name + "/" + pkg.Version
			response := map[string]any{
				"namespace": pkg.Namespace,
				"name":      pkg.Name,
				"full_name": pkg.Namespace + "-" + pkg.Name,
				"latest":    metadata(pkg, "https://thunderstore.io/package/download/"+key+"/", archives[key]),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}

		http.NotFound(w, r)
	})

	certificate, err := tls.LoadX509KeyPair("/fixture/tls/server.crt", "/fixture/tls/server.key")
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", ":443")
	if err != nil {
		log.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	readyPath := path.Join(recordDir, "sidecar-ready."+token)
	if err := os.WriteFile(readyPath, []byte(token+"\n"), 0o600); err != nil {
		_ = tlsListener.Close()
		log.Fatal(err)
	}
	log.Fatal(http.Serve(tlsListener, nil))
}

func validToken(token string) bool {
	if token == "" || len(token) > 128 {
		return false
	}
	for index, value := range token {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || index > 0 && (value == '_' || value == '.' || value == '-') {
			continue
		}
		return false
	}
	return true
}

func metadata(pkg fixturePackage, downloadURL string, body []byte) map[string]any {
	sum := sha256.Sum256(body)
	return map[string]any{
		"namespace": pkg.Namespace, "name": pkg.Name, "version_number": pkg.Version,
		"full_name":    pkg.Namespace + "-" + pkg.Name + "-" + pkg.Version,
		"dependencies": pkg.Dependencies, "download_url": downloadURL, "file_size": len(body),
		"is_active": true, "uuid4": "00000000-0000-0000-0000-000000000001",
		"date_created": "2026-09-05T00:00:00Z", "website_url": "", "description": "fixture",
		"icon": "", "downloads": 1, "sha256": hex.EncodeToString(sum[:]),
	}
}
