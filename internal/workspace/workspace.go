// Package workspace manages the directories coding clients work in and the
// per-turn snapshots that make multi-client handoffs auditable.
//
// Workspace kinds:
//
//   - repo: an existing local git repo owned by the user. Snapshots MUST
//     NOT disturb it, so they are taken through a temporary index:
//     git add -A (into the temp index) → write-tree → commit-tree →
//     update-ref refs/agentchat/snapshots/<n>. HEAD, the user's index,
//     branches, and worktree are never touched; the ref keeps the snapshot
//     objects alive across gc.
//   - worktree: a git worktree created from a repo on a dedicated
//     agentchat/<name> branch, so parallel conversations don't fight over
//     one checkout.
//   - scratch: a conversation with no repo association gets a git-inited
//     directory under the manager root; ZIP export of any snapshot makes
//     the "project in a ZIP per turn" flow work.
//
// The same snapshot mechanism is used for all kinds (uniform and
// non-invasive everywhere). Diffs between any two snapshots come from
// `git diff --name-status -M`. Restore is offered only for owned kinds
// (worktree, scratch), never for the user's own repo.
//
// Everything shells out to the git binary; no cgo, no go-git.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// Kind classifies a workspace.
type Kind string

const (
	KindRepo     Kind = "repo"
	KindWorktree Kind = "worktree"
	KindScratch  Kind = "scratch"
)

// ErrNotARepo is returned by OpenRepo for directories that aren't inside a
// git work tree.
var ErrNotARepo = errors.New("workspace: not a git repository")

// ErrRestoreForbidden is returned when Restore is called on a user-owned
// repo workspace.
var ErrRestoreForbidden = errors.New("workspace: restore is only allowed on worktree/scratch workspaces")

// snapshotIdentity is the committer/author used for snapshot commit
// objects, independent of the user's git config.
var snapshotIdentity = []string{
	"GIT_AUTHOR_NAME=agentchat", "GIT_AUTHOR_EMAIL=agentchat@localhost",
	"GIT_COMMITTER_NAME=agentchat", "GIT_COMMITTER_EMAIL=agentchat@localhost",
}

// Workspace is a directory a coding client runs in.
type Workspace struct {
	Kind Kind   `json:"kind"`
	Dir  string `json:"dir"`
	// RepoDir is the originating repo for worktree workspaces.
	RepoDir string `json:"repo_dir,omitempty"`
	// Branch is the dedicated branch for worktree workspaces.
	Branch string `json:"branch,omitempty"`
}

// Snapshot pins the exact workspace contents after a turn.
type Snapshot struct {
	// Commit is the snapshot commit object (dangling from branches, kept
	// alive by Ref). Diff, Restore, and Zip take these.
	Commit string    `json:"commit"`
	Tree   string    `json:"tree"`
	Ref    string    `json:"ref"`
	Label  string    `json:"label"`
	Time   time.Time `json:"time"`
	// Changed is false when the tree is identical to the previous
	// snapshot (or HEAD when there is no previous snapshot).
	Changed bool `json:"changed"`
}

// Manager creates and opens workspaces under a root directory (worktrees
// and scratch dirs live there; repo workspaces stay where they are).
type Manager struct {
	root string
}

// NewManager creates (if needed) and opens a manager rooted at dir.
func NewManager(dir string) (*Manager, error) {
	for _, sub := range []string{"scratch", "worktrees"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("workspace: init %s: %w", dir, err)
		}
	}
	return &Manager{root: dir}, nil
}

// OpenRepo wraps an existing local git repo as a workspace.
func (m *Manager) OpenRepo(ctx context.Context, dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	out, err := runGit(ctx, abs, nil, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		return nil, fmt.Errorf("%w: %s", ErrNotARepo, abs)
	}
	return &Workspace{Kind: KindRepo, Dir: abs}, nil
}

