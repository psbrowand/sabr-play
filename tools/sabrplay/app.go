package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const steamAppID = "513710"

type bundleFile struct {
	Path   string `json:"path"` // relative to <SCUM>\SCUM\Binaries\Win64, forward slashes
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Keep   bool   `json:"keep,omitempty"` // user-owned after first install (config files): written only when absent
}
type bundle struct {
	Schema   string       `json:"schema"`
	Version  string       `json:"version"`
	Game     string       `json:"game,omitempty"`
	Notes    string       `json:"notes,omitempty"`
	Files    []bundleFile `json:"files"`
	Launcher *bundleFile  `json:"launcher,omitempty"` // LAN layout: URL path under /files/; release layout: asset name next to manifest.json
	Archive  *bundleFile  `json:"archive,omitempty"`  // release layout (schema 2): one zip with every file of the pack, path = asset name
	Paks     []bundleFile `json:"paks,omitempty"`     // content paks, path = asset name next to the manifest (see paks.go); not in the zip
}

// A mod source is a base URL that serves <base>/manifest.json (or the older <base>/bundle.json) and either
//   - release layout: manifest.archive names a zip asset next to the manifest (GitHub Releases), or
//   - LAN layout: every file at <base>/files/<path> (the old private mod hub).
type source struct {
	name string
	base string
}

func newSource(name, base string) *source {
	return &source{name: name, base: strings.TrimRight(base, "/")}
}

func (s *source) String() string { return s.name + " (" + s.base + ")" }

type state struct {
	Server     string            `json:"server,omitempty"` // legacy (pre-release installs): the private mod hub, now migrated to Fallback
	Source     string            `json:"source,omitempty"` // explicit mod-source override (--server); empty = the public GitHub release
	Fallback   string            `json:"fallback"`         // optional second source tried when the first one cannot be reached (LAN hub)
	ScumDir    string            `json:"scum_dir"`
	Win64      string            `json:"win64"`
	ScumExe    string            `json:"scum_exe"`
	Version    string            `json:"version"`
	Installed  string            `json:"installed"`
	Updated    string            `json:"updated"`
	Files      map[string]string `json:"files"`   // path -> sha256 of what we installed
	Backups    map[string]string `json:"backups"` // path -> backup file name for pre-existing files we replaced
	Existed    []string          `json:"existed"` // kept (Keep) files we did not create
	Paks       map[string]string `json:"paks,omitempty"` // pak name -> sha256 of what we installed
	Shortcuts  []string          `json:"shortcuts"`
	LaunchArgs []string          `json:"launch_args"`
}

type app struct {
	o    options
	home string
	st   state
	http *http.Client
	out  io.Writer
	cur  *source     // the source the current bundle came from
	zr   *zip.Reader // open archive of the current sync (release layout)
}

func newApp(o options) *app {
	a := &app{o: o, out: os.Stdout, http: &http.Client{Timeout: 10 * time.Minute}}
	a.home = o.home
	if a.home == "" {
		a.home = defaultHome()
	}
	a.loadState()
	return a
}

func (a *app) say(f string, v ...interface{}) { fmt.Fprintf(a.out, f+"\n", v...) }

func (a *app) statePath() string { return filepath.Join(a.home, "state.json") }

func (a *app) loadState() {
	a.st = state{Files: map[string]string{}, Backups: map[string]string{}, LaunchArgs: []string{"-nobattleye"}}
	b, err := os.ReadFile(a.statePath())
	if err != nil {
		return
	}
	json.Unmarshal(b, &a.st)
	if a.st.Server != "" {
		// installs made before the public release stored the private mod hub as "server": it is now only the fallback
		if a.st.Fallback == "" {
			a.st.Fallback = a.st.Server
		}
		a.st.Server = ""
	}
	if a.st.Files == nil {
		a.st.Files = map[string]string{}
	}
	if a.st.Backups == nil {
		a.st.Backups = map[string]string{}
	}
	if len(a.st.LaunchArgs) == 0 {
		a.st.LaunchArgs = []string{"-nobattleye"}
	}
}

func (a *app) saveState() error {
	if err := os.MkdirAll(a.home, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(a.st, "", "  ")
	tmp := a.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.statePath())
}

func (a *app) installed() bool { return a.st.Win64 != "" && a.st.Version != "" }

