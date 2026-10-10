package mail

import (
	"bytes"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"
	gomail "github.com/emersion/go-message/mail"
)

// Caps applied to everything returned to the model. Bodies are truncated to
// protect the Pi's memory and the model context window; attachment metadata is
// returned but attachment bytes are never fetched.
const (
	// MaxBodyBytes caps a returned text/HTML body (100 KB).
	MaxBodyBytes = 100 * 1024
	// maxFetchBytes bounds a single message download (512 KB). A partial
	// BODY.PEEK[] fetch means a huge attachment can never be pulled in full.
	maxFetchBytes = 512 * 1024
	// SnippetChars is how much body text a summary/search result carries.
	SnippetChars = 200
)

// Address is a normalized mail address.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Folder is a mailbox as returned by LIST.
type Folder struct {
	Name       string   `json:"name"`
	Delimiter  string   `json:"delimiter,omitempty"`
	Attrs      []string `json:"attrs,omitempty"`
	SpecialUse string   `json:"special_use,omitempty"` // sent | drafts | junk | trash | archive | all | flagged | important
}

// MessageSummary is the metadata returned by email_search.
type MessageSummary struct {
	UID     uint32    `json:"uid"`
	Folder  string    `json:"folder"`
	From    []Address `json:"from,omitempty"`
	To      []Address `json:"to,omitempty"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	Flags   []string  `json:"flags,omitempty"`
	Size    int64     `json:"size"`
	Snippet string    `json:"snippet,omitempty"`
}

// Attachment is attachment metadata (never the bytes).
type Attachment struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
}

// Message is a normalized, read-only view of one message.
type Message struct {
	UID         uint32       `json:"uid"`
	Folder      string       `json:"folder"`
	MessageID   string       `json:"message_id,omitempty"`
	From        []Address    `json:"from,omitempty"`
	To          []Address    `json:"to,omitempty"`
	Cc          []Address    `json:"cc,omitempty"`
	Subject     string       `json:"subject"`
	Date        time.Time    `json:"date"`
	Flags       []string     `json:"flags,omitempty"`
	Size        int64        `json:"size"`
	BodyText    string       `json:"body_text"`
	BodyHTML    string       `json:"body_html,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// SearchQuery is the normalized search request built from the email_search
// tool arguments.
type SearchQuery struct {
	Query      string
	From       string
	To         string
	Subject    string
	Since      time.Time
	Before     time.Time
	UnreadOnly bool
	Folder     string
	Limit      int
}

// searchCriteria translates a SearchQuery into standard IMAP SEARCH keys.
// (Gmail's X-GM-RAW syntax is intentionally not used: the standard keys work
// everywhere and keep the wrapper free of raw command injection.)
func searchCriteria(q SearchQuery) *imap.SearchCriteria {
	crit := &imap.SearchCriteria{}
	if s := strings.TrimSpace(q.From); s != "" {
		crit.Header = append(crit.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: s})
	}
	if s := strings.TrimSpace(q.To); s != "" {
		crit.Header = append(crit.Header, imap.SearchCriteriaHeaderField{Key: "To", Value: s})
	}
	if s := strings.TrimSpace(q.Subject); s != "" {
		crit.Header = append(crit.Header, imap.SearchCriteriaHeaderField{Key: "Subject", Value: s})
	}
	if s := strings.TrimSpace(q.Query); s != "" {
		crit.Text = append(crit.Text, s)
	}
	if !q.Since.IsZero() {
		crit.Since = q.Since
	}
	if !q.Before.IsZero() {
		crit.Before = q.Before
	}
	if q.UnreadOnly {
		crit.NotFlag = append(crit.NotFlag, imap.FlagSeen)
	}
	return crit
}

// normalizeFolder returns the folder to open, defaulting to INBOX.
func normalizeFolder(folder string) string {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return "INBOX"
	}
	return folder
}

// normalizeLimit clamps a requested result count into [1, MaxSearchLimit].
func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		return MaxSearchLimit
	}
	return limit
}

// folderFromListData converts a LIST response into a Folder.
func folderFromListData(d *imap.ListData) Folder {
	f := Folder{Name: d.Mailbox}
	if d.Delim != 0 {
		f.Delimiter = string(d.Delim)
	}
	for _, a := range d.Attrs {
		f.Attrs = append(f.Attrs, string(a))
		if su := specialUse(a); su != "" {
			f.SpecialUse = su
		}
	}
	return f
}

