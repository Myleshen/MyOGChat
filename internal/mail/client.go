package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-sasl"
)

const (
	dialTimeout = 15 * time.Second
	// opTimeout bounds a single IMAP operation (list/search/fetch) so a hung
	// server can never outlive the request or the Cloudflare tunnel (5.9).
	opTimeout = 20 * time.Second

	// DefaultSearchLimit is used when a search omits a limit.
	DefaultSearchLimit = 20
	// MaxSearchLimit caps how many summaries one search may return.
	MaxSearchLimit = 50
)

// Account holds the decrypted connection details for one mailbox. Password is
// never logged or returned to callers.
type Account struct {
	Provider string
	Email    string
	Host     string
	Port     int
	TLS      string
	Password string
}

// Validate checks the account fields without connecting.
func (a Account) Validate() error {
	tmp := a
	return tmp.normalize()
}

// normalize validates the account and fills in defaults.
func (a *Account) normalize() error {
	a.Provider = strings.TrimSpace(a.Provider)
	a.Email = strings.TrimSpace(a.Email)
	a.Host = strings.TrimSpace(a.Host)
	a.TLS = strings.TrimSpace(a.TLS)
	if a.TLS == "" {
		a.TLS = TLSImplicit
	}
	if a.TLS != TLSImplicit {
		return fmt.Errorf("unsupported IMAP TLS mode %q (only implicit TLS is supported)", a.TLS)
	}
	if a.Email == "" {
		return errors.New("email address is required")
	}
	if a.Host == "" {
		return errors.New("IMAP host is required")
	}
	if a.Port <= 0 || a.Port > 65535 {
		return errors.New("IMAP port is invalid")
	}
	if a.Password == "" {
		return errors.New("app-specific password is required")
	}
	return nil
}

// Client is a read-only IMAP session. It exposes only read methods; there is no
// write path anywhere in the type.
type Client struct {
	conn net.Conn
	c    *imapclient.Client
	acct Account
}

