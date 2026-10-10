package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"myogchat/internal/store"
)

// fakeSTT is an OpenAI-compatible transcription endpoint returning a fixed text.
func fakeSTTHandler(text string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
	})
	return mux
}

// TestAsyncMediaTranscription exercises the async media path (4.5b): an
// audio upload returns 202 "pending", a background worker transcribes it, and
// GET /api/attachments/{id} flips to "ready" with the transcript.
func TestAsyncMediaTranscription(t *testing.T) {
	stt := httptest.NewServer(fakeSTTHandler("the quick brown fox"))
	defer stt.Close()

	srv, st := newImageTestServer(t, "")
	ctx := context.Background()
	if err := st.SetSetting(ctx, settingSTTURL, stt.URL); err != nil {
		t.Fatalf("SetSetting(stt_url): %v", err)
	}
	seedActiveUser(t, st, "u-a", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	// Upload a fake mp3.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "clip.mp3")
	_, _ = fw.Write([]byte("not-really-mp3-bytes"))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}
	var up map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &up)
	if up["status"] != "pending" {
		t.Fatalf("upload status field = %v, want pending", up["status"])
	}
	id, _ := up["id"].(string)
	if id == "" {
		t.Fatal("no attachment id")
	}

	// Poll until the worker finishes.
	var got map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		grr := doJSON(t, h, cookie, http.MethodGet, "/api/attachments/"+id, nil)
		if grr.Code != http.StatusOK {
			t.Fatalf("GET status = %d", grr.Code)
		}
		got = nil
		_ = json.Unmarshal(grr.Body.Bytes(), &got)
		if got["status"] == "ready" || got["status"] == "error" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got["status"] != "ready" {
		t.Fatalf("final status = %v, error=%v", got["status"], got["error"])
	}
	if n, _ := got["chars"].(float64); n <= 0 {
		t.Errorf("chars = %v, want > 0", got["chars"])
	}
	// The stored transcript is foldable.
	a, err := st.GetAttachment(ctx, id, "u-a")
	if err != nil || !strings.Contains(a.Text, "quick brown fox") {
		t.Errorf("stored transcript = %q err=%v", a.Text, err)
	}
}

// TestAttachmentNotReadyRejected checks a send referencing a still-pending
// attachment is rejected with 409 rather than folding empty context.
func TestAttachmentNotReadyRejected(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	ctx := context.Background()
	seedActiveUser(t, st, "u-a", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	if err := st.InsertConversation(ctx, &store.Conversation{
		ID: "c1", UserID: strptr("u-a"), Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	if err := st.InsertAttachment(ctx, &store.Attachment{
		ID: "a-pending", UserID: "u-a", Name: "clip.mp3", Mime: "audio/mpeg",
		Size: 10, Status: "pending", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertAttachment: %v", err)
	}

	rr := doJSON(t, h, cookie, http.MethodPost, "/api/conversations/c1/messages", map[string]any{
		"content": "transcribe this", "attachment_ids": []string{"a-pending"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "still being transcribed") {
		t.Errorf("body = %s", rr.Body.String())
	}
}