// specialUse maps an RFC 6154 mailbox attribute to a short name.
func specialUse(a imap.MailboxAttr) string {
	switch a {
	case imap.MailboxAttrSent:
		return "sent"
	case imap.MailboxAttrDrafts:
		return "drafts"
	case imap.MailboxAttrJunk:
		return "junk"
	case imap.MailboxAttrTrash:
		return "trash"
	case imap.MailboxAttrArchive:
		return "archive"
	case imap.MailboxAttrAll:
		return "all"
	case imap.MailboxAttrFlagged:
		return "flagged"
	case imap.MailboxAttrImportant:
		return "important"
	default:
		return ""
	}
}

// addresses converts an IMAP address list to the normalized form.
func addresses(in []imap.Address) []Address {
	out := make([]Address, 0, len(in))
	for i := range in {
		if in[i].IsGroupStart() || in[i].IsGroupEnd() {
			continue
		}
		email := in[i].Addr()
		if email == "" && strings.TrimSpace(in[i].Name) == "" {
			continue
		}
		out = append(out, Address{Name: strings.TrimSpace(in[i].Name), Email: email})
	}
	return out
}

// flagsToStrings converts IMAP flags to plain strings.
func flagsToStrings(in []imap.Flag) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		out = append(out, string(f))
	}
	return out
}

// attachmentsFromBodyStructure extracts attachment metadata from a BODYSTRUCTURE
// without fetching any bytes.
func attachmentsFromBodyStructure(bs imap.BodyStructure) []Attachment {
	if bs == nil {
		return nil
	}
	var out []Attachment
	bs.Walk(func(_ []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		filename := sp.Filename()
		disp := sp.Disposition()
		isAttachment := filename != "" || (disp != nil && strings.EqualFold(disp.Value, "attachment"))
		if !isAttachment {
			return true
		}
		out = append(out, Attachment{
			Filename:    filename,
			ContentType: sp.MediaType(),
			Size:        int64(sp.Size),
		})
		return true
	})
	return out
}

// parseMessage parses a raw RFC 5322 message into the normalized form. It is
// deliberately lenient: a truncated (partial) fetch still yields the fields
// that could be decoded. includeHTML controls whether an HTML alternative is
// returned alongside the plain-text body.
func parseMessage(raw []byte, includeHTML bool) (*Message, error) {
	msg := &Message{}
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		return msg, err
	}
	defer mr.Close()

	if subj, serr := mr.Header.Subject(); serr == nil {
		msg.Subject = subj
	}
	if date, derr := mr.Header.Date(); derr == nil {
		msg.Date = date
	}
	if id, ierr := mr.Header.MessageID(); ierr == nil {
		msg.MessageID = id
	}
	msg.From = headerAddresses(&mr.Header, "From")
	msg.To = headerAddresses(&mr.Header, "To")
	msg.Cc = headerAddresses(&mr.Header, "Cc")

	var text, html strings.Builder
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil && part == nil {
			break
		}
		inline, ok := part.Header.(*gomail.InlineHeader)
		if !ok {
			if part.Body != nil {
				_, _ = io.Copy(io.Discard, part.Body)
			}
			continue
		}
		ct, _, _ := inline.ContentType()
		data, _ := io.ReadAll(io.LimitReader(part.Body, MaxBodyBytes+1))
		if strings.HasPrefix(strings.ToLower(ct), "text/html") {
			if includeHTML && html.Len() < MaxBodyBytes {
				html.Write(data)
			}
			continue
		}
		if text.Len() < MaxBodyBytes {
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.Write(data)
		}
	}
	msg.BodyText = truncateUTF8(text.String(), MaxBodyBytes)
	if includeHTML {
		msg.BodyHTML = truncateUTF8(html.String(), MaxBodyBytes)
	}
	return msg, nil
}

// headerAddresses reads an address header, tolerating a malformed value.
func headerAddresses(h *gomail.Header, key string) []Address {
	list, err := h.AddressList(key)
	if err != nil {
		return nil
	}
	out := make([]Address, 0, len(list))
	for _, a := range list {
		out = append(out, Address{Name: strings.TrimSpace(a.Name), Email: a.Address})
	}
	return out
}

// snippet returns the first ~SnippetChars of body text, single-lined.
func snippet(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	return truncateUTF8(text, SnippetChars)
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
