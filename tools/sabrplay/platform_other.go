//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Non-Windows build exists for the test-suite (fake SCUM tree, fake Steam): everything Windows-specific is a thin, env-driven stub.
func defaultHome() string {
	if h := os.Getenv("SABRPLAY_HOME"); h != "" {
		return h
	}
	d, _ := os.UserCacheDir()
	return filepath.Join(d, "SabrPlay")
}
func exeName() string { return "SabrPlay" }
func steamRoots() []string {
	return strings.FieldsFunc(os.Getenv("SABRPLAY_STEAM_ROOTS"), func(r rune) bool { return r == ':' })
}
func isTerminal() bool { return false }
func isRunning(name string) bool {
	for _, n := range strings.Split(os.Getenv("SABRPLAY_FAKE_RUNNING"), ",") {
		if n != "" && n == name {
			return true
		}
	}
	return false
}
func (a *app) ensureSteam() error { return nil }
func spawnDetached(exe, dir string, args []string) error {
	if f := os.Getenv("SABRPLAY_LAUNCH_LOG"); f != "" { // tests: record instead of starting a game
		return os.WriteFile(f, []byte(exe+"\n"+strings.Join(args, " ")+"\n"), 0o644)
	}
	c := exec.Command(exe, args...)
	c.Dir = dir
	return c.Start()
}
func reexec() int { return 0 }
func (a *app) makeShortcuts(target string) ([]string, error) {
	dir := a.o.desktop
	if dir == "" {
		return nil, nil
	}
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "Play SABR.lnk.txt")
	if err := os.WriteFile(p, []byte(target+" play\n"), 0o644); err != nil {
		return nil, err
	}
	out := []string{p}
	if sm := a.o.startmenu; sm != "" {
		os.MkdirAll(sm, 0o755)
		q := filepath.Join(sm, "Play SABR.lnk.txt")
		os.WriteFile(q, []byte(target+" play\n"), 0o644)
		u := filepath.Join(sm, "Uninstall SABR.lnk.txt")
		os.WriteFile(u, []byte(target+" uninstall\n"), 0o644)
		out = append(out, q, u)
	}
	return out, nil
}
func removeShortcutDirs(a *app) {
	if a.o.startmenu != "" {
		os.Remove(a.o.startmenu)
	}
}
