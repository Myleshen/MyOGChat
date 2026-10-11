package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"myogchat/internal/mail"
)

// digestConn is a small in-memory EmailConn for digest tests.
type digestConn struct {
	msgs      []mail.MessageSummary
	lastQuery mail.SearchQuery
}

func (d *digestConn) Folders(context.Context) ([]mail.Folder, error) { return nil, nil }
func (d *digestConn) Search(_ context.Context, q mail.SearchQuery) ([]mail.MessageSummary, error) {
	d.lastQuery = q
	return d.msgs, nil
}
func (d *digestConn) Read(context.Context, uint32, string, bool) (*mail.Message, error) {
	return nil, nil
}
func (d *digestConn) Close() error { return nil }

// mapConnector returns a connector that maps an account id to its connection and
// records the call order.
func mapConnector(conns map[string]*digestConn, order *[]string) EmailConnector {
	return func(_ context.Context, account string) (EmailConn, error) {
		*order = append(*order, account)
		if c, ok := conns[account]; ok {
			return c, nil
		}
		return &digestConn{}, nil
	}
}

func accounts(ids ...string) []EmailAccount {
	out := make([]EmailAccount, 0, len(ids))
	for i, id := range ids {
		out = append(out, EmailAccount{ID: id, Label: id, Email: id + "@example.com", Default: i == 0})
	}
	return out
}

// TestEmailDigestRangeWindow checks the server-side range resolution and the
// widened IMAP window + precise client-side Date filter.
func TestEmailDigestRangeWindow(t *testing.T) {
	loc := time.UTC
	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	conn := &digestConn{msgs: []mail.MessageSummary{
		{UID: 1, Folder: "INBOX", Subject: "in window", Date: dayStart.Add(9 * time.Hour)},
		{UID: 2, Folder: "INBOX", Subject: "yesterday", Date: dayStart.Add(-12 * time.Hour)},
		{UID: 3, Folder: "INBOX", Subject: "no date"},
	}}
	order := []string{}
	tool := NewEmailDigestTool(mapConnector(map[string]*digestConn{"a1": conn}, &order), accounts("a1"))

	out, err := tool.Run(context.Background(), json.RawMessage(`{"range":"today","timezone":"UTC"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Widened IMAP window: [today-1d, tomorrow+1d) in day granularity.
	wantSince := dayStart.AddDate(0, 0, -1)
	wantBefore := dayStart.AddDate(0, 0, 2)
	if !conn.lastQuery.Since.Equal(wantSince) {
		t.Errorf("Since = %v, want %v", conn.lastQuery.Since, wantSince)
	}
	if !conn.lastQuery.Before.Equal(wantBefore) {
		t.Errorf("Before = %v, want %v", conn.lastQuery.Before, wantBefore)
	}
	if conn.lastQuery.PreviewBytes != digestPreviewBytes {
		t.Errorf("PreviewBytes = %d, want %d", conn.lastQuery.PreviewBytes, digestPreviewBytes)
	}
	// The precise filter keeps in-window + zero-date, drops yesterday.
	if !strings.Contains(out, "uid=1") || !strings.Contains(out, "uid=3") {
		t.Errorf("digest missing in-window messages:\n%s", out)
	}
	if strings.Contains(out, "uid=2") {
		t.Errorf("digest kept an out-of-window message:\n%s", out)
	}
	if !strings.Contains(out, "2 message(s)") {
		t.Errorf("header count wrong:\n%s", out)
	}
}

// TestEmailDigestRangeVariants verifies each named range resolves as expected.
func TestEmailDigestRangeVariants(t *testing.T) {
	loc := time.UTC
	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	cases := []struct {
		rangeArg string
		wantFrom time.Time
		wantTo   time.Time
		unread   bool
	}{
		{"yesterday", dayStart.AddDate(0, 0, -2), dayStart.AddDate(0, 0, 1), false},
		{"last_7_days", dayStart.AddDate(0, 0, -7), dayStart.AddDate(0, 0, 2), false},
		{"unread", time.Time{}, time.Time{}, true},
		{"all", time.Time{}, time.Time{}, false},
	}
	for _, tc := range cases {
		conn := &digestConn{}
		tool := NewEmailDigestTool(mapConnector(map[string]*digestConn{"a1": conn}, &[]string{}), accounts("a1"))
		if _, err := tool.Run(context.Background(), json.RawMessage(`{"range":"`+tc.rangeArg+`","timezone":"UTC"}`)); err != nil {
			t.Fatalf("%s: Run: %v", tc.rangeArg, err)
		}
		if !conn.lastQuery.Since.Equal(tc.wantFrom) {
			t.Errorf("%s: Since = %v, want %v", tc.rangeArg, conn.lastQuery.Since, tc.wantFrom)
		}
		if !conn.lastQuery.Before.Equal(tc.wantTo) {
			t.Errorf("%s: Before = %v, want %v", tc.rangeArg, conn.lastQuery.Before, tc.wantTo)
		}
		if conn.lastQuery.UnreadOnly != tc.unread {
			t.Errorf("%s: UnreadOnly = %v, want %v", tc.rangeArg, conn.lastQuery.UnreadOnly, tc.unread)
		}
	}

	// An unknown range is an error; an unknown timezone too.
	conn := &digestConn{}
	tool := NewEmailDigestTool(mapConnector(map[string]*digestConn{"a1": conn}, &[]string{}), accounts("a1"))
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"range":"fortnight"}`)); err == nil {
		t.Error("expected error for unknown range")
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"range":"today","timezone":"Mars/Olympus"}`)); err == nil {
		t.Error("expected error for unknown timezone")
	}
}

// TestEmailDigestFormattingAndMarkers checks deterministic unread/flagged/automated
// markers and the compact formatting.
func TestEmailDigestFormattingAndMarkers(t *testing.T) {
	loc := time.UTC
	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	conn := &digestConn{msgs: []mail.MessageSummary{
		{
			UID: 10, Folder: "INBOX", Subject: "Q3 invoice",
			From: []mail.Address{{Name: "Priya Shah", Email: "priya@acme.io"}},
			Date: dayStart.Add(9 * time.Hour), Flags: []string{"\\Flagged"},
			Snippet: "Please approve the Q3 invoice.",
		},
		{
			UID: 11, Folder: "INBOX", Subject: "Newsletter",
			From: []mail.Address{{Email: "no-reply@example.com"}},
			Date: dayStart.Add(8 * time.Hour), Flags: []string{"\\Seen"},
		},
	}}
	tool := NewEmailDigestTool(mapConnector(map[string]*digestConn{"a1": conn}, &[]string{}), accounts("a1"))
	out, err := tool.Run(context.Background(), json.RawMessage(`{"range":"today","timezone":"UTC","account":"a1"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"●", "⚑", "[automated]", "Priya Shah <priya@acme.io>", "Q3 invoice", "snippet:", "## a1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "2 message(s) (1 unread, 1 flagged)") {
		t.Errorf("header missing markers summary:\n%s", out)
	}
}

