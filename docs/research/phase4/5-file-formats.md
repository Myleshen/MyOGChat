# Phase 4.5 — File-format support: Office docs, EPUB, and media

**Research / scoping report.** Repo `~/dev/myogchat` (single Go binary,
`CGO_ENABLED=0`, `linux/arm64` on a Raspberry Pi 4B, stdlib `net/http`, pure-Go deps only,
embedded vanilla-JS PWA). **No code was changed.**

Goal: support **office documents** (`.docx/.xlsx/.pptx` + ODF `.odt/.ods/.odp`),
**EPUB**, and **major media** (`.mp3/.mp4` and friends) through the existing
attachment → `internal/extract` → folded-text pipeline, with a format **dispatcher** and
pluggable sub-extractors.

Date: 2026-10-10 · Related: `docs/2.3-attachments.md`, `docs/2.5-ocr.md`,
`docs/research/2.13-vision-ocr.md`, `docs/3.5-3.6-speech.md`,
`docs/research/3.5-3.6-tts-stt.md`.

---

## 0. TL;DR / recommendation

- **Dispatcher first.** Keep `internal/extract` as the front door and add sub-extractors
  (`ooxml.go`, `odf.go`, `epub.go`, `rtf.go`, `media.go`) plus a magic-byte detector.
- **OOXML (`.docx/.xlsx/.pptx`), ODF (`.odt/.ods/.odp`), EPUB, RTF: stdlib only**
  (`archive/zip` + `encoding/xml`, a ~150-line HTML/RTF tag stripper). No new Go dep. These
  formats are all ZIP (OOXML/ODF/EPUB) or flat XML/text (RTF), so stdlib is genuinely
  sufficient; the Go "office" libraries are thin and mostly single-maintainer.
- **XLSX is the one place a library helps** (shared strings, inline strings, formatted
  numbers). Recommend **stdlib first**; if robustness matters, add
  `github.com/xuri/excelize/v2` (**BSD-3-Clause, pure Go, CGO-free, arm64-OK**) — but it
  drags in ~7 modules (`x/crypto`, `x/image`, `x/net`, `x/text`, `xuri/efp`, `xuri/nfp`,
  `richardlehane/mscfb`, `tiendc/go-deepcopy`). Documented as optional.
- **CSV/TSV already work** (they are in `textExtensions`, `extract.go:47`) — no work needed.
- **Media** (audio + video) → the existing **whisper.cpp STT** (`internal/speech`). Probe and
  bound with a **dedicated `ffmpeg`/`ffprobe` Docker container** (precedent: `internal/ocr`),
  built by `deploy/media-setup.sh`. **Video = extract the audio track** inside that container.
  Do **not** attempt a pure-Go decoder — AAC/Opus/Vorbis/AMR need cgo and webm/mkv/mov demux
  is a rabbit hole; ffmpeg covers everything in one place.
- **Media transcription must be asynchronous.** whisper `base.en` on the Pi is ~2×
  realtime, and the public Cloudflare Tunnel caps an origin response at **~100 s (524)**;
  a 5-minute clip cannot be transcribed inside a synchronous upload. Store the media with a
  `pending` status, return immediately, transcribe in a bounded background worker, and have
  the SPA poll `GET /api/attachments/{id}` (it already exists). Documents stay synchronous.
- **Cut for v1:** legacy binary `.doc/.xls/.ppt` (OLE), Outlook `.msg` (OLE + MAPI),
  Apple `.pages/.key/.numbers` (Snappy/protobuf IWA). Recommend a clear "unsupported" error
  and an *optional* LibreOffice-in-Docker catch-all later.
- **Effort: ~4–7 days** (documents 2–3, media 2–4). **No new Go dependency is required** for
  the core; excelize is the only optional one.

---

## 1. Current state (exact surface)

### Dispatcher

`internal/extract/extract.go:83-100` — the only routing today:

```go
func ExtractWith(name, mimeType string, data []byte, ocrFn ocr.OCRFunc) (string, error) {
	base := baseMime(mimeType)
	ext := strings.ToLower(filepath.Ext(name))

	switch {
	case isPDF(base, ext, data):
		return extractPDF(data)
	case isImage(base, ext):
		return extractImage(data, ocrFn)
	case isText(base, ext, data):
		return extractText(data)
	default:
		...
		return "", fmt.Errorf("%w: %s", ErrUnsupported, base)
	}
}
```

