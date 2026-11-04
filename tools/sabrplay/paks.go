package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Content paks (new assets for the game) are not like the UE4SS files in Win64: SCUM exits at startup with "Pak file or matching sig
// file integrity compromised" when an unsigned pak sits under Content\Paks and the game runs without -fileopenlog. A plain Steam start
// has to keep working (official servers), so a pak lives in a stash folder and is under Paks\~mods only while Play SABR runs the game.
const pakFlag = "-fileopenlog"

// how the restore-paks watcher notices the game has closed
var (
	pakWatchPoll    = 3 * time.Second
	pakWatchStart   = 5 * time.Minute  // the game must show up within this time
	pakWatchGone    = 30 * time.Second // and stay gone this long (a crash reporter or a quick restart must not pull the pak away)
	pakDownloadTime = 2 * time.Hour    // a pak is far bigger than the pack zip
)

func validPak(n string) error {
	if err := validAsset(n); err != nil {
		return err
	}
	if !strings.EqualFold(path.Ext(n), ".pak") {
		return fmt.Errorf("pak name not allowed: %q", n)
	}
	return nil
}

func (a *app) pakStash() string { return filepath.Join(a.st.ScumDir, "SCUM", "Content", "SabrPaks") }
func (a *app) pakLive() string {
	return filepath.Join(a.st.ScumDir, "SCUM", "Content", "Paks", "~mods")
}

func gameRunning() bool { return isRunning("SCUM.exe") || isRunning("SCUM-Win64-Shipping.exe") }

// syncPaks makes the stash match the manifest. Returns how many paks were downloaded.
func (a *app) syncPaks(b *bundle) (int, error) {
	if a.cur == nil || a.cur.name == "fallback server" || a.st.ScumDir == "" {
		return 0, nil // the LAN hub does not carry paks: not a reason to delete them
	}
	if gameRunning() {
		return 0, nil // a pak under ~mods is in use, and it is not in the stash to compare
	}
	if a.st.Paks == nil {
		a.st.Paks = map[string]string{}
	}
	want := map[string]bool{}
	n := 0
	var firstErr error
	for _, p := range b.Paks {
		want[p.Path] = true
		dest := filepath.Join(a.pakStash(), p.Path)
		if fileSHA(dest) == p.SHA256 {
			a.st.Paks[p.Path] = p.SHA256
			continue
		}
		a.say("Downloading %s (%d MB)...", p.Path, p.Size/(1<<20))
		u := a.cur.base + "/" + p.Path
		if b.Archive == nil {
			u = a.cur.base + "/files/" + p.Path
		}
		saved := a.http.Timeout
		a.http.Timeout = pakDownloadTime
		err := a.fetchTo(u, p.SHA256, dest)
		a.http.Timeout = saved
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %v", p.Path, err)
			}
			continue
		}
		a.st.Paks[p.Path] = p.SHA256
		n++
	}
	var gone []string
	for name := range a.st.Paks {
		if !want[name] {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	for _, name := range gone {
		a.deletePak(name)
		delete(a.st.Paks, name)
	}
	return n, firstErr
}

func (a *app) deletePak(name string) {
	os.Remove(filepath.Join(a.pakStash(), name))
	os.Remove(filepath.Join(a.pakLive(), name))
	os.Remove(a.pakStash())
	os.Remove(a.pakLive()) // only if nothing else is in there: other mods may live in ~mods
}

// stashedPaks lists the paks ready to go live (intact: a half-written or damaged pak must never be mounted).
func (a *app) stashedPaks() []string {
	var l []string
	for name, want := range a.st.Paks {
		if validPak(name) == nil && fileSHA(filepath.Join(a.pakStash(), name)) == want {
			l = append(l, name)
		}
	}
	sort.Strings(l)
	return l
}

func (a *app) activatePaks(names []string) error {
	if err := os.MkdirAll(a.pakLive(), 0o755); err != nil {
		return err
	}
	for _, name := range names {
		if err := os.Rename(filepath.Join(a.pakStash(), name), filepath.Join(a.pakLive(), name)); err != nil {
			return err
		}
	}
	return nil
}

// restorePaks moves our paks out of ~mods again, so a plain Steam start of the game works.
func (a *app) restorePaks() {
	if a.st.ScumDir == "" {
		return
	}
	for name := range a.st.Paks {
		if validPak(name) != nil {
			continue
		}
		live := filepath.Join(a.pakLive(), name)
		if _, err := os.Stat(live); err != nil {
			continue
		}
		if err := os.MkdirAll(a.pakStash(), 0o755); err != nil {
			a.say("WARNING: could not put %s back: %v", name, err)
			continue
		}
		if err := os.Rename(live, filepath.Join(a.pakStash(), name)); err != nil {
			a.say("WARNING: could not put %s back: %v", name, err)
		}
	}
	os.Remove(a.pakLive()) // only if empty
}

func (a *app) startPakWatcher() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return spawnDetached(self, a.home, []string{"restore-paks", "--home", a.home})
}

// watchPaks is the hidden `restore-paks` process: it waits for the game to start and close, then restores the paks.
func (a *app) watchPaks() int {
	for waited := time.Duration(0); !gameRunning() && waited < pakWatchStart; waited += pakWatchPoll {
		time.Sleep(pakWatchPoll)
	}
	for gone := time.Duration(0); gone < pakWatchGone; {
		time.Sleep(pakWatchPoll)
		if gameRunning() {
			gone = 0
		} else {
			gone += pakWatchPoll
		}
	}
	a.restorePaks()
	return 0
}
