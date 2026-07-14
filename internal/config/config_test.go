package config_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/clients"
	"github.com/example/agentchat/internal/config"
)

const sample = `{
  "providers": {
    "localai": {
      "env": {
        "OPENAI_API_BASE": "http://localhost:8080/v1",
        "OPENAI_API_KEY": "${TEST_LOCALAI_KEY}"
      }
    },
    "openrouter": {
      "label": "OpenRouter (keyring)",
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_API_KEY",
      "api_key_secret": { "provider": "openrouter", "token_type": "api_key" },
      "clients": [ "aider", "swival", "codex" ],
      "models": [ { "id": "qwen/qwen3-coder:free", "label": "Qwen3 Coder (free)" } ]
    }
  },
  "clients": {
    "aider": {
      "provider": "localai",
      "env": { "AIDER_CHECK_UPDATE": "false" },
      "extra": { "restore_chat_history": "true" },
      "default_effort": "medium",
      "models": [
        { "id": "openai/qwen3-coder", "label": "Qwen3 Coder (LocalAI)" },
        { "id": "sonnet", "label": "Sonnet (renamed)" }
      ]
    },
    "swival": {
      "extra": { "provider": "generic", "base_url": "http://localhost:8080/v1" },
      "models": [ { "id": "qwen3-coder", "label": "Qwen3 Coder" } ],
      "replace_models": true,
      "efforts": [ "low", "high" ],
      "replace_efforts": true
    },
    "claude": {
      "binary": "/opt/bin/claude-custom",
      "efforts": [ "high", "ultrathink" ]
    }
  }
}`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	// Missing file: empty usable config.
	c, err := config.Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || c == nil {
		t.Fatalf("missing file: %v, %v", c, err)
	}
	c.Apply("aider", &adapter.TurnRequest{}) // must not panic

	// Malformed file: loud error.
	if _, err := config.Load(writeConfig(t, "{not json")); err == nil {
		t.Fatal("malformed config accepted")
	}

	// Unknown provider reference: loud error.
	bad := `{"clients": {"aider": {"provider": "ghost"}}}`
	if _, err := config.Load(writeConfig(t, bad)); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown provider err = %v", err)
	}

	if _, err := config.Load(writeConfig(t, sample)); err != nil {
		t.Fatalf("sample config: %v", err)
	}
}

func TestApply(t *testing.T) {
	t.Setenv("TEST_LOCALAI_KEY", "sekrit")
	c, err := config.Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}

	req := adapter.TurnRequest{
		Env:   []string{"PRE=1"},
		Extra: map[string]string{"restore_chat_history": "false"}, // per-turn wins
	}
	c.Apply("aider", &req)

	wantEnv := []string{
		"PRE=1",
		"OPENAI_API_BASE=http://localhost:8080/v1",
		"OPENAI_API_KEY=sekrit", // ${VAR} expanded
		"AIDER_CHECK_UPDATE=false",
	}
	if !reflect.DeepEqual(req.Env, wantEnv) {
		t.Errorf("Env:\n got %v\nwant %v", req.Env, wantEnv)
	}
	if req.Extra["restore_chat_history"] != "false" {
		t.Errorf("per-turn Extra overwritten: %v", req.Extra)
	}
	if req.Effort != "medium" {
		t.Errorf("Effort = %q, want configured default \"medium\"", req.Effort)
	}

	// A per-turn effort beats the configured default.
	reqE := adapter.TurnRequest{Effort: "xhigh"}
	c.Apply("aider", &reqE)
	if reqE.Effort != "xhigh" {
		t.Errorf("per-turn Effort overwritten: %q", reqE.Effort)
	}

	// A client with only Extra defaults fills a nil map.
	req2 := adapter.TurnRequest{}
	c.Apply("swival", &req2)
	if req2.Extra["provider"] != "generic" || req2.Extra["base_url"] != "http://localhost:8080/v1" {
		t.Errorf("swival Extra = %v", req2.Extra)
	}

	// Unconfigured clients are untouched.
	req3 := adapter.TurnRequest{}
	c.Apply("codex", &req3)
	if len(req3.Env) != 0 || req3.Extra != nil {
		t.Errorf("codex request mutated: %+v", req3)
	}
}

