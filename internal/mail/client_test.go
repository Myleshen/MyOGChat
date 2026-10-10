package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// sampleRawMessage is a simple RFC 5322 message served by the fake IMAP server.
const sampleRawMessage = "From: Alice Example <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Cc: carol@example.com\r\n" +
	"Subject: Hello world\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
	"Message-ID: <abc123@example.com>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Hello body text.\r\n"

// fakeIMAPServer is a minimal, read-only IMAP server for the tests. It records
// every command line it receives so tests can assert on the wire behaviour
// (EXAMINE vs SELECT, BODY.PEEK vs BODY).
type fakeIMAPServer struct {
	ln  net.Listener
	raw []byte

	mu       sync.Mutex
	commands []string
}

func startFakeIMAP(t *testing.T) *fakeIMAPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeIMAPServer{ln: ln, raw: []byte(sampleRawMessage)}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeIMAPServer) addr() string { return s.ln.Addr().String() }

func (s *fakeIMAPServer) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, line)
}

func (s *fakeIMAPServer) recorded() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.commands, "\n")
}

func (s *fakeIMAPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeIMAPServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	fmt.Fprint(w, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] fake ready\r\n")
	w.Flush()

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.record(line)
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag := fields[0]
		switch strings.ToUpper(fields[1]) {
		case "CAPABILITY":
			fmt.Fprintf(w, "* CAPABILITY IMAP4rev1 AUTH=PLAIN\r\n%s OK\r\n", tag)
		case "AUTHENTICATE":
			if len(fields) < 4 {
				// No SASL-IR: ask for the SASL response on the next line.
				fmt.Fprint(w, "+ \r\n")
				w.Flush()
				if _, err := r.ReadString('\n'); err != nil {
					return
				}
			}
			fmt.Fprintf(w, "%s OK\r\n", tag)
		case "LOGIN":
			fmt.Fprintf(w, "%s OK\r\n", tag)
		case "LIST", "LSUB":
			fmt.Fprint(w, "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n")
			fmt.Fprint(w, "* LIST (\\HasNoChildren \\Sent) \"/\" \"Sent\"\r\n")
			fmt.Fprintf(w, "%s OK\r\n", tag)
		case "EXAMINE", "SELECT":
			fmt.Fprint(w, "* FLAGS (\\Answered \\Seen)\r\n")
			fmt.Fprint(w, "* 1 EXISTS\r\n")
			fmt.Fprint(w, "* OK [UIDVALIDITY 1] UIDs valid\r\n")
			fmt.Fprintf(w, "%s OK [READ-ONLY] done\r\n", tag)
		case "SEARCH":
			fmt.Fprintf(w, "* SEARCH 42\r\n%s OK\r\n", tag)
		case "UID":
			switch strings.ToUpper(fields[2]) {
			case "SEARCH":
				fmt.Fprintf(w, "* SEARCH 42\r\n%s OK\r\n", tag)
			case "FETCH":
				s.respondFetch(w, line, tag)
			default:
				fmt.Fprintf(w, "%s BAD unsupported UID subcommand\r\n", tag)
			}
		case "FETCH":
			s.respondFetch(w, line, tag)
		case "NOOP", "CLOSE":
			fmt.Fprintf(w, "%s OK\r\n", tag)
		case "LOGOUT":
			fmt.Fprintf(w, "* BYE\r\n%s OK\r\n", tag)
			w.Flush()
			return
		default:
			fmt.Fprintf(w, "%s BAD unsupported command\r\n", tag)
		}
		w.Flush()
	}
}

