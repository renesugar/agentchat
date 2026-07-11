// Package clients assembles the adapter registry with configuration
// applied, so the CLI harness and the desktop app build identical client
// sets from one place.
package clients

import (
	"context"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/adapters/aider"
	"github.com/example/agentchat/internal/adapters/claudecode"
	"github.com/example/agentchat/internal/adapters/codex"
	"github.com/example/agentchat/internal/adapters/echo"
	"github.com/example/agentchat/internal/adapters/swival"
	"github.com/example/agentchat/internal/config"
)

// Set couples the registry with the configuration that shapes turns.
type Set struct {
	Registry *adapter.Registry
	Config   *config.Config
}

// New builds the full registry, applying configured binary overrides.
func New(cfg *config.Config) *Set {
	if cfg == nil {
		cfg = &config.Config{}
	}
	reg := adapter.NewRegistry()

	cc := claudecode.New()
	if b := cfg.Binary(cc.Name()); b != "" {
		cc.Binary = b
	}
	reg.Register(cc)

	cx := codex.New()
	if b := cfg.Binary(cx.Name()); b != "" {
		cx.Binary = b
	}
	reg.Register(cx)

	ai := aider.New()
	if b := cfg.Binary(ai.Name()); b != "" {
		ai.Binary = b
	}
	reg.Register(ai)

	sw := swival.New()
	if b := cfg.Binary(sw.Name()); b != "" {
		sw.Binary = b
	}
	reg.Register(sw)

	reg.Register(echo.New())
	return &Set{Registry: reg, Config: cfg}
}

// Models returns a client's model picker entries: the adapter's built-in
// list merged with configured additions (see config.Config.Models).
func (s *Set) Models(ctx context.Context, client string) ([]adapter.Model, error) {
	a, err := s.Registry.Get(client)
	if err != nil {
		return nil, err
	}
	builtin, err := a.Models(ctx)
	if err != nil {
		return nil, err
	}
	return s.Config.Models(client, builtin), nil
}

// Efforts returns a client's effort picker entries: the adapter's
// advertised levels (nil when the adapter has no effort control) merged
// with configured additions (see config.Config.Efforts). "" (client
// default) is implied and never part of the list.
func (s *Set) Efforts(ctx context.Context, client string) ([]string, error) {
	a, err := s.Registry.Get(client)
	if err != nil {
		return nil, err
	}
	var builtin []string
	if el, ok := a.(adapter.EffortLister); ok {
		builtin = el.Efforts()
	}
	return s.Config.Efforts(client, builtin), nil
}

// Prepare applies the client's configured defaults (provider env, extra
// values) to a turn request. Call before Engine.RunTurn.
func (s *Set) Prepare(client string, req *adapter.TurnRequest) {
	s.Config.Apply(client, req)
}