Supporting pieces to extend: `textExtensions` (`extract.go:45-61`), `imageExtensions`
(`:65-69`), `isPDF` (`:117-119`), `isImage` (`:123-125`), `isText` (`:149-167`), the
panic-guarded `extractPDF` pattern (`:183-210`), and `capText` (`:213-219`).

### Call site and the seam

`internal/server/attachments.go:81` is the single caller:

```go
text, err := extract.ExtractWith(name, mimeType, data, s.ocrFunc(r.Context(), u))
```

`handleUploadAttachment` (`attachments.go:32-108`) is **fully synchronous**: it reads the
part, extracts, stores the row, and returns `201`. `extractMessage` (`:155-169`) maps
sentinels (`ErrNotText`, `ErrEmpty`, `ErrOCR`, `ErrUnsupported`) to user text.

### Limits

| Limit | Value | Where |
|---|---|---|
| Per-file upload | 10 MB | `maxAttachmentBytes`, `attachments.go:18` |
| Stored text/file | 1,000,000 runes + marker | `extract.MaxTextChars`, `extract.go:26` |
| Attached-text budget | 200,000 runes total | `maxAttachedChars`, `attachments.go:23` |
| Files per message | 20 | `maxAttachmentsPerMessage`, `attachments.go:20` |

### Storage

`internal/store/store.go:73-81`:

```sql
CREATE TABLE IF NOT EXISTS attachments (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL, mime TEXT, size INTEGER NOT NULL, text TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
```

`store.Attachment` (`store.go:249-257`) has `Text string` and `Chars()` (`:260`).

### Frontend

`static/index.html:250` — `<input id="file-input" ... type="file" multiple>` has **no
`accept` filter**, so the browser already lets users pick any file. `static/app.js`:
`MAX_ATTACHMENT_BYTES`/`MAX_ATTACHMENTS` (`:3530-3531`), `uploadAttachment` (`:3552-3564`),
`onFilesSelected` (`:3612-3646`) with an optimistic `uploading:true` chip, and send gating
via `isUploading()` (`:3540-3549`).

### Precedents to mirror

- OCR container: `internal/ocr/ocr.go:149-167` (hardened `docker run`), timeout +
  `docker rm -f` cleanup (`:172-197`), `isDockerUnavailable` (`:203-225`); image built by
  `deploy/ocr-setup.sh` from `deploy/ocr/Dockerfile` (`alpine:3.20` + tesseract).
- Sandbox hardening flags: `internal/sandbox/sandbox.go:346-373`.
- STT client + `STTFunc`: `internal/speech/speech.go:82`, `:148-212`; server wiring
  `internal/server/speech.go:115-134`.
- Engine wiring in `main.go:658-717`.
- The speech research states the whisper.cpp image **bundles ffmpeg** and that whisper
  needs 16 kHz mono WAV for non-WAV input (`docs/research/3.5-3.6-tts-stt.md:100-104`,
  `:350-364`).

---

## 2. Dispatcher design

### 2.1 Detection by magic + MIME + extension

All new document containers are ZIP. Disambiguate by reading the central directory
(`archive/zip.NewReader(bytes.NewReader(data), int64(len(data)))`) and looking at entry
names — **no extraction of content at detection time**:

| Family | Zip entry that identifies it |
|---|---|
| `.docx` | `word/document.xml` (or `[Content_Types].xml` with `wordprocessingml`) |
| `.xlsx` | `xl/workbook.xml` |
| `.pptx` | `ppt/presentation.xml` |
| `.odt/.ods/.odp/.odg` | first entry `mimetype` = `application/vnd.oasis.opendocument.*` |
| `.epub` | `META-INF/container.xml` (and/or `mimetype` = `application/epub+zip`) |
| (unknown zip) | reject — preserves today's `TestExtractBinaryUnsupported` intent |

OLE compound files (`.doc/.xls/.ppt/.msg`) start with `D0 CF 11 E0 A1 B1 1A E1` → route to
`ErrUnsupported` (or the optional LibreOffice path, §6). Images (`isImage`) and PDF stay as
they are. ZIP magic is `PK\x03\x04` (also `PK\x05\x06` empty, `PK\x07\x08` spanned).

