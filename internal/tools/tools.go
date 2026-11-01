// Package tools implements the agentic tool framework for MyOGChat (Phase
// 2.4): a normalized Tool interface, an ordered per-request registry, and the
// built-in tools — web_search (Tavily), fetch_url (readable-text extraction,
// Phase 2.8) and the Linkwarden search/browse/save tools.
//
// The package is deliberately free of upstream/server types so it can be unit
// tested in isolation. The server bridges registry Specs to the upstream
// adapter's wire types.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tool is one callable function exposed to the model.
type Tool interface {
	// Name is the function name the model calls (snake_case).
	Name() string
	// Description tells the model what the tool does and when to use it.
	Description() string
	// Parameters returns the JSON-schema object describing the arguments.
	Parameters() json.RawMessage
	// Run executes the tool with raw JSON arguments and returns a textual
	// result that is fed back to the model.
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// Spec is the model-facing description of a tool.
type Spec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Registry is an ordered set of tools keyed by name. A fresh registry is built
// per request so per-user/per-setting tools (e.g. Linkwarden) are isolated.
type Registry struct {
	order  []string
	byName map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Tool{}}
}

// Add registers t, replacing any existing tool with the same name while
// preserving its position.
func (r *Registry) Add(t Tool) {
	name := t.Name()
	if _, ok := r.byName[name]; !ok {
		r.order = append(r.order, name)
	}
	r.byName[name] = t
}

// Get returns the tool registered under name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Len reports how many tools are registered.
func (r *Registry) Len() int { return len(r.order) }

// Specs returns the tools in registration order.
func (r *Registry) Specs() []Spec {
	specs := make([]Spec, 0, len(r.order))
	for _, name := range r.order {
		t := r.byName[name]
		specs = append(specs, Spec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return specs
}

// DecodeArgs unmarshals tool arguments into v. Empty/whitespace arguments are
// treated as an empty JSON object so tools with all-optional fields work.
func DecodeArgs(args json.RawMessage, v any) error {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		trimmed = "{}"
	}
	if err := json.Unmarshal([]byte(trimmed), v); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}

// schemaObj marshals a JSON-schema object; it panics only on programmer error
// (the schemas are static literals).
func schemaObj(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("tools: invalid static JSON schema: " + err.Error())
	}
	return b
}
