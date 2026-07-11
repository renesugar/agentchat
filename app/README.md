# AgentChat desktop app (Wails v2)

This directory is a separate Go module so the headless core (`../internal`)
stays buildable in environments that cannot fetch Wails or webkit. The
frontend is plain HTML/CSS/JS served from `frontend/dist` — no npm, no
bundler, nothing to install besides Wails itself.

## Build & run (on a machine with network access)

    # prerequisites: Go 1.22+, Wails CLI, platform webview deps
    go install github.com/wailsapp/wails/v2/cmd/wails@latest
    wails doctor        # tells you what system packages are missing

    cd app
    go mod tidy         # fetches wails; the core comes via the replace directive
    wails dev           # live-reload development
    wails build         # production binary in build/bin/

Prefer the root Makefile targets — they handle the webkit build tag:

    make app-build-check   # compile + vet with production tags, no packaging
    make app-dev
    make app-build

On Linux distros that ship only webkit2gtk-4.1 (Ubuntu 24.04+, no
webkit2gtk-4.0 dev package), wails needs `-tags webkit2_41`; the Makefile
detects this via pkg-config. `wails doctor` may still warn about the
missing 4.0 package — that warning is harmless when the tag is used.

Pinned wails module version: **v2.13.0** (matches the wails CLI it was
verified with; keep the CLI and the `go.mod` requirement in sync when
upgrading). Verified compiling on Linux with webkit2gtk-4.1 (2.52.3),
Go 1.26.

## Architecture

`app.go` is a thin binding layer over the headless engine:

- `Adapters()` — clients + availability + model lists
- `Conversations()`, `Turns(conv)`, `Events(conv, turn)` — transcript reads
- `CreateConversation(title, repoPath)` — repoPath "" → managed scratch
  workspace; a git repo path → snapshot-managed repo workspace
- `Run(conv, client, model, effort, prompt)` — executes a turn (effort ""
  = client default); every normalized event is also pushed to the
  frontend as the Wails event `turn-event` with payload
  `{conversationId, event}`
- `ExportMarkdown(conv)` / `ExportBundle(conv)` — save-dialog exports
- `TurnMarkdown(conv, turn)` — one turn's markdown section (per-turn
  copy button in the transcript)
- `DeleteConversation(conv)` — removes turns+events (artifacts kept);
  refused while a turn is running
- `Projects()` — distinct projects derived from conversations
  ({path, label, count}); feeds the sidebar groups and creation picker
- `PromoteConversation(conv)` — save-dialog: move the scratch workspace
  (snapshot chain intact) to a new directory and make it the project
- `MoveConversation(conv, path)` — re-associate with a project repo
  ("" detaches); future turns run there, history stays untouched
- `UIState()` / `SetUIState(json)` — persisted frontend state
  (`<data>/ui-state.json`; collapsed project groups, selected theme)
- `Themes()` / `Theme(name)` — UI themes as CSS-variable maps. Built-in:
  agentchat-dark, agentchat-light. User themes are JSON files in
  `<data>/themes/` (same name = override a built-in; new name + "base"
  = extend one); format in docs/theme.example.json. Values are
  validated as CSS colors; loaded fresh per call so edits apply on the
  next theme switch without restarting.
- `ImportBundle()` — open-dialog restore of an exported bundle; refuses
  ID collisions, associates a restored workspace for this session
- `Artifacts(conv)` — the conversation's artifact records

Workspace resolution per conversation: cached handle → the conversation's
project repo → the last turn's workspace dir (reopened) → a fresh scratch
workspace. All turns are snapshotted by the engine (see ARCHITECTURE.md).
