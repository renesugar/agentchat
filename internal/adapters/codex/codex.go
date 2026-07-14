// Package codex adapts the OpenAI Codex CLI to the AgentChat engine. It
// runs the client non-interactively:
//
//	codex exec --json --sandbox workspace-write --skip-git-repo-check \
//	    [--model <id>] [resume <thread_id>] -
//
// with the prompt supplied on stdin ("-" placeholder), and translates the
// emitted JSONL thread/turn/item events (see stream.go) into normalized
// adapter events. Unit tests never execute the real binary; parsing is
// covered by fixtures under testdata/. See docs/adapters.md for the flag
// notes and version caveats (notably around `resume`).
package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// DefaultSandbox is the sandbox mode used unless overridden via
// Extra["sandbox"] ("read-only", "workspace-write", "danger-full-access").
// workspace-write lets the agent edit the workspace but nothing else.
const DefaultSandbox = "workspace-write"

// Adapter implements adapter.Adapter for the Codex CLI.
type Adapter struct {
	// Binary is the executable name or path; defaults to "codex".
	Binary string
}

// New returns a Codex adapter using the `codex` binary from PATH.
func New() *Adapter { return &Adapter{Binary: "codex"} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "codex" }

// Available implements adapter.Adapter.
func (a *Adapter) Available(ctx context.Context) error {
	if _, err := exec.LookPath(a.Binary); err != nil {
		return fmt.Errorf("codex binary %q not found on PATH: %w", a.Binary, err)
	}
	return nil
}

// Models implements adapter.Adapter. Any model ID configured for the
// user's Codex install passes through; these are common defaults.
func (a *Adapter) Models(ctx context.Context) ([]adapter.Model, error) {
	// Current codex model lineup (verified against a live codex-cli
	// 0.142.5 environment, 2026-07-10). Differently configured installs
	// can replace this list via config.json (clients.codex.models +
	// replace_models); any other ID passes through as-is.
	return []adapter.Model{
		{ID: "", Label: "Default (client-configured)"},
		{ID: "gpt-5.6-sol", Label: "GPT-5.6 Sol (most capable)"},
		{ID: "gpt-5.6-terra", Label: "GPT-5.6 Terra (balanced)"},
		{ID: "gpt-5.6-luna", Label: "GPT-5.6 Luna (fast, affordable)"},
		{ID: "gpt-5.5", Label: "GPT-5.5 (complex coding & research)"},
		{ID: "gpt-5.4", Label: "GPT-5.4 (everyday coding)"},
		{ID: "gpt-5.4-mini", Label: "GPT-5.4 Mini (fast, cost-efficient)"},
	}, nil
}

// Efforts implements adapter.EffortLister. Documented
// model_reasoning_effort levels; codex validates the value at the model,
// not at config parse (verified on 0.142.5).
func (a *Adapter) Efforts() []string {
	return []string{"minimal", "low", "medium", "high", "xhigh"}
}

// buildArgs constructs the CLI arguments for a turn. The trailing "-" makes
// codex read the prompt from stdin, avoiding quoting/dash-prefix pitfalls.
// Kept separate from RunTurn for unit testing.
func buildArgs(req adapter.TurnRequest) []string {
	args := []string{"exec", "--json"}

	sandbox := DefaultSandbox
	if s, ok := req.Extra["sandbox"]; ok {
		sandbox = s
	}
	if sandbox != "" {
		args = append(args, "--sandbox", sandbox)
	}
	// The engine controls the workspace (scratch dirs may not be git repos
	// until Step 7 git-inits them), so skip codex's repo guard by default.
	if req.Extra["skip_git_repo_check"] != "false" {
		args = append(args, "--skip-git-repo-check")
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		// Config key verified on codex-cli 0.142.5 (--strict-config
		// accepts it; unknown keys error). Values are validated by the
		// client/model, not at config parse time.
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.Effort))
	}
	if req.SystemPrompt != "" {
		// Extra developer/system instructions. Verified LIVE on
		// codex-cli 0.142.5: with -c developer_instructions the model
		// followed an injected marker instruction end to end
		// (`instructions` is also accepted by --strict-config but was
		// not behavior-verified). tomlQuote because the text is
		// multi-line and %q emits Go-only escapes TOML rejects.
		args = append(args, "-c", "developer_instructions="+tomlQuote(req.SystemPrompt))
	}
	if req.MCP != nil {
		// Streamable-HTTP MCP server via config overrides (verified on
		// codex-cli 0.142.5: `codex mcp add --url/--bearer-token-env-var`
		// writes exactly these keys). The token itself goes through the
		// environment (adapter.MCPEnv), never the command line. Placed
		// before a possible `resume` subcommand — exec-level flags there
		// parse fine and `resume` re-defines -c as well.
		args = append(args,
			"-c", fmt.Sprintf("mcp_servers.%s.url=%q", req.MCP.Name, req.MCP.URL),
			"-c", fmt.Sprintf("mcp_servers.%s.bearer_token_env_var=%q", req.MCP.Name, adapter.MCPTokenEnv),
		)
	}
	if req.SessionID != "" {
		args = append(args, "resume", req.SessionID)
	}
	return append(args, "-")
}

// tomlQuote renders s as a TOML basic string. Go's %q is close but not
// safe: it emits \xNN escapes for some control bytes, which TOML rejects
// (TOML basic strings allow only \b \t \n \f \r \" \\ and \uXXXX).
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// RunTurn implements adapter.Adapter.
func (a *Adapter) RunTurn(ctx context.Context, req adapter.TurnRequest, emit adapter.EmitFunc) (*adapter.Result, error) {
	start := time.Now()

	cmd := exec.CommandContext(ctx, a.Binary, buildArgs(req)...)
	cmd.Dir = req.WorkDir
	cmd.Env = append(append(os.Environ(), req.Env...), req.MCPEnv()...)
	cmd.Stdin = strings.NewReader(req.Prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex: starting %q: %w", a.Binary, err)
	}

	parsed, parseErr := parseStream(stdout, req.WorkDir, emit)

	waitErr := cmd.Wait()
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if waitErr != nil {
		exitCode = -1
	}

	res := parsed.result()
	res.ExitCode = exitCode
	res.Duration = time.Since(start)

	// Exactly one terminal event, after the process has fully exited.
	emit(adapter.Event{Kind: adapter.EventResult, Time: time.Now(), Result: res})

	switch {
	case parseErr != nil:
		return res, fmt.Errorf("codex: reading output: %w", parseErr)
	case waitErr != nil:
		return res, fmt.Errorf("codex: exited with code %d: %w (stderr: %s)",
			exitCode, waitErr, truncate(stderr.String(), 2000))
	case parsed.failed:
		return res, fmt.Errorf("codex: turn failed: %s", truncate(parsed.failMsg, 2000))
	default:
		return res, nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
