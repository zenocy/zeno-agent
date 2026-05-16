package llm

import (
	"context"
	"sort"
	"sync"
)

// Tool is one callable read-side capability the synthesizer exposes to the
// LLM tool-using loop. Phase 2 wires three tools (read_thread, read_event,
// read_weather_window); the interface is intentionally small.
type Tool interface {
	Name() string
	Description() string
	Parameters() []ToolParamSpec
	Execute(ctx context.Context, args map[string]any) (string, error)
}

// ToolRegistry is a lookup of Tool by name plus a stable Definitions slice
// suitable for passing to ChatCompletion. The registry is safe for concurrent
// reads but is intended to be built once and frozen before the loop runs.
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry constructs a registry seeded with the given tools.
func NewRegistry(tools ...Tool) *ToolRegistry {
	r := &ToolRegistry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		if t == nil {
			continue
		}
		r.tools[t.Name()] = t
	}
	return r
}

// Register adds (or replaces) a tool by name.
func (r *ToolRegistry) Register(t Tool) {
	if t == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get returns the tool registered under name, or false if absent.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Len returns the number of registered tools.
func (r *ToolRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Names returns registered tool names sorted alphabetically.
func (r *ToolRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Definitions returns the tool list in a stable order for the ChatCompletion
// request. Order is alphabetical by name so prompt caches stay warm across
// runs that use the same registry.
func (r *ToolRegistry) Definitions() []ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ToolDefinition, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		out = append(out, ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return out
}
