// Package provider models the ways a coding client can reach models: a
// named Provider is either the client's own account ("subscription" /
// inherited environment — the default, which injects nothing) or an API
// endpoint (base URL + key). Definitions come from three sources:
//
//   - builtin: the per-client default entry (always first in a catalog);
//   - config: the user's config.json providers (internal/config);
//   - codex: READ-ONLY parsing of ~/.codex/config.toml model_providers
//     (codexconfig.go) — AgentChat never writes client config files.
//
// API keys are never stored in the clear and never travel in argv: a
// definition carries only the *lookup attributes* for the platform
// secret store (Linux: secret-tool), and ResolveEnv fetches the value at
// turn time, injecting it as the provider's env key.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/example/agentchat/internal/adapter"
)

// Def is one provider definition. The zero Name means the client default
// (subscription / inherited environment), mirroring how model ID "" means
// the client-default model.
type Def struct {
	// Name identifies the provider in pickers and per-turn requests.
	// "" = client default.
	Name string `json:"name"`
	// Label is the human-readable picker text; defaults to Name.
	Label string `json:"label,omitempty"`
	// Source records where the definition came from: "builtin",
	// "config", or "codex".
	Source string `json:"source"`
	// Subscription marks the client's own auth: ResolveEnv injects
	// nothing and adapters pass no provider flags.
	Subscription bool `json:"subscription,omitempty"`
	// BaseURL is the API endpoint, when the source declares one. It is
	// metadata for adapters (swival --base-url, claude ANTHROPIC_BASE_URL,
	// ...); ResolveEnv does not inject it — env var names differ per
	// client, and codex reads it from its own config.
	BaseURL string `json:"base_url,omitempty"`
	// EnvKey is the environment variable the CLIENT reads the API key
	// from (codex env_key, e.g. OPENROUTER_API_KEY).
	EnvKey string `json:"env_key,omitempty"`
	// KeySecret holds platform secret-store lookup attributes for the
	// key's value (e.g. {"provider":"openrouter","token_type":"api_key"}
	// for `secret-tool lookup provider openrouter token_type api_key`).
	// Never the value itself.
	KeySecret map[string]string `json:"key_secret,omitempty"`
	// Env is extra environment for this provider; values support ${VAR}
	// expansion against the process environment.
	Env map[string]string `json:"env,omitempty"`
	// Models offered by this provider, for the cascading pickers.
	Models []adapter.Model `json:"models,omitempty"`
}

// Default returns the builtin leading catalog entry for a client:
// claude and codex authenticate with their own subscriptions by default;
// the other clients inherit whatever the process environment provides.
func Default(client string) Def {
	label := "Default (inherited environment)"
	if client == "claude" || client == "codex" {
		label = "Subscription (default)"
	}
	return Def{Name: "", Label: label, Source: "builtin", Subscription: true}
}

// Catalog assembles a client's provider list: the builtin default first,
// then the client-relevant definitions. For codex, the usable providers
// are the ones DECLARED IN CODEX'S OWN CONFIG (codexDefs); same-named
// config.json entries overlay them (contributing key_secret, models,
// label, env) but config-only names are dropped — codex cannot use a
// provider its config does not declare. For every other client the
// config.json definitions are used as-is.
func Catalog(client string, configDefs []Def, codexDefs []Def) []Def {
	out := []Def{Default(client)}
	if client == "codex" {
		byName := make(map[string]Def, len(configDefs))
		for _, d := range configDefs {
			byName[d.Name] = d
		}
		for _, d := range codexDefs {
			if c, ok := byName[d.Name]; ok {
				d = merge(d, c)
			}
			out = append(out, d)
		}
		return out
	}
	return append(out, configDefs...)
}

// merge overlays the config definition c onto the codex-declared base.
func merge(base, c Def) Def {
	if c.Label != "" {
		base.Label = c.Label
	}
	if c.BaseURL != "" {
		base.BaseURL = c.BaseURL
	}
	if c.EnvKey != "" {
		base.EnvKey = c.EnvKey
	}
	if len(c.KeySecret) > 0 {
		base.KeySecret = c.KeySecret
	}
	if len(c.Env) > 0 {
		base.Env = c.Env
	}
	if len(c.Models) > 0 {
		base.Models = c.Models
	}
	return base
}

