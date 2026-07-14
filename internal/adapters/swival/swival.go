// Package swival adapts Swival (swival.dev) to the AgentChat engine. It
// runs the client in one-shot mode with the task piped to stdin:
//
//	swival [--profile P] [--provider P] [--base-url U] [--model M]
//	       [--reasoning-effort E] [--max-turns N] --report <tmpfile>
//
// Swival's output contract is unusually clean: stdout is exclusively the
// final answer, diagnostics (turn headers, tool traces, timing) go to
// stderr, and --report writes a structured JSON run report. The adapter
// streams stderr lines as thinking events for live progress, emits the
// stdout answer as the final text event, and mines the report for usage.
//
// Swival does not auto-commit and has no resumable session IDs — its
// continuity lives in the .swival/ state directory inside the workspace,
// which persists across turns under this app's workspace model. File
// changes are detected best-effort by diffing `git status --porcelain`
// before and after the run (the Step 7 workspace snapshots are the
// authoritative record).
//
// Exit codes (documented): 0 success, 1 runtime/config failure, 2 turn
// limit reached before finishing, 130/143 signals.
package swival

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// Adapter implements adapter.Adapter for Swival.
type Adapter struct {
	// Binary is the executable name or path; defaults to "swival".
	Binary string
}

// New returns a Swival adapter using the `swival` binary from PATH.
func New() *Adapter { return &Adapter{Binary: "swival"} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "swival" }

// Available implements adapter.Adapter.
func (a *Adapter) Available(ctx context.Context) error {
	if _, err := exec.LookPath(a.Binary); err != nil {
		return fmt.Errorf("swival binary %q not found on PATH: %w", a.Binary, err)
	}
	return nil
}

// Models implements adapter.Adapter. Swival auto-discovers models from
// local servers (LM Studio, llama.cpp) and accepts provider-specific IDs
// otherwise; model strings pass through. Provider/base-url/profile are
// selected via TurnRequest.Extra (see buildArgs).
func (a *Adapter) Models(ctx context.Context) ([]adapter.Model, error) {
	return []adapter.Model{
		{ID: "", Label: "Default (auto-discovered / profile)"},
	}, nil
}

// Efforts implements adapter.EffortLister. Levels per swival 1.0.25
// --help (--reasoning-effort LEVEL).
func (a *Adapter) Efforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "default"}
}

// buildArgs constructs the CLI arguments for a turn; the task itself is
// piped to stdin (documented behavior when no positional task is given and
// stdin is not a TTY). Kept separate from RunTurn for unit testing.
//
// Recognized Extra keys: profile, provider, base_url, reasoning_effort
// (back-compat override of the first-class req.Effort field), max_turns,
// diagnostics ("false" adds --quiet and disables the stderr thinking
// stream). SessionID is ignored — swival has no session resume;
// continuity comes from .swival/ state in the workspace.
func buildArgs(req adapter.TurnRequest, reportPath string) []string {
	var args []string
	if req.Extra["diagnostics"] == "false" {
		args = append(args, "--quiet")
	}
	if v := req.Extra["profile"]; v != "" {
		args = append(args, "--profile", v)
	}
	if v := req.Extra["provider"]; v != "" {
		args = append(args, "--provider", v)
	}
	if v := req.Extra["base_url"]; v != "" {
		args = append(args, "--base-url", v)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	effort := req.Effort
	if v := req.Extra["reasoning_effort"]; v != "" {
		effort = v // Extra wins for back-compat with pre-Step-13 configs
	}
	if effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if v := req.Extra["max_turns"]; v != "" {
		args = append(args, "--max-turns", v)
	}
	if req.SystemPrompt != "" {
		// "System prompt to include" (swival 1.0.25 --help) — included
		// alongside swival's own instructions, not replacing them.
		args = append(args, "--system-prompt", req.SystemPrompt)
	}
	return append(args, "--report", reportPath)
}

// RunTurn implements adapter.Adapter.
func (a *Adapter) RunTurn(ctx context.Context, req adapter.TurnRequest, emit adapter.EmitFunc) (*adapter.Result, error) {
	start := time.Now()

	reportFile, err := os.CreateTemp("", "swival-report-*.json")
	if err != nil {
		return nil, err
	}
	reportPath := reportFile.Name()
	reportFile.Close()
	defer os.Remove(reportPath)

	before := porcelain(ctx, req.WorkDir)

	cmd := exec.CommandContext(ctx, a.Binary, buildArgs(req, reportPath)...)
	cmd.Dir = req.WorkDir
	cmd.Env = append(append(os.Environ(), req.Env...), req.MCPEnv()...)
	cmd.Stdin = strings.NewReader(req.Prompt)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("swival: starting %q: %w", a.Binary, err)
	}

	// Diagnostics stream: each stderr line becomes a thinking event so the
	// chat shows live progress (tool traces, turn headers). Kept even when
	// the run later fails; also retained for error reporting.
	var stderrTail []string
	streamDiag := req.Extra["diagnostics"] != "false"
	sc := bufio.NewScanner(stderrPipe)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		stderrTail = append(stderrTail, line)
		if len(stderrTail) > 50 {
			stderrTail = stderrTail[1:]
		}
		if streamDiag {
			emit(adapter.Event{Kind: adapter.EventThinking, Time: time.Now(), Text: line})
		}
	}

	waitErr := cmd.Wait()
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if waitErr != nil {
		exitCode = -1
	}

	res := &adapter.Result{ExitCode: exitCode}

	// stdout is exclusively the final answer.
	if answer := strings.TrimSpace(stdout.String()); answer != "" {
		res.FinalText = answer
		emit(adapter.Event{Kind: adapter.EventText, Time: time.Now(), Text: answer})
	}

	// Mine the JSON report for usage and (as a fallback) the answer.
	if b, err := os.ReadFile(reportPath); err == nil && len(b) > 0 {
		applyReport(b, res)
	}

	// Best-effort file changes: git worktree status before vs. after.
	after := porcelain(ctx, req.WorkDir)
	res.FilesChanged = porcelainDiff(before, after)
	for i := range res.FilesChanged {
		emit(adapter.Event{Kind: adapter.EventFileChange, Time: time.Now(), File: &res.FilesChanged[i]})
	}

	res.Duration = time.Since(start)

	// Exactly one terminal event, after the process has fully exited.
	emit(adapter.Event{Kind: adapter.EventResult, Time: time.Now(), Result: res})

	switch {
	case exitCode == 2:
		return res, fmt.Errorf("swival: turn limit reached before finishing (exit 2); answer may be partial")
	case waitErr != nil:
		return res, fmt.Errorf("swival: exited with code %d: %w (stderr tail: %s)",
			exitCode, waitErr, truncate(strings.Join(stderrTail, " | "), 2000))
	default:
		return res, nil
	}
}

