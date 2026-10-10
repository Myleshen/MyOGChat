# Phase 4 · Image generation prompt quality & context

**Status:** research only · no code changed
**Author:** subagent research pass
**Scope:** why self-hosted SD-Turbo produces off-model / "disturbing" images, and how
to give the image model the "right context and details" the owner asked for.
**Related:** `docs/3.1-imagegen.md`, `docs/research/3.1-imagegen-recheck.md`,
`API_CONTRACT.md` §16.

---

## 0. TL;DR

1. **Root cause is prompt starvation, not the backend.** The app sends the user's
   raw composer text to SD-Turbo with no expansion and no conversation context.
   The live DB shows the owner's prompt stored verbatim as
   `"Tom and Jerry with pluto"` (18 chars) — far too thin for a 4-step distilled
   model. SD-Turbo melts on terse, out-of-distribution prompts.
2. **Recommended fix: an LLM prompt-enhancement step** that rewrites the brief
   into a rich, structured SD prompt using the **existing** `internal/upstream`
   client + the user's resolved key, with a **preview & edit** UI so the user can
   correct it before spending GPU time. Fall back to a **deterministic template**
   if no key is available. This is the smallest change that directly addresses
   the complaint and works with the existing composer UX.
3. **Negative prompts do not work here.** The fixed workflow runs `cfg: 1.0`
   (`imagegen.go:405`), which means classifier-free guidance is disabled and the
   negative-prompt node (`imagegen.go:396-399`) is **mathematically inert**.
   Safety must come from the positive prompt + a sanitizer, not a negative prompt.
4. **Expose `generate_image` as a model tool too**, as a second step, because the
   model already holds the conversation context. It is more work (tool results are
   text-only today; images need a server-side side channel) and should reuse the
   same enhance+generate function. Keep it optional.
5. **Effort:** enhancement + preview/edit ≍ **1.5–2.5 days**; add the
   `generate_image` tool ≍ **+1–1.5 days**; lightweight safety ≍ **0.5 day**.

---

## 1. Evidence

### 1.1 Live database (queried read-only over SSH from the Pi, 2026-10-10)

```
sqlite> select id, prompt, model, width, height, created_at from images;
cd8037bb-…  'Tom and Jerry with pluto'  'sd_turbo.safetensors'  512  512  1791649987647
count == 1
```

The assistant message that carries it (`messages.images`):

```json
[{"id":"cd8037bb-…","prompt":"Tom and Jerry with pluto","mime":"image/png",
  "width":512,"height":512,"size":548316}]
```

The image prompt was typed into the composer bar **mid-conversation** — the
parent conversation is titled *"What is today's date"* (model
`deepseek-v4.1-flash`), and the surrounding turns are unrelated (date, IST time,
a bash probe, a PDF summary). Nothing from that context reached the image model,
and nothing expanded the 18-character brief. This is exactly the "not having the
right context and details" complaint.

### 1.2 Code path (raw prompt, no context, no enhancement)

`internal/server/images.go` reads the prompt verbatim and generates with it:

```go
// images.go:125-133
prompt := strings.TrimSpace(req.Prompt)
if prompt == "" { … }
if len([]rune(prompt)) > maxImagePromptChars { … }   // max 2000

// images.go:157-158
width, height := imagegen.ParseSize(req.Size)
res, err := client.Generate(r.Context(), prompt, width, height)
```

`handleCreateImage` never reads conversation history, so an image prompt like
"the same but at night" cannot work.

The fixed workflow (`internal/imagegen/imagegen.go:382-424`) has a **negative
node that is always empty** and a KSampler at **cfg 1.0 / 4 steps**:

```go
"7": { "class_type": "CLIPTextEncode", "inputs": {"text": "", "clip": ["4", 1]} },  // negative — always ""
"3": { "class_type": "KSampler", "inputs": { …, "steps": 4, "cfg": 1.0,          // imagegen.go:404-405
       "sampler_name": "dpmpp_2m", "scheduler": "karras", … } },
```

### 1.3 The composer UI does not help

`static/app.js:3959-3967` — the image bar **prefills from the chat composer**
text, so unrelated chat text can bleed into the image prompt:

```js
if (open && imagePrompt) {
  if (!imagePrompt.value && input.value.trim()) imagePrompt.value = input.value.trim();
  imagePrompt.focus({ preventScroll: true });
}
```

The field is a single-line `<input maxlength="2000">` (`static/index.html:203`)
— fine for a brief, but it gives no room to show or edit a richer prompt.

