package transcript_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/adapters/echo"
	"github.com/example/agentchat/internal/engine"
	"github.com/example/agentchat/internal/transcript"
)

func newStore(t *testing.T) *transcript.FSStore {
	t.Helper()
	s, err := transcript.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConversationRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	c, err := s.CreateConversation(ctx, transcript.NewConversation{
		Title: "demo", ProjectPath: "/tmp/repo",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetConversation(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "demo" || got.ProjectPath != "/tmp/repo" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if _, err := s.GetConversation(ctx, "missing"); !errors.Is(err, transcript.ErrNotFound) {
		t.Fatalf("missing conversation err = %v, want ErrNotFound", err)
	}

	list, err := s.ListConversations(ctx)
	if err != nil || len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("ListConversations = %v, %v", list, err)
	}
}

func TestSetConversationProject(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	c, err := s.CreateConversation(ctx, transcript.NewConversation{Title: "wanderer"})
	if err != nil {
		t.Fatal(err)
	}
	before := c.UpdatedAt

	got, err := s.SetConversationProject(ctx, c.ID, "/home/u/proj")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectPath != "/home/u/proj" {
		t.Fatalf("ProjectPath = %q", got.ProjectPath)
	}
	if !got.UpdatedAt.After(before) && !got.UpdatedAt.Equal(before) {
		t.Fatalf("UpdatedAt went backwards: %v -> %v", before, got.UpdatedAt)
	}
	// The change persists.
	if re, err := s.GetConversation(ctx, c.ID); err != nil || re.ProjectPath != "/home/u/proj" {
		t.Fatalf("reloaded = %+v, %v", re, err)
	}

	// Empty path detaches.
	if got, err = s.SetConversationProject(ctx, c.ID, ""); err != nil || got.ProjectPath != "" {
		t.Fatalf("detach = %+v, %v", got, err)
	}

	if _, err := s.SetConversationProject(ctx, "missing", "/x"); !errors.Is(err, transcript.ErrNotFound) {
		t.Fatalf("missing conversation err = %v, want ErrNotFound", err)
	}
}

func TestDeleteAndImportConversation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	c, err := s.CreateConversation(ctx, transcript.NewConversation{Title: "doomed"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.BeginTurn(ctx, c.ID, transcript.NewTurn{Client: "echo", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, c.ID, turn.ID, adapter.Event{Kind: adapter.EventText, Text: "yo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishTurn(ctx, c.ID, turn.ID, &adapter.Result{ExitCode: 0}, "", nil); err != nil {
		t.Fatal(err)
	}

	// Importing over an existing conversation is refused.
	if err := s.ImportConversation(ctx, c.ID, os.DirFS(s.ConversationDir(c.ID))); err == nil {
		t.Fatal("import over existing conversation succeeded")
	}

	// Capture the raw subtree, delete, and restore it byte-identically.
	snapshot := readTree(t, s.ConversationDir(c.ID))
	if err := s.DeleteConversation(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversation(ctx, c.ID); !errors.Is(err, transcript.ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteConversation(ctx, c.ID); !errors.Is(err, transcript.ErrNotFound) {
		t.Fatalf("double delete err = %v, want ErrNotFound", err)
	}

	src := t.TempDir()
	for rel, content := range snapshot {
		path := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ImportConversation(ctx, c.ID, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, s.ConversationDir(c.ID)); !reflect.DeepEqual(got, snapshot) {
		t.Fatalf("restored subtree differs:\n got %v\nwant %v", keysOf(got), keysOf(snapshot))
	}

	// A mismatched ID is refused and leaves nothing behind.
	if err := s.ImportConversation(ctx, "other-id", os.DirFS(src)); err == nil {
		t.Fatal("import with mismatched id succeeded")
	}
	if _, err := os.Stat(s.ConversationDir("other-id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed import left a partial conversation dir")
	}
}

// readTree maps relative path -> content for every regular file under root.
func readTree(t *testing.T, root string) map[string][]byte {
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

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestTurnLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	c, _ := s.CreateConversation(ctx, transcript.NewConversation{Title: "t"})

	turn, err := s.BeginTurn(ctx, c.ID, transcript.NewTurn{
		Client: "echo", Model: "echo-1", Prompt: "hi", WorkspaceRef: "/ws",
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Seq != 1 || turn.Status != transcript.TurnRunning {
		t.Fatalf("begin: %+v", turn)
	}

	events := []adapter.Event{
		{Kind: adapter.EventText, Text: "hello"},
		{Kind: adapter.EventFileChange, File: &adapter.FileChange{Path: "a.go", Op: adapter.FileCreated}},
	}
	for _, e := range events {
		if err := s.AppendEvent(ctx, c.ID, turn.ID, e); err != nil {
			t.Fatal(err)
		}
	}

	res := &adapter.Result{ExitCode: 0, FinalText: "hello"}
	done, err := s.FinishTurn(ctx, c.ID, turn.ID, res, "snap-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != transcript.TurnDone || done.EndedAt.IsZero() || done.Result == nil || done.SnapshotID != "snap-1" {
		t.Fatalf("finish: %+v", done)
	}

	// Second turn gets Seq 2; a failing finish records the error.
	t2, _ := s.BeginTurn(ctx, c.ID, transcript.NewTurn{Client: "echo", Prompt: "again"})
	if t2.Seq != 2 {
		t.Fatalf("seq = %d, want 2", t2.Seq)
	}
	failed, err := s.FinishTurn(ctx, c.ID, t2.ID, nil, "", errors.New("boom"))
	if err != nil || failed.Status != transcript.TurnFailed || failed.Error != "boom" {
		t.Fatalf("failed turn: %+v, err=%v", failed, err)
	}

	turns, err := s.ListTurns(ctx, c.ID)
	if err != nil || len(turns) != 2 || turns[0].Seq != 1 || turns[1].Seq != 2 {
		t.Fatalf("ListTurns = %+v, %v", turns, err)
	}

	got, err := s.Events(ctx, c.ID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != adapter.EventText || got[1].File == nil || got[1].File.Path != "a.go" {
		t.Fatalf("Events = %+v", got)
	}

	// Conversation UpdatedAt advanced past CreatedAt.
	cc, _ := s.GetConversation(ctx, c.ID)
	if !cc.UpdatedAt.After(cc.CreatedAt) && !cc.UpdatedAt.Equal(done.EndedAt) {
		t.Fatalf("UpdatedAt not bumped: %+v", cc)
	}
}

func TestEngineRunTurnPersists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	reg := adapter.NewRegistry()
	reg.Register(echo.New())
	eng := engine.New(reg, s)

	c, _ := s.CreateConversation(ctx, transcript.NewConversation{Title: "engine"})

	var live int
	turn, err := eng.RunTurn(ctx, c.ID, "echo", nil, adapter.TurnRequest{
		Prompt:  "persist me",
		WorkDir: t.TempDir(),
		Model:   "echo-1",
	}, func(adapter.Event) { live++ })
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if turn.Status != transcript.TurnDone || turn.Result == nil || turn.Result.ExitCode != 0 {
		t.Fatalf("turn: %+v", turn)
	}

	stored, err := s.Events(ctx, c.ID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 || len(stored) != live {
		t.Fatalf("stored %d events, tapped %d", len(stored), live)
	}
	if last := stored[len(stored)-1]; last.Kind != adapter.EventResult {
		t.Fatalf("last stored event = %+v, want result", last)
	}

	if _, err := eng.RunTurn(ctx, c.ID, "nope", nil, adapter.TurnRequest{Prompt: "x", WorkDir: t.TempDir()}, nil); !errors.Is(err, adapter.ErrUnknownAdapter) {
		t.Fatalf("unknown client err = %v", err)
	}
}