**Ordering matters:** the current `isText` fallback (`extract.go:160-163`) accepts
"valid UTF-8 with no NUL" for empty/`octet-stream` MIME. A ZIP/PDF/OLE body is *not* valid
UTF-8, so binary formats are never misrouted as text today; keep the binary detection
**before** the text fallback.

### 2.2 Interface: an options struct, not another positional arg

`ExtractWith(name, mime, data []byte, ocrFn)` cannot grow cleanly to also carry a media
transcriber. Recommend:

```go
// internal/extract
type TranscribeFunc func(data []byte, filename, mime string) (string, error) // == speech.STTFunc

type Options struct {
	OCR        ocr.OCRFunc      // images
	Transcribe TranscribeFunc   // audio/video → text (ffmpeg probe + STT); nil = unsupported
}

func Extract(name, mime string, data []byte, opts Options) (string, error) // new
func ExtractWith(name, mime string, data []byte, ocrFn ocr.OCRFunc) (string, error) {
	return Extract(name, mime, data, Options{OCR: ocrFn}) // kept for compatibility
}
```

`TranscribeFunc` is defined **inside** `extract` (mirroring how the OCR seam is a plain
func type) so `extract` never imports `internal/speech` or Docker code. The concrete
implementation (ffmpeg + STT) lives in `internal/media` and is injected by the server —
exactly the way `s.ocrFunc` injects `ocr.OCRFunc` at `attachments.go:81`. No import cycles:
`server → extract`, `server → media → speech`, `extract → ocr`.

Sub-extractors stay unexported and are unit-tested through `Extract`, matching
`extract_test.go`.

---

## 3. OOXML — `.docx` / `.xlsx` / `.pptx`

All three are ZIP + XML. Stream with `encoding/xml.Decoder.Token()` (never `Unmarshal` a
whole part) and wrap each part in an `io.LimitReader` (see §13, zip bombs). Collect text,
insert `\n` per block, `\t` per cell.

### `.docx` — `word/document.xml`

Text lives in `w:t` runs; `w:tab`→`\t`, `w:br`/`w:cr`→`\n`, `w:p`→paragraph break. A minimal
token walk:

```go
// pseudo: switch on xml.Name.Local
case "t":        if !inDelete { b.WriteString(ch.TokenData()) } // w:t (mixed)
case "tab":      b.WriteByte('\t')
case "br", "cr": b.WriteByte('\n')
case "p":        b.WriteByte('\n') // at EndElement
```

Skip `w:instrText` (field codes) and `w:del` (tracked deletions) when practical. Optionally
also read `word/header*.xml`, `word/footer*.xml`, `word/footnotes.xml` (lower priority).
OOXML text is Unicode inside the XML — no encoding work. **No library needed.**
(If a library is ever wanted: `github.com/gomutex/godocx` — MIT, pure Go.)

### `.xlsx` — `xl/workbook.xml`, `xl/worksheets/sheet*.xml`, `xl/sharedStrings.xml`

The only non-trivial spreadsheet. Read `sharedStrings.xml` into `[]string` (`<si>` → concat
`<t>`), then each sheet's `<row>`/`<c>`; a cell's value is `<v>` interpreted by `t`:
`t="s"` → sharedStrings[index], `t="inlineStr"` → `<is><t>`, `t="str"` → literal, else
numeric literal. Join cells with `\t`, rows with `\n`. Respect `r`/`c` coordinates if you
want gap-filling (optional). Sheet order from `workbook.xml` `<sheets>`.

**Recommendation:** stdlib for v1 (covers the common read case). Add
**`github.com/xuri/excelize/v2`** only if real-world files (formulas, dates, merged cells)
prove painful — it is BSD-3-Clause, pure Go, CGO-free, and cross-compiles to `arm64`, but
its module graph is the heaviest of anything proposed here (`go.mod` requires
`x/crypto`, `x/image`, `x/net`, `x/text`, `xuri/efp`, `xuri/nfp`, `richardlehane/mscfb`,
`tiendc/go-deepcopy`).

### `.pptx` — `ppt/slides/slideN.xml`

Concatenate `<a:t>` runs per slide, in slide-number order (parse `ppt/_rels/presentation.xml.rels`
or just sort `slide<N>` numerically), joined by `\n--- slide N ---\n`. Include speaker notes
(`ppt/notesSlides/notesSlideN.xml`) optionally — high value for meeting decks. **No library
needed** (Go pptx libraries are thin; `unidoc/unioffice` is **commercial-licensed** — avoid).