### 1.4 Live backend (probed from the Pi)

```
GET http://comfyui.local:8188/system_stats
  comfyui_version 0.39.0 · pytorch 2.11.0+cu128 · argv ["main.py","--listen","0.0.0.0",
  "--port","8188","--force-fp32"]
  GPU NVIDIA GeForce GTX 1650 Ti · vram_total 3,911,581,696 (~3.64 GiB) · free ~1.37 GiB
GET /object_info/CheckpointLoaderSimple
  ckpt_name: [["sd_turbo.safetensors"]]   // the ONLY checkpoint installed
```

So today only SD-Turbo is wired and only one checkpoint is on disk. Any "different
model" option also needs a model download (see §4.4).

---

## 2. SD-Turbo / SD 1.5 prompting best practice

### 2.1 What SD-Turbo is, and why it is prompt-hungry

- SD-Turbo is a **distilled SD 2.1** (0.9B) model: **1–4 steps**, 512², and the
  model card specifies **`guidance_scale = 0.0`** (i.e. **no** classifier-free
  guidance). This repo uses `cfg: 1.0` in ComfyUI, which is the same thing.
- Distillation compresses the denoiser: the model has less capacity to "fill in"
  a vague brief. It therefore rewards **concrete, well-formed descriptions** and
  punishes short noun soup with melted anatomy, merged subjects, and random
  artifacts — precisely the "disturbing" result for *"Tom and Jerry with pluto"*.
- **Negatives are inert at cfg 1.0.** ComfyUI computes
  `noise = uncond + cfg·(cond − uncond)`; at `cfg = 1.0` this reduces to `cond`,
  so the negative-prompt CLIPTextEncode is unused. Do **not** rely on negative
  prompts for quality or safety unless the workflow moves to `cfg > 1`, which
  degrades SD-Turbo (it was trained for no guidance).
- There is also a **~77-token CLIP context** (OpenCLIP ViT-H, SD 2.1 encoder).
  Very long prompts get chunked/pooled and can dilute; target roughly **40–90
  tokens / 60–90 words**, front-loading the most important subject.
- SD 1.5 differs: `cfg ≈ 7`, 20–30 steps, responds to negatives and to
  "masterpiece / best quality" tags. SD-Turbo (SD 2.1 base) responds less to
  those boilerplate tags and more to **concrete nouns + visual style + lighting**.

### 2.2 The structured brief (what an expansion should cover)

Expand a short brief into these slots, in roughly this order:

| Slot | Example contribution |
|---|---|
| **Subject(s)** | "the gray cat Tom and the brown mouse Jerry"; describe appearances for IP |
| **Action / interaction** | "playing tug-of-war with a garden hose" |
| **Setting / background** | "on a suburban lawn beside a red doghouse" |
| **Style / medium** | "classic 1950s Technicolor cel animation, bold ink outlines" |
| **Lighting** | "soft even daylight, gentle shadows" |
| **Composition / shot** | "medium-wide shot, eye level, subject centered" |
| **Color / mood** | "bright saturated palette, playful, family-friendly" |
| **Detail / quality** | "clean linework, crisp, high detail" (light touch for SD-Turbo) |

### 2.3 Aspect / size

SD-Turbo was trained at **512²**; non-square sizes (and 1024) cause duplicated or
stretched subjects. `ParseSize` (`imagegen.go:453-478`) allows 64–1024 in
multiples of 64. **Recommendation:** keep the default 512²; if a ratio is offered,
limit to **512×768 / 768×512** and label it "portrait/landscape (experimental)".

### 2.4 Example transform (this repo's live case)

**Raw:** `Tom and Jerry with pluto`

**Enhanced (LLM or template):**
> *A cheerful cartoon scene in classic 1950s Technicolor cel-animation style: the
> gray cat Tom and the brown mouse Jerry on a green suburban lawn beside a red
> doghouse, joined by Pluto, the yellow dog with black floppy ears and a long
> snout, all smiling and playing together; bold clean ink outlines, flat cel
> shading, bright saturated colors, soft even daylight, medium-wide eye-level
> shot, playful family-friendly mood, crisp high detail.*

Note in the report/UI copy that **copyrighted characters are hard for SD-Turbo**
(it has no reliable memory of them), so physically describing the characters is
what makes them appear recognizably — and it is also a trademark caution.

### 2.5 Negative prompt (documentation only, given §2.1)

