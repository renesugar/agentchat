package transcript

import (
	"path/filepath"
	"sort"
	"strings"
)

// Project summarizes one known project (a repo path shared by one or more
// conversations). There is no separate project registry — conversations
// are the source of truth and projects are derived from them.
type Project struct {
	Path  string `json:"path"`
	Label string `json:"label"` // basename of Path
	Count int    `json:"count"` // conversations in this project
}

// Projects derives the distinct projects among convs, with conversation
// counts, sorted by label (then path for equal labels). Conversations
// without a project (scratch) are excluded.
func Projects(convs []*Conversation) []Project {
	counts := make(map[string]int)
	for _, c := range convs {
		if c.ProjectPath != "" {
			counts[c.ProjectPath]++
		}
	}
	out := make([]Project, 0, len(counts))
	for path, n := range counts {
		label := filepath.Base(strings.TrimRight(path, "/\\"))
		if label == "" || label == "." || label == string(filepath.Separator) {
			label = path
		}
		out = append(out, Project{Path: path, Label: label, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Path < out[j].Path
	})
	return out
}