---

## 4. ODF — `.odt` / `.ods` / `.odp` (+ `.odg`)

ZIP with `mimetype` (stored uncompressed, first entry) + `content.xml` (the document body).
Parse `content.xml` with `encoding/xml`:

- `.odt`: `<text:p>`/`<text:h>` blocks; `<text:s/>`→space, `<text:tab/>`→`\t`,
  `<text:line-break/>`→`\n`.
- `.ods`: `<table:table>` / `<table:table-row>` / `<table:table-cell>`; cell text in nested
  `<text:p>`; numeric value in `office:value`. Join with `\t` / `\n`.
- `.odp`: `<draw:page>` per slide; `<text:p>` inside `<draw:frame>`/shapes.
- `.odg` (drawing): shapes with `<text:p>` — low value; include only if trivial (it is).

Flat variants (`.fodt/.fods/.fodp`, single XML file, not zipped) are an easy bonus: detect
`<office:document` at offset 0 and parse the same walk. Namespaced element names: match on
`Local` (`"p"`, `"h"`, `"s"`, `"tab"`, `"table-cell"`) and optionally the `Space`/namespace
to avoid collisions.

**No library needed.** (`github.com/lu4p/cat` — **Unlicense** — and
`github.com/taigrr/document-crack` — **0BSD** — both do ODF/DOCX, but both are tiny
single-maintainer repos; treat as reference, not dependencies.)

---

## 5. EPUB

EPUB 2/3 is ZIP + OPF + XHTML:

1. Read `META-INF/container.xml` → `<rootfile full-path="...opf">`.
2. Parse the OPF: `<manifest>` (id → href/media-type) and `<spine>` (ordered `idref`s).
   Resolve hrefs relative to the OPF directory (usually `OEBPS/`).
3. For each spine item with an XHTML media type, read it and strip markup to text; join
   with blank lines (optionally prefix a heading from `<title>`/`<h1>`).
4. Skip `<script>`, `<style>`, `<head>`; decode entities; collapse whitespace.

**HTML→text:** the repo has no HTML stripper today (`grep` found no `x/net/html` usage; the
`fetch_url` tool does its own thing). Add a small `internal/extract/html.go` tag-stripper
(handle `<script>/<style>` removal, block tags → newline, entities via `html.UnescapeString`
from stdlib `html`). If correctness matters later, `golang.org/x/net/html` (BSD-3) is the
robust parser; it is a pure-Go module. Either is fine — **stdlib `html` + a stripper is
enough** and keeps the dep count at zero. (Reuse the same stripper for `.html`/`.htm`
attachments, which are currently treated as *raw text* — an easy improvement.)

---

## 6. Others: cut lines

| Format | Reality | Recommendation |
|---|---|---|
| **`.rtf`** | Flat ASCII with control words, groups, `\'hh` (cp1252), `\uNNNN`, `\*` destinations. | **In-repo ~150-LOC canonicalizer** (stdlib). Handle group nesting, `\par/\line/\tab`, `\'hh` (cp1252), `\uN` (with `\ucN` skip count), drop `\*`/ignorable destinations. No dep. |
| **`.csv` / `.tsv`** | already text (`extract.go:47`) | done |
| **`.html`/`.htm`** | treated as raw text today | reuse the EPUB stripper (nice-to-have) |
| legacy `.doc/.xls/.ppt` | OLE Compound File; no stdlib parser; needs `antiword`/LibreOffice/Tika | **cut v1**; clear `ErrUnsupported`. Optional: LibreOffice-in-Docker catch-all (below). |
| **`.msg`** (Outlook) | OLE/CFB + MAPI property streams (`[MS-OXMSG]`) | **cut v1** (rare when IMAP mail reading already exists, feature 3.2). `richardlehane/mscfb` (BSD-2) only enumerates streams; MAPI decoding is real work. |
| **`.pages/.key/.numbers`** | modern iWork = ZIP + `Index/*.iwa` (Snappy-compressed protobuf) | **cut v1**. Optional: extract an embedded preview/quicklook thumbnail if present. |
| **`.odg`** | ODF drawing | include if trivial; low priority |
| **LibreOffice catch-all** | `libreoffice --headless --convert-to txt` in a container | **optional later**: one `lscr.io/linuxserver/libreoffice` (or Debian `libreoffice --headless`) container would also cover `.doc/.xls/.ppt/.pages/.msg`; ~1 GB image, ~several-second startup — too heavy for the Pi as a default, fine as an opt-in. |

