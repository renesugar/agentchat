package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrphans(t *testing.T) {
	ctx := context.Background()
	lib, err := NewLibrary(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Same content across conversations: one shared CAS blob.
	for _, m := range []Meta{{ConversationID: "gone"}, {ConversationID: "gone"}, {ConversationID: "alive"}} {
		if _, err := lib.AddFile(ctx, "n.txt", strings.NewReader("shared"), m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lib.AddLink(ctx, "global", "/tmp/x", "", Meta{}); err != nil {
		t.Fatal(err)
	}

	exists := func(id string) bool { return id == "alive" }
	orphans, err := lib.Orphans(ctx, exists)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 2 {
		t.Fatalf("orphans = %d, want 2 (globals and live conversations excluded)", len(orphans))
	}
	for _, o := range orphans {
		if o.ConversationID != "gone" {
			t.Fatalf("wrong orphan: %+v", o)
		}
		if err := lib.Delete(ctx, o.ID); err != nil {
			t.Fatal(err)
		}
	}

	// The live record and the global survive; the shared blob survives
	// because the live record still references it.
	rest, err := lib.List(ctx, "")
	if err != nil || len(rest) != 2 {
		t.Fatalf("remaining = %v, %v", rest, err)
	}
	blobs := 0
	filepath.WalkDir(filepath.Join(lib.Root(), "cas"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs != 1 {
		t.Fatalf("blobs = %d, want 1 (still referenced by the live record)", blobs)
	}
}
