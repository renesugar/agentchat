// Package export renders a conversation into portable context for humans,
// other coding clients, or other chat GUIs:
//
//   - Markdown(...) produces a single transcript document: per turn the
//     prompt, the client/model, the plan (when one was announced), the
//     response text, file-change summary, snapshot hash, and usage.
//   - Bundle(...) writes a ZIP containing transcript.md, the
//     conversation's file artifacts (links are listed in links.md rather
//     than copied), and — when a workspace is supplied — workspace.zip,
//     the tree of the latest per-turn snapshot.
package export

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/artifact"
	"github.com/example/agentchat/internal/transcript"
	"github.com/example/agentchat/internal/workspace"
)

// Exporter renders conversations. Library is optional (no artifacts
// section when nil).
type Exporter struct {
	Store   transcript.Store
	Library *artifact.Library
}

// Markdown renders the whole conversation as a transcript document.
func (e *Exporter) Markdown(ctx context.Context, convID string) ([]byte, error) {
	conv, err := e.Store.GetConversation(ctx, convID)
	if err != nil {
		return nil, err
	}
	turns, err := e.Store.ListTurns(ctx, convID)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", conv.Title)
	fmt.Fprintf(&b, "**Conversation** `%s`  \n", conv.ID)
	if conv.ProjectPath != "" {
		fmt.Fprintf(&b, "**Project** `%s`  \n", conv.ProjectPath)
	}
	fmt.Fprintf(&b, "**Created** %s · **Updated** %s  \n",
		conv.CreatedAt.Format("2006-01-02 15:04 MST"), conv.UpdatedAt.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "**Turns** %d\n", len(turns))

	for _, t := range turns {
		events, err := e.Store.Events(ctx, convID, t.ID)
		if err != nil {
			return nil, err
		}
		renderTurn(&b, t, events)
	}

	if e.Library != nil {
		arts, err := e.Library.List(ctx, convID)
		if err != nil {
			return nil, err
		}
		if len(arts) > 0 {
			b.WriteString("\n---\n\n## Artifacts\n\n")
			for _, a := range arts {
				switch a.Kind {
				case artifact.KindLink:
					target := a.LocalPath
					if a.RemoteURL != "" {
						target += " (remote: " + a.RemoteURL + ")"
					}
					fmt.Fprintf(&b, "- link `%s` → %s\n", a.Name, target)
				default:
					fmt.Fprintf(&b, "- file `%s` (%d bytes, sha256 %.12s…)\n", a.Name, a.Size, a.SHA256)
				}
			}
		}
	}
	return []byte(b.String()), nil
}

func renderTurn(b *strings.Builder, t *transcript.Turn, events []adapter.Event) {
	model := t.Model
	if model == "" {
		model = "default model"
	}
	fmt.Fprintf(b, "\n---\n\n## Turn %d — %s (%s) — %s\n\n", t.Seq, t.Client, model, t.Status)

	b.WriteString("**Prompt:**\n\n")
	for _, line := range strings.Split(strings.TrimSpace(t.Prompt), "\n") {
		fmt.Fprintf(b, "> %s\n", line)
	}

	var texts, plans, errs []string
	for _, ev := range events {
		switch ev.Kind {
		case adapter.EventText:
			texts = append(texts, ev.Text)
		case adapter.EventPlan:
			plans = append(plans, ev.Text)
		case adapter.EventError:
			errs = append(errs, ev.Text)
		}
	}

	if len(plans) > 0 {
		// The last plan event is the most complete (checklists update).
		fmt.Fprintf(b, "\n**Plan:**\n\n```\n%s\n```\n", plans[len(plans)-1])
	}

	response := strings.TrimSpace(strings.Join(texts, "\n\n"))
	if response == "" && t.Result != nil {
		response = strings.TrimSpace(t.Result.FinalText)
	}
	if response != "" {
		fmt.Fprintf(b, "\n**Response:**\n\n%s\n", response)
	}

	if t.Status == transcript.TurnFailed {
		fmt.Fprintf(b, "\n**Error:** %s\n", t.Error)
		for _, e := range errs {
			fmt.Fprintf(b, "- %s\n", e)
		}
	}

	if t.Result != nil && len(t.Result.FilesChanged) > 0 {
		b.WriteString("\n**Files changed:**\n\n")
		for _, fc := range t.Result.FilesChanged {
			if fc.Op == adapter.FileRenamed && fc.OldPath != "" {
				fmt.Fprintf(b, "- %s `%s` → `%s`\n", fc.Op, fc.OldPath, fc.Path)
				continue
			}
			fmt.Fprintf(b, "- %s `%s`\n", fc.Op, fc.Path)
		}
	}

	var meta []string
	if t.SnapshotID != "" {
		meta = append(meta, fmt.Sprintf("snapshot `%s`", t.SnapshotID))
	}
	if t.Result != nil {
		u := t.Result.Usage
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			s := fmt.Sprintf("tokens in=%d out=%d", u.InputTokens, u.OutputTokens)
			if u.CostUSD > 0 {
				s += fmt.Sprintf(" cost=$%.4f", u.CostUSD)
			}
			meta = append(meta, s)
		}
		if t.Result.SessionID != "" {
			meta = append(meta, fmt.Sprintf("session `%s`", t.Result.SessionID))
		}
	}
	if len(meta) > 0 {
		fmt.Fprintf(b, "\n*%s*\n", strings.Join(meta, " · "))
	}
}

