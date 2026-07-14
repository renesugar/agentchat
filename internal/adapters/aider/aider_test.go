package aider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/example/agentchat/internal/adapter"
)

func kinds(events []adapter.Event) []adapter.EventKind {
	out := make([]adapter.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func TestParseSimpleSession(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "simple_session.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var events []adapter.Event
	st, err := parseOutput(f, func(e adapter.Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}

	want := []adapter.EventKind{
		adapter.EventText,       // prose + edit block
		adapter.EventFileChange, // Applied edit to hello.py
		adapter.EventToolResult, // Commit ...
		adapter.EventText,       // closing prose
	}
	if got := kinds(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event kinds:\n got %v\nwant %v", got, want)
	}

	// Banner noise suppressed; prose and the edit block kept.
	if strings.Contains(events[0].Text, "Aider v") || strings.Contains(events[0].Text, "Repo-map") {
		t.Errorf("banner noise leaked into text: %q", events[0].Text)
	}
	if !strings.Contains(events[0].Text, "SEARCH") {
		t.Errorf("edit block missing from text: %q", events[0].Text)
	}

	if events[1].File.Path != "hello.py" || events[1].File.Op != adapter.FileModified {
		t.Errorf("file change = %+v", events[1].File)
	}
	if events[2].Tool.Name != "git-commit" || events[2].Tool.Input != "a1b2c3d" ||
		!strings.Contains(events[2].Tool.Output, "personalize greeting") {
		t.Errorf("commit event = %+v", events[2].Tool)
	}

	res := st.result()
	if res.Usage.InputTokens != 12000 || res.Usage.OutputTokens != 1200 || res.Usage.CostUSD != 0.0341 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if !strings.HasPrefix(res.FinalText, "The greeting now takes") {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if len(res.FilesChanged) != 1 || res.FilesChanged[0].Path != "hello.py" {
		t.Errorf("FilesChanged = %+v", res.FilesChanged)
	}
	if res.SessionID != "" {
		t.Errorf("SessionID = %q, want empty (aider has no sessions)", res.SessionID)
	}
}

func TestParseTokenCount(t *testing.T) {
	cases := map[string]int64{
		"12k": 12000, "1.2k": 1200, "847": 847, "1,234": 1234, "junk": 0,
	}
	for in, want := range cases {
		if got := parseTokenCount(in); got != want {
			t.Errorf("parseTokenCount(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	got := buildArgs(adapter.TurnRequest{Prompt: "fix the bug"}, "")
	want := []string{"--message", "fix the bug", "--yes-always", "--no-stream", "--no-pretty"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default args:\n got %v\nwant %v", got, want)
	}

	got = buildArgs(adapter.TurnRequest{
		Prompt: "continue", Model: "sonnet", SessionID: "ignored", Effort: "high",
		Extra: map[string]string{"restore_chat_history": "true"},
	}, "/tmp/ctx.md")
	want = []string{"--message", "continue", "--yes-always", "--no-stream", "--no-pretty",
		"--model", "sonnet", "--reasoning-effort", "high",
		"--read", "/tmp/ctx.md", "--restore-chat-history"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("full args:\n got %v\nwant %v", got, want)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestRunTurnDerivesChangesFromGit uses a stub script standing in for aider
// that edits a file and commits (as aider's auto-commit would), verifying
// that RunTurn derives authoritative file changes from the git before/after
// diff — including a change the textual output never mentioned.
func TestRunTurnDerivesChangesFromGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub script requires a POSIX shell")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	mustGit(t, repo, "config", "user.email", "t@example.com")
	mustGit(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "hello.py"), []byte("print('hello')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-qm", "initial")

	// Stub: modify hello.py, create extra.txt, commit, print aider-ish output.
	stub := filepath.Join(t.TempDir(), "aider-stub")
	script := `#!/bin/sh
echo "print('hello, world')" > hello.py
echo "notes" > extra.txt
git add -A
git commit -qm "stub change"
echo "Aider v0.86.1"
echo "I updated the greeting."
echo "Applied edit to hello.py"
echo "Tokens: 1k sent, 100 received. Cost: \$0.01 message, \$0.01 session."
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &Adapter{Binary: stub}
	var events []adapter.Event
	res, err := a.RunTurn(context.Background(), adapter.TurnRequest{
		Prompt: "update greeting", WorkDir: repo,
	}, func(e adapter.Event) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	// Git diff is authoritative: both files, correct ops — the parser alone
	// only saw hello.py.
	byPath := map[string]adapter.FileOp{}
	for _, fc := range res.FilesChanged {
		byPath[fc.Path] = fc.Op
	}
	if byPath["hello.py"] != adapter.FileModified || byPath["extra.txt"] != adapter.FileCreated {
		t.Fatalf("FilesChanged = %+v", res.FilesChanged)
	}
	if res.ExitCode != 0 || res.Usage.InputTokens != 1000 || res.Duration <= 0 {
		t.Fatalf("result = %+v", res)
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
	bad := filepath.Join(t.TempDir(), "aider-bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho boom >&2\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a = &Adapter{Binary: bad}
	res, err = a.RunTurn(context.Background(), adapter.TurnRequest{Prompt: "x", WorkDir: repo}, func(adapter.Event) {})
	if err == nil || res.ExitCode != 4 || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("bad binary: res=%+v err=%v", res, err)
	}
}

// TestRunTurnOutsideGitRepo verifies the fallback: without a repo, file
// changes come from the Applied-edit lines only.
func TestRunTurnOutsideGitRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub script requires a POSIX shell")
	}
	dir := t.TempDir()
	stub := filepath.Join(t.TempDir(), "aider-stub")
	script := "#!/bin/sh\necho \"Applied edit to notes.md\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Binary: stub}
	res, err := a.RunTurn(context.Background(), adapter.TurnRequest{Prompt: "x", WorkDir: dir}, func(adapter.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FilesChanged) != 1 || res.FilesChanged[0].Path != "notes.md" {
		t.Fatalf("FilesChanged = %+v", res.FilesChanged)
	}
}
