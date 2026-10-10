package tools

import (
	"fmt"
	"sort"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
)

// Registry holds the tools an engine offers to the model. It is safe for
// concurrent use; the adapted snapshot returned by Adapt is deterministic
// (sorted by name) so recreating a runner over the same registry yields a
// byte-identical tool list — a requirement for prompt-cache stability.
type Registry struct {
	mu    sync.Mutex
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool. Duplicate names are rejected.
func (r *Registry) Register(t Tool) error {
	if t == nil || t.Name() == "" {
		return fmt.Errorf("tools: nil or unnamed tool")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name()]; exists {
		return fmt.Errorf("tools: %q already registered", t.Name())
	}
	r.tools[t.Name()] = t
	return nil
}

// Get returns a registered tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names returns the registered tool names, sorted.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Adapt wraps every registered tool with the given per-session policy and
// returns them sorted by name, ready to hand to the tool runner.
func (r *Registry) Adapt(cfg AdapterConfig) []anthropic.BetaTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]anthropic.BetaTool, 0, len(names))
	for _, n := range names {
		out = append(out, Adapt(r.tools[n], cfg))
	}
	return out
}
