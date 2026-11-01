// SabrPlay - the "Play SABR" launcher and one-click installer for the SABR SCUM server.
//
//	SabrPlay.exe              first run: install (finds SCUM via Steam, installs UE4SS + the SABR mods + the voice pack, makes the desktop
//	                          shortcut); later runs: same as "play"
//	SabrPlay.exe install      (re)install / repair
//	SabrPlay.exe play         check GitHub for mod + voice-pack updates, then start SCUM WITHOUT BattlEye and exit
//	SabrPlay.exe uninstall    remove everything this tool installed (the game itself is never touched) and the shortcuts
//	SabrPlay.exe status       what is installed (JSON)
//
// It is not a service, has no tray icon and no autostart: it runs, updates, starts the game and exits.
// NEVER use the "Play SABR" shortcut for official servers: the game it starts has no anti-cheat.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	version = "1.0.0"
	// where the mod pack comes from: the latest public GitHub release (manifest.json + assets)
	defaultRepoURL = "https://github.com/psbrowand/sabr-play"
	defaultSource  = defaultRepoURL + "/releases/latest/download"
	// optional second source (e.g. a LAN mod hub) tried only when the first is unreachable; empty in the public build.
	// set at build time: -ldflags "-X main.defaultFallback=http://host:port/mods", or per run/install with --fallback
	defaultFallback = ""
)

const warning = `
  !! Play SABR starts SCUM WITHOUT BattlEye (anti-cheat) so mods can load.
  !! It can only join servers that also run without BattlEye (the SABR server).
  !! NEVER use it on official servers - for those start SCUM from Steam as usual.
`

type options struct {
	server, fallback, scum, home, desktop, startmenu string
	yes, noLaunch, dryLaunch, noUpdate               bool
	extra                                            []string // game arguments after `--`
}

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("sabrplay", flag.ExitOnError)
	var o options
	fs.StringVar(&o.server, "server", "", "mod source base URL serving manifest.json (default: the latest GitHub release of psbrowand/sabr-play)")
	fs.StringVar(&o.fallback, "fallback", "", "optional second mod source (a mod hub with bundle.json + files/) used when the first is unreachable")
	fs.StringVar(&o.scum, "scum", "", "SCUM install folder (the one that contains SCUM\\Binaries), default: find via Steam")
	fs.StringVar(&o.home, "home", "", "where SabrPlay keeps its own files (default %LOCALAPPDATA%\\SabrPlay)")
	fs.StringVar(&o.desktop, "desktop", "", "folder for the desktop shortcut (default: the user's Desktop)")
	fs.StringVar(&o.startmenu, "startmenu", "", "folder for the Start-menu shortcuts (default: the user's Programs\\SABR)")
	fs.BoolVar(&o.yes, "yes", false, "no questions, no 'press Enter'")
	fs.BoolVar(&o.noLaunch, "no-launch", false, "install/update only, do not start the game")
	fs.BoolVar(&o.dryLaunch, "dry-launch", false, "print the launch command instead of running it")
	fs.BoolVar(&o.noUpdate, "no-update", false, "play: skip the update check")
	fs.Parse(args)
	o.extra = fs.Args()

	app := newApp(o)
	code := 0
	switch cmd {
	case "", "auto":
		if app.installed() {
			code = app.play()
		} else {
			code = app.install()
		}
	case "install":
		code = app.install()
	case "play":
		code = app.play()
	case "uninstall":
		code = app.uninstall()
	case "status":
		code = app.status()
	case "version":
		fmt.Println("SabrPlay", version)
	default:
		fmt.Println("usage: SabrPlay [install|play|uninstall|status] [--server URL] [--fallback URL] [--scum DIR] [--yes] [-- game args, e.g. -windowed -ResX=1280]")
		code = 2
	}
	if code != 0 || (cmd == "" || cmd == "install" || cmd == "uninstall" || cmd == "auto") && !o.yes {
		pause(o)
	}
	os.Exit(code)
}

func pause(o options) {
	if o.yes || !isTerminal() {
		return
	}
	fmt.Print("\nPress Enter to close...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}
