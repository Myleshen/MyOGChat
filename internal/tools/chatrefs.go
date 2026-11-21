package tools

// Chat-reference tools (6.3): let the model read the caller's *other*
// conversations when the user mentions one ("check the chat about the
// launcher"). Both tools are always registered with the rest of the tools.
//
// Access is injected through ChatSource so this package stays free of store
// types. Every implementation must be owner-scoped: a conversation belonging to
// another user has to be indistinguishable from a missing one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	chatListDefault = 20  // default list_chats limit
	chatListMax     = 50  // hard cap on list_chats
	chatReadDefault = 40  // default messages per read_chat page
	chatPageMax     = 100 // hard page limit: one read_chat call never returns more messages

	// chatMessageCap bounds one message's text (runes) so one huge turn cannot
	// crowd out the rest of the transcript.
	chatMessageCap = 8000
	// chatReasoningCap and chatToolFieldCaps bound the extra per-message
	// material. Truncation is always marked, never silent.
	chatReasoningCap = 2000
	chatArgsCap      = 1000
	chatOutputCap    = 2000
	// chatBudget bounds one read_chat page (runes), header and footer included.
	chatBudget = 200000
	// chatAmbiguousMax is how many candidates an ambiguous-title error lists.
	chatAmbiguousMax = 5
	// chatTitleCap bounds the title echoed in the read_chat header.
	chatTitleCap = 200
)

// ChatInfo is one caller-owned conversation as the tools see it.
type ChatInfo struct {
	ID        string
	Title     string
	Project   string // display name; empty when unfiled
	CreatedAt int64  // unix milliseconds
	UpdatedAt int64  // unix milliseconds
}

// ChatMessage is one stored message, including the material that was persisted
// alongside it: thinking, tool activity and file metadata (never file bytes).
type ChatMessage struct {
	Role        string
	Content     string
	Reasoning   string
	Tools       []ChatTool
	Attachments []ChatFile
}

// ChatTool is one persisted tool call from a message's history.
type ChatTool struct {
	Name       string
	Status     string // running | done | error
	Args       string
	Output     string
	Error      string
	DurationMs int64
}

// ChatFile is display metadata for a file referenced by a message (an upload,
// a generated image, or a sandbox artifact). Bytes are never exposed here.
type ChatFile struct {
	Kind string // "attached" | "image" | "artifact"
	Name string
	Mime string
	Size int64
}

// ChatSource is the read-only conversation access the chat tools need. All
// methods are scoped to the caller.
type ChatSource interface {
	// Chats lists the caller's conversations.
	Chats(ctx context.Context) ([]ChatInfo, error)
	// SearchIDs returns the caller's conversation ids matching query by title
	// or message content, most recently active first.
	SearchIDs(ctx context.Context, query string) ([]string, error)
	// Messages returns all messages of one caller-owned conversation, oldest
	// first, with their persisted thinking, tools and file metadata.
	Messages(ctx context.Context, id string) ([]ChatMessage, error)
}

// ---------- list_chats ----------

// ListChatsTool lists the caller's conversations.
type ListChatsTool struct{ src ChatSource }

// NewListChatsTool wraps src as list_chats.
func NewListChatsTool(src ChatSource) *ListChatsTool { return &ListChatsTool{src: src} }

func (t *ListChatsTool) Name() string { return "list_chats" }

func (t *ListChatsTool) Description() string {
	return "List the user's other conversations, newest first, with their ids. " +
		"Use this when the user refers to another chat (\"the launcher chat\", " +
		"\"what we decided about the paper review\") and you need to find it; " +
		"prefer it first when the reference is vague. Then call read_chat with " +
		"the id. These chats are not part of this conversation: never invent or " +
		"assume their contents — read them."
}

func (t *ListChatsTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Optional case-insensitive text matched against chat titles and message content.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Optional project name (case-insensitive), or \"unfiled\" for chats outside any project.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of chats to return (default 20, max 50).",
			},
		},
	})
}

// chatListEntry is one JSON row returned by list_chats.
type chatListEntry struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Project      *string `json:"project"`
	UpdatedAt    string  `json:"updated_at"`
	MessageCount int     `json:"message_count"`
}

