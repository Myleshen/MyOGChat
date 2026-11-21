package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeChatSource is an in-memory tools.ChatSource for the chat-reference tests.
type fakeChatSource struct {
	chats []ChatInfo
	msgs  map[string][]ChatMessage
	hits  map[string][]string
}

func (f *fakeChatSource) Chats(context.Context) ([]ChatInfo, error) { return f.chats, nil }

func (f *fakeChatSource) SearchIDs(_ context.Context, query string) ([]string, error) {
	return f.hits[query], nil
}

func (f *fakeChatSource) Messages(_ context.Context, id string) ([]ChatMessage, error) {
	return f.msgs[id], nil
}

func runTool(t *testing.T, tool Tool, args string) (string, error) {
	t.Helper()
	return tool.Run(context.Background(), json.RawMessage(args))
}

// TestListChatsFormatsAndSorts checks the JSON shape, newest-first order,
// project names, RFC3339 timestamps and message counts.
func TestListChatsFormatsAndSorts(t *testing.T) {
	src := &fakeChatSource{
		chats: []ChatInfo{
			{ID: "old", Title: "Old chat", CreatedAt: 1_700_000_000_000, UpdatedAt: 1_700_000_100_000},
			{ID: "new", Title: "New chat", Project: "Launcher", CreatedAt: 1_700_000_000_000, UpdatedAt: 1_700_000_900_000},
		},
		msgs: map[string][]ChatMessage{
			"old": {{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
			"new": {
				{Role: "system", Content: "internal"},
				{Role: "user", Content: "q"},
				{Role: "assistant", Content: ""},
				{Role: "assistant", Content: "a"},
			},
		},
	}
	out, err := runTool(t, NewListChatsTool(src), `{}`)
	if err != nil {
		t.Fatalf("list_chats: %v", err)
	}
	want := `[{"id":"new","title":"New chat","project":"Launcher","updated_at":"2023-11-14T22:28:20Z","message_count":2},` +
		`{"id":"old","title":"Old chat","project":null,"updated_at":"2023-11-14T22:15:00Z","message_count":2}]`
	if out != want {
		t.Errorf("list_chats = %s\nwant %s", out, want)
	}
}

// TestListChatsQueryAndProject covers the query join and the project filter.
func TestListChatsQueryAndProject(t *testing.T) {
	src := &fakeChatSource{
		chats: []ChatInfo{
			{ID: "a", Title: "Launcher plan", Project: "Launcher", UpdatedAt: 3},
			{ID: "b", Title: "Paper review", Project: "Papers", UpdatedAt: 2},
			{ID: "c", Title: "Misc", UpdatedAt: 1},
		},
		msgs: map[string][]ChatMessage{},
		hits: map[string][]string{"launcher": {"a", "c"}},
	}
	// query joins the existing search (title or content hit ids).
	out, err := runTool(t, NewListChatsTool(src), `{"query":"launcher"}`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var got []chatListEntry
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("query results = %+v, want ids a,c", got)
	}

	out, err = runTool(t, NewListChatsTool(src), `{"project":"papers"}`)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if !strings.Contains(out, `"id":"b"`) || strings.Contains(out, `"id":"a"`) {
		t.Errorf("project filter = %s, want only b", out)
	}

	out, err = runTool(t, NewListChatsTool(src), `{"project":"unfiled"}`)
	if err != nil {
		t.Fatalf("unfiled: %v", err)
	}
	if !strings.Contains(out, `"id":"c"`) || strings.Contains(out, `"id":"a"`) {
		t.Errorf("unfiled filter = %s, want only c", out)
	}

	out, err = runTool(t, NewListChatsTool(src), `{"query":"missing"}`)
	if err != nil || out != "[]" {
		t.Errorf("no hits = %q, %v; want []", out, err)
	}
}

// TestListChatsLimitClamping checks the default, explicit and max limits.
func TestListChatsLimitClamping(t *testing.T) {
	src := &fakeChatSource{chats: nil, msgs: map[string][]ChatMessage{}}
	for i := 0; i < 60; i++ {
		src.chats = append(src.chats, ChatInfo{ID: fmt.Sprintf("c%02d", i), Title: "t", UpdatedAt: int64(i)})
	}
	for _, tc := range []struct {
		arg  string
		want int
	}{
		{`{}`, chatListDefault},
		{`{"limit":3}`, 3},
		{`{"limit":-1}`, chatListDefault},
		{`{"limit":999}`, chatListMax},
	} {
		out, err := runTool(t, NewListChatsTool(src), tc.arg)
		if err != nil {
			t.Fatalf("%s: %v", tc.arg, err)
		}
		var got []chatListEntry
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%s: %v", tc.arg, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s returned %d chats, want %d", tc.arg, len(got), tc.want)
		}
	}
}