// SecretStore fetches secret values from the platform's secret storage.
type SecretStore interface {
	// Lookup resolves the secret identified by attrs, or errors when the
	// store or entry is unavailable (never returns an empty secret).
	Lookup(ctx context.Context, attrs map[string]string) (string, error)
}

// ResolveEnv builds the ready-to-inject environment for a turn using
// this provider: the Env map (sorted, ${VAR}-expanded) plus
// EnvKey=<secret> when KeySecret names a stored key. Subscription
// definitions resolve to nil. Secret failures are loud — a
// misconfigured key should fail the turn, not silently fall back.
func (d Def) ResolveEnv(ctx context.Context, store SecretStore) ([]string, error) {
	if d.Subscription {
		return nil, nil
	}
	keys := make([]string, 0, len(d.Env))
	for k := range d.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		env = append(env, k+"="+os.Expand(d.Env[k], os.Getenv))
	}
	if len(d.KeySecret) > 0 {
		if d.EnvKey == "" {
			return nil, fmt.Errorf("provider %q: key_secret is set but no env_key names the variable to inject", d.Name)
		}
		if store == nil {
			return nil, fmt.Errorf("provider %q: no secret store available", d.Name)
		}
		val, err := store.Lookup(ctx, d.KeySecret)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", d.Name, err)
		}
		env = append(env, d.EnvKey+"="+val)
	}
	return env, nil
}

// PlatformStore returns the secret store for the current OS: Linux
// secret-tool (libsecret/GNOME Keyring), macOS the `security` Keychain
// CLI, Windows Credential Manager via PowerShell. All are exec-based —
// the secret value exists only in the child's stdout pipe, never argv.
//
// ⚠ Step 32 status: the darwin and windows backends are stub-tested but
// NOT yet verified on real macOS/Windows machines (see PLAN.md).
func PlatformStore() SecretStore {
	switch runtime.GOOS {
	case "linux":
		return execStore{tool: "secret-tool"}
	case "darwin":
		return darwinStore{tool: "security"}
	case "windows":
		return windowsStore{tool: "powershell"}
	default:
		return unsupportedStore{goos: runtime.GOOS}
	}
}

// darwinStore reads macOS Keychain generic passwords via the `security`
// CLI. api_key_secret uses the reserved attribute names "service"
// (required) and "account" (optional):
//
//	security find-generic-password -s <service> [-a <account>] -w
type darwinStore struct{ tool string }

func (s darwinStore) Lookup(ctx context.Context, attrs map[string]string) (string, error) {
	service := attrs["service"]
	if service == "" {
		return "", errors.New(`macOS keychain lookup needs {"service": "<name>"} (plus optional "account") in api_key_secret`)
	}
	for k := range attrs {
		if k != "service" && k != "account" {
			return "", fmt.Errorf("macOS keychain lookup: unknown attribute %q (only service/account map to `security find-generic-password`)", k)
		}
	}
	if _, err := exec.LookPath(s.tool); err != nil {
		return "", fmt.Errorf("%s not found on PATH: %w", s.tool, err)
	}
	args := []string{"find-generic-password", "-s", service}
	if a := attrs["account"]; a != "" {
		args = append(args, "-a", a)
	}
	args = append(args, "-w") // print only the password, to stdout
	cmd := exec.CommandContext(ctx, s.tool, args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s find-generic-password -s %s failed: %w (%s)",
			s.tool, service, err, strings.TrimSpace(errBuf.String()))
	}
	secret := strings.TrimRight(out.String(), "\r\n")
	if secret == "" {
		return "", fmt.Errorf("keychain item service=%s returned no secret", service)
	}
	return secret, nil
}

// windowsStore reads Windows Credential Manager generic credentials via
// PowerShell P/Invoke (CredReadW) — no Go syscall dependency, and the
// target name travels through the environment, not the command line.
// api_key_secret uses exactly {"target": "<credential name>"}.
type windowsStore struct{ tool string }

