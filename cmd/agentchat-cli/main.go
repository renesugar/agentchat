// Command agentchat-cli is a headless harness for the AgentChat engine.
// It runs a single conversation turn with a chosen adapter, persists it to
// the transcript store, and prints the normalized event stream — useful for
// developing/testing adapters before the Wails GUI (Step 10) exists.
//
// Usage:
//
//	agentchat-cli -client echo -dir /path/to/workspace [-model ID] "prompt..."
//	agentchat-cli -conv <id> -client echo -dir ... "next prompt"   # continue
//	agentchat-cli -list            # adapters and models
//	agentchat-cli -conversations   # stored conversations
//
// Data dir: -data flag, else $AGENTCHAT_DATA, else ~/.agentchat.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/artifact"
	"github.com/example/agentchat/internal/clients"
	"github.com/example/agentchat/internal/config"
	"github.com/example/agentchat/internal/engine"
	"github.com/example/agentchat/internal/export"
	"github.com/example/agentchat/internal/mcpserver"
	"github.com/example/agentchat/internal/transcript"
	"github.com/example/agentchat/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentchat-cli:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		client    = flag.String("client", "echo", "coding client adapter to use")
		dir       = flag.String("dir", ".", "workspace directory")
		model     = flag.String("model", "", "model ID (adapter default if empty)")
		effort    = flag.String("effort", "", "reasoning effort level (client default if empty; client validates values)")
		provName  = flag.String("provider", "", "model provider name (client default/subscription if empty; see -list)")
		session   = flag.String("resume", "", "session ID from a previous turn")
		convID    = flag.String("conv", "", "conversation ID to continue (new one if empty)")
		title     = flag.String("title", "", "title for a new conversation")
		project   = flag.String("project", "", "project (git repo) path for a new conversation")
		dataDir   = flag.String("data", "", "data dir (default $AGENTCHAT_DATA or ~/.agentchat)")
		scratch   = flag.Bool("scratch", false, "create a scratch workspace under the data dir (ignores -dir)")
		exportMD  = flag.String("export-md", "", "write a markdown transcript of -conv to this file, then exit")
		exportSeq = flag.Int("export-turn", 0, "print turn <seq> of -conv as markdown to stdout, then exit")
		exportZip = flag.String("export-bundle", "", "write a ZIP bundle of -conv (transcript+artifacts[+workspace via -dir]) to this file, then exit")
		importZip = flag.String("import-bundle", "", "restore a conversation from this bundle ZIP, then exit")
		deleteID  = flag.String("delete-conv", "", "delete this conversation (turns+events; artifacts are kept), then exit")
		setProj   = flag.String("set-project", "", "re-associate -conv with this project repo path (\"-\" detaches to scratch), then exit")
		promote   = flag.String("promote", "", "move -conv's scratch workspace to this NEW directory and make it the project, then exit")
		asJSON    = flag.Bool("json", false, "print events as JSON lines")
		useMCP    = flag.Bool("mcp", true, "expose the MCP callback channel (loopback) to MCP-capable clients")
		listOnly  = flag.Bool("list", false, "list adapters and models, then exit")
		listConvs = flag.Bool("conversations", false, "list stored conversations, then exit")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(filepath.Join(store.Root(), "config.json"))
	if err != nil {
		return err
	}
	set := clients.New(cfg)

	if *listOnly {
		return listAdapters(ctx, set)
	}

	if *listConvs {
		convs, err := store.ListConversations(ctx)
		if err != nil {
			return err
		}
		for _, c := range convs {
			fmt.Printf("%s\t%s\t%s\t%s\n", c.ID, c.UpdatedAt.Format("2006-01-02 15:04"), c.Title, c.ProjectPath)
		}
		return nil
	}

	if *deleteID != "" {
		if err := store.DeleteConversation(ctx, *deleteID); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "deleted conversation %s (artifacts kept)\n", *deleteID)
		return nil
	}

	if *setProj != "" {
		if *convID == "" {
			return fmt.Errorf("-set-project requires -conv <conversation id>")
		}
		mgr, err := workspace.NewManager(filepath.Join(store.Root(), "workspaces"))
		if err != nil {
			return err
		}
		path := *setProj
		if path == "-" {
			path = "" // detach back to scratch
		} else {
			ws, err := mgr.OpenRepo(ctx, path)
			if err != nil {
				return err
			}
			path = ws.Dir
		}
		conv, err := store.SetConversationProject(ctx, *convID, path)
		if err != nil {
			return err
		}
		if conv.ProjectPath == "" {
			fmt.Fprintf(os.Stderr, "conversation %s detached from its project\n", conv.ID)
		} else {
			fmt.Fprintf(os.Stderr, "conversation %s now belongs to %s\n", conv.ID, conv.ProjectPath)
		}
		return nil
	}

	if *promote != "" {
		if *convID == "" {
			return fmt.Errorf("-promote requires -conv <conversation id>")
		}
		mgr, err := workspace.NewManager(filepath.Join(store.Root(), "workspaces"))
		if err != nil {
			return err
		}
		conv, err := store.GetConversation(ctx, *convID)
		if err != nil {
			return err
		}
		if conv.ProjectPath != "" {
			return fmt.Errorf("conversation already belongs to project %s", conv.ProjectPath)
		}
		turns, err := store.ListTurns(ctx, *convID)
		if err != nil {
			return err
		}
		if len(turns) == 0 || turns[len(turns)-1].WorkspaceRef == "" {
			return fmt.Errorf("conversation has no workspace to promote (no turns yet)")
		}
		ws, err := mgr.OpenScratch(ctx, turns[len(turns)-1].WorkspaceRef)
		if err != nil {
			return err
		}
		promoted, err := mgr.PromoteScratch(ctx, ws, *promote)
		if err != nil {
			return err
		}
		if _, err := store.SetConversationProject(ctx, *convID, promoted.Dir); err != nil {
			return fmt.Errorf("workspace moved to %s but re-associating failed: %w", promoted.Dir, err)
		}
		fmt.Fprintf(os.Stderr, "promoted: conversation %s → project %s\n", *convID, promoted.Dir)
		return nil
	}

	if *importZip != "" {
		lib, err := artifact.NewLibrary(filepath.Join(store.Root(), "artifacts"))
		if err != nil {
			return err
		}
		mgr, err := workspace.NewManager(filepath.Join(store.Root(), "workspaces"))
		if err != nil {
			return err
		}
		conv, ws, err := export.Import(ctx, store, lib, mgr, *importZip)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "imported conversation %s (%q)\n", conv.ID, conv.Title)
		if ws != nil {
			fmt.Fprintf(os.Stderr, "workspace restored at %s (use -dir to continue there)\n", ws.Dir)
		}
		return nil
	}

	// Export modes need a conversation, not a prompt.
	if *exportSeq > 0 {
		if *convID == "" {
			return fmt.Errorf("export requires -conv <conversation id>")
		}
		turns, err := store.ListTurns(ctx, *convID)
		if err != nil {
			return err
		}
		for _, t := range turns {
			if t.Seq != *exportSeq {
				continue
			}
			events, err := store.Events(ctx, *convID, t.ID)
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(export.TurnMarkdown(t, events))
			return err
		}
		return fmt.Errorf("conversation %s has no turn with seq %d", *convID, *exportSeq)
	}
	if *exportMD != "" || *exportZip != "" {
		if *convID == "" {
			return fmt.Errorf("export requires -conv <conversation id>")
		}
		lib, err := artifact.NewLibrary(filepath.Join(store.Root(), "artifacts"))
		if err != nil {
			return err
		}
		ex := &export.Exporter{Store: store, Library: lib}
		if *exportMD != "" {
			md, err := ex.Markdown(ctx, *convID)
			if err != nil {
				return err
			}
			if err := os.WriteFile(*exportMD, md, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s\n", *exportMD)
		}
		if *exportZip != "" {
			mgr, err := workspace.NewManager(filepath.Join(store.Root(), "workspaces"))
			if err != nil {
				return err
			}
			var ws *workspace.Workspace
			if abs, err := filepath.Abs(*dir); err == nil {
				if w, err := mgr.OpenRepo(ctx, abs); err == nil {
					ws = w
				}
			}
			if err := ex.Bundle(ctx, *convID, ws, *exportZip); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s\n", *exportZip)
		}
		return nil
	}

	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if prompt == "" {
		return fmt.Errorf("no prompt given (pass it as trailing arguments)")
	}

	// Resolve the workspace: -scratch creates a managed scratch dir; a git
	// repo at -dir becomes a snapshot-managed repo workspace; any other
	// directory runs unmanaged (no snapshots).
	mgr, err := workspace.NewManager(filepath.Join(store.Root(), "workspaces"))
	if err != nil {
		return err
	}
	var ws *workspace.Workspace
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	switch {
	case *scratch:
		if ws, err = mgr.CreateScratch(ctx, *title); err != nil {
			return err
		}
		absDir = ws.Dir
		fmt.Fprintf(os.Stderr, "scratch workspace: %s\n", ws.Dir)
	default:
		if st, err := os.Stat(absDir); err != nil || !st.IsDir() {
			return fmt.Errorf("workspace %q is not a directory", absDir)
		}
		if w, err := mgr.OpenRepo(ctx, absDir); err == nil {
			ws = w
		}
	}

	// Resolve or create the conversation.
	id := *convID
	if id == "" {
		t := *title
		if t == "" {
			t = prompt
			if len(t) > 48 {
				t = t[:48] + "…"
			}
		}
		c, err := store.CreateConversation(ctx, transcript.NewConversation{Title: t, ProjectPath: *project})
		if err != nil {
			return err
		}
		id = c.ID
		fmt.Fprintf(os.Stderr, "conversation: %s\n", id)
	}

	enc := json.NewEncoder(os.Stdout)
	tap := func(e adapter.Event) {
		if *asJSON {
			_ = enc.Encode(e)
			return
		}
		printEvent(e)
	}

	req := adapter.TurnRequest{
		Prompt:    prompt,
		WorkDir:   absDir,
		Model:     *model,
		Effort:    *effort,
		SessionID: *session,
	}
	if *provName != "" {
		req.Provider = &adapter.ProviderInfo{Name: *provName}
	}
	if err := set.Prepare(ctx, *client, &req); err != nil {
		return err
	}

	eng := engine.New(set.Registry, store)
	if *useMCP {
		srv, err := mcpserver.Start()
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp callback disabled: %v\n", err)
		} else {
			defer srv.Close()
			eng.MCP = srv
			lib, err := artifact.NewLibrary(filepath.Join(store.Root(), "artifacts"))
			if err != nil {
				return err
			}
			eng.ArtifactSink = func(ctx context.Context, convID, turnID, path, note string) (string, error) {
				art, err := lib.AddFileFromPath(ctx, path, "", artifact.Meta{
					ConversationID: convID, TurnID: turnID, Origin: "mcp", Note: note,
				})
				if err != nil {
					return "", err
				}
				return art.ID, nil
			}
		}
	}
	turn, err := eng.RunTurn(ctx, id, *client, ws, req, tap)
	if err != nil {
		return err
	}
	if !*asJSON {
		res := turn.Result
		fmt.Printf("\n[done] conv=%s turn=%s seq=%d exit=%d files=%d session=%q\n",
			id, turn.ID, turn.Seq, res.ExitCode, len(res.FilesChanged), res.SessionID)
		if turn.SnapshotID != "" {
			fmt.Printf("[snapshot] %s\n", turn.SnapshotID)
		}
	}
	return nil
}

