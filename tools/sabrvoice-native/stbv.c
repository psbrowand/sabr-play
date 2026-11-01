/* stb_vorbis lives in its own translation unit (its single-letter macros clash with other code). */
#define STB_VORBIS_NO_STDIO 1
#define STB_VORBIS_NO_PUSHDATA_API 1
#include "stb_vorbis.c"