---

## 7. Media — audio and video

### 7.1 Route to the existing STT

`internal/speech.OpenAISTT.Transcribe` (`speech.go:148-212`) uploads a multipart `file` to
`/v1/audio/transcriptions`; the whisper.cpp server decodes via its bundled ffmpeg. So
**audio and video can be posted almost as-is**. Audio extensions worth accepting:
`.mp3 .m4a .aac .wav .ogg .oga .opus .flac .wma .weba`; video:
`.mp4 .m4v .mov .webm .mkv .avi .wmv .flv .3gp`.

### 7.2 Why an `ffmpeg` container (not a pure-Go demuxer)

- **Bounds.** We must know the **duration** and reject/trim long media before spending
  minutes of Pi CPU. `ffprobe -show_entries format=duration` is one call and works for every
  container. Pure-Go duration reading is fragmentary: `github.com/abema/go-mp4` (MIT, pure
  Go) can read MP4 `mvhd`, stdlib can read WAV, `hajimehoshi/go-mp3` (BSD) / `mewkiz/flac`
  read their own — but **nothing pure-Go decodes AAC/Opus/Vorbis/AMR**, and demuxing
  webm/mkv/mov is not worth writing.
- **Video.** Extracting the audio track (`-vn`) is the whole job; a page of ffmpeg flags
  beats a Go muxer.
- **Precedent.** `internal/ocr` already runs a hardened, ephemeral Docker container for
  host-side tooling (`ocr.go:149-167`), so this fits the established shape.

**Design (`internal/media`, new):** mirror `internal/ocr` — an `Engine` with an injectable
`CommandRunner` (tests assert argv without Docker). Two container invocations in one
hardened `docker run` (or two runs):

```
# 1) probe: presence of audio + duration
ffprobe -v error -show_entries format=duration:stream=codec_type -of json < stdin
# 2) transcode to the STT-native format, capped at max duration
ffmpeg -nostdin -i pipe:0 -vn -ac 1 -ar 16000 -t <maxDur> -f wav pipe:1
```

Run flags (copy OCR/sandbox hardening): `--rm --network none --memory 512m --cpus 1
--pids-limit 128 --read-only --tmpfs /tmp:rw,size=64m --cap-drop ALL
--security-opt no-new-privileges --user 65534:65534 -i <image>`. Media is small enough to
pipe over stdin; output WAV over stdout (~1.9 MB/min at 16 kHz mono 16-bit, well under the
existing `maxJSONResponse`-style limits). Reuse the timeout + `docker rm -f <name>` cleanup
pattern from `ocr.go:172-197`. Reject files with **no audio stream** (`ErrEmpty`).

The recovered WAV is then handed to `speech.Transcriber.Transcribe`. A convenience
`media.TranscribeFunc(ctx, stt, maxDur)` composes probe → transcode → STT and is injected as
`extract.Options.Transcribe`.

### 7.3 Bounding cost

- **Base cap is already 10 MB** (`maxAttachmentBytes`). 10 MB of MP3 ≈ 10–17 min; 10 MB of
  video is short. Keep 10 MB, or raise modestly for media if desired.
- **Duration cap** (`MYOGCHAT_MEDIA_MAX_DURATION`, default **600 s**, clamp to [1, 3600]):
  if `ffprobe` duration exceeds it, **trim** (`-t`) to the cap and note truncation, or reject
  (config `MYOGCHAT_MEDIA_OVER_LIMIT=trim|reject`). Trimming is friendlier.
- **Container timeout** (`MYOGCHAT_MEDIA_TIMEOUT`, default **120 s**) bounds ffmpeg itself.
- **STT timeout** is already `MYOGCHAT_STT_TIMEOUT` (120 s, `speech.go:35`); raise it (e.g.
  300 s) if you allow long media, *or* rely on the async worker (§8) where the caller is a
  background goroutine, not an HTTP request.
- **Serialise** transcription: the Pi has 4 cores and whisper `base.en` pins them; a single
  worker (or a queue of 1–2) prevents thrash (`docs/research/3.5-3.6-tts-stt.md:213-224`).