// TestReadChatIncludesEverything checks reasoning, tool activity, file metadata
// and the header's provenance note.
func TestReadChatIncludesEverything(t *testing.T) {
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "c1", Title: "Launcher", Project: "Home", CreatedAt: 1000, UpdatedAt: 2000}},
		msgs: map[string][]ChatMessage{"c1": {
			{Role: "user", Content: "build the launcher",
				Attachments: []ChatFile{{Kind: "attached", Name: "spec.pdf", Mime: "application/pdf", Size: 1234}}},
			{Role: "assistant", Content: "Built it.",
				Reasoning: "I checked the logs first.",
				Tools: []ChatTool{
					{Name: "run_bash", Status: "done", Args: `{"cmd":"ls"}`, Output: "a.txt", DurationMs: 120},
					{Name: "fetch_url", Status: "error", Error: "timed out", DurationMs: 500},
				},
				Attachments: []ChatFile{
					{Kind: "image", Name: "a sunset", Mime: "image/png", Size: 2048},
					{Kind: "artifact", Name: "out.csv", Mime: "text/csv", Size: 99},
				}},
		}},
	}
	out, err := runTool(t, NewReadChatTool(src), `{"title":"launcher"}`)
	if err != nil {
		t.Fatalf("read_chat: %v", err)
	}
	for _, want := range []string{
		`Chat: "Launcher" (id c1)`,
		"Project: Home",
		"Updated: 1970-01-01T00:00:02Z; started 1970-01-01T00:00:01Z",
		"Transcript: all 2 messages (2 tool call(s)).",
		"message text is verbatim from storage",
		"User: build the launcher",
		"Assistant: Built it.",
		"  Thinking: I checked the logs first.",
		"  Tool: run_bash · done · 120ms",
		`    args: {"cmd":"ls"}`,
		"    output: a.txt",
		"  Tool: fetch_url · error · 500ms",
		"    error: timed out",
		"Files: attached: spec.pdf (application/pdf, 1234 bytes)",
		"image: a sunset (image/png, 2048 bytes)",
		"artifact: out.csv (text/csv, 99 bytes)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("read_chat output missing %q:\n%s", want, out)
		}
	}
}

// TestReadChatTruncationMarks checks every cap leaves a visible marker and
// stays on rune boundaries.
func TestReadChatTruncationMarks(t *testing.T) {
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "c1", Title: "Long"}},
		msgs: map[string][]ChatMessage{"c1": {{
			Role:      "assistant",
			Content:   strings.Repeat("汉", chatMessageCap+10),
			Reasoning: strings.Repeat("é", chatReasoningCap+10),
			Tools: []ChatTool{{
				Name: "run_bash", Status: "done",
				Args:   strings.Repeat("a", chatArgsCap+10),
				Output: strings.Repeat("b", chatOutputCap+10),
				Error:  strings.Repeat("c", chatOutputCap+10),
			}},
		}}},
	}
	out, err := runTool(t, NewReadChatTool(src), `{"id":"c1"}`)
	if err != nil {
		t.Fatalf("read_chat: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Fatal("output is not valid UTF-8")
	}
	if strings.ContainsRune(out, '\uFFFD') {
		t.Error("output contains a replacement rune (mid-rune cut)")
	}
	for _, want := range []string{
		"Assistant: " + strings.Repeat("汉", chatMessageCap) + " …[truncated]",
		"  Thinking: " + strings.Repeat("é", chatReasoningCap) + " …[truncated]",
		"    args: " + strings.Repeat("a", chatArgsCap) + " …[truncated]",
		"    output: " + strings.Repeat("b", chatOutputCap) + " …[truncated]",
		"    error: " + strings.Repeat("c", chatOutputCap) + " …[truncated]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing capped field %q (len %d)", want[:20], len(want))
		}
	}
}

// TestReadChatBudgetTrimsOldest checks the 48k budget drops whole old messages
// and reports exactly how many were omitted.
func TestReadChatBudgetTrimsOldest(t *testing.T) {
	msgs := make([]ChatMessage, 0, 8)
	for i := 0; i < 8; i++ {
		msgs = append(msgs, ChatMessage{Role: "user", Content: fmt.Sprintf("m%d ", i) + strings.Repeat("汉", 8000)})
	}
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "c1", Title: "Big"}},
		msgs:  map[string][]ChatMessage{"c1": msgs},
	}
	out, err := runTool(t, NewReadChatTool(src), `{"id":"c1"}`)
	if err != nil {
		t.Fatalf("read_chat: %v", err)
	}
	if n := len([]rune(out)); n > chatBudget {
		t.Fatalf("output is %d runes, budget %d", n, chatBudget)
	}
	if !utf8.ValidString(out) {
		t.Fatal("output is not valid UTF-8")
	}
	if strings.ContainsRune(out, '\uFFFD') {
		t.Error("output contains a replacement rune (mid-rune cut)")
	}
	if strings.Contains(out, "m0 ") {
		t.Error("oldest message should have been trimmed")
	}
	// 8 messages at ~8000 runes each: 5 fit in 48k, so 3 are omitted.
	if !strings.Contains(out, "3 earlier messages omitted") {
		t.Errorf("header does not report the omitted count:\n%.300s", out)
	}
	if !strings.Contains(out, "m7 ") {
		t.Error("newest message missing")
	}
	if strings.Count(out, "User: ") != 5 {
		t.Errorf("kept %d messages, want 5", strings.Count(out, "User: "))
	}
}

