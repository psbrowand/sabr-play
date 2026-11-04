package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pakRelease serves a release with one mod file (in the zip) and paks next to the manifest.
func pakRelease(t *testing.T, paks map[string]string) (*httptest.Server, map[string]int) {
	t.Helper()
	rel := newRelease(map[string]string{"Mods/SabrVoice/scripts/main.lua": "lua1"})
	t.Cleanup(rel.srv.Close)
	hits := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		hits[name]++
		switch name {
		case "manifest.json":
			z := rel.zipBytes()
			b := bundle{Schema: "sabr-modpack/2", Version: "1.0.0", Archive: &bundleFile{Path: "sabr-modpack.zip", SHA256: sum(z), Size: int64(len(z))},
				Files: []bundleFile{{Path: "Mods/SabrVoice/scripts/main.lua", SHA256: sum([]byte("lua1"))}}}
			for p, c := range paks {
				b.Paks = append(b.Paks, bundleFile{Path: p, SHA256: sum([]byte(c)), Size: int64(len(c))})
			}
			json.NewEncoder(w).Encode(b)
		case "sabr-modpack.zip":
			w.Write(rel.zipBytes())
		default:
			if c, ok := paks[name]; ok {
				w.Write([]byte(c))
				return
			}
			http.NotFound(w, r)
		}
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, hits
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPakInstalledInStashNotInMods(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	s, _ := pakRelease(t, map[string]string{"SABR.pak": "pak1"})
	a := newApp(options{server: s.URL, home: t.TempDir(), yes: true})
	buf := &bytes.Buffer{}
	a.out = buf
	if a.install() != 0 {
		t.Fatal(buf.String())
	}
	if !exists(filepath.Join(a.pakStash(), "SABR.pak")) {
		t.Fatal("pak should be in the stash")
	}
	if exists(filepath.Join(a.pakLive(), "SABR.pak")) {
		t.Fatal("after install a plain Steam start must still work: no pak under ~mods")
	}
}

func TestPlayAddsFlagAndGoesLiveAndRestoreEmptiesMods(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	log := filepath.Join(t.TempDir(), "launch.txt")
	t.Setenv("SABRPLAY_LAUNCH_LOG", log)
	s, _ := pakRelease(t, map[string]string{"SABR.pak": "pak1"})
	home := t.TempDir()
	a := newApp(options{server: s.URL, home: home, yes: true})
	a.out = &bytes.Buffer{}
	if a.install() != 0 {
		t.Fatal("install")
	}
	p := newApp(options{server: s.URL, home: home, yes: true})
	buf := &bytes.Buffer{}
	p.out = buf
	if p.play() != 0 {
		t.Fatal(buf.String())
	}
	got, _ := os.ReadFile(log)
	if !strings.Contains(string(got), "-nobattleye") || !strings.Contains(string(got), pakFlag) {
		t.Fatalf("launch args: %s", got)
	}
	if !exists(filepath.Join(p.pakLive(), "SABR.pak")) || exists(filepath.Join(p.pakStash(), "SABR.pak")) {
		t.Fatal("pak should be live while the game runs")
	}
	// what the watcher does when the game has closed
	p.restorePaks()
	if exists(p.pakLive()) || !exists(filepath.Join(p.pakStash(), "SABR.pak")) {
		t.Fatal("pak should be back in the stash and the empty ~mods gone")
	}
}

func TestNoPaksMeansNoFlagAndNoModsFolder(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	log := filepath.Join(t.TempDir(), "launch.txt")
	t.Setenv("SABRPLAY_LAUNCH_LOG", log)
	s, _ := pakRelease(t, nil)
	home := t.TempDir()
	a := newApp(options{server: s.URL, home: home, yes: true})
	a.out = &bytes.Buffer{}
	if a.install() != 0 {
		t.Fatal("install")
	}
	p := newApp(options{server: s.URL, home: home, yes: true})
	p.out = &bytes.Buffer{}
	if p.play() != 0 {
		t.Fatal("play")
	}
	got, _ := os.ReadFile(log)
	if strings.Contains(string(got), pakFlag) {
		t.Fatalf("players without paks must not get the slow flag: %s", got)
	}
	if exists(p.pakLive()) {
		t.Fatal("no ~mods folder expected")
	}
}

func TestStalePakFromACrashIsRecovered(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	t.Setenv("SABRPLAY_LAUNCH_LOG", filepath.Join(t.TempDir(), "launch.txt"))
	s, hits := pakRelease(t, map[string]string{"SABR.pak": "pak1"})
	home := t.TempDir()
	a := newApp(options{server: s.URL, home: home, yes: true})
	a.out = &bytes.Buffer{}
	if a.install() != 0 {
		t.Fatal("install")
	}
	if err := a.activatePaks([]string{"SABR.pak"}); err != nil { // the PC died with the pak live
		t.Fatal(err)
	}
	p := newApp(options{server: s.URL, home: home, yes: true, dryLaunch: true})
	p.out = &bytes.Buffer{}
	if p.play() != 0 {
		t.Fatal("play")
	}
	if hits["SABR.pak"] != 1 {
		t.Fatalf("a stale live pak must not be downloaded again: %v", hits)
	}
	if exists(filepath.Join(p.pakLive(), "SABR.pak")) {
		t.Fatal("dry launch must leave the pak in the stash")
	}
}

func TestPakChangedAndDropped(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	paks := map[string]string{"SABR.pak": "pak1", "Old.pak": "old"}
	s, hits := pakRelease(t, paks)
	home := t.TempDir()
	a := newApp(options{server: s.URL, home: home, yes: true})
	a.out = &bytes.Buffer{}
	if a.install() != 0 {
		t.Fatal("install")
	}
	paks["SABR.pak"] = "pak2"
	delete(paks, "Old.pak")
	p := newApp(options{server: s.URL, home: home, yes: true, dryLaunch: true})
	p.out = &bytes.Buffer{}
	if p.play() != 0 {
		t.Fatal("play")
	}
	if b, _ := os.ReadFile(filepath.Join(p.pakStash(), "SABR.pak")); string(b) != "pak2" || hits["SABR.pak"] != 2 {
		t.Fatalf("changed pak not updated: %q %v", b, hits)
	}
	if exists(filepath.Join(p.pakStash(), "Old.pak")) {
		t.Fatal("dropped pak should be gone")
	}
}

func TestTamperedPakIsNeverMounted(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	log := filepath.Join(t.TempDir(), "launch.txt")
	t.Setenv("SABRPLAY_LAUNCH_LOG", log)
	s, _ := pakRelease(t, map[string]string{"SABR.pak": "pak1"})
	home := t.TempDir()
	a := newApp(options{server: s.URL, home: home, yes: true})
	a.out = &bytes.Buffer{}
	if a.install() != 0 {
		t.Fatal("install")
	}
	os.WriteFile(filepath.Join(a.pakStash(), "SABR.pak"), []byte("evil"), 0o644)
	p := newApp(options{home: home, yes: true, noUpdate: true})
	p.out = &bytes.Buffer{}
	if p.play() != 0 {
		t.Fatal("play")
	}
	got, _ := os.ReadFile(log)
	if strings.Contains(string(got), pakFlag) || exists(filepath.Join(p.pakLive(), "SABR.pak")) {
		t.Fatalf("a pak that fails its hash must not go live: %s", got)
	}
}

func TestFallbackSourceDoesNotDeletePaks(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	s, _ := pakRelease(t, map[string]string{"SABR.pak": "pak1"})
	home := t.TempDir()
	a := newApp(options{server: s.URL, home: home, yes: true})
	a.out = &bytes.Buffer{}
	if a.install() != 0 {
		t.Fatal("install")
	}
	a.cur = newSource("fallback server", "http://hub.example")
	if _, err := a.syncPaks(&bundle{}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(a.pakStash(), "SABR.pak")) {
		t.Fatal("a LAN hub without paks must not delete them")
	}
}

func TestUninstallRemovesPaks(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	s, _ := pakRelease(t, map[string]string{"SABR.pak": "pak1"})
	home := t.TempDir()
	a := newApp(options{server: s.URL, home: home, yes: true})
	a.out = &bytes.Buffer{}
	if a.install() != 0 {
		t.Fatal("install")
	}
	stash, live := a.pakStash(), a.pakLive()
	a.activatePaks([]string{"SABR.pak"})
	if a.uninstall() != 0 {
		t.Fatal("uninstall")
	}
	if exists(stash) || exists(live) {
		t.Fatal("paks must be gone after uninstall")
	}
}

func TestUninstallKeepsOtherModsInMods(t *testing.T) {
	lib, _, _ := fakeScum(t)
	t.Setenv("SABRPLAY_STEAM_ROOTS", lib)
	s, _ := pakRelease(t, map[string]string{"SABR.pak": "pak1"})
	a := newApp(options{server: s.URL, home: t.TempDir(), yes: true})
	a.out = &bytes.Buffer{}
	if a.install() != 0 {
		t.Fatal("install")
	}
	live := a.pakLive()
	os.MkdirAll(live, 0o755)
	mine := filepath.Join(live, "SomeoneElses.pak")
	os.WriteFile(mine, []byte("x"), 0o644)
	if a.uninstall() != 0 {
		t.Fatal("uninstall")
	}
	if !exists(mine) {
		t.Fatal("a pak we did not install must stay")
	}
}

func TestValidPak(t *testing.T) {
	for _, g := range []string{"SABR_TestProps.pak", "x.PAK"} {
		if validPak(g) != nil {
			t.Errorf("%q should be fine", g)
		}
	}
	for _, b := range []string{"", "../x.pak", "a/b.pak", `a\b.pak`, "x.exe", "x.pak.exe", "C:x.pak"} {
		if validPak(b) == nil {
			t.Errorf("%q must be refused", b)
		}
	}
}