func openStore(dataDir string) (*transcript.FSStore, error) {
	dir := dataDir
	if dir == "" {
		dir = os.Getenv("AGENTCHAT_DATA")
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving data dir: %w (use -data)", err)
		}
		dir = filepath.Join(home, ".agentchat")
	}
	return transcript.NewFSStore(dir)
}

func listAdapters(ctx context.Context, set *clients.Set) error {
	for _, name := range set.Registry.Names() {
		a, _ := set.Registry.Get(name)
		status := "available"
		if err := a.Available(ctx); err != nil {
			status = "unavailable: " + err.Error()
		}
		models, err := set.Models(ctx, name)
		if err != nil {
			return fmt.Errorf("%s: listing models: %w", name, err)
		}
		ids := make([]string, len(models))
		for i, m := range models {
			ids[i] = m.ID
		}
		efforts, err := set.Efforts(ctx, name)
		if err != nil {
			return fmt.Errorf("%s: listing efforts: %w", name, err)
		}
		provs, err := set.Providers(ctx, name)
		if err != nil {
			return fmt.Errorf("%s: listing providers: %w", name, err)
		}
		pnames := make([]string, len(provs))
		for i, p := range provs {
			if p.Name == "" {
				pnames[i] = "(" + p.Label + ")"
			} else {
				pnames[i] = p.Name
			}
		}
		fmt.Printf("%s\t%s\tproviders: %s\tmodels: %s\tefforts: %s\n",
			name, status, strings.Join(pnames, ", "), strings.Join(ids, ", "), strings.Join(efforts, ", "))
	}
	return nil
}

func printEvent(e adapter.Event) {
	switch e.Kind {
	case adapter.EventText:
		fmt.Println(e.Text)
	case adapter.EventThinking:
		fmt.Println("[thinking]", e.Text)
	case adapter.EventPlan:
		fmt.Println("[plan]\n" + e.Text)
	case adapter.EventToolUse:
		fmt.Printf("[tool] %s %s\n", e.Tool.Name, e.Tool.Input)
	case adapter.EventToolResult:
		fmt.Printf("[tool✓] %s → %s\n", e.Tool.Name, e.Tool.Output)
	case adapter.EventFileChange:
		fmt.Printf("[file] %s %s\n", e.File.Op, e.File.Path)
	case adapter.EventError:
		fmt.Println("[error]", e.Text)
	case adapter.EventResult:
		// summarized by caller
	default:
		fmt.Printf("[%s] %s\n", e.Kind, e.Text)
	}
}
