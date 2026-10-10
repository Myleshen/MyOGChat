package mail

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// readOnlyCommands is the transport-level allowlist. Any IMAP client command
// whose name is not in this set is rejected before a single byte reaches the
// socket. The list is deliberately the read-only subset: no STORE, COPY, MOVE,
// APPEND, EXPUNGE, CREATE, DELETE, RENAME, SUBSCRIBE, UNSELECT, STARTTLS, etc.
var readOnlyCommands = map[string]bool{
	"CAPABILITY":   true,
	"NOOP":         true,
	"LOGIN":        true,
	"AUTHENTICATE": true,
	"LIST":         true,
	"LSUB":         true,
	"STATUS":       true,
	"EXAMINE":      true,
	"SELECT":       true,
	"FETCH":        true,
	"SEARCH":       true,
	"UID":          true,
	"IDLE":         true,
	"CLOSE":        true,
	"LOGOUT":       true,
}

// readOnlyUIDCommands restricts the sub-command of "UID". Allowing the bare
// "UID" atom would let "UID STORE"/"UID COPY"/"UID MOVE" through, so the
// allowlist is narrowed to the two read sub-commands the wrapper actually uses.
var readOnlyUIDCommands = map[string]bool{
	"FETCH":  true,
	"SEARCH": true,
}

// literalSuffix matches a trailing IMAP literal marker, e.g. {123} or {123+}.
var literalSuffix = regexp.MustCompile(`\{(\d+)\+?\}$`)

// commandFilter wraps the IMAP net.Conn and validates every client command
// against readOnlyCommands. Reads pass straight through; writes are buffered
// until a complete command line is seen, validated, and only then forwarded.
//
// It understands two IMAP subtleties so that legitimate read commands are not
// mis-flagged:
//   - SASL continuation: after "AUTHENTICATE <mech>" (no initial response) the
//     next line is an opaque SASL blob, not a command.
//   - literals: after a command line ending in {N}, the following N bytes are
//     literal data and are not parsed as commands.
type commandFilter struct {
	conn net.Conn

	mu          sync.Mutex
	pending     []byte // bytes received but not yet a complete, validated command
	skipLiteral int64  // remaining literal bytes to pass through untouched
	awaitSASL   bool   // next line is a SASL continuation response
	err         error  // sticky rejection error
}

func newCommandFilter(conn net.Conn) *commandFilter {
	return &commandFilter{conn: conn}
}

func (f *commandFilter) Read(b []byte) (int, error) { return f.conn.Read(b) }

func (f *commandFilter) Close() error { return f.conn.Close() }

func (f *commandFilter) LocalAddr() net.Addr { return f.conn.LocalAddr() }

func (f *commandFilter) RemoteAddr() net.Addr { return f.conn.RemoteAddr() }

func (f *commandFilter) SetDeadline(t time.Time) error { return f.conn.SetDeadline(t) }

func (f *commandFilter) SetReadDeadline(t time.Time) error { return f.conn.SetReadDeadline(t) }

func (f *commandFilter) SetWriteDeadline(t time.Time) error { return f.conn.SetWriteDeadline(t) }

// Write validates b (which may contain zero or more complete command lines)
// and forwards only validated data. A rejected command returns an error, which
// the IMAP client surfaces and turns into a failed operation.
func (f *commandFilter) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.pending = append(f.pending, b...)
	for {
		// Literal data is passed through verbatim.
		if f.skipLiteral > 0 {
			if len(f.pending) == 0 {
				break // wait for the rest of the literal
			}
			n := int64(len(f.pending))
			if n > f.skipLiteral {
				n = f.skipLiteral
			}
			if _, err := f.conn.Write(f.pending[:n]); err != nil {
				f.err = err
				return 0, err
			}
			f.pending = f.pending[n:]
			f.skipLiteral -= n
			continue
		}

		idx := bytes.Index(f.pending, []byte("\r\n"))
		if idx < 0 {
			break // wait for the rest of the line
		}
		line := f.pending[:idx]
		full := f.pending[:idx+2]
		rest := f.pending[idx+2:]

		if f.awaitSASL {
			// Opaque SASL response; not a command.
			f.awaitSASL = false
		} else if err := validateCommand(line); err != nil {
			f.err = err
			return 0, err
		} else if err := f.trackContinuations(line); err != nil {
			f.err = err
			return 0, err
		}

		if _, err := f.conn.Write(full); err != nil {
			f.err = err
			return 0, err
		}
		f.pending = rest
	}
	return len(b), nil
}

// trackContinuations records whether the just-validated line expects a SASL
// continuation or a literal, so the following bytes are not parsed as commands.
func (f *commandFilter) trackContinuations(line []byte) error {
	s := strings.TrimSpace(string(line))
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return nil
	}
	cmd := strings.ToUpper(fields[1])
	if cmd == "AUTHENTICATE" && len(fields) < 4 {
		// No SASL-IR initial response: the next line is the SASL blob.
		f.awaitSASL = true
	}
	if m := literalSuffix.FindStringSubmatch(s); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("imap guardrail: invalid literal size in %q", s)
		}
		f.skipLiteral = n
	}
	return nil
}

// validateCommand parses one client command line and checks it against the
// allowlist. Empty lines and continuation markers are ignored.
func validateCommand(line []byte) error {
	s := strings.TrimSpace(string(line))
	if s == "" || s == "+" {
		return nil
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return fmt.Errorf("imap guardrail: malformed command %q", s)
	}
	cmd := strings.ToUpper(fields[1])
	if !readOnlyCommands[cmd] {
		return fmt.Errorf("imap guardrail: command %q is not on the read-only allowlist", cmd)
	}
	if cmd == "UID" {
		sub := ""
		if len(fields) >= 3 {
			sub = strings.ToUpper(fields[2])
		}
		if !readOnlyUIDCommands[sub] {
			return fmt.Errorf("imap guardrail: UID %s is not on the read-only allowlist", sub)
		}
	}
	return nil
}
