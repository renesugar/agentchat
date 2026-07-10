package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/adapters/aider"
	"github.com/example/agentchat/internal/adapters/claudecode"
	"github.com/example/agentchat/internal/adapters/codex"
	"github.com/example/agentchat/internal/adapters/echo"
	"github.com/example/agentchat/internal/adapters/swival"
	"github.com/example/agentchat/internal/artifact"
	"github.com/example/agentchat/internal/engine"
	"github.com/example/agentchat/internal/export"
	"github.com/example/agentchat/internal/transcript"
	"github.com/example/agentchat/internal/workspace"
)

// App is the Wails binding surface: a thin layer over the headless engine.
type App struct {
	ctx   context.Context
	store *transcript.FSStore
	lib   *artifact.Library
	mgr   *workspace.Manager
	reg   *adapter.Registry
	eng   *engine.Engine

	mu       sync.Mutex
	wsByConv map[string]*workspace.Workspace
	running  map[string]bool // convID -> a turn is in flight
}

// NewApp wires the engine against the data directory (defaults to
// $AGENTCHAT_DATA or ~/.agentchat).
func NewApp(dataDir string) (*App, error) {
	if dataDir == "" {
		dataDir = os.Getenv("AGENTCHAT_DATA")
	}
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving data dir: %w", err)
		}
		dataDir = filepath.Join(home, ".agentchat")
	}

	store, err := transcript.NewFSStore(dataDir)
	if err != nil {
		return nil, err
	}
	lib, err := artifact.NewLibrary(filepath.Join(dataDir, "artifacts"))
	if err != nil {
		return nil, err
	}
	mgr, err := workspace.NewManager(filepath.Join(dataDir, "workspaces"))
	if err != nil {
		return nil, err
	}

	reg := adapter.NewRegistry()
	reg.Register(claudecode.New())
	reg.Register(codex.New())
	reg.Register(aider.New())
	reg.Register(swival.New())
	reg.Register(echo.New())

	return &App{
		store:    store,
		lib:      lib,
		mgr:      mgr,
		reg:      reg,
		eng:      engine.New(reg, store),
		wsByConv: make(map[string]*workspace.Workspace),
		running:  make(map[string]bool),
	}, nil
}

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// --- adapters ---

// AdapterInfo is what the model picker renders.
type AdapterInfo struct {
	Name      string          `json:"name"`
	Available bool            `json:"available"`
	Detail    string          `json:"detail,omitempty"` // why unavailable
	Models    []adapter.Model `json:"models"`
}

// Adapters lists registered coding clients with availability and models.
func (a *App) Adapters() ([]AdapterInfo, error) {
	var out []AdapterInfo
	for _, name := range a.reg.Names() {
		ad, err := a.reg.Get(name)
		if err != nil {
			continue
		}
		info := AdapterInfo{Name: name, Available: true}
		if err := ad.Available(a.ctx); err != nil {
			info.Available = false
			info.Detail = err.Error()
		}
		if models, err := ad.Models(a.ctx); err == nil {
			info.Models = models
		}
		out = append(out, info)
	}
	return out, nil
}

// --- conversations & transcript reads ---

// Conversations returns all conversations, newest first.
func (a *App) Conversations() ([]*transcript.Conversation, error) {
	return a.store.ListConversations(a.ctx)
}

// Turns returns a conversation's turns in order.
func (a *App) Turns(convID string) ([]*transcript.Turn, error) {
	return a.store.ListTurns(a.ctx, convID)
}

// Events returns a turn's normalized event stream.
func (a *App) Events(convID, turnID string) ([]adapter.Event, error) {
	return a.store.Events(a.ctx, convID, turnID)
}

// CreateConversation makes a conversation and its workspace. repoPath ""
// creates a managed scratch workspace; a git repo path is opened as a
// snapshot-managed repo workspace (and becomes the project grouping key).
func (a *App) CreateConversation(title, repoPath string) (*transcript.Conversation, error) {
	var ws *workspace.Workspace
	var err error
	if repoPath != "" {
		if ws, err = a.mgr.OpenRepo(a.ctx, repoPath); err != nil {
			return nil, err
		}
		repoPath = ws.Dir
	}
	conv, err := a.store.CreateConversation(a.ctx, transcript.NewConversation{
		Title: title, ProjectPath: repoPath,
	})
	if err != nil {
		return nil, err
	}
	if ws == nil {
		if ws, err = a.mgr.CreateScratch(a.ctx, title); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	a.wsByConv[conv.ID] = ws
	a.mu.Unlock()
	return conv, nil
}

// PickRepoDirectory opens a native directory chooser and returns the
// selected path ("" if cancelled).
func (a *App) PickRepoDirectory() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Choose a git repository",
	})
}

// --- running turns ---

// turnEvent is the payload pushed to the frontend for every normalized
// event while a turn is running.
type turnEvent struct {
	ConversationID string        `json:"conversationId"`
	Event          adapter.Event `json:"event"`
}