// Bundle writes a ZIP at outPath containing transcript.md, the
// conversation's stored file artifacts under artifacts/ (link artifacts
// are listed in artifacts/links.md), and — when ws is non-nil and has at
// least one snapshot — workspace.zip with the latest snapshot's tree.
func (e *Exporter) Bundle(ctx context.Context, convID string, ws *workspace.Workspace, outPath string) error {
	md, err := e.Markdown(ctx, convID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)

	if err := writeZipFile(zw, "transcript.md", md); err != nil {
		return err
	}

	if e.Library != nil {
		arts, err := e.Library.List(ctx, convID)
		if err != nil {
			return err
		}
		var links strings.Builder
		for _, a := range arts {
			switch a.Kind {
			case artifact.KindFile:
				blob, err := e.Library.BlobPath(ctx, a.ID)
				if err != nil {
					return err
				}
				if err := copyIntoZip(zw, "artifacts/"+a.ID+"-"+sanitizeName(a.Name), blob); err != nil {
					return err
				}
			case artifact.KindLink:
				fmt.Fprintf(&links, "- %s: local %s", a.Name, a.LocalPath)
				if a.RemoteURL != "" {
					fmt.Fprintf(&links, " · remote %s", a.RemoteURL)
				}
				links.WriteString("\n")
			}
		}
		if links.Len() > 0 {
			body := "# Linked artifacts\n\nThese were stored as references, not copies:\n\n" + links.String()
			if err := writeZipFile(zw, "artifacts/links.md", []byte(body)); err != nil {
				return err
			}
		}
	}

	if ws != nil {
		if snap := ws.LatestSnapshot(ctx); snap != "" {
			tmp, err := os.CreateTemp("", "agentchat-wszip-*.zip")
			if err != nil {
				return err
			}
			tmpPath := tmp.Name()
			tmp.Close()
			defer os.Remove(tmpPath)
			if err := ws.Zip(ctx, snap, tmpPath); err != nil {
				return err
			}
			if err := copyIntoZip(zw, "workspace.zip", tmpPath); err != nil {
				return err
			}
			note := fmt.Sprintf("workspace.zip contains the tree of snapshot %s from %s\n", snap, ws.Dir)
			if err := writeZipFile(zw, "workspace.txt", []byte(note)); err != nil {
				return err
			}
		}
	}

	return zw.Close()
}

func writeZipFile(zw *zip.Writer, name string, content []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(content)
	return err
}

func copyIntoZip(zw *zip.Writer, name, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}

func sanitizeName(s string) string {
	s = filepath.Base(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "artifact"
	}
	return b.String()
}