// Dial connects, authenticates, and returns a read-only session. The caller
// must Close it. The connection uses implicit TLS with certificate
// verification (never DialInsecure / InsecureSkipVerify).
func Dial(ctx context.Context, acct Account) (*Client, error) {
	if err := acct.normalize(); err != nil {
		return nil, err
	}
	conn, err := dialTLS(ctx, acct)
	if err != nil {
		return nil, err
	}
	c := newClient(conn, acct)
	if err := c.login(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// TestConnection dials, authenticates, and disconnects — used by the Settings
// "Test connection" action without persisting anything.
func TestConnection(ctx context.Context, acct Account) error {
	c, err := Dial(ctx, acct)
	if err != nil {
		return err
	}
	return c.Close()
}

func dialTLS(ctx context.Context, acct Account) (net.Conn, error) {
	addr := net.JoinHostPort(acct.Host, strconv.Itoa(acct.Port))
	dialer := &net.Dialer{Timeout: dialTimeout}
	tlsDialer := &tls.Dialer{
		NetDialer: dialer,
		Config: &tls.Config{
			ServerName: acct.Host,
			MinVersion: tls.VersionTLS12,
		},
	}
	conn, err := tlsDialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return conn, nil
}

// newClient wraps conn in the read-only command filter and builds the IMAP
// client. It performs no I/O.
func newClient(conn net.Conn, acct Account) *Client {
	filtered := newCommandFilter(conn)
	c := imapclient.New(filtered, &imapclient.Options{
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
	})
	return &Client{conn: filtered, c: c, acct: acct}
}

// login authenticates with SASL PLAIN (app-specific passwords), falling back to
// the LOGIN command for servers that do not offer AUTHENTICATE PLAIN.
func (c *Client) login() error {
	if err := c.c.WaitGreeting(); err != nil {
		return fmt.Errorf("imap greeting: %w", err)
	}
	auth := sasl.NewPlainClient("", c.acct.Email, c.acct.Password)
	if err := c.c.Authenticate(auth); err == nil {
		return nil
	} else if loginErr := c.c.Login(c.acct.Email, c.acct.Password).Wait(); loginErr != nil {
		return fmt.Errorf("imap authentication failed (check the app-specific password): %v", loginErr)
	}
	return nil
}

// Folders lists the mailboxes of the account.
func (c *Client) Folders(ctx context.Context) ([]Folder, error) {
	c.setDeadline(ctx)
	cmd := c.c.List("", "*", nil)
	mailboxes, err := cmd.Collect()
	if err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	out := make([]Folder, 0, len(mailboxes))
	for _, m := range mailboxes {
		out = append(out, folderFromListData(m))
	}
	return out, nil
}

// Search returns message summaries for the query, newest first. The mailbox is
// opened read-only (EXAMINE) and bodies are not fetched.
func (c *Client) Search(ctx context.Context, q SearchQuery) ([]MessageSummary, error) {
	c.setDeadline(ctx)
	folder := normalizeFolder(q.Folder)
	if err := c.openReadOnly(folder); err != nil {
		return nil, err
	}
	data, err := c.c.UIDSearch(searchCriteria(q), nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", folder, err)
	}
	uids := uidsFromNumSet(data.All)
	if len(uids) == 0 {
		return nil, nil
	}
	// Newest first (UIDs are chronological), then cap.
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if limit := normalizeLimit(q.Limit); len(uids) > limit {
		uids = uids[:limit]
	}
	// Fetch in ascending order for predictable responses.
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

	fetchOpts := &imap.FetchOptions{
		UID:        true,
		Envelope:   true,
		Flags:      true,
		RFC822Size: true,
	}
	// Opt-in body preview (5.9): a small partial BODY.PEEK[] fetch so summaries
	// can carry a snippet without ever setting \Seen. email_search keeps its
	// token profile by default (PreviewBytes == 0).
	if q.PreviewBytes > 0 {
		fetchOpts.BodySection = []*imap.FetchItemBodySection{{
			Peek:    true,
			Partial: &imap.SectionPartial{Offset: 0, Size: int64(q.PreviewBytes)},
		}}
	}
	fetch := c.c.Fetch(imap.UIDSetNum(uids...), fetchOpts)
	bufs, err := fetch.Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch summaries: %w", err)
	}
	out := make([]MessageSummary, 0, len(bufs))
	for _, b := range bufs {
		s := MessageSummary{
			UID:    uint32(b.UID),
			Folder: folder,
			Flags:  flagsToStrings(b.Flags),
			Size:   b.RFC822Size,
		}
		if b.Envelope != nil {
			s.Subject = b.Envelope.Subject
			s.Date = b.Envelope.Date
			s.From = addresses(b.Envelope.From)
			s.To = addresses(b.Envelope.To)
		}
		if q.PreviewBytes > 0 {
			if raw := firstBodySection(b); len(raw) > 0 {
				if parsed, _ := parseMessage(raw, false); parsed != nil {
					s.Snippet = snippet(parsed.BodyText)
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Read returns one message by UID: envelope, flags, attachment metadata, and a
// truncated text body. Bodies are fetched with BODY.PEEK[] (never sets \Seen)
// and capped at maxFetchBytes so a large attachment is never downloaded.
func (c *Client) Read(ctx context.Context, uid uint32, folder string, includeHTML bool) (*Message, error) {
	c.setDeadline(ctx)
	folder = normalizeFolder(folder)
	if uid == 0 {
		return nil, errors.New("uid is required")
	}
	if err := c.openReadOnly(folder); err != nil {
		return nil, err
	}
	bodySection := &imap.FetchItemBodySection{
		Peek:    true,
		Partial: &imap.SectionPartial{Offset: 0, Size: maxFetchBytes},
	}
	fetch := c.c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		Flags:         true,
		RFC822Size:    true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{bodySection},
	})
	bufs, err := fetch.Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch message: %w", err)
	}
	if len(bufs) == 0 {
		return nil, fmt.Errorf("message %d not found in %q", uid, folder)
	}
	buf := bufs[0]
	msg := &Message{
		UID:    uint32(buf.UID),
		Folder: folder,
		Flags:  flagsToStrings(buf.Flags),
		Size:   buf.RFC822Size,
	}
	if buf.Envelope != nil {
		msg.Subject = buf.Envelope.Subject
		msg.Date = buf.Envelope.Date
		msg.MessageID = buf.Envelope.MessageID
		msg.From = addresses(buf.Envelope.From)
		msg.To = addresses(buf.Envelope.To)
		msg.Cc = addresses(buf.Envelope.Cc)
	}
	msg.Attachments = attachmentsFromBodyStructure(buf.BodyStructure)

	if raw := firstBodySection(buf); len(raw) > 0 {
		if parsed, _ := parseMessage(raw, includeHTML); parsed != nil {
			// The envelope is authoritative for headers; the parsed MIME body
			// fills in the text and any header the server omitted.
			if msg.Subject == "" {
				msg.Subject = parsed.Subject
			}
			if msg.Date.IsZero() {
				msg.Date = parsed.Date
			}
			if msg.MessageID == "" {
				msg.MessageID = parsed.MessageID
			}
			if len(msg.From) == 0 {
				msg.From = parsed.From
			}
			if len(msg.To) == 0 {
				msg.To = parsed.To
			}
			if len(msg.Cc) == 0 {
				msg.Cc = parsed.Cc
			}
			msg.BodyText = parsed.BodyText
			if includeHTML {
				msg.BodyHTML = parsed.BodyHTML
			}
		}
	}
	return msg, nil
}

// Close logs out and closes the connection (best effort).
func (c *Client) Close() error {
	if c == nil || c.c == nil {
		return nil
	}
	_ = c.c.Logout().Wait()
	_ = c.c.Close()
	return nil
}

// setDeadline bounds one IMAP operation: the shorter of the 20 s per-op budget
// and the caller's context deadline, applied to the underlying connection (the
// command filter forwards SetDeadline). This closes the gap where a hung fetch
// could outlive the request/tunnel because the ctx was previously unused.
func (c *Client) setDeadline(ctx context.Context) {
	if c == nil || c.conn == nil {
		return
	}
	d := time.Now().Add(opTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(d) {
		d = dl
	}
	_ = c.conn.SetDeadline(d)
}

// openReadOnly selects a mailbox with EXAMINE (read-only), never SELECT, so
// opening it has no side effects.
func (c *Client) openReadOnly(folder string) error {
	if _, err := c.c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return fmt.Errorf("open mailbox %q: %w", folder, err)
	}
	return nil
}

// firstBodySection returns the raw bytes of the whole-message BODY[] section.
func firstBodySection(buf *imapclient.FetchMessageBuffer) []byte {
	for _, bs := range buf.BodySection {
		if len(bs.Section.Part) == 0 && bs.Section.Specifier == imap.PartSpecifierNone {
			return bs.Bytes
		}
	}
	return nil
}

// uidsFromNumSet extracts UIDs from a SEARCH result.
func uidsFromNumSet(ns imap.NumSet) []imap.UID {
	switch set := ns.(type) {
	case imap.UIDSet:
		uids, _ := set.Nums()
		return uids
	case imap.SeqSet:
		nums, _ := set.Nums()
		out := make([]imap.UID, len(nums))
		for i, n := range nums {
			out[i] = imap.UID(n)
		}
		return out
	default:
		return nil
	}
}
