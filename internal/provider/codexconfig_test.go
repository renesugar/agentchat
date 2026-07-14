package provider

import (
	"os"
	"path/filepath"
	"testing"
)

// sample mirrors real-world codex configs: the user's own shape (top-level
// provider/model/effort, projects tables) plus the OpenRouter recipe from
// the codex docs (wire_api, env_key), comments, quoted table keys, and
// syntax the mini-parser must skip without derailing.
const sampleCodexConfig = `# ~/.codex/config.toml
model = "gpt-5.6-sol"
model_provider = "openrouter"
model_reasoning_effort = "medium"

approval_policy = "on-request"
sandbox_mode = "workspace-write"   # trailing comment

[sandbox_workspace_write]
network_access = false

[projects."/home/renes/projects/test"]
trust_level = "trusted"

[features]
skills = true

[model_providers.openrouter]
name = "OpenRouter"
base_url = "https://openrouter.ai/api/v1"
env_key = "OPENROUTER_API_KEY"
wire_api = "responses"

[model_providers."ollama-local"]
name = 'Ollama'
base_url = "http://localhost:11434/v1"
# no env_key: local server needs none

[model_providers.notes]
description = """multi-line
strings are consumed
and dropped"""
query_params = { key = "value" }
tags = ["a", "b"]
`

func TestReadCodexConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(sampleCodexConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadCodexConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProvider != "openrouter" || cfg.DefaultModel != "gpt-5.6-sol" {
		t.Errorf("defaults = %q / %q", cfg.DefaultProvider, cfg.DefaultModel)
	}
	if len(cfg.Providers) != 3 {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	// Sorted by name: notes, ollama-local, openrouter... no — lexical:
	// "notes" < "ollama-local" < "openrouter".
	notes, ollama, or := cfg.Providers[0], cfg.Providers[1], cfg.Providers[2]
	if or.Name != "openrouter" || or.Label != "OpenRouter" || or.Source != "codex" ||
		or.BaseURL != "https://openrouter.ai/api/v1" || or.EnvKey != "OPENROUTER_API_KEY" {
		t.Errorf("openrouter = %+v", or)
	}
	if ollama.Name != "ollama-local" || ollama.Label != "Ollama" ||
		ollama.BaseURL != "http://localhost:11434/v1" || ollama.EnvKey != "" {
		t.Errorf("ollama-local = %+v", ollama)
	}
	if notes.Name != "notes" || notes.Label != "notes" || notes.BaseURL != "" {
		t.Errorf("notes = %+v", notes)
	}
}

func TestReadCodexConfigMissing(t *testing.T) {
	cfg, err := ReadCodexConfig(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || len(cfg.Providers) != 0 || cfg.DefaultProvider != "" {
		t.Fatalf("missing file: %+v, %v", cfg, err)
	}
	if cfg, err := ReadCodexConfig(""); err != nil || len(cfg.Providers) != 0 {
		t.Fatalf("empty path: %+v, %v", cfg, err)
	}
}

func TestCodexConfigPath(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/codex")
	if got := CodexConfigPath(); got != "/custom/codex/config.toml" {
		t.Errorf("CODEX_HOME path = %q", got)
	}
	t.Setenv("CODEX_HOME", "")
	if got := CodexConfigPath(); !filepath.IsAbs(got) || filepath.Base(got) != "config.toml" {
		t.Errorf("default path = %q", got)
	}
}

func TestParseTomlValue(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{`"basic"`, "basic", true},
		{`"esc \"q\" and \\ and \n"`, "esc \"q\" and \\ and \n", true},
		{`'literal "as-is"'`, `literal "as-is"`, true},
		{`true`, "true", true},
		{`42 # comment`, "42", true},
		{`["array"]`, "", false},
		{`{ inline = "table" }`, "", false},
		{`"unterminated`, "", false},
	}
	for _, c := range cases {
		got, ok := parseTomlValue(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("parseTomlValue(%s) = %q, %v; want %q, %v", c.raw, got, ok, c.want, c.ok)
		}
	}
}
