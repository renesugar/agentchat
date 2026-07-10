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
	"github.com/example/agentchat/internal/artifact"
	"github.com/example/agentchat/internal/clients"
	"github.com/example/agentchat/internal/config"
	"github.com/example/agentchat/internal/engine"
	"github.com/example/agentchat/internal/export"
	"github.com/example/agentchat/internal/mcpserver"
	"github.com/example/agentchat/internal/transcript"
	"github.com/example/agentchat/internal/workspace"
)

// App is the Wails binding surface: a thin layer over the headless engine.
type App struct {
	ctx   context.Context
	store *transcript.FSStore
	lib   *artifact.Library
	mgr   *workspace.Manager
	set   *clients.Set
	eng   *engine.Engine
	mcp   *mcpserver.Server // nil if the callback channel failed to start

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

	cfg, err := config.Load(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return nil, err
	}
	set := clients.New(cfg)

	app := &App{
		store:    store,
		lib:      lib,
		mgr:      mgr,
		set:      set,
		eng:      engine.New(set.Registry, store),
		wsByConv: make(map[string]*workspace.Workspace),
		running:  make(map[string]bool),
	}

	// MCP callback channel (Step 12): best-effort — if the loopback
	// listener can't start, turns still work via output capture.
	if srv, err := mcpserver.Start(); err == nil {
		app.mcp = srv
		app.eng.MCP = srv
		app.eng.ArtifactSink = func(ctx context.Context, convID, turnID, path, note string) (string, error) {
			art, err := lib.AddFileFromPath(ctx, path, "", artifact.Meta{
				ConversationID: convID, TurnID: turnID, Origin: "mcp", Note: note,
			})
			if err != nil {
				return "", err
			}
			return art.ID, nil
		}
	}

	return app, nil
}

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

func (a *App) shutdown(ctx context.Context) {
	if a.mcp != nil {
		_ = a.mcp.Close()
	}
}

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
	for _, name := range a.set.Registry.Names() {
		ad, err := a.set.Registry.Get(name)
		if err != nil {
			continue
		}
		info := AdapterInfo{Name: name, Available: true}
		if err := ad.Available(a.ctx); err != nil {
			info.Available = false
			info.Detail = err.Error()
		}
		if models, err := a.set.Models(a.ctx, name); err == nil {
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

// DeleteConversation removes a conversation (turns and events; artifacts
// are kept — they may be shared or exported). Refused while a turn is
// running in it.
func (a *App) DeleteConversation(convID string) error {
	a.mu.Lock()
	if a.running[convID] {
		a.mu.Unlock()
		return fmt.Errorf("a turn is running in this conversation")
	}
	delete(a.wsByConv, convID)
	a.mu.Unlock()
	return a.store.DeleteConversation(a.ctx, convID)
}

// ImportBundle restores a conversation from a bundle ZIP chosen via a
// native open dialog. Returns nil when the dialog is cancelled. On ID
// collision the import is refused and the error names the existing
// conversation.
func (a *App) ImportBundle() (*transcript.Conversation, error) {
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Import a conversation bundle",
		Filters: []runtime.FileFilter{
			{DisplayName: "AgentChat bundles (*.zip)", Pattern: "*.zip"},
		},
	})
	if err != nil || path == "" {
		return nil, err
	}
	conv, ws, err := export.Import(a.ctx, a.store, a.lib, a.mgr, path)
	if err != nil {
		return nil, err
	}
	if ws != nil {
		a.mu.Lock()
		a.wsByConv[conv.ID] = ws
		a.mu.Unlock()
	}
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
// Wails event "turn-event". effort "" means client default (a configured
// default_effort may still apply). It returns the finished turn record; a
// client failure is reported in Turn.Status/Error rather than as a hard
// error, so the UI can render it in place.
func (a *App) Run(convID, client, model, effort, prompt string) (*transcript.Turn, error) {
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

	req := adapter.TurnRequest{
		Prompt:    prompt,
		Model:     model,
		Effort:    effort,
		SessionID: sessionID,
	}
	a.set.Prepare(client, &req)

	turn, runErr := a.eng.RunTurn(a.ctx, convID, client, ws, req, tap)
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

// TurnMarkdown renders one turn as a standalone markdown section — the
// same content the full transcript export embeds for it. Used by the
// per-turn copy button.
func (a *App) TurnMarkdown(convID, turnID string) (string, error) {
	turns, err := a.store.ListTurns(a.ctx, convID)
	if err != nil {
		return "", err
	}
	for _, t := range turns {
		if t.ID != turnID {
			continue
		}
		events, err := a.store.Events(a.ctx, convID, turnID)
		if err != nil {
			return "", err
		}
		return string(export.TurnMarkdown(t, events)), nil
	}
	return "", fmt.Errorf("turn %q not found in conversation %q", turnID, convID)
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
