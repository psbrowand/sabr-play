#!/usr/bin/env bash
# Publish a new "Play SABR" release (SabrPlay-Setup.exe + sabr-modpack.zip + manifest.json) to GitHub Releases.
#
#   tools/publish_release.sh <version> --pack <modpack dir>      # new mod pack (tools/build_modpack.py output: bundle.json + files/)
#   tools/publish_release.sh <version> --reuse-latest            # launcher-only release: keep the mods/voices of the latest release
#   options: --repo owner/name (default psbrowand/sabr-play)  --notes "text"  --dry-run (build + verify locally, publish nothing)
#            --skip-tests  --out DIR (default build/release-<version>)
#
# Needs on this machine: gh (logged in, repo scope), go, python3, curl.
# What it does: go test -> cross-build the Windows launcher -> tools/build_release.py -> secret scan of the pack ->
#   gh release create v<version> (tag on the default branch) -> anonymous curl of every public asset URL + hash check against manifest.json.
# Players pick the new version up the next time they click "Play SABR": the launcher reads <repo>/releases/latest/download/manifest.json,
# compares versions, and (if different) downloads sabr-modpack.zip and verifies every file by sha256. Nothing is pushed to any server.
set -euo pipefail
VER=${1:?usage: publish_release.sh <version e.g. 1.0.1> (--pack DIR | --reuse-latest) [--repo owner/name] [--notes TEXT] [--dry-run]}; shift
[[ $VER =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "version must look like 1.2.3"; exit 1; }
REPO=${SABR_RELEASE_REPO:-psbrowand/sabr-play}; PACK=""; REUSE=0; DRY=0; TESTS=1; NOTES=""; OUT=""
while [ $# -gt 0 ]; do
  case $1 in
    --pack) PACK=$2; shift 2;; --reuse-latest) REUSE=1; shift;; --repo) REPO=$2; shift 2;; --notes) NOTES=$2; shift 2;;
    --dry-run) DRY=1; shift;; --skip-tests) TESTS=0; shift;; --out) OUT=$2; shift 2;; *) echo "unknown option $1"; exit 1;;
  esac
done
HERE=$(cd "$(dirname "$0")" && pwd); ROOT=$(dirname "$HERE")
OUT=${OUT:-$ROOT/build/release-$VER}; TAG=v$VER; WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
for t in go python3 curl; do command -v $t >/dev/null || { echo "missing $t"; exit 1; }; done
[ $DRY = 1 ] || command -v gh >/dev/null || { echo "missing gh"; exit 1; }
[ -n "$PACK" ] || [ $REUSE = 1 ] || { echo "give --pack DIR or --reuse-latest"; exit 1; }

if [ $REUSE = 1 ]; then
  echo "== reusing the mod pack of the latest release of $REPO"
  PACK=$WORK/pack; mkdir -p "$WORK/dl" "$PACK/files"
  BASE=https://github.com/$REPO/releases/latest/download
  curl -fsSL -o "$WORK/dl/manifest.json" "$BASE/manifest.json"; curl -fsSL -o "$WORK/dl/sabr-modpack.zip" "$BASE/sabr-modpack.zip"
  python3 - "$WORK/dl" "$PACK" <<'PY'
import hashlib, json, sys, zipfile
dl, pack = sys.argv[1:3]
m = json.load(open(dl + "/manifest.json")); z = zipfile.ZipFile(dl + "/sabr-modpack.zip")
assert hashlib.sha256(open(dl + "/sabr-modpack.zip", "rb").read()).hexdigest() == m["archive"]["sha256"], "latest zip does not match its manifest"
z.extractall(pack + "/files")
json.dump({"schema": "sabr-modpack/1", "version": m["version"], "game": m.get("game", ""), "notes": m.get("notes", ""), "files": m["files"]}, open(pack + "/bundle.json", "w"))
PY
fi
[ -f "$PACK/bundle.json" ] && [ -d "$PACK/files" ] || { echo "pack dir needs bundle.json and files/"; exit 1; }

if [ $TESTS = 1 ]; then
  echo "== tests"
  (cd "$ROOT/tools/sabrplay" && go vet ./... && go test -count=1 ./...)
  if [ -f "$ROOT/tests/test_release_e2e.py" ]; then (cd "$ROOT" && python3 -m unittest tests/test_release_e2e.py 2>&1 | tail -4); fi
fi

echo "== build launcher $VER (windows/amd64)"
mkdir -p "$WORK/bin"
(cd "$ROOT/tools/sabrplay" && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$VER" -o "$WORK/bin/SabrPlay-Setup.exe" .)
python3 "$HERE/build_release.py" --pack "$PACK" --launcher "$WORK/bin/SabrPlay-Setup.exe" --version "$VER" --out "$OUT"

echo "== secret scan (pack files, manifest, launcher) - patterns only, values are never printed"
python3 - "$PACK" "$OUT" <<'PY'
import json, os, re, sys
pats = {"private IPv4": rb"\b(?:10\.\d{1,3}|192\.168|172\.(?:1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}\b", "steam id": rb"76561[1]9\d{10}", "credential assignment": rb"(?i)(p[a]ss(?:wo)?r?d|t[o]ken|api[_-]?k[e]y|secret_k[e]y)\s*[=:]\s*\S",
        "env var": rb"SCUM_D[A]SH\w*|D[I]SCORD_\w+"}
pack, out = sys.argv[1:3]
targets = [os.path.join(pack, "files", e["path"]) for e in json.load(open(os.path.join(pack, "bundle.json")))["files"]]  # only what ships
targets += [os.path.join(out, n) for n in ("SabrPlay-Setup.exe", "manifest.json")]
bad = 0
for p in targets:
    data = open(p, "rb").read()
    for label, rx in pats.items():
        if re.search(rx, data):
            print("FOUND %s in %s" % (label, os.path.basename(p))); bad += 1
sys.exit(1 if bad else 0)
PY
echo "   clean"
[ $DRY = 1 ] && { echo "== dry run: assets are in $OUT (nothing published)"; ls -la "$OUT"; exit 0; }

NOTES=${NOTES:-"Play SABR $VER. Run SabrPlay-Setup.exe (Windows may show SmartScreen: More info > Run anyway - the exe is not code-signed). Never use the launcher on official servers: it starts SCUM without BattlEye."}
echo "== gh release create $TAG on $REPO"
gh release create "$TAG" "$OUT/SabrPlay-Setup.exe" "$OUT/sabr-modpack.zip" "$OUT/manifest.json" "$OUT/SHA256SUMS" --repo "$REPO" --title "Play SABR $VER" --notes "$NOTES" --latest

echo "== verify the public URLs (anonymous) and the manifest hashes"
python3 - "$REPO" "$TAG" "$OUT/manifest.json" <<'PY'
import hashlib, json, sys, urllib.request
repo, tag, mf = sys.argv[1:4]
m = json.load(open(mf)); ok = True
for name, key in (("manifest.json", None), ("sabr-modpack.zip", "archive"), ("SabrPlay-Setup.exe", "launcher")):
    for url in ("https://github.com/%s/releases/download/%s/%s" % (repo, tag, name), "https://github.com/%s/releases/latest/download/%s" % (repo, name)):
        data = urllib.request.urlopen(url, timeout=120).read()
        good = key is None or hashlib.sha256(data).hexdigest() == m[key]["sha256"]
        ok &= good
        print("%s %s %d bytes %s" % ("OK " if good else "BAD", url, len(data), "" if good else "HASH MISMATCH"))
sys.exit(0 if ok else 1)
PY
echo "published $TAG: https://github.com/$REPO/releases/tag/$TAG"
