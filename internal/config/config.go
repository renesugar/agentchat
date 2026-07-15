// Package config loads user configuration: model providers (named sets of
// environment variables — API keys, base URLs such as a LocalAI endpoint)
// and per-client overrides (binary path, default provider, extra model
// picker entries, default TurnRequest.Extra values).
//
// The file is JSON at <data-dir>/config.json. A missing file is a valid
// empty configuration. Environment values support ${VAR} expansion from
// the process environment so secrets never need to live in the file:
//
//	{
//	  "providers": {
//	    "localai": {
//	      "env": {
//	        "OPENAI_API_BASE": "http://localhost:8080/v1",
//	        "OPENAI_BASE_URL": "http://localhost:8080/v1",
//	        "OPENAI_API_KEY": "${LOCALAI_API_KEY}"
//	      }
//	    }
//	  },
//	  "clients": {
//	    "aider":  { "provider": "localai",
//	                "models": [{ "id": "openai/qwen3-coder", "label": "Qwen3 Coder (LocalAI)" }] },
//	    "swival": { "extra": { "provider": "generic",
//	                            "base_url": "http://localhost:8080/v1" } },
//	    "claude": { "binary": "/opt/claude/bin/claude" }
//	  }
//	}
//
// Each coding client reads different variables, so provider entries are
// deliberately explicit env maps rather than an abstract base_url/api_key
// pair the app would have to translate per client. docs/adapters.md and
// docs/config.example.json carry per-client recipes.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/renesugar/agentchat/internal/adapter"
	"github.com/renesugar/agentchat/internal/provider"
)

// Provider is a named way to reach models: a set of environment
// variables handed to client processes (values expanded with ${VAR}
// against the process env), optionally an endpoint plus an API key
// sourced from the platform secret store at turn time (Step 27) — key
// VALUES never live in this file.
type Provider struct {
	Env map[string]string `json:"env,omitempty"`
	// Label is the human-readable picker text; defaults to the name.
	Label string `json:"label,omitempty"`
	// BaseURL is the API endpoint, for clients that take one (swival
	// --base-url; claude ANTHROPIC_BASE_URL; codex reads its own config).
	BaseURL string `json:"base_url,omitempty"`
	// APIKeyEnv names the environment variable the client reads the API
	// key from (e.g. OPENROUTER_API_KEY, ANTHROPIC_API_KEY).
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// APIKeySecret holds the platform secret-store lookup attributes for
	// the key's value (Linux: `secret-tool lookup k1 v1 k2 v2...`).
	// Requires APIKeyEnv. The value is fetched per turn, never stored.
	APIKeySecret map[string]string `json:"api_key_secret,omitempty"`
	// Clients restricts which coding clients offer this provider in
	// their pickers; empty = all clients.
	Clients []string `json:"clients,omitempty"`
	// Models offered by this provider (for the cascading pickers).
	Models []adapter.Model `json:"models,omitempty"`
}

// Client holds per-adapter overrides.
type Client struct {
	// Binary overrides the executable name/path.
	Binary string `json:"binary,omitempty"`
	// Provider names an entry in Config.Providers whose env is applied to
	// every turn of this client.
	Provider string `json:"provider,omitempty"`
	// Env is client-specific extra environment, applied after (and thus
	// overriding) the provider's. Values support ${VAR} expansion.
	Env map[string]string `json:"env,omitempty"`
	// Extra sets default TurnRequest.Extra values (per-turn values win).
	Extra map[string]string `json:"extra,omitempty"`
	// DefaultEffort sets TurnRequest.Effort when the turn doesn't pick a
	// level (per-turn values win). Passed through to the client, which
	// owns validation — e.g. claude: low/medium/high/xhigh/max.
	DefaultEffort string `json:"default_effort,omitempty"`
	// Models are added to the client's model picker; with ReplaceModels
	// they replace the adapter's built-in list entirely.
	Models        []adapter.Model `json:"models,omitempty"`
	ReplaceModels bool            `json:"replace_models,omitempty"`
	// Efforts are added to the client's effort picker (deduplicated);
	// with ReplaceEfforts they replace the adapter's built-in levels
	// entirely. Values pass through to the client, which owns
	// validation.
	Efforts        []string `json:"efforts,omitempty"`
	ReplaceEfforts bool     `json:"replace_efforts,omitempty"`
}

// Config is the root of config.json.
type Config struct {
	Providers map[string]Provider `json:"providers,omitempty"`
	Clients   map[string]Client   `json:"clients,omitempty"`
}

