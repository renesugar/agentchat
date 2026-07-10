package export

import (
	"archive/zip"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/artifact"
	"github.com/example/agentchat/internal/transcript"
	"github.com/example/agentchat/internal/workspace"
)

var update = flag.Bool("update", false, "rewrite golden files")

// buildFixtureConversation assembles a two-turn conversation (one success
// with plan/files/usage, one failure) plus artifacts, directly through the
// store so the content is fully controlled.
func buildFixtureConversation(t *testing.T) (*transcript.FSStore, *artifact.Library, string) {
	t.Helper()
	ctx := context.Background()
	store, err := transcript.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lib, err := artifact.NewLibrary(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	conv, err := store.CreateConversation(ctx, transcript.NewConversation{
		Title: "Fix the greeting", ProjectPath: "/home/user/proj",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1: success with plan, text, file change, usage, snapshot.
	t1, _ := store.BeginTurn(ctx, conv.ID, transcript.NewTurn{
		Client: "claude", Model: "sonnet", Effort: "high",
		Prompt:       "Personalize the greeting.\nKeep it short.",
		WorkspaceRef: "/home/user/proj",
	})
	for _, ev := range []adapter.Event{
		{Kind: adapter.EventPlan, Text: "[x] Edit hello.py\n[x] Verify"},
		{Kind: adapter.EventToolUse, Tool: &adapter.ToolInfo{Name: "Edit", Input: "hello.py"}},
		{Kind: adapter.EventText, Text: "I updated the greeting to take a name."},
		{Kind: adapter.EventText, Text: "Verified with `python3 hello.py`."},
	} {
		if err := store.AppendEvent(ctx, conv.ID, t1.ID, ev); err != nil {
			t.Fatal(err)
		}
	}
	res1 := &adapter.Result{
		SessionID: "sess-1", ExitCode: 0, FinalText: "done",
		FilesChanged: []adapter.FileChange{{Path: "hello.py", Op: adapter.FileModified}},
		Usage:        adapter.Usage{InputTokens: 1200, OutputTokens: 340, CostUSD: 0.0123},
	}
	if _, err := store.FinishTurn(ctx, conv.ID, t1.ID, res1, "0123456789abcdef0123456789abcdef01234567", nil); err != nil {
		t.Fatal(err)
	}

	// Turn 2: failure.
	t2, _ := store.BeginTurn(ctx, conv.ID, transcript.NewTurn{
		Client: "codex", Prompt: "Now add tests.",
	})
	_ = store.AppendEvent(ctx, conv.ID, t2.ID, adapter.Event{Kind: adapter.EventError, Text: "stream disconnected"})
	if _, err := store.FinishTurn(ctx, conv.ID, t2.ID, nil, "", errors.New("codex: turn failed")); err != nil {
		t.Fatal(err)
	}

	// Artifacts: one stored file, one link.
	if _, err := lib.AddFile(ctx, "notes.md", strings.NewReader("remember the docstring\n"),
		artifact.Meta{ConversationID: conv.ID, Origin: "user-upload"}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.AddLink(ctx, "big-repo", "/home/user/proj", "https://github.com/example/proj",
		artifact.Meta{ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}

	return store, lib, conv.ID
}

var (
	idRe   = regexp.MustCompile(`\d{8}T\d{6}-[0-9a-f]{8}`)
	dateRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2} [A-Z]+`)
	shaRe  = regexp.MustCompile(`sha256 [0-9a-f]{12}…`)
)

// normalize replaces run-dependent values (IDs, dates, hashes) so the
// golden file is stable.
func normalize(s string) string {
	s = idRe.ReplaceAllString(s, "ID")
	s = dateRe.ReplaceAllString(s, "DATE")
	s = shaRe.ReplaceAllString(s, "sha256 HASH…")
	return s
}

func TestMarkdownGolden(t *testing.T) {
	store, lib, convID := buildFixtureConversation(t)
	ex := &Exporter{Store: store, Library: lib}

	md, err := ex.Markdown(context.Background(), convID)
	if err != nil {
		t.Fatal(err)
	}
	got := normalize(string(md))

	golden := filepath.Join("testdata", "transcript.golden.md")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("markdown mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBundle(t *testing.T) {
	ctx := context.Background()
	store, lib, convID := buildFixtureConversation(t)

	// A real workspace with one snapshot so workspace.zip is included.
	mgr, err := workspace.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := mgr.CreateScratch(ctx, "bundle")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "hello.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Snapshot(ctx, "turn 1"); err != nil {
		t.Fatal(err)
	}

	ex := &Exporter{Store: store, Library: lib}
	out := filepath.Join(t.TempDir(), "bundle.zip")
	if err := ex.Bundle(ctx, convID, ws, out); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}

	if files["transcript.md"] == nil {
		t.Fatal("bundle missing transcript.md")
	}
	if files["artifacts/links.md"] == nil {
		t.Fatal("bundle missing artifacts/links.md")
	}
	if files["workspace.zip"] == nil || files["workspace.txt"] == nil {
		t.Fatalf("bundle missing workspace snapshot; entries: %v", keys(files))
	}
	var notes string
	for name, f := range files {
		if strings.HasPrefix(name, "artifacts/") && strings.HasSuffix(name, "-notes.md") {
			notes = readZip(t, f)
		}
	}
	if notes != "remember the docstring\n" {
		t.Fatalf("stored artifact content = %q", notes)
	}
	if !strings.Contains(readZip(t, files["artifacts/links.md"]), "github.com/example/proj") {
		t.Fatal("links.md missing remote URL")
	}

	// The nested workspace.zip round-trips.
	wsBytes := readZip(t, files["workspace.zip"])
	inner, err := zip.NewReader(strings.NewReader(wsBytes), int64(len(wsBytes)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range inner.File {
		if f.Name == "hello.py" {
			found = true
		}
	}
	if !found {
		t.Fatal("workspace.zip missing hello.py")
	}
}

func keys(m map[string]*zip.File) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func readZip(t *testing.T, f *zip.File) string {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
