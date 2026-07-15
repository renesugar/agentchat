package export

import (
	"archive/zip"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
		Client: "claude", Model: "sonnet", Provider: "openrouter", Effort: "high",
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

// TestTurnMarkdownEmbedded pins the per-turn copy contract: the full
// transcript embeds exactly TurnMarkdown's output for every turn, so the
// two renderings can never drift apart.
func TestTurnMarkdownEmbedded(t *testing.T) {
	ctx := context.Background()
	store, lib, convID := buildFixtureConversation(t)
	ex := &Exporter{Store: store, Library: lib}

	md, err := ex.Markdown(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	turns, err := store.ListTurns(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 {
		t.Fatal("fixture has no turns")
	}
	for _, turn := range turns {
		events, err := store.Events(ctx, convID, turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		section := TurnMarkdown(turn, events)
		if len(section) == 0 {
			t.Fatalf("turn %d: empty TurnMarkdown", turn.Seq)
		}
		if !strings.Contains(string(md), string(section)) {
			t.Errorf("turn %d: transcript does not embed TurnMarkdown output verbatim.\n--- section ---\n%s", turn.Seq, section)
		}
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

// TestBundleImportRoundTrip is the Step 15 contract: export → delete →
// import restores the conversation byte-identically, artifacts survive,
// and the bundled workspace tree is materialized and usable for a next
// turn.
func TestBundleImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, lib, convID := buildFixtureConversation(t)

	mgr, err := workspace.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := mgr.CreateScratch(ctx, "roundtrip")
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
	bundlePath := filepath.Join(t.TempDir(), "bundle.zip")
	if err := ex.Bundle(ctx, convID, ws, bundlePath); err != nil {
		t.Fatal(err)
	}

	origTree := readDirTree(t, store.ConversationDir(convID))
	origArts, err := lib.List(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	origRecords := map[string][]byte{}
	for _, a := range origArts {
		rec, err := lib.ExportRecord(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		origRecords[a.ID] = rec
	}

	if err := store.DeleteConversation(ctx, convID); err != nil {
		t.Fatal(err)
	}

	conv, restoredWS, err := Import(ctx, store, lib, mgr, bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ID != convID {
		t.Fatalf("imported conversation id = %q, want %q", conv.ID, convID)
	}

	// Store subtree restored byte-identically.
	if got := readDirTree(t, store.ConversationDir(convID)); !reflect.DeepEqual(got, origTree) {
		t.Fatalf("restored subtree differs from original")
	}
	// Artifact records untouched/byte-identical (they survive deletion,
	// so import skips them).
	for id, orig := range origRecords {
		got, err := lib.ExportRecord(ctx, id)
		if err != nil {
			t.Fatalf("artifact %s gone after import: %v", id, err)
		}
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("artifact record %s changed across round trip", id)
		}
	}

	// Workspace materialized with the snapshot tree, usable for a next
	// turn: the store's sequence continues and the snapshot chain
	// extends in the new location. (Exercised via store + workspace
	// directly — importing engine here would create a test-only import
	// cycle, since engine renders MCP context through this package.)
	if restoredWS == nil {
		t.Fatal("import returned no workspace despite workspace.zip")
	}
	if b, err := os.ReadFile(filepath.Join(restoredWS.Dir, "hello.py")); err != nil || string(b) != "print('hi')\n" {
		t.Fatalf("restored workspace tree: %q, %v", b, err)
	}
	turn, err := store.BeginTurn(ctx, convID, transcript.NewTurn{
		Client: "echo", Prompt: "continue", WorkspaceRef: restoredWS.Dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Seq != 3 {
		t.Fatalf("next turn seq = %d, want 3", turn.Seq)
	}
	snap, err := restoredWS.Snapshot(ctx, "post-import")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishTurn(ctx, convID, turn.ID, &adapter.Result{ExitCode: 0}, snap.Commit, nil); err != nil {
		t.Fatal(err)
	}

	// The imported-workspace link artifact records the new location.
	arts, err := lib.List(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range arts {
		if a.Origin == "import" && a.LocalPath == restoredWS.Dir {
			found = true
		}
	}
	if !found {
		t.Error("no imported-workspace link artifact recorded")
	}
}

// TestImportFreshMachine imports a bundle into empty store/library/manager
// (another user's machine): everything is re-created, and file artifacts
// sharing content land as ONE blob in the CAS.
func TestImportFreshMachine(t *testing.T) {
	ctx := context.Background()
	store, lib, convID := buildFixtureConversation(t)

	// A second file artifact with identical content, to observe dedupe.
	if _, err := lib.AddFile(ctx, "notes-copy.md", strings.NewReader("remember the docstring\n"),
		artifact.Meta{ConversationID: convID, Origin: "user-upload"}); err != nil {
		t.Fatal(err)
	}

	ex := &Exporter{Store: store, Library: lib}
	bundlePath := filepath.Join(t.TempDir(), "bundle.zip")
	if err := ex.Bundle(ctx, convID, nil, bundlePath); err != nil {
		t.Fatal(err)
	}

	freshStore, err := transcript.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	freshLib, err := artifact.NewLibrary(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conv, ws, err := Import(ctx, freshStore, freshLib, nil, bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if ws != nil {
		t.Fatal("workspace returned despite bundling without one")
	}
	if conv.Title != "Fix the greeting" {
		t.Fatalf("imported title = %q", conv.Title)
	}

	// Turns and events byte-identical to the source store.
	if got, want := readDirTree(t, freshStore.ConversationDir(convID)), readDirTree(t, store.ConversationDir(convID)); !reflect.DeepEqual(got, want) {
		t.Fatal("imported subtree differs from source store")
	}

	// All three artifact records restored; the two identical files share
	// one CAS blob.
	arts, err := freshLib.List(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 3 {
		t.Fatalf("restored %d artifacts, want 3", len(arts))
	}
	blobs := 0
	err = filepath.WalkDir(filepath.Join(freshLib.Root(), "cas"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if blobs != 1 {
		t.Fatalf("CAS has %d blobs, want 1 (dedupe)", blobs)
	}
}

func TestImportCollision(t *testing.T) {
	ctx := context.Background()
	store, lib, convID := buildFixtureConversation(t)
	ex := &Exporter{Store: store, Library: lib}
	bundlePath := filepath.Join(t.TempDir(), "bundle.zip")
	if err := ex.Bundle(ctx, convID, nil, bundlePath); err != nil {
		t.Fatal(err)
	}

	before := readDirTree(t, store.ConversationDir(convID))
	_, _, err := Import(ctx, store, lib, nil, bundlePath)
	if err == nil {
		t.Fatal("import over existing conversation succeeded")
	}
	if !strings.Contains(err.Error(), "Fix the greeting") {
		t.Errorf("collision error should name the existing conversation: %v", err)
	}
	if got := readDirTree(t, store.ConversationDir(convID)); !reflect.DeepEqual(got, before) {
		t.Error("store changed on refused import")
	}
}

func TestImportOldBundle(t *testing.T) {
	// A pre-Step-15 bundle: transcript.md but no bundle.json.
	path := filepath.Join(t.TempDir(), "old.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	if err := writeZipFile(zw, "transcript.md", []byte("# old\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	store, err := transcript.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Import(context.Background(), store, nil, nil, path)
	if err == nil || !strings.Contains(err.Error(), "predates import support") {
		t.Fatalf("old bundle err = %v, want a clear rejection", err)
	}
}

// readDirTree maps relative path -> content for every file under root.
func readDirTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
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
