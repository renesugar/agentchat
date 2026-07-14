// Package clients assembles the adapter registry with configuration
// applied, so the CLI harness and the desktop app build identical client
// sets from one place.
package clients

import (
	"context"
	"fmt"
	"strings"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/adapters/aider"
	"github.com/example/agentchat/internal/adapters/claudecode"
	"github.com/example/agentchat/internal/adapters/codex"
	"github.com/example/agentchat/internal/adapters/echo"
	"github.com/example/agentchat/internal/adapters/swival"
	"github.com/example/agentchat/internal/config"
	"github.com/example/agentchat/internal/provider"
)

// Set couples the registry with the configuration that shapes turns.
type Set struct {
	Registry *adapter.Registry
	Config   *config.Config
	// Secrets resolves provider API keys at turn time (defaults to the
	// platform store; tests substitute fakes).
	Secrets provider.SecretStore
	// CodexConfigPath is where codex's own config is read (READ-ONLY)
	// for its declared model providers. Defaults to ~/.codex/config.toml
	// (honoring $CODEX_HOME).
	CodexConfigPath string
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
	return &Set{
		Registry:        reg,
		Config:          cfg,
		Secrets:         provider.PlatformStore(),
		CodexConfigPath: provider.CodexConfigPath(),
	}
}

// Providers returns a client's provider catalog: the builtin default
// (subscription / inherited environment) first, then the client-relevant
// definitions — config.json providers, and for codex the providers its
// own config declares (overlaid with same-named config.json entries).
func (s *Set) Providers(ctx context.Context, client string) ([]provider.Def, error) {
	if _, err := s.Registry.Get(client); err != nil {
		return nil, err
	}
	var codexDefs []provider.Def
	if client == "codex" {
		cc, err := provider.ReadCodexConfig(s.CodexConfigPath)
		if err != nil {
			return nil, err
		}
		codexDefs = cc.Providers
	}
	return provider.Catalog(client, s.Config.ProviderDefs(client), codexDefs), nil
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

// Prepare readies a turn request: applies the client's configured
// defaults (env, extra values, default effort) and resolves the selected
// provider — filling req.Provider's BaseURL/Subscription and appending
// the provider's environment, API key included (fetched from the
// platform secret store; a failed lookup fails the turn loudly). Call
// before Engine.RunTurn.
func (s *Set) Prepare(ctx context.Context, client string, req *adapter.TurnRequest) error {
	s.Config.Apply(client, req)

	if req.Provider == nil || req.Provider.Name == "" {
		return nil // client default: subscription / inherited environment
	}
	defs, err := s.Providers(ctx, client)
	if err != nil {
		return err
	}
	for _, d := range defs {
		if d.Name != req.Provider.Name {
			continue
		}
		env, err := d.ResolveEnv(ctx, s.Secrets)
		if err != nil {
			return err
		}
		req.Env = append(req.Env, env...)
		if req.Provider.BaseURL == "" {
			req.Provider.BaseURL = d.BaseURL
		}
		req.Provider.Subscription = d.Subscription
		return nil
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		if d.Name != "" {
			names = append(names, d.Name)
		}
	}
	return fmt.Errorf("client %q has no provider %q (available: default%s)",
		client, req.Provider.Name, strings.Join(append([]string{""}, names...), ", "))
}
