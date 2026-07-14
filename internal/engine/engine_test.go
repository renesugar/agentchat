package engine_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/adapters/echo"
	"github.com/example/agentchat/internal/engine"
	"github.com/example/agentchat/internal/mcpserver"
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
	t1, err := eng.RunTurn(ctx, conv.ID, "echo", ws, adapter.TurnRequest{Prompt: "one", Effort: "high"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if t1.SnapshotID == "" {
		t.Fatal("turn 1 has no snapshot")
	}
	if t1.WorkspaceRef != ws.Dir {
		t.Fatalf("WorkspaceRef = %q, want %q", t1.WorkspaceRef, ws.Dir)
	}
	// Effort is recorded on the turn and reached the client (echo writes
	// it into ECHO.md), proving the end-to-end plumbing.
	if t1.Effort != "high" {
		t.Fatalf("turn 1 Effort = %q, want %q", t1.Effort, "high")
	}
	if b, err := os.ReadFile(filepath.Join(ws.Dir, "ECHO.md")); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(b), "effort: high") {
		t.Fatalf("ECHO.md missing effort line:\n%s", b)
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

// TestMoveConversationToProject exercises the Step 17 flow at engine
// level: after SetConversationProject, callers resolve the workspace from
// ProjectPath, so the next turn runs (and snapshots) in the project repo
// while earlier turns keep their historical refs; sidebar grouping
// follows via transcript.Projects.
func TestMoveConversationToProject(t *testing.T) {
	ctx := context.Background()
	store, err := transcript.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := workspace.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := mgr.CreateScratch(ctx, "mover")
	if err != nil {
		t.Fatal(err)
	}

	reg := adapter.NewRegistry()
	reg.Register(echo.New())
	eng := engine.New(reg, store)
	conv, _ := store.CreateConversation(ctx, transcript.NewConversation{Title: "mover"})

	t1, err := eng.RunTurn(ctx, conv.ID, "echo", scratch, adapter.TurnRequest{Prompt: "one"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Move: associate with a project repo (git-inited like a user repo).
	repoDir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=u", "-c", "user.email=u@x", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	moved, err := store.SetConversationProject(ctx, conv.ID, repoDir)
	if err != nil {
		t.Fatal(err)
	}

	// Future resolution prefers ProjectPath (what App.workspaceFor does).
	projWS, err := mgr.OpenRepo(ctx, moved.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := eng.RunTurn(ctx, conv.ID, "echo", projWS, adapter.TurnRequest{Prompt: "two"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if t2.WorkspaceRef != projWS.Dir || t2.SnapshotID == "" {
		t.Fatalf("turn 2 ran in %q (snapshot %q), want project repo %q", t2.WorkspaceRef, t2.SnapshotID, projWS.Dir)
	}
	// History untouched: turn 1 still points at the scratch workspace.
	if t1.WorkspaceRef != scratch.Dir {
		t.Fatalf("turn 1 ref rewritten to %q", t1.WorkspaceRef)
	}

	// Grouping follows the association.
	convs, err := store.ListConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projects := transcript.Projects(convs)
	if len(projects) != 1 || projects[0].Path != repoDir || projects[0].Count != 1 {
		t.Fatalf("Projects = %+v", projects)
	}
}

// mcpAdapter plays a coding client that uses the MCP callback channel: it
// POSTs JSON-RPC to req.MCP.URL like a streamable-HTTP MCP client would.
type mcpAdapter struct{ t *testing.T }

func (m *mcpAdapter) Name() string                        { return "mcpfake" }
func (m *mcpAdapter) Available(ctx context.Context) error { return nil }
func (m *mcpAdapter) Models(ctx context.Context) ([]adapter.Model, error) {
	return []adapter.Model{{ID: ""}}, nil
}

func (m *mcpAdapter) rpc(mcp *adapter.MCPServerInfo, body string) (int, string) {
	m.t.Helper()
	req, err := http.NewRequest(http.MethodPost, mcp.URL, strings.NewReader(body))
	if err != nil {
		m.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+mcp.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		m.t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func (m *mcpAdapter) RunTurn(ctx context.Context, req adapter.TurnRequest, emit adapter.EmitFunc) (*adapter.Result, error) {
	if req.MCP == nil {
		m.t.Fatal("engine did not set req.MCP")
	}
	if req.MCP.Name != "agentchat" {
		m.t.Errorf("MCP.Name = %q", req.MCP.Name)
	}

	// The context bootstrap fragment (Step 26): tells the client about
	// get_turns and the REST endpoint, names the token env var, and
	// NEVER contains the token itself.
	if req.Extra["context_bootstrap"] == "false" {
		if req.SystemPrompt != "" {
			m.t.Errorf("bootstrap not suppressed: %q", req.SystemPrompt)
		}
		res := &adapter.Result{ExitCode: 0, FinalText: "suppressed"}
		emit(adapter.Event{Kind: adapter.EventResult, Result: res})
		return res, nil
	}
	{
		sp := req.SystemPrompt
		if !strings.Contains(sp, "get_turns") || !strings.Contains(sp, "/context") ||
			!strings.Contains(sp, "AGENTCHAT_MCP_TOKEN") {
			m.t.Errorf("bootstrap fragment incomplete: %q", sp)
		}
		if strings.Contains(sp, req.MCP.Token) {
			m.t.Error("bearer token leaked into the system prompt")
		}
		if env := req.MCPEnv(); len(env) != 1 || env[0] != "AGENTCHAT_MCP_TOKEN="+req.MCP.Token {
			m.t.Errorf("MCPEnv = %v", env)
		}
	}

	if code, _ := m.rpc(req.MCP, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`); code != 200 {
		m.t.Errorf("initialize: status %d", code)
	}
	if code, _ := m.rpc(req.MCP, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		m.t.Errorf("initialized notification: status %d", code)
	}
	if code, body := m.rpc(req.MCP, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"progress","arguments":{"message":"compiling"}}}`); code != 200 || strings.Contains(body, "isError") {
		m.t.Errorf("progress: status %d body %s", code, body)
	}

	// A real artifact inside the workspace succeeds...
	if err := os.WriteFile(filepath.Join(req.WorkDir, "report.txt"), []byte("hi"), 0o644); err != nil {
		return nil, err
	}
	if code, body := m.rpc(req.MCP, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"add_artifact","arguments":{"path":"report.txt","note":"the report"}}}`); code != 200 || strings.Contains(body, `"isError":true`) {
		m.t.Errorf("add_artifact: status %d body %s", code, body)
	}
	// ...but escaping the workspace is refused (tool-level error).
	if code, body := m.rpc(req.MCP, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"add_artifact","arguments":{"path":"../../etc/passwd"}}}`); code != 200 || !strings.Contains(body, `"isError":true`) {
		m.t.Errorf("escaping add_artifact: status %d body %s, want isError", code, body)
	}

	// Conversation context mid-turn: the full transcript includes the
	// previous echo turn, and this in-flight turn's prompt.
	if code, body := m.rpc(req.MCP, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_turns","arguments":{}}}`); code != 200 ||
		!strings.Contains(body, "history marker one") || !strings.Contains(body, "## Turn 1") || !strings.Contains(body, "## Turn 2") {
		m.t.Errorf("get_turns(all): status %d body %.400s", code, body)
	}
	// last_n=1 trims to just the current turn.
	if code, body := m.rpc(req.MCP, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_turns","arguments":{"last_n":1}}}`); code != 200 ||
		strings.Contains(body, "## Turn 1") || !strings.Contains(body, "(1 of 2 turns)") {
		m.t.Errorf("get_turns(last_n=1): status %d body %.400s", code, body)
	}
	// The REST twin serves the same transcript for non-MCP clients.
	restReq, err := http.NewRequest(http.MethodGet, strings.Replace(req.MCP.URL, "/mcp", "/context", 1)+"?last_n=1", nil)
	if err != nil {
		m.t.Fatal(err)
	}
	restReq.Header.Set("Authorization", "Bearer "+req.MCP.Token)
	resp, err := http.DefaultClient.Do(restReq)
	if err != nil {
		m.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "(1 of 2 turns)") ||
		resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" {
		m.t.Errorf("GET /context: status %d type %q body %.200s", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
	}

	res := &adapter.Result{ExitCode: 0, FinalText: "done"}
	emit(adapter.Event{Kind: adapter.EventResult, Result: res})
	return res, nil
}

func TestRunTurnMCPCallback(t *testing.T) {
	ctx := context.Background()
	store, err := transcript.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := workspace.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := mgr.CreateScratch(ctx, "mcp-test")
	if err != nil {
		t.Fatal(err)
	}

	srv, err := mcpserver.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	reg := adapter.NewRegistry()
	reg.Register(&mcpAdapter{t: t})
	reg.Register(echo.New())
	eng := engine.New(reg, store)
	eng.MCP = srv
	type artCall struct{ convID, turnID, path, note string }
	var arts []artCall
	eng.ArtifactSink = func(ctx context.Context, convID, turnID, path, note string) (string, error) {
		arts = append(arts, artCall{convID, turnID, path, note})
		return "art-42", nil
	}

	conv, _ := store.CreateConversation(ctx, transcript.NewConversation{Title: "mcp"})
	// Turn 1 (echo) gives the context tool history to serve.
	if _, err := eng.RunTurn(ctx, conv.ID, "echo", ws, adapter.TurnRequest{Prompt: "history marker one"}, nil); err != nil {
		t.Fatal(err)
	}
	turn, err := eng.RunTurn(ctx, conv.ID, "mcpfake", ws, adapter.TurnRequest{Prompt: "go"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The progress push landed in the persisted event stream.
	events, err := store.Events(ctx, conv.ID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == adapter.EventThinking && ev.Text == "compiling" {
			found = true
		}
	}
	if !found {
		t.Errorf("no thinking event from MCP progress in %d events", len(events))
	}

	// The artifact sink got the resolved workspace path exactly once.
	if len(arts) != 1 {
		t.Fatalf("artifact sink calls = %+v, want 1", arts)
	}
	if arts[0].convID != conv.ID || arts[0].turnID != turn.ID ||
		arts[0].path != filepath.Join(ws.Dir, "report.txt") || arts[0].note != "the report" {
		t.Errorf("artifact call = %+v", arts[0])
	}

	// Tokens are turn-scoped: a fresh request with a made-up token after
	// the turn is rejected.
	m := &mcpAdapter{t: t}
	code, _ := m.rpc(&adapter.MCPServerInfo{URL: srv.URL(), Token: "not-a-real-token"}, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if code != 401 {
		t.Errorf("post-turn request: status %d, want 401", code)
	}

	// Extra["context_bootstrap"]="false" suppresses the fragment (the
	// adapter asserts SystemPrompt is empty and returns early).
	if _, err := eng.RunTurn(ctx, conv.ID, "mcpfake", ws, adapter.TurnRequest{
		Prompt: "again", Extra: map[string]string{"context_bootstrap": "false"},
	}, nil); err != nil {
		t.Fatal(err)
	}
}