// CreateWorktree makes a git worktree of repoDir on a fresh
// agentchat/<name> branch starting at the repo's current HEAD.
func (m *Manager) CreateWorktree(ctx context.Context, repoDir, name string) (*Workspace, error) {
	repo, err := m.OpenRepo(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	name = sanitize(name) + "-" + shortID()
	branch := "agentchat/" + name
	path := filepath.Join(m.root, "worktrees", name)
	if _, err := runGit(ctx, repo.Dir, nil, "worktree", "add", "-b", branch, path); err != nil {
		return nil, fmt.Errorf("workspace: creating worktree: %w", err)
	}
	return &Workspace{Kind: KindWorktree, Dir: path, RepoDir: repo.Dir, Branch: branch}, nil
}

// CreateScratch makes a fresh git-inited directory for a conversation with
// no repo association. HEAD exists from an initial empty commit so the
// first turn snapshot has a parent and diffs work from turn one.
func (m *Manager) CreateScratch(ctx context.Context, name string) (*Workspace, error) {
	dir := filepath.Join(m.root, "scratch", sanitize(name)+"-"+shortID())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	steps := [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=agentchat", "-c", "user.email=agentchat@localhost",
			"commit", "-q", "--allow-empty", "-m", "agentchat: scratch workspace"},
	}
	for _, args := range steps {
		if _, err := runGit(ctx, dir, snapshotIdentity, args...); err != nil {
			return nil, fmt.Errorf("workspace: init scratch: %w", err)
		}
	}
	return &Workspace{Kind: KindScratch, Dir: dir}, nil
}

// Remove deletes an owned workspace (worktree or scratch). Repo workspaces
// are the user's; Remove refuses them.
func (m *Manager) Remove(ctx context.Context, ws *Workspace) error {
	switch ws.Kind {
	case KindWorktree:
		if _, err := runGit(ctx, ws.RepoDir, nil, "worktree", "remove", "--force", ws.Dir); err != nil {
			return err
		}
		return nil
	case KindScratch:
		return os.RemoveAll(ws.Dir)
	default:
		return fmt.Errorf("workspace: refusing to remove %s workspace %s", ws.Kind, ws.Dir)
	}
}

// Snapshot records the exact current contents of the workspace (tracked
// and untracked files, .gitignore respected) without touching HEAD, the
// user's index, or the working tree. label becomes the commit message.
func (ws *Workspace) Snapshot(ctx context.Context, label string) (*Snapshot, error) {
	tmpIdx, err := os.CreateTemp("", "agentchat-index-*")
	if err != nil {
		return nil, err
	}
	tmpIdx.Close()
	os.Remove(tmpIdx.Name()) // git wants to create it itself
	defer os.Remove(tmpIdx.Name())
	env := append([]string{"GIT_INDEX_FILE=" + tmpIdx.Name()}, snapshotIdentity...)

	// Populate the temp index from HEAD (so deletions are represented),
	// then overlay the working tree.
	if head, err := runGit(ctx, ws.Dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err == nil && strings.TrimSpace(head) != "" {
		if _, err := runGit(ctx, ws.Dir, env, "read-tree", strings.TrimSpace(head)); err != nil {
			return nil, fmt.Errorf("workspace: read-tree: %w", err)
		}
	}
	if _, err := runGit(ctx, ws.Dir, env, "add", "-A"); err != nil {
		return nil, fmt.Errorf("workspace: staging snapshot: %w", err)
	}
	treeOut, err := runGit(ctx, ws.Dir, env, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("workspace: write-tree: %w", err)
	}
	tree := strings.TrimSpace(treeOut)

	// Parent: the latest agentchat snapshot if any, else HEAD if any.
	parent := ws.latestSnapshotCommit(ctx)
	if parent == "" {
		if head, err := runGit(ctx, ws.Dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
			parent = strings.TrimSpace(head)
		}
	}

	changed := true
	if parent != "" {
		if pt, err := runGit(ctx, ws.Dir, nil, "rev-parse", parent+"^{tree}"); err == nil &&
			strings.TrimSpace(pt) == tree {
			changed = false
		}
	}

	args := []string{"commit-tree", tree, "-m", label}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commitOut, err := runGit(ctx, ws.Dir, snapshotIdentity, args...)
	if err != nil {
		return nil, fmt.Errorf("workspace: commit-tree: %w", err)
	}
	commit := strings.TrimSpace(commitOut)

	ref := fmt.Sprintf("refs/agentchat/snapshots/%d-%s", time.Now().UnixNano(), shortID())
	if _, err := runGit(ctx, ws.Dir, nil, "update-ref", ref, commit); err != nil {
		return nil, fmt.Errorf("workspace: update-ref: %w", err)
	}

	return &Snapshot{
		Commit: commit, Tree: tree, Ref: ref, Label: label,
		Time: time.Now().UTC(), Changed: changed,
	}, nil
}

// LatestSnapshot returns the newest agentchat snapshot commit in the
// workspace, or "" if none exist.
func (ws *Workspace) LatestSnapshot(ctx context.Context) string {
	return ws.latestSnapshotCommit(ctx)
}

// latestSnapshotCommit returns the newest refs/agentchat/snapshots commit,
// or "" if none exist. Ref names embed a nanosecond timestamp, so the
// lexically greatest name is the newest.
func (ws *Workspace) latestSnapshotCommit(ctx context.Context) string {
	out, err := runGit(ctx, ws.Dir, nil, "for-each-ref",
		"--format=%(refname) %(objectname)", "refs/agentchat/snapshots/")
	if err != nil {
		return ""
	}
	latestName, latestCommit := "", ""
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[0] > latestName {
			latestName, latestCommit = fields[0], fields[1]
		}
	}
	return latestCommit
}

