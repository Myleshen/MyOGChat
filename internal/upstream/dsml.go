package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DeepSeek models sometimes emit their native tool-call markup as ordinary
// delta.content instead of structured tool_calls. The separator is U+FF5C
// (FULLWIDTH VERTICAL LINE), not an ASCII pipe:
//
//	<｜DSML｜calls>
//	<｜DSML｜invoke name="run_python">
//	<｜DSML｜parameter name="code" string="true">
//	print(1)
//	</｜DSML｜parameter>
//	</｜DSML｜invoke>
//	</｜DSML｜calls>
const (
	dsmlMark        = "<\uFF5CDSML\uFF5C"
	dsmlCallsClose  = "</\uFF5CDSML\uFF5Ccalls>"
	dsmlInvokeOpen  = "<\uFF5CDSML\uFF5Cinvoke"
	dsmlInvokeClose = "</\uFF5CDSML\uFF5Cinvoke>"
	dsmlParamOpen   = "<\uFF5CDSML\uFF5Cparameter"
	dsmlParamClose  = "</\uFF5CDSML\uFF5Cparameter>"
)

// DSMLFilter removes DeepSeek markup from a streamed reply. It holds back any
// trailing bytes that could be the start of a marker, so neither a partial nor
// a complete marker ever reaches the client. The raw markup is kept in Block
// for parseDSMLCalls.
type DSMLFilter struct {
	buf   strings.Builder // text not yet classified
	block strings.Builder // raw markup, tags included
	in    bool            // inside a calls block
}

// Write consumes one delta and returns the text that is safe to emit.
func (f *DSMLFilter) Write(chunk string) string {
	if chunk == "" {
		return ""
	}
	f.buf.WriteString(chunk)
	s := f.buf.String()
	var safe strings.Builder
	for {
		if f.in {
			// Keep the block pending until its closing tag arrives, so a tag
			// split across two deltas is still recognised.
			if i := strings.Index(s, dsmlCallsClose); i >= 0 {
				end := i + len(dsmlCallsClose)
				f.block.WriteString(s[:end])
				s = s[end:]
				f.in = false
				continue
			}
			break
		}
		i := strings.Index(s, dsmlMark)
		if i < 0 {
			// Emit everything except a trailing partial marker.
			hold := partialSuffix(s, dsmlMark)
			safe.WriteString(s[:len(s)-hold])
			s = s[len(s)-hold:]
			break
		}
		safe.WriteString(s[:i])
		s = s[i:]
		f.in = true
	}
	f.buf.Reset()
	f.buf.WriteString(s)
	return safe.String()
}

// Close finishes the stream. An unclosed block is kept in Block for parsing;
// a trailing partial marker is dropped (it is never emitted as text).
func (f *DSMLFilter) Close() {
	if f.in {
		f.block.WriteString(f.buf.String())
	}
	f.buf.Reset()
}

// Block returns the raw markup collected so far.
func (f *DSMLFilter) Block() string { return f.block.String() }

// SanitizeDSML removes every DSML block from s. It is used at the persistence
// and history-replay boundaries, where markup written by older turns may still
// be stored.
func SanitizeDSML(s string) string {
	if !strings.Contains(s, dsmlMark) {
		return s
	}
	var f DSMLFilter
	safe := f.Write(s)
	f.Close()
	return safe
}

// partialSuffix returns the length of the longest suffix of s that is a proper
// prefix of mark. Bytes are compared, so a rune split across chunks cannot
// produce a false match.
func partialSuffix(s, mark string) int {
	max := len(mark) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, mark[:n]) {
			return n
		}
	}
	return 0
}

// parseDSMLCalls turns raw DSML markup into the tool calls the dialect adapters
// already understand. Unclosed tags at EOF are accepted; a block that never
// closed still yields the invokes it contains.
func parseDSMLCalls(raw string) []ToolCall {
	var calls []ToolCall
	rest := raw
	for {
		i := strings.Index(rest, dsmlInvokeOpen)
		if i < 0 {
			return calls
		}
		gt := strings.IndexByte(rest[i:], '>')
		if gt < 0 {
			return calls
		}
		header := rest[i+len(dsmlInvokeOpen) : i+gt]
		rest = rest[i+gt+1:]

		body := rest
		if end := strings.Index(rest, dsmlInvokeClose); end >= 0 {
			body = rest[:end]
			rest = rest[end+len(dsmlInvokeClose):]
		} else {
			rest = ""
		}
		name := dsmlAttr(header, "name")
		if name == "" {
			continue
		}
		calls = append(calls, ToolCall{
			ID:        fmt.Sprintf("call_dsml_%d", len(calls)+1),
			Name:      name,
			Arguments: dsmlArgs(body),
		})
	}
}

// dsmlArgs renders a parameter body as a JSON argument object. string="false"
// values are JSON literals and pass through as-is; everything else is a raw
// string.
func dsmlArgs(body string) string {
	args := map[string]any{}
	rest := body
	for {
		i := strings.Index(rest, dsmlParamOpen)
		if i < 0 {
			break
		}
		gt := strings.IndexByte(rest[i:], '>')
		if gt < 0 {
			break
		}
		header := rest[i+len(dsmlParamOpen) : i+gt]
		rest = rest[i+gt+1:]

		raw := rest
		if end := strings.Index(rest, dsmlParamClose); end >= 0 {
			raw = rest[:end]
			rest = rest[end+len(dsmlParamClose):]
		} else {
			rest = ""
		}
		name := dsmlAttr(header, "name")
		if name == "" {
			continue
		}
		if dsmlAttr(header, "string") == "false" {
			var v any
			if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err == nil {
				args[name] = v
			} else {
				args[name] = raw
			}
			continue
		}
		args[name] = trimLeadingNewline(raw)
	}
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// dsmlAttr reads key="value" from an opening tag's attribute list.
func dsmlAttr(header, key string) string {
	i := strings.Index(header, key+"=\"")
	if i < 0 {
		return ""
	}
	rest := header[i+len(key)+2:]
	if end := strings.IndexByte(rest, '"'); end >= 0 {
		return rest[:end]
	}
	return rest
}

// trimLeadingNewline drops the formatting newline that follows an opening
// parameter tag.
func trimLeadingNewline(s string) string {
	s = strings.TrimPrefix(s, "\r\n")
	return strings.TrimPrefix(s, "\n")
}
