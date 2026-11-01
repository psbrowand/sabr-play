/* sabrvoice - in-process NPC voice player for SCUM, loaded by the SabrVoice UE4SS Lua mod.  No separate app, no service, no autostart.
 *
 *   Lua (UE4SS)  --appends "<ms>\tCHAT\t<text>" / "PLAY\t<line_id>" to <ModDir>\data\queue.txt-->  this DLL (a thread inside SCUM.exe)
 *   matches the text to a pre-rendered clip (voice\lines.tsv + voice\audio\*.ogg), decodes it (stb_vorbis) and plays it (miniaudio, WASAPI).
 *
 * Why a file queue: it needs nothing from the Lua C API, so the DLL has no import on UE4SS/Lua internals and cannot break when UE4SS changes.
 * The Lua side calls package.loadlib(<this dll>, "sabrvoice_start") once; that function ignores its argument, starts one worker thread and returns.
 * The matcher is a C twin of agent/framework/voice.py VoiceIndex (parity-tested on every line in every chat shape).
 *
 * Layout:  <ModDir>\scripts\sabrvoice.dll   <ModDir>\data\{queue.txt,config.ini,sabrvoice.log}   <ModDir>\voice\{lines.tsv,audio\*.ogg}
 * Build:   see tools/sabrvoice-native/build.sh        Test CLI: -DSV_CLI (match / matchbatch / decode / decodeall / render / run)
 */
#define _CRT_SECURE_NO_WARNINGS
#define _FILE_OFFSET_BITS 64
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdarg.h>
#include <math.h>
#include <time.h>

#ifdef _WIN32
#include <windows.h>
#define SV_EXPORT __declspec(dllexport)
#else
#include <pthread.h>
#include <unistd.h>
#include <dlfcn.h>
#include <sys/stat.h>
#define SV_EXPORT __attribute__((visibility("default")))
#endif

#define MA_NO_DECODING
#define MA_NO_ENCODING
#define MA_NO_GENERATION
#define MA_NO_RESOURCE_MANAGER
#define MA_NO_NODE_GRAPH
#define MA_NO_ENGINE
#define MA_NO_THREADING_NONE
#define MA_ENABLE_ONLY_SPECIFIC_BACKENDS
#ifdef _WIN32
#define MA_ENABLE_WASAPI
#define MA_ENABLE_DSOUND
#define MA_ENABLE_WINMM
#endif
#define MA_ENABLE_NULL
#define MINIAUDIO_IMPLEMENTATION
#include "miniaudio.h"

extern int stb_vorbis_decode_memory(const unsigned char *mem, int len, int *channels, int *sample_rate, short **output);

#define SV_VERSION "0.3.0"
#define OUT_RATE 48000
#define OUT_CH 2
#define MAX_QUEUE 8
#define DEDUPE_MS 4000

/* ------------------------------------------------------------------ portability */
static void sv_sleep_ms(int ms) {
#ifdef _WIN32
    Sleep((DWORD)ms);
#else
    usleep((useconds_t)ms * 1000);
#endif
}
static int64_t sv_now_ms(void) {
#ifdef _WIN32
    return (int64_t)GetTickCount64();
#else
    struct timespec ts; clock_gettime(CLOCK_MONOTONIC, &ts); return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
#endif
}
#ifdef _WIN32
static wchar_t *u8w(const char *s) {
    int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, NULL, 0);
    wchar_t *w = (wchar_t *)malloc((size_t)(n > 0 ? n : 1) * sizeof(wchar_t));
    if (n > 0) MultiByteToWideChar(CP_UTF8, 0, s, -1, w, n); else w[0] = 0;
    return w;
}
#endif
static FILE *sv_fopen(const char *path, const char *mode) {
#ifdef _WIN32
    wchar_t *w = u8w(path), *m = u8w(mode);
    FILE *f = _wfopen(w, m);
    free(w); free(m);
    return f;
#else
    return fopen(path, mode);
#endif
}
static unsigned char *sv_readfile(const char *path, long *len) {
    FILE *f = sv_fopen(path, "rb");
    if (!f) return NULL;
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    if (n < 0 || n > 64L * 1024 * 1024) { fclose(f); return NULL; }
    unsigned char *b = (unsigned char *)malloc((size_t)n + 1);
    if (!b) { fclose(f); return NULL; }
    size_t got = fread(b, 1, (size_t)n, f);
    fclose(f);
    b[got] = 0;
    *len = (long)got;
    return b;
}
static long sv_filesize(const char *path) {
    FILE *f = sv_fopen(path, "rb");
    if (!f) return -1;
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fclose(f);
    return n;
}

/* ------------------------------------------------------------------ log */
static char g_logpath[1024];
static void sv_log(const char *fmt, ...) {
    if (!g_logpath[0]) return;
    FILE *f = sv_fopen(g_logpath, "ab");
    if (!f) return;
    time_t t = time(NULL);
    struct tm tmv;
#ifdef _WIN32
    localtime_s(&tmv, &t);
#else
    localtime_r(&t, &tmv);
#endif
    char ts[32];
    strftime(ts, sizeof ts, "%Y-%m-%d %H:%M:%S", &tmv);
    fprintf(f, "%s ", ts);
    va_list ap;
    va_start(ap, fmt);
    vfprintf(f, fmt, ap);
    va_end(ap);
    fputc('\n', f);
    fclose(f);
}

