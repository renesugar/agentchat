// Package aider adapts Aider to the AgentChat engine. It runs the client
// non-interactively:
//
//	aider --message <prompt> --yes-always --no-stream --no-pretty \
//	    [--model <id>] [--restore-chat-history]
//
// Aider's output is line-oriented text, not JSON, so parsing (output.go) is
// heuristic: prose becomes text events, "Applied edit to <path>" and
// "Commit <hash> <msg>" lines become structured events, and the trailing
// "Tokens: ... Cost: ..." line feeds usage. Authoritative file changes are
// derived from git (HEAD before vs. after — aider auto-commits by default),
// falling back to the Applied-edit lines when the workspace isn't a repo or
// nothing was committed.
//
// Aider has no session IDs; conversational continuity comes from its own
// history files in the workspace (.aider.chat.history.md), reloaded with
// --restore-chat-history. See docs/adapters.md.
package aider

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

// Adapter implements adapter.Adapter for Aider.
type Adapter struct {
	// Binary is the executable name or path; defaults to "aider".
	Binary string
}

// New returns an Aider adapter using the `aider` binary from PATH.
func New() *Adapter { return &Adapter{Binary: "aider"} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "aider" }

// Available implements adapter.Adapter.
func (a *Adapter) Available(ctx context.Context) error {
	if _, err := exec.LookPath(a.Binary); err != nil {
		return fmt.Errorf("aider binary %q not found on PATH: %w", a.Binary, err)
	}
	return nil
}

// Models implements adapter.Adapter. Aider accepts litellm-style model
// strings and shorthand aliases; anything passes through.
func (a *Adapter) Models(ctx context.Context) ([]adapter.Model, error) {
	return []adapter.Model{
		{ID: "", Label: "Default (client-configured)"},
		{ID: "sonnet", Label: "Claude Sonnet (alias)"},
		{ID: "gpt-5", Label: "GPT-5"},
		{ID: "deepseek", Label: "DeepSeek (alias)"},
	}, nil
}

// buildArgs constructs the CLI arguments for a turn. Kept separate from
// RunTurn for unit testing.
//
// SessionID has no aider equivalent and is ignored; continuity comes from
// aider's history files in the workspace. Extra["restore_chat_history"]
// = "true" reloads them at the start of the turn.
func buildArgs(req adapter.TurnRequest) []string {
	args := []string{
		"--message", req.Prompt,
		"--yes-always", // never block on confirmation prompts
		"--no-stream",  // whole responses; simpler, cheaper output
		"--no-pretty",  // no colors/spinners in captured output
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Extra["restore_chat_history"] == "true" {
		args = append(args, "--restore-chat-history")
	}
	return args
}

// RunTurn implements adapter.Adapter.
func (a *Adapter) RunTurn(ctx context.Context, req adapter.TurnRequest, emit adapter.EmitFunc) (*adapter.Result, error) {
	start := time.Now()

	// Record the git state before the run; empty when not a repo.
	before := gitHead(ctx, req.WorkDir)

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
		return nil, fmt.Errorf("aider: starting %q: %w", a.Binary, err)
	}

	parsed, parseErr := parseOutput(stdout, emit)

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

	// Authoritative file changes: git diff of HEAD before vs. after (aider
	// auto-commits). Falls back to Applied-edit lines already in res.
	after := gitHead(ctx, req.WorkDir)
	if before != "" && after != "" && before != after {
		if changes, err := gitDiffNameStatus(ctx, req.WorkDir, before, after); err == nil && len(changes) > 0 {
			res.FilesChanged = changes
			for i := range changes {
				emit(adapter.Event{Kind: adapter.EventFileChange, Time: time.Now(), File: &changes[i]})
			}
		}
	}

	// Exactly one terminal event, after the process has fully exited.
	emit(adapter.Event{Kind: adapter.EventResult, Time: time.Now(), Result: res})

	switch {
	case parseErr != nil:
		return res, fmt.Errorf("aider: reading output: %w", parseErr)
	case waitErr != nil:
		return res, fmt.Errorf("aider: exited with code %d: %w (stderr: %s)",
			exitCode, waitErr, truncate(stderr.String(), 2000))
	default:
		return res, nil
	}
}

// gitHead returns the current HEAD commit of dir, or "" if dir is not a
// git repo (or git is unavailable / repo has no commits).
func gitHead(ctx context.Context, dir string) string {
	out, err := runGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// gitDiffNameStatus lists the files changed between two commits.
func gitDiffNameStatus(ctx context.Context, dir, before, after string) ([]adapter.FileChange, error) {
	out, err := runGit(ctx, dir, "diff", "--name-status", "-M", before, after)
	if err != nil {
		return nil, err
	}
	var changes []adapter.FileChange
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		fc := adapter.FileChange{Path: fields[1]}
		switch fields[0][0] {
		case 'A':
			fc.Op = adapter.FileCreated
		case 'D':
			fc.Op = adapter.FileDeleted
		case 'R':
			fc.Op = adapter.FileRenamed
			if len(fields) >= 3 {
				fc.OldPath = fields[1]
				fc.Path = fields[2]
			}
		default: // M, C, T, ...
			fc.Op = adapter.FileModified
		}
		changes = append(changes, fc)
	}
	return changes, nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
