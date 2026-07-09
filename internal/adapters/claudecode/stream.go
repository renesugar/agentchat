package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

// streamLine is the superset of fields we care about across Claude Code
// stream-json line types ("system", "assistant", "user", "result").
// Unknown types and fields are ignored, which keeps the parser tolerant of
// client version drift; each emitted event preserves the original line in
// Event.Raw.
type streamLine struct {
	Type      string   `json:"type"`
	Subtype   string   `json:"subtype"`
	SessionID string   `json:"session_id"`
	Message   *message `json:"message"`

	// "result" line fields.
	Result       string     `json:"result"`
	IsError      bool       `json:"is_error"`
	TotalCostUSD float64    `json:"total_cost_usd"`
	DurationMS   int64      `json:"duration_ms"`
	Usage        *usageInfo `json:"usage"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type block struct {
	Type string `json:"type"`

	// text / thinking blocks.
	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	// tool_use blocks.
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result blocks; Content is a string or a list of typed parts.
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type usageInfo struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// parseState accumulates what the terminal Result needs.
type parseState struct {
	sessionID    string
	finalText    string
	isError      bool
	usage        adapter.Usage
	filesChanged []adapter.FileChange
	seen         map[string]int // path -> index into filesChanged
}

func (p *parseState) result() *adapter.Result {
	return &adapter.Result{
		SessionID:    p.sessionID,
		FinalText:    p.finalText,
		FilesChanged: p.filesChanged,
		Usage:        p.usage,
	}
}

// parseStream consumes stream-json lines from r, emitting normalized events
// (never EventResult — the caller owns the terminal event). workDir is used
// to relativize file paths reported by tools. It returns the accumulated
// state and the first read/decode error, after draining as much as it can.
func parseStream(r io.Reader, workDir string, emit adapter.EmitFunc) (*parseState, error) {
	st := &parseState{seen: make(map[string]int)}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024) // tool results can be large
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var line streamLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			// A malformed line shouldn't kill the turn; surface and move on.
			emit(adapter.Event{Kind: adapter.EventError, Time: time.Now(),
				Text: fmt.Sprintf("unparseable output line: %v", err)})
			continue
		}
		if line.SessionID != "" {
			st.sessionID = line.SessionID
		}
		st.handle(&line, json.RawMessage(raw), workDir, emit)
	}
	return st, sc.Err()
}

func (st *parseState) handle(line *streamLine, raw json.RawMessage, workDir string, emit adapter.EmitFunc) {
	now := time.Now()
	switch line.Type {
	case "system":
		// init carries session_id (captured above); nothing user-visible.

	case "assistant":
		if line.Message == nil {
			return
		}
		for _, b := range line.Message.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					emit(adapter.Event{Kind: adapter.EventText, Time: now, Text: b.Text, Raw: raw})
				}
			case "thinking":
				if b.Thinking != "" {
					emit(adapter.Event{Kind: adapter.EventThinking, Time: now, Text: b.Thinking, Raw: raw})
				}
			case "tool_use":
				emit(adapter.Event{Kind: adapter.EventToolUse, Time: now, Raw: raw,
					Tool: &adapter.ToolInfo{Name: b.Name, Input: compact(b.Input)}})
				if fc := fileChangeFromTool(b.Name, b.Input, workDir); fc != nil {
					st.recordChange(*fc)
					emit(adapter.Event{Kind: adapter.EventFileChange, Time: now, File: fc})
				}
			}
		}

	case "user":
		if line.Message == nil {
			return
		}
		for _, b := range line.Message.Content {
			if b.Type != "tool_result" {
				continue
			}
			emit(adapter.Event{Kind: adapter.EventToolResult, Time: now, Raw: raw,
				Tool: &adapter.ToolInfo{
					Name:   b.ToolUseID, // tool name isn't repeated; the id links back
					Output: truncate(toolResultText(b.Content), 4000),
					IsErr:  b.IsError,
				}})
		}

	case "result":
		st.isError = line.IsError
		st.finalText = line.Result
		st.usage.CostUSD = line.TotalCostUSD
		if line.Usage != nil {
			st.usage.InputTokens = line.Usage.InputTokens
			st.usage.OutputTokens = line.Usage.OutputTokens
		}
		if line.IsError && line.Result != "" {
			emit(adapter.Event{Kind: adapter.EventError, Time: now, Text: line.Result, Raw: raw})
		}
	}
}

func (st *parseState) recordChange(fc adapter.FileChange) {
	if i, ok := st.seen[fc.Path]; ok {
		// Later ops win, except an initial create stays a create.
		if st.filesChanged[i].Op != adapter.FileCreated {
			st.filesChanged[i].Op = fc.Op
		}
		return
	}
	st.seen[fc.Path] = len(st.filesChanged)
	st.filesChanged = append(st.filesChanged, fc)
}

// fileChangeFromTool derives a FileChange from Claude Code's file-editing
// tools. Heuristic: Write creates (or overwrites), Edit-family modifies.
func fileChangeFromTool(tool string, input json.RawMessage, workDir string) *adapter.FileChange {
	var op adapter.FileOp
	switch tool {
	case "Write":
		op = adapter.FileCreated
	case "Edit", "MultiEdit", "NotebookEdit":
		op = adapter.FileModified
	default:
		return nil
	}
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil
	}
	path := in.FilePath
	if path == "" {
		path = in.NotebookPath
	}
	if path == "" {
		return nil
	}
	if rel, err := filepath.Rel(workDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		path = rel
	}
	return &adapter.FileChange{Path: path, Op: op}
}

// toolResultText flattens a tool_result content payload (string, or a list
// of {"type":"text","text":...} parts) into plain text.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return string(raw)
}

func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return truncate(string(raw), 4000)
	}
	return truncate(buf.String(), 4000)
}