// sources lists where to look for the mod pack, in order: the public GitHub release (or --server / the saved override),
// then the optional fallback (--fallback, the saved one, or the build-time default; none in the public build).
func (a *app) sources() []*source {
	var l []*source
	switch {
	case a.o.server != "":
		l = append(l, newSource("server", a.o.server))
	case a.st.Source != "":
		l = append(l, newSource("server", a.st.Source))
	default:
		l = append(l, newSource("GitHub release", defaultSource))
	}
	fb := a.o.fallback
	if fb == "" {
		fb = a.st.Fallback
	}
	if fb == "" {
		fb = defaultFallback
	}
	if fb != "" && !strings.EqualFold(strings.TrimRight(fb, "/"), l[0].base) {
		l = append(l, newSource("fallback server", fb))
	}
	return l
}

// ---------------------------------------------------------------------------------------------- finding SCUM
var vdfPath = regexp.MustCompile(`"path"\s+"([^"]+)"`)

func steamLibraries(roots []string) []string {
	seen := map[string]bool{}
	var libs []string
	add := func(p string) {
		p = filepath.Clean(strings.ReplaceAll(p, `\\`, `\`))
		if p != "." && !seen[p] {
			seen[p] = true
			libs = append(libs, p)
		}
	}
	for _, r := range roots {
		if r == "" {
			continue
		}
		add(r)
		b, err := os.ReadFile(filepath.Join(r, "steamapps", "libraryfolders.vdf"))
		if err != nil {
			continue
		}
		for _, m := range vdfPath.FindAllStringSubmatch(string(b), -1) {
			add(m[1])
		}
	}
	return libs
}

// findScum returns (scumDir, win64, scumExe): <lib>\steamapps\common\SCUM and its SCUM\Binaries\Win64\SCUM.exe
func findScum(override string, roots []string) (string, string, string, error) {
	cands := []string{}
	if override != "" {
		cands = append(cands, override)
	}
	for _, l := range steamLibraries(roots) {
		if _, err := os.Stat(filepath.Join(l, "steamapps", "appmanifest_"+steamAppID+".acf")); err == nil {
			cands = append(cands, filepath.Join(l, "steamapps", "common", "SCUM"))
		}
	}
	for _, c := range cands {
		for _, base := range []string{c, filepath.Dir(c)} {
			w := filepath.Join(base, "SCUM", "Binaries", "Win64")
			if st, err := os.Stat(w); err == nil && st.IsDir() {
				for _, name := range []string{"SCUM.exe", "SCUM-Win64-Shipping.exe"} {
					if _, err := os.Stat(filepath.Join(w, name)); err == nil {
						return base, w, filepath.Join(w, name), nil
					}
				}
			}
		}
	}
	return "", "", "", errors.New("SCUM was not found. Install it from Steam first, or run: SabrPlay install --scum \"D:\\SteamLibrary\\steamapps\\common\\SCUM\"")
}

// ---------------------------------------------------------------------------------------------- bundle
func (a *app) fetchBundle() (*bundle, error) {
	var errs []string
	for _, src := range a.sources() {
		b, err := a.fetchFrom(src)
		if err == nil {
			a.cur = src
			return b, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", src.name, err))
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

func (a *app) fetchFrom(src *source) (*bundle, error) {
	c := &http.Client{Timeout: 15 * time.Second}
	var lastErr error
	for _, name := range []string{"manifest.json", "bundle.json"} {
		u := src.base + "/" + name
		resp, err := c.Get(u)
		if err != nil {
			lastErr = fmt.Errorf("cannot reach %s: %v", u, err)
			continue
		}
		var b bundle
		if resp.StatusCode != 200 {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d for %s", resp.StatusCode, u)
			continue
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&b)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bad %s: %v", name, err)
		}
		if b.Schema != "sabr-modpack/1" && b.Schema != "sabr-modpack/2" {
			return nil, fmt.Errorf("unsupported manifest schema %q", b.Schema)
		}
		if b.Archive != nil {
			if err := validAsset(b.Archive.Path); err != nil || b.Archive.SHA256 == "" {
				return nil, fmt.Errorf("bad archive entry in %s", name)
			}
			b.Archive.SHA256 = strings.ToLower(b.Archive.SHA256)
		} else if b.Schema == "sabr-modpack/2" {
			return nil, fmt.Errorf("%s has no archive", name)
		}
		for i := range b.Files {
			if err := validPath(b.Files[i].Path); err != nil {
				return nil, err
			}
			b.Files[i].SHA256 = strings.ToLower(b.Files[i].SHA256)
		}
		for i := range b.Paks {
			if err := validPak(b.Paks[i].Path); err != nil || b.Paks[i].SHA256 == "" {
				return nil, fmt.Errorf("bad pak entry in %s: %q", name, b.Paks[i].Path)
			}
			b.Paks[i].SHA256 = strings.ToLower(b.Paks[i].SHA256)
		}
		return &b, nil
	}
	return nil, lastErr
}

// release asset names: a single plain file name
func validAsset(n string) error {
	if n == "" || strings.ContainsAny(n, `/\:`) || n == "." || n == ".." {
		return fmt.Errorf("asset name not allowed: %q", n)
	}
	return nil
}

var topLevel = map[string]bool{"dwmapi.dll": true, "UE4SS.dll": true, "UE4SS-settings.ini": true}

// only the three UE4SS root files and anything under Mods/ may be written; never absolute, never ..
func validPath(p string) error {
	if p == "" || strings.Contains(p, `\`) || strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return fmt.Errorf("bundle path not allowed: %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("bundle path not allowed: %q", p)
		}
	}
	if topLevel[p] || strings.HasPrefix(p, "Mods/") {
		return nil
	}
	return fmt.Errorf("bundle path outside the allowed set: %q", p)
}

func fileSHA(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fetchTo downloads url into dest (via dest+".sabrpart"), refusing anything whose sha256 is not `want`.
func (a *app) fetchTo(u, want, dest string) error {
	var err error
	for try := 0; try < 3; try++ {
		var retry bool
		if retry, err = a.fetchOnce(u, want, dest); err == nil || !retry {
			return err
		}
		time.Sleep(time.Duration(try+1) * 700 * time.Millisecond)
	}
	return err
}

func (a *app) fetchOnce(u, want, dest string) (retry bool, err error) {
	resp, err := a.http.Get(u)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return resp.StatusCode >= 500, fmt.Errorf("HTTP %d for %s", resp.StatusCode, path.Base(u))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	tmp := dest + ".sabrpart"
	f, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return true, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return false, fmt.Errorf("hash mismatch for %s (got %s…)", path.Base(u), got[:12])
	}
	os.Remove(dest) // Windows: rename over an existing file needs the target gone
	return false, os.Rename(tmp, dest)
}

// download fetches one pack file: out of the open archive (release layout) or from <base>/files/<rel> (LAN layout).
func (a *app) download(rel, want, dest string) error {
	if a.zr != nil {
		return a.extract(rel, want, dest)
	}
	return a.fetchTo(a.cur.base+"/files/"+path.Clean(rel), want, dest)
}

func (a *app) extract(rel, want, dest string) error {
	var zf *zip.File
	for _, f := range a.zr.File {
		if f.Name == rel {
			zf = f
			break
		}
	}
	if zf == nil {
		return fmt.Errorf("%s is not in the downloaded archive", rel)
	}
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".sabrpart"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(rc, 1<<30))
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("hash mismatch for %s (got %s…)", rel, got[:12])
	}
	os.Remove(dest)
	return os.Rename(tmp, dest)
}

// openArchive downloads the pack zip of a release manifest (verified against the manifest) and opens it; closeArchive cleans up.
func (a *app) openArchive(b *bundle) (func(), error) {
	a.zr = nil
	if b.Archive == nil {
		return func() {}, nil
	}
	zp := filepath.Join(a.home, "cache", b.Archive.Path)
	if err := a.fetchTo(a.cur.base+"/"+b.Archive.Path, b.Archive.SHA256, zp); err != nil {
		return func() {}, fmt.Errorf("cannot download %s: %v", b.Archive.Path, err)
	}
	r, err := zip.OpenReader(zp)
	if err != nil {
		os.Remove(zp)
		return func() {}, fmt.Errorf("bad %s: %v", b.Archive.Path, err)
	}
	a.zr = &r.Reader
	return func() { a.zr = nil; r.Close(); os.Remove(zp); os.Remove(filepath.Dir(zp)) }, nil
}

// sync makes <win64> match the bundle. Returns (downloaded, removed, error).
func (a *app) sync(b *bundle) (int, int, error) {
	w := a.st.Win64
	type job struct{ f bundleFile }
	var todo []bundleFile
	want := map[string]bool{}
	for _, f := range b.Files {
		want[f.Path] = true
		dest := filepath.Join(w, filepath.FromSlash(f.Path))
		cur := fileSHA(dest)
		if f.Keep {
			if cur == "" {
				todo = append(todo, f)
			} else if _, ours := a.st.Files[f.Path]; !ours {
				a.st.Existed = appendUnique(a.st.Existed, f.Path)
			}
			continue
		}
		if cur != f.SHA256 {
			todo = append(todo, f)
		}
	}
	// pre-existing files we are about to replace (a UE4SS the user already had, ...): back up once
	for _, f := range todo {
		dest := filepath.Join(w, filepath.FromSlash(f.Path))
		if _, ours := a.st.Files[f.Path]; !ours {
			if _, err := os.Stat(dest); err == nil {
				bk := filepath.Join(a.home, "backup", filepath.FromSlash(f.Path))
				os.MkdirAll(filepath.Dir(bk), 0o755)
				if err := copyFile(dest, bk); err != nil {
					return 0, 0, fmt.Errorf("cannot back up %s: %v", f.Path, err)
				}
				a.st.Backups[f.Path] = bk
			}
		}
	}
	if len(todo) > 0 {
		closeArchive, err := a.openArchive(b)
		if err != nil {
			return 0, 0, err
		}
		defer closeArchive()
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	ch := make(chan bundleFile)
	n := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				dest := filepath.Join(w, filepath.FromSlash(f.Path))
				err := a.download(f.Path, f.SHA256, dest)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("%s: %v", f.Path, err)
				}
				if err == nil {
					a.st.Files[f.Path] = f.SHA256
					n++
				}
				mu.Unlock()
			}
		}()
	}
	for _, f := range todo {
		ch <- f
	}
	close(ch)
	wg.Wait()
	for _, f := range b.Files { // record unchanged files we own (so uninstall removes them)
		if !f.Keep && fileSHA(filepath.Join(w, filepath.FromSlash(f.Path))) == f.SHA256 {
			a.st.Files[f.Path] = f.SHA256
		}
	}
	// files we installed earlier that the server no longer ships
	removed := 0
	var gone []string
	for p := range a.st.Files {
		if !want[p] {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)
	for _, p := range gone {
		if err := os.Remove(filepath.Join(w, filepath.FromSlash(p))); err == nil || os.IsNotExist(err) {
			delete(a.st.Files, p)
			removed++
		}
	}
	return n, removed, firstErr
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---------------------------------------------------------------------------------------------- self update
// A running exe can be renamed on Windows (and Linux) but not overwritten: rename it away, drop the new one in place, re-run.
func (a *app) selfUpdate(b *bundle) bool {
	if b.Launcher == nil || b.Launcher.SHA256 == "" {
		return false
	}
	exe, err := os.Executable()
	if err != nil || filepath.Dir(exe) != a.home {
		return false // only the installed copy updates itself
	}
	if fileSHA(exe) == strings.ToLower(b.Launcher.SHA256) {
		return false
	}
	nw := exe + ".new"
	lu := a.cur.base + "/files/" + path.Clean(b.Launcher.Path)
	if b.Archive != nil { // release layout: the launcher is an asset next to the manifest
		if validAsset(b.Launcher.Path) != nil {
			return false
		}
		lu = a.cur.base + "/" + b.Launcher.Path
	}
	if err := a.fetchTo(lu, strings.ToLower(b.Launcher.SHA256), nw); err != nil {
		a.say("  (launcher update skipped: %v)", err)
		return false
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(nw)
		return false
	}
	if err := os.Rename(nw, exe); err != nil {
		os.Rename(old, exe)
		return false
	}
	a.say("Launcher updated to the latest version; restarting it...")
	return true
}

// ---------------------------------------------------------------------------------------------- commands
func (a *app) install() int {
	a.say("SABR installer %s", version)
	a.say(warning)
	scumDir, win64, scumExe, err := findScum(a.o.scum, steamRoots())
	if err != nil {
		a.say("ERROR: %v", err)
		return 1
	}
	a.say("SCUM found: %s", scumDir)
	if isRunning("SCUM.exe") || isRunning("SCUM-Win64-Shipping.exe") {
		a.say("ERROR: SCUM is running. Close the game and run the installer again.")
		return 1
	}
	a.restorePaks()
	a.st.ScumDir, a.st.Win64, a.st.ScumExe = scumDir, win64, scumExe
	if a.o.server != "" {
		a.st.Source = a.o.server
	}
	if a.o.fallback != "" {
		a.st.Fallback = a.o.fallback
	}
	b, err := a.fetchBundle()
	if err != nil {
		a.say("ERROR: %v\n  The mods are downloaded from %s - check your internet connection (or whether github.com is blocked) and try again.", err, defaultRepoURL)
		return 1
	}
	a.say("Mod source: %s", a.cur)
	a.say("Installing mod pack %s (%d files)...", b.Version, len(b.Files))
	n, rm, err := a.sync(b)
	if err != nil {
		a.saveState()
		a.say("ERROR while installing: %v", err)
		return 1
	}
	a.st.Version = b.Version
	now := time.Now().Format(time.RFC3339)
	if a.st.Installed == "" {
		a.st.Installed = now
	}
	a.st.Updated = now
	a.say("  %d files written, %d removed, rest already up to date.", n, rm)
	if _, err := a.syncPaks(b); err != nil {
		a.saveState()
		a.say("ERROR while installing: %v", err)
		return 1
	}
	// keep a copy of ourselves in the app folder: the shortcuts point there, so the Downloads copy can be deleted
	self, _ := os.Executable()
	target := filepath.Join(a.home, exeName())
	if err := os.MkdirAll(a.home, 0o755); err == nil && filepath.Clean(self) != filepath.Clean(target) {
		os.Remove(target)
		if err := copyFile(self, target); err != nil {
			a.say("WARNING: could not copy the launcher to %s: %v", target, err)
			target = self
		} else {
			os.Chmod(target, 0o755)
		}
	}
	os.WriteFile(filepath.Join(a.home, "Uninstall-SABR.cmd"), []byte("@echo off\r\n\""+target+"\" uninstall\r\n"), 0o644)
	sc, err := a.makeShortcuts(target)
	if err != nil {
		a.say("WARNING: could not create shortcuts: %v (start the game with: \"%s\" play)", err, target)
	}
	a.st.Shortcuts = sc
	if err := a.saveState(); err != nil {
		a.say("ERROR: cannot save state: %v", err)
		return 1
	}
	a.say("\nDone. Use the \"Play SABR\" shortcut on your desktop to start the game (it updates the mods first).")
	a.say("In SCUM: Multi Play -> Connect to IP -> use the server address (and password) from the SABR Discord.")
	return 0
}

func (a *app) play() int {
	if !a.installed() {
		a.say("Not installed yet - running the installer first.")
		return a.install()
	}
	a.say("Play SABR %s%s", version, warning)
	if !gameRunning() {
		a.restorePaks() // left behind if the PC went down while the game was running
	}
	if !a.o.noUpdate {
		b, err := a.fetchBundle()
		switch {
		case err != nil:
			a.say("Update check skipped: %v\nStarting the game with what is installed.", err)
		default:
			if a.selfUpdate(b) {
				return reexec()
			}
			if b.Version != a.st.Version || a.needsRepair(b) {
				a.say("Updating mods %s -> %s ...", orDash(a.st.Version), b.Version)
				n, rm, err := a.sync(b)
				if err != nil {
					a.say("WARNING: update incomplete: %v (is the game already running?)", err)
				} else {
					a.say("  %d files updated, %d removed.", n, rm)
					a.st.Version = b.Version
					a.st.Updated = time.Now().Format(time.RFC3339)
				}
				a.saveState()
			} else {
				a.say("Mods are up to date (%s).", a.st.Version)
			}
			if n, err := a.syncPaks(b); err != nil {
				a.say("WARNING: content update incomplete: %v", err)
			} else if n > 0 {
				a.say("  %d content paks updated.", n)
			}
			a.saveState()
		}
	}
	if _, err := os.Stat(a.st.ScumExe); err != nil {
		// the game moved (library changed): look again
		d, w, e, err2 := findScum(a.o.scum, steamRoots())
		if err2 != nil {
			a.say("ERROR: SCUM.exe not found at %s and not in any Steam library.", a.st.ScumExe)
			return 1
		}
		a.st.ScumDir, a.st.Win64, a.st.ScumExe = d, w, e
		a.saveState()
	}
	if err := a.ensureSteam(); err != nil {
		a.say("ERROR: %v", err)
		return 1
	}
	if isRunning("SCUM.exe") || isRunning("SCUM-Win64-Shipping.exe") {
		a.say("SCUM is already running.")
		return 0
	}
	args := append([]string{}, a.st.LaunchArgs...)
	paks := a.stashedPaks()
	if len(paks) > 0 {
		args = appendUnique(args, pakFlag)
	}
	args = append(args, a.o.extra...)
	a.say("Starting: %s %s", a.st.ScumExe, strings.Join(args, " "))
	if a.o.dryLaunch || a.o.noLaunch {
		return 0
	}
	if len(paks) > 0 {
		if err := a.activatePaks(paks); err != nil {
			a.restorePaks()
			a.say("ERROR: could not activate the content paks: %v", err)
			return 1
		}
		if err := a.startPakWatcher(); err != nil {
			a.say("WARNING: %v. After the game closes, run: SabrPlay restore-paks", err)
		}
	}
	if err := spawnDetached(a.st.ScumExe, filepath.Dir(a.st.ScumExe), args); err != nil {
		a.restorePaks()
		a.say("ERROR: could not start the game: %v", err)
		return 1
	}
	a.say("Game started - this window closes by itself.")
	time.Sleep(1500 * time.Millisecond)
	return 0
}

// every file of the bundle still present and intact? (a Steam "verify files" or an antivirus may have removed a DLL)
func (a *app) needsRepair(b *bundle) bool {
	for _, f := range b.Files {
		cur := fileSHA(filepath.Join(a.st.Win64, filepath.FromSlash(f.Path)))
		if f.Keep {
			if cur == "" {
				return true
			}
		} else if cur != f.SHA256 {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *app) uninstall() int {
	if !a.installed() && len(a.st.Files) == 0 {
		a.say("Nothing is installed (no state in %s).", a.home)
		return 0
	}
	if isRunning("SCUM.exe") || isRunning("SCUM-Win64-Shipping.exe") {
		a.say("ERROR: SCUM is running. Close it and run the uninstaller again.")
		return 1
	}
	removed := 0
	for p := range a.st.Files {
		dest := filepath.Join(a.st.Win64, filepath.FromSlash(p))
		if err := os.Remove(dest); err == nil {
			removed++
		}
	}
	// restore what we replaced
	for p, bk := range a.st.Backups {
		dest := filepath.Join(a.st.Win64, filepath.FromSlash(p))
		if err := copyFile(bk, dest); err != nil {
			a.say("WARNING: could not restore %s: %v", p, err)
		}
	}
	for name := range a.st.Paks {
		if validPak(name) == nil {
			a.deletePak(name)
		}
	}
	// our mod folder (data/log written at runtime) and empty directories
	os.RemoveAll(filepath.Join(a.st.Win64, "Mods", "SabrVoice"))
	pruneEmpty(filepath.Join(a.st.Win64, "Mods"))
	for _, s := range a.st.Shortcuts {
		os.Remove(s)
	}
	removeShortcutDirs(a)
	a.say("Removed %d mod files, restored %d original files, removed the shortcuts. The game itself was not touched.", removed, len(a.st.Backups))
	a.say("(Steam: \"Verify integrity of game files\" is not needed.)")
	// our own folder last (we may be running from it: leave the exe, delete the rest)
	self, _ := os.Executable()
	ents, _ := os.ReadDir(a.home)
	for _, e := range ents {
		if filepath.Join(a.home, e.Name()) == self {
			continue
		}
		os.RemoveAll(filepath.Join(a.home, e.Name()))
	}
	if runtime.GOOS != "windows" {
		if filepath.Dir(self) == a.home {
			os.Remove(self)
		}
		os.Remove(a.home)
	} else {
		a.say("(You can delete %s by hand.)", a.home)
	}
	return 0
}

func pruneEmpty(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			pruneEmpty(filepath.Join(dir, e.Name()))
		}
	}
	ents, _ = os.ReadDir(dir)
	if len(ents) == 0 {
		os.Remove(dir)
	}
}

func (a *app) status() int {
	b, _ := json.MarshalIndent(map[string]interface{}{
		"installed": a.installed(), "version": a.st.Version, "source": a.sources()[0].base, "scum": a.st.ScumExe, "files": len(a.st.Files),
		"launch_args": a.st.LaunchArgs, "home": a.home, "launcher": version, "paks": a.st.Paks,
	}, "", "  ")
	fmt.Fprintln(a.out, string(b))
	return 0
}