- **GPU offload:** pointing `MYOGCHAT_STT_URL` at the Proxmox faster-whisper shim makes long
  media cheap; keep the async design regardless.

---

## 8. Architecture: sync documents, async media

### 8.1 Why async is required for media

`handleUploadAttachment` blocks until extraction returns (`attachments.go:81`). The SPA
uploads through the **Cloudflare Tunnel**, which caps a single origin response at **~100 s**;
slow origins get **524**. whisper `base.en` on the Pi is ~2× realtime, so anything over
~50–60 s of audio cannot be transcribed inside a synchronous upload. Documents (ZIP/XML,
sub-second) are unaffected.

### 8.2 Proposed flow

```
POST /api/attachments (media/*)
      │  store row {status:"pending", text:""}  → 202 {id,status:"pending"}
      ▼
 (background worker, bounded queue)          ┌── ffprobe duration/audio
      │  internal/media: probe → trim ────────┤
      │  → 16 kHz WAV → speak → STT            └── ffmpeg (Docker)
      ▼
 UPDATE attachments SET text=?, status='ready'   (or status='failed', error=?)
      ▲
GET /api/attachments/{id}   ← SPA polls (1–2 s) while any chip is pending
```

Documents/images keep the current synchronous behaviour (extract inline, `status='ready'`).

### 8.3 Storage + API changes (additive)

Mirror the existing additive migration style (`store.go` `migrate()`):

```sql
ALTER TABLE attachments ADD COLUMN status TEXT;   -- 'ready' | 'pending' | 'failed'
ALTER TABLE attachments ADD COLUMN error  TEXT;   -- user-safe failure message
-- backfill: UPDATE attachments SET status='ready' WHERE status IS NULL
```

- `store.Attachment` gains `Status string` and `Error string` (not JSON-exposed to other
  users; owner-only endpoints already).
- `POST /api/attachments`: `201` for sync success, `202 {id,status:"pending"}` for media.
- `GET /api/attachments/{id}` (`attachments.go:111-134`) gains `status` + `error`.
- Optional admin setting `media_enabled` (default true) + DB overrides for the caps, mirroring
  `ocr_enabled` (`internal/server/ocr.go:10-22`) and `speechSettings` (`speech.go:301-311`).
- **Crash safety:** a process restart drops in-flight jobs. On startup, mark stale `pending`
  rows `failed` with "interrupted" (or re-enqueue them). A tiny in-memory registry (map of
  id → job) is enough; no new table.

### 8.4 Frontend

The optimistic chip already exists (`app.js:3632` sets `uploading:true`). Extend it:
`pending` → chip shows **"transcribing…"** and a spinner; `ready` → size/chars as today;
`failed` → chip shows the error and a retry affordance. Poll `GET /api/attachments/{id}`
every ~2 s while any pending chip exists, and keep **send disabled** until all chips are
`ready` (reuse the `isUploading()` gate at `app.js:3540-3549`, renamed to cover `pending`).
Media upload progress uses XHR if you want a percentage (fetch has none); optional.

### 8.5 Alternative (smaller, weaker)

Keep everything synchronous, cap media to **~45 s** (`-t 45`), and bump the STT timeout.
Simple, but arbitrary (no podcasts/meetings), fragile near the Cloudflare limit, and gives
no feedback for long files. **Not recommended** — async is the right long-term shape and the
`pending` status is the same machinery a future long-PDF OCR or LibreOffice conversion would
reuse.

---

## 9. Limits, timeouts, Pi CPU/GPU

| Concern | Bound |
|---|---|
| Upload size | 10 MB (`maxAttachmentBytes`) |
| Stored text | 1,000,000 runes (`extract.MaxTextChars`) |
| Media duration | `MYOGCHAT_MEDIA_MAX_DURATION` (default 600 s; trim or reject) |
| ffmpeg run | `MYOGCHAT_MEDIA_TIMEOUT` (default 120 s) + `docker rm -f` cleanup |
| STT run | `MYOGCHAT_STT_TIMEOUT` (default 120 s; raise to 300 s for long media) |
| Concurrency | 1 media worker (queue depth 1–2) |
| Zip/XML expansion | per-entry `io.LimitReader` ≈ `MaxTextChars*4` (see risks) |

