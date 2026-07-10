// Package claudecode adapts Claude Code (the `claude` CLI) to the AgentChat
// engine. It runs the client non-interactively:
//
//	claude -p --output-format stream-json --verbose \
//	    [--model <id>] [--resume <session_id>] \
//	    [--permission-mode <mode>] -- <prompt>
//
// and translates the emitted JSON lines (see stream.go) into normalized
// adapter events. Unit tests never execute the real binary; parsing is
// covered by fixtures under testdata/. Flag names were taken from Claude
// Code documentation and must be re-verified against the installed version
// (see docs/adapters.md).
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// DefaultPermissionMode is used when the request doesn't override it via
// Extra["permission_mode"]. "acceptEdits" lets non-interactive runs edit
// files without prompting while still blocking more dangerous actions.
const DefaultPermissionMode = "acceptEdits"

// Adapter implements adapter.Adapter for Claude Code.
type Adapter struct {
	// Binary is the executable name or path; defaults to "claude".
	Binary string
}

// New returns a Claude Code adapter using the `claude` binary from PATH.
func New() *Adapter { return &Adapter{Binary: "claude"} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "claude" }

// Available implements adapter.Adapter.
func (a *Adapter) Available(ctx context.Context) error {
	if _, err := exec.LookPath(a.Binary); err != nil {
		return fmt.Errorf("claude binary %q not found on PATH: %w", a.Binary, err)
	}
	return nil
}

// Models implements adapter.Adapter. Claude Code accepts aliases as well as
// full model IDs; the aliases are stable across releases so they are what
// we offer by default. Any other ID can still be passed through.
func (a *Adapter) Models(ctx context.Context) ([]adapter.Model, error) {
	return []adapter.Model{
		{ID: "", Label: "Default (client-configured)"},
		{ID: "sonnet", Label: "Claude Sonnet (alias)"},
		{ID: "opus", Label: "Claude Opus (alias)"},
		{ID: "haiku", Label: "Claude Haiku (alias)"},
	}, nil
}

// buildArgs constructs the CLI arguments for a turn. Kept separate from
// RunTurn so it can be unit-tested without spawning anything.
func buildArgs(req adapter.TurnRequest) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Effort != "" {
		// Launch flag is the reliable path: the /effort command is "Not
		// applied" on some models in non-interactive runs. Levels on
		// claude 2.1.206: low, medium, high, xhigh, max.
		args = append(args, "--effort", req.Effort)
	}
	mode := DefaultPermissionMode
	if m, ok := req.Extra["permission_mode"]; ok {
		mode = m
	}
	if mode != "" {
		args = append(args, "--permission-mode", mode)
	}
	if req.MCP != nil {
		// Inline JSON config for the app's callback server (verified on
		// claude 2.1.206: --mcp-config accepts JSON strings). Allowing
		// the bare server name (mcp__<name>) pre-approves its tools,
		// which -p mode could never prompt for.
		cfg, _ := json.Marshal(map[string]any{
			"mcpServers": map[string]any{
				req.MCP.Name: map[string]any{
					"type": "http",
					"url":  req.MCP.URL,
					"headers": map[string]string{
						"Authorization": "Bearer " + req.MCP.Token,
					},
				},
			},
		})
		args = append(args, "--mcp-config", string(cfg),
			"--allowedTools", "mcp__"+req.MCP.Name)
	}
	// "--" guards against prompts that begin with a dash.
	return append(args, "--", req.Prompt)
}

// RunTurn implements adapter.Adapter.
func (a *Adapter) RunTurn(ctx context.Context, req adapter.TurnRequest, emit adapter.EmitFunc) (*adapter.Result, error) {
	start := time.Now()

	cmd := exec.CommandContext(ctx, a.Binary, buildArgs(req)...)
	cmd.Dir = req.WorkDir
	cmd.Env = append(os.Environ(), req.Env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude: starting %q: %w", a.Binary, err)
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
		return res, fmt.Errorf("claude: reading output: %w", parseErr)
	case waitErr != nil:
		return res, fmt.Errorf("claude: exited with code %d: %w (stderr: %s)",
			exitCode, waitErr, truncate(stderr.String(), 2000))
	case parsed.isError:
		return res, fmt.Errorf("claude: reported error: %s", truncate(res.FinalText, 2000))
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
