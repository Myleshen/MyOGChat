# Phase 4 — Live verification (deployed instance)

**Date:** 2026-10-10
**Target:** `https://chat.example.com` (Pi `raspberrypi.local`, `~/softwares/MyOGChat/myogchat`)
**Method:** live black-box verification against the running service; a temporary user
`p4verify` was created and removed. No code was modified or deployed.
**Deployed revision:** HEAD of `main` includes `c4ab9e7 feat(4.5a): document extraction`,
`444b6c3 feat(4.5b): transcribe audio/video attachments` (per the Pi's startup log:
`ocr=true stt=true tts=true imagegen_url=http://comfyui.local:8188 server_key=true`).

## Summary

| Test | Feature | Result |
|---|---|---|
| A | Document extraction (`.docx`) | ✅ **PASS** |
| B | Media transcription (TTS→WAV→STT) | ✅ **PASS** |
| C | Image-prompt enhancement (`/api/images/enhance`) | ✅ **PASS** (after one transient upstream timeout) |
| D | Tool-call I/O persistence (`tools[]`) | ✅ **PASS** (after one deploy-interrupted attempt) |
| E | Cleanup / cascade delete | ✅ **PASS** |

Two environmental anomalies were observed (a concurrent deploy/restart loop, and one
transient tunnel 502). Details in **Anomalies** below.

---

## Setup

Temp user created via the app CLI (admin=false):

```
ssh pi@raspberrypi.local '~/softwares/MyOGChat/myogchat user add --username p4verify --email p4verify@example.com --password testpass12345'
# → created user p4verify (p4verify@example.com) admin=false
```

Login and cookie jar:

```
J=/tmp/opencode/p4verify.jar; rm -f $J
curl -s -c $J -o /dev/null -X POST https://chat.example.com/api/auth/login \
  -d '{"identifier":"p4verify","password":"testpass12345"}'
# → HTTP 204; cookie jar populated (HttpOnly `myogchat=…` for chat.example.com)
```

---

## TEST A — document extraction (`.docx`) — PASS

A minimal OOXML `.docx` was built with Python's `zipfile`:

```
zipfile.ZipFile('/tmp/opencode/p4.docx','w',ZIP_DEFLATED) with entries:
  [Content_Types].xml   (docx override)
  _rels/.rels           (officeDocument → word/document.xml)
  word/document.xml     (two <w:p> with runs "Alpha paragraph one", "Beta paragraph two")
```

Command:

```
curl -s -b $J -w "\nHTTP=%{http_code}\n" -F "file=@/tmp/opencode/p4.docx" \
  https://chat.example.com/api/attachments
```

Observed (HTTP **201**):

```json
{"chars":38,"id":"a4e14697-6414-4349-871e-b6c00cfd8c91","mime":"application/octet-stream","name":"p4.docx","size":933}
```

`chars=38` and no error. (19 + 18 chars + 1 paragraph newline = 38, matching the two
paragraphs.) `GET /api/attachments/{id}` → 200, `chars=38`, no `text` field exposed
(owner-only metadata endpoint), consistent with the contract.

**PASS** — the OOXML dispatcher routes a generic `application/octet-stream` `.docx`
correctly and extracts text synchronously.

---

## TEST B — media transcription (TTS → WAV → STT) — PASS

Synthesize speech via the app's TTS:

```
curl -s -b $J -o /tmp/opencode/p4.wav -X POST -H 'Content-Type: application/json' \
  -d '{"text":"The quick brown fox jumps over the lazy dog"}' \
  https://chat.example.com/api/tts
```

Observed: HTTP **200**, `size=109612`, body is a valid WAV:

```
/tmp/opencode/p4.wav: RIFF (little-endian) data, WAVE audio, Microsoft PCM, 16 bit, mono 22050 Hz
00000000: 5249 4646 24ac 0100 5741 5645 666d 7420   RIFF$...WAVEfmt
```

Upload the WAV as an attachment (STT path; `--max-time 180`):

```
curl -s -b $J --max-time 180 -w "\nHTTP=%{http_code} time=%{time_total}s\n" \
  -F "file=@/tmp/opencode/p4.wav" https://chat.example.com/api/attachments
```

Observed (HTTP **201**, `time=1.389s`):