func (s *fakeIMAPServer) respondFetch(w *bufio.Writer, line, tag string) {
	up := strings.ToUpper(line)
	if strings.Contains(up, "BODY") {
		// Whole-message fetch: return the raw message as a literal.
		fmt.Fprintf(w, "* 1 FETCH (UID 42 RFC822.SIZE %d BODY[] {%d}\r\n", len(s.raw), len(s.raw))
		w.Write(s.raw)
		fmt.Fprintf(w, ")\r\n%s OK\r\n", tag)
		return
	}
	// Metadata-only fetch (search summaries).
	envelope := `("Mon, 02 Jan 2006 15:04:05 -0700" "Hello world" ` +
		`(("Alice Example" NIL "alice" "example.com")) ` +
		`(("Alice Example" NIL "alice" "example.com")) ` +
		`(("Alice Example" NIL "alice" "example.com")) ` +
		`(("Bob" NIL "bob" "example.com")) NIL NIL NIL "<abc123@example.com>")`
	fmt.Fprintf(w, "* 1 FETCH (UID 42 FLAGS (\\Seen) RFC822.SIZE %d ENVELOPE %s)\r\n%s OK\r\n",
		len(s.raw), envelope, tag)
}

func dialFake(t *testing.T, s *fakeIMAPServer) *Client {
	t.Helper()
	conn, err := net.Dial("tcp", s.addr())
	if err != nil {
		t.Fatalf("dial fake server: %v", err)
	}
	acct := Account{Provider: "custom", Email: "bob@example.com", Host: "127.0.0.1", Password: "app-password"}
	return newClient(conn, acct)
}

func TestClientFoldersSearchRead(t *testing.T) {
	srv := startFakeIMAP(t)
	c := dialFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.login(); err != nil {
		t.Fatalf("login: %v", err)
	}

	folders, err := c.Folders(ctx)
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 2 || folders[0].Name != "INBOX" || folders[1].SpecialUse != "sent" {
		t.Fatalf("folders = %+v", folders)
	}

	summaries, err := c.Search(ctx, SearchQuery{Folder: "INBOX", Query: "hello", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(summaries) != 1 || summaries[0].UID != 42 || summaries[0].Subject != "Hello world" {
		t.Fatalf("summaries = %+v", summaries)
	}

	msg, err := c.Read(ctx, 42, "INBOX", false)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if msg.UID != 42 || msg.Subject != "Hello world" {
		t.Fatalf("message = %+v", msg)
	}
	if !strings.Contains(msg.BodyText, "Hello body text.") {
		t.Fatalf("body = %q", msg.BodyText)
	}
	if len(msg.From) != 1 || msg.From[0].Email != "alice@example.com" {
		t.Fatalf("from = %+v", msg.From)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wire := srv.recorded()
	upper := strings.ToUpper(wire)
	if !strings.Contains(upper, "EXAMINE INBOX") {
		t.Errorf("expected EXAMINE INBOX on the wire, got:\n%s", wire)
	}
	if strings.Contains(upper, "SELECT INBOX") {
		t.Errorf("wrapper must never SELECT, got:\n%s", wire)
	}
	if !strings.Contains(upper, "BODY.PEEK[") {
		t.Errorf("expected BODY.PEEK[] on the wire, got:\n%s", wire)
	}
	if strings.Contains(upper, "BODY[") {
		t.Errorf("wrapper must not use BODY[] (non-peek), got:\n%s", wire)
	}
	// No mutation commands may ever appear.
	for _, bad := range []string{"STORE", "COPY", "APPEND", "EXPUNGE", "MOVE"} {
		if strings.Contains(upper, bad) {
			t.Errorf("wire contains forbidden command %q:\n%s", bad, wire)
		}
	}
}

func TestTestConnectionRejectsBadConfig(t *testing.T) {
	ctx := context.Background()
	if err := TestConnection(ctx, Account{Email: "", Host: "h", Port: 993, Password: "p"}); err == nil {
		t.Error("expected error for missing email")
	}
	if err := TestConnection(ctx, Account{Email: "a@b.c", Host: "h", Port: 993, TLS: "starttls", Password: "p"}); err == nil {
		t.Error("expected error for unsupported TLS mode")
	}
	if err := TestConnection(ctx, Account{Email: "a@b.c", Host: "h", Port: 993, Password: ""}); err == nil {
		t.Error("expected error for missing password")
	}
}
