package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/renesugar/agentchat/internal/adapter"
)

// streamLine is the superset of fields across `codex exec --json` event
// lines: thread.started/thread.resumed, turn.started/completed/failed,
// item.started/updated/completed, and top-level error. Unknown types and
// fields are ignored so the parser tolerates version drift; emitted events
// preserve the original line in Event.Raw.
type streamLine struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id"`
	Item     *item      `json:"item"`
	Usage    *usageInfo `json:"usage"`
	Error    *errInfo   `json:"error"`
	Message  string     `json:"message"` // top-level "error" lines
}

type item struct {
	ID string `json:"id"`
	// Current releases use "type"; some earlier docs/builds used
	// "item_type". Accept both (see UnmarshalJSON below).
	Type    string `json:"type"`
	AltType string `json:"item_type"`
	Status  string `json:"status"` // in_progress | completed | failed

	// agent_message / reasoning / error items.
	Text    string `json:"text"`
	Message string `json:"message"`

	// command_execution items.
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`

	// file_change items.
	Changes []change `json:"changes"`

	// mcp_tool_call items.
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`

	// web_search items.
	Query string `json:"query"`

	// todo_list items.
	Items []todo `json:"items"`
}

func (it *item) kind() string {
	if it.Type != "" {
		return it.Type
	}
	return it.AltType
}

type change struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // add | update | delete
}

type todo struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

type usageInfo struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

type errInfo struct {
	Message string `json:"message"`
}

// parseState accumulates what the terminal Result needs.
type parseState struct {
	sessionID    string
	finalText    string
	failed       bool
	failMsg      string
	usage        adapter.Usage
	filesChanged []adapter.FileChange
	seen         map[string]int
}

func (p *parseState) result() *adapter.Result {
	return &adapter.Result{
		SessionID:    p.sessionID,
		FinalText:    p.finalText,
		FilesChanged: p.filesChanged,
		Usage:        p.usage,
	}
}

// parseStream consumes JSONL events from r, emitting normalized events
// (never EventResult — the caller owns the terminal event). workDir is used
// to relativize reported file paths. It returns accumulated state and the
// first read error, after draining as much as it can.
func parseStream(r io.Reader, workDir string, emit adapter.EmitFunc) (*parseState, error) {
	st := &parseState{seen: make(map[string]int)}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024) // aggregated_output can be large
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var line streamLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			emit(adapter.Event{Kind: adapter.EventError, Time: time.Now(),
				Text: fmt.Sprintf("unparseable output line: %v", err)})
			continue
		}
		st.handle(&line, json.RawMessage(raw), workDir, emit)
	}
	return st, sc.Err()
}

func (st *parseState) handle(line *streamLine, raw json.RawMessage, workDir string, emit adapter.EmitFunc) {
	now := time.Now()
	switch line.Type {
	case "thread.started", "thread.resumed":
		if line.ThreadID != "" {
			st.sessionID = line.ThreadID
		}

	case "turn.started":
		// lifecycle only

	case "turn.completed":
		if line.Usage != nil {
			st.usage.InputTokens = line.Usage.InputTokens + line.Usage.CachedInputTokens
			st.usage.OutputTokens = line.Usage.OutputTokens
		}

	case "turn.failed":
		st.failed = true
		if line.Error != nil {
			st.failMsg = line.Error.Message
		}
		if st.failMsg != "" {
			emit(adapter.Event{Kind: adapter.EventError, Time: now, Text: st.failMsg, Raw: raw})
		}

	case "error":
		// Transient "Reconnecting... X/Y" notices are progress, not failure.
		if strings.HasPrefix(line.Message, "Reconnecting") {
			emit(adapter.Event{Kind: adapter.EventError, Time: now, Text: line.Message, Raw: raw})
			return
		}
		st.failed = true
		if st.failMsg == "" {
			st.failMsg = line.Message
		}
		emit(adapter.Event{Kind: adapter.EventError, Time: now, Text: line.Message, Raw: raw})

	case "item.started", "item.updated", "item.completed":
		if line.Item != nil {
			st.handleItem(line.Type, line.Item, raw, workDir, emit)
		}
	}
}