func TestModelsMerging(t *testing.T) {
	c, err := config.Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	builtin := []adapter.Model{
		{ID: "", Label: "Default"},
		{ID: "sonnet", Label: "Claude Sonnet (alias)"},
	}

	// Append + dedupe by ID with configured label winning.
	got := c.Models("aider", builtin)
	want := []adapter.Model{
		{ID: "", Label: "Default"},
		{ID: "sonnet", Label: "Sonnet (renamed)"},
		{ID: "openai/qwen3-coder", Label: "Qwen3 Coder (LocalAI)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("aider models:\n got %v\nwant %v", got, want)
	}

	// replace_models drops built-ins.
	got = c.Models("swival", builtin)
	if len(got) != 1 || got[0].ID != "qwen3-coder" {
		t.Errorf("swival models = %v", got)
	}

	// Unconfigured client: built-ins untouched.
	if got := c.Models("codex", builtin); !reflect.DeepEqual(got, builtin) {
		t.Errorf("codex models = %v", got)
	}
}

func TestProviderDefs(t *testing.T) {
	c, err := config.Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}

	// aider sees both providers (localai unrestricted, openrouter listed).
	defs := c.ProviderDefs("aider")
	if len(defs) != 2 || defs[0].Name != "localai" || defs[1].Name != "openrouter" {
		t.Fatalf("aider defs = %+v", defs)
	}
	or := defs[1]
	if or.Label != "OpenRouter (keyring)" || or.Source != "config" ||
		or.BaseURL != "https://openrouter.ai/api/v1" || or.EnvKey != "OPENROUTER_API_KEY" ||
		or.KeySecret["token_type"] != "api_key" ||
		len(or.Models) != 1 || or.Models[0].ID != "qwen/qwen3-coder:free" {
		t.Errorf("openrouter def = %+v", or)
	}

	// claude is not in openrouter's clients list.
	defs = c.ProviderDefs("claude")
	if len(defs) != 1 || defs[0].Name != "localai" {
		t.Errorf("claude defs = %+v", defs)
	}

	// api_key_secret without api_key_env is a loud config error.
	bad := `{"providers": {"x": {"api_key_secret": {"a": "b"}}}}`
	if _, err := config.Load(writeConfig(t, bad)); err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Errorf("missing api_key_env err = %v", err)
	}
}

func TestEfforts(t *testing.T) {
	c, err := config.Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	builtin := []string{"low", "medium", "high"}

	// Configured efforts append with dedupe ("high" already built in).
	got := c.Efforts("claude", builtin)
	want := []string{"low", "medium", "high", "ultrathink"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claude efforts:\n got %v\nwant %v", got, want)
	}

	// replace_efforts drops built-ins.
	if got := c.Efforts("swival", builtin); !reflect.DeepEqual(got, []string{"low", "high"}) {
		t.Errorf("swival efforts = %v", got)
	}

	// Unconfigured client: built-ins untouched.
	if got := c.Efforts("codex", builtin); !reflect.DeepEqual(got, builtin) {
		t.Errorf("codex efforts = %v", got)
	}

	// Set.Efforts merges the adapter capability with config: echo
	// advertises low/medium/high and has no config entry.
	set := clients.New(c)
	efforts, err := set.Efforts(context.Background(), "echo")
	if err != nil || !reflect.DeepEqual(efforts, []string{"low", "medium", "high"}) {
		t.Errorf("echo efforts via Set = %v, %v", efforts, err)
	}
	// claude: adapter levels (low..max) + config's ultrathink, deduped.
	efforts, err = set.Efforts(context.Background(), "claude")
	if err != nil || !reflect.DeepEqual(efforts, []string{"low", "medium", "high", "xhigh", "max", "ultrathink"}) {
		t.Errorf("claude efforts via Set = %v, %v", efforts, err)
	}
	if _, err := set.Efforts(context.Background(), "nope"); err == nil {
		t.Error("unknown client accepted")
	}
}

