#!/usr/bin/env python3
"""manifest.json (docs/lore/tools/voice) -> voice/lines.tsv for sabrvoice.dll (no JSON parser needed in the DLL).
columns: id, speaker, speaker_name, dynamic(0|1), file (bare name under audio/), duration_s, text-or-template"""
import json
import sys


def rows(manifest: dict):
    for l in manifest["lines"]:
        tpl = l.get("template") or l.get("text") or ""
        clean = lambda s: str(s).replace("\t", " ").replace("\r", " ").replace("\n", " ")
        yield "\t".join([clean(l["id"]), clean(l.get("speaker", "")), clean(l.get("speaker_name", "")), "1" if l.get("dynamic") else "0",
                         clean(l["file"].split("/")[-1]), "%.2f" % float(l.get("duration_s") or 0), clean(tpl)])


def convert(manifest_path: str, out_path: str) -> int:
    with open(manifest_path, encoding="utf-8-sig") as f:
        m = json.load(f)
    with open(out_path, "w", encoding="utf-8", newline="\n") as o:
        o.write("# sabrvoice lines.tsv (generated from manifest.json; do not edit)\n")
        n = 0
        for r in rows(m):
            o.write(r + "\n")
            n += 1
    return n


if __name__ == "__main__":
    print(convert(sys.argv[1], sys.argv[2]), "lines")