func (t *ListChatsTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query   string `json:"query"`
		Project string `json:"project"`
		Limit   int    `json:"limit"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	limit := clampChatLimit(in.Limit, chatListDefault, chatListMax)

	chats, err := t.src.Chats(ctx)
	if err != nil {
		return "", err
	}
	chats = append([]ChatInfo(nil), chats...) // never mutate the source's slice
	if q := strings.TrimSpace(in.Query); q != "" {
		ids, err := t.src.SearchIDs(ctx, q)
		if err != nil {
			return "", err
		}
		match := make(map[string]bool, len(ids))
		for _, id := range ids {
			match[id] = true
		}
		filtered := chats[:0]
		for _, c := range chats {
			if match[c.ID] {
				filtered = append(filtered, c)
			}
		}
		chats = filtered
	}
	if p := strings.TrimSpace(in.Project); p != "" {
		unfiled := strings.EqualFold(p, "unfiled")
		filtered := chats[:0]
		for _, c := range chats {
			if (unfiled && c.Project == "") || (!unfiled && strings.EqualFold(c.Project, p)) {
				filtered = append(filtered, c)
			}
		}
		chats = filtered
	}
	sortChats(chats)
	if len(chats) > limit {
		chats = chats[:limit]
	}

	out := make([]chatListEntry, 0, len(chats))
	for _, c := range chats {
		msgs, err := t.src.Messages(ctx, c.ID)
		if err != nil {
			return "", err
		}
		e := chatListEntry{
			ID:           c.ID,
			Title:        c.Title,
			UpdatedAt:    formatMillis(c.UpdatedAt),
			MessageCount: len(chatTurns(msgs)),
		}
		if c.Project != "" {
			project := c.Project
			e.Project = &project
		}
		out = append(out, e)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---------- read_chat ----------

// ReadChatTool returns the transcript of one caller-owned conversation.
type ReadChatTool struct{ src ChatSource }

// NewReadChatTool wraps src as read_chat.
func NewReadChatTool(src ChatSource) *ReadChatTool { return &ReadChatTool{src: src} }

func (t *ReadChatTool) Name() string { return "read_chat" }

func (t *ReadChatTool) Description() string {
	return "Read one of the user's other conversations, by id (from list_chats) " +
		"or title, as it was stored: user and assistant text, the assistant's " +
		"thinking, the tool calls it made (with arguments, output and errors) and " +
		"the names of files involved. Use this whenever the user refers to another " +
		"chat and you need what was actually said and done there. Long chats are " +
		"paged: the footer says which messages were shown and which offset reads on. " +
		"If the reference is vague, call list_chats first. Never invent or summarize " +
		"a chat from its title: read it."
}

func (t *ReadChatTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Conversation id from list_chats. Takes precedence over title.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Conversation title. An exact (case-insensitive) match is preferred, then a case-insensitive substring; several matches return an error listing the candidates.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "0-based message index to start reading from (default 0). Use the offset in the previous result's footer to read the next page.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum messages in this page (default 40, max 100). The page may be shorter when the size budget runs out; the footer names the offset that continues.",
			},
		},
	})
}

func (t *ReadChatTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	chats, err := t.src.Chats(ctx)
	if err != nil {
		return "", err
	}
	c, err := resolveChat(chats, in.ID, in.Title)
	if err != nil {
		return "", err
	}
	msgs, err := t.src.Messages(ctx, c.ID)
	if err != nil {
		return "", err
	}
	turns := chatTurns(msgs)
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}
	limit := clampChatLimit(in.Limit, chatReadDefault, chatPageMax)
	return renderChat(c, len(turns), turns, offset, limit), nil
}

// renderChat renders one page: the fixed header, then whole turns from offset
// while they fit in chatBudget, then a footer naming the range shown and the
// offset that continues. A single turn bigger than the budget is cut at the
// budget, marked, and reported in the footer.
func renderChat(c ChatInfo, total int, turns []chatTurn, offset, limit int) string {
	header := chatHeader(c, total)
	if total == 0 {
		return header
	}
	if offset >= total {
		return header + "\n\n" + fmt.Sprintf(
			"Offset %d is past the end of this chat, which has %d messages. Call read_chat again with offset=0 to start from the beginning.",
			offset, total)
	}
	end := offset + limit
	if end > total {
		end = total
	}
	used := len([]rune(header)) + 2 // header + blank line before the body
	shown := 0
	var body strings.Builder
	for i := offset; i < end; i++ {
		block := turns[i].block()
		add := len([]rune(block))
		if shown > 0 {
			add += 2 // blank line between turns
		}
		footer := chatFooter(offset, shown+1, total, false)
		if used+add+2+len([]rune(footer)) > chatBudget {
			if shown == 0 {
				return oversizedPage(header, block, offset, total)
			}
			break
		}
		if shown > 0 {
			body.WriteString("\n\n")
		}
		body.WriteString(block)
		used += add
		shown++
	}
	return header + "\n\n" + body.String() + "\n\n" + chatFooter(offset, shown, total, false)
}

// chatFooter states the message range shown and how to continue, or that the
// chat ends here. Numbers are 1-based, so the last number shown is also the
// 0-based offset of the next message.
func chatFooter(offset, shown, total int, oversized bool) string {
	first, last := offset+1, offset+shown
	var s string
	if last >= total {
		s = fmt.Sprintf("Showing messages %d–%d of %d (end of chat).", first, total, total)
	} else {
		s = fmt.Sprintf("Showing messages %d–%d of %d. Call read_chat again with offset=%d to continue.", first, last, total, last)
	}
	if oversized {
		s += fmt.Sprintf(" Message %d alone exceeds the page budget, so only its beginning is shown.", first)
	}
	return s
}

// oversizedPage renders the head of a single turn that cannot fit the budget.
// The footer advances past it so paging still terminates.
func oversizedPage(header, block string, offset, total int) string {
	footer := chatFooter(offset, 1, total, true)
	sep := len([]rune("\n\n"))
	mark := " …[truncated]"
	room := chatBudget - len([]rune(header)) - 2*sep - len([]rune(footer)) - len([]rune(mark))
	if room < 1 {
		return header + "\n\n" + footer
	}
	return header + "\n\n" + truncateRunes(block, room) + mark + "\n\n" + footer
}

// chatHeader is the fixed preamble of a transcript: identity, timestamps, how
// many messages it has, and how faithful the body is. The footer, not the
// header, reports the page range.
func chatHeader(c ChatInfo, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Chat: %q (id %s)\n", truncateRunes(c.Title, chatTitleCap), c.ID)
	if c.Project != "" {
		fmt.Fprintf(&b, "Project: %s\n", c.Project)
	} else {
		b.WriteString("Project: unfiled\n")
	}
	fmt.Fprintf(&b, "Updated: %s; started %s\n", formatMillis(c.UpdatedAt), formatMillis(c.CreatedAt))
	if total == 0 {
		b.WriteString("Transcript: no messages yet.\n")
	} else {
		fmt.Fprintf(&b, "Transcript: %d messages.\n", total)
	}
	b.WriteString("Note: message text is verbatim from storage; tool lines are what the sandbox/API actually returned; " +
		"thinking may be truncated; attachments/artifacts are file metadata only (no file contents).")
	return b.String()
}

// chatTurn is one transcript entry: a user or assistant message plus whatever
// was persisted with it.
type chatTurn struct {
	role      string
	text      string
	reasoning string
	tools     []ChatTool
	files     []ChatFile
}

// block renders one turn: the message, then thinking, tool calls and files.
func (t chatTurn) block() string {
	var b strings.Builder
	b.WriteString(t.role)
	b.WriteString(": ")
	b.WriteString(capText(t.text, chatMessageCap))
	if r := capText(t.reasoning, chatReasoningCap); r != "" {
		b.WriteString("\n  Thinking: ")
		b.WriteString(r)
	}
	for _, tool := range t.tools {
		b.WriteString("\n  ")
		b.WriteString(tool.line())
	}
	if len(t.files) > 0 {
		b.WriteString("\n  Files: ")
		parts := make([]string, 0, len(t.files))
		for _, f := range t.files {
			parts = append(parts, fmt.Sprintf("%s: %s (%s, %d bytes)", f.Kind, f.Name, f.Mime, f.Size))
		}
		b.WriteString(strings.Join(parts, "; "))
	}
	return b.String()
}

// line renders one tool call: name, status and timing, then arguments, output
// and any error — each capped and always marked when cut.
func (t ChatTool) line() string {
	status := t.Status
	if status == "" {
		status = "done"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Tool: %s · %s · %dms", t.Name, status, t.DurationMs)
	if args := capText(t.Args, chatArgsCap); args != "" {
		b.WriteString("\n    args: ")
		b.WriteString(args)
	}
	if out := capText(t.Output, chatOutputCap); out != "" {
		b.WriteString("\n    output: ")
		b.WriteString(out)
	}
	if err := capText(t.Error, chatOutputCap); err != "" {
		b.WriteString("\n    error: ")
		b.WriteString(err)
	}
	return b.String()
}

// chatTurns keeps the transcript-eligible messages: user/assistant rows that
// carry anything (text, thinking, tool activity or files). System prompts and
// empty rows are dropped.
func chatTurns(msgs []ChatMessage) []chatTurn {
	turns := make([]chatTurn, 0, len(msgs))
	for _, m := range msgs {
		var role string
		switch m.Role {
		case "user":
			role = "User"
		case "assistant":
			role = "Assistant"
		default:
			continue
		}
		if strings.TrimSpace(m.Content) == "" && strings.TrimSpace(m.Reasoning) == "" &&
			len(m.Tools) == 0 && len(m.Attachments) == 0 {
			continue
		}
		turns = append(turns, chatTurn{
			role: role, text: m.Content, reasoning: m.Reasoning,
			tools: m.Tools, files: m.Attachments,
		})
	}
	return turns
}

// capText trims text and truncates it to n runes with a visible marker.
func capText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > n {
		s = truncateRunes(s, n) + " …[truncated]"
	}
	return s
}

// resolveChat finds the conversation named by id (preferred) or title. Title
// resolution prefers an exact case-insensitive match, then a case-insensitive
// substring; several matches are an error listing the candidates, never a guess.
func resolveChat(chats []ChatInfo, id, title string) (ChatInfo, error) {
	if id = strings.TrimSpace(id); id != "" {
		for _, c := range chats {
			if c.ID == id {
				return c, nil
			}
		}
		return ChatInfo{}, fmt.Errorf("no conversation with id %q; use list_chats to find it", id)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return ChatInfo{}, errors.New(`either "id" or "title" is required`)
	}
	sorted := append([]ChatInfo(nil), chats...)
	sortChats(sorted)

	var exact, partial []ChatInfo
	for _, c := range sorted {
		switch {
		case strings.EqualFold(strings.TrimSpace(c.Title), title):
			exact = append(exact, c)
		case strings.Contains(strings.ToLower(c.Title), strings.ToLower(title)):
			partial = append(partial, c)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return ChatInfo{}, ambiguousChats(title, exact)
	}
	switch len(partial) {
	case 1:
		return partial[0], nil
	case 0:
		return ChatInfo{}, fmt.Errorf("no conversation titled %q; use list_chats to find it", title)
	default:
		return ChatInfo{}, ambiguousChats(title, partial)
	}
}

// ambiguousChats reports several title matches with up to chatAmbiguousMax
// candidates, so the model can pick one by id.
func ambiguousChats(title string, candidates []ChatInfo) error {
	parts := make([]string, 0, chatAmbiguousMax)
	for i, c := range candidates {
		if i == chatAmbiguousMax {
			break
		}
		parts = append(parts, fmt.Sprintf("%q (id %s)", c.Title, c.ID))
	}
	msg := fmt.Sprintf("%d conversations match %q: %s", len(candidates), title, strings.Join(parts, ", "))
	if len(candidates) > chatAmbiguousMax {
		msg += fmt.Sprintf(", and %d more", len(candidates)-chatAmbiguousMax)
	}
	return errors.New(msg + "; call read_chat again with one id")
}

// sortChats orders conversations newest first.
func sortChats(chats []ChatInfo) {
	sort.SliceStable(chats, func(i, j int) bool { return chats[i].UpdatedAt > chats[j].UpdatedAt })
}

// clampChatLimit applies a default and a hard cap to a model-supplied limit.
func clampChatLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// formatMillis renders a unix-millisecond timestamp as RFC3339 UTC.
func formatMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
