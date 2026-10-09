package tools

import (
	"context"
	"encoding/json"
	"testing"
)

type stubTool struct {
	name string
	desc string
	run  func(args json.RawMessage) (string, error)
}

func (s stubTool) Name() string                { return s.name }
func (s stubTool) Description() string         { return s.desc }
func (s stubTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s stubTool) Run(_ context.Context, args json.RawMessage) (string, error) {
	return s.run(args)
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if r.Len() != 0 {
		t.Fatalf("new registry len = %d, want 0", r.Len())
	}
	r.Add(stubTool{name: "b"})
	r.Add(stubTool{name: "a"})
	r.Add(stubTool{name: "b", desc: "replaced"}) // replace keeps position

	specs := r.Specs()
	if len(specs) != 2 {
		t.Fatalf("specs len = %d, want 2", len(specs))
	}
	if specs[0].Name != "b" || specs[1].Name != "a" {
		t.Errorf("order = %q, %q; want b, a", specs[0].Name, specs[1].Name)
	}
	if specs[0].Description != "replaced" {
		t.Errorf("replacement not applied: %q", specs[0].Description)
	}
	if _, ok := r.Get("a"); !ok {
		t.Error("Get(a) missing")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) should be absent")
	}
}

func TestDecodeArgs(t *testing.T) {
	var in struct {
		Query string `json:"query"`
	}
	if err := DecodeArgs(nil, &in); err != nil {
		t.Fatalf("nil args: %v", err)
	}
	if err := DecodeArgs(json.RawMessage(""), &in); err != nil {
		t.Fatalf("empty args: %v", err)
	}
	if err := DecodeArgs(json.RawMessage("null"), &in); err != nil {
		t.Fatalf("null args: %v", err)
	}
	if err := DecodeArgs(json.RawMessage(`{"query":"hi"}`), &in); err != nil || in.Query != "hi" {
		t.Fatalf("valid args = %q, %v", in.Query, err)
	}
	if err := DecodeArgs(json.RawMessage(`{bad`), &in); err == nil {
		t.Fatal("invalid args should error")
	}
}