If the workflow ever moves to `cfg > 1` (e.g. SD 1.5 + LCM-LoRA, §4.4), a
reasonable default negative is:
`blurry, low quality, distorted, deformed, extra limbs, extra fingers, watermark, text, signature, jpeg artifacts, worst quality`.
At the current `cfg: 1.0` this has no effect and should not be promised.

---

## 3. Recommended architecture: an LLM enhancement step

### 3.1 Design

Insert an enhancement stage **before** `imagegen.Generate`:

```
composer brief
   │  (optional: recent conversation turns for context)
   ▼
POST /api/images/enhance  →  upstream ChatOnce(user key, enhancer prompt)  →  enhanced prompt
   │                                                                            │
   ▼  preview & edit (SPA)                                                       │
POST /api/images { prompt, conversation_id }  ───────────────────────────────────┘
   ▼
imagegen.Generate(prompt) → ComfyUI → images table → user+assistant messages
```

Key properties:

- **Reuse the existing upstream client.** `s.up.ChatOnce(ctx, apiKey, sessionID,
  model, msgs, maxTokens)` already exists (`internal/upstream/upstream.go:215-247`)
  and is used by auto-title (`internal/server/chat.go:407-408`). The enhancer is
  the same shape. Resolve the key with the existing `s.resolveKey` (`keys.go:33`),
  so BYOK/shared-key semantics are inherited for free.
- **Model choice:** use the free `longcat-2.5-preview-free` (`chat.go:22`) like
  auto-title, or the conversation's own model for on-brand phrasing. Give it a
  generous `maxTokens` (~400) because the free title model is a reasoning model.
- **Context:** pass the last N (e.g. 6) conversation messages so references like
  "the same character, now at night" resolve. This is the literal "right context".
- **Robustness:** on any enhancer error (no key, rate limit, empty output,
  refusal), fall back to a **deterministic template** (§3.3) so generation never
  regresses to the raw brief.
- **Prompt-injection guard:** the enhancer must treat the brief as *data* to
  expand, not instructions to follow; strip/ignore "ignore previous instructions"
  style content and always append the safety clause.

A self-contained, unit-testable home:

```go
// internal/promptenh/promptenh.go (new; stdlib only)
type Completer func(ctx context.Context, system, user string) (string, error)

// Enhance rewrites a short brief into a rich SD-Turbo prompt.
// Callers wire Completer to upstream.ChatOnce.
func Enhance(ctx context.Context, c Completer, brief string, context string) (string, error)
```

The server then does `promptenh.Enhance(ctx, s.completer(apiKey, conv.ID, model), brief, recentTurns)`.

### 3.2 Preview & edit UX (recommended)

- Change `#image-bar` from a single `<input>` (`index.html:203`) to a small
  two-step block: a one-line brief field, an **Enhance** button, and an editable
  **`<textarea id="image-prompt-final">`** that receives the enhanced prompt.
- Flow: type brief → **Enhance** (spinner) → textarea fills → user edits →
  **Generate** posts the edited text. Add a "Skip enhancement" toggle for users
  who want raw control.
- `static/app.js`: extend `generateImage` (`app.js:3971-4018`) into
  `enhanceImagePrompt()` + `generateImage()`. `buildImagesBlock` (`app.js:2983`)
  already renders results, so no rendering change is needed.
- New endpoint `POST /api/images/enhance` → `{prompt}` (no GPU work, no DB write),
  registered next to the existing image routes (`internal/server/server.go:163-165`).

### 3.3 Alternative / simpler options compared

| Option | Context-aware? | Cost / latency | Determinism | Effort | Verdict |
|---|---|---|---|---|---|
| **A. LLM enhancement + preview/edit** | ✅ (folds history) | +1 cheap/free LLM call (~0.3–1 s) | Medium | ~1.5–2.5 d | **Recommended** |
| **B. Structured style presets + template** | Partial (no history) | None | High | ~0.5 d | **Ship as fallback/toggle** |
| **C. Better manual prompt field only** | ❌ | None | n/a | ~0.1 d | Insufficient alone |
| **D. Different sampler / model (SD 1.5 + LCM, cfg 7)** | ❌ (same prompt problem) | 3–6× slower, more VRAM | n/a | ~1–2 d + download | Only if quality still poor; enables negatives |

