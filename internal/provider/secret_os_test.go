package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stub-tool tests: the darwin/windows stores are pure exec code, so the
// argument/env mapping and error paths run anywhere. Real macOS/Windows
// verification is tracked in PLAN.md step 32.

func writeStub(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinStoreStub(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	writeStub(t, dir, "security",
		"#!/bin/sh\necho \"$@\" > "+argsFile+"\nif [ \"$3\" = \"missing\" ]; then echo nope >&2; exit 44; fi\necho pw-from-keychain\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := darwinStore{tool: "security"}
	ctx := context.Background()

	val, err := store.Lookup(ctx, map[string]string{"service": "openrouter", "account": "api_key"})
	if err != nil || val != "pw-from-keychain" {
		t.Fatalf("val=%q err=%v", val, err)
	}
	if b, _ := os.ReadFile(argsFile); strings.TrimSpace(string(b)) != "find-generic-password -s openrouter -a api_key -w" {
		t.Errorf("args = %q", b)
	}
	if _, err := store.Lookup(ctx, map[string]string{"service": "s"}); err != nil {
		t.Errorf("account should be optional: %v", err)
	}
	if b, _ := os.ReadFile(argsFile); strings.Contains(string(b), "-a") {
		t.Errorf("optional account leaked: %q", b)
	}
	if _, err := store.Lookup(ctx, map[string]string{"service": "missing"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("failure err = %v", err)
	}
	if _, err := store.Lookup(ctx, map[string]string{"account": "x"}); err == nil {
		t.Error("missing service accepted")
	}
	if _, err := store.Lookup(ctx, map[string]string{"service": "s", "provider": "x"}); err == nil {
		t.Error("unknown attribute accepted")
	}
}

func TestWindowsStoreStub(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "powershell",
		"#!/bin/sh\nif [ \"$AGENTCHAT_CRED_TARGET\" = \"missing\" ]; then echo not found >&2; exit 1; fi\nprintf pw-%s \"$AGENTCHAT_CRED_TARGET\"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := windowsStore{tool: "powershell"}
	ctx := context.Background()

	val, err := store.Lookup(ctx, map[string]string{"target": "openrouter"})
	if err != nil || val != "pw-openrouter" {
		t.Fatalf("val=%q err=%v", val, err)
	}
	if _, err := store.Lookup(ctx, map[string]string{"target": "missing"}); err == nil {
		t.Error("failure not surfaced")
	}
	if _, err := store.Lookup(ctx, map[string]string{"target": "x", "extra": "y"}); err == nil {
		t.Error("extra attribute accepted")
	}
	if _, err := store.Lookup(ctx, nil); err == nil {
		t.Error("empty attrs accepted")
	}
}
