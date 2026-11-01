//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

func defaultHome() string {
	d := os.Getenv("LOCALAPPDATA")
	if d == "" {
		d, _ = os.UserCacheDir()
	}
	return filepath.Join(d, "SabrPlay")
}
func exeName() string { return "SabrPlay.exe" }

func regQuery(key, val string) string {
	c := exec.Command("reg", "query", key, "/v", val)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := c.Output()
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(val) + `\s+REG_\w+\s+(.*?)\s*$`)
	if m := re.FindStringSubmatch(string(out)); m != nil {
		return m[1]
	}
	return ""
}

func steamRoots() []string {
	var r []string
	if p := regQuery(`HKCU\Software\Valve\Steam`, "SteamPath"); p != "" {
		r = append(r, filepath.FromSlash(p))
	}
	if p := regQuery(`HKLM\SOFTWARE\WOW6432Node\Valve\Steam`, "InstallPath"); p != "" {
		r = append(r, p)
	}
	r = append(r, `C:\Program Files (x86)\Steam`, `C:\Program Files\Steam`)
	return r
}

func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func isRunning(name string) bool {
	c := exec.Command("tasklist", "/FI", "IMAGENAME eq "+name, "/FO", "CSV", "/NH")
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := c.Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), strings.ToLower(name))
}

func steamUser() string { return regQuery(`HKCU\Software\Valve\Steam\ActiveProcess`, "ActiveUser") }

// SCUM.exe talks to the Steam client: Steam must be running and signed in. Start it (silently) if needed and wait.
func (a *app) ensureSteam() error {
	ok := func() bool { u := steamUser(); return u != "" && u != "0x0" }
	if ok() {
		return nil
	}
	exe := regQuery(`HKCU\Software\Valve\Steam`, "SteamExe")
	if exe == "" {
		return fmt.Errorf("Steam is not running and was not found. Start Steam, sign in, then click Play SABR again")
	}
	a.say("Starting Steam...")
	if err := spawnDetached(filepath.FromSlash(exe), filepath.Dir(filepath.FromSlash(exe)), []string{"-silent"}); err != nil {
		return fmt.Errorf("could not start Steam: %v", err)
	}
	for i := 0; i < 60; i++ {
		time.Sleep(2 * time.Second)
		if ok() {
			time.Sleep(3 * time.Second)
			return nil
		}
	}
	return fmt.Errorf("Steam did not sign in within 2 minutes. Sign in to Steam, then click Play SABR again")
}

func spawnDetached(exe, dir string, args []string) error {
	c := exec.Command(exe, args...)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
	if err := c.Start(); err != nil {
		return err
	}
	c.Process.Release()
	return nil
}

func reexec() int {
	self, _ := os.Executable()
	c := exec.Command(self, os.Args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return 1
	}
	return 0
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Desktop + Start-menu shortcuts through WScript.Shell (per-user folders, no admin). Icon = SCUM's own.
func (a *app) makeShortcuts(target string) ([]string, error) {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$sh = New-Object -ComObject WScript.Shell
$desk = %s; if (-not $desk) { $desk = [Environment]::GetFolderPath('Desktop') }
$sm = %s; if (-not $sm) { $sm = Join-Path ([Environment]::GetFolderPath('Programs')) 'SABR' }
New-Item -ItemType Directory -Force $sm | Out-Null
function Mk($path, $largs, $desc, $icon) { $l = $sh.CreateShortcut($path); $l.TargetPath = %s; $l.Arguments = $largs; $l.WorkingDirectory = %s; $l.Description = $desc; if ($icon) { $l.IconLocation = $icon }; $l.Save(); $path }
Mk (Join-Path $desk 'Play SABR.lnk') 'play' 'Update the SABR mods and start SCUM without BattlEye (SABR server only - never official servers)' (%s + ',0')
Mk (Join-Path $sm 'Play SABR.lnk') 'play' 'Update the SABR mods and start SCUM without BattlEye (SABR server only)' (%s + ',0')
Mk (Join-Path $sm 'Uninstall SABR.lnk') 'uninstall' 'Remove the SABR mods and shortcuts' $null
`, psQuote(a.o.desktop), psQuote(a.o.startmenu), psQuote(target), psQuote(filepath.Dir(target)), psQuote(a.st.ScumExe), psQuote(a.st.ScumExe))
	c := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := c.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	var made []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasSuffix(strings.ToLower(l), ".lnk") {
			made = append(made, l)
		}
	}
	return made, nil
}

func removeShortcutDirs(a *app) {
	for _, s := range a.st.Shortcuts {
		d := filepath.Dir(s)
		if strings.EqualFold(filepath.Base(d), "SABR") {
			os.Remove(d)
		}
	}
}
