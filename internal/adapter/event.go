package adapter

import (
	"encoding/json"
	"time"
)

// EventKind classifies a normalized event emitted while a coding client
// works on a turn. Adapters translate client-specific output into these.
type EventKind string

const (
	// EventText is assistant-visible prose (a chunk or a whole block).
	EventText EventKind = "text"
	// EventThinking is reasoning output, when the client surfaces it.
	EventThinking EventKind = "thinking"
	// EventPlan is a plan the client announced before acting.
	EventPlan EventKind = "plan"
	// EventToolUse reports the client invoking a tool/command.
	EventToolUse EventKind = "tool_use"
	// EventToolResult reports the outcome of a tool invocation.
	EventToolResult EventKind = "tool_result"
	// EventFileChange reports a file created/modified/deleted in the workspace.
	EventFileChange EventKind = "file_change"
	// EventError is a non-fatal error surfaced mid-turn.
	EventError EventKind = "error"
	// EventResult is the terminal event of a turn; Result is non-nil.
	EventResult EventKind = "result"
)

// Event is the normalized unit streamed from an adapter to the engine/UI.
// Exactly the fields relevant to Kind are set; Raw optionally preserves the
// client-specific payload for debugging and export.
type Event struct {
	Kind EventKind `json:"kind"`
	Time time.Time `json:"time"`

	// Text carries content for text/thinking/plan/error events.
	Text string `json:"text,omitempty"`

	Tool   *ToolInfo   `json:"tool,omitempty"`
	File   *FileChange `json:"file,omitempty"`
	Result *Result     `json:"result,omitempty"`

	Raw json.RawMessage `json:"raw,omitempty"`
}

// ToolInfo describes a tool invocation or its result.
type ToolInfo struct {
	Name   string `json:"name"`
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
	IsErr  bool   `json:"is_err,omitempty"`
}

// FileOp is the kind of change made to a workspace file.
type FileOp string

const (
	FileCreated  FileOp = "created"
	FileModified FileOp = "modified"
	FileDeleted  FileOp = "deleted"
	FileRenamed  FileOp = "renamed"
)

// FileChange describes one file touched during a turn.
type FileChange struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"` // for renames
	Op      FileOp `json:"op"`
}

// Usage is best-effort token/cost accounting reported by the client.
type Usage struct {
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Result summarizes a completed turn.
type Result struct {
	// SessionID is a client-specific handle for resuming the session on a
	// later turn (e.g. `claude --resume`). Empty if unsupported.
	SessionID string `json:"session_id,omitempty"`
	// ExitCode of the client process.
	ExitCode int `json:"exit_code"`
	// FinalText is the client's final answer, if distinguishable from the
	// event stream (some clients emit a dedicated result payload).
	FinalText string `json:"final_text,omitempty"`
	// FilesChanged aggregates file changes observed during the turn.
	FilesChanged []FileChange  `json:"files_changed,omitempty"`
	Usage        Usage         `json:"usage"`
	Duration     time.Duration `json:"duration"`
}