// Diff lists the file changes between two snapshot commits (or any two
// commits/trees resolvable in the workspace).
func (ws *Workspace) Diff(ctx context.Context, from, to string) ([]adapter.FileChange, error) {
	out, err := runGit(ctx, ws.Dir, nil, "diff", "--name-status", "-M", from, to)
	if err != nil {
		return nil, err
	}
	var changes []adapter.FileChange
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		fc := adapter.FileChange{Path: fields[1]}
		switch fields[0][0] {
		case 'A':
			fc.Op = adapter.FileCreated
		case 'D':
			fc.Op = adapter.FileDeleted
		case 'R':
			fc.Op = adapter.FileRenamed
			if len(fields) >= 3 {
				fc.OldPath = fields[1]
				fc.Path = fields[2]
			}
		default:
			fc.Op = adapter.FileModified
		}
		changes = append(changes, fc)
	}
	return changes, nil
}

// Restore resets an OWNED workspace's working tree to a snapshot commit:
// tracked content is checked out from the snapshot and files that didn't
// exist in it are cleaned. Refuses to operate on user repos.
func (ws *Workspace) Restore(ctx context.Context, commit string) error {
	if ws.Kind == KindRepo {
		return ErrRestoreForbidden
	}
	// Point the index + worktree at the snapshot tree, then drop anything
	// untracked relative to it. HEAD/branch stay where they are.
	if _, err := runGit(ctx, ws.Dir, nil, "restore", "--source", commit, "--staged", "--worktree", "--", "."); err != nil {
		return fmt.Errorf("workspace: restore: %w", err)
	}
	if _, err := runGit(ctx, ws.Dir, nil, "clean", "-fd"); err != nil {
		return fmt.Errorf("workspace: clean: %w", err)
	}
	return nil
}

// Zip writes a snapshot commit's tree to a ZIP file at outPath, ready for
// download or for seeding another tool.
func (ws *Workspace) Zip(ctx context.Context, commit, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if _, err := runGit(ctx, ws.Dir, nil, "archive", "--format=zip", "-o", outPath, commit); err != nil {
		return fmt.Errorf("workspace: archive: %w", err)
	}
	return nil
}

// --- helpers ---

func runGit(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

func sanitize(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '/':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "ws"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func shortID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("workspace: rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}
