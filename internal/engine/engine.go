// Package engine composes the adapter registry, the transcript store, and
// (optionally) a managed workspace: it runs one turn with a chosen coding
// client, persists the prompt, event stream, and result as they happen,
// and snapshots the workspace after the turn so every turn has a pinned
// tree. The Wails GUI (Step 10) and the CLI harness are both thin callers
// of this package.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/mcpserver"
	"github.com/example/agentchat/internal/transcript"
	"github.com/example/agentchat/internal/workspace"
)

// Engine runs turns.
type Engine struct {
	Registry *adapter.Registry
	Store    transcript.Store

	// MCP, when non-nil, is the app's callback server (Step 12): each
	// turn gets a token-scoped channel and MCP-capable clients can push
	// progress/artifacts straight into the turn's event stream. Optional
	// by design — output capture stays the baseline transport.
	MCP *mcpserver.Server
	// ArtifactSink stores a file pushed through the MCP add_artifact
	// tool and returns the artifact ID. nil (or a nil MCP) makes the
	// tool report that artifact storage is unavailable. path is already
	// resolved and confined to the turn's workspace.
	ArtifactSink func(ctx context.Context, convID, turnID, path, note string) (string, error)
}

// New returns an Engine over reg and store.
func New(reg *adapter.Registry, store transcript.Store) *Engine {
	return &Engine{Registry: reg, Store: store}
}

// RunTurn executes one turn of conversation convID with the named client.
//
// If ws is non-nil the turn runs inside that managed workspace:
// req.WorkDir is set to the workspace dir, the workspace is snapshotted
// after the client finishes (regardless of success), the snapshot commit
// is recorded on the turn, and — when the adapter itself couldn't report
// file changes — the diff against the previous snapshot fills
// Result.FilesChanged authoritatively.
//
// Events are persisted to the store as they stream; if tap is non-nil it
// also receives each event (for live UI updates). The returned Turn is the
// finished record (status done or failed).
//
// A client run error is recorded on the turn and returned; storage errors
// take precedence, since losing the transcript is worse than losing one
// turn's outcome.
func (e *Engine) RunTurn(ctx context.Context, convID, client string, ws *workspace.Workspace, req adapter.TurnRequest, tap adapter.EmitFunc) (*transcript.Turn, error) {
	a, err := e.Registry.Get(client)
	if err != nil {
		return nil, err
	}
	if err := a.Available(ctx); err != nil {
		return nil, fmt.Errorf("client %q not usable: %w", client, err)
	}

	var prevSnap string
	if ws != nil {
		req.WorkDir = ws.Dir
		prevSnap = ws.LatestSnapshot(ctx)
	}

	turn, err := e.Store.BeginTurn(ctx, convID, transcript.NewTurn{
		Client:       client,
		Model:        req.Model,
		WorkspaceRef: req.WorkDir,
		Prompt:       req.Prompt,
	})
	if err != nil {
		return nil, err
	}

	// Persist every event; remember the first storage failure without
	// aborting the client run (the adapter cannot be interrupted safely
	// through emit, and partial persistence still has value). The mutex
	// serializes the adapter's emits with MCP pushes, which arrive on
	// HTTP handler goroutines.
	var (
		emitMu   sync.Mutex
		storeErr error
	)
	emit := func(ev adapter.Event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		if err := e.Store.AppendEvent(ctx, convID, turn.ID, ev); err != nil && storeErr == nil {
			storeErr = err
		}
		if tap != nil {
			tap(ev)
		}
	}

	// Open the turn's MCP callback channel; the token dies with the turn.
	if e.MCP != nil && req.MCP == nil {
		workDir := req.WorkDir
		ch := e.MCP.Register(mcpserver.Sink{
			Emit: emit,
			AddArtifact: func(path, note string) (string, error) {
				if e.ArtifactSink == nil {
					return "", errors.New("artifact storage not configured")
				}
				resolved, err := resolveInWorkspace(workDir, path)
				if err != nil {
					return "", err
				}
				return e.ArtifactSink(ctx, convID, turn.ID, resolved, note)
			},
		})
		defer ch.Close()
		req.MCP = &adapter.MCPServerInfo{Name: "agentchat", URL: e.MCP.URL(), Token: ch.Token}
	}

	res, runErr := a.RunTurn(ctx, req, emit)

	// Snapshot the workspace state this turn produced — also on failure,
	// so a half-finished mess is still pinned and diffable/restorable.
	var snapID string
	if ws != nil {
		label := fmt.Sprintf("agentchat turn %d (%s)", turn.Seq, client)
		if snap, err := ws.Snapshot(ctx, label); err != nil {
			emit(adapter.Event{Kind: adapter.EventError, Time: time.Now(),
				Text: fmt.Sprintf("workspace snapshot failed: %v", err)})
		} else {
			snapID = snap.Commit
			if res != nil && len(res.FilesChanged) == 0 && snap.Changed && prevSnap != "" {
				if changes, err := ws.Diff(ctx, prevSnap, snap.Commit); err == nil {
					res.FilesChanged = changes
				}
			}
		}
	}

	finished, finErr := e.Store.FinishTurn(ctx, convID, turn.ID, res, snapID, runErr)
	switch {
	case finErr != nil:
		return nil, fmt.Errorf("finishing turn %s: %w", turn.ID, finErr)
	case storeErr != nil:
		return finished, fmt.Errorf("persisting events for turn %s: %w", turn.ID, storeErr)
	default:
		return finished, runErr
	}
}

// resolveInWorkspace resolves an MCP-supplied artifact path against the
// turn's workspace and refuses paths that escape it — the client already
// has free rein inside the workspace, but the callback channel must not
// become a way to read arbitrary files from the host.
func resolveInWorkspace(workDir, path string) (string, error) {
	if workDir == "" {
		return "", errors.New("no workspace for this turn")
	}
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(workDir, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(workDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace", path)
	}
	return p, nil
}
