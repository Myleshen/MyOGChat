package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Fixtures taken verbatim from the production DB. The separator is U+FF5C
// (fullwidth vertical line), not an ASCII pipe. Note the first fixture ends with
// an unclosed calls tag, exactly as the model emitted it.
const (
	dsmlFixtureSingle = "<\uFF5CDSML\uFF5Ccalls>\n" +
		"<\uFF5CDSML\uFF5Cinvoke name=\"run_python\">\n" +
		"<\uFF5CDSML\uFF5Cparameter name=\"code\" string=\"true\">\n" +
		"import json\nprint(1)\n" +
		"</\uFF5CDSML\uFF5Cparameter>\n" +
		"</\uFF5CDSML\uFF5Cinvoke>\n" +
		"<\uFF5CDSML\uFF5Ccalls>"

	dsmlFixtureTwo = "<\uFF5CDSML\uFF5Ccalls>\n" +
		"<\uFF5CDSML\uFF5Cinvoke name=\"fetch_url\">\n" +
		"<\uFF5CDSML\uFF5Cparameter name=\"url\" string=\"true\">https://example.com</\uFF5CDSML\uFF5Cparameter>\n" +
		"<\uFF5CDSML\uFF5Cparameter name=\"limit\" string=\"false\">5</\uFF5CDSML\uFF5Cparameter>\n" +
		"</\uFF5CDSML\uFF5Cinvoke>\n" +
		"<\uFF5CDSML\uFF5Cinvoke name=\"run_python\">\n" +
		"<\uFF5CDSML\uFF5Cparameter name=\"code\" string=\"true\">print(\"hi\")</\uFF5CDSML\uFF5Cparameter>\n" +
		"</\uFF5CDSML\uFF5Cinvoke>\n" +
		"</\uFF5CDSML\uFF5Ccalls>"
)

func decodeArgs(t *testing.T, call ToolCall) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		t.Fatalf("arguments %q are not JSON: %v", call.Arguments, err)
	}
	return args
}

func TestParseDSMLCalls(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		names []string
		check func(t *testing.T, calls []ToolCall)
	}{
		{
			name:  "single unclosed at EOF",
			raw:   dsmlFixtureSingle,
			names: []string{"run_python"},
			check: func(t *testing.T, calls []ToolCall) {
				args := decodeArgs(t, calls[0])
				if got := args["code"]; got != "import json\nprint(1)\n" {
					t.Errorf("code = %q", got)
				}
				if calls[0].ID != "call_dsml_1" {
					t.Errorf("id = %q", calls[0].ID)
				}
			},
		},
		{
			name:  "two invokes with string=false",
			raw:   dsmlFixtureTwo,
			names: []string{"fetch_url", "run_python"},
			check: func(t *testing.T, calls []ToolCall) {
				args := decodeArgs(t, calls[0])
				if args["url"] != "https://example.com" {
					t.Errorf("url = %v", args["url"])
				}
				if args["limit"] != float64(5) {
					t.Errorf("limit = %v (%T), want the JSON number 5", args["limit"], args["limit"])
				}
				if got := decodeArgs(t, calls[1])["code"]; got != `print("hi")` {
					t.Errorf("code = %q", got)
				}
			},
		},
		{
			name:  "unknown tool name is preserved",
			raw:   "<\uFF5CDSML\uFF5Cinvoke name=\"email_list\">\n</\uFF5CDSML\uFF5Cinvoke>",
			names: []string{"email_list"},
			check: func(t *testing.T, calls []ToolCall) {
				if calls[0].Arguments != "{}" {
					t.Errorf("arguments = %q, want empty object", calls[0].Arguments)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := parseDSMLCalls(tc.raw)
			if len(calls) != len(tc.names) {
				t.Fatalf("got %d calls, want %d: %+v", len(calls), len(tc.names), calls)
			}
			for i, want := range tc.names {
				if calls[i].Name != want {
					t.Errorf("call %d name = %q, want %q", i, calls[i].Name, want)
				}
			}
			if tc.check != nil {
				tc.check(t, calls)
			}
		})
	}
}

func TestDSMLFilterStreaming(t *testing.T) {
	full := "Here you go.\n" + dsmlFixtureTwo + "\nAll done."
	want := "Here you go.\n\nAll done."
	for _, size := range []int{1, 2, 3, 5, 8, 11, 13, 64, len(full)} {
		t.Run(fmt.Sprintf("chunk_%d", size), func(t *testing.T) {
			var f DSMLFilter
			var got strings.Builder
			for i := 0; i < len(full); i += size {
				end := i + size
				if end > len(full) {
					end = len(full)
				}
				safe := f.Write(full[i:end])
				if strings.Contains(safe, "\uFF5CDSML") {
					t.Fatalf("chunk leaked markup: %q", safe)
				}
				got.WriteString(safe)
			}
			f.Close()
			if got.String() != want {
				t.Errorf("safe text = %q, want %q", got.String(), want)
			}
			if calls := parseDSMLCalls(f.Block()); len(calls) != 2 {
				t.Errorf("block parsed %d calls, want 2", len(calls))
			}
		})
	}
}

func TestDSMLFilterUnclosedBlock(t *testing.T) {
	var f DSMLFilter
	safe := f.Write("Before.\n" + dsmlFixtureSingle)
	f.Close()
	if safe != "Before.\n" {
		t.Errorf("safe text = %q", safe)
	}
	if calls := parseDSMLCalls(f.Block()); len(calls) != 1 || calls[0].Name != "run_python" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestSanitizeDSML(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"block between prose", "Before.\n" + dsmlFixtureTwo + "\nAfter.", "Before.\n\nAfter."},
		{"block only", dsmlFixtureSingle, ""},
		{"no markup", "plain answer", "plain answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeDSML(tc.in); got != tc.want {
				t.Errorf("SanitizeDSML = %q, want %q", got, tc.want)
			}
		})
	}
}
