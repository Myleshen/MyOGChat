package mail

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConn is a net.Conn that records everything written through it.
type fakeConn struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *fakeConn) Read([]byte) (int, error)         { return 0, nil }
func (c *fakeConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakeConn) Close() error                     { return nil }

func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *fakeConn) written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type dummyAddr struct{}

func (dummyAddr) Network() string { return "fake" }
func (dummyAddr) String() string  { return "fake" }

func TestCommandFilterAllowsReadCommands(t *testing.T) {
	allowed := []string{
		"T1 CAPABILITY",
		"T2 NOOP",
		`T3 LOGIN "user@example.com" "app password"`,
		"T4 LIST \"\" *",
		"T5 LSUB \"\" *",
		"T6 STATUS INBOX (MESSAGES)",
		"T7 EXAMINE INBOX",
		"T8 SELECT INBOX",
		"T9 FETCH 1 (UID BODY.PEEK[])",
		"T10 SEARCH ALL",
		"T11 UID FETCH 1:10 (UID ENVELOPE)",
		"T12 UID SEARCH ALL",
		"T13 IDLE",
		"T14 CLOSE",
		"T15 LOGOUT",
	}
	for _, line := range allowed {
		conn := &fakeConn{}
		f := newCommandFilter(conn)
		if _, err := f.Write([]byte(line + "\r\n")); err != nil {
			t.Errorf("Write(%q) = %v, want allowed", line, err)
			continue
		}
		if got := conn.written(); got != line+"\r\n" {
			t.Errorf("forwarded = %q, want %q", got, line+"\r\n")
		}
	}
}

func TestCommandFilterRejectsWriteCommands(t *testing.T) {
	// These are the mutations that must never reach the server, even if a
	// higher layer tried to issue them.
	rejected := []string{
		"T1 STORE 1 +FLAGS (\\Seen)",
		"T2 UID STORE 1 +FLAGS (\\Seen)",
		"T3 COPY 1 Sent",
		"T4 UID COPY 1 Sent",
		"T5 APPEND Sent (\\Seen) {10}",
		"T6 EXPUNGE",
		"T7 UID EXPUNGE 1:5",
		"T8 MOVE 1 Trash",
		"T9 UID MOVE 1 Trash",
		"T10 CREATE Foo",
		"T11 DELETE Foo",
		"T12 RENAME Foo Bar",
		"T13 SUBSCRIBE Foo",
		"T14 UNSUBSCRIBE Foo",
		"T15 UNSELECT",
		"T16 STARTTLS",
	}

	for _, line := range rejected {
		conn := &fakeConn{}
		f := newCommandFilter(conn)
		_, err := f.Write([]byte(line + "\r\n"))
		if err == nil {
			t.Errorf("Write(%q) = nil, want rejection", line)
			continue
		}
		if !strings.Contains(err.Error(), "guardrail") {
			t.Errorf("Write(%q) error = %v, want guardrail error", line, err)
		}
		if got := conn.written(); got != "" {
			t.Errorf("Write(%q) forwarded %q, want nothing", line, got)
		}
	}
}

func TestCommandFilterSASLContinuation(t *testing.T) {
	conn := &fakeConn{}
	f := newCommandFilter(conn)
	// No SASL-IR: AUTHENTICATE PLAIN is followed by an opaque base64 line.
	if _, err := f.Write([]byte("T1 AUTHENTICATE PLAIN\r\n")); err != nil {
		t.Fatalf("AUTHENTICATE rejected: %v", err)
	}
	// The SASL blob contains characters that would otherwise look like a
	// malformed command; it must be passed through untouched.
	blob := "AGJvYkBsb2NhbGhvc3QAcGFzcw=="
	if _, err := f.Write([]byte(blob + "\r\n")); err != nil {
		t.Fatalf("SASL continuation rejected: %v", err)
	}
	if got := conn.written(); got != "T1 AUTHENTICATE PLAIN\r\n"+blob+"\r\n" {
		t.Fatalf("forwarded = %q", got)
	}
}

func TestCommandFilterSASLIR(t *testing.T) {
	// With SASL-IR the initial response rides on the AUTHENTICATE line and no
	// continuation follows.
	conn := &fakeConn{}
	f := newCommandFilter(conn)
	line := "T1 AUTHENTICATE PLAIN AGJvYkBsb2NhbGhvc3QAcGFzcw=="
	if _, err := f.Write([]byte(line + "\r\n")); err != nil {
		t.Fatalf("AUTHENTICATE (SASL-IR) rejected: %v", err)
	}
	if got := conn.written(); got != line+"\r\n" {
		t.Fatalf("forwarded = %q", got)
	}
}

func TestCommandFilterSkipsLiteralData(t *testing.T) {
	conn := &fakeConn{}
	f := newCommandFilter(conn)
	// LOGIN with a synchronizing literal password (unusual but legal).
	if _, err := f.Write([]byte("T1 LOGIN \"user@example.com\" {5}\r\n")); err != nil {
		t.Fatalf("LOGIN literal header rejected: %v", err)
	}
	// The next 5 bytes are literal data — even if they look like a command.
	if _, err := f.Write([]byte("APPEN")); err != nil {
		t.Fatalf("literal bytes rejected: %v", err)
	}
	// Back to normal: a disallowed command after the literal is rejected.
	if _, err := f.Write([]byte("\r\nT2 EXPUNGE\r\n")); err == nil {
		t.Fatal("EXPUNGE after literal was allowed, want rejection")
	}
	if got := conn.written(); !strings.Contains(got, "APPEN") {
		t.Fatalf("literal not forwarded: %q", got)
	}
}

func TestCommandFilterRejectsAcrossWrites(t *testing.T) {
	// A command split across two Write calls must still be validated as a whole.
	conn := &fakeConn{}
	f := newCommandFilter(conn)
	if _, err := f.Write([]byte("T1 APP")); err != nil {
		t.Fatalf("partial write rejected early: %v", err)
	}
	if _, err := f.Write([]byte("END Sent {3}\r\n")); err == nil {
		t.Fatal("APPEND split across writes was allowed, want rejection")
	}
	if got := conn.written(); got != "" {
		t.Fatalf("partial disallowed command leaked: %q", got)
	}
}
