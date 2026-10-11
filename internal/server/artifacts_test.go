package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"myogchat/internal/store"
)

// insertArtifact seeds one artifact row for a user.
func insertArtifact(t *testing.T, st *store.Store, a store.Artifact) {
	t.Helper()
	if err := st.InsertArtifact(context.Background(), &a); err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}
}

func TestArtifactGetDownloadHeaders(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	insertArtifact(t, st, store.Artifact{
		ID: "art-1", UserID: "u-alice", Name: "quarterly report.csv",
		Mime: "text/csv", Size: 6, Data: []byte("a,b\n1\n"), CreatedAt: 1,
	})

	rr := doJSON(t, h, cookie, http.MethodGet, "/api/artifacts/art-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET artifact status = %d, body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/csv; charset=utf-8", ct)
	}
	if v := rr.Header().Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", v)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") ||
		!strings.Contains(cd, `filename="quarterly report.csv"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if string(rr.Body.Bytes()) != "a,b\n1\n" {
		t.Errorf("body = %q", rr.Body.Bytes())
	}
}

// TestArtifactUnexpectedMimeIsOpaque checks a MIME outside the allowlist is
// served as application/octet-stream (never rendered), still as an attachment.
func TestArtifactUnexpectedMimeIsOpaque(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	insertArtifact(t, st, store.Artifact{
		ID: "art-x", UserID: "u-alice", Name: "weird.bin",
		Mime: "application/x-evil-mime", Size: 3, Data: []byte("abc"), CreatedAt: 1,
	})
	rr := doJSON(t, h, cookie, http.MethodGet, "/api/artifacts/art-x", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
}

// TestArtifactOwnerScoping checks another user cannot read or delete it.
func TestArtifactOwnerScoping(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	seedActiveUser(t, st, "u-bob", "bob", false)
	h := srv.Handler()
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")

	insertArtifact(t, st, store.Artifact{
		ID: "art-2", UserID: "u-alice", Name: "secret.txt",
		Mime: "text/plain", Size: 3, Data: []byte("hey"), CreatedAt: 1,
	})

	if rr := doJSON(t, h, bob, http.MethodGet, "/api/artifacts/art-2", nil); rr.Code != http.StatusNotFound {
		t.Errorf("bob GET status = %d, want 404", rr.Code)
	}
	if rr := doJSON(t, h, bob, http.MethodDelete, "/api/artifacts/art-2", nil); rr.Code != http.StatusNotFound {
		t.Errorf("bob DELETE status = %d, want 404", rr.Code)
	}
	if rr := doJSON(t, h, alice, http.MethodGet, "/api/artifacts/art-2", nil); rr.Code != http.StatusOK {
		t.Errorf("alice GET status = %d, want 200", rr.Code)
	}

	// Owner delete, then it is gone for the owner too.
	if rr := doJSON(t, h, alice, http.MethodDelete, "/api/artifacts/art-2", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("owner DELETE status = %d, want 204", rr.Code)
	}
	if rr := doJSON(t, h, alice, http.MethodGet, "/api/artifacts/art-2", nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET after delete status = %d, want 404", rr.Code)
	}
}

// TestSanitizeDownloadName checks header-injection and unsafe characters.
func TestSanitizeDownloadName(t *testing.T) {
	cases := map[string]string{
		`a"b.txt`:        "a_b.txt",
		"a\nb.txt":       "a_b.txt",
		`..\evil/path`:   ".._evil_path",
		"   ":            "download",
		"":               "download",
		"normal name.md": "normal name.md",
	}
	for in, want := range cases {
		if got := sanitizeDownloadName(in); got != want {
			t.Errorf("sanitizeDownloadName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestArtifactRouteRegistered ensures the routes exist on the mux.
func TestArtifactRouteRegistered(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")
	// An unknown id is a clean 404 (not a mux 405), proving the route is wired.
	rr := doJSON(t, h, cookie, http.MethodGet, "/api/artifacts/nope", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["error"] != "artifact not found" {
		t.Errorf("error = %q", body["error"])
	}
}