/* ------------------------------------------------------------------ text normalisation (twin of voice.py norm / _strip_variants) */
static size_t sv_norm(const char *in, char *out, size_t cap) {
    size_t o = 0;
    int pend_space = 0;
    const unsigned char *s = (const unsigned char *)in;
    while (*s && o + 2 < cap) {
        unsigned char c = *s;
        char rep[4] = {0, 0, 0, 0};
        int adv = 1;
        if (c == 0xE2 && s[1] == 0x80) {
            switch (s[2]) {
            case 0x99: case 0x98: rep[0] = '\''; adv = 3; break;
            case 0x9C: case 0x9D: rep[0] = '"'; adv = 3; break;
            case 0xA6: rep[0] = ' '; adv = 3; break;      /* ellipsis -> "..." -> spaces */
            case 0x94: case 0x93: rep[0] = '-'; adv = 3; break;
            default: break;
            }
        }
        if (!rep[0]) rep[0] = (char)c;
        char r = rep[0];
        if (r >= 'A' && r <= 'Z') r = (char)(r - 'A' + 'a');
        if (r == '\'') { s += adv; continue; }                /* apostrophes vanish: don't -> dont */
        if ((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
            if (pend_space && o > 0) out[o++] = ' ';
            pend_space = 0;
            out[o++] = r;
        } else {
            pend_space = 1;
        }
        s += adv;
    }
    out[o] = 0;
    return o;
}

typedef struct { char hint[96]; char text[1024]; } Variant;
static int variant_has(const Variant *v, int n, const char *hint, const char *text) {
    for (int i = 0; i < n; i++) if (!strcmp(v[i].hint, hint) && !strcmp(v[i].text, text)) return 1;
    return 0;
}
static int isws(unsigned char c) { return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v'; }
static int namechar(unsigned char c) {
    return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == ' ' || c == '.' || c == '\'' || c == '-';
}
/* r"^\s*\[([^\]]{1,40})\]\s*" -> returns bytes consumed or 0 */
static size_t m_tag(const char *s, char *inner, size_t cap) {
    const char *p = s;
    while (isws((unsigned char)*p)) p++;
    if (*p != '[') return 0;
    p++;
    const char *st = p;
    while (*p && *p != ']') p++;
    size_t n = (size_t)(p - st);
    if (*p != ']' || n < 1 || n > 40) return 0;
    if (n >= cap) n = cap - 1;
    memcpy(inner, st, n); inner[n] = 0;
    p++;
    while (isws((unsigned char)*p)) p++;
    return (size_t)(p - s);
}
/* r"^\s*([A-Za-z][\w .'\-]{0,30}?):\s+" */
static size_t m_colon(const char *s, char *inner, size_t cap) {
    const char *p = s;
    while (isws((unsigned char)*p)) p++;
    if (!((*p >= 'a' && *p <= 'z') || (*p >= 'A' && *p <= 'Z'))) return 0;
    const char *st = p;
    p++;
    while (namechar((unsigned char)*p) && (size_t)(p - st) < 31) p++;
    if (*p != ':') return 0;
    size_t n = (size_t)(p - st);
    p++;
    if (!isws((unsigned char)*p)) return 0;
    while (isws((unsigned char)*p)) p++;
    if (n >= cap) n = cap - 1;
    memcpy(inner, st, n); inner[n] = 0;
    return (size_t)(p - s);
}
static int strip_variants(const char *raw, Variant *out, int maxv) {
    int n = 0;
    snprintf(out[n].hint, sizeof out[n].hint, "%s", "");
    snprintf(out[n].text, sizeof out[n].text, "%.1023s", raw);
    n++;
    char cur[1024];
    snprintf(cur, sizeof cur, "%.1023s", raw);
    for (int i = 0; i < 3 && n < maxv; i++) {
        char inner[128], hint[96];
        size_t c = m_tag(cur, inner, sizeof inner);
        if (!c) c = m_colon(cur, inner, sizeof inner);
        if (!c) break;
        sv_norm(inner, hint, sizeof hint);
        memmove(cur, cur + c, strlen(cur + c) + 1);
        if (!variant_has(out, n, hint, cur)) {
            snprintf(out[n].hint, sizeof out[n].hint, "%s", hint);
            snprintf(out[n].text, sizeof out[n].text, "%.1023s", cur);
            n++;
        }
    }
    return n;
}

/* ------------------------------------------------------------------ the index */
typedef struct {
    char id[96], speaker[64], speaker_name[64], file[96];
    int dynamic;
    char key[1024];          /* static: normalised text; dynamic: unused */
    char **segs; int nseg;   /* dynamic: normalised literal parts between {tokens} */
    double dur;
} SvLine;
typedef struct { SvLine *v; int n; } SvIndex;

static void split_template(const char *tpl, SvLine *l) {
    /* regex \{[a-z_]+\} */
    int cap = 4; l->segs = (char **)calloc((size_t)cap, sizeof(char *)); l->nseg = 0;
    const char *p = tpl, *start = tpl;
    char buf[1024];
    for (;;) {
        const char *q = strchr(p, '{');
        int ok = 0; const char *end = NULL;
        while (q) {
            const char *r = q + 1;
            while ((*r >= 'a' && *r <= 'z') || *r == '_') r++;
            if (r > q + 1 && *r == '}') { ok = 1; end = r + 1; break; }
            q = strchr(q + 1, '{');
        }
        size_t len = ok ? (size_t)(q - start) : strlen(start);
        if (len > sizeof buf - 1) len = sizeof buf - 1;
        memcpy(buf, start, len); buf[len] = 0;
        if (l->nseg == cap) { cap *= 2; l->segs = (char **)realloc(l->segs, (size_t)cap * sizeof(char *)); }
        char nb[1024]; sv_norm(buf, nb, sizeof nb);
        l->segs[l->nseg++] = strdup(nb);
        if (!ok) break;
        start = p = end;
    }
}
static void index_free(SvIndex *ix) {
    for (int i = 0; i < ix->n; i++) { for (int k = 0; k < ix->v[i].nseg; k++) free(ix->v[i].segs[k]); free(ix->v[i].segs); }
    free(ix->v); ix->v = NULL; ix->n = 0;
}
/* lines.tsv:  id \t speaker \t speaker_name \t dynamic(0|1) \t file \t duration \t text-or-template   ('#' comment lines allowed) */
static int index_load(SvIndex *ix, const char *tsv_path) {
    long len; unsigned char *buf = sv_readfile(tsv_path, &len);
    if (!buf) return -1;
    int cap = 64; ix->v = (SvLine *)calloc((size_t)cap, sizeof(SvLine)); ix->n = 0;
    char *p = (char *)buf;
    while (*p) {
        char *e = strchr(p, '\n'); if (e) *e = 0;
        size_t ll = strlen(p); if (ll && p[ll - 1] == '\r') p[ll - 1] = 0;
        if (*p && *p != '#') {
            char *f[7]; int nf = 0; char *c = p;
            f[nf++] = c;
            while (nf < 7 && (c = strchr(c, '\t'))) { *c++ = 0; f[nf++] = c; }
            if (nf == 7) {
                if (ix->n == cap) { cap *= 2; ix->v = (SvLine *)realloc(ix->v, (size_t)cap * sizeof(SvLine)); }
                SvLine *l = &ix->v[ix->n]; memset(l, 0, sizeof *l);
                snprintf(l->id, sizeof l->id, "%.95s", f[0]);
                snprintf(l->speaker, sizeof l->speaker, "%.63s", f[1]);
                snprintf(l->speaker_name, sizeof l->speaker_name, "%.63s", f[2]);
                l->dynamic = atoi(f[3]);
                snprintf(l->file, sizeof l->file, "%.95s", f[4]);
                l->dur = atof(f[5]);
                if (l->dynamic) split_template(f[6], l); else sv_norm(f[6], l->key, sizeof l->key);
                ix->n++;
            }
        }
        if (!e) break;
        p = e + 1;
    }
    free(buf);
    return ix->n;
}

#define MAXW 24
static int split_words(const char *s, char w[MAXW][48]) {
    int n = 0;
    while (*s && n < MAXW) {
        while (*s == ' ') s++;
        if (!*s) break;
        int k = 0;
        while (*s && *s != ' ') { if (k < 47) w[n][k++] = *s; s++; }
        w[n][k] = 0; n++;
    }
    return n;
}
static int subset(char a[MAXW][48], int na, char b[MAXW][48], int nb) {   /* set(a) <= set(b) */
    for (int i = 0; i < na; i++) {
        int f = 0;
        for (int j = 0; j < nb; j++) if (!strcmp(a[i], b[j])) { f = 1; break; }
        if (!f) return 0;
    }
    return 1;
}
static int speaker_ok(const SvLine *l, const char *hint) {
    if (!hint[0]) return 0;
    char name[128], sp[128], tmp[128];
    sv_norm(l->speaker_name, name, sizeof name);
    snprintf(tmp, sizeof tmp, "%s", l->speaker);
    for (char *c = tmp; *c; c++) if (*c == '_') *c = ' ';
    sv_norm(tmp, sp, sizeof sp);
    if (!strcmp(hint, name) || !strcmp(hint, sp)) return 1;
    char a[MAXW][48], b[MAXW][48];
    int na = split_words(hint, a), nb = split_words(name, b);
    return na > 0 && (subset(a, na, b, nb) || subset(b, nb, a, na));
}

/* template match: segs joined by [a-z0-9 ]{0,40}? (lazy).  anchored: ^...$ ; else leftmost search.  Returns match length or -1. */
static int seg_at(const char *t, int tl, int pos, const char *seg) {
    int sl = (int)strlen(seg);
    if (pos + sl > tl) return 0;
    return memcmp(t + pos, seg, (size_t)sl) == 0;
}
static int tpl_rec(const SvLine *l, int i, const char *t, int tl, int pos, int anchored_end, int *end) {
    /* segment i has been matched ending at pos */
    if (i == l->nseg - 1) {
        if (anchored_end && pos != tl) return 0;
        *end = pos; return 1;
    }
    for (int g = 0; g <= 40 && pos + g <= tl; g++) {
        int p2 = pos + g;
        if (seg_at(t, tl, p2, l->segs[i + 1]) && tpl_rec(l, i + 1, t, tl, p2 + (int)strlen(l->segs[i + 1]), anchored_end, end)) return 1;
    }
    return 0;
}
static int tpl_match(const SvLine *l, const char *t, int anchored, int *mlen) {
    int tl = (int)strlen(t), end = 0;
    if (anchored) {
        if (seg_at(t, tl, 0, l->segs[0]) && tpl_rec(l, 0, t, tl, (int)strlen(l->segs[0]), 1, &end)) { *mlen = end; return 1; }
        return 0;
    }
    for (int st = 0; st <= tl; st++)
        if (seg_at(t, tl, st, l->segs[0]) && tpl_rec(l, 0, t, tl, st + (int)strlen(l->segs[0]), 0, &end)) { *mlen = end - st; return 1; }
    return 0;
}

static const SvLine *pick(const SvLine **c, int n, const char *hint) {
    for (int i = 0; i < n; i++) if (speaker_ok(c[i], hint)) return c[i];
    return n ? c[0] : NULL;
}

static const SvLine *index_match(const SvIndex *ix, const char *raw) {
    Variant *vars = (Variant *)malloc(sizeof(Variant) * 8);
    int nv = strip_variants(raw, vars, 8);
    const SvLine **cands = (const SvLine **)malloc(sizeof(SvLine *) * (size_t)(ix->n + 1));
    const SvLine *res = NULL;
    for (int vi = 0; vi < nv && !res; vi++) {
        char k[1024]; sv_norm(vars[vi].text, k, sizeof k);
        if (!k[0]) continue;
        int nc = 0;
        for (int i = 0; i < ix->n; i++) if (!ix->v[i].dynamic && !strcmp(ix->v[i].key, k)) cands[nc++] = &ix->v[i];
        res = pick(cands, nc, vars[vi].hint);
        if (res) break;
        nc = 0;
        for (int i = 0; i < ix->n; i++) { int ml; if (ix->v[i].dynamic && tpl_match(&ix->v[i], k, 1, &ml)) cands[nc++] = &ix->v[i]; }
        res = pick(cands, nc, vars[vi].hint);
    }
    if (!res) {
        char whole[1024]; sv_norm(raw, whole, sizeof whole);
        int blen = 0; const SvLine *best = NULL;
        /* static keys in first-appearance order: first line of each distinct key; strictly longer wins */
        for (int i = 0; i < ix->n; i++) {
            const SvLine *l = &ix->v[i];
            if (l->dynamic) continue;
            int first = 1;
            for (int j = 0; j < i; j++) if (!ix->v[j].dynamic && !strcmp(ix->v[j].key, l->key)) { first = 0; break; }
            if (!first) continue;
            int kl = (int)strlen(l->key);
            if (kl >= 14 && kl > blen && strstr(whole, l->key)) { best = l; blen = kl; }
        }
        for (int i = 0; i < ix->n; i++) {
            const SvLine *l = &ix->v[i];
            int ml;
            if (l->dynamic && tpl_match(l, whole, 0, &ml) && ml >= 14 && ml > blen) { best = l; blen = ml; }
        }
        res = best;
    }
    free(vars); free(cands);
    return res;
}

/* ------------------------------------------------------------------ locking */
#ifdef _WIN32
typedef CRITICAL_SECTION SvMutex;
static void mu_init(SvMutex *m) { InitializeCriticalSection(m); }
static void mu_lock(SvMutex *m) { EnterCriticalSection(m); }
static void mu_unlock(SvMutex *m) { LeaveCriticalSection(m); }
#else
typedef pthread_mutex_t SvMutex;
static void mu_init(SvMutex *m) { pthread_mutex_init(m, NULL); }
static void mu_lock(SvMutex *m) { pthread_mutex_lock(m); }
static void mu_unlock(SvMutex *m) { pthread_mutex_unlock(m); }
#endif

/* ------------------------------------------------------------------ clips + mixer */
typedef struct Clip { float *pcm; int64_t frames; char id[96]; struct Clip *next; } Clip;

static SvMutex g_mu;
static int g_mu_ready;
static Clip *q_head, *q_tail, *g_cur;
static int q_len;
static int64_t g_cur_pos;
static volatile int64_t g_last_audio_ms;
static volatile float g_volume = 0.9f;

static void clip_free(Clip *c) { if (c) { free(c->pcm); free(c); } }

/* decode an .ogg file to stereo float at 48 kHz */
static Clip *clip_load(const char *path, const char *id) {
    long len; unsigned char *buf = sv_readfile(path, &len);
    if (!buf) return NULL;
    int ch = 0, rate = 0; short *pcm = NULL;
    int frames = stb_vorbis_decode_memory(buf, (int)len, &ch, &rate, &pcm);
    free(buf);
    if (frames <= 0 || !pcm || ch < 1 || rate < 8000) { free(pcm); return NULL; }
    int64_t of = (int64_t)frames * OUT_RATE / rate;
    Clip *c = (Clip *)calloc(1, sizeof *c);
    c->pcm = (float *)malloc((size_t)of * OUT_CH * sizeof(float));
    c->frames = of;
    snprintf(c->id, sizeof c->id, "%s", id);
    for (int64_t i = 0; i < of; i++) {
        double src = (double)i * rate / OUT_RATE;
        int64_t i0 = (int64_t)src; double fr = src - (double)i0;
        int64_t i1 = i0 + 1 < frames ? i0 + 1 : i0;
        for (int k = 0; k < OUT_CH; k++) {
            int chn = k < ch ? k : 0;
            double a = pcm[i0 * ch + chn] / 32768.0, b = pcm[i1 * ch + chn] / 32768.0;
            c->pcm[i * OUT_CH + k] = (float)(a + (b - a) * fr);
        }
    }
    free(pcm);
    return c;
}

/* returns 1 while something is (or was just) playing */
static int sv_mix(float *out, uint32_t frames) {
    memset(out, 0, (size_t)frames * OUT_CH * sizeof(float));
    int active = 0;
    mu_lock(&g_mu);
    uint32_t done = 0;
    while (done < frames) {
        if (!g_cur) {
            if (!q_head) break;
            g_cur = q_head; q_head = q_head->next; if (!q_head) q_tail = NULL; q_len--;
            g_cur_pos = 0;
        }
        int64_t left = g_cur->frames - g_cur_pos;
        uint32_t n = (uint32_t)(left < (int64_t)(frames - done) ? left : (int64_t)(frames - done));
        float vol = g_volume;
        const float *src = g_cur->pcm + g_cur_pos * OUT_CH;
        float *dst = out + (size_t)done * OUT_CH;
        for (uint32_t i = 0; i < n * OUT_CH; i++) {
            float v = src[i] * vol;
            dst[i] = v > 1.f ? 1.f : (v < -1.f ? -1.f : v);
        }
        g_cur_pos += n; done += n; active = 1;
        if (g_cur_pos >= g_cur->frames) { clip_free(g_cur); g_cur = NULL; }
    }
    if (active || g_cur) g_last_audio_ms = sv_now_ms();
    mu_unlock(&g_mu);
    return active;
}

static void data_cb(ma_device *dev, void *out, const void *in, ma_uint32 n) {
    (void)dev; (void)in;
    sv_mix((float *)out, n);
}

/* ------------------------------------------------------------------ device */
static ma_context g_ctx;
static ma_device g_dev;
static int g_ctx_ok, g_dev_ok, g_dev_started;

static int dev_start(void) {
    if (g_dev_started) return 1;
    if (!g_dev_ok) {
        ma_backend be[4]; int nb = 0;
#ifdef _WIN32
        if (getenv("SABRVOICE_NULL") && getenv("SABRVOICE_NULL")[0] == '1') be[nb++] = ma_backend_null;
        else { be[nb++] = ma_backend_wasapi; be[nb++] = ma_backend_dsound; be[nb++] = ma_backend_winmm; }
#else
        be[nb++] = ma_backend_null;
#endif
        ma_context_config cc = ma_context_config_init();
        if (!g_ctx_ok) {
            if (ma_context_init(be, (ma_uint32)nb, &cc, &g_ctx) != MA_SUCCESS) { sv_log("audio: context init failed"); return 0; }
            g_ctx_ok = 1;
        }
        ma_device_config dc = ma_device_config_init(ma_device_type_playback);
        dc.playback.format = ma_format_f32;
        dc.playback.channels = OUT_CH;
        dc.sampleRate = OUT_RATE;
        dc.dataCallback = data_cb;
        dc.periodSizeInMilliseconds = 20;
        ma_result r = ma_device_init(&g_ctx, &dc, &g_dev);
        if (r != MA_SUCCESS) { sv_log("audio: device init failed (%d)", (int)r); return 0; }
        g_dev_ok = 1;
        sv_log("audio: backend=%s device=%s", ma_get_backend_name(g_ctx.backend), g_dev.playback.name);
    }
    if (ma_device_start(&g_dev) != MA_SUCCESS) { sv_log("audio: device start failed"); return 0; }
    g_dev_started = 1;
    return 1;
}
static void dev_stop(void) {
    if (g_dev_started) { ma_device_stop(&g_dev); g_dev_started = 0; }
}

/* ------------------------------------------------------------------ engine state */
static SvIndex g_ix;
static char g_moddir[1024], g_voicedir[1024], g_queue[1024], g_cfg[1024];
static volatile int g_stop, g_started;
static long g_off;
static struct { char id[96]; int64_t t; } g_recent[16];
static int g_recent_n;

static void join_path(char *out, size_t cap, const char *a, const char *b) {
    snprintf(out, cap, "%s/%s", a, b);
}

static void read_config(void) {
    long len; unsigned char *b = sv_readfile(g_cfg, &len);
    if (!b) return;
    char *p = (char *)b;
    while (*p) {
        char *e = strchr(p, '\n'); if (e) *e = 0;
        size_t ll = strlen(p); if (ll && p[ll - 1] == '\r') p[ll - 1] = 0;
        char *eq = strchr(p, '=');
        if (eq && *p != '#' && *p != ';') {
            *eq = 0; const char *k = p, *v = eq + 1;
            if (!strcmp(k, "volume")) { double d = atof(v); if (d >= 0 && d <= 1.5) g_volume = (float)d; }
            else if (!strcmp(k, "voice_dir") && *v) snprintf(g_voicedir, sizeof g_voicedir, "%s", v);
        }
        if (!e) break;
        p = e + 1;
    }
    free(b);
}

static int recently_played(const char *id) {
    int64_t now = sv_now_ms();
    for (int i = 0; i < g_recent_n; i++) if (!strcmp(g_recent[i].id, id) && now - g_recent[i].t < DEDUPE_MS) return 1;
    return 0;
}
static void remember(const char *id) {
    int slot = g_recent_n < 16 ? g_recent_n++ : 0;
    if (g_recent_n == 16) { int64_t oldest = g_recent[0].t; slot = 0; for (int i = 1; i < 16; i++) if (g_recent[i].t < oldest) { oldest = g_recent[i].t; slot = i; } }
    snprintf(g_recent[slot].id, sizeof g_recent[slot].id, "%s", id);
    g_recent[slot].t = sv_now_ms();
}

/* decode + enqueue a line; returns 1 if it will be heard */
static int sv_cue_line(const SvLine *l, const char *source) {
    if (recently_played(l->id)) { sv_log("skip (just played) %s", l->id); return 0; }
    char path[1200];
    snprintf(path, sizeof path, "%s/audio/%s", g_voicedir, l->file);
    Clip *c = clip_load(path, l->id);
    if (!c) { sv_log("cannot decode %s (%s)", l->id, path); return 0; }
    mu_lock(&g_mu);
    if (q_len >= MAX_QUEUE) { mu_unlock(&g_mu); clip_free(c); sv_log("queue full, dropped %s", l->id); return 0; }
    if (q_tail) q_tail->next = c; else q_head = c;
    q_tail = c; q_len++;
    mu_unlock(&g_mu);
    remember(l->id);
    sv_log("[play] %s (%s, %.1fs)", l->id, source, (double)c->frames / OUT_RATE);
    if (!dev_start()) return 0;
    g_last_audio_ms = sv_now_ms();
    return 1;
}

static const SvLine *find_id(const char *id) {
    for (int i = 0; i < g_ix.n; i++) if (!strcmp(g_ix.v[i].id, id)) return &g_ix.v[i];
    return NULL;
}

static void handle_line(char *ln) {
    char *kind = strchr(ln, '\t');
    if (!kind) return;
    *kind++ = 0;
    char *pay = strchr(kind, '\t');
    if (pay) *pay++ = 0; else pay = (char *)"";
    if (!strcmp(kind, "CHAT")) {
        const SvLine *l = index_match(&g_ix, pay);
        if (l) sv_cue_line(l, "chat"); else sv_log("no clip for chat: %.120s", pay);
    } else if (!strcmp(kind, "PLAY")) {
        const SvLine *l = find_id(pay);
        if (l) sv_cue_line(l, "cue"); else sv_log("unknown line id %.80s", pay);
    } else if (!strcmp(kind, "PING")) {
        sv_log("ping %.60s (v" SV_VERSION ", %d lines)", pay, g_ix.n);
    } else if (!strcmp(kind, "VOL")) {
        double d = atof(pay); if (d >= 0 && d <= 1.5) { g_volume = (float)d; sv_log("volume %.2f", d); }
    } else if (!strcmp(kind, "STOP")) {
        mu_lock(&g_mu);
        while (q_head) { Clip *n = q_head->next; clip_free(q_head); q_head = n; }
        q_tail = NULL; q_len = 0; clip_free(g_cur); g_cur = NULL;
        mu_unlock(&g_mu);
    }
}

static void poll_queue(void) {
    long sz = sv_filesize(g_queue);
    if (sz < 0) return;
    if (sz < g_off) g_off = 0;                       /* truncated / replaced */
    if (sz == g_off) return;
    FILE *f = sv_fopen(g_queue, "rb");
    if (!f) return;
    fseek(f, g_off, SEEK_SET);
    long want = sz - g_off; if (want > 65536) want = 65536;
    char *buf = (char *)malloc((size_t)want + 1);
    size_t got = fread(buf, 1, (size_t)want, f);
    fclose(f);
    buf[got] = 0;
    char *last = NULL;
    for (size_t i = 0; i < got; i++) if (buf[i] == '\n') last = buf + i;
    if (!last) { if (got >= 65536) g_off += (long)got; free(buf); return; }   /* partial line: wait for the rest */
    *last = 0;
    g_off += (long)(last - buf) + 1;
    char *p = buf;
    while (p <= last) {
        char *e = strchr(p, '\n');
        if (!e || e > last) e = last;
        *e = 0;
        size_t ll = strlen(p); if (ll && p[ll - 1] == '\r') p[ll - 1] = 0;
        if (*p) handle_line(p);
        p = e + 1;
    }
    free(buf);
}

static int sv_init_dirs(const char *moddir) {
    snprintf(g_moddir, sizeof g_moddir, "%s", moddir);
    char data[1100];
    join_path(data, sizeof data, g_moddir, "data");
    join_path(g_voicedir, sizeof g_voicedir, g_moddir, "voice");
    join_path(g_queue, sizeof g_queue, data, "queue.txt");
    join_path(g_cfg, sizeof g_cfg, data, "config.ini");
    join_path(g_logpath, sizeof g_logpath, data, "sabrvoice.log");
    if (!g_mu_ready) { mu_init(&g_mu); g_mu_ready = 1; }
    if (sv_filesize(g_logpath) > 200000) { FILE *f = sv_fopen(g_logpath, "wb"); if (f) fclose(f); }
    read_config();
    char tsv[1200];
    snprintf(tsv, sizeof tsv, "%s/lines.tsv", g_voicedir);
    int n = index_load(&g_ix, tsv);
    sv_log("sabrvoice v" SV_VERSION " starting; moddir=%s lines=%d", g_moddir, n);
    if (n <= 0) { sv_log("no voice pack (%s) - nothing will be played", tsv); return 0; }
    long qs = sv_filesize(g_queue);
    if (qs > 262144) { FILE *f = sv_fopen(g_queue, "wb"); if (f) fclose(f); qs = 0; }
    g_off = qs < 0 ? 0 : qs;                          /* never replay what was queued before we started */
    return 1;
}

static void sv_loop(void) {
    int tick = 0;
    while (!g_stop) {
        poll_queue();
        sv_sleep_ms(30);
        if (++tick % 33 == 0 && g_dev_started && sv_now_ms() - g_last_audio_ms > 4000) {
            mu_lock(&g_mu); int idle = !g_cur && !q_head; mu_unlock(&g_mu);
            if (idle) dev_stop();
        }
    }
    dev_stop();
}

#ifdef _WIN32
static DWORD WINAPI worker_main(LPVOID p) { (void)p; sv_loop(); return 0; }
static int thread_spawn(void) { HANDLE h = CreateThread(NULL, 0, worker_main, NULL, 0, NULL); if (!h) return 0; CloseHandle(h); return 1; }
#else
static void *worker_main(void *p) { (void)p; sv_loop(); return NULL; }
static int thread_spawn(void) { pthread_t t; if (pthread_create(&t, NULL, worker_main, NULL)) return 0; pthread_detach(t); return 1; }
#endif

static int sv_start_dir(const char *moddir) {
    if (g_started) return 1;
    g_started = 1;
    if (!sv_init_dirs(moddir)) { /* keep the thread out of it: without a pack there is nothing to do */ return 0; }
    if (!thread_spawn()) { sv_log("cannot start worker thread"); return 0; }
    return 1;
}

#ifdef _WIN32
static void module_dir(char *out, size_t cap) {
    HMODULE h = NULL;
    out[0] = 0;
    if (!GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, (LPCWSTR)&module_dir, &h)) return;
    wchar_t wp[1024];
    DWORD n = GetModuleFileNameW(h, wp, 1024);
    if (!n || n >= 1024) return;
    char u[2048];
    WideCharToMultiByte(CP_UTF8, 0, wp, -1, u, sizeof u, NULL, NULL);
    for (char *c = u; *c; c++) if (*c == '\\') *c = '/';
    char *s = strrchr(u, '/'); if (s) *s = 0;                 /* ...\Mods\SabrVoice\scripts */
    s = strrchr(u, '/'); if (s) *s = 0;                       /* ...\Mods\SabrVoice */
    snprintf(out, cap, "%s", u);
}
#endif

#ifndef _WIN32
static void module_dir(char *out, size_t cap) {      /* Linux test build: same layout <ModDir>/scripts/sabrvoice.so */
    Dl_info di; out[0] = 0;
    if (!dladdr((void *)&module_dir, &di) || !di.dli_fname) return;
    char u[2048]; snprintf(u, sizeof u, "%s", di.dli_fname);
    char *s = strrchr(u, '/'); if (s) *s = 0;
    s = strrchr(u, '/'); if (s) *s = 0;
    snprintf(out, cap, "%s", u);
}
#endif

/* Lua entry points: lua_CFunction-shaped, argument ignored, return 0 values.  Called as  package.loadlib(dll, "sabrvoice_start")() */
/* the worker thread runs code of this module: never let the host (or a Lua GC) unload it */
static void pin_module(void) {
#ifdef _WIN32
    HMODULE h = NULL;
    GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_PIN, (LPCWSTR)&pin_module, &h);
#else
    Dl_info di;
    if (dladdr((void *)&pin_module, &di) && di.dli_fname) dlopen(di.dli_fname, RTLD_NOW | RTLD_NODELETE);
#endif
}

