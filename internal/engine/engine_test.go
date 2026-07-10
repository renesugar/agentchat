package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/adapters/echo"
	"github.com/example/agentchat/internal/engine"
	"github.com/example/agentchat/internal/transcript"
	"github.com/example/agentchat/internal/workspace"
)

// silentAdapter edits the workspace but reports no file changes — like a
// client whose output gave us nothing structured. The engine must fill
// FilesChanged from the workspace snapshot diff.
type silentAdapter struct{ filename string }

func (s *silentAdapter) Name() string                        { return "silent" }
func (s *silentAdapter) Available(ctx context.Context) error { return nil }
func (s *silentAdapter) Models(ctx context.Context) ([]adapter.Model, error) {
	return []adapter.Model{{ID: ""}}, nil
}
func (s *silentAdapter) RunTurn(ctx context.Context, req adapter.TurnRequest, emit adapter.EmitFunc) (*adapter.Result, error) {
	if err := os.WriteFile(filepath.Join(req.WorkDir, s.filename), []byte(req.Prompt), 0o644); err != nil {
		return nil, err
	}
	res := &adapter.Result{ExitCode: 0, FinalText: "done"}
	emit(adapter.Event{Kind: adapter.EventResult, Result: res})
	return res, nil
}

func TestRunTurnSnapshotsWorkspace(t *testing.T) {
	ctx := context.Background()
	store, err := transcript.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := workspace.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := mgr.CreateScratch(ctx, "engine-test")
	if err != nil {
		t.Fatal(err)
	}

	reg := adapter.NewRegistry()
	reg.Register(echo.New())
	reg.Register(&silentAdapter{filename: "silent.txt"})
	eng := engine.New(reg, store)
	conv, _ := store.CreateConversation(ctx, transcript.NewConversation{Title: "ws"})

	// Turn 1 (echo): snapshot recorded; adapter-reported changes kept.
	t1, err := eng.RunTurn(ctx, conv.ID, "echo", ws, adapter.TurnRequest{Prompt: "one"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if t1.SnapshotID == "" {
		t.Fatal("turn 1 has no snapshot")
	}
	if t1.WorkspaceRef != ws.Dir {
		t.Fatalf("WorkspaceRef = %q, want %q", t1.WorkspaceRef, ws.Dir)
	}
	if len(t1.Result.FilesChanged) != 1 || t1.Result.FilesChanged[0].Path != "ECHO.md" {
		t.Fatalf("turn 1 FilesChanged = %+v", t1.Result.FilesChanged)
	}

	// Turn 2 (silent): adapter reported nothing; the snapshot diff must
	// supply the change.
	t2, err := eng.RunTurn(ctx, conv.ID, "silent", ws, adapter.TurnRequest{Prompt: "two"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if t2.SnapshotID == "" || t2.SnapshotID == t1.SnapshotID {
		t.Fatalf("turn 2 snapshot = %q (turn 1 = %q)", t2.SnapshotID, t1.SnapshotID)
	}
	if len(t2.Result.FilesChanged) != 1 || t2.Result.FilesChanged[0].Path != "silent.txt" ||
		t2.Result.FilesChanged[0].Op != adapter.FileCreated {
		t.Fatalf("turn 2 FilesChanged = %+v", t2.Result.FilesChanged)
	}

	// The two snapshots diff to exactly the second turn's change.
	changes, err := ws.Diff(ctx, t1.SnapshotID, t2.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "silent.txt" {
		t.Fatalf("snapshot diff = %+v", changes)
	}
}
