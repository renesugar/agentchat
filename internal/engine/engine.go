// Package engine composes the adapter registry and the transcript store:
// it runs one turn with a chosen coding client and persists the prompt,
// event stream, and result as they happen. The Wails GUI (Step 10) and the
// CLI harness are both thin callers of this package. Workspace snapshotting
// hooks in here once Step 7 lands.
package engine

import (
	"context"
	"fmt"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/transcript"
)

// Engine runs turns.
type Engine struct {
	Registry *adapter.Registry
	Store    transcript.Store
}

// New returns an Engine over reg and store.
func New(reg *adapter.Registry, store transcript.Store) *Engine {
	return &Engine{Registry: reg, Store: store}
}

// RunTurn executes one turn of conversation convID with the named client.
// Events are persisted to the store as they stream; if tap is non-nil it
// also receives each event (for live UI updates). The returned Turn is the
// finished record (status done or failed).
//
// A client run error is recorded on the turn and returned; storage errors
// take precedence, since losing the transcript is worse than losing one
// turn's outcome.
func (e *Engine) RunTurn(ctx context.Context, convID, client string, req adapter.TurnRequest, tap adapter.EmitFunc) (*transcript.Turn, error) {
	a, err := e.Registry.Get(client)
	if err != nil {
		return nil, err
	}
	if err := a.Available(ctx); err != nil {
		return nil, fmt.Errorf("client %q not usable: %w", client, err)
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
	// through emit, and partial persistence still has value).
	var storeErr error
	emit := func(ev adapter.Event) {
		if err := e.Store.AppendEvent(ctx, convID, turn.ID, ev); err != nil && storeErr == nil {
			storeErr = err
		}
		if tap != nil {
			tap(ev)
		}
	}

	res, runErr := a.RunTurn(ctx, req, emit)

	finished, finErr := e.Store.FinishTurn(ctx, convID, turn.ID, res, runErr)
	switch {
	case finErr != nil:
		return nil, fmt.Errorf("finishing turn %s: %w", turn.ID, finErr)
	case storeErr != nil:
		return finished, fmt.Errorf("persisting events for turn %s: %w", turn.ID, storeErr)
	default:
		return finished, runErr
	}
}
