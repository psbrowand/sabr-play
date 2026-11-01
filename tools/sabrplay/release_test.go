package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeRelease serves what `gh release` serves: manifest.json + flat assets (the pack zip and the launcher exe) under one base URL.
type fakeRelease struct {
	srv      *httptest.Server
	files    map[string]string
	version  string
	launcher []byte
	hits     map[string]int
	tamper   bool // serve a zip whose bytes differ from the manifest's archive hash
}

func newRelease(files map[string]string) *fakeRelease {
	r := &fakeRelease{files: files, version: "1.0.0", launcher: []byte("launcher-v1"), hits: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/")
		r.hits[name]++
		switch name {
		case "manifest.json":
			z := r.zipBytes()
			b := bundle{Schema: "sabr-modpack/2", Version: r.version, Archive: &bundleFile{Path: "sabr-modpack.zip", SHA256: sum(z), Size: int64(len(z))},
				Launcher: &bundleFile{Path: "SabrPlay-Setup.exe", SHA256: sum(r.launcher), Size: int64(len(r.launcher))}}
			for p, c := range r.files {
				b.Files = append(b.Files, bundleFile{Path: p, SHA256: sum([]byte(c)), Size: int64(len(c)), Keep: p == "Mods/SabrVoice/data/config.ini"})
			}
			json.NewEncoder(w).Encode(b)
		case "sabr-modpack.zip":
			z := r.zipBytes()
			if r.tamper {
				z = append(z, 0)
			}
			w.Write(z)
		case "SabrPlay-Setup.exe":
			w.Write(r.launcher)
		default:
			http.NotFound(w, req)
		}
	})
	r.srv = httptest.NewServer(mux)
	return r
}

func (r *fakeRelease) zipBytes() []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var names []string
	for p := range r.files {
		names = append(names, p)
	}
	sort.Strings(names) // deterministic: the manifest and the served zip are built by separate calls
	for _, p := range names {
		w, _ := zw.Create(p)
		w.Write([]byte(r.files[p]))
	}
	zw.Close()
	return buf.Bytes()
}

func TestReleaseLayoutInstallAndUpdate(t *testing.T) {
	_, win64, _ := fakeScum(t)
	rel := newRelease(map[string]string{"dwmapi.dll": "proxy1", "UE4SS.dll": "ue4ss1", "Mods/SabrVoice/scripts/main.lua": "lua1", "Mods/SabrVoice/data/config.ini": "volume=0.9\n"})
	defer rel.srv.Close()
	home := t.TempDir()
	a := newApp(options{server: rel.srv.URL, home: home, yes: true, scum: filepath.Dir(filepath.Dir(filepath.Dir(win64)))})
	buf := &bytes.Buffer{}
	a.out = buf
	if code := a.install(); code != 0 {
		t.Fatal(buf.String())
	}
	read := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(win64, filepath.FromSlash(p)))
		return string(b)
	}
	if read("UE4SS.dll") != "ue4ss1" || read("Mods/SabrVoice/scripts/main.lua") != "lua1" || read("Mods/SabrVoice/data/config.ini") != "volume=0.9\n" {
		t.Fatal("files not installed from the archive")
	}
	if rel.hits["sabr-modpack.zip"] != 1 {
		t.Fatalf("the archive should be downloaded exactly once, hits=%v", rel.hits)
	}
	if _, err := os.Stat(filepath.Join(home, "cache")); !os.IsNotExist(err) {
		t.Fatal("the downloaded zip must be cleaned up")
	}
	// nothing changed: no archive download at all
	a2 := newApp(options{server: rel.srv.URL, home: home, yes: true, dryLaunch: true})
	a2.out = &bytes.Buffer{}
	t.Setenv("SABRPLAY_LAUNCH_LOG", filepath.Join(t.TempDir(), "launch.txt"))
	if a2.play() != 0 || rel.hits["sabr-modpack.zip"] != 1 {
		t.Fatalf("up-to-date play must not download the pack again: %v", rel.hits)
	}
	// new release: one file changed, the user's config is untouched, a file is dropped
	os.WriteFile(filepath.Join(win64, "Mods", "SabrVoice", "data", "config.ini"), []byte("volume=0.3\n"), 0o644)
	rel.files["Mods/SabrVoice/scripts/main.lua"] = "lua2"
	delete(rel.files, "UE4SS.dll")
	rel.version = "1.0.1"
	a3 := newApp(options{server: rel.srv.URL, home: home, yes: true, dryLaunch: true})
	b3 := &bytes.Buffer{}
	a3.out = b3
	if a3.play() != 0 || !strings.Contains(b3.String(), "1.0.0 -> 1.0.1") {
		t.Fatal(b3.String())
	}
	if read("Mods/SabrVoice/scripts/main.lua") != "lua2" || read("Mods/SabrVoice/data/config.ini") != "volume=0.3\n" {
		t.Fatal("update not applied / config overwritten")
	}
	if _, err := os.Stat(filepath.Join(win64, "UE4SS.dll")); !os.IsNotExist(err) {
		t.Fatal("dropped file should be removed")
	}
}

