package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"myogchat/internal/sandbox"
	"myogchat/internal/store"
)

// artifactCacheControl is a long cache header for immutable artifact bytes (the
// id is a UUID and the content never changes).
const artifactCacheControl = "private, max-age=31536000, immutable"

// artifactMimeAllowlist is the closed set of Content-Type values served for an
// artifact download. A stored MIME outside the list is served as
// application/octet-stream, so an unexpected type can never be interpreted by
// the browser (the response is always an attachment with nosniff anyway).
var artifactMimeAllowlist = map[string]string{
	"text/plain":                "text/plain; charset=utf-8",
	"text/csv":                  "text/csv; charset=utf-8",
	"text/tab-separated-values": "text/tab-separated-values; charset=utf-8",
	"text/markdown":             "text/markdown; charset=utf-8",
	"text/html":                 "text/plain; charset=utf-8",
	"application/json":          "application/json",
	"application/xml":           "application/xml",
	"application/yaml":          "application/yaml",
	"application/pdf":           "application/pdf",
	"application/zip":           "application/zip",
	"application/sql":           "application/sql",
	"text/x-shellscript":        "text/plain; charset=utf-8",
	"text/x-python":             "text/plain; charset=utf-8",
	"text/x-go":                 "text/plain; charset=utf-8",
	"text/javascript":           "text/plain; charset=utf-8",
	"image/png":                 "image/png",
	"image/jpeg":                "image/jpeg",
	"image/gif":                 "image/gif",
	"image/webp":                "image/webp",
	"application/octet-stream":  "application/octet-stream",
	artifactXLSX:                artifactXLSX,
	artifactDOCX:                artifactDOCX,
	artifactPPTX:                artifactPPTX,
	"application/vnd.oasis.opendocument.spreadsheet": "application/octet-stream",
	"application/vnd.oasis.opendocument.text":        "application/octet-stream",
}

// OOXML media types, kept as named constants so the (long) strings are readable.
const (
	artifactXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	artifactDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	artifactPPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

// storeArtifact persists one file produced by a sandbox run for userID and
// returns the reference the assistant message stores (5.8). The conversation id
// scopes retention/cascade; it may be empty when there is no conversation.
func (s *Server) storeArtifact(ctx context.Context, userID, convID string, a sandbox.Artifact) (store.ArtifactRef, error) {
	sum := sha256.Sum256(a.Data)
	art := store.Artifact{
		ID:        newUUID(),
		UserID:    userID,
		Name:      a.Name,
		Mime:      a.MIME,
		Size:      int64(len(a.Data)),
		SHA256:    hex.EncodeToString(sum[:]),
		Data:      a.Data,
		CreatedAt: time.Now().UnixMilli(),
	}
	if convID != "" {
		art.ConversationID = &convID
	}
	if err := s.st.InsertArtifact(ctx, &art); err != nil {
		return store.ArtifactRef{}, err
	}
	return store.ArtifactRef{ID: art.ID, Name: art.Name, Mime: art.Mime, Size: art.Size}, nil
}

// handleGetArtifact serves an agent-produced file as an opaque attachment (owner
// only, 5.8). The bytes are never extracted or rendered: Content-Type comes from
// a closed allowlist, nosniff is set, and Content-Disposition is always
// attachment with a sanitized filename.
func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	art, err := s.st.GetArtifact(r.Context(), r.PathValue("id"), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if err != nil {
		s.log.Error("get artifact", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load artifact")
		return
	}
	ct, ok := artifactMimeAllowlist[strings.ToLower(art.Mime)]
	if !ok {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeDownloadName(art.Name)+`"`)
	w.Header().Set("Cache-Control", artifactCacheControl)
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(art.Data)), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(art.Data); err != nil {
		s.log.Error("write artifact", "error", err)
	}
}

// handleDeleteArtifact removes an artifact (owner only, 5.8).
func (s *Server) handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	n, err := s.st.DeleteArtifact(r.Context(), r.PathValue("id"), u.ID)
	if err != nil {
		s.log.Error("delete artifact", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete artifact")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sanitizeDownloadName makes a filename safe to place in a Content-Disposition
// header: ASCII printable only, no quotes/backslashes/control characters (so a
// name can never inject a header or escape the quoted string). A name that
// sanitizes away falls back to "download".
func sanitizeDownloadName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 32 && r < 127 && r != '"' && r != '\\' && r != '/':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "download"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}
