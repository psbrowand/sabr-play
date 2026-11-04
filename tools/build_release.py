#!/usr/bin/env python3
"""Turn a built mod pack (tools/build_modpack.py output: bundle.json + files/) into the assets of a GitHub release.

  python3 tools/build_release.py --pack build/modpack --launcher build/SabrPlay-Setup.exe --version 1.0.0 --out build/release

Writes into --out (these are exactly the files `gh release create` uploads):
  manifest.json         schema sabr-modpack/2: version, every pack file with sha256 + size, the archive entry and the launcher entry
  sabr-modpack.zip      every pack file (paths relative to <SCUM>\\SCUM\\Binaries\\Win64): UE4SS, the SabrVoice mod, the voice clips
  SabrPlay-Setup.exe    the installer / launcher (the launcher updates itself from this same asset)
  SHA256SUMS            for people who like to check by hand
The launcher reads  <release>/latest/download/manifest.json  and downloads sabr-modpack.zip once; every file is verified against manifest.json.
"""
import argparse
import hashlib
import json
import os
import shutil
import sys
import zipfile


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for c in iter(lambda: f.read(1 << 20), b""):
            h.update(c)
    return h.hexdigest()


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--pack", required=True, help="modpack dir with bundle.json and files/")
    ap.add_argument("--launcher", required=True, help="the launcher exe (becomes SabrPlay-Setup.exe)")
    ap.add_argument("--version", required=True, help="release / pack version, e.g. 1.0.0")
    ap.add_argument("--out", required=True)
    ap.add_argument("--pak", action="append", default=[], help="a content pak to ship as its own asset (repeatable); never put in the zip, so a small mod update doesn't re-download it")
    a = ap.parse_args(argv)

    with open(os.path.join(a.pack, "bundle.json"), encoding="utf-8") as f:
        b = json.load(f)
    if b.get("schema") != "sabr-modpack/1":
        raise SystemExit("unexpected bundle schema %r" % b.get("schema"))
    files = os.path.join(a.pack, "files")
    for e in b["files"]:
        p = os.path.join(files, e["path"])
        if not os.path.isfile(p) or sha(p) != e["sha256"]:
            raise SystemExit("pack does not match its bundle.json: %s" % e["path"])

    out = os.path.abspath(a.out)
    if os.path.isdir(out):
        shutil.rmtree(out)
    os.makedirs(out)
    zpath = os.path.join(out, "sabr-modpack.zip")
    with zipfile.ZipFile(zpath, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for e in sorted(b["files"], key=lambda e: e["path"]):
            zi = zipfile.ZipInfo(e["path"], date_time=(2026, 1, 1, 0, 0, 0))  # fixed time: the zip is reproducible
            zi.compress_type = zipfile.ZIP_STORED if e["path"].endswith(".ogg") else zipfile.ZIP_DEFLATED
            zi.external_attr = 0o644 << 16
            with open(os.path.join(files, e["path"]), "rb") as f:
                z.writestr(zi, f.read())
    exe = os.path.join(out, "SabrPlay-Setup.exe")
    shutil.copy(a.launcher, exe)
    manifest = {
        "schema": "sabr-modpack/2", "version": a.version, "game": b.get("game", ""), "notes": b.get("notes", ""),
        "files": b["files"],
        "archive": {"path": "sabr-modpack.zip", "sha256": sha(zpath), "size": os.path.getsize(zpath)},
        "launcher": {"path": "SabrPlay-Setup.exe", "sha256": sha(exe), "size": os.path.getsize(exe)},
    }
    paks = []
    for src in a.pak:
        name = os.path.basename(src)
        if not name.lower().endswith(".pak"):
            raise SystemExit("not a pak: %s" % src)
        shutil.copy(src, os.path.join(out, name))
        paks.append({"path": name, "sha256": sha(src), "size": os.path.getsize(src)})
    if paks:
        manifest["paks"] = paks
    with open(os.path.join(out, "manifest.json"), "w", encoding="utf-8") as f:
        json.dump(manifest, f, indent=1)
        f.write("\n")
    with open(os.path.join(out, "SHA256SUMS"), "w") as f:
        for n in ["SabrPlay-Setup.exe", "sabr-modpack.zip", "manifest.json"] + [p["path"] for p in paks]:
            f.write("%s  %s\n" % (sha(os.path.join(out, n)), n))
    print("release %s: %d pack files, zip %.1f MB, launcher %.1f MB -> %s" % (
        a.version, len(b["files"]), manifest["archive"]["size"] / 1e6, manifest["launcher"]["size"] / 1e6, out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
