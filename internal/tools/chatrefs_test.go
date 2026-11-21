package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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
		"Transcript: 2 messages.",
		"message text is verbatim from storage",
		"Showing messages 1–2 of 2 (end of chat).",
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

// pageEnd reads a read_chat footer: -1 means the page ended the chat,
// otherwise the 0-based offset that continues.
func pageEnd(t *testing.T, out string) int {
	t.Helper()
	if strings.Contains(out, "(end of chat)") {
		return -1
	}
	i := strings.LastIndex(out, "offset=")
	if i < 0 {
		t.Fatalf("footer has no offset:\n%.300s", out)
	}
	i += len("offset=")
	j := i
	for j < len(out) && out[j] >= '0' && out[j] <= '9' {
		j++
	}
	n, err := strconv.Atoi(out[i:j])
	if err != nil {
		t.Fatalf("bad footer offset %q: %v", out[i:j], err)
	}
	return n
}

// TestReadChatBudgetPagesLargeMessages checks the 200k budget bounds every page,
// large messages are paged rather than silently dropped, and every message is
// still reachable.
func TestReadChatBudgetPagesLargeMessages(t *testing.T) {
	const total = 40
	msgs := make([]ChatMessage, 0, total)
	for i := 0; i < total; i++ {
		msgs = append(msgs, ChatMessage{Role: "user", Content: fmt.Sprintf("m%02d ", i) + strings.Repeat("汉", 7990)})
	}
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "c1", Title: "Big"}},
		msgs:  map[string][]ChatMessage{"c1": msgs},
	}
	offset, pages := 0, 0
	for {
		pages++
		if pages > total {
			t.Fatalf("paging did not terminate after %d pages", pages)
		}
		out, err := runTool(t, NewReadChatTool(src), fmt.Sprintf(`{"id":"c1","offset":%d}`, offset))
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if n := len([]rune(out)); n > chatBudget {
			t.Fatalf("page %d is %d runes, budget %d", pages, n, chatBudget)
		}
		if !utf8.ValidString(out) || strings.ContainsRune(out, '\uFFFD') {
			t.Fatalf("page %d is not clean UTF-8", pages)
		}
		next := pageEnd(t, out)
		if next < 0 {
			if !strings.Contains(out, fmt.Sprintf("Showing messages %d–%d of %d (end of chat).", offset+1, total, total)) {
				t.Errorf("last page footer wrong:\n%.300s", out)
			}
			break
		}
		if next <= offset {
			t.Fatalf("page %d did not advance (offset %d → %d)", pages, offset, next)
		}
		offset = next
	}
	if pages < 2 {
		t.Fatalf("40 large messages fit in %d page(s); expected several", pages)
	}
}

// TestReadChatPagingCoversEveryMessage walks a 400-message chat with the offset
// from each footer and checks the pages join up exactly: no gap, no repeat.
func TestReadChatPagingCoversEveryMessage(t *testing.T) {
	const total = 400
	msgs := make([]ChatMessage, 0, total)
	for i := 0; i < total; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, ChatMessage{Role: role, Content: fmt.Sprintf("m%03d", i)})
	}
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "c1", Title: "Long"}},
		msgs:  map[string][]ChatMessage{"c1": msgs},
	}
	var got []string
	offset, pages := 0, 0
	for {
		pages++
		if pages > 20 {
			t.Fatalf("paging did not terminate after %d pages", pages)
		}
		out, err := runTool(t, NewReadChatTool(src), fmt.Sprintf(`{"id":"c1","offset":%d}`, offset))
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if n := len([]rune(out)); n > chatBudget {
			t.Fatalf("page %d is %d runes, budget %d", pages, n, chatBudget)
		}
		for _, line := range strings.Split(out, "\n") {
			if s, ok := strings.CutPrefix(line, "User: "); ok {
				got = append(got, s)
			} else if s, ok := strings.CutPrefix(line, "Assistant: "); ok {
				got = append(got, s)
			}
		}
		if pages == 1 && !strings.Contains(out, fmt.Sprintf("Showing messages 1–%d of %d. Call read_chat again with offset=%d to continue.", chatReadDefault, total, chatReadDefault)) {
			t.Errorf("first page footer wrong:\n%.300s", out)
		}
		next := pageEnd(t, out)
		if next < 0 {
			if !strings.Contains(out, fmt.Sprintf("Showing messages %d–%d of %d (end of chat).", total-chatReadDefault+1, total, total)) {
				t.Errorf("last page footer wrong:\n%.300s", out)
			}
			break
		}
		if next <= offset {
			t.Fatalf("page %d did not advance (offset %d → %d)", pages, offset, next)
		}
		offset = next
	}
	if len(got) != total {
		t.Fatalf("read %d messages, want %d", len(got), total)
	}
	for i, s := range got {
		if want := fmt.Sprintf("m%03d", i); s != want {
			t.Fatalf("message %d = %q, want %q (gap or repeat)", i, s, want)
		}
	}
}

// TestReadChatOnePageEndFooter checks a short chat still marks the end.
func TestReadChatOnePageEndFooter(t *testing.T) {
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "c1", Title: "Short"}},
		msgs: map[string][]ChatMessage{"c1": {
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		}},
	}
	out, err := runTool(t, NewReadChatTool(src), `{"id":"c1"}`)
	if err != nil {
		t.Fatalf("read_chat: %v", err)
	}
	if !strings.Contains(out, "Showing messages 1–2 of 2 (end of chat).") {
		t.Errorf("missing end footer:\n%s", out)
	}
}

