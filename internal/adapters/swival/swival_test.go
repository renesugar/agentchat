package swival

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

func TestBuildArgs(t *testing.T) {
	got := buildArgs(adapter.TurnRequest{Prompt: "task"}, "/tmp/r.json")
	want := []string{"--report", "/tmp/r.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default args:\n got %v\nwant %v", got, want)
	}

	got = buildArgs(adapter.TurnRequest{
		Prompt: "task", Model: "qwen3-coder-next", SessionID: "ignored",
		Extra: map[string]string{
			"diagnostics": "false", "profile": "local", "provider": "generic",
			"base_url": "http://127.0.0.1:8080", "reasoning_effort": "high", "max_turns": "50",
		},
	}, "/tmp/r.json")
	want = []string{"--quiet", "--profile", "local", "--provider", "generic",
		"--base-url", "http://127.0.0.1:8080", "--model", "qwen3-coder-next",
		"--reasoning-effort", "high", "--max-turns", "50", "--report", "/tmp/r.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("full args:\n got %v\nwant %v", got, want)
	}
}

func TestApplyReport(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "report.json"))
	if err != nil {
		t.Fatal(err)
	}

	// stdout answer wins; report fills usage from timeline llm_calls.
	res := &adapter.Result{FinalText: "from stdout"}
	applyReport(b, res)
	if res.FinalText != "from stdout" {
		t.Errorf("stdout answer overwritten: %q", res.FinalText)
	}
	if res.Usage.InputTokens != 3800+4200+2000+4500 {
		t.Errorf("InputTokens = %d", res.Usage.InputTokens)
	}

	// Without stdout (e.g. --quiet piping mishap), report answer is the fallback.
	res = &adapter.Result{}
	applyReport(b, res)
	if res.FinalText != "Added greet() to hello.py and verified it." {
		t.Errorf("fallback answer = %q", res.FinalText)
	}

	// Garbage report is ignored.
	res = &adapter.Result{FinalText: "keep"}
	applyReport([]byte("not json"), res)
	if res.FinalText != "keep" {
		t.Errorf("garbage report mutated result: %+v", res)
	}
}

func TestPorcelainDiff(t *testing.T) {
	before := map[string]string{
		"dirty.go": " M", // pre-existing dirt: unchanged, must not appear
		"gone.txt": "??",
	}
	after := map[string]string{
		"dirty.go":   " M",
		"new.txt":    "??",
		"edited.go":  " M",
		"removed.go": " D",
	}
	got := porcelainDiff(before, after)
	want := []adapter.FileChange{
		{Path: "edited.go", Op: adapter.FileModified},
		{Path: "gone.txt", Op: adapter.FileDeleted},
		{Path: "new.txt", Op: adapter.FileCreated},
		{Path: "removed.go", Op: adapter.FileDeleted},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("porcelainDiff:\n got %+v\nwant %+v", got, want)
	}
	if porcelainDiff(nil, nil) != nil {
		t.Error("nil snapshots should yield nil")
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

// TestRunTurnWithStubBinary uses a stub script standing in for swival that
// honors the real output contract: diagnostics on stderr, answer on stdout,
// JSON report to the --report path, file edits in the workspace.
func TestRunTurnWithStubBinary(t *testing.T) {
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

	fixture, _ := filepath.Abs(filepath.Join("testdata", "report.json"))
	stub := filepath.Join(t.TempDir(), "swival-stub")
	script := `#!/bin/sh
task=$(cat)
# find the --report path
report=""
prev=""
for a in "$@"; do
  [ "$prev" = "--report" ] && report="$a"
  prev="$a"
done
echo "turn 1: read_file(hello.py)" >&2
echo "turn 2: edit_file(hello.py)" >&2
echo "def greet(): pass" >> hello.py
echo "notes" > extra.txt
cp ` + fixture + ` "$report"
echo "Added greet() to hello.py. Task was: $task"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &Adapter{Binary: stub}
	var events []adapter.Event
	res, err := a.RunTurn(context.Background(), adapter.TurnRequest{
		Prompt: "add a greet function", WorkDir: repo,
	}, func(e adapter.Event) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	// stdin task made it through; stdout is the final text.
	if !strings.Contains(res.FinalText, "Task was: add a greet function") {
		t.Fatalf("FinalText = %q", res.FinalText)
	}
	// stderr diagnostics streamed as thinking events, before the text.
	var thinking int
	for _, e := range events {
		if e.Kind == adapter.EventThinking {
			thinking++
		}
	}
	if thinking != 2 {
		t.Fatalf("thinking events = %d, want 2", thinking)
	}
	// Report mined for usage.
	if res.Usage.InputTokens == 0 {
		t.Fatalf("usage not mined from report: %+v", res.Usage)
	}
	// Worktree diff caught both files.
	byPath := map[string]adapter.FileOp{}
	for _, fc := range res.FilesChanged {
		byPath[fc.Path] = fc.Op
	}
	if byPath["hello.py"] != adapter.FileModified || byPath["extra.txt"] != adapter.FileCreated {
		t.Fatalf("FilesChanged = %+v", res.FilesChanged)
	}
	// Terminal event contract.
	if events[len(events)-1].Kind != adapter.EventResult {
		t.Fatalf("last event = %v", events[len(events)-1].Kind)
	}

	// Exit code 2 = turn limit: partial result, explicit error.
	limited := filepath.Join(t.TempDir(), "swival-limited")
	if err := os.WriteFile(limited, []byte("#!/bin/sh\ncat > /dev/null\necho partial\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a = &Adapter{Binary: limited}
	res, err = a.RunTurn(context.Background(), adapter.TurnRequest{Prompt: "x", WorkDir: repo}, func(adapter.Event) {})
	if err == nil || !strings.Contains(err.Error(), "turn limit") || res.ExitCode != 2 || res.FinalText != "partial" {
		t.Fatalf("turn limit: res=%+v err=%v", res, err)
	}

	// Exit code 1 = failure: stderr tail surfaced.
	bad := filepath.Join(t.TempDir(), "swival-bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\ncat > /dev/null\necho boom >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a = &Adapter{Binary: bad}
	res, err = a.RunTurn(context.Background(), adapter.TurnRequest{Prompt: "x", WorkDir: repo}, func(adapter.Event) {})
	if err == nil || res.ExitCode != 1 || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("bad binary: res=%+v err=%v", res, err)
	}
}
