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
	return []adapter.Model{
		{ID: "", Label: "Default (client-configured)"},
		{ID: "gpt-5-codex", Label: "GPT-5 Codex"},
		{ID: "gpt-5", Label: "GPT-5"},
	}, nil
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
	if req.SessionID != "" {
		args = append(args, "resume", req.SessionID)
	}
	return append(args, "-")
}

// RunTurn implements adapter.Adapter.
func (a *Adapter) RunTurn(ctx context.Context, req adapter.TurnRequest, emit adapter.EmitFunc) (*adapter.Result, error) {
	start := time.Now()

	cmd := exec.CommandContext(ctx, a.Binary, buildArgs(req)...)
	cmd.Dir = req.WorkDir
	cmd.Env = append(os.Environ(), req.Env...)
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