// applyReport fills res from swival's --report JSON. Parsing is defensive:
// the report schema (result.answer, stats counters, timeline events) is
// documented but stats key names for token totals are not pinned, so
// several common names are tried and the timeline's llm_call events serve
// as a fallback for prompt-side tokens.
func applyReport(b []byte, res *adapter.Result) {
	var rep struct {
		Result struct {
			Answer string `json:"answer"`
		} `json:"result"`
		Stats    map[string]json.Number `json:"stats"`
		Timeline []struct {
			Type            string `json:"type"`
			PromptTokensEst int64  `json:"prompt_tokens_est"`
			CachedTokens    int64  `json:"cached_tokens"`
		} `json:"timeline"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return
	}
	if res.FinalText == "" && rep.Result.Answer != "" {
		res.FinalText = rep.Result.Answer
	}

	statInt := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := rep.Stats[k]; ok {
				if n, err := v.Int64(); err == nil && n > 0 {
					return n
				}
			}
		}
		return 0
	}
	res.Usage.InputTokens = statInt("prompt_tokens", "input_tokens", "total_prompt_tokens")
	res.Usage.OutputTokens = statInt("completion_tokens", "output_tokens", "total_completion_tokens")

	if res.Usage.InputTokens == 0 {
		var est int64
		for _, ev := range rep.Timeline {
			if ev.Type == "llm_call" {
				est += ev.PromptTokensEst + ev.CachedTokens
			}
		}
		res.Usage.InputTokens = est
	}
}

// porcelain returns `git status --porcelain` parsed into path -> status,
// or nil when dir is not a git repo.
func porcelain(ctx context.Context, dir string) map[string]string {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil
	}
	m := make(map[string]string)
	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) < 4 {
			continue
		}
		status, path := line[:2], strings.TrimSpace(line[3:])
		// Renames are listed as "old -> new"; keep the new path.
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		m[path] = status
	}
	return m
}

// porcelainDiff derives file changes from two worktree-status snapshots:
// paths whose status appeared or changed between before and after.
func porcelainDiff(before, after map[string]string) []adapter.FileChange {
	if after == nil {
		return nil
	}
	var out []adapter.FileChange
	for path, status := range after {
		if before[path] == status {
			continue // unchanged (including pre-existing dirt)
		}
		fc := adapter.FileChange{Path: path, Op: adapter.FileModified}
		switch {
		case strings.Contains(status, "?") || strings.Contains(status, "A"):
			fc.Op = adapter.FileCreated
		case strings.Contains(status, "D"):
			fc.Op = adapter.FileDeleted
		case strings.Contains(status, "R"):
			fc.Op = adapter.FileRenamed
		}
		out = append(out, fc)
	}
	// Untracked files that vanished were deleted by the run.
	for path, status := range before {
		if _, still := after[path]; !still && strings.Contains(status, "?") {
			out = append(out, adapter.FileChange{Path: path, Op: adapter.FileDeleted})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
