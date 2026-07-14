package codex

import (
	"context"
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
		adapter.EventThinking,                         // reasoning
		adapter.EventPlan,                             // todo_list started
		adapter.EventToolUse, adapter.EventToolResult, // ls
		adapter.EventFileChange, adapter.EventFileChange, // hello.py add, README.md update
		adapter.EventToolUse, adapter.EventToolResult, // python3 hello.py
		adapter.EventPlan, // todo_list completed
		adapter.EventText, // agent_message
	}
	if got := kinds(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event kinds:\n got %v\nwant %v", got, want)
	}

	if st.sessionID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Errorf("sessionID = %q", st.sessionID)
	}
	res := st.result()
	if res.FinalText != "Created hello.py and verified it prints hello." {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	// input + cached input are both prompt-side tokens.
	if res.Usage.InputTokens != 4547+2432 || res.Usage.OutputTokens != 88 {
		t.Errorf("Usage = %+v", res.Usage)
	}

	wantFC := []adapter.FileChange{
		{Path: "hello.py", Op: adapter.FileCreated},
		{Path: "README.md", Op: adapter.FileModified},
	}
	if !reflect.DeepEqual(res.FilesChanged, wantFC) {
		t.Errorf("FilesChanged = %+v, want %+v", res.FilesChanged, wantFC)
	}

	// Tool result carries command output; plan renders a checklist.
	if events[3].Tool.Output != "README.md\n" || events[3].Tool.IsErr {
		t.Errorf("tool result = %+v", events[3].Tool)
	}
	if !strings.Contains(events[8].Text, "[x] Write hello.py") {
		t.Errorf("plan text = %q", events[8].Text)
	}
	if st.failed {
		t.Error("failed = true on success fixture")
	}
}

