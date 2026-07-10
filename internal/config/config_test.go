package config_test

import (
	"context"
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
      "replace_models": true
    },
    "claude": { "binary": "/opt/bin/claude-custom" }
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
	set.Prepare("swival", &req)
	if req.Extra["base_url"] == "" {
		t.Errorf("Prepare did not apply config: %+v", req)
	}

	// nil config is a valid empty set.
	empty := clients.New(nil)
	if got := empty.Registry.Names(); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("nil-config registry names = %v", got)
	}
}
