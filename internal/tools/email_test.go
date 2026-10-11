package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"myogchat/internal/mail"
)

// fakeEmailConn is an in-memory EmailConn for tool tests.
type fakeEmailConn struct {
	folders   []mail.Folder
	searchRes []mail.MessageSummary
	readMsg   *mail.Message
	err       error
	closed    bool
	lastQuery mail.SearchQuery
	lastUID   uint32
	lastIncl  bool
	lastAcct  string
}

func (f *fakeEmailConn) Folders(context.Context) ([]mail.Folder, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.folders, nil
}

func (f *fakeEmailConn) Search(_ context.Context, q mail.SearchQuery) ([]mail.MessageSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastQuery = q
	return f.searchRes, nil
}

func (f *fakeEmailConn) Read(_ context.Context, uid uint32, _ string, includeHTML bool) (*mail.Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastUID = uid
	f.lastIncl = includeHTML
	return f.readMsg, nil
}

func (f *fakeEmailConn) Close() error {
	f.closed = true
	return nil
}

// fakeConnector returns a connector that always yields f and records the account
// reference it was called with.
func fakeConnector(f *fakeEmailConn) EmailConnector {
	return func(_ context.Context, account string) (EmailConn, error) {
		f.lastAcct = account
		return f, nil
	}
}

func TestEmailFoldersTool(t *testing.T) {
	f := &fakeEmailConn{folders: []mail.Folder{
		{Name: "INBOX"},
		{Name: "Sent", SpecialUse: "sent"},
	}}
	tool := NewEmailFoldersTool(fakeConnector(f))
	if tool.Name() != "email_folders" {
		t.Fatalf("name = %q", tool.Name())
	}
	out, err := tool.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "INBOX") || !strings.Contains(out, "Sent (sent)") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "read-only") {
		t.Errorf("output missing read-only note: %q", out)
	}
	if !f.closed {
		t.Error("connection not closed")
	}
}

func TestEmailSearchTool(t *testing.T) {
	f := &fakeEmailConn{searchRes: []mail.MessageSummary{{
		UID: 7, Folder: "INBOX",
		From:    []mail.Address{{Name: "Alice", Email: "alice@example.com"}},
		Subject: "Invoice",
		Date:    time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
		Flags:   []string{"\\Seen"},
	}}}
	tool := NewEmailSearchTool(fakeConnector(f))
	args := json.RawMessage(`{"query":"invoice","unread_only":true,"limit":3,"folder":"Archive","account":"work","preview":true}`)
	out, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "uid=7") || !strings.Contains(out, "Invoice") || !strings.Contains(out, "Alice <alice@example.com>") {
		t.Fatalf("output = %q", out)
	}
	if f.lastQuery.Query != "invoice" || !f.lastQuery.UnreadOnly || f.lastQuery.Limit != 3 || f.lastQuery.Folder != "Archive" {
		t.Fatalf("query = %+v", f.lastQuery)
	}
	if f.lastAcct != "work" {
		t.Errorf("account ref = %q, want work", f.lastAcct)
	}
	if f.lastQuery.PreviewBytes != 4096 {
		t.Errorf("PreviewBytes = %d, want 4096", f.lastQuery.PreviewBytes)
	}
}

func TestEmailSearchToolDateArgs(t *testing.T) {
	f := &fakeEmailConn{}
	tool := NewEmailSearchTool(fakeConnector(f))
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"since":"2026-01-02"}`)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.lastQuery.Since.IsZero() || f.lastQuery.Since.Format("2006-01-02") != "2026-01-02" {
		t.Fatalf("since = %v", f.lastQuery.Since)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"since":"not-a-date"}`)); err == nil {
		t.Error("expected error for invalid date")
	}
}

func TestEmailReadTool(t *testing.T) {
	f := &fakeEmailConn{readMsg: &mail.Message{
		UID: 9, Folder: "INBOX",
		From:        []mail.Address{{Name: "Alice", Email: "alice@example.com"}},
		To:          []mail.Address{{Email: "bob@example.com"}},
		Subject:     "Hi",
		Date:        time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
		MessageID:   "m1@example.com",
		BodyText:    "Hello there.",
		Attachments: []mail.Attachment{{Filename: "a.pdf", ContentType: "application/pdf", Size: 100}},
	}}
	tool := NewEmailReadTool(fakeConnector(f))
	out, err := tool.Run(context.Background(), json.RawMessage(`{"uid":9,"include_html":true}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"Subject: Hi", "Hello there.", "a.pdf", "BEGIN MESSAGE BODY"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if f.lastUID != 9 || !f.lastIncl {
		t.Fatalf("lastUID=%d lastIncl=%v", f.lastUID, f.lastIncl)
	}
	if !f.closed {
		t.Error("connection not closed")
	}
}

func TestEmailReadToolRequiresUID(t *testing.T) {
	tool := NewEmailReadTool(fakeConnector(&fakeEmailConn{}))
	if _, err := tool.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("expected error for missing uid")
	}
}

func TestEmailToolsPropagateConnectorError(t *testing.T) {
	connect := func(context.Context, string) (EmailConn, error) {
		return nil, errors.New("no mailbox connected")
	}
	for _, tool := range []Tool{
		NewEmailFoldersTool(connect),
		NewEmailSearchTool(connect),
		NewEmailReadTool(connect),
	} {
		var args json.RawMessage
		if tool.Name() == "email_read" {
			args = json.RawMessage(`{"uid":1}`)
		}
		if _, err := tool.Run(context.Background(), args); err == nil {
			t.Errorf("%s: expected connector error", tool.Name())
		}
	}
}

func TestEmailToolsSchemasAreValidJSON(t *testing.T) {
	conn := fakeConnector(&fakeEmailConn{})
	list := []Tool{
		NewEmailFoldersTool(conn),
		NewEmailSearchTool(conn),
		NewEmailReadTool(conn),
		NewEmailAccountsTool(nil),
		NewEmailDigestTool(conn, nil),
	}
	for _, tool := range list {
		var v any
		if err := json.Unmarshal(tool.Parameters(), &v); err != nil {
			t.Errorf("%s parameters are not valid JSON: %v", tool.Name(), err)
		}
	}
}

// TestEmailAccountsTool lists the caller's mailboxes so the model can pick one.
func TestEmailAccountsTool(t *testing.T) {
	tool := NewEmailAccountsTool([]EmailAccount{
		{ID: "a1", Label: "work", Email: "me@work.com", Default: true},
		{ID: "a2", Label: "personal", Email: "me@home.com"},
	})
	out, err := tool.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"work", "me@work.com", "id=a1", "[default]", "personal", "id=a2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "read-only") {
		t.Errorf("output missing read-only note: %q", out)
	}
}