// Run executes one turn and streams its events to the frontend as the
// Wails event "turn-event". It returns the finished turn record; a client
// failure is reported in Turn.Status/Error rather than as a hard error, so
// the UI can render it in place.
func (a *App) Run(convID, client, model, prompt string) (*transcript.Turn, error) {
	a.mu.Lock()
	if a.running[convID] {
		a.mu.Unlock()
		return nil, fmt.Errorf("a turn is already running in this conversation")
	}
	a.running[convID] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.running, convID)
		a.mu.Unlock()
	}()

	ws, err := a.workspaceFor(convID)
	if err != nil {
		return nil, err
	}

	// Session continuity: reuse the last successful turn's session ID when
	// the same client runs again.
	sessionID := ""
	if turns, err := a.store.ListTurns(a.ctx, convID); err == nil {
		for i := len(turns) - 1; i >= 0; i-- {
			t := turns[i]
			if t.Client == client && t.Result != nil && t.Result.SessionID != "" {
				sessionID = t.Result.SessionID
				break
			}
		}
	}

	tap := func(ev adapter.Event) {
		runtime.EventsEmit(a.ctx, "turn-event", turnEvent{ConversationID: convID, Event: ev})
	}

	turn, runErr := a.eng.RunTurn(a.ctx, convID, client, ws, adapter.TurnRequest{
		Prompt:    prompt,
		Model:     model,
		SessionID: sessionID,
	}, tap)
	if turn == nil && runErr != nil {
		return nil, runErr // setup failure (unknown client, unavailable, storage)
	}
	return turn, nil
}

// workspaceFor resolves the conversation's workspace: cached handle → the
// conversation's project repo → the last turn's workspace dir (reopened)
// → a fresh scratch workspace.
func (a *App) workspaceFor(convID string) (*workspace.Workspace, error) {
	a.mu.Lock()
	if ws, ok := a.wsByConv[convID]; ok {
		a.mu.Unlock()
		return ws, nil
	}
	a.mu.Unlock()

	conv, err := a.store.GetConversation(a.ctx, convID)
	if err != nil {
		return nil, err
	}

	var ws *workspace.Workspace
	if conv.ProjectPath != "" {
		if ws, err = a.mgr.OpenRepo(a.ctx, conv.ProjectPath); err != nil {
			return nil, fmt.Errorf("project repo unavailable: %w", err)
		}
	} else if turns, err := a.store.ListTurns(a.ctx, convID); err == nil && len(turns) > 0 {
		if dir := turns[len(turns)-1].WorkspaceRef; dir != "" {
			if w, err := a.mgr.OpenRepo(a.ctx, dir); err == nil {
				ws = w // a scratch dir is a git repo; reopening keeps snapshots chained
			}
		}
	}
	if ws == nil {
		if ws, err = a.mgr.CreateScratch(a.ctx, conv.Title); err != nil {
			return nil, err
		}
	}

	a.mu.Lock()
	a.wsByConv[convID] = ws
	a.mu.Unlock()
	return ws, nil
}

// --- artifacts & export ---

// Artifacts lists the conversation's artifact records, newest first.
func (a *App) Artifacts(convID string) ([]*artifact.Artifact, error) {
	return a.lib.List(a.ctx, convID)
}

// AttachFile copies a local file into the artifact library for the
// conversation, via a native file chooser.
func (a *App) AttachFile(convID string) (*artifact.Artifact, error) {
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{Title: "Attach a file"})
	if err != nil || path == "" {
		return nil, err
	}
	return a.lib.AddFileFromPath(a.ctx, path, "", artifact.Meta{
		ConversationID: convID, Origin: "user-upload",
	})
}

// ExportMarkdown writes the conversation transcript to a user-chosen path
// and returns it ("" if cancelled).
func (a *App) ExportMarkdown(convID string) (string, error) {
	ex := &export.Exporter{Store: a.store, Library: a.lib}
	md, err := ex.Markdown(a.ctx, convID)
	if err != nil {
		return "", err
	}
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title: "Save transcript", DefaultFilename: "transcript-" + convID + ".md",
	})
	if err != nil || path == "" {
		return "", err
	}
	if err := os.WriteFile(path, md, 0o644); err != nil {
		return "", err
	}
	a.recordExport(convID, path)
	return path, nil
}

// ExportBundle writes the conversation's ZIP bundle (transcript, stored
// artifacts, latest workspace snapshot) to a user-chosen path.
func (a *App) ExportBundle(convID string) (string, error) {
	ws, err := a.workspaceFor(convID)
	if err != nil {
		ws = nil // bundle still works without a workspace
	}
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title: "Save bundle", DefaultFilename: "bundle-" + convID + ".zip",
	})
	if err != nil || path == "" {
		return "", err
	}
	ex := &export.Exporter{Store: a.store, Library: a.lib}
	if err := ex.Bundle(a.ctx, convID, ws, path); err != nil {
		return "", err
	}
	a.recordExport(convID, path)
	return path, nil
}

func (a *App) recordExport(convID, path string) {
	_, _ = a.lib.AddLink(a.ctx, filepath.Base(path), path, "", artifact.Meta{
		ConversationID: convID, Origin: "export",
		Note: "exported " + time.Now().UTC().Format(time.RFC3339),
	})
}
