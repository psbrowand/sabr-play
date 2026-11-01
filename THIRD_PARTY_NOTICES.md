# Third-party components

The release assets (`sabr-modpack.zip`) and this source tree include or build on:

| Component | Used for | License |
|---|---|---|
| [UE4SS](https://github.com/UE4SS-RE/RE-UE4SS) v3.0.1 Beta (`UE4SS.dll`, `dwmapi.dll` proxy) - Copyright (c) 2022 Narknon and contributors | Lua mod loader for Unreal Engine games | MIT |
| [miniaudio](https://miniaud.io) (`tools/sabrvoice-native/miniaudio.h`) | audio playback inside `sabrvoice.dll` | public domain or MIT-0 |
| [stb_vorbis](https://github.com/nothings/stb) (`tools/sabrvoice-native/stb_vorbis.c`) | Ogg Vorbis decoding | public domain / MIT |
| [Kokoro-82M](https://huggingface.co/hexgrad/Kokoro-82M) text-to-speech | the generated voice clips (`Mods/SabrVoice/voice/audio/*.ogg`) | Apache-2.0 |

UE4SS is MIT licensed; its copyright notice and permission notice are:

> MIT License - Copyright (c) 2022 Narknon. Permission is hereby granted, free of charge, to any person obtaining a copy of this software and
> associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use,
> copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is
> furnished to do so, subject to the following conditions: The above copyright notice and this permission notice shall be included in all copies
> or substantial portions of the Software. THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND.

SCUM is a trademark of its respective owners. This project is an unofficial fan project and is not affiliated with or endorsed by the SCUM developers or publisher.
