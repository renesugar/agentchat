package export

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/example/agentchat/internal/artifact"
	"github.com/example/agentchat/internal/transcript"
	"github.com/example/agentchat/internal/workspace"
)

// Import restores a conversation from a bundle written by Bundle.
//
// Collision rule: if the bundle's conversation ID already exists in the
// store, Import refuses and changes nothing (the error names the existing
// conversation and its title). No merge, no overwrite.
//
// Otherwise the raw store subtree is copied back (byte-identical to the
// export), artifact records are re-created (records whose ID already
// exists are skipped — content is identical; file blobs dedupe by hash in
// the CAS), and, when the bundle carries workspace.zip and mgr is
// non-nil, the snapshot tree is materialized into a fresh scratch
// workspace, pinned with an initial snapshot, and recorded as a link
// artifact. The original snapshot refs are not recoverable from a git
// archive, so SnapshotIDs on imported turns remain historical references.
//
// The returned workspace (nil when the bundle had none) is where the next
// turn should run; associating it with the conversation is the caller's
// job.
func Import(ctx context.Context, store *transcript.FSStore, lib *artifact.Library, mgr *workspace.Manager, bundlePath string) (*transcript.Conversation, *workspace.Workspace, error) {
	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("export: opening bundle: %w", err)
	}
	defer zr.Close()

	info, err := readBundleInfo(zr)
	if err != nil {
		return nil, nil, err
	}
	id := info.ConversationID

	// Collision rule: refuse and change nothing.
	if existing, err := store.GetConversation(ctx, id); err == nil {
		return nil, nil, fmt.Errorf("export: conversation %s already exists (%q); delete it first if you want to re-import", id, existing.Title)
	} else if !errors.Is(err, transcript.ErrNotFound) {
		return nil, nil, err
	}

	sub, err := fs.Sub(zr, "data/conversation")
	if err != nil {
		return nil, nil, fmt.Errorf("export: bundle has no data/conversation: %w", err)
	}
	if err := store.ImportConversation(ctx, id, sub); err != nil {
		return nil, nil, err
	}
	// From here on, failures roll the conversation back so a botched
	// import leaves the store as it was. (Artifact records already
	// restored are idempotent and shared, so they stay.)
	fail := func(err error) (*transcript.Conversation, *workspace.Workspace, error) {
		_ = store.DeleteConversation(ctx, id)
		return nil, nil, err
	}

	if lib != nil {
		if err := restoreArtifacts(ctx, lib, &zr.Reader); err != nil {
			return fail(err)
		}
	}

	var ws *workspace.Workspace
	if mgr != nil {
		if wsEntry, err := zr.Open("workspace.zip"); err == nil {
			ws, err = materializeWorkspace(ctx, mgr, info, wsEntry, bundlePath)
			wsEntry.Close()
			if err != nil {
				return fail(err)
			}
			if lib != nil {
				_, _ = lib.AddLink(ctx, "imported-workspace", ws.Dir, "", artifact.Meta{
					ConversationID: id, Origin: "import",
					Note: "workspace restored from " + filepath.Base(bundlePath) +
						"; snapshot IDs on earlier turns are historical references from before the export",
				})
			}
		}
	}

	conv, err := store.GetConversation(ctx, id)
	if err != nil {
		return fail(err)
	}
	return conv, ws, nil
}

func readBundleInfo(zr *zip.ReadCloser) (*BundleInfo, error) {
	f, err := zr.Open("bundle.json")
	if err != nil {
		return nil, errors.New("export: this bundle predates import support (no bundle.json); re-export it with a current AgentChat")
	}
	defer f.Close()
	var info BundleInfo
	if err := json.NewDecoder(f).Decode(&info); err != nil {
		return nil, fmt.Errorf("export: parsing bundle.json: %w", err)
	}
	if info.Format > bundleFormat {
		return nil, fmt.Errorf("export: bundle format %d is newer than this AgentChat understands (%d); upgrade to import it", info.Format, bundleFormat)
	}
	if info.ConversationID == "" {
		return nil, errors.New("export: bundle.json has no conversation_id")
	}
	return &info, nil
}

// restoreArtifacts re-creates every record under data/artifacts/, feeding
// file blobs from the bundle's artifacts/ directory. Existing records are
// skipped inside RestoreRecord.
func restoreArtifacts(ctx context.Context, lib *artifact.Library, zr *zip.Reader) error {
	for _, f := range zr.File {
		name := f.Name
		if !strings.HasPrefix(name, "data/artifacts/") || !strings.HasSuffix(name, ".json") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}

		// Peek at the record to locate its blob in the bundle.
		var rec artifact.Artifact
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("export: parsing %s: %w", name, err)
		}
		var blob io.Reader
		if rec.Kind == artifact.KindFile {
			if b, err := zr.Open("artifacts/" + rec.ID + "-" + sanitizeName(rec.Name)); err == nil {
				defer b.Close()
				blob = b
			}
		}
		if _, _, err := lib.RestoreRecord(ctx, raw, blob); err != nil {
			return err
		}
	}
	return nil
}

// materializeWorkspace extracts the bundled snapshot tree into a fresh
// scratch workspace and pins it with an initial snapshot.
func materializeWorkspace(ctx context.Context, mgr *workspace.Manager, info *BundleInfo, wsZip io.Reader, bundlePath string) (*workspace.Workspace, error) {
	// workspace.zip is a nested archive: copy it out to seek in it.
	tmp, err := os.CreateTemp("", "agentchat-import-ws-*.zip")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, wsZip); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	inner, err := zip.OpenReader(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("export: opening workspace.zip: %w", err)
	}
	defer inner.Close()

	ws, err := mgr.CreateScratch(ctx, info.Title+" (imported)")
	if err != nil {
		return nil, err
	}
	if err := extractZip(&inner.Reader, ws.Dir); err != nil {
		return nil, err
	}
	if _, err := ws.Snapshot(ctx, "imported from "+filepath.Base(bundlePath)); err != nil {
		return nil, err
	}
	return ws, nil
}

// extractZip unpacks all regular entries into dstDir, refusing paths that
// would escape it.
func extractZip(zr *zip.Reader, dstDir string) error {
	for _, f := range zr.File {
		name := f.Name
		if f.FileInfo().IsDir() {
			continue
		}
		if !fs.ValidPath(name) {
			return fmt.Errorf("export: unsafe path %q in workspace.zip", name)
		}
		target := filepath.Join(dstDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