Recommended combination: **A primary, B as the always-on deterministic fallback
(and an "enhancement off" path), C trivially folded in (placeholder + examples),
D deferred.** Note that SD-Turbo on a 4 GB card is the only fast interactive
option (recheck report §5); SD 1.5 is ~20–35 s and needs a checkpoint download
(§1.4), so keep it behind an explicit "quality (slow)" setting if ever added.

### 3.4 Storing what happened

Additive migration (pattern at `internal/store/store.go:344-396`) to keep both
the raw brief and the final prompt:

```sql
ALTER TABLE images ADD COLUMN raw_prompt TEXT;  -- the user's original brief
ALTER TABLE images ADD COLUMN style      TEXT;  -- optional preset id
```

`Image` (`store.go:232-244`) and `imageCols` (`store.go:1424`) gain the fields;
`InsertImage` (`store.go:1440`) writes them. Persisting `raw_prompt` lets "try
again" re-enhance from the brief rather than re-rolling the expanded text.

---

## 4. Should generation also be a model tool (`generate_image`)?

### 4.1 Why it is attractive

The chat model already has the **full conversation, project instructions, and
attached knowledge** in context. A `generate_image` tool lets the assistant
generate mid-conversation, incorporate prior turns implicitly ("make the logo
from earlier"), and iterate ("now make it nighttime") without the user
copy-pasting context into the image bar.

### 4.2 Why it is more work (the mechanics)

- **Tool results are text-only.** `tools.Tool.Run` returns `(string, error)`
  (`internal/tools/tools.go:19-29`) and the agent loop feeds that string straight
  back to the model (`internal/upstream/agent.go:349-358`). There is **no channel
  for image bytes/ids** on the tool result.
- **Images attach to the assistant message, set only in the composer path.**
  `store.Message.Images` (`store.go:196`) is populated in `handleCreateImage`
  (`images.go:206-209`). For a tool to produce an image in a reply, `streamReply`
  (`chat.go:159-344`) must inject ids into `asstMsg.Images` (`chat.go:298-312`)
  after the agent loop finishes.
- **Design:** give the tool a request-scoped **collector** (e.g.
  `*[]store.GeneratedImage` closed over by `s.toolExecutor(reg)` at `chat.go:210`).
  The tool generates + stores the image, appends metadata to the collector, and
  returns a short text result (`"Generated image #1 (512×512)."`) so the model can
  keep going. After the loop, `streamReply` sets `asstMsg.Images` from the
  collector and the existing `buildImagesBlock` renders it.
- **Package boundary:** `internal/tools` is deliberately free of server/upstream
  types (`tools.go:6-8`). The new tool should depend on small local interfaces
  (an `ImageGenerator` and a metadata sink), not on `store`/`server`.

### 4.3 Registry & agent-loop changes

- **New file `internal/tools/imagegen.go`**: `GenerateImageTool{Name: "generate_image"}`
  with params `{ prompt: string, size?: string }`; `Run` calls the injected
  generator + sink.
- **Register it in `toolRegistry`** (`internal/server/integrations.go:252-296`),
  gated on `s.imagegenClient(ctx)` succeeding (mirroring how Linkwarden/maps/
  calendar tools are gated, e.g. `integrations.go:273-282`). No change to the
  upstream wire types: `upstreamToolSpecs` (`integrations.go:299-312`) already
  converts any registry spec.
- **`internal/server/chat.go`**: thread the collector through `toolExecutor`
  (`chat.go:210`) and attach to `asstMsg.Images`.
- Because a generation can take up to ~90 s, the existing `tool` SSE activity
  (`chat.go:274-284`) gives the "running" indicator; the loop's
  `streamTimeout` = 5 min and `maxToolIterations = 4`
  (`internal/upstream/upstream.go:26,47`) comfortably accommodate it. Tool-calling
  models that reject `tools` already retry once without them (`agent.go:74-81`).

### 4.4 Pros / cons summary

**Pros:** implicit context; conversational iteration; one UX for chat and images;
reuses enhancer + ComfyUI client.
**Cons / risks:** text-only tool results force a side channel; the model **cannot
see** the generated image (no vision round-trip in the loop) so it cannot
visually self-correct; a slow tool call blocks the SSE turn; model-written prompts
can be worse or more jailbreak-prone than a dedicated enhancer (mitigate by
running the enhancer *inside* the tool); more surface area + tests.

**Recommendation:** ship the enhancer first (fixes the owner's complaint now,
works in the current composer UI). Then add `generate_image` as a follow-up that
**reuses the same `promptenh` + generate functions**; keep it behind the existing
tools-enabled / imagegen-configured gates.

---

## 5. Safety / moderation (self-hosted, no external provider)

### 5.1 Two different failure modes — do not conflate them

The owner's example is most likely a **quality** failure: SD-Turbo "melted" a
terse, out-of-distribution prompt into a grotesque image. That is fixed by better
prompts (§2–3), not by a content filter. Genuine **NSFW** risk remains for
adversarial prompts and needs its own (lightweight) mitigation.

### 5.2 The negative-prompt lever is unavailable (repeat of §2.1)

At `cfg: 1.0` the negative node is inert. Any plan whose safety story is
"add NSFW terms to the negative prompt" **will silently do nothing**. Safety must
be enforced on the **positive prompt** and/or **after** rendering.

### 5.3 Recommended lightweight measures

1. **Positive-prompt sanitization (server-side, before generation).** Normalize
   whitespace/case and reject or rewrite a small denylist of explicit sexual,
   extreme-violence, and minor-related terms. Always append a benign safety
   clause ("family-friendly, tasteful, no text or watermarks") to the enhanced
   prompt. Fast, deterministic, no GPU cost. Note it is bypassable, so treat it
   as a floor, not a guarantee.
2. **Enhancer-side safety clause.** The enhancement system prompt (§3.1) already
   instructs "keep it family-friendly / no explicit content", which reduces
   accidental NSFW far more than a negative prompt would.
3. **Optional post-generation checker (self-hosted).** Add a ComfyUI
   safety-checker node between `VAEDecode` and `SaveImage` in `buildWorkflow`
   (`imagegen.go:415-422`). ComfyUI's `Stable Diffusion Safety Checker` pulls a
   small classifier (e.g. `Falconsai/nsfw_image_detection`); on a hit, return a
   generic "that image was blocked" instead of the bytes. **Caveat:** the GTX
   1650 Ti has ~1.37 GiB free (§1.4); loading a second model risks offload/OOM,
   so gate this behind a setting and measure.
4. **Vision-model classifier (de-prioritize).** `internal/upstream.VisionOnce`
   (`internal/upstream/vision.go:36`) can classify an image with an OpenCode Go
   vision model, but the image must be reachable as a URL and this sends the
   render off-device. Convenient, but not "self-hosted"; use only if (3) is
   infeasible.

**Recommendation:** ship (1)+(2) immediately (essentially free), add (3) only if
abuse is observed. Keep the safety-checker toggle off by default because of VRAM.

---

## 6. Concrete changes (files, smallest snippets)

### 6.1 Recommended slice (enhancer + preview/edit + sanitize)

| File | Change |
|---|---|
| `internal/promptenh/promptenh.go` **(new)** | `Enhance(ctx, Completer, brief, context)` + deterministic template fallback + safety clause + sanitizer. Stdlib only. |
| `internal/server/images.go` | New `handleEnhanceImagePrompt` (POST `/api/images/enhance`); in `handleCreateImage` accept `raw_prompt`/`enhance`/`style`, sanitize, store `RawPrompt`/`Style`; optionally fold recent turns. Reuse `s.up.ChatOnce` + `s.resolveKey`. |
| `internal/server/server.go` | Add `mux.HandleFunc("POST /api/images/enhance", s.handleEnhanceImagePrompt)` next to lines 163-165. |
| `internal/store/store.go` | Additive migration at `migrate()` (after line 362): `{"images","raw_prompt",…}`, `{"images","style",…}`; extend `Image` (232-244), `imageCols` (1424), `InsertImage` (1440), `scanImage`. |
| `internal/promptenh/promptenh_test.go` **(new)** | Fake `Completer`; assert slot coverage, fallback, sanitizer, injection guard. |
| `internal/server/images_test.go` | Cover `/enhance` (200, no-key fallback, empty output), `raw_prompt` round-trip, sanitizer rejection. |
| `static/index.html` | Replace the single input (lines 200-207) with brief field + **Enhance** button + editable `textarea#image-prompt-final` + "skip enhancement" toggle. |
| `static/app.js` | Split `generateImage` (3971-4018) into `enhanceImagePrompt` + `generateImage`; read the textarea; keep `buildImagesBlock` (2983) unchanged. |

Minimal illustration of the sanitizer/template hook (new package):

```go
// promptenh
func Enhance(ctx context.Context, c Completer, brief, convCtx string) (string, error) {
    if c != nil {
        if out, err := c(ctx, enhancerSystem, buildUser(brief, convCtx)); err == nil {
            if p := sanitize(out); p != "" { return p, nil }
        }
    }
    return Template(brief), nil // deterministic fallback, never empty
}
```

### 6.2 Follow-up (optional `generate_image` tool)

| File | Change |
|---|---|
| `internal/tools/imagegen.go` **(new)** | `GenerateImageTool` over local `ImageGenerator` + sink interfaces (no server/store imports). |
| `internal/server/integrations.go` | Register it in `toolRegistry` (252-296) when `s.imagegenClient(ctx)` is ok. |
| `internal/server/chat.go` | Collector through `toolExecutor` (210); attach to `asstMsg.Images` (298-312). |
| `internal/tools/imagegen_test.go` **(new)** | Params, size clamping, generator error → tool error text. |

---

## 7. Tests

- **`promptenh`:** fake completer returns a rich prompt → slots present; completer
  error/empty → deterministic template; injection string stripped; sanitizer
  blocks denylisted terms and appends the safety clause.
- **server:** `/api/images/enhance` with and without a resolvable key (exercises
  `resolveKey` fallback); `POST /api/images` persists `raw_prompt` + final prompt;
  sanitizer rejection returns `400`.
- **store:** additive migration is idempotent (`migrate()` pattern); `raw_prompt`
  round-trip; existing DBs without the column still load.
- **tool (if shipped):** registry includes `generate_image` only when configured;
  generated ids land on the assistant message; a generator error surfaces as a
  tool error the model can recover from.

---

## 8. Effort estimate

| Workstream | Estimate |
|---|---|
| Enhancement package + endpoint + store columns + tests | 1–1.5 d |
| SPA preview/edit UX + styles | 0.5–1 d |
| Sanitizer + safety clause | 0.25–0.5 d |
| **Recommended slice total** | **1.75–3 d** |
| `generate_image` tool + collector + tests | +1–1.5 d |
| Post-gen safety-checker node (optional, VRAM-gated) | +0.5–1 d |
| SD 1.5 + LCM "quality" model (optional, download + selector) | +1–2 d |

---

## 9. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Enhancer adds latency/cost | Cheap/free model; cache by brief hash; run only when the user clicks Enhance; always allow "skip". |
| No usable API key (server sharing off) | Deterministic template fallback (§3.3); surface a hint. |
| Enhancer hallucinates IP details or refuses | Preview & edit before Generate; user can override. |
| Copyrighted characters still render poorly | Set expectations in copy; describe appearances; note trademark caution. |
| Non-square / 1024 sizes cause artifacts | Keep 512² default; cap ratios to 512×768/768×512. |
| Negative-prompt safety silently no-ops | Document cfg=1.0 behavior; enforce on positive prompt (§5.2). |
| Safety checker OOMs 4 GB card | Off by default; measure free VRAM (~1.37 GiB) before enabling. |
| `generate_image` blocks the SSE turn | Existing keepalive (`chat.go:286-289`) + tool "running" activity; consider a hard timeout. |
| Model can't see its own output | Accept; the enhancer + user preview are the correction loop. |
| Migration on existing DB | Follow the idempotent additive `migrate()` pattern (`store.go:344-396`); tested by `store_test.go`. |

---

## 10. Sources

- **Stability AI, `sd-turbo` model card** — SD 2.1 distill, 1–4 steps, 512²,
  `guidance_scale = 0.0`, 0.9B. <https://huggingface.co/stabilityai/sd-turbo>
- **ComfyUI docs** — Server routes (`/prompt`, `/history/{id}`, `/view`,
  `/system_stats`); KSampler/CFG semantics (negative branch scaled by `cfg`;
  `cfg = 1.0` ⇒ no guidance).
  <https://docs.comfy.org/development/comfyui-server/comms_routes>
- **ComfyUI Stable Diffusion Safety Checker node** (post-render NSFW classifier).
- **OpenCLIP ViT-H / SD 2.1 text encoder** — ~77-token context window.
- **Repo:** `docs/research/3.1-imagegen-recheck.md` §5 (VRAM/speed table; SD-Turbo
  default), `docs/3.1-imagegen.md`, `API_CONTRACT.md` §16,
  `internal/imagegen/imagegen.go` (workflow), `internal/server/images.go`
  (create), `internal/server/chat.go` (agent turn), `internal/tools/tools.go`
  (tool interface), `internal/upstream/{upstream,agent}.go`.
