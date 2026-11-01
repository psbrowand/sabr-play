#!/usr/bin/env bash
# Build sabrvoice.dll (Windows x64, in-process voice player) and the native test CLI (Linux).
#   needs: python3 -m ziglang (pip install ziglang) or any x86_64-w64-mingw32 gcc/clang; gcc for the Linux CLI
set -euo pipefail
cd "$(dirname "$0")"
OUT=${1:-build}; mkdir -p "$OUT"
ZIG=${ZIG:-"python3 -m ziglang"}
FLAGS="-O2 -fno-sanitize=undefined -DNDEBUG -D_CRT_SECURE_NO_WARNINGS"
# Windows DLL: no CRT dependencies beyond msvcrt/ucrt; static link of everything else; WASAPI via miniaudio (COM resolved at runtime)
$ZIG cc -target x86_64-windows-gnu $FLAGS -shared -o "$OUT/sabrvoice.dll" sabrvoice.c stbv.c -lole32 -luuid -lwinmm -ladvapi32 -luser32 -lkernel32
# Linux test CLI (null audio backend)
gcc -O1 -g -DSV_CLI -DNDEBUG -o "$OUT/sv" sabrvoice.c stbv.c -lm -lpthread -ldl
gcc -O1 -g -shared -fPIC -DNDEBUG -o "$OUT/sabrvoice.so" sabrvoice.c stbv.c -lm -lpthread -ldl      # Linux twin of the DLL, loaded by the Lua test
ls -l "$OUT"
