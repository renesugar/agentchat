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

## Architecture

`app.go` is a thin binding layer over the headless engine:

- `Adapters()` — clients + availability + model lists
- `Conversations()`, `Turns(conv)`, `Events(conv, turn)` — transcript reads
- `CreateConversation(title, repoPath)` — repoPath "" → managed scratch
  workspace; a git repo path → snapshot-managed repo workspace
- `Run(conv, client, model, prompt)` — executes a turn; every normalized
  event is also pushed to the frontend as the Wails event `turn-event`
  with payload `{conversationId, event}`
- `ExportMarkdown(conv)` / `ExportBundle(conv)` — save-dialog exports
- `Artifacts(conv)` — the conversation's artifact records

Workspace resolution per conversation: cached handle → the conversation's
project repo → the last turn's workspace dir (reopened) → a fresh scratch
workspace. All turns are snapshotted by the engine (see ARCHITECTURE.md).