func TestClientsSet(t *testing.T) {
	c, err := config.Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	set := clients.New(c)

	wantNames := []string{"aider", "claude", "codex", "echo", "swival"}
	if got := set.Registry.Names(); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("registry names = %v", got)
	}

	// Binary override surfaces in the availability error.
	cl, _ := set.Registry.Get("claude")
	if err := cl.Available(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "/opt/bin/claude-custom") {
		t.Errorf("claude Available err = %v, want override path mentioned", err)
	}

	// Merged model lists flow through Set.Models.
	models, err := set.Models(context.Background(), "swival")
	if err != nil || len(models) != 1 || models[0].ID != "qwen3-coder" {
		t.Errorf("swival models via Set = %v, %v", models, err)
	}

	// Prepare delegates to Config.Apply.
	req := adapter.TurnRequest{}
	if err := set.Prepare(context.Background(), "swival", &req); err != nil {
		t.Fatal(err)
	}
	if req.Extra["base_url"] == "" {
		t.Errorf("Prepare did not apply config: %+v", req)
	}

	// nil config is a valid empty set.
	empty := clients.New(nil)
	if got := empty.Registry.Names(); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("nil-config registry names = %v", got)
	}
}

type fakeSecrets struct{ val string }

func (f fakeSecrets) Lookup(context.Context, map[string]string) (string, error) {
	if f.val == "" {
		return "", fmt.Errorf("keyring says no")
	}
	return f.val, nil
}

func TestPrepareResolvesProvider(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TEST_LOCALAI_KEY", "sekrit")
	c, err := config.Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	set := clients.New(c)
	set.Secrets = fakeSecrets{val: "sk-or-fake"}
	set.CodexConfigPath = "" // no codex config in this test

	// Provider "openrouter" for aider: secret resolved into the env,
	// ProviderInfo enriched with the base URL.
	req := adapter.TurnRequest{Provider: &adapter.ProviderInfo{Name: "openrouter"}}
	if err := set.Prepare(ctx, "aider", &req); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range req.Env {
		if e == "OPENROUTER_API_KEY=sk-or-fake" {
			found = true
		}
		if strings.Contains(e, "sk-or-fake") && !strings.HasPrefix(e, "OPENROUTER_API_KEY=") {
			t.Errorf("secret in unexpected env entry %q", e)
		}
	}
	if !found {
		t.Errorf("secret not injected: %v", req.Env)
	}
	if req.Provider.BaseURL != "https://openrouter.ai/api/v1" || req.Provider.Subscription {
		t.Errorf("provider info = %+v", req.Provider)
	}

	// Secret failures fail Prepare loudly.
	set.Secrets = fakeSecrets{}
	req = adapter.TurnRequest{Provider: &adapter.ProviderInfo{Name: "openrouter"}}
	if err := set.Prepare(ctx, "aider", &req); err == nil || !strings.Contains(err.Error(), "keyring says no") {
		t.Errorf("secret failure = %v", err)
	}

	// Unknown provider names error and list what exists; nil/empty is
	// the client default and resolves nothing.
	req = adapter.TurnRequest{Provider: &adapter.ProviderInfo{Name: "nope"}}
	if err := set.Prepare(ctx, "aider", &req); err == nil || !strings.Contains(err.Error(), "openrouter") {
		t.Errorf("unknown provider err = %v", err)
	}
	req = adapter.TurnRequest{}
	if err := set.Prepare(ctx, "claude", &req); err != nil || len(req.Env) != 0 {
		t.Errorf("default: env=%v err=%v", req.Env, err)
	}

	// claude is restricted out of openrouter's clients list.
	req = adapter.TurnRequest{Provider: &adapter.ProviderInfo{Name: "openrouter"}}
	if err := set.Prepare(ctx, "claude", &req); err == nil {
		t.Error("clients restriction not honored")
	}

	// codex catalogs come from its own config file (read-only).
	codexToml := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(codexToml, []byte("[model_providers.openrouter]\nname = \"OpenRouter\"\nbase_url = \"https://openrouter.ai/api/v1\"\nenv_key = \"OPENROUTER_API_KEY\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set.Secrets = fakeSecrets{val: "sk-or-fake"}
	set.CodexConfigPath = codexToml
	provs, err := set.Providers(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 2 || !provs[0].Subscription || provs[1].Name != "openrouter" ||
		provs[1].KeySecret["provider"] != "openrouter" {
		t.Fatalf("codex providers = %+v", provs)
	}
	req = adapter.TurnRequest{Provider: &adapter.ProviderInfo{Name: "openrouter"}}
	if err := set.Prepare(ctx, "codex", &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Env) != 1 || req.Env[0] != "OPENROUTER_API_KEY=sk-or-fake" {
		t.Errorf("codex env = %v", req.Env)
	}
}
