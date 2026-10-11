# 5.1 — Vision image input (+ paste image)

Status: implemented on `feat/p5-vision` (Phase 5).

## What it does

A photo (or screenshot) attached to a message can be sent to a **vision-capable**
model so the model actually *sees* it — not just OCR-to-text. You can also
**paste** an image straight into the composer.

## How it works

1. **Composer.** Attaching (paperclip) or pasting an image downscales it in the
   browser (long edge ≤ 1568 px) and keeps the bytes as a data URI. The file is
   uploaded to `POST /api/attachments` as before (chip + metadata; OCR text when
   an OCR engine is configured) and the data URI is held only in the tab.
2. **Send.** `POST /api/conversations/{id}/messages` includes `images:[dataURI…]`
   for the image attachments.
3. **Server.** If the conversation's model has `vision:true` and images are
   present, the inline images are attached to the just-sent user message as
   multimodal content parts and the redundant OCR text of image attachments is
   dropped from the folded context. Non-image attachments are folded as usual.
   For a non-vision model (or no images), behavior is unchanged: the OCR text is
   folded like any other attachment.
4. **Persistence.** Bytes are never stored. A reload or regenerate has no inline
   images and falls back to the folded OCR text.

### Dialect shapes

- chat: `content:[{type:"text",…},{type:"image_url",image_url:{url,detail:"high"}}]`
- responses: `content:[{type:"input_text",…},{type:"input_image",image_url,detail:"high"}]`

Both are the same shapes `upstream.VisionOnce` already used for OCR.

## Images without OCR

An image is useful even when OCR is unavailable, so an image upload no longer
fails when OCR is off/absent: it is stored metadata-only (`status:"ready"`,
empty text). A vision-capable model reads it directly; a non-vision model simply
gets no text for it. Non-image files still return `400` on extraction failure.

## Caps

- ≤ 4 images per message; each data URI ≤ 8 MB; ≤ 16 MB total; message body
  cap 18 MB. Invalid/oversized `images` → `400`.
- The browser downscales before upload/send, so real photos rarely approach this.

## Files

- `internal/upstream/upstream.go` / `agent.go` — `Message.Images`, `chatMessageObj`,
  `responsesInputObj`, `responsesInput`; both streaming and agentic paths render
  vision parts.
- `internal/server/chat.go` — `images` on the message request, `cleanVisionImages`,
  `attachImagesToLastUser`, `nonImageAttachments`, `streamReply` vision handling.
- `internal/server/attachments.go` — image uploads succeed without OCR.
- `internal/extract/extract.go` — exported `IsImage`.
- `static/app.js` — downscale + data URI, `addFiles`, paste handler, thumbnails.
- `static/style.css` — `.attachment-thumb`.

## Tests

- `internal/upstream/vision_test.go` — part shapes + agent-loop payloads (chat + responses).
- `internal/server/vision_test.go` — validation, last-user attach, image-up-without-OCR.

## Not done (deliberate)

- No byte persistence → multi-turn follow-ups about an image work only within the
  same turn; after a reload the model has the OCR text (if any), not the pixels.