```json
{"chars":44,"id":"43a76cb8-3263-4a3a-8c69-c5ae5b12fc41","mime":"application/octet-stream","name":"p4.wav","size":109612}
```

`chars=44` = the 43-character phrase + trailing newline. The STT round-trip is
correct and fast (~1.4 s on the Pi), i.e. the media→STT path (`internal/media` →
`internal/speech`) works end-to-end for a WAV upload.

**PASS** — audio attachment transcribed to the expected text with no error.

---

## TEST C — image prompt enhancement — PASS (after one transient timeout)

Command:

```
curl -s -b $J -X POST -H 'Content-Type: application/json' \
  -d '{"brief":"tom and jerry with pluto"}' \
  https://chat.example.com/api/images/enhance
```

**Attempt 1** (HTTP 200, `time=30.02s`) — fell back to the deterministic template:

```json
{"enhanced":false,"original":"tom and jerry with pluto",
 "prompt":"tom and jerry with pluto, highly detailed, vivid colors, soft cinematic lighting, sharp focus, clean composition, appealing, high quality, 4k"}
```

Pi log for that request:

```
level=WARN msg="image prompt enhance failed"
  error="Post \"https://opencode.ai/zen/go/v1/chat/completions\": context deadline exceeded"
msg=request method=POST path=/api/images/enhance status=200 ms=30008
```

**Attempt 2** (HTTP 200, `time=8.59s`) — genuine LLM enhancement:

```json
{"enhanced":true,"original":"tom and jerry with pluto",
 "prompt":"Classic 1950s hand-painted cel animation still: a plump gray-and-white tuxedo cat with round yellow eyes and white gloves leaps mid-air, claws out, mouth open in a comical yowl, while a tiny cheerful brown mouse with big round ears grins mischievously and darts past him; beside them a friendly bright yellow-orange hound dog with long floppy black ears, a red collar and a wagging tail bounds forward, tongue lolling happily. They race across a sunlit checkered kitchen floor toward a wooden table, with a cozy vintage suburban home interior behind them, warm gouache-painted background of soft mint walls and a window with lace curtains. Flat cel-shaded characters with bold black outlines over painterly watercolor scenery, golden afternoon light streaming in, palette of cream, mustard yellow, mint green and tomato red, dynamic wide-angle action composition, squash-and-stretch exaggeration, high-detail nostalgic cartoon illustration, crisp, vibrant, masterpiece quality."}
```

`server_key=true` (from the startup log), so the enhancer had a key. The first call's
`context deadline exceeded` is a transient upstream latency event (30 s client timeout);
the retry produced a rich, slot-covered SD-Turbo prompt (`enhanced:true`).
The `enhanced:false` template fallback behaved as designed when the upstream failed.

**PASS** — enhancement works and returns a substantially richer prompt; the fallback
path also works. See Anomaly 2 for the transient timeout.

---

## TEST D — tool-call I/O persistence — PASS (after one deploy-interrupted attempt)

Create a chat-dialect conversation and send a tool-triggering message (SSE):

```
CID=$(curl -s -b $J -X POST -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash"}' https://chat.example.com/api/conversations \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

curl -s -N -b $J --max-time 180 -X POST -H 'Content-Type: application/json' \
  -d '{"content":"Use web search to find the official Rust programming language website, then reply with just the URL."}' \
  "https://chat.example.com/api/conversations/$CID/messages"
```

SSE wire (event counts: `meta`×1, `text`×8, `tool`×2, `usage`×1, `done`×1). The tool
events carry the Phase 4.1 fields — `args` on `running`, `output`+`duration_ms` on `done`:

```
event: tool
data: {"name":"web_search","status":"running","args":"{\"query\": \"official Rust programming language website\"}"}

event: tool
data: {"name":"web_search","status":"done","summary":"Rust (programming language) - Wikipedia — https://en.wikipedia.org/wiki/Rust_(pr",
      "output":"Rust (programming language) - Wikipedia — … (Wikipedia/Rust Foundation/r/rust text) …","duration_ms":6834}

event: usage
data: {"prompt_tokens":4504,"completion_tokens":51,"cached_tokens":1664,"reasoning_tokens":0,"cost_micros":462,"duration_ms":26}

event: done
data: {"message_id":"71ae3d46-ad02-40d7-becd-fcda248ef14d","title":"Finding Rust Official Website URL"}
```

