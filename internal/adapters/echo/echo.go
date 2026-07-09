// Package echo provides a fake coding-client adapter. It needs no external
// binary: it "works on" the workspace by writing ECHO.md containing the
// prompt, and streams a plausible event sequence. It exists so the engine,
// transcript store, workspace snapshots, and UI can be built and tested
// before any real adapter (Steps 3-6) lands.
package echo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// Adapter implements adapter.Adapter.
type Adapter struct{}

// New returns the echo adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "echo" }

// Available implements adapter.Adapter; echo is always available.
func (a *Adapter) Available(ctx context.Context) error { return nil }

// Models implements adapter.Adapter.
func (a *Adapter) Models(ctx context.Context) ([]adapter.Model, error) {
	return []adapter.Model{{ID: "echo-1", Label: "Echo (fake)"}}, nil
}

// RunTurn implements adapter.Adapter.
func (a *Adapter) RunTurn(ctx context.Context, req adapter.TurnRequest, emit adapter.EmitFunc) (*adapter.Result, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	emit(adapter.Event{Kind: adapter.EventPlan, Time: time.Now(),
		Text: "1. Write the prompt to ECHO.md\n2. Report the change"})

	const name = "ECHO.md"
	path := filepath.Join(req.WorkDir, name)
	op := adapter.FileCreated
	if _, err := os.Stat(path); err == nil {
		op = adapter.FileModified
	}

	emit(adapter.Event{Kind: adapter.EventToolUse, Time: time.Now(),
		Tool: &adapter.ToolInfo{Name: "write_file", Input: name}})

	content := fmt.Sprintf("# Echo\n\nmodel: %s\n\n%s\n", req.Model, req.Prompt)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		emit(adapter.Event{Kind: adapter.EventError, Time: time.Now(), Text: err.Error()})
		return nil, fmt.Errorf("echo: write %s: %w", name, err)
	}

	change := adapter.FileChange{Path: name, Op: op}
	emit(adapter.Event{Kind: adapter.EventToolResult, Time: time.Now(),
		Tool: &adapter.ToolInfo{Name: "write_file", Output: "ok"}})
	emit(adapter.Event{Kind: adapter.EventFileChange, Time: time.Now(), File: &change})

	final := fmt.Sprintf("Echoed your prompt (%d bytes) into %s.", len(req.Prompt), name)
	emit(adapter.Event{Kind: adapter.EventText, Time: time.Now(), Text: final})

	res := &adapter.Result{
		SessionID:    "echo-session",
		ExitCode:     0,
		FinalText:    final,
		FilesChanged: []adapter.FileChange{change},
		Usage:        adapter.Usage{InputTokens: int64(len(req.Prompt)), OutputTokens: int64(len(final))},
		Duration:     time.Since(start),
	}
	emit(adapter.Event{Kind: adapter.EventResult, Time: time.Now(), Result: res})
	return res, nil
}