// Load reads the config file at path. A missing file yields an empty,
// usable config; a malformed file is an error (silent misconfig is worse).
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	for name, cl := range c.Clients {
		if cl.Provider == "" {
			continue
		}
		if _, ok := c.Providers[cl.Provider]; !ok {
			return fmt.Errorf("client %q references unknown provider %q", name, cl.Provider)
		}
	}
	for name, p := range c.Providers {
		if len(p.APIKeySecret) > 0 && p.APIKeyEnv == "" {
			return fmt.Errorf("provider %q sets api_key_secret without api_key_env (which variable should receive the key?)", name)
		}
	}
	return nil
}

// ProviderDefs converts the configured providers into provider.Def
// values for a client's catalog, honoring each provider's Clients
// restriction. Sorted by name for stable pickers.
func (c *Config) ProviderDefs(client string) []provider.Def {
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []provider.Def
	for _, name := range names {
		p := c.Providers[name]
		if len(p.Clients) > 0 && !slices.Contains(p.Clients, client) {
			continue
		}
		label := p.Label
		if label == "" {
			label = name
		}
		out = append(out, provider.Def{
			Name:      name,
			Label:     label,
			Source:    "config",
			BaseURL:   p.BaseURL,
			EnvKey:    p.APIKeyEnv,
			KeySecret: p.APIKeySecret,
			Env:       p.Env,
			Models:    p.Models,
		})
	}
	return out
}

// Binary returns the configured binary override for a client ("" = none).
func (c *Config) Binary(client string) string {
	return c.Clients[client].Binary
}

// Apply fills a TurnRequest with the client's configured defaults:
// provider env then client env are appended to req.Env (later entries win
// for duplicate variables in the child process), Extra defaults are set
// only where the request doesn't already define the key, and
// DefaultEffort applies only when the request picked no effort.
func (c *Config) Apply(client string, req *adapter.TurnRequest) {
	cl, ok := c.Clients[client]
	if !ok {
		return
	}
	if p, ok := c.Providers[cl.Provider]; ok {
		req.Env = append(req.Env, expandEnv(p.Env)...)
	}
	req.Env = append(req.Env, expandEnv(cl.Env)...)

	if req.Effort == "" {
		req.Effort = cl.DefaultEffort
	}

	if len(cl.Extra) > 0 {
		if req.Extra == nil {
			req.Extra = make(map[string]string, len(cl.Extra))
		}
		for k, v := range cl.Extra {
			if _, exists := req.Extra[k]; !exists {
				req.Extra[k] = v
			}
		}
	}
}

// Models merges the adapter's built-in model list with configured entries.
// ReplaceModels drops the built-ins; otherwise configured models are
// appended (deduplicated by ID, configured labels win).
func (c *Config) Models(client string, builtin []adapter.Model) []adapter.Model {
	cl, ok := c.Clients[client]
	if !ok || len(cl.Models) == 0 {
		return builtin
	}
	if cl.ReplaceModels {
		return cl.Models
	}
	out := make([]adapter.Model, 0, len(builtin)+len(cl.Models))
	index := make(map[string]int)
	for _, m := range builtin {
		index[m.ID] = len(out)
		out = append(out, m)
	}
	for _, m := range cl.Models {
		if i, dup := index[m.ID]; dup {
			out[i] = m
			continue
		}
		index[m.ID] = len(out)
		out = append(out, m)
	}
	return out
}

// Efforts merges the adapter's built-in effort levels with configured
// entries. ReplaceEfforts drops the built-ins; otherwise configured
// levels are appended (deduplicated, order preserved).
func (c *Config) Efforts(client string, builtin []string) []string {
	cl, ok := c.Clients[client]
	if !ok || len(cl.Efforts) == 0 {
		return builtin
	}
	if cl.ReplaceEfforts {
		return cl.Efforts
	}
	out := make([]string, 0, len(builtin)+len(cl.Efforts))
	seen := make(map[string]bool)
	for _, e := range builtin {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	for _, e := range cl.Efforts {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// expandEnv renders an env map as sorted KEY=VALUE strings with ${VAR}
// expansion against the process environment. Sorting keeps output
// deterministic; later slices appended by Apply still win in the child
// process, since duplicate variables resolve to the last occurrence.
func expandEnv(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, k+"="+os.Expand(m[k], os.Getenv))
	}
	return out
}