// winCredScript reads $env:AGENTCHAT_CRED_TARGET and writes the
// credential blob to stdout.
const winCredScript = `$sig = @"
using System;
using System.Runtime.InteropServices;
public class AgentChatCred {
  [DllImport("advapi32", CharSet=CharSet.Unicode, SetLastError=true)]
  public static extern bool CredReadW(string target, int type, int flags, out IntPtr pcred);
  [DllImport("advapi32")]
  public static extern void CredFree(IntPtr cred);
  [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)]
  public struct CREDENTIAL {
    public int Flags; public int Type; public string TargetName; public string Comment;
    public System.Runtime.InteropServices.ComTypes.FILETIME LastWritten;
    public int CredentialBlobSize; public IntPtr CredentialBlob; public int Persist;
    public int AttributeCount; public IntPtr Attributes; public string TargetAlias; public string UserName;
  }
  public static string Get(string target) {
    IntPtr p;
    if (!CredReadW(target, 1, 0, out p)) { throw new Exception("credential not found: " + target); }
    var c = (CREDENTIAL)Marshal.PtrToStructure(p, typeof(CREDENTIAL));
    var s = Marshal.PtrToStringUni(c.CredentialBlob, c.CredentialBlobSize / 2);
    CredFree(p);
    return s;
  }
}
"@
Add-Type -TypeDefinition $sig
[Console]::Out.Write([AgentChatCred]::Get($env:AGENTCHAT_CRED_TARGET))`

func (s windowsStore) Lookup(ctx context.Context, attrs map[string]string) (string, error) {
	target := attrs["target"]
	if target == "" || len(attrs) != 1 {
		return "", errors.New(`windows credential lookup takes exactly {"target": "<credential name>"} in api_key_secret`)
	}
	if _, err := exec.LookPath(s.tool); err != nil {
		return "", fmt.Errorf("%s not found on PATH: %w", s.tool, err)
	}
	cmd := exec.CommandContext(ctx, s.tool, "-NoProfile", "-NonInteractive", "-Command", winCredScript)
	cmd.Env = append(os.Environ(), "AGENTCHAT_CRED_TARGET="+target)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("credential manager lookup for %q failed: %w (%s)",
			target, err, strings.TrimSpace(errBuf.String()))
	}
	secret := strings.TrimRight(out.String(), "\r\n")
	if secret == "" {
		return "", fmt.Errorf("credential %q returned no secret", target)
	}
	return secret, nil
}

// execStore shells out to secret-tool. The secret value exists only in
// the tool's stdout pipe and the returned string — never in argv (only
// the lookup attributes are arguments) and never on disk.
type execStore struct {
	tool string
}

func (s execStore) Lookup(ctx context.Context, attrs map[string]string) (string, error) {
	if len(attrs) == 0 {
		return "", errors.New("secret lookup needs at least one attribute")
	}
	if _, err := exec.LookPath(s.tool); err != nil {
		return "", fmt.Errorf("%s not found on PATH (install libsecret-tools or store the key differently): %w", s.tool, err)
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := []string{"lookup"}
	for _, k := range keys {
		args = append(args, k, attrs[k])
	}
	cmd := exec.CommandContext(ctx, s.tool, args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s lookup %s failed: %w (%s)",
			s.tool, describeAttrs(keys, attrs), err, strings.TrimSpace(errBuf.String()))
	}
	secret := strings.TrimRight(out.String(), "\r\n")
	if secret == "" {
		return "", fmt.Errorf("%s lookup %s returned no secret", s.tool, describeAttrs(keys, attrs))
	}
	return secret, nil
}

func describeAttrs(keys []string, attrs map[string]string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + attrs[k]
	}
	return strings.Join(parts, " ")
}

type unsupportedStore struct{ goos string }

func (s unsupportedStore) Lookup(context.Context, map[string]string) (string, error) {
	return "", fmt.Errorf("platform secret store not supported on %s yet (Linux secret-tool only for now)", s.goos)
}
