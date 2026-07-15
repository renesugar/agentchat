package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// FSStore is a Store backed by plain files:
//
//	<root>/conversations/<convID>/conversation.json
//	<root>/conversations/<convID>/turns/<seq>-<turnID>/turn.json
//	<root>/conversations/<convID>/turns/<seq>-<turnID>/events.jsonl
//
// Human-inspectable, diff-friendly, and trivially portable. A single
// process-wide mutex serializes writes; this app is a single-user desktop
// tool, so contention is not a concern at this layer.
type FSStore struct {
	root string
	mu   sync.Mutex
	now  func() time.Time // injectable for tests
}

var _ Store = (*FSStore)(nil)

// NewFSStore creates (if needed) and opens a store rooted at dir.
func NewFSStore(dir string) (*FSStore, error) {
	if err := os.MkdirAll(filepath.Join(dir, "conversations"), 0o755); err != nil {
		return nil, fmt.Errorf("transcript: init %s: %w", dir, err)
	}
	return &FSStore{root: dir, now: time.Now}, nil
}

// Root returns the store's root directory.
func (s *FSStore) Root() string { return s.root }

func (s *FSStore) convDir(convID string) string {
	return filepath.Join(s.root, "conversations", convID)
}

// CreateConversation implements Store.
func (s *FSStore) CreateConversation(ctx context.Context, nc NewConversation) (*Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	c := &Conversation{
		ID:          newID(now),
		Title:       nc.Title,
		ProjectPath: nc.ProjectPath,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if c.Title == "" {
		c.Title = "Untitled " + now.Format("2006-01-02 15:04")
	}
	dir := s.convDir(c.ID)
	if err := os.MkdirAll(filepath.Join(dir, "turns"), 0o755); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "conversation.json"), c); err != nil {
		return nil, err
	}
	return c, nil
}

// GetConversation implements Store.
func (s *FSStore) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	var c Conversation
	if err := readJSON(filepath.Join(s.convDir(id), "conversation.json"), &c); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: conversation %q", ErrNotFound, id)
		}
		return nil, err
	}
	return &c, nil
}

