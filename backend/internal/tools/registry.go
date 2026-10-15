package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Spec describes a tool to the model and to the gate's tests.
type Spec struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Risk        string
	AutoAllowed bool
}

// Registry is the set of tools the agent may call.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry registers the tools; names must be unique and non-empty.
func NewRegistry(ts ...Tool) (*Registry, error) {
	r := &Registry{tools: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		name := t.Name()
		if name == "" {
			return nil, errors.New("tools: a tool needs a name")
		}
		if _, dup := r.tools[name]; dup {
			return nil, fmt.Errorf("tools: duplicate tool %q", name)
		}
		r.tools[name] = t
	}
	return r, nil
}

// Get returns the tool with this name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names returns the tool names, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Specs returns a description of every tool, sorted by name.
func (r *Registry) Specs() []Spec {
	out := make([]Spec, 0, len(r.tools))
	for _, n := range r.Names() {
		t := r.tools[n]
		out = append(out, Spec{
			Name: n, Description: t.Description(), Schema: t.Schema(),
			Risk: t.Risk(nil).String(), AutoAllowed: t.AutoAllowed(),
		})
	}
	return out
}
