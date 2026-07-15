package artifact_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/renesugar/agentchat/internal/artifact"
)

func newLib(t *testing.T) *artifact.Library {
	t.Helper()
	l, err := artifact.NewLibrary(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddFileRoundTripAndDedup(t *testing.T) {
	ctx := context.Background()
	l := newLib(t)

	a1, err := l.AddFile(ctx, "report.md", strings.NewReader("# hello\n"),
		artifact.Meta{ConversationID: "c1", TurnID: "t1", Origin: "client-output"})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Kind != artifact.KindFile || a1.Size != 8 || a1.SHA256 == "" {
		t.Fatalf("artifact = %+v", a1)
	}
	if a1.MediaType == "" || !strings.Contains(a1.MediaType, "markdown") {
		t.Errorf("MediaType = %q", a1.MediaType)
	}
	if got := readAll(t, mustOpen(t, l, a1.ID)); got != "# hello\n" {
		t.Fatalf("content = %q", got)
	}

	// Same content again: new record, same blob (deduplicated).
	a2, err := l.AddFile(ctx, "copy.md", strings.NewReader("# hello\n"), artifact.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if a2.ID == a1.ID {
		t.Fatal("records not distinct")
	}
	if a2.SHA256 != a1.SHA256 {
		t.Fatal("hashes differ for identical content")
	}
	p1, _ := l.BlobPath(ctx, a1.ID)
	p2, _ := l.BlobPath(ctx, a2.ID)
	if p1 != p2 {
		t.Fatalf("blobs not shared: %s vs %s", p1, p2)
	}

	// GC: deleting one record keeps the blob; deleting both removes it.
	if err := l.Delete(ctx, a1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p1); err != nil {
		t.Fatal("blob GCed while still referenced")
	}
	if err := l.Delete(ctx, a2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p1); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("blob survived last delete")
	}
	if _, err := l.Get(ctx, a1.ID); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("deleted record err = %v", err)
	}
}

func mustOpen(t *testing.T, l *artifact.Library, id string) io.ReadCloser {
	t.Helper()
	rc, err := l.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func TestLinks(t *testing.T) {
	ctx := context.Background()
	l := newLib(t)

	// Link to an existing local repo path.
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := l.AddLink(ctx, "big-repo", repo, "https://github.com/example/big-repo", artifact.Meta{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != artifact.KindLink || a.LocalPath != repo {
		t.Fatalf("link = %+v", a)
	}

	// Opening a link whose local path is a file works; a dangling link
	// mentions the remote fallback.
	fileLink, err := l.AddLink(ctx, "notes", filepath.Join(repo, "f.txt"), "", artifact.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, mustOpen(t, l, fileLink.ID)); got != "x" {
		t.Fatalf("link content = %q", got)
	}

	dangling, err := l.AddLink(ctx, "gone", filepath.Join(repo, "missing.bin"),
		"https://git.example.com/archive.zip", artifact.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Open(ctx, dangling.ID); err == nil || !strings.Contains(err.Error(), "git.example.com") {
		t.Fatalf("dangling link err = %v", err)
	}

	// A link with neither path nor URL is rejected.
	if _, err := l.AddLink(ctx, "empty", "", "", artifact.Meta{}); err == nil {
		t.Fatal("empty link accepted")
	}

	// BlobPath is meaningless for links.
	if _, err := l.BlobPath(ctx, a.ID); err == nil {
		t.Fatal("BlobPath succeeded for a link")
	}
}

func TestListFiltersByConversation(t *testing.T) {
	ctx := context.Background()
	l := newLib(t)

	mk := func(name, conv string) {
		if _, err := l.AddFile(ctx, name, strings.NewReader(name), artifact.Meta{ConversationID: conv}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a.txt", "c1")
	mk("b.txt", "c2")
	mk("c.txt", "c1")

	all, err := l.List(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("List all = %d, %v", len(all), err)
	}
	c1, err := l.List(ctx, "c1")
	if err != nil || len(c1) != 2 {
		t.Fatalf("List c1 = %d, %v", len(c1), err)
	}
	for _, a := range c1 {
		if a.ConversationID != "c1" {
			t.Fatalf("filter leak: %+v", a)
		}
	}
}
