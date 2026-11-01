"""Play SABR from a GitHub-style release, end to end on Linux (no network, no game):
tools/build_modpack.py -> tools/build_release.py (manifest.json + sabr-modpack.zip + SabrPlay-Setup.exe) -> a static HTTP server that looks like
https://github.com/<repo>/releases/latest/download -> the REAL launcher binary (tools/sabrplay, Linux build, fake SCUM tree + fake Steam):
install / update / repair / self-update / offline play / uninstall / tamper refusal / LAN-style fallback.
Needs: go.   Run:  python3 -m unittest tests/test_release_e2e.py   (from the repo root)"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, ".."))
sys.path.insert(0, os.path.join(REPO, "tools"))
import build_modpack  # noqa: E402
import build_release  # noqa: E402

GO = shutil.which("go")


def make_inputs(tmp, lua_note="v1"):
    ue = os.path.join(tmp, "ue"); vo = os.path.join(tmp, "voice"); dll = os.path.join(tmp, "sabrvoice.dll"); exe = os.path.join(tmp, "SabrPlay.exe")
    for d in (ue, os.path.join(vo, "audio")):
        os.makedirs(d, exist_ok=True)
    open(os.path.join(ue, "UE4SS.dll"), "wb").write(b"UE4SS-" + lua_note.encode())
    open(os.path.join(ue, "dwmapi.dll"), "wb").write(b"proxy")
    open(dll, "wb").write(b"MZ-sabrvoice-" + lua_note.encode())
    open(exe, "wb").write(b"launcher-placeholder")
    lines = []
    for i in range(3):
        fn = "%016x.ogg" % (i + 1)
        open(os.path.join(vo, "audio", fn), "wb").write(b"OggS" + bytes([i]) * 50)
        lines.append({"id": "npc.t.%d" % i, "speaker": "t", "speaker_name": "Tester", "text": "Line number %d here friend." % i,
                      "template": "Line number %d here friend." % i, "dynamic": False, "file": "audio/" + fn, "duration_s": 1.0})
    json.dump({"schema": "x", "lines": lines}, open(os.path.join(vo, "manifest.json"), "w"))
    return ue, vo, dll, exe



@unittest.skipUnless(GO, "needs go")
class TestPlaySabrFromRelease(unittest.TestCase):
    """The public path: build_modpack -> build_release (manifest.json + sabr-modpack.zip + SabrPlay-Setup.exe) -> a static HTTP server that looks
    like github.com/<repo>/releases/latest/download -> the REAL launcher binary. Plus the optional LAN fallback (the framework modserve)."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="playsabrrel")
        cls.exe = os.path.join(cls.tmp, "sabrplay")
        r = subprocess.run([GO, "build", "-o", cls.exe, "."], cwd=os.path.join(REPO, "tools", "sabrplay"), capture_output=True, text=True)
        if r.returncode:
            raise RuntimeError(r.stderr)

    def _mk(self, name):
        d = os.path.join(self.tmp, name)
        os.makedirs(d, exist_ok=True)
        return d

    def release(self, note, launcher=None):
        ue, vo, dll, exe = make_inputs(self._mk("rin-" + note + os.urandom(2).hex()), note)
        pack = os.path.join(self.tmp, "pack-" + note)
        build_modpack.main(["--out", pack, "--ue4ss-dir", ue, "--voice-dir", vo, "--dll", dll, "--launcher", launcher or exe, "--allow-unpinned-ue4ss", "--version", note])
        out = self.relroot
        build_release.main(["--pack", pack, "--launcher", launcher or exe, "--version", note, "--out", out])
        return out

    def serve(self, directory):
        class Quiet(SimpleHTTPRequestHandler):
            def log_message(self, *a):
                pass
        srv = ThreadingHTTPServer(("127.0.0.1", 0), partial(Quiet, directory=directory))
        srv.daemon_threads = True
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(lambda: (srv.shutdown(), srv.server_close()))
        return srv, "http://127.0.0.1:%d" % srv.server_address[1]

    def setUp(self):
        self.relroot = os.path.join(self.tmp, "rel-%s" % os.urandom(3).hex())
        self.release("1.0.0")
        self.srv, self.base = self.serve(self.relroot)
        self.lib = self._mk("steam-%s" % os.urandom(3).hex())
        self.win64 = os.path.join(self.lib, "steamapps", "common", "SCUM", "SCUM", "Binaries", "Win64")
        os.makedirs(self.win64)
        open(os.path.join(self.lib, "steamapps", "appmanifest_513710.acf"), "w").write('"AppState" {}')
        open(os.path.join(self.win64, "SCUM.exe"), "wb").write(b"game")
        self.home = self._mk("home-%s" % os.urandom(3).hex())
        self.desk = self._mk("desk-%s" % os.urandom(3).hex())
        self.sm = self._mk("sm-%s" % os.urandom(3).hex())
        self.launchlog = os.path.join(self.tmp, "launch-%s.txt" % os.urandom(3).hex())
        self.env = dict(os.environ, SABRPLAY_STEAM_ROOTS=self.lib, SABRPLAY_HOME=self.home, SABRPLAY_LAUNCH_LOG=self.launchlog)

    def run_exe(self, *args, exe=None, server=True):
        extra = ["--server", self.base] if server else []
        return subprocess.run([exe or self.exe, *args, "--yes", "--home", self.home, "--desktop", self.desk, "--startmenu", self.sm, *extra],
                              capture_output=True, text=True, env=self.env, timeout=120)

    def test_manifest_matches_assets(self):
        m = json.load(open(os.path.join(self.relroot, "manifest.json")))
        self.assertEqual(m["schema"], "sabr-modpack/2")
        self.assertEqual(m["version"], "1.0.0")
        import hashlib
        for key, name in (("archive", "sabr-modpack.zip"), ("launcher", "SabrPlay-Setup.exe")):
            data = open(os.path.join(self.relroot, name), "rb").read()
            self.assertEqual(m[key]["path"], name)
            self.assertEqual(m[key]["sha256"], hashlib.sha256(data).hexdigest())
            self.assertEqual(m[key]["size"], len(data))
        import zipfile
        z = zipfile.ZipFile(os.path.join(self.relroot, "sabr-modpack.zip"))
        self.assertEqual(sorted(z.namelist()), sorted(e["path"] for e in m["files"]))
        for e in m["files"]:
            self.assertEqual(hashlib.sha256(z.read(e["path"])).hexdigest(), e["sha256"])
        self.assertNotIn("SabrPlay.exe", z.namelist())

    def test_install_update_repair_offline_uninstall(self):
        r = self.run_exe("install")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("NEVER use it on official servers", r.stdout)
        self.assertIn("Mod source: server", r.stdout)
        for rel in ("UE4SS.dll", "dwmapi.dll", "UE4SS-settings.ini", "Mods/SabrVoice/scripts/main.lua", "Mods/SabrVoice/scripts/sabrvoice.dll",
                    "Mods/SabrVoice/voice/lines.tsv", "Mods/SabrVoice/enabled.txt", "Mods/SabrVoice/data/config.ini"):
            self.assertTrue(os.path.isfile(os.path.join(self.win64, rel)), rel)
        self.assertEqual(len(os.listdir(os.path.join(self.win64, "Mods", "SabrVoice", "voice", "audio"))), 3)
        self.assertEqual(sorted(os.listdir(self.win64)), sorted(["SCUM.exe", "UE4SS.dll", "dwmapi.dll", "UE4SS-settings.ini", "Mods"]))
        self.assertFalse(os.path.exists(os.path.join(self.home, "cache")))
        r = self.run_exe("play")
        self.assertIn("Mods are up to date", r.stdout)
        log = open(self.launchlog).read().splitlines()
        self.assertEqual(log[1], "-nobattleye")
        # new release: changed files, the user's config survives
        cfgp = os.path.join(self.win64, "Mods", "SabrVoice", "data", "config.ini")
        open(cfgp, "w").write("volume=0.2\n")
        self.release("1.0.1")
        r = self.run_exe("play")
        self.assertIn("Updating mods 1.0.0 -> 1.0.1", r.stdout, r.stdout)
        self.assertEqual(open(os.path.join(self.win64, "UE4SS.dll"), "rb").read(), b"UE4SS-1.0.1")
        self.assertEqual(open(cfgp).read(), "volume=0.2\n")
        # offline: the game still starts
        self.srv.shutdown()
        self.srv.server_close()
        os.remove(self.launchlog)
        r = self.run_exe("play")
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("Update check skipped", r.stdout)
        self.assertTrue(os.path.exists(self.launchlog))
        r = self.run_exe("uninstall")
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertEqual(sorted(os.listdir(self.win64)), ["SCUM.exe"])

    def test_launcher_updates_itself_from_the_release_asset(self):
        self.assertEqual(self.run_exe("install").returncode, 0)
        installed = os.path.join(self.home, "SabrPlay")
        newexe = os.path.join(self.tmp, "SabrPlay-new-%s.exe" % os.urandom(2).hex())
        shutil.copy(self.exe, newexe)
        with open(newexe, "ab") as f:
            f.write(b"\0different")
        self.release("1.0.0", launcher=newexe)
        r = subprocess.run([installed, "play", "--yes", "--home", self.home, "--dry-launch", "--server", self.base], capture_output=True, text=True, env=self.env)
        self.assertIn("Launcher updated", r.stdout + r.stderr)
        self.assertEqual(open(installed, "rb").read(), open(newexe, "rb").read())

    def test_tampered_zip_is_refused(self):
        with open(os.path.join(self.relroot, "sabr-modpack.zip"), "ab") as f:
            f.write(b"x")
        r = self.run_exe("install")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("hash mismatch", r.stdout)
        self.assertEqual(os.listdir(self.win64), ["SCUM.exe"])

    def test_default_source_is_github_and_unreachable_primary_falls_back(self):
        # no --server: the status command reports the public GitHub release as the source (no network needed)
        r = self.run_exe("status", server=False)
        self.assertIn("https://github.com/psbrowand/sabr-play/releases/latest/download", r.stdout)
        # primary dead (connection refused) + a LAN-style hub as --fallback
        pack = os.path.join(self.tmp, "pack-1.0.0")
        lan, lanbase = self.serve(pack)  # bundle.json + files/ layout straight from the build_modpack output
        dead = subprocess.run([self.exe, "install", "--yes", "--home", self.home, "--desktop", self.desk, "--startmenu", self.sm,
                               "--server", "http://127.0.0.1:9", "--fallback", lanbase], capture_output=True, text=True, env=self.env, timeout=120)
        self.assertEqual(dead.returncode, 0, dead.stdout)
        self.assertIn("Mod source: fallback server", dead.stdout)
        self.assertTrue(os.path.isfile(os.path.join(self.win64, "Mods", "SabrVoice", "scripts", "main.lua")))


if __name__ == "__main__":
    unittest.main()