// TestReadChatOversizedTurn checks one turn larger than the whole budget is cut
// and marked, and the footer still advances past it.
func TestReadChatOversizedTurn(t *testing.T) {
	big := ChatMessage{Role: "assistant", Content: "BIG"}
	for i := 0; i < 120; i++ {
		big.Tools = append(big.Tools, ChatTool{Name: "run_bash", Status: "done", Output: strings.Repeat("x", chatOutputCap)})
	}
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "c1", Title: "Huge"}},
		msgs:  map[string][]ChatMessage{"c1": {big, {Role: "user", Content: "after the big one"}}},
	}
	out, err := runTool(t, NewReadChatTool(src), `{"id":"c1"}`)
	if err != nil {
		t.Fatalf("read_chat: %v", err)
	}
	if n := len([]rune(out)); n > chatBudget {
		t.Fatalf("oversized page is %d runes, budget %d", n, chatBudget)
	}
	if !strings.Contains(out, "…[truncated]") || !strings.Contains(out, "Message 1 alone exceeds the page budget, so only its beginning is shown.") {
		t.Errorf("oversized turn not marked:\n%.400s", out)
	}
	if !strings.Contains(out, "Showing messages 1–1 of 2. Call read_chat again with offset=1 to continue.") {
		t.Errorf("oversized footer wrong:\n%.400s", out)
	}
	if strings.Contains(out, "after the big one") {
		t.Error("page should stop before the next turn")
	}

	out, err = runTool(t, NewReadChatTool(src), `{"id":"c1","offset":1}`)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if !strings.Contains(out, "after the big one") || !strings.Contains(out, "Showing messages 2–2 of 2 (end of chat).") {
		t.Errorf("second page wrong:\n%s", out)
	}
}

// TestReadChatOffsetEdges pins down past-the-end and negative offsets (clamped
// to 0) and a non-numeric offset (a normal argument error).
func TestReadChatOffsetEdges(t *testing.T) {
	src := &fakeChatSource{
		chats: []ChatInfo{{ID: "c1", Title: "Three"}},
		msgs: map[string][]ChatMessage{"c1": {
			{Role: "user", Content: "alpha"},
			{Role: "assistant", Content: "beta"},
			{Role: "user", Content: "gamma"},
		}},
	}
	for _, arg := range []string{`{"id":"c1","offset":3}`, `{"id":"c1","offset":99}`} {
		out, err := runTool(t, NewReadChatTool(src), arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if !strings.Contains(out, "which has 3 messages") {
			t.Errorf("%s: empty page does not name the total:\n%s", arg, out)
		}
		if strings.Contains(out, "alpha") || strings.Contains(out, "gamma") {
			t.Errorf("%s: past-the-end page leaked messages", arg)
		}
	}

	out, err := runTool(t, NewReadChatTool(src), `{"id":"c1","offset":-5}`)
	if err != nil {
		t.Fatalf("negative offset: %v", err)
	}
	if !strings.Contains(out, "Showing messages 1–3 of 3 (end of chat).") || !strings.Contains(out, "alpha") {
		t.Errorf("negative offset not clamped to 0:\n%s", out)
	}

	out, err = runTool(t, NewReadChatTool(src), `{"id":"c1","offset":2}`)
	if err != nil {
		t.Fatalf("offset 2: %v", err)
	}
	if !strings.Contains(out, "Showing messages 3–3 of 3 (end of chat).") || !strings.Contains(out, "gamma") || strings.Contains(out, "alpha") {
		t.Errorf("offset 2 page wrong:\n%s", out)
	}

	_, err = runTool(t, NewReadChatTool(src), `{"id":"c1","offset":"two"}`)
	if err == nil || !strings.Contains(err.Error(), "invalid tool arguments") {
		t.Errorf("non-numeric offset error = %v", err)
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

// TestReadChatLimitClamping checks limit as a page size: default, max and
// explicit values, with offset continuing where the page ended.
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
	if !strings.Contains(out, "Transcript: 120 messages.") ||
		!strings.Contains(out, "Showing messages 1–40 of 120. Call read_chat again with offset=40 to continue.") {
		t.Errorf("default page wrong:\n%.300s", out)
	}
	if !strings.Contains(out, "m000") || strings.Contains(out, "m040") {
		t.Error("default page should start at the first message")
	}

	out, err = runTool(t, NewReadChatTool(src), `{"id":"c1","limit":9999}`)
	if err != nil {
		t.Fatalf("max limit: %v", err)
	}
	if !strings.Contains(out, "Showing messages 1–100 of 120. Call read_chat again with offset=100 to continue.") {
		t.Errorf("clamped page wrong:\n%.300s", out)
	}

	out, err = runTool(t, NewReadChatTool(src), `{"id":"c1","limit":2}`)
	if err != nil {
		t.Fatalf("limit 2: %v", err)
	}
	if !strings.Contains(out, "Showing messages 1–2 of 120.") {
		t.Errorf("explicit page wrong:\n%.300s", out)
	}

	out, err = runTool(t, NewReadChatTool(src), `{"id":"c1","offset":100,"limit":9999}`)
	if err != nil {
		t.Fatalf("last page: %v", err)
	}
	if !strings.Contains(out, "Showing messages 101–120 of 120 (end of chat).") || !strings.Contains(out, "m119") {
		t.Errorf("last page wrong:\n%.300s", out)
	}
}