The Pi's **CPU is the bottleneck** for whisper (`docs/research/3.5-3.6-tts-stt.md:213-224`);
run media jobs **sequentially** and let the chat path stay responsive. GPU offload via
`MYOGCHAT_STT_URL` is the clean upgrade.

---

## 10. Dependency table

| Purpose | Module / tool | License | CGO | arm64 | Weight / notes |
|---|---|---|---|---|---|
| ZIP, XML, HTML entities | `archive/zip`, `encoding/xml`, `html` (stdlib) | Go BSD | no | yes | **covers docx/xlsx/pptx/odt/ods/odp/epub/rtf** |
| XLSX robustness (**optional**) | `github.com/xuri/excelize/v2` | BSD-3-Clause | no | yes | ~7 transitive modules (x/crypto, x/image, x/net, x/text, efp, nfp, mscfb, go-deepcopy) |
| EPUB/fetch HTML (**optional**) | `golang.org/x/net/html` | BSD-3-Clause | no | yes | only if the stdlib stripper is insufficient |
| RTF (**alternative**) | `github.com/lu4p/cat` | Unlicense | no | yes | tiny repo (104★); prefer in-repo parser |
| MP4 metadata (**reference only**) | `github.com/abema/go-mp4` | MIT | no | yes | can't decode AAC — not a substitute for ffmpeg |
| Media probe/convert | `ffmpeg`/`ffprobe` in Docker (`deploy/media/`) | LGPL/GPL build | n/a | yes | **external container, never linked**; like the Tesseract container |
| Transcription | existing whisper.cpp service (`internal/speech`) | MIT | n/a | arm64 image | reuse as-is |

**Net new Go dependencies required: zero** (core). Optional: excelize and/or `x/net/html`.

---

## 11. Tests

Mirror the existing style (`extract_test.go` builds a real PDF fixture with `buildPDF`,
`ocr_test.go` injects a fake `CommandRunner`).

- `internal/extract/ooxml_test.go`, `odf_test.go`, `epub_test.go`, `rtf_test.go` — build
  **minimal real ZIP/XML fixtures with `archive/zip`** (like `buildPDF`) for each format;
  assert extracted text, block/newline handling, magic-byte detection with a generic
  `application/octet-stream` MIME, and corrupt/truncated input → `ErrUnsupported`.
- **Bomb tests** — a deeply nested / huge-decompressed entry must be bounded (no OOM).
- `internal/media/media_test.go` — fake `CommandRunner` asserts the `ffprobe`/`ffmpeg` argv,
  stdout parsing, the duration cap logic, timeout + `docker rm -f`, and "no audio stream" →
  error. Opt-in live smoke (`MYOGCHAT_MEDIA_LIVE=1`) behind Docker + the built image
  (mirrors `internal/ocr/ocr_test.go`).
- `internal/server` — routing: media type → injected `Transcribe` called with the bytes;
  nil transcriber → `ErrUnsupported`; async status transitions (`pending`→`ready`/`failed`),
  `GET /api/attachments/{id}` payload, and stale-pending handling on startup.
- `internal/store/store_test.go` — the `status`/`error` migration + round-trip (mirror the
  `messages.attachments` migration test).
- Verification commands unchanged: `mise exec -- go build/vet/test ./...`,
  `node --check static/app.js`, `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...`.

---

## 12. Effort estimate

| Workstream | Scope | Estimate |
|---|---|---|
| Dispatcher + detection + `Options` seam | `detect.go`, `extract.go`, server/tests | 0.5 d |
| OOXML (docx/pptx + stdlib xlsx) | `ooxml.go` + tests | 1 d |
| ODF + EPUB (+ HTML stripper) | `odf.go`, `epub.go`, `html.go` + tests | 1 d |
| RTF | `rtf.go` + tests | 0.5 d |
| Media container + `internal/media` | `deploy/media/*`, `internal/media/media.go` + tests | 1.5 d |
| Async media pipeline | store migration, worker, API, SPA polling, gating | 1.5–2 d |
| Docs / contract / env | `docs/4.5-*`, `API_CONTRACT.md`, `TODO.md`, `env.example` | 0.5 d |
| **Total** | | **≈ 4–7 days** |

---

## 13. Risks