SV_EXPORT int sabrvoice_start(void *L) {
    (void)L;
    pin_module();
    char d[1100]; module_dir(d, sizeof d);
    if (d[0]) sv_start_dir(d);
    return 0;
}
SV_EXPORT int luaopen_sabrvoice(void *L) { return sabrvoice_start(L); }
SV_EXPORT int sabrvoice_version(void *L) { (void)L; return 3; }

#ifdef _WIN32
BOOL WINAPI DllMain(HINSTANCE h, DWORD reason, LPVOID r) {
    (void)r;
    if (reason == DLL_PROCESS_ATTACH) DisableThreadLibraryCalls(h);
    else if (reason == DLL_PROCESS_DETACH) g_stop = 1;
    return TRUE;
}
#endif

/* ------------------------------------------------------------------ test CLI */
#ifdef SV_CLI
static int cli_load(const char *pack) {
    char tsv[1200]; snprintf(tsv, sizeof tsv, "%s/lines.tsv", pack);
    int n = index_load(&g_ix, tsv);
    if (n <= 0) { fprintf(stderr, "cannot load %s\n", tsv); return 0; }
    snprintf(g_voicedir, sizeof g_voicedir, "%s", pack);
    return 1;
}
static void wav_write(FILE *f, const float *pcm, int64_t frames) {
    uint32_t dsz = (uint32_t)(frames * OUT_CH * 2), rate = OUT_RATE, br = OUT_RATE * OUT_CH * 2;
    uint16_t ch = OUT_CH, bits = 16, blk = OUT_CH * 2, fmt = 1;
    uint32_t riff = 36 + dsz, fl = 16;
    fwrite("RIFF", 1, 4, f); fwrite(&riff, 4, 1, f); fwrite("WAVEfmt ", 1, 8, f); fwrite(&fl, 4, 1, f);
    fwrite(&fmt, 2, 1, f); fwrite(&ch, 2, 1, f); fwrite(&rate, 4, 1, f); fwrite(&br, 4, 1, f); fwrite(&blk, 2, 1, f); fwrite(&bits, 2, 1, f);
    fwrite("data", 1, 4, f); fwrite(&dsz, 4, 1, f);
    for (int64_t i = 0; i < frames * OUT_CH; i++) { short s = (short)(pcm[i] * 32767.f); fwrite(&s, 2, 1, f); }
}
int main(int argc, char **argv) {
    if (!g_mu_ready) { mu_init(&g_mu); g_mu_ready = 1; }
    if (argc < 3) { fprintf(stderr, "usage: sv match|matchbatch|decode|decodeall|render|run <packdir|moddir> ...\n"); return 2; }
    const char *cmd = argv[1];
    if (!strcmp(cmd, "match")) {
        if (!cli_load(argv[2])) return 1;
        const SvLine *l = index_match(&g_ix, argc > 3 ? argv[3] : "");
        printf("%s\n", l ? l->id : "NONE"); return l ? 0 : 3;
    }
    if (!strcmp(cmd, "matchbatch")) {
        if (!cli_load(argv[2])) return 1;
        char *ln = NULL; size_t cap = 0; ssize_t n;
        while ((n = getline(&ln, &cap, stdin)) >= 0) {
            while (n > 0 && (ln[n - 1] == '\n' || ln[n - 1] == '\r')) ln[--n] = 0;
            /* the harness escapes embedded newlines/tabs; nothing to undo: lines are single-line */
            const SvLine *l = index_match(&g_ix, ln);
            printf("%s\n", l ? l->id : "NONE");
        }
        free(ln); return 0;
    }
    if (!strcmp(cmd, "decode") && argc > 3) {
        if (!cli_load(argv[2])) return 1;
        const SvLine *l = find_id(argv[3]); if (!l) { puts("unknown id"); return 1; }
        char p[1300]; snprintf(p, sizeof p, "%s/audio/%s", argv[2], l->file);
        Clip *c = clip_load(p, l->id); if (!c) { puts("DECODE FAIL"); return 1; }
        printf("%s frames=%lld secs=%.2f manifest=%.2f\n", l->id, (long long)c->frames, (double)c->frames / OUT_RATE, l->dur); clip_free(c); return 0;
    }
    if (!strcmp(cmd, "decodeall")) {
        if (!cli_load(argv[2])) return 1;
        int ok = 0, bad = 0, off = 0; double tot = 0;
        for (int i = 0; i < g_ix.n; i++) {
            char p[1300]; snprintf(p, sizeof p, "%s/audio/%s", argv[2], g_ix.v[i].file);
            Clip *c = clip_load(p, g_ix.v[i].id);
            if (!c) { bad++; printf("FAIL %s\n", g_ix.v[i].id); continue; }
            double s = (double)c->frames / OUT_RATE; tot += s; ok++;
            if (g_ix.v[i].dur > 0 && fabs(s - g_ix.v[i].dur) > 0.15) off++;
            clip_free(c);
        }
        printf("decoded=%d failed=%d total_secs=%.1f duration_mismatch=%d\n", ok, bad, tot, off);
        return bad ? 1 : 0;
    }
    if (!strcmp(cmd, "render") && argc > 4) {         /* render <pack> <out.wav> id... : queue everything, mix offline like the device callback would */
        if (!cli_load(argv[2])) return 1;
        for (int i = 4; i < argc; i++) {
            const SvLine *l = find_id(argv[i]); if (!l) { fprintf(stderr, "unknown id %s\n", argv[i]); return 1; }
            char p[1300]; snprintf(p, sizeof p, "%s/audio/%s", argv[2], l->file);
            Clip *c = clip_load(p, l->id); if (!c) return 1;
            if (q_tail) q_tail->next = c; else q_head = c; q_tail = c; q_len++;
        }
        size_t cap = 48000 * 60; float *all = (float *)malloc(cap * OUT_CH * sizeof(float)); int64_t n = 0;
        float buf[480 * OUT_CH];
        for (;;) {
            sv_mix(buf, 480);
            mu_lock(&g_mu); int more = g_cur || q_head; mu_unlock(&g_mu);
            if (n + 480 > (int64_t)cap) break;
            memcpy(all + n * OUT_CH, buf, sizeof buf); n += 480;
            if (!more) break;
        }
        FILE *f = fopen(argv[3], "wb"); wav_write(f, all, n); fclose(f);
        printf("rendered frames=%lld secs=%.2f\n", (long long)n, (double)n / OUT_RATE); return 0;
    }
    if (!strcmp(cmd, "run")) {                        /* run <moddir> [seconds]: the real worker (null audio device) */
        double secs = argc > 3 ? atof(argv[3]) : 5;
        if (!sv_start_dir(argv[2])) return 1;
        sv_sleep_ms((int)(secs * 1000));
        g_stop = 1; sv_sleep_ms(200);
        return 0;
    }
    fprintf(stderr, "bad command\n"); return 2;
}
#endif
