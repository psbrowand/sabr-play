package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type fakeServer struct {
	files   map[string][]byte
	keep    map[string]bool
	version string
	srv     *httptest.Server
}

func newFake(files map[string]string) *fakeServer {
	f := &fakeServer{files: map[string][]byte{}, keep: map[string]bool{}, version: "v1"}
	for k, v := range files {
		f.files[k] = []byte(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/bundle.json", func(w http.ResponseWriter, r *http.Request) {
		b := bundle{Schema: "sabr-modpack/1", Version: f.version}
		for p, c := range f.files {
			b.Files = append(b.Files, bundleFile{Path: p, SHA256: sum(c), Size: int64(len(c)), Keep: f.keep[p]})
		}
		json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		c, ok := f.files[strings.TrimPrefix(r.URL.Path, "/files/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(c)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func fakeScum(t *testing.T) (string, string, string) {
	t.Helper()
	lib := t.TempDir()
	os.MkdirAll(filepath.Join(lib, "steamapps", "common", "SCUM", "SCUM", "Binaries", "Win64"), 0o755)
	os.WriteFile(filepath.Join(lib, "steamapps", "appmanifest_513710.acf"), []byte(`"AppState" {}`), 0o644)
	exe := filepath.Join(lib, "steamapps", "common", "SCUM", "SCUM", "Binaries", "Win64", "SCUM.exe")
	os.WriteFile(exe, []byte("exe"), 0o755)
	return lib, filepath.Join(filepath.Dir(exe)), exe
}

func TestValidPath(t *testing.T) {
	good := []string{"dwmapi.dll", "UE4SS.dll", "UE4SS-settings.ini", "Mods/SabrVoice/scripts/main.lua"}
	bad := []string{"", "../x", "Mods/../../x", "/etc/passwd", `Mods\x`, "C:/x", "SCUM.exe", "Mods//x", "foo/bar.dll", "BattlEye/x.dll"}
	for _, p := range good {
		if err := validPath(p); err != nil {
			t.Errorf("%q should be allowed: %v", p, err)
		}
	}
	for _, p := range bad {
		if validPath(p) == nil {
			t.Errorf("%q must be refused", p)
		}
	}
}

func TestSteamLibrariesAndFindScum(t *testing.T) {
	root := t.TempDir()
	lib2, win64, exe := fakeScum(t)
	os.MkdirAll(filepath.Join(root, "steamapps"), 0o755)
	vdf := `"libraryfolders" { "0" { "path" "` + strings.ReplaceAll(root, `\`, `\\`) + `" } "1" { "path" "` + strings.ReplaceAll(lib2, `\`, `\\`) + `" } }`
	os.WriteFile(filepath.Join(root, "steamapps", "libraryfolders.vdf"), []byte(vdf), 0o644)
	libs := steamLibraries([]string{root})
	if len(libs) != 2 {
		t.Fatalf("libs=%v", libs)
	}
	d, w, e, err := findScum("", []string{root})
	if err != nil || w != win64 || e != exe || !strings.HasSuffix(d, "SCUM") {
		t.Fatalf("findScum: %v %v %v %v", d, w, e, err)
	}
	if _, _, _, err := findScum("", []string{t.TempDir()}); err == nil {
		t.Fatal("expected not found")
	}
	if _, w2, _, err := findScum(filepath.Join(lib2, "steamapps", "common", "SCUM"), nil); err != nil || w2 != win64 {
		t.Fatalf("override: %v", err)
	}
}

func TestInstallUpdateRepairUninstall(t *testing.T) {
	_, win64, _ := fakeScum(t)
	fs := newFake(map[string]string{"dwmapi.dll": "proxy1", "UE4SS.dll": "ue4ss1", "Mods/SabrVoice/scripts/main.lua": "lua1", "Mods/SabrVoice/data/config.ini": "volume=0.9\n"})
	defer fs.srv.Close()
	fs.keep["Mods/SabrVoice/data/config.ini"] = true
	// the user already has a different dwmapi.dll (another mod): must be backed up and restored
	os.WriteFile(filepath.Join(win64, "dwmapi.dll"), []byte("theirs"), 0o644)
	home := t.TempDir()
	a := newApp(options{server: fs.srv.URL, home: home, yes: true})
	a.out = &bytes.Buffer{}
	a.o.scum = filepath.Dir(filepath.Dir(filepath.Dir(win64)))
	if code := a.install(); code != 0 {
		t.Fatal(a.out.(*bytes.Buffer).String())
	}
	read := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(win64, filepath.FromSlash(p)))
		return string(b)
	}
	if read("dwmapi.dll") != "proxy1" || read("Mods/SabrVoice/scripts/main.lua") != "lua1" || read("Mods/SabrVoice/data/config.ini") != "volume=0.9\n" {
		t.Fatal("files not installed")
	}
	// user edits their keep-file; an update must not overwrite it, but must update normal files
	os.WriteFile(filepath.Join(win64, "Mods", "SabrVoice", "data", "config.ini"), []byte("volume=0.3\n"), 0o644)
	fs.files["Mods/SabrVoice/scripts/main.lua"] = []byte("lua2")
	fs.files["Mods/SabrVoice/new.txt"] = []byte("n")
	delete(fs.files, "UE4SS.dll")
	fs.version = "v2"
	a2 := newApp(options{server: fs.srv.URL, home: home, yes: true, dryLaunch: true})
	a2.out = &bytes.Buffer{}
	t.Setenv("SABRPLAY_LAUNCH_LOG", filepath.Join(t.TempDir(), "launch.txt"))
	if code := a2.play(); code != 0 {
		t.Fatal(a2.out.(*bytes.Buffer).String())
	}
	if read("Mods/SabrVoice/scripts/main.lua") != "lua2" || read("Mods/SabrVoice/new.txt") != "n" {
		t.Fatal("update not applied")
	}
	if read("Mods/SabrVoice/data/config.ini") != "volume=0.3\n" {
		t.Fatal("user-owned config was overwritten")
	}
	if _, err := os.Stat(filepath.Join(win64, "UE4SS.dll")); !os.IsNotExist(err) {
		t.Fatal("file dropped by the server should be removed")
	}
	// something deleted behind our back (antivirus / Steam verify) is repaired even though the version is unchanged
	os.Remove(filepath.Join(win64, "Mods", "SabrVoice", "new.txt"))
	a3 := newApp(options{server: fs.srv.URL, home: home, yes: true, dryLaunch: true})
	a3.out = &bytes.Buffer{}
	a3.play()
	if read("Mods/SabrVoice/new.txt") != "n" {
		t.Fatal("repair failed")
	}
	// server down: play still starts the game (dry-launch prints the command)
	fs.srv.Close()
	a4 := newApp(options{server: fs.srv.URL, home: home, yes: true, dryLaunch: true})
	buf := &bytes.Buffer{}
	a4.out = buf
	if code := a4.play(); code != 0 || !strings.Contains(buf.String(), "Update check skipped") || !strings.Contains(buf.String(), "-nobattleye") {
		t.Fatalf("offline play: %s", buf.String())
	}
	// uninstall restores the user's own dwmapi.dll and removes ours
	a5 := newApp(options{home: home, yes: true})
	a5.out = &bytes.Buffer{}
	if code := a5.uninstall(); code != 0 {
		t.Fatal(a5.out.(*bytes.Buffer).String())
	}
	if read("dwmapi.dll") != "theirs" {
		t.Fatalf("original dwmapi.dll not restored: %q", read("dwmapi.dll"))
	}
	if _, err := os.Stat(filepath.Join(win64, "Mods")); !os.IsNotExist(err) {
		t.Fatal("Mods folder should be gone")
	}
	if _, err := os.Stat(filepath.Join(win64, "SCUM.exe")); err != nil {
		t.Fatal("the game itself must be untouched")
	}
}

func TestHashMismatchIsRejected(t *testing.T) {
	_, win64, _ := fakeScum(t)
	fs := newFake(map[string]string{"Mods/SabrVoice/x.lua": "good"})
	defer fs.srv.Close()
	// lie in the bundle: advertised hash does not match the served bytes
	mux := http.NewServeMux()
	mux.HandleFunc("/bundle.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bundle{Schema: "sabr-modpack/1", Version: "v", Files: []bundleFile{{Path: "Mods/SabrVoice/x.lua", SHA256: sum([]byte("other"))}}})
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("good")) })
	bad := httptest.NewServer(mux)
	defer bad.Close()
	a := newApp(options{server: bad.URL, home: t.TempDir(), yes: true, scum: filepath.Dir(filepath.Dir(filepath.Dir(win64)))})
	a.out = &bytes.Buffer{}
	if a.install() == 0 {
		t.Fatal("install must fail on a hash mismatch")
	}
	if _, err := os.Stat(filepath.Join(win64, "Mods", "SabrVoice", "x.lua")); err == nil {
		t.Fatal("bad file must not be written")
	}
}

func TestBundleWithEscapingPathIsRefused(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bundle.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bundle{Schema: "sabr-modpack/1", Version: "v", Files: []bundleFile{{Path: "Mods/../../../evil.dll", SHA256: "00"}}})
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	a := newApp(options{server: s.URL, home: t.TempDir()})
	if _, err := a.fetchBundle(); err == nil {
		t.Fatal("must refuse")
	}
}