// ListConversations implements Store; newest UpdatedAt first.
func (s *FSStore) ListConversations(ctx context.Context) ([]*Conversation, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "conversations"))
	if err != nil {
		return nil, err
	}
	var out []*Conversation
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := s.GetConversation(ctx, e.Name())
		if err != nil {
			continue // skip damaged entries rather than failing the list
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// BeginTurn implements Store.
func (s *FSStore) BeginTurn(ctx context.Context, convID string, nt NewTurn) (*Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.GetConversation(ctx, convID); err != nil {
		return nil, err
	}
	seq, err := s.nextSeq(convID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	t := &Turn{
		ID:             newID(now),
		ConversationID: convID,
		Seq:            seq,
		Client:         nt.Client,
		Model:          nt.Model,
		Provider:       nt.Provider,
		Effort:         nt.Effort,
		WorkspaceRef:   nt.WorkspaceRef,
		Prompt:         nt.Prompt,
		Status:         TurnRunning,
		StartedAt:      now,
	}
	dir := s.turnDir(convID, t.Seq, t.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "turn.json"), t); err != nil {
		return nil, err
	}
	return t, nil
}

// AppendEvent implements Store.
func (s *FSStore) AppendEvent(ctx context.Context, convID, turnID string, e adapter.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, _, err := s.findTurnDir(convID, turnID)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(e); err != nil {
		return err
	}
	return f.Sync()
}

// FinishTurn implements Store.
func (s *FSStore) FinishTurn(ctx context.Context, convID, turnID string, res *adapter.Result, snapshotID string, runErr error) (*Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, t, err := s.findTurnDir(convID, turnID)
	if err != nil {
		return nil, err
	}
	t.EndedAt = s.now().UTC()
	t.Result = res
	t.SnapshotID = snapshotID
	if runErr != nil {
		t.Status = TurnFailed
		t.Error = runErr.Error()
	} else {
		t.Status = TurnDone
	}
	if err := writeJSON(filepath.Join(dir, "turn.json"), t); err != nil {
		return nil, err
	}

	// Bump the conversation's UpdatedAt.
	c, err := s.GetConversation(ctx, convID)
	if err != nil {
		return nil, err
	}
	c.UpdatedAt = t.EndedAt
	if err := writeJSON(filepath.Join(s.convDir(convID), "conversation.json"), c); err != nil {
		return nil, err
	}
	return t, nil
}

// ListTurns implements Store; ordered by Seq.
func (s *FSStore) ListTurns(ctx context.Context, convID string) ([]*Turn, error) {
	if _, err := s.GetConversation(ctx, convID); err != nil {
		return nil, err
	}
	dirs, err := s.turnDirs(convID)
	if err != nil {
		return nil, err
	}
	out := make([]*Turn, 0, len(dirs))
	for _, d := range dirs {
		var t Turn
		if err := readJSON(filepath.Join(d, "turn.json"), &t); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// Events implements Store.
func (s *FSStore) Events(ctx context.Context, convID, turnID string) ([]adapter.Event, error) {
	dir, _, err := s.findTurnDir(convID, turnID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // turn began but no events yet
		}
		return nil, err
	}
	defer f.Close()

	var out []adapter.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // events can carry large raw payloads
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e adapter.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("transcript: decode event: %w", err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// SetConversationProject implements Store.
func (s *FSStore) SetConversationProject(ctx context.Context, id, projectPath string) (*Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.GetConversation(ctx, id)
	if err != nil {
		return nil, err
	}
	c.ProjectPath = projectPath
	c.UpdatedAt = s.now().UTC()
	if err := writeJSON(filepath.Join(s.convDir(id), "conversation.json"), c); err != nil {
		return nil, err
	}
	return c, nil
}

// DeleteConversation implements Store.
func (s *FSStore) DeleteConversation(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.GetConversation(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(s.convDir(id))
}

// ConversationDir returns the on-disk directory holding a conversation's
// raw store subtree (conversation.json + turns/...). Used by export to
// bundle the subtree verbatim; treat the contents as read-only.
func (s *FSStore) ConversationDir(id string) string { return s.convDir(id) }

// ImportConversation restores a conversation subtree captured by export:
// src must contain conversation.json (whose "id" matches id) plus the
// turns/ tree, and the conversation must not already exist. Files are
// copied verbatim so a re-imported conversation is byte-identical to the
// exported one.
func (s *FSStore) ImportConversation(ctx context.Context, id string, src fs.FS) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.GetConversation(ctx, id); err == nil {
		return fmt.Errorf("transcript: conversation %q already exists", id)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	// The subtree must at least identify itself correctly.
	b, err := fs.ReadFile(src, "conversation.json")
	if err != nil {
		return fmt.Errorf("transcript: import: %w", err)
	}
	var c Conversation
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("transcript: import: parsing conversation.json: %w", err)
	}
	if c.ID != id {
		return fmt.Errorf("transcript: import: conversation.json has id %q, want %q", c.ID, id)
	}

	dst := s.convDir(id)
	err = fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !fs.ValidPath(path) {
			return fmt.Errorf("transcript: import: unsafe path %q", path)
		}
		target := filepath.Join(dst, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // ignore anything exotic in the archive
		}
		in, err := src.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		// Leave no half-imported conversation behind.
		_ = os.RemoveAll(dst)
		return err
	}
	return nil
}

// --- internals ---

func (s *FSStore) turnDir(convID string, seq int, turnID string) string {
	return filepath.Join(s.convDir(convID), "turns", fmt.Sprintf("%04d-%s", seq, turnID))
}

func (s *FSStore) turnDirs(convID string) ([]string, error) {
	base := filepath.Join(s.convDir(convID), "turns")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(base, e.Name()))
		}
	}
	sort.Strings(out) // %04d prefix keeps lexical == numeric order
	return out, nil
}

func (s *FSStore) findTurnDir(convID, turnID string) (string, *Turn, error) {
	dirs, err := s.turnDirs(convID)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, fmt.Errorf("%w: conversation %q", ErrNotFound, convID)
		}
		return "", nil, err
	}
	for _, d := range dirs {
		if strings.HasSuffix(filepath.Base(d), "-"+turnID) {
			var t Turn
			if err := readJSON(filepath.Join(d, "turn.json"), &t); err != nil {
				return "", nil, err
			}
			return d, &t, nil
		}
	}
	return "", nil, fmt.Errorf("%w: turn %q in conversation %q", ErrNotFound, turnID, convID)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomic on POSIX
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *FSStore) nextSeq(convID string) (int, error) {
	dirs, err := s.turnDirs(convID)
	if err != nil {
		return 0, err
	}
	max := 0
	for _, d := range dirs {
		name := filepath.Base(d)
		var seq int
		if _, err := fmt.Sscanf(name, "%d-", &seq); err == nil && seq > max {
			max = seq
		}
	}
	return max + 1, nil
}