// TestReadChatTitleResolution covers exact, partial, ambiguous and missing titles.
func TestReadChatTitleResolution(t *testing.T) {
	src := &fakeChatSource{
		chats: []ChatInfo{
			{ID: "a", Title: "Paper review", UpdatedAt: 3},
			{ID: "b", Title: "Paper review notes", UpdatedAt: 2},
			{ID: "c", Title: "Launcher", UpdatedAt: 1},
		},
		msgs: map[string][]ChatMessage{
			"a": {{Role: "user", Content: "exact"}},
			"b": {{Role: "user", Content: "partial"}},
			"c": {{Role: "user", Content: "launcher"}},
		},
	}
	out, err := runTool(t, NewReadChatTool(src), `{"title":"paper REVIEW"}`)
	if err != nil || !strings.Contains(out, "exact") {
		t.Errorf("exact match = %.80q, %v", out, err)
	}
	out, err = runTool(t, NewReadChatTool(src), `{"title":"launcher"}`)
	if err != nil || !strings.Contains(out, "launcher") {
		t.Errorf("case-insensitive match = %.80q, %v", out, err)
	}
	_, err = runTool(t, NewReadChatTool(src), `{"title":"paper"}`)
	if err == nil || !strings.Contains(err.Error(), `"Paper review" (id a)`) ||
		!strings.Contains(err.Error(), `"Paper review notes" (id b)`) {
		t.Errorf("ambiguous error = %v", err)
	}
	_, err = runTool(t, NewReadChatTool(src), `{"title":"nothing"}`)
	if err == nil || !strings.Contains(err.Error(), "no conversation titled") {
		t.Errorf("missing title error = %v", err)
	}
	_, err = runTool(t, NewReadChatTool(src), `{}`)
	if err == nil {
		t.Error("missing id+title should error")
	}
}

// TestReadChatAmbiguousCapsCandidates lists at most five candidates.
func TestReadChatAmbiguousCapsCandidates(t *testing.T) {
	src := &fakeChatSource{chats: nil, msgs: map[string][]ChatMessage{}}
	for i := 0; i < 7; i++ {
		src.chats = append(src.chats, ChatInfo{ID: fmt.Sprintf("w%d", i), Title: "Weekly sync", UpdatedAt: int64(i)})
	}
	_, err := runTool(t, NewReadChatTool(src), `{"title":"weekly"}`)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if n := strings.Count(err.Error(), "(id "); n != chatAmbiguousMax {
		t.Errorf("listed %d candidates, want %d: %v", n, chatAmbiguousMax, err)
	}
	if !strings.Contains(err.Error(), "and 2 more") {
		t.Errorf("missing overflow note: %v", err)
	}
}

// TestReadChatEmptyAndUnknown covers an empty chat and ids that are not the
// caller's (another user's chat must look missing).
func TestReadChatEmptyAndUnknown(t *testing.T) {
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "empty", Title: "Empty chat"}},
		msgs:  map[string][]ChatMessage{"empty": nil},
	}
	out, err := runTool(t, NewReadChatTool(src), `{"id":"empty"}`)
	if err != nil {
		t.Fatalf("empty chat: %v", err)
	}
	if !strings.Contains(out, "no messages yet") {
		t.Errorf("empty chat output = %q", out)
	}
	_, err = runTool(t, NewReadChatTool(src), `{"id":"someone-elses"}`)
	if err == nil || !strings.Contains(err.Error(), "no conversation with id") {
		t.Errorf("foreign id error = %v", err)
	}
}

// TestReadChatLimitClamping checks the default and max message windows.
func TestReadChatLimitClamping(t *testing.T) {
	msgs := make([]ChatMessage, 0, 120)
	for i := 0; i < 120; i++ {
		msgs = append(msgs, ChatMessage{Role: "user", Content: fmt.Sprintf("m%03d", i)})
	}
	src := &fakeChatSource{chats: []ChatInfo{{ID: "c1", Title: "Many"}}, msgs: map[string][]ChatMessage{"c1": msgs}}

	out, err := runTool(t, NewReadChatTool(src), `{"id":"c1"}`)
	if err != nil {
		t.Fatalf("default limit: %v", err)
	}
	if !strings.Contains(out, "the last 40 of 120 messages") {
		t.Errorf("default window wrong:\n%.200s", out)
	}
	if !strings.Contains(out, "m119") || strings.Contains(out, "m079") {
		t.Error("default window should be the newest 40")
	}

	out, err = runTool(t, NewReadChatTool(src), `{"id":"c1","limit":9999}`)
	if err != nil {
		t.Fatalf("max limit: %v", err)
	}
	if !strings.Contains(out, "the last 100 of 120 messages") {
		t.Errorf("clamped window wrong:\n%.200s", out)
	}

	out, err = runTool(t, NewReadChatTool(src), `{"id":"c1","limit":2}`)
	if err != nil {
		t.Fatalf("limit 2: %v", err)
	}
	if !strings.Contains(out, "the last 2 of 120 messages") {
		t.Errorf("explicit window wrong:\n%.200s", out)
	}
}