func TestParseFailedSession(t *testing.T) {
	events, st := parseFixture(t, "failed_session.jsonl", "/ws")
	if !st.failed || st.failMsg != "stream disconnected before completion" {
		t.Fatalf("failed=%v msg=%q", st.failed, st.failMsg)
	}
	// Reconnect notice, item error, turn.failed → three error events, but
	// only turn.failed/fatal errors flip the failed flag.
	want := []adapter.EventKind{adapter.EventError, adapter.EventError, adapter.EventError}
	if got := kinds(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	if !strings.HasPrefix(events[0].Text, "Reconnecting") {
		t.Errorf("first event = %q", events[0].Text)
	}
}

func TestReconnectNoticeIsNotFatal(t *testing.T) {
	in := strings.NewReader(`{"type":"error","message":"Reconnecting... 2/5"}` + "\n" +
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"ok"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}` + "\n")
	var events []adapter.Event
	st, err := parseStream(in, "/ws", func(e adapter.Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if st.failed {
		t.Fatal("reconnect notice marked the turn failed")
	}
	if st.finalText != "ok" {
		t.Fatalf("finalText = %q", st.finalText)
	}
}

func TestParseAcceptsLegacyItemTypeKey(t *testing.T) {
	in := strings.NewReader(`{"type":"item.completed","item":{"id":"i","item_type":"agent_message","text":"legacy"}}` + "\n")
	var events []adapter.Event
	if _, err := parseStream(in, "/ws", func(e adapter.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != adapter.EventText || events[0].Text != "legacy" {
		t.Fatalf("events = %+v", events)
	}
}

func TestBuildArgs(t *testing.T) {
	got := buildArgs(adapter.TurnRequest{Prompt: "fix the bug"})
	want := []string{"exec", "--json", "--sandbox", "workspace-write", "--skip-git-repo-check", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default args:\n got %v\nwant %v", got, want)
	}

	got = buildArgs(adapter.TurnRequest{
		Prompt: "continue", Model: "gpt-5.6-sol", SessionID: "0199-abc", Effort: "low",
		Extra: map[string]string{"sandbox": "read-only", "skip_git_repo_check": "false"},
	})
	want = []string{"exec", "--json", "--sandbox", "read-only",
		"--model", "gpt-5.6-sol", "-c", `model_reasoning_effort="low"`,
		"resume", "0199-abc", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("full args:\n got %v\nwant %v", got, want)
	}

	// Explicitly empty sandbox drops the flag entirely.
	got = buildArgs(adapter.TurnRequest{Prompt: "x", Extra: map[string]string{"sandbox": ""}})
	for _, a := range got {
		if a == "--sandbox" {
			t.Errorf("empty sandbox should omit the flag: %v", got)
		}
	}
}

func TestBuildArgsMCP(t *testing.T) {
	mcp := &adapter.MCPServerInfo{Name: "agentchat", URL: "http://127.0.0.1:9999/mcp", Token: "tok123"}

	got := buildArgs(adapter.TurnRequest{Prompt: "go", SessionID: "0199-abc", MCP: mcp})
	want := []string{"exec", "--json", "--sandbox", "workspace-write", "--skip-git-repo-check",
		"-c", `mcp_servers.agentchat.url="http://127.0.0.1:9999/mcp"`,
		"-c", `mcp_servers.agentchat.bearer_token_env_var="AGENTCHAT_MCP_TOKEN"`,
		"resume", "0199-abc", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mcp args:\n got %v\nwant %v", got, want)
	}

	// The token travels via the environment, never argv.
	for _, a := range got {
		if strings.Contains(a, "tok123") {
			t.Errorf("token leaked into argv: %v", got)
		}
	}
	r := adapter.TurnRequest{MCP: mcp}
	if env := r.MCPEnv(); !reflect.DeepEqual(env, []string{"AGENTCHAT_MCP_TOKEN=tok123"}) {
		t.Errorf("MCPEnv = %v", env)
	}
	none := adapter.TurnRequest{}
	if none.MCPEnv() != nil {
		t.Error("MCPEnv without MCP should be nil")
	}
}

func TestBuildArgsSystemPrompt(t *testing.T) {
	got := buildArgs(adapter.TurnRequest{Prompt: "go", SystemPrompt: "line one\nline \"two\""})
	want := []string{"exec", "--json", "--sandbox", "workspace-write", "--skip-git-repo-check",
		"-c", `developer_instructions="line one\nline \"two\""`, "-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("system-prompt args:\n got %v\nwant %v", got, want)
	}
}

func TestTomlQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", `"plain"`},
		{"a\nb\tc", `"a\nb\tc"`},
		{`quote " and \ slash`, `"quote \" and \\ slash"`},
		{"ctrl\x01char", `"ctrl\u0001char"`},
		{"unicode ünïcode", `"unicode ünïcode"`},
	}
	for _, c := range cases {
		if got := tomlQuote(c.in); got != c.want {
			t.Errorf("tomlQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestRunTurnWithStubBinary exercises RunTurn's process handling using a
// shell script that replays a fixture — never the real client. It also
// verifies the prompt arrives on stdin.
func TestRunTurnWithStubBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub script requires a POSIX shell")
	}
	fixture, err := filepath.Abs(filepath.Join("testdata", "simple_session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	promptCopy := filepath.Join(dir, "prompt.txt")
	stub := filepath.Join(dir, "codex-stub")
	script := "#!/bin/sh\ncat > " + promptCopy + "\ncat " + fixture + "\n"
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

	if res.ExitCode != 0 || res.SessionID == "" || res.Duration <= 0 {
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
	if b, _ := os.ReadFile(promptCopy); string(b) != "make hello world" {
		t.Fatalf("prompt on stdin = %q", b)
	}

	// A failing binary surfaces the exit code and stderr.
	bad := filepath.Join(dir, "codex-bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\ncat > /dev/null\necho boom >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a = &Adapter{Binary: bad}
	res, err = a.RunTurn(context.Background(), adapter.TurnRequest{Prompt: "x", WorkDir: dir}, func(adapter.Event) {})
	if err == nil || res.ExitCode != 2 || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("bad binary: res=%+v err=%v", res, err)
	}
}
