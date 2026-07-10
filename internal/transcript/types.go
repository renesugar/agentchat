// Package transcript persists conversations: their turns and the normalized
// event stream of each turn. The Store interface is deliberately small so
// the file-system implementation (FSStore) can later be replaced by SQLite
// without touching the engine or UI.
package transcript

import (
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// Conversation groups turns. If ProjectPath is set, the conversation is
// associated with a local git repo (project grouping in the UI); otherwise
// it is a scratch conversation.
type Conversation struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	ProjectPath string    `json:"project_path,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NewConversation is the caller-supplied part of a Conversation.
type NewConversation struct {
	Title       string
	ProjectPath string
}

// TurnStatus is the lifecycle state of a turn.
type TurnStatus string

const (
	TurnRunning TurnStatus = "running"
	TurnDone    TurnStatus = "done"
	TurnFailed  TurnStatus = "failed"
)

// Turn records one execution of a coding client within a conversation.
// Its event stream is stored separately (Store.Events).
type Turn struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	Seq            int        `json:"seq"` // 1-based position within the conversation
	Client         string     `json:"client"`
	Model          string     `json:"model,omitempty"`
	Effort         string     `json:"effort,omitempty"`        // reasoning effort, when one was set
	WorkspaceRef   string     `json:"workspace_ref,omitempty"` // dir/worktree/snapshot id (Step 7 refines)
	Prompt         string     `json:"prompt"`
	Status         TurnStatus `json:"status"`
	// SnapshotID is the workspace snapshot commit taken after this turn
	// (empty when the turn ran without a managed workspace).
	SnapshotID string    `json:"snapshot_id,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`

	// Result is set when Status == TurnDone (and sometimes on failure if
	// the adapter produced a partial result).
	Result *adapter.Result `json:"result,omitempty"`
	// Error is the run error string when Status == TurnFailed.
	Error string `json:"error,omitempty"`
}

// NewTurn is the caller-supplied part of a Turn; Seq/ID/times are assigned
// by the store.
type NewTurn struct {
	Client       string
	Model        string
	Effort       string
	WorkspaceRef string
	Prompt       string
}
