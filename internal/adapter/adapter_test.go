package adapter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/agentchat/internal/adapter"
	"github.com/example/agentchat/internal/adapters/echo"
)

func TestRegistry(t *testing.T) {
	r := adapter.NewRegistry()
	r.Register(echo.New())

	if got := r.Names(); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("Names() = %v, want [echo]", got)
	}
	if _, err := r.Get("echo"); err != nil {
		t.Fatalf("Get(echo): %v", err)
	}
	if _, err := r.Get("nope"); !errors.Is(err, adapter.ErrUnknownAdapter) {
		t.Fatalf("Get(nope) err = %v, want ErrUnknownAdapter", err)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	r.Register(echo.New())
}

func TestEchoTurn(t *testing.T) {
	dir := t.TempDir()
	a := echo.New()

	if err := a.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}

	var events []adapter.Event
	res, err := a.RunTurn(context.Background(), adapter.TurnRequest{
		Prompt:  "hello world",
		WorkDir: dir,
		Model:   "echo-1",
	}, func(e adapter.Event) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	// Terminal event contract: exactly one EventResult, and it is last.
	var results int
	for _, e := range events {
		if e.Kind == adapter.EventResult {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("got %d result events, want 1", results)
	}
	if last := events[len(events)-1]; last.Kind != adapter.EventResult || last.Result == nil {
		t.Fatalf("last event = %+v, want terminal result", last)
	}

	if res.ExitCode != 0 || len(res.FilesChanged) != 1 || res.FilesChanged[0].Path != "ECHO.md" {
		t.Fatalf("unexpected result: %+v", res)
	}

	b, err := os.ReadFile(filepath.Join(dir, "ECHO.md"))
	if err != nil {
		t.Fatalf("reading ECHO.md: %v", err)
	}
	if !strings.Contains(string(b), "hello world") {
		t.Fatalf("ECHO.md missing prompt; got:\n%s", b)
	}
}
