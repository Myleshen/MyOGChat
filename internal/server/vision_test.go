package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// TestCleanVisionImages covers the inline image validation (Phase 5.1).
func TestCleanVisionImages(t *testing.T) {
	// Blank entries are skipped; a valid data URI passes through.
	got, err := cleanVisionImages([]string{"  ", "data:image/png;base64,AAAA"})
	if err != nil {
		t.Fatalf("cleanVisionImages: %v", err)
	}
	if len(got) != 1 || got[0] != "data:image/png;base64,AAAA" {
		t.Fatalf("got %v", got)
	}
	// A non-image data URI is rejected.
	if _, err := cleanVisionImages([]string{"data:text/plain;base64,AAAA"}); err == nil {
		t.Error("expected a rejection for a non-image data URI")
	}
	// A malformed value is rejected.
	if _, err := cleanVisionImages([]string{"not-a-data-uri"}); err == nil {
		t.Error("expected a rejection for a malformed value")
	}
	// Too many images are rejected.
	many := make([]string, maxVisionImages+1)
	for i := range many {
		many[i] = "data:image/png;base64,AAAA"
	}
	if _, err := cleanVisionImages(many); err == nil {
		t.Error("expected a rejection when over the image-count cap")
	}
}

// TestAttachImagesToLastUser verifies images land on the most recent user turn.
func TestAttachImagesToLastUser(t *testing.T) {
	msgs := []upstream.Message{
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}
	attachImagesToLastUser(msgs, []string{"data:image/png;base64,AAAA"})
	if len(msgs[0].Images) != 0 {
		t.Errorf("images attached to the first user message: %+v", msgs[0])
	}
	if len(msgs[2].Images) != 1 {
		t.Errorf("images not attached to the last user message: %+v", msgs[2])
	}
}

// TestNonImageAttachments verifies image attachments are dropped before the OCR
// text is folded (so a vision model does not receive redundant text).
func TestNonImageAttachments(t *testing.T) {
	atts := []store.Attachment{
		{Name: "shot.png", Mime: "image/png"},
		{Name: "notes.pdf", Mime: "application/pdf"},
	}
	got := nonImageAttachments(atts)
	if len(got) != 1 || got[0].Name != "notes.pdf" {
		t.Fatalf("nonImageAttachments = %+v", got)
	}
}

// TestImageUploadWithoutOCR verifies an image attachment still uploads when OCR
// is unavailable (Phase 5.1): it is stored metadata-only and reported ready,
// because a vision-capable model can read it directly.
func TestImageUploadWithoutOCR(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-a", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", `form-data; name="file"; filename="shot.png"`)
	partHeader.Set("Content-Type", "image/png")
	fw, _ := mw.CreatePart(partHeader)
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nnot-really-a-png"))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201 (body %s)", rr.Code, rr.Body.String())
	}
	var up map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &up)
	if up["status"] != "ready" {
		t.Fatalf("status = %v, want ready", up["status"])
	}
	if n, _ := up["chars"].(float64); n != 0 {
		t.Errorf("chars = %v, want 0 (no OCR text)", up["chars"])
	}
	if !strings.HasPrefix(up["mime"].(string), "image/") {
		t.Errorf("mime = %v", up["mime"])
	}
}