func (st *parseState) handleItem(phase string, it *item, raw json.RawMessage, workDir string, emit adapter.EmitFunc) {
	now := time.Now()
	completed := phase == "item.completed"

	switch it.kind() {
	case "agent_message":
		if completed && it.Text != "" {
			emit(adapter.Event{Kind: adapter.EventText, Time: now, Text: it.Text, Raw: raw})
			st.finalText = it.Text // the last agent_message is the final answer
		}

	case "reasoning":
		if completed && it.Text != "" {
			emit(adapter.Event{Kind: adapter.EventThinking, Time: now, Text: it.Text, Raw: raw})
		}

	case "command_execution":
		switch phase {
		case "item.started":
			emit(adapter.Event{Kind: adapter.EventToolUse, Time: now, Raw: raw,
				Tool: &adapter.ToolInfo{Name: "shell", Input: it.Command}})
		case "item.completed":
			isErr := it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0)
			emit(adapter.Event{Kind: adapter.EventToolResult, Time: now, Raw: raw,
				Tool: &adapter.ToolInfo{Name: "shell", Input: it.Command,
					Output: truncate(it.AggregatedOutput, 4000), IsErr: isErr}})
		}

	case "file_change":
		if !completed {
			return
		}
		for _, c := range it.Changes {
			fc := adapter.FileChange{Path: relativize(c.Path, workDir), Op: opForKind(c.Kind)}
			if it.Status != "failed" {
				st.recordChange(fc)
			}
			emit(adapter.Event{Kind: adapter.EventFileChange, Time: now, File: &fc, Raw: raw})
		}

	case "mcp_tool_call":
		name := strings.TrimPrefix(it.Server+"."+it.Tool, ".")
		switch phase {
		case "item.started":
			emit(adapter.Event{Kind: adapter.EventToolUse, Time: now, Raw: raw,
				Tool: &adapter.ToolInfo{Name: name, Input: compact(it.Arguments)}})
		case "item.completed":
			emit(adapter.Event{Kind: adapter.EventToolResult, Time: now, Raw: raw,
				Tool: &adapter.ToolInfo{Name: name,
					Output: truncate(compact(it.Result), 4000), IsErr: it.Status == "failed"}})
		}

	case "web_search":
		if phase == "item.started" || completed {
			emit(adapter.Event{Kind: adapter.EventToolUse, Time: now, Raw: raw,
				Tool: &adapter.ToolInfo{Name: "web_search", Input: it.Query}})
		}

	case "todo_list":
		var sb strings.Builder
		for _, td := range it.Items {
			mark := "[ ]"
			if td.Completed {
				mark = "[x]"
			}
			fmt.Fprintf(&sb, "%s %s\n", mark, td.Text)
		}
		if sb.Len() > 0 {
			emit(adapter.Event{Kind: adapter.EventPlan, Time: now,
				Text: strings.TrimRight(sb.String(), "\n"), Raw: raw})
		}

	case "error":
		if it.Message != "" {
			emit(adapter.Event{Kind: adapter.EventError, Time: now, Text: it.Message, Raw: raw})
		}
	}
}

func (st *parseState) recordChange(fc adapter.FileChange) {
	if i, ok := st.seen[fc.Path]; ok {
		if st.filesChanged[i].Op != adapter.FileCreated || fc.Op == adapter.FileDeleted {
			st.filesChanged[i].Op = fc.Op
		}
		return
	}
	st.seen[fc.Path] = len(st.filesChanged)
	st.filesChanged = append(st.filesChanged, fc)
}

func opForKind(kind string) adapter.FileOp {
	switch kind {
	case "add":
		return adapter.FileCreated
	case "delete":
		return adapter.FileDeleted
	case "rename":
		return adapter.FileRenamed
	default: // "update" and anything unknown
		return adapter.FileModified
	}
}

func relativize(path, workDir string) string {
	if rel, err := filepath.Rel(workDir, path); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
		return rel
	}
	return path
}

func compact(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return truncate(string(raw), 4000)
	}
	return truncate(buf.String(), 4000)
}
