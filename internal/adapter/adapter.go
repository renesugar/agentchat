// Package adapter defines the contract between the AgentChat engine and
// terminal coding clients (Claude Code, Codex, Aider, Swival, ...).
//
// An Adapter runs one conversation turn by spawning its client
// non-interactively inside a workspace directory and translating the
// client's output into normalized Events.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Model is one model a client can be asked to use.
type Model struct {
	// ID is the value passed to the client (e.g. "gpt-5.6-sol",
	// "claude-sonnet-4-6", or an OpenAI-compatible provider/model ref).
	ID string `json:"id"`
	// Label is the human-readable name shown in the UI; defaults to ID.
	Label string `json:"label,omitempty"`
}

// TurnRequest describes one turn to execute.
type TurnRequest struct {
	// Prompt is the user's message for this turn.
	Prompt string
	// WorkDir is the workspace directory the client must operate in.
	WorkDir string
	// Model selects one of the adapter's models; empty = client default.
	Model string
	// SessionID resumes a prior session when the client supports it
	// (value comes from a previous Result.SessionID).
	SessionID string
	// Effort is the reasoning-effort level for this turn; empty = client
	// default. Values pass through to the client, which owns validation
	// (the common scale is none/low/medium/high, plus client-specific
	// extremes like claude's xhigh/max). Adapters map it to the client's
	// flag and only drop it when the client has no effort control at all.
	Effort string
	// Env is extra environment (API keys, base URLs such as a LocalAI
	// endpoint) appended to the client process environment.
	Env []string
	// Extra carries adapter-specific options without widening this struct.
	Extra map[string]string
	// MCP, when non-nil, points the client at the app's MCP callback
	// server so it can push progress/artifacts directly during the turn.
	// Adapters for clients without MCP support ignore it; output capture
	// remains the baseline transport either way.
	MCP *MCPServerInfo
}

// MCPServerInfo describes the app's per-turn MCP callback endpoint
// (streamable HTTP on loopback, authenticated by a turn-scoped bearer
// token). Filled in by the engine when an MCP server is configured.
type MCPServerInfo struct {
	// Name is the MCP server name the client sees (tool names become
	// e.g. mcp__<name>__progress in Claude Code).
	Name string
	// URL is the streamable-HTTP endpoint, e.g. "http://127.0.0.1:PORT/mcp".
	URL string
	// Token authorizes exactly this turn's channel ("Authorization:
	// Bearer <token>"); it is revoked when the turn finishes.
	Token string
}

// EmitFunc receives normalized events as the turn progresses. Adapters call
// it from a single goroutine, in order.
type EmitFunc func(Event)

// Adapter is implemented once per supported coding client.
type Adapter interface {
	// Name is the stable registry key, e.g. "claude", "codex", "aider",
	// "swival", "echo".
	Name() string
	// Available reports whether the client can run on this machine
	// (typically: binary found on PATH). The error explains why not.
	Available(ctx context.Context) error
	// Models lists models selectable for this client. May be a static
	// list; must not require network in tests.
	Models(ctx context.Context) ([]Model, error)
	// RunTurn executes one turn, streaming events via emit. It must emit a
	// terminal EventResult (matching the returned *Result) exactly once,
	// respect ctx cancellation, and leave the workspace in whatever state
	// the client produced (snapshotting is the engine's job).
	RunTurn(ctx context.Context, req TurnRequest, emit EmitFunc) (*Result, error)
}

// ErrUnknownAdapter is returned by Registry.Get for unregistered names.
var ErrUnknownAdapter = errors.New("unknown adapter")

// Registry holds the set of available adapters.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[string]Adapter)}
}

// Register adds a; it panics on duplicate names (a programming error).
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := a.Name()
	if _, dup := r.m[name]; dup {
		panic(fmt.Sprintf("adapter %q registered twice", name))
	}
	r.m[name] = a
}

// Get returns the adapter registered under name.
func (r *Registry) Get(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.m[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAdapter, name)
	}
	return a, nil
}

// Names returns registered adapter names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