1. **Zip bombs / XML DoS.** A tiny ZIP can decompress to gigabytes; XML can be deeply nested.
   Mitigate: `io.LimitReader` on every part (~`MaxTextChars*4`), cap entry count, and cap
   total uncompressed bytes; `encoding/xml` streaming (no `Unmarshal` of huge trees).
2. **Panic on malformed input.** Follow `extractPDF`'s `recover()` guard
   (`extract.go:183-188`) around each sub-extractor so a bad file can't crash the server.
3. **Cloudflare 524.** The whole reason for async media; documents stay fast.
4. **Pi CPU contention.** Serialise media jobs; consider `tiny.en` for media or GPU offload.
5. **ffmpeg image size/build + codec licenses.** Alpine's `ffmpeg` build is GPL/LGPL; it runs
   **only inside a container** (never linked), the same posture as Tesseract. Pin the base
   image (`alpine:3.20`/`3.22`) for reproducibility.
6. **Unsupported-but-common formats.** `.doc/.xls/.ppt/.msg/.pages` will error clearly; users
   may expect them. Document the cut and the optional LibreOffice path.
7. **Large XML in memory.** Shared strings and long docs; keep the streaming reader and the
   existing char cap.
8. **STT service not configured.** Media must degrade to `ErrUnsupported` with a clear message
   when `MYOGCHAT_STT` is off / unconfigured (same behaviour as OCR-disabled,
   `extract.go:131-133`).
9. **Prompt injection from documents.** Office/EPUB text is untrusted data; it is already
   folded as context only (`foldAttachments`), which is the correct posture — keep it.

---

## 14. Host provisioning (mirror the OCR setup)

- **`deploy/media/Dockerfile`** (new), pinned:
  ```dockerfile
  FROM alpine:3.22
  RUN apk add --no-cache ffmpeg
  CMD ["ffmpeg", "-version"]
  ```
  (Alpine's `ffmpeg` package ships both `ffmpeg` and `ffprobe`; ~70–90 MB.)
- **`deploy/media-setup.sh`** (new) — copy `deploy/ocr-setup.sh`: verify docker, build the
  image, run `ffmpeg -version` / `ffprobe -version`, print the OK line.
- **Env (`deploy/env.example`)**: `MYOGCHAT_MEDIA=1`,
  `MYOGCHAT_FFMPEG_IMAGE=myogchat-media`, `MYOGCHAT_MEDIA_MAX_DURATION=600`,
  `MYOGCHAT_MEDIA_TIMEOUT=120s`, `MYOGCHAT_MEDIA_OVER_LIMIT=trim`, and (existing)
  `MYOGCHAT_STT_URL`. Optional DB settings `media_enabled`, `media_max_duration`.
- **No whisper image change:** reuse the persistent STT service from `deploy/speech-setup.sh`.
- **`main.go:658-717`** gains a media engine when `MYOGCHAT_MEDIA != 0` (mirroring the OCR
  engine block), and `server.New` takes it alongside `ocrEngine`.

---

## 15. Files + open questions

**New / changed**
- `internal/extract/`: `detect.go`, `ooxml.go`, `xlsx.go`, `odf.go`, `epub.go`, `html.go`,
  `rtf.go`, `media.go`, `extract.go` (add `Options`/`Extract`, keep `ExtractWith`), tests.
- `internal/media/`: `media.go` (+ test) — the ffmpeg engine + `TranscribeFunc`.
- `internal/server/attachments.go` (dispatch + `202` + worker), a small
  `internal/server/media_jobs.go`, `internal/store/store.go` (migration + fields),
  `main.go` (env + wiring), `internal/server/server.go` (field/route hooks).
- `static/app.js` (media chip status + polling + accept list), `static/sw.js` (cache bump).
- `deploy/media/Dockerfile`, `deploy/media-setup.sh`, `deploy/env.example`.
- `docs/4.5-file-formats.md`, `API_CONTRACT.md` (§ attachments), `TODO.md`.

**Open questions**
1. Async now, or ship documents first (fast, sync) and media async in a second batch?
2. Media duration cap: 600 s default? trim or reject over-limit?
3. Add `excelize` for XLSX robustness, or keep stdlib-only?
4. Optional LibreOffice-in-Docker catch-all for `.doc/.xls/.ppt/.pages` — worth ~1 GB on the
   Proxmox box (LAN-only) rather than the Pi?
5. Raise the 10 MB attachment cap for media, or keep it uniform?
