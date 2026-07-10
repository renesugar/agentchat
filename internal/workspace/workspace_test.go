package workspace

import (
	"archive/zip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/agentchat/internal/adapter"
)

func ctxT(t *testing.T) context.Context { t.Helper(); return context.Background() }

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newUserRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "u@example.com")
	mustGit(t, dir, "config", "user.name", "u")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", "initial")
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScratchSnapshotDiffRestoreZip(t *testing.T) {
	ctx := ctxT(t)
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := m.CreateScratch(ctx, "My Project!")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Kind != KindScratch || !strings.Contains(filepath.Base(ws.Dir), "my-project") {
		t.Fatalf("workspace = %+v", ws)
	}

	// Turn 1: create a file.
	write(t, ws.Dir, "a.txt", "one\n")
	s1, err := ws.Snapshot(ctx, "turn 1")
	if err != nil {
		t.Fatal(err)
	}
	if !s1.Changed {
		t.Fatal("s1.Changed = false")
	}

	// Turn 2: modify + add.
	write(t, ws.Dir, "a.txt", "two\n")
	write(t, ws.Dir, "b.txt", "new\n")
	s2, err := ws.Snapshot(ctx, "turn 2")
	if err != nil {
		t.Fatal(err)
	}

	changes, err := ws.Diff(ctx, s1.Commit, s2.Commit)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]adapter.FileOp{}
	for _, c := range changes {
		byPath[c.Path] = c.Op
	}
	if byPath["a.txt"] != adapter.FileModified || byPath["b.txt"] != adapter.FileCreated {
		t.Fatalf("diff = %+v", changes)
	}

	// No-op turn: snapshot exists but reports Changed=false.
	s3, err := ws.Snapshot(ctx, "turn 3 (no-op)")
	if err != nil {
		t.Fatal(err)
	}
	if s3.Changed {
		t.Fatal("s3.Changed = true for identical tree")
	}

	// Restore to s1: a.txt back to "one", b.txt gone.
	if err := ws.Restore(ctx, s1.Commit); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws.Dir, "a.txt")); string(b) != "one\n" {
		t.Fatalf("a.txt after restore = %q", b)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("b.txt survived restore")
	}

	// Zip s2 and check contents.
	out := filepath.Join(t.TempDir(), "snap.zip")
	if err := ws.Zip(ctx, s2.Commit, out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["a.txt"] || !names["b.txt"] {
		t.Fatalf("zip contents = %v", names)
	}

	// Remove cleans up.
	if err := m.Remove(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Fatal("scratch dir survived Remove")
	}
}

// TestRepoSnapshotIsNonInvasive is the critical guarantee: snapshotting a
// user's repo must not move HEAD, touch their index, or alter the worktree
// — while still capturing untracked files and deletions.
func TestRepoSnapshotIsNonInvasive(t *testing.T) {
	ctx := ctxT(t)
	m, _ := NewManager(t.TempDir())
	repoDir := newUserRepo(t)

	// User state: staged change + untracked file.
	write(t, repoDir, "main.go", "package main // edited\n")
	mustGit(t, repoDir, "add", "main.go")
	write(t, repoDir, "untracked.txt", "scratch\n")

	headBefore := mustGit(t, repoDir, "rev-parse", "HEAD")
	statusBefore := mustGit(t, repoDir, "status", "--porcelain")

	ws, err := m.OpenRepo(ctx, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ws.Snapshot(ctx, "turn 1")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Changed {
		t.Fatal("Changed = false; dirty worktree should differ from HEAD")
	}

	// Nothing about the user's repo moved.
	if got := mustGit(t, repoDir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD moved: %s -> %s", headBefore, got)
	}
	if got := mustGit(t, repoDir, "status", "--porcelain"); got != statusBefore {
		t.Fatalf("status changed:\nbefore: %q\nafter:  %q", statusBefore, got)
	}

	// The snapshot captured the untracked file (diff vs HEAD shows it).
	changes, err := ws.Diff(ctx, headBefore, snap.Commit)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range changes {
		if c.Path == "untracked.txt" && c.Op == adapter.FileCreated {
			found = true
		}
	}
	if !found {
		t.Fatalf("untracked.txt not in snapshot diff: %+v", changes)
	}

	// A deletion is represented in the next snapshot.
	if err := os.Remove(filepath.Join(repoDir, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	snap2, err := ws.Snapshot(ctx, "turn 2")
	if err != nil {
		t.Fatal(err)
	}
	changes, err = ws.Diff(ctx, snap.Commit, snap2.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "untracked.txt" || changes[0].Op != adapter.FileDeleted {
		t.Fatalf("deletion diff = %+v", changes)
	}

	// Restore must refuse user repos.
	if err := ws.Restore(ctx, snap.Commit); err != ErrRestoreForbidden {
		t.Fatalf("Restore on repo kind: err = %v", err)
	}

	// Snapshot parent chain: snap2's parent is snap.
	if parent := mustGit(t, repoDir, "rev-parse", snap2.Commit+"^"); parent != snap.Commit {
		t.Fatalf("snap2 parent = %s, want %s", parent, snap.Commit)
	}
}

func TestWorktreeLifecycle(t *testing.T) {
	ctx := ctxT(t)
	m, _ := NewManager(t.TempDir())
	repoDir := newUserRepo(t)

	ws, err := m.CreateWorktree(ctx, repoDir, "Feature X")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Kind != KindWorktree || ws.RepoDir != repoDir || !strings.HasPrefix(ws.Branch, "agentchat/feature-x") {
		t.Fatalf("workspace = %+v", ws)
	}
	// It starts from the repo's HEAD content.
	if _, err := os.Stat(filepath.Join(ws.Dir, "main.go")); err != nil {
		t.Fatalf("worktree missing repo content: %v", err)
	}

	// Snapshots and restore work in the worktree without touching the repo.
	headBefore := mustGit(t, repoDir, "rev-parse", "HEAD")
	write(t, ws.Dir, "feature.go", "package main\n")
	s1, err := ws.Snapshot(ctx, "turn 1")
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws.Dir, "feature.go", "package main // v2\n")
	if err := ws.Restore(ctx, s1.Commit); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws.Dir, "feature.go")); string(b) != "package main\n" {
		t.Fatalf("feature.go after restore = %q", b)
	}
	if got := mustGit(t, repoDir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatal("repo HEAD moved by worktree activity")
	}

	if err := m.Remove(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Fatal("worktree dir survived Remove")
	}
}

func TestOpenRepoRejectsNonRepo(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	if _, err := m.OpenRepo(ctxT(t), t.TempDir()); err == nil {
		t.Fatal("OpenRepo accepted a plain directory")
	}
}
