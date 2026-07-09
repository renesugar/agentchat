// Command agentchat-cli is a headless harness for the AgentChat engine.
// It runs a single conversation turn with a chosen adapter and prints the
// normalized event stream — useful for developing/testing adapters before
// the Wails GUI (Step 10) exists.
//
// Usage:
//
//	agentchat-cli -client echo -dir /path/to/workspace [-model ID] [-json] "prompt..."
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/adapters/echo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentchat-cli:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		client   = flag.String("client", "echo", "coding client adapter to use")
		dir      = flag.String("dir", ".", "workspace directory")
		model    = flag.String("model", "", "model ID (adapter default if empty)")
		session  = flag.String("resume", "", "session ID from a previous turn")
		asJSON   = flag.Bool("json", false, "print events as JSON lines")
		listOnly = flag.Bool("list", false, "list adapters and models, then exit")
	)
	flag.Parse()

	reg := adapter.NewRegistry()
	reg.Register(echo.New())
	// Real adapters are registered here as PLAN.md Steps 3-6 land.

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *listOnly {
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

	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if prompt == "" {
		return fmt.Errorf("no prompt given (pass it as trailing arguments)")
	}

	a, err := reg.Get(*client)
	if err != nil {
		return err
	}
	if err := a.Available(ctx); err != nil {
		return fmt.Errorf("client %q not usable: %w", *client, err)
	}

	absDir, err := os.Getwd()
	if err != nil {
		return err
	}
	if *dir != "." {
		absDir = *dir
	}
	if st, err := os.Stat(absDir); err != nil || !st.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", absDir)
	}

	enc := json.NewEncoder(os.Stdout)
	emit := func(e adapter.Event) {
		if *asJSON {
			_ = enc.Encode(e)
			return
		}
		printEvent(e)
	}

	res, err := a.RunTurn(ctx, adapter.TurnRequest{
		Prompt:    prompt,
		WorkDir:   absDir,
		Model:     *model,
		SessionID: *session,
	}, emit)
	if err != nil {
		return err
	}
	if !*asJSON {
		fmt.Printf("\n[done] exit=%d files=%d session=%q in=%d out=%d\n",
			res.ExitCode, len(res.FilesChanged), res.SessionID,
			res.Usage.InputTokens, res.Usage.OutputTokens)
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
