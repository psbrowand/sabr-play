#!/usr/bin/env python3
"""Assemble the SABR client mod pack (bundle.json + files/) that tools/build_release.py turns into GitHub release assets and that tools/sabrplay installs.

  python3 tools/build_modpack.py --out build/modpack --ue4ss-dir <dir with UE4SS.dll + dwmapi.dll> --voice-dir <manifest.json + audio/> \\
        --dll tools/sabrvoice-native/build/sabrvoice.dll --launcher <SabrPlay.exe>

Layout of --out:
  bundle.json           sabr-modpack/1: version + [{path, sha256, size, keep?}] (paths are relative to <SCUM>\\SCUM\\Binaries\\Win64) + launcher entry
  files/<path>          every file of the pack (UE4SS proxy+dll, client settings, Mods/SabrVoice/{enabled.txt,scripts/main.lua,scripts/sabrvoice.dll,voice/*})
  SabrPlay-Setup.exe    the launcher/installer
UE4SS: the pinned build is UE4SS v3.0.1 Beta (git c2ac246; sha256 55a8cbe8...), legacy flat layout (UE4SS.dll + dwmapi.dll next to SCUM.exe, Mods\\ beside them)."""
import argparse
import hashlib
import json
import os
import re
import shutil
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
sys.path.insert(0, os.path.join(HERE, "sabrvoice-native"))
import make_lines_tsv  # noqa: E402

LUA = next((p for p in (os.path.join(REPO, "mod", "SabrVoice", "scripts", "main.lua"),
                        os.path.join(REPO, "docs", "lore", "voice-mod", "ue4ss", "SabrVoice", "scripts", "main.lua")) if os.path.isfile(p)), "")
PINNED_UE4SS_SHA = "55a8cbe82f6c373d00f6d4d78d069ccca22df980ecec5bef63cd5e6ac4544059"

# client profile: conservative (the server-proven hooks only), no consoles, no object cache, engine pinned to 4.27
CLIENT_INI = {
    "UseCache": "0", "bUseUObjectArrayCache": "false", "MajorVersion": "4", "MinorVersion": "27",
    "ConsoleEnabled": "0", "GuiConsoleEnabled": "0", "GuiConsoleVisible": "0",
    "HookProcessLocalScriptFunction": "0", "HookInitGameState": "0", "HookCallFunctionByNameWithArguments": "0", "HookBeginPlay": "0",
}
DEFAULT_CONFIG = "; SabrVoice settings (this file is yours: updates never overwrite it)\nvolume=0.9\n"


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for c in iter(lambda: f.read(1 << 20), b""):
            h.update(c)
    return h.hexdigest()


def client_ini(base_text: str) -> str:
    out = base_text
    for k, v in CLIENT_INI.items():
        out, n = re.subn(r"(?m)^(%s\s*=).*$" % re.escape(k), lambda m, v=v: m.group(1) + " " + v, out)
        if n == 0:
            raise SystemExit("UE4SS-settings base has no key %s" % k)
    return out


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--ue4ss-dir", required=True)
    ap.add_argument("--ue4ss-ini", default=os.path.join(HERE, "modpack", "UE4SS-settings.base.ini"))
    ap.add_argument("--voice-dir", required=True)
    ap.add_argument("--dll", required=True)
    ap.add_argument("--launcher", required=True)
    ap.add_argument("--version", default="")
    ap.add_argument("--game", default="1.3.5.2")
    ap.add_argument("--allow-unpinned-ue4ss", action="store_true")
    a = ap.parse_args(argv)

    if not LUA:
        raise SystemExit("cannot find the SabrVoice main.lua (mod/SabrVoice/scripts/main.lua)")
    ue = os.path.join(a.ue4ss_dir, "UE4SS.dll")
    if sha(ue) != PINNED_UE4SS_SHA and not a.allow_unpinned_ue4ss:
        raise SystemExit("UE4SS.dll is not the pinned build (%s); pass --allow-unpinned-ue4ss to override" % PINNED_UE4SS_SHA[:12])
    out = os.path.abspath(a.out)
    if os.path.isdir(out):
        shutil.rmtree(out)
    files = os.path.join(out, "files")
    mod = os.path.join(files, "Mods", "SabrVoice")
    for d in (os.path.join(mod, "scripts"), os.path.join(mod, "voice", "audio"), os.path.join(mod, "data")):
        os.makedirs(d)
    shutil.copy(ue, os.path.join(files, "UE4SS.dll"))
    shutil.copy(os.path.join(a.ue4ss_dir, "dwmapi.dll"), os.path.join(files, "dwmapi.dll"))
    with open(a.ue4ss_ini, encoding="utf-8") as f, open(os.path.join(files, "UE4SS-settings.ini"), "w", encoding="utf-8", newline="\r\n") as o:
        o.write(client_ini(f.read()))
    with open(os.path.join(mod, "enabled.txt"), "w") as f:
        f.write("1\n")
    shutil.copy(LUA, os.path.join(mod, "scripts", "main.lua"))
    shutil.copy(a.dll, os.path.join(mod, "scripts", "sabrvoice.dll"))
    with open(os.path.join(mod, "data", "config.ini"), "w") as f:
        f.write(DEFAULT_CONFIG)
    n = make_lines_tsv.convert(os.path.join(a.voice_dir, "manifest.json"), os.path.join(mod, "voice", "lines.tsv"))
    shutil.copy(os.path.join(a.voice_dir, "manifest.json"), os.path.join(mod, "voice", "manifest.json"))
    with open(os.path.join(a.voice_dir, "manifest.json"), encoding="utf-8-sig") as f:
        man = json.load(f)
    need = {l["file"].split("/")[-1] for l in man["lines"]}
    for name in sorted(need):
        src = os.path.join(a.voice_dir, "audio", name)
        if not os.path.isfile(src):
            raise SystemExit("voice pack is missing audio/%s" % name)
        shutil.copy(src, os.path.join(mod, "voice", "audio", name))
    shutil.copy(a.launcher, os.path.join(files, "SabrPlay.exe"))
    shutil.copy(a.launcher, os.path.join(out, "SabrPlay-Setup.exe"))

    entries = []
    for root, _, names in os.walk(files):
        for name in sorted(names):
            full = os.path.join(root, name)
            rel = os.path.relpath(full, files).replace(os.sep, "/")
            if rel == "SabrPlay.exe":
                continue
            e = {"path": rel, "sha256": sha(full), "size": os.path.getsize(full)}
            if rel == "Mods/SabrVoice/data/config.ini":
                e["keep"] = True
            entries.append(e)
    entries.sort(key=lambda e: e["path"])
    digest = hashlib.sha256(json.dumps(entries, sort_keys=True).encode()).hexdigest()[:8]
    version = a.version or time.strftime("%Y.%m.%d") + "-" + digest
    bundle = {"schema": "sabr-modpack/1", "version": version, "game": a.game, "files": entries,
              "launcher": {"path": "SabrPlay.exe", "sha256": sha(os.path.join(files, "SabrPlay.exe")), "size": os.path.getsize(os.path.join(files, "SabrPlay.exe"))},
              "notes": "UE4SS v3.0.1 (server build c2ac246) + SabrVoice %d voice lines" % n}
    with open(os.path.join(out, "bundle.json"), "w") as f:
        json.dump(bundle, f, indent=1)
    total = sum(e["size"] for e in entries)
    print("modpack %s: %d files, %.1f MB -> %s" % (version, len(entries), total / 1e6, out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
