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
	"github.com/example/agentchat/internal/adapters/echo"
	"github.com/example/agentchat/internal/engine"
	"github.com/example/agentchat/internal/transcript"
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
		session   = flag.String("resume", "", "session ID from a previous turn")
		convID    = flag.String("conv", "", "conversation ID to continue (new one if empty)")
		title     = flag.String("title", "", "title for a new conversation")
		project   = flag.String("project", "", "project (git repo) path for a new conversation")
		dataDir   = flag.String("data", "", "data dir (default $AGENTCHAT_DATA or ~/.agentchat)")
		asJSON    = flag.Bool("json", false, "print events as JSON lines")
		listOnly  = flag.Bool("list", false, "list adapters and models, then exit")
		listConvs = flag.Bool("conversations", false, "list stored conversations, then exit")
	)
	flag.Parse()

	reg := adapter.NewRegistry()
	reg.Register(echo.New())
	// Real adapters are registered here as PLAN.md Steps 3-6 land.

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *listOnly {
		return listAdapters(ctx, reg)
	}

	store, err := openStore(*dataDir)
	if err != nil {
		return err
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

	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if prompt == "" {
		return fmt.Errorf("no prompt given (pass it as trailing arguments)")
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(absDir); err != nil || !st.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", absDir)
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

	eng := engine.New(reg, store)
	turn, err := eng.RunTurn(ctx, id, *client, adapter.TurnRequest{
		Prompt:    prompt,
		WorkDir:   absDir,
		Model:     *model,
		SessionID: *session,
	}, tap)
	if err != nil {
		return err
	}
	if !*asJSON {
		res := turn.Result
		fmt.Printf("\n[done] conv=%s turn=%s seq=%d exit=%d files=%d session=%q\n",
			id, turn.ID, turn.Seq, res.ExitCode, len(res.FilesChanged), res.SessionID)
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

func listAdapters(ctx context.Context, reg *adapter.Registry) error {
	for _, name := range reg.Names() {
		a, _ := reg.Get(name)
		status := "available"
		if err := a.Available(ctx); err != nil {
			status = "unavailable: " + err.Error()
		}
		models, err := a.Models(ctx)
		if err != nil {
			return fmt.Errorf("%s: listing models: %w", name, err)
		}
		ids := make([]string, len(models))
		for i, m := range models {
			ids[i] = m.ID
		}
		fmt.Printf("%s\t%s\tmodels: %s\n", name, status, strings.Join(ids, ", "))
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
