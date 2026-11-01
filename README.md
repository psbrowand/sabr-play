# Play SABR

**Play SABR** is a small Windows launcher/installer for the SABR community SCUM server. It installs the client-side mods the server uses, keeps them
up to date, and starts SCUM for you.

What it installs (only into your SCUM folder, `SCUM\Binaries\Win64`):

* [UE4SS](https://github.com/UE4SS-RE/RE-UE4SS) (the mod loader) with conservative client settings
* **SabrVoice** - a client mod that plays spoken lines (text-to-speech voice pack, ~800 clips) for the server's NPCs and events

It also puts a **Play SABR** shortcut on your desktop (and in the Start menu). The shortcut checks this repo's latest release for mod updates, then starts
SCUM. It is not a service: no tray icon, no autostart, it runs, updates, launches the game and exits.

## Download

**[Download SabrPlay-Setup.exe](https://github.com/psbrowand/sabr-play/releases/latest/download/SabrPlay-Setup.exe)** (always the latest release) -
all releases and checksums: <https://github.com/psbrowand/sabr-play/releases>

1. Install SCUM from Steam and start it once (so Steam has finished installing it). Close the game.
2. Run `SabrPlay-Setup.exe`. It finds SCUM through Steam, downloads the mods (~28 MB) and creates the **Play SABR** shortcut.
3. Start the game with the **Play SABR** shortcut from now on. Updates are automatic.
4. **Join via Discord:** the SABR Discord has the server address and the join password. In SCUM: *Multiplayer -> Connect to IP*, enter the address and the
   password from Discord. (This repo does not list them on purpose.)

### Windows says "Windows protected your PC" (SmartScreen)

`SabrPlay-Setup.exe` is **not code-signed** (certificates cost money and this is a hobby project), so Windows SmartScreen warns about it, and some
antivirus programs may flag it because UE4SS loads through a `dwmapi.dll` proxy. Click **More info -> Run anyway**.
If you would rather not trust a binary, read the source in this repo and build it yourself (below), or compare the SHA-256 of your download with
`SHA256SUMS` on the release page.

## WARNING: no BattlEye

> **Play SABR starts SCUM with `-nobattleye`** (anti-cheat off) so the mods can load. It can only join servers that also run without BattlEye - the SABR
> server. **Never use the Play SABR shortcut for official servers** - for those, start SCUM from Steam as usual. Joining official servers with anti-cheat
> disabled or with injected mods is not allowed and can put your account at risk.

## Uninstall

* Easiest: Start menu -> **SABR -> Uninstall SABR** (or run `%LOCALAPPDATA%\SabrPlay\Uninstall-SABR.cmd`). Close SCUM first.
  It removes every file the installer put into SCUM's `Win64` folder (restoring a `dwmapi.dll` or UE4SS you had before, if any), the shortcuts, and
  `%LOCALAPPDATA%\SabrPlay`. The game itself is never modified; no "Verify integrity of game files" is needed.
* By hand: delete `UE4SS.dll`, `dwmapi.dll`, `UE4SS-settings.ini` and the `Mods` folder from `...\steamapps\common\SCUM\SCUM\Binaries\Win64`, then the two
  shortcuts and `%LOCALAPPDATA%\SabrPlay`.
* Command line: `SabrPlay.exe uninstall`, `SabrPlay.exe status` (JSON of what is installed).

## Troubleshooting

* *"SCUM was not found"* - install SCUM from Steam first, or run `SabrPlay.exe install --scum "D:\SteamLibrary\steamapps\common\SCUM"`.
* *"Update check skipped"* - GitHub was not reachable; the game starts with what is installed. Check your internet connection.
* *"SCUM is running"* - close the game, then run the installer again.
* Antivirus removed `dwmapi.dll`/`UE4SS.dll` - add an exclusion for the SCUM `Win64` folder; the next **Play SABR** run repairs missing files.
* Your own settings (`Mods\SabrVoice\data\config.ini`, e.g. `volume=0.9`) are never overwritten by updates.

## How it works / security

* The launcher reads <https://github.com/psbrowand/sabr-play/releases/latest/download/manifest.json>. It lists every mod file with its SHA-256 and the
  `sabr-modpack.zip` asset; the zip is downloaded once and **every file is verified against the manifest** before it is written. Nothing unverified is
  ever installed, and only `UE4SS.dll`, `dwmapi.dll`, `UE4SS-settings.ini` and paths under `Mods/` can be written (path traversal is refused).
* The launcher updates itself from the `SabrPlay-Setup.exe` asset of the latest release (verified by hash as well).
* An optional second source can be given with `--fallback <url>` (a plain web server with `bundle.json` + `files/`); it is used only if GitHub is unreachable.
  `--server <url>` replaces the GitHub source entirely (a web server with `manifest.json` + the assets). Neither is needed by players.
* Releases are produced by `tools/publish_release.sh`.

## Build from source

Needs Go 1.21+, Python 3 and (for the voice DLL) Zig or a MinGW toolchain.

```sh
cd tools/sabrplay && go test ./... && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o SabrPlay-Setup.exe .
python3 -m unittest tests/test_release_e2e.py        # full install/update/uninstall run against a fake SCUM tree (Linux)
tools/sabrvoice-native/build.sh                      # builds sabrvoice.dll
```

Layout: `tools/sabrplay` (launcher, Go) - `mod/SabrVoice` (the UE4SS Lua mod) - `tools/sabrvoice-native` (C voice player DLL) -
`tools/build_modpack.py` + `tools/build_release.py` (assemble the pack and the release assets) - `tools/publish_release.sh` (publish a release with `gh`).
The voice clips themselves are generated offline and only ship in the release zip.

## License

[MIT](LICENSE). Third-party components: see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Unofficial fan project, not affiliated with the SCUM developers or publisher.