func TestReleaseTamperedArchiveIsRejected(t *testing.T) {
	_, win64, _ := fakeScum(t)
	rel := newRelease(map[string]string{"Mods/SabrVoice/scripts/main.lua": "lua1"})
	defer rel.srv.Close()
	rel.tamper = true
	a := newApp(options{server: rel.srv.URL, home: t.TempDir(), yes: true, scum: filepath.Dir(filepath.Dir(filepath.Dir(win64)))})
	a.out = &bytes.Buffer{}
	if a.install() == 0 {
		t.Fatal("install must fail when the archive does not match the manifest hash")
	}
	if _, err := os.Stat(filepath.Join(win64, "Mods")); err == nil {
		t.Fatal("nothing may be written")
	}
}

func TestReleaseArchiveWithWrongMemberHashIsRejected(t *testing.T) {
	_, win64, _ := fakeScum(t)
	rel := newRelease(map[string]string{"Mods/SabrVoice/scripts/main.lua": "lua1"})
	defer rel.srv.Close()
	// manifest is honest about the zip, but the file entry's hash differs from the zip member
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		z := rel.zipBytes()
		json.NewEncoder(w).Encode(bundle{Schema: "sabr-modpack/2", Version: "v", Archive: &bundleFile{Path: "sabr-modpack.zip", SHA256: sum(z)},
			Files: []bundleFile{{Path: "Mods/SabrVoice/scripts/main.lua", SHA256: sum([]byte("something else"))}}})
	})
	mux.HandleFunc("/sabr-modpack.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(rel.zipBytes()) })
	s := httptest.NewServer(mux)
	defer s.Close()
	a := newApp(options{server: s.URL, home: t.TempDir(), yes: true, scum: filepath.Dir(filepath.Dir(filepath.Dir(win64)))})
	a.out = &bytes.Buffer{}
	if a.install() == 0 {
		t.Fatal("must fail on a member hash mismatch")
	}
	if _, err := os.Stat(filepath.Join(win64, "Mods", "SabrVoice", "scripts", "main.lua")); err == nil {
		t.Fatal("bad file must not be written")
	}
}

func TestFallbackSourceUsedWhenPrimaryIsDown(t *testing.T) {
	_, win64, _ := fakeScum(t)
	lan := newFake(map[string]string{"Mods/SabrVoice/scripts/main.lua": "from-lan"})
	defer lan.srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler()) // primary answers 404 for everything
	defer dead.Close()
	home := t.TempDir()
	a := newApp(options{server: dead.URL, fallback: lan.srv.URL, home: home, yes: true, scum: filepath.Dir(filepath.Dir(filepath.Dir(win64)))})
	buf := &bytes.Buffer{}
	a.out = buf
	if code := a.install(); code != 0 {
		t.Fatal(buf.String())
	}
	if b, _ := os.ReadFile(filepath.Join(win64, "Mods", "SabrVoice", "scripts", "main.lua")); string(b) != "from-lan" {
		t.Fatal("fallback source was not used")
	}
	if !strings.Contains(buf.String(), "fallback server") {
		t.Fatal(buf.String())
	}
	// the fallback is remembered: a later play with no flags still finds it
	a2 := newApp(options{home: home, yes: true})
	got := a2.sources()
	if len(got) != 2 || got[1].base != strings.TrimRight(lan.srv.URL, "/") {
		t.Fatalf("saved fallback lost: %v", got)
	}
}

func TestDefaultSourceIsTheGitHubRelease(t *testing.T) {
	a := newApp(options{home: t.TempDir()})
	s := a.sources()
	if len(s) != 1 || s[0].base != "https://github.com/psbrowand/sabr-play/releases/latest/download" {
		t.Fatalf("unexpected default sources: %v", s)
	}
	for _, v := range []string{defaultSource, defaultRepoURL, defaultFallback} {
		if strings.Contains(v, "10.") || strings.Contains(v, "192.168.") || strings.Contains(v, "8080") {
			t.Fatalf("private address baked into the default: %q", v)
		}
	}
}

func TestLegacyStateServerBecomesFallback(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"server":"http://lan.example:1/mods","win64":"x","version":"old"}`), 0o644)
	a := newApp(options{home: home})
	s := a.sources()
	if len(s) != 2 || !strings.Contains(s[0].base, "github.com") || s[1].base != "http://lan.example:1/mods" {
		t.Fatalf("legacy server should only be the fallback: %v", s)
	}
}

func TestValidAsset(t *testing.T) {
	for _, g := range []string{"sabr-modpack.zip", "SabrPlay-Setup.exe"} {
		if validAsset(g) != nil {
			t.Errorf("%q should be fine", g)
		}
	}
	for _, b := range []string{"", "../x.zip", "a/b.zip", `a\b.zip`, "C:x", "..", "."} {
		if validAsset(b) == nil {
			t.Errorf("%q must be refused", b)
		}
	}
}
