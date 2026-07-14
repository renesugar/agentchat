package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/example/agentchat/internal/adapter"
)

func parseFixture(t *testing.T, name, workDir string) ([]adapter.Event, *parseState) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var events []adapter.Event
	st, err := parseStream(f, workDir, func(e adapter.Event) { events = append(events, e) })
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	return events, st
}

func kinds(events []adapter.Event) []adapter.EventKind {
	out := make([]adapter.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func TestParseSimpleSession(t *testing.T) {
	events, st := parseFixture(t, "simple_session.jsonl", "/ws")

	want := []adapter.EventKind{
		adapter.EventThinking,
		adapter.EventText,
		adapter.EventToolUse, adapter.EventFileChange, // Write hello.py
		adapter.EventToolResult,
		adapter.EventToolUse, // Bash
		adapter.EventToolResult,
		adapter.EventToolUse, adapter.EventFileChange, // Edit hello.py
		adapter.EventToolResult,
		adapter.EventText,
	}
	if got := kinds(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event kinds:\n got %v\nwant %v", got, want)
	}

	// Session and result accumulation.
	if st.sessionID != "sess-abc123" {
		t.Errorf("sessionID = %q", st.sessionID)
	}
	res := st.result()
	if res.FinalText != `Done. hello.py prints "hello, world".` {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if res.Usage.InputTokens != 2410 || res.Usage.OutputTokens != 352 || res.Usage.CostUSD != 0.0134 {
		t.Errorf("Usage = %+v", res.Usage)
	}

	// File changes: created wins over the later edit; path relativized.
	if len(res.FilesChanged) != 1 {
		t.Fatalf("FilesChanged = %+v", res.FilesChanged)
	}
	if fc := res.FilesChanged[0]; fc.Path != "hello.py" || fc.Op != adapter.FileCreated {
		t.Errorf("FilesChanged[0] = %+v, want hello.py created", fc)
	}

	// tool_result content in both string and list-of-parts form parsed.
	var toolOutputs []string
	for _, e := range events {
		if e.Kind == adapter.EventToolResult {
			toolOutputs = append(toolOutputs, e.Tool.Output)
		}
	}
	if len(toolOutputs) != 3 || !strings.Contains(toolOutputs[0], "File created") || toolOutputs[1] != "hello\n" {
		t.Errorf("tool outputs = %q", toolOutputs)
	}

	// Raw payload preserved on normalized events.
	if len(events[0].Raw) == 0 {
		t.Error("Event.Raw not preserved")
	}
	if st.isError {
		t.Error("isError = true on success fixture")
	}
}

func TestParseErrorSession(t *testing.T) {
	events, st := parseFixture(t, "error_session.jsonl", "/ws")
	if !st.isError {
		t.Fatal("isError = false on error fixture")
	}
	if st.result().FinalText != "Credit balance is too low" {
		t.Errorf("FinalText = %q", st.result().FinalText)
	}
	// The error is surfaced as an event too.
	if got := kinds(events); !reflect.DeepEqual(got, []adapter.EventKind{adapter.EventError}) {
		t.Fatalf("event kinds = %v, want [error]", got)
	}
}

func TestParseToleratesGarbageLines(t *testing.T) {
	in := strings.NewReader("not json at all\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}` + "\n")
	var events []adapter.Event
	if _, err := parseStream(in, "/ws", func(e adapter.Event) { events = append(events, e) }); err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if got := kinds(events); !reflect.DeepEqual(got, []adapter.EventKind{adapter.EventError, adapter.EventText}) {
		t.Fatalf("event kinds = %v, want [error text]", got)
	}
}

func TestBuildArgs(t *testing.T) {
	got := buildArgs(adapter.TurnRequest{Prompt: "fix the bug"})
	want := []string{"-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "acceptEdits", "--", "fix the bug"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default args:\n got %v\nwant %v", got, want)
	}

	got = buildArgs(adapter.TurnRequest{
		Prompt: "-continue", Model: "opus", SessionID: "sess-1", Effort: "xhigh",
		Extra: map[string]string{"permission_mode": "plan"},
	})
	want = []string{"-p", "--output-format", "stream-json", "--verbose",
		"--model", "opus", "--resume", "sess-1", "--effort", "xhigh",
		"--permission-mode", "plan", "--", "-continue"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("full args:\n got %v\nwant %v", got, want)
	}

	// SystemPrompt rides on --append-system-prompt (never --system-prompt,
	// which would replace the preset).
	got = buildArgs(adapter.TurnRequest{Prompt: "x", SystemPrompt: "[AgentChat context] hello"})
	want = []string{"-p", "--output-format", "stream-json", "--verbose",
		"--append-system-prompt", "[AgentChat context] hello",
		"--permission-mode", "acceptEdits", "--", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("system-prompt args:\n got %v\nwant %v", got, want)
	}

	// Explicitly empty permission mode drops the flag entirely.
	got = buildArgs(adapter.TurnRequest{Prompt: "x", Extra: map[string]string{"permission_mode": ""}})
	for _, a := range got {
		if a == "--permission-mode" {
			t.Errorf("empty permission_mode should omit the flag: %v", got)
		}
	}
}

func TestBuildArgsMCP(t *testing.T) {
	got := buildArgs(adapter.TurnRequest{Prompt: "go", MCP: &adapter.MCPServerInfo{
		Name: "agentchat", URL: "http://127.0.0.1:9999/mcp", Token: "tok123",
	}})

	// The inline --mcp-config JSON must round-trip to the http server spec.
	var cfgJSON string
	for i, a := range got {
		if a == "--mcp-config" && i+1 < len(got) {
			cfgJSON = got[i+1]
		}
	}
	if cfgJSON == "" {
		t.Fatalf("no --mcp-config in %v", got)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatalf("parsing %q: %v", cfgJSON, err)
	}
	srv, ok := cfg.MCPServers["agentchat"]
	if !ok || srv.Type != "http" || srv.URL != "http://127.0.0.1:9999/mcp" ||
		srv.Headers["Authorization"] != "Bearer tok123" {
		t.Errorf("mcp config = %+v", cfg)
	}

	// The server's tools must be pre-approved (print mode cannot prompt).
	allowed := ""
	for i, a := range got {
		if a == "--allowedTools" && i+1 < len(got) {
			allowed = got[i+1]
		}
	}
	if allowed != "mcp__agentchat" {
		t.Errorf("allowedTools = %q, want mcp__agentchat", allowed)
	}

	// Without MCP neither flag appears.
	for _, a := range buildArgs(adapter.TurnRequest{Prompt: "go"}) {
		if a == "--mcp-config" || a == "--allowedTools" {
			t.Errorf("unexpected %s without req.MCP", a)
		}
	}
}

func TestFileChangeFromTool(t *testing.T) {
	cases := []struct {
		tool, input string
		want        *adapter.FileChange
	}{
		{"Write", `{"file_path":"/ws/a.go","content":"x"}`, &adapter.FileChange{Path: "a.go", Op: adapter.FileCreated}},
		{"Edit", `{"file_path":"/ws/sub/b.go"}`, &adapter.FileChange{Path: "sub/b.go", Op: adapter.FileModified}},
		{"NotebookEdit", `{"notebook_path":"/ws/n.ipynb"}`, &adapter.FileChange{Path: "n.ipynb", Op: adapter.FileModified}},
		{"Edit", `{"file_path":"/elsewhere/c.go"}`, &adapter.FileChange{Path: "/elsewhere/c.go", Op: adapter.FileModified}},
		{"Bash", `{"command":"ls"}`, nil},
		{"Write", `{}`, nil},
	}
	for _, c := range cases {
		got := fileChangeFromTool(c.tool, []byte(c.input), "/ws")
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("fileChangeFromTool(%s, %s) = %+v, want %+v", c.tool, c.input, got, c.want)
		}
	}
}

// TestRunTurnWithStubBinary exercises RunTurn's process handling (exit
// codes, terminal event contract) using a shell script that replays a
// fixture — never the real client.
func TestRunTurnWithStubBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub script requires a POSIX shell")
	}
	fixture, err := filepath.Abs(filepath.Join("testdata", "simple_session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	stub := filepath.Join(dir, "claude-stub")
	script := "#!/bin/sh\ncat " + fixture + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &Adapter{Binary: stub}
	var events []adapter.Event
	res, err := a.RunTurn(context.Background(), adapter.TurnRequest{
		Prompt: "make hello world", WorkDir: dir,
	}, func(e adapter.Event) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	if res.ExitCode != 0 || res.SessionID != "sess-abc123" || res.Duration <= 0 {
		t.Fatalf("result: %+v", res)
	}
	var terminal int
	for _, e := range events {
		if e.Kind == adapter.EventResult {
			terminal++
		}
	}
	if terminal != 1 || events[len(events)-1].Kind != adapter.EventResult {
		t.Fatalf("terminal event contract violated: kinds=%v", kinds(events))
	}

	// A failing binary surfaces the exit code and stderr.
	bad := filepath.Join(dir, "claude-bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a = &Adapter{Binary: bad}
	res, err = a.RunTurn(context.Background(), adapter.TurnRequest{Prompt: "x", WorkDir: dir}, func(adapter.Event) {})
	if err == nil || res.ExitCode != 3 || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("bad binary: res=%+v err=%v", res, err)
	}
}
