package aider

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/example/agentchat/internal/adapter"
)

var (
	appliedRe = regexp.MustCompile(`^Applied edit to (.+)$`)
	commitRe  = regexp.MustCompile(`^Commit ([0-9a-f]{7,40}) (.*)$`)
	// e.g. "Tokens: 12k sent, 1.2k received. Cost: $0.0341 message, $0.10 session."
	tokensRe = regexp.MustCompile(`Tokens: ([\d.,]+k?) sent, ([\d.,]+k?) received`)
	costRe   = regexp.MustCompile(`Cost: \$([\d.]+) message`)
)

// noisePrefixes are aider banner/housekeeping lines suppressed from the
// text stream (they'd otherwise pollute the chat with per-turn boilerplate).
var noisePrefixes = []string{
	"Aider v",
	"Main model:",
	"Weak model:",
	"Model:",
	"Git repo:",
	"Repo-map:",
	"Added ",   // "Added foo.py to the chat"
	"Scanning", // repo scanning progress
	"Restored previous conversation history",
	"Note: in-chat filenames",
	"Use /help",
	"Cur working dir",
	"Warning: it's best to only add files",
	"Initial repo scan",
	// Always emitted when aider runs non-interactively (stdin is a pipe).
	"Warning: Input is not a terminal",
	// First-run analytics/privacy notice (seen on aider 0.86.2).
	"Aider respects your privacy",
	"For more info: https://aider.chat",
}

// parseState accumulates what the terminal Result needs.
type parseState struct {
	textParts    []string
	filesChanged []adapter.FileChange
	seen         map[string]bool
	usage        adapter.Usage
}

func (p *parseState) result() *adapter.Result {
	final := ""
	if n := len(p.textParts); n > 0 {
		final = p.textParts[n-1]
	}
	return &adapter.Result{
		FinalText:    final,
		FilesChanged: p.filesChanged,
		Usage:        p.usage,
		// SessionID stays empty: aider keeps continuity via its own
		// history files in the workspace, not resumable session IDs.
	}
}

// parseOutput consumes aider's stdout line by line, emitting normalized
// events (never EventResult — the caller owns the terminal event).
// Consecutive prose lines are grouped into a single text event, flushed
// when a structured or noise line interrupts them.
func parseOutput(r io.Reader, emit adapter.EmitFunc) (*parseState, error) {
	st := &parseState{seen: make(map[string]bool)}
	var buf []string

	flush := func() {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		if text == "" {
			return
		}
		st.textParts = append(st.textParts, text)
		emit(adapter.Event{Kind: adapter.EventText, Time: time.Now(), Text: text})
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			buf = append(buf, "") // paragraph break within a text block

		case isNoise(trimmed):
			flush()

		case appliedRe.MatchString(trimmed):
			flush()
			path := appliedRe.FindStringSubmatch(trimmed)[1]
			fc := adapter.FileChange{Path: path, Op: adapter.FileModified}
			if !st.seen[path] {
				st.seen[path] = true
				st.filesChanged = append(st.filesChanged, fc)
			}
			emit(adapter.Event{Kind: adapter.EventFileChange, Time: time.Now(), File: &fc})

		case commitRe.MatchString(trimmed):
			flush()
			m := commitRe.FindStringSubmatch(trimmed)
			emit(adapter.Event{Kind: adapter.EventToolResult, Time: time.Now(),
				Tool: &adapter.ToolInfo{Name: "git-commit", Input: m[1], Output: m[2]}})

		case tokensRe.MatchString(trimmed):
			flush()
			m := tokensRe.FindStringSubmatch(trimmed)
			st.usage.InputTokens = parseTokenCount(m[1])
			st.usage.OutputTokens = parseTokenCount(m[2])
			if c := costRe.FindStringSubmatch(trimmed); c != nil {
				if v, err := strconv.ParseFloat(c[1], 64); err == nil {
					st.usage.CostUSD = v
				}
			}

		default:
			buf = append(buf, line)
		}
	}
	flush()
	return st, sc.Err()
}

func isNoise(line string) bool {
	for _, p := range noisePrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// parseTokenCount converts aider's human token counts ("12k", "1.2k",
// "847", "1,234") to an integer, best-effort.
func parseTokenCount(s string) int64 {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	mult := int64(1)
	if strings.HasSuffix(s, "k") {
		mult = 1000
		s = strings.TrimSuffix(s, "k")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(mult))
}
