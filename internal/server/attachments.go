package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"myogchat/internal/extract"
	"myogchat/internal/speech"
	"myogchat/internal/store"
)

const (
	// maxAttachmentBytes caps a single uploaded file (2.3).
	maxAttachmentBytes = 10 << 20 // 10 MB
	// maxAttachmentsPerMessage bounds how many files one message may fold in.
	maxAttachmentsPerMessage = 20
	// maxAttachedChars is the total attachment-text budget folded into one
	// request (~200k characters). Over-budget text is truncated with a marker.
	maxAttachedChars = 200_000
	// foldTruncationMarker is appended when attachment text is cut to fit.
	foldTruncationMarker = "\n\n[... attachment truncated to fit the context budget ...]"
	// attachmentIntro leads the folded context block.
	attachmentIntro = "The user attached the following file(s). Treat their contents as context for the request.\n\n"
)

// handleUploadAttachment accepts a multipart/form-data upload (field "file"),
// extracts its text, and stores it for the caller (2.3).
func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	// Cap the whole request body (file + multipart overhead).
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes+1<<20)
	if err := r.ParseMultipartForm(maxAttachmentBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "file exceeds the 10 MB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "malformed multipart upload")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()
	if header.Size > maxAttachmentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "file exceeds the 10 MB limit")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAttachmentBytes+1))
	if err != nil {
		s.log.Error("read upload", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read upload")
		return
	}
	if len(data) > maxAttachmentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "file exceeds the 10 MB limit")
		return
	}

	name := strings.TrimSpace(header.Filename)
	if name == "" {
		name = "file"
	}
	mimeType := header.Header.Get("Content-Type")
	// Images are OCR'd when enabled (features 2.5/2.13); audio/video are
	// transcribed via speech-to-text (4.5); other types are unchanged.
	opts := extract.Options{OCR: s.ocrFunc(r.Context(), u)}
	if tr := s.sttTranscriber(r.Context(), ""); tr != nil {
		opts.Transcribe = speech.STT(r.Context(), tr)
	}
	text, err := extract.ExtractOpts(name, mimeType, data, opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, extractMessage(err))
		return
	}

	att := store.Attachment{
		ID:        newUUID(),
		UserID:    u.ID,
		Name:      name,
		Mime:      mimeType,
		Size:      int64(len(data)),
		Text:      text,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.st.InsertAttachment(r.Context(), &att); err != nil {
		s.log.Error("insert attachment", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store attachment")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":    att.ID,
		"name":  att.Name,
		"mime":  att.Mime,
		"size":  att.Size,
		"chars": att.Chars(),
	})
}

// handleGetAttachment returns an attachment's metadata (owner only, 2.3).
func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	att, err := s.st.GetAttachment(r.Context(), r.PathValue("id"), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "attachment not found")
		return
	}
	if err != nil {
		s.log.Error("get attachment", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load attachment")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         att.ID,
		"name":       att.Name,
		"mime":       att.Mime,
		"size":       att.Size,
		"chars":      att.Chars(),
		"created_at": att.CreatedAt,
	})
}

// handleDeleteAttachment removes an attachment (owner only, 2.3).
func (s *Server) handleDeleteAttachment(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	n, err := s.st.DeleteAttachment(r.Context(), r.PathValue("id"), u.ID)
	if err != nil {
		s.log.Error("delete attachment", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete attachment")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "attachment not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// extractMessage maps extractor errors to user-facing text.
func extractMessage(err error) string {
	switch {
	case errors.Is(err, extract.ErrNotText):
		return "file is not valid UTF-8 text"
	case errors.Is(err, extract.ErrEmpty):
		return "no text could be extracted from the file"
	case errors.Is(err, extract.ErrOCR):
		return "could not read text from the image: " + err.Error()
	case errors.Is(err, extract.ErrTranscribe):
		return "could not transcribe the audio/video: " + err.Error()
	case errors.Is(err, extract.ErrUnsupported):
		return err.Error()
	default:
		return "could not extract text: " + err.Error()
	}
}

// loadAttachments resolves attachment ids for a user, preserving order and
// de-duplicating. An unknown id (or one owned by another user) is an error so
// the send fails clearly rather than silently dropping context.
func (s *Server) loadAttachments(ctx context.Context, userID string, ids []string) ([]store.Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	atts := make([]store.Attachment, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if len(atts) >= maxAttachmentsPerMessage {
			break
		}
		a, err := s.st.GetAttachment(ctx, id, userID)
		if err != nil {
			return nil, err
		}
		seen[id] = true
		atts = append(atts, a)
	}
	return atts, nil
}

// attachmentMeta projects stored attachments to the message display metadata.
func attachmentMeta(atts []store.Attachment) []store.AttachedFile {
	if len(atts) == 0 {
		return nil
	}
	out := make([]store.AttachedFile, 0, len(atts))
	for _, a := range atts {
		out = append(out, store.AttachedFile{ID: a.ID, Name: a.Name, Mime: a.Mime, Size: a.Size})
	}
	return out
}

// foldAttachments renders attachment text as a leading context block, enforcing
// the total character budget and truncating with a clear marker (2.3).
func foldAttachments(atts []store.Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	write := func(s string) {
		b.WriteString(s)
		used += len([]rune(s))
	}

	intro := attachmentIntro
	if len([]rune(intro)) >= maxAttachedChars {
		intro = ""
	}
	write(intro)

	for i, a := range atts {
		header := "### " + a.Name + "\n"
		if used+len([]rune(header)) >= maxAttachedChars {
			// No room left even for the heading: note the omission and stop.
			write("\n\n[... " + pluralRemaining(len(atts)-i) + " omitted: context budget reached ...]")
			break
		}
		write(header)
		remaining := maxAttachedChars - used
		body := []rune(a.Text)
		if len(body) > remaining {
			marker := []rune(foldTruncationMarker)
			keep := remaining - len(marker)
			if keep < 0 {
				keep = 0
			}
			write(string(body[:keep]))
			write(foldTruncationMarker)
			if len(atts)-i-1 > 0 {
				write("\n\n[... " + pluralRemaining(len(atts)-i-1) + " omitted: context budget reached ...]")
			}
			break
		}
		write(a.Text)
		if i < len(atts)-1 {
			write("\n\n")
		}
	}
	return b.String()
}

// pluralRemaining formats "N more attachment(s)".
func pluralRemaining(n int) string {
	if n == 1 {
		return "1 more attachment"
	}
	return strconv.Itoa(n) + " more attachments"
}
