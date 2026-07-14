package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultAndCatalog(t *testing.T) {
	for _, client := range []string{"claude", "codex"} {
		d := Default(client)
		if !d.Subscription || d.Name != "" || !strings.Contains(d.Label, "Subscription") {
			t.Errorf("%s default = %+v", client, d)
		}
	}
	if d := Default("aider"); !d.Subscription || strings.Contains(d.Label, "Subscription") {
		t.Errorf("aider default = %+v", d)
	}

	configDefs := []Def{
		{Name: "openrouter", Source: "config", KeySecret: map[string]string{"provider": "openrouter"}, EnvKey: "OPENROUTER_API_KEY"},
		{Name: "localai", Source: "config", BaseURL: "http://localhost:8080/v1"},
	}
	codexDefs := []Def{
		{Name: "openrouter", Label: "OpenRouter", Source: "codex",
			BaseURL: "https://openrouter.ai/api/v1", EnvKey: "OPENROUTER_API_KEY"},
	}

	// Non-codex clients: builtin + config defs as-is.
	cat := Catalog("aider", configDefs, codexDefs)
	if len(cat) != 3 || !cat[0].Subscription || cat[1].Name != "openrouter" || cat[2].Name != "localai" {
		t.Fatalf("aider catalog = %+v", cat)
	}

	// codex: only codex-declared providers, with config overlaid by name
	// (key_secret from config, base_url/label from codex); config-only
	// names (localai) are dropped — codex cannot use them.
	cat = Catalog("codex", configDefs, codexDefs)
	if len(cat) != 2 {
		t.Fatalf("codex catalog = %+v", cat)
	}
	or := cat[1]
	if or.Label != "OpenRouter" || or.BaseURL != "https://openrouter.ai/api/v1" ||
		or.KeySecret["provider"] != "openrouter" || or.Source != "codex" {
		t.Errorf("merged openrouter = %+v", or)
	}
}

type fakeStore struct {
	val  string
	err  error
	seen map[string]string
}

func (f *fakeStore) Lookup(_ context.Context, attrs map[string]string) (string, error) {
	f.seen = attrs
	return f.val, f.err
}

func TestResolveEnv(t *testing.T) {
	ctx := context.Background()

	// Subscription: nothing, ever.
	if env, err := Default("claude").ResolveEnv(ctx, nil); err != nil || env != nil {
		t.Fatalf("subscription env = %v, %v", env, err)
	}

	// Env map: sorted, ${VAR}-expanded.
	t.Setenv("PROV_TEST_HOST", "localhost")
	d := Def{Name: "localai", Env: map[string]string{
		"B_URL": "http://${PROV_TEST_HOST}:8080",
		"A_KEY": "plain",
	}}
	env, err := d.ResolveEnv(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A_KEY=plain", "B_URL=http://localhost:8080"}; !reflect.DeepEqual(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}

	// KeySecret resolves through the store into EnvKey.
	store := &fakeStore{val: "sk-or-secret"}
	d = Def{Name: "openrouter", EnvKey: "OPENROUTER_API_KEY",
		KeySecret: map[string]string{"provider": "openrouter", "token_type": "api_key"}}
	env, err = d.ResolveEnv(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env, []string{"OPENROUTER_API_KEY=sk-or-secret"}) {
		t.Errorf("env = %v", env)
	}
	if store.seen["token_type"] != "api_key" {
		t.Errorf("store saw %v", store.seen)
	}

	// Loud failures: secret error propagates; key_secret without env_key
	// and a missing store are configuration errors.
	store.err = fmt.Errorf("no such secret")
	if _, err := d.ResolveEnv(ctx, store); err == nil || !strings.Contains(err.Error(), "no such secret") {
		t.Errorf("secret error = %v", err)
	}
	bad := Def{Name: "x", KeySecret: map[string]string{"a": "b"}}
	if _, err := bad.ResolveEnv(ctx, store); err == nil || !strings.Contains(err.Error(), "env_key") {
		t.Errorf("missing env_key error = %v", err)
	}
	if _, err := d.ResolveEnv(ctx, nil); err == nil {
		t.Error("nil store accepted for key_secret")
	}
}

// TestSecretToolLookup exercises the real exec path against a stub
// secret-tool on PATH, asserting the attribute order is deterministic
// and failure modes are loud.
func TestSecretToolLookup(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	stub := filepath.Join(dir, "secret-tool")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\nif [ \"$3\" = \"missing\" ]; then exit 1; fi\nif [ \"$3\" = \"empty\" ]; then exit 0; fi\necho sk-from-keyring\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := execStore{tool: "secret-tool"}
	ctx := context.Background()

	val, err := store.Lookup(ctx, map[string]string{"token_type": "api_key", "provider": "openrouter"})
	if err != nil {
		t.Fatal(err)
	}
	if val != "sk-from-keyring" {
		t.Errorf("value = %q", val)
	}
	// Attributes sorted by key: provider before token_type.
	if b, _ := os.ReadFile(argsFile); strings.TrimSpace(string(b)) != "lookup provider openrouter token_type api_key" {
		t.Errorf("args = %q", b)
	}

	if _, err := store.Lookup(ctx, map[string]string{"provider": "missing"}); err == nil {
		t.Error("nonzero exit not surfaced")
	}
	if _, err := store.Lookup(ctx, map[string]string{"provider": "empty"}); err == nil || !strings.Contains(err.Error(), "no secret") {
		t.Errorf("empty secret err = %v", err)
	}
	if _, err := store.Lookup(ctx, nil); err == nil {
		t.Error("empty attrs accepted")
	}
	if _, err := (execStore{tool: "no-such-secret-tool-xyz"}).Lookup(ctx, map[string]string{"a": "b"}); err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("missing tool err = %v", err)
	}
}

func TestUnsupportedStore(t *testing.T) {
	_, err := (unsupportedStore{goos: "plan9"}).Lookup(context.Background(), map[string]string{"a": "b"})
	if err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Errorf("err = %v", err)
	}
}
