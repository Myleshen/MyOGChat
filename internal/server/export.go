package server

// Export & backup (§19). Two owner/admin-scoped downloads:
//   - GET /api/conversations/{id}/export?format=md|json — a transcript of one
//     conversation (Markdown or the same JSON as GET /api/conversations/{id}).
//   - GET /api/admin/backup — a consistent SQLite snapshot (VACUUM INTO).
//
// Both set Content-Disposition so the browser saves a file; the conversation
// export is owner-scoped (a non-owner gets 404, matching every other
// conversation route).

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/store"
)

// maxExportContentBytes caps a single message's content in a Markdown export,
// so a pathological message cannot blow up the transcript. 100 KB matches the
// send-time limit (contract §3).
const maxExportContentBytes = 100 * 1024

// handleExportConversation streams one conversation as Markdown or JSON.
func (s *Server) handleExportConversation(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	msgs, err := s.st.ListMessages(r.Context(), conv.ID)
	if err != nil {
		s.log.Error("export: load messages", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load messages")
		return
	}
	slug := slugify(conv.Title)
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "md"
	}
	switch format {
	case "json":
		w.Header().Set("Content-Disposition", `attachment; filename="`+slug+"-"+conv.ID+`.json"`)
		w.Header().Set("Cache-Control", "no-store")
		// Same shape as GET /api/conversations/{id}: conversation fields plus
		// an ascending messages array.
		writeJSON(w, http.StatusOK, struct {
			store.Conversation
			Messages []store.Message `json:"messages"`
		}{conv, msgs})
	case "md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+slug+"-"+conv.ID+`.md"`)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, renderTranscriptMarkdown(conv, msgs))
	default:
		writeError(w, http.StatusBadRequest, "unsupported format (want md or json)")
	}
}

// handleAdminBackup streams a consistent snapshot of the live database. Admin
// only. The snapshot is produced by VACUUM INTO into a temp file, then served
// and removed; a non-admin gets 403.
func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !auth.RequireAdmin(w, u) {
		return
	}
	// A private (0700) temp dir: the snapshot holds every user's password hash
	// and encrypted secrets, so the file must never be world-readable (5.x
	// hardening). SQLite creates the destination with the daemon's umask, so the
	// directory is what protects it.
	dir, err := os.MkdirTemp("", "myogchat-backup-*")
	if err != nil {
		s.log.Error("backup: create temp dir", "error", err)
		writeError(w, http.StatusInternalServerError, "backup failed")
		return
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "backup.db")

	if err := s.st.Backup(r.Context(), tmp); err != nil {
		s.log.Error("backup: vacuum into", "error", err)
		writeError(w, http.StatusInternalServerError, "backup failed")
		return
	}
	out, err := os.Open(tmp)
	if err != nil {
		s.log.Error("backup: open snapshot", "error", err)
		writeError(w, http.StatusInternalServerError, "backup failed")
		return
	}
	defer out.Close()
	fi, err := out.Stat()
	if err != nil {
		s.log.Error("backup: stat snapshot", "error", err)
		writeError(w, http.StatusInternalServerError, "backup failed")
		return
	}
	name := "myogchat-backup-" + time.Now().UTC().Format("2006-01-02") + ".db"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, fi.ModTime(), out)
}

// renderTranscriptMarkdown builds a human-readable transcript: a title header,
// a metadata block, then each message as "## You" / "## Assistant" with a
// model · time · cost footer line where the fields are present.
func renderTranscriptMarkdown(conv store.Conversation, msgs []store.Message) string {
	title := strings.TrimSpace(oneLine(conv.Title))
	if title == "" {
		title = "Untitled conversation"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "- **Model:** %s\n", conv.Model)
	fmt.Fprintf(&b, "- **Created:** %s\n", formatExportTime(conv.CreatedAt))
	fmt.Fprintf(&b, "- **Updated:** %s\n", formatExportTime(conv.UpdatedAt))
	fmt.Fprintf(&b, "- **Messages:** %d\n\n", len(msgs))
	b.WriteString("---\n\n")
	for _, m := range msgs {
		role := "Assistant"
		if m.Role == "user" {
			role = "You"
		}
		fmt.Fprintf(&b, "## %s\n\n", role)
		// Fence the body so model output (or a crafted message) can never spoof
		// the transcript structure (5.x hardening).
		b.WriteString(fenceMarkdown(capExportContent(m.Content)))
		b.WriteString("\n\n")
		parts := make([]string, 0, 3)
		if m.Model != nil && *m.Model != "" {
			parts = append(parts, *m.Model)
		}
		parts = append(parts, formatExportTime(m.CreatedAt))
		if m.CostMicros != nil {
			parts = append(parts, formatExportCost(*m.CostMicros))
		}
		fmt.Fprintf(&b, "_%s_\n\n", strings.Join(parts, " · "))
	}
	return b.String()
}

// fenceMarkdown wraps content in a code fence one backtick longer than the
// longest backtick run inside it, so the content can never break out.
func fenceMarkdown(s string) string {
	longest, cur := 0, 0
	for _, r := range s {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	return fence + "\n" + s + "\n" + fence
}

// capExportContent UTF-8-truncates an over-long message and marks the cut.
func capExportContent(s string) string {
	if len(s) <= maxExportContentBytes {
		return s
	}
	return firstNBytes(s, maxExportContentBytes) + "\n\n… [truncated]"
}

// formatExportTime renders unix-millisecond timestamps as UTC wall clock.
func formatExportTime(ms int64) string {
	if ms <= 0 {
		return "unknown"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC")
}

// formatExportCost renders micro-dollars the way the SPA does (4 decimals,
// "Free" for zero).
func formatExportCost(micros int64) string {
	if micros == 0 {
		return "Free"
	}
	return fmt.Sprintf("$%.4f", float64(micros)/1e6)
}

// slugify reduces a conversation title to a filename-safe fragment (lowercase
// a-z0-9 with single dashes). It never returns an empty string.
func slugify(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 60 {
		out = strings.Trim(out[:60], "-")
	}
	if out == "" {
		return "chat"
	}
	return out
}