// TestEmailDigestCaps checks the 30-message cap.
func TestEmailDigestCaps(t *testing.T) {
	loc := time.UTC
	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	msgs := make([]mail.MessageSummary, 0, 40)
	for i := 0; i < 40; i++ {
		msgs = append(msgs, mail.MessageSummary{
			UID: uint32(i + 1), Folder: "INBOX", Subject: "m",
			Date: dayStart.Add(time.Duration(i) * time.Minute),
		})
	}
	conn := &digestConn{msgs: msgs}
	tool := NewEmailDigestTool(mapConnector(map[string]*digestConn{"a1": conn}, &[]string{}), accounts("a1"))
	out, err := tool.Run(context.Background(), json.RawMessage(`{"range":"today","timezone":"UTC","limit":30,"snippet":false}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := strings.Count(out, "- uid="); n != digestMaxMessages {
		t.Errorf("rendered %d messages, want %d", n, digestMaxMessages)
	}
	if !strings.Contains(out, "showing "+strconv.Itoa(digestMaxMessages)+" of 40") {
		t.Errorf("caps footer missing:\n%s", out)
	}
}

// TestEmailDigestAccountScoping checks that the digest spans every mailbox by
// default and narrows to one when account is given.
func TestEmailDigestAccountScoping(t *testing.T) {
	loc := time.UTC
	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	msg := mail.MessageSummary{UID: 1, Folder: "INBOX", Subject: "hi", Date: dayStart.Add(time.Hour)}
	conns := map[string]*digestConn{
		"a1": {msgs: []mail.MessageSummary{msg}},
		"a2": {msgs: []mail.MessageSummary{msg}},
	}

	order := []string{}
	tool := NewEmailDigestTool(mapConnector(conns, &order), accounts("a1", "a2"))
	out, err := tool.Run(context.Background(), json.RawMessage(`{"range":"today","timezone":"UTC"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 2 {
		t.Errorf("all-mailbox digest connected %v, want both", order)
	}
	if !strings.Contains(out, "## a1") || !strings.Contains(out, "## a2") {
		t.Errorf("digest should group per mailbox:\n%s", out)
	}

	order = nil
	out, err = tool.Run(context.Background(), json.RawMessage(`{"range":"today","timezone":"UTC","account":"a2"}`))
	if err != nil {
		t.Fatalf("Run(account): %v", err)
	}
	if len(order) != 1 || order[0] != "a2" {
		t.Errorf("narrowed digest connected %v, want [a2]", order)
	}
	if strings.Contains(out, "## a1") {
		t.Errorf("narrowed digest included a1:\n%s", out)
	}

	// Unknown account ref → error.
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"account":"ghost"}`)); err == nil {
		t.Error("expected error for unknown account")
	}
}