`GET /api/conversations/{CID}` → 200. The assistant message persists `content` and a
`tools[]` entry with **`args`, `output`, and `duration_ms`** (full output preserved, no
`truncated` flag since it is under the cap):

```json
{
  "role": "assistant",
  "content": "https://www.rust-lang.org",
  "tools": [
    {
      "name": "web_search",
      "status": "done",
      "summary": "Rust (programming language) - Wikipedia — https://en.wikipedia.org/wiki/Rust_(pr",
      "args": "{\"query\": \"official Rust programming language website\"}",
      "output": "Rust (programming language) - Wikipedia — https://en.wikipedia.org/wiki/Rust_(programming_language)\nRust Foundation\n … (4,157 chars total) …",
      "duration_ms": 6834
    }
  ]
}
```

**Attempt 1 FAILED** — the turn happened to coincide with a concurrent deploy. The SSE
stream ended right after the tool `done` event with **no `usage`/`done`**, and the
assistant message was never persisted (only the user row remained). Pi log:

```
22:40:29 msg="shutting down"
22:40:34 level=ERROR msg=fatal error="context deadline exceeded"
22:40:34 systemd: Main process exited, code=exited, status=1/FAILURE
```

The retry (after the service came back up) completed cleanly as shown above.

**PASS** — tool arguments, output, and per-tool duration round-trip through the DB and
are served by `GET /api/conversations/{id}`.

---

## TEST E — cleanup / cascade delete — PASS

```
ssh pi@raspberrypi.local '~/softwares/MyOGChat/myogchat user rm --username p4verify'
# → removed user p4verify ; rm_exit=0
```

Verified with a read-only SQLite query over SSH (`sqlite3` is not installed; used
Python's `sqlite3` in URI read-only mode) *after* removal:

```
p4verify users left: [(0,)]
orphan attachments:  [(0,)]
orphan conversations:[(0,)]
orphan messages:     [(0,)]
```

`~/softwares/MyOGChat/myogchat user list` now shows only `Myleshen`. Attachment,
conversation, and message rows owned by the temp user were cascade-deleted
(`ON DELETE CASCADE`), leaving no orphans.

**PASS.**

---

## Anomalies & follow-ups

1. **Concurrent deploy / restart loop during verification (environmental).**
   Between ~22:25 and ~22:40 the service logged repeated graceful `"shutting down"`
   (SIGTERM) followed by `"myogchat starting"` roughly every 2–5 minutes — consistent
   with a parallel session repeatedly running `deploy/deploy.sh`. This is not a Phase 4
   code defect, but it exposed two robustness gaps worth a follow-up:
   - A deploy during an active chat turn **aborts the turn**: the user message is
     saved, no assistant reply is added, and the SSE stream ends without `usage`/`done`.
   - Graceful shutdown has a hard **5 s** deadline (`main.go:775`,
     `httpSrv.Shutdown(shutdownCtx)`). With an in-flight SSE request it is exceeded and
     the process exits non-zero (`status=1/FAILURE`) instead of a clean stop. Consider
     draining/cancelling in-flight SSE streams before `Shutdown`, or a longer/stream-aware
     shutdown context.
2. **Transient upstream timeout in `/api/images/enhance`.**
   First call took 30.0 s and hit `context deadline exceeded` → template fallback
   (`enhanced:false`); immediate retry succeeded in 8.6 s (`enhanced:true`). The
   fallback behaviour is correct, but the `enhance` upstream timeout (30 s) can surface
   as a silent quality downgrade. Worth confirming `mimo-v2.6-flash`
   (`defaultEnhanceModel`) latency and possibly a retry-once on timeout.
3. **Transient tunnel 502.** One `GET /api/conversations/{id}` returned
   `502 Bad Gateway` from `openresty` (Cloudflare tunnel) while the app was healthy; the
   next GET returned 200. Client/tunnel-level blip, not app-side.

## Notes

- No secrets were printed or transmitted; all responses above are app-generated content
  intended for the requesting user (owner-scoped).
- The temp user `p4verify` and all its rows were removed; the Pi is back to a single
  user (`Myleshen`).
