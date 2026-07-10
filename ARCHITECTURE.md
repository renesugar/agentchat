# AgentChat Architecture

## What this app is

A desktop chat GUI whose "models" are **terminal coding agents**. Instead of
`chat → LLM HTTP API`, the flow is:

```
chat turn → adapter → spawn coding-client CLI non-interactively in a workspace
          → parse its output stream → normalized events → chat UI + transcript
          → snapshot workspace → artifacts
```

Per turn the user picks a **client** (claude, codex, aider, swival) and one of
the **models** that client supports. Different turns in one conversation may
use different clients working on the same workspace — that hand-off is the
point of the app.

## Key decisions

1. **Output capture is the baseline transport; MCP is optional.** Every
   supported client has a non-interactive mode whose stdout we can parse
   (JSON streams for claude/codex; line output + git commits for aider).
   Building on stdout works uniformly; an MCP callback channel is a later
   enhancement for clients that support it, never a requirement.

2. **Normalized event schema.** Adapters translate client-specific output
   into `adapter.Event` values (`text`, `thinking`, `tool_use`,
   `tool_result`, `file_change`, `plan`, `error`, `result`). The raw
   client payload is preserved in `Event.Raw` for debugging/export.

3. **Adapters are the heart.** `adapter.Adapter` is a small interface:
   availability check, model list, and `RunTurn(ctx, req, emit)`.
   Everything else (engine, store, UI) is client-agnostic.

4. **Snapshot between turns.** Because consecutive turns may be executed by
   different agents, the engine snapshots the workspace after every turn
   (even failed ones). Snapshots are taken through a temporary git index
   (add -A → write-tree → commit-tree → refs/agentchat/snapshots/<n>), so
   they capture untracked files and deletions while never touching the
   user's HEAD, index, branches, or worktree. This gives per-turn diffs in
   the transcript, rollback for owned workspaces, and makes export nearly
   free. When a client's output yields no structured file changes, the
   snapshot diff fills them in.

5. **Workspace kinds.**
   - `repo`: conversation bound to an existing local git repo (may have a
     GitHub/Gitea remote). Conversations are grouped by project = repo.
   - `worktree`: a git worktree created for the conversation, so parallel
     conversations don't fight over one checkout.
   - `scratch`: no repo association → a git-inited temp dir; the turn
     result can be packed as a ZIP so it is ready for the next turn or for
     download.

6. **Artifact library.** Content-addressed storage for user uploads and
   generated files. For very large repos, store a *link* (local path +
   optional remote URL as an archival fallback) rather than a ZIP copy.

7. **Export.** Any conversation can be exported as (a) a Markdown
   transcript and (b) a ZIP of artifacts + final snapshot — portable
   context for another coding client or another chat GUI.

8. **Headless-first.** `internal/` never imports Wails. A CLI harness
   (`cmd/agentchat-cli`) exercises the whole engine, which keeps the core
   testable in CI/sandboxes without a webview. The GUI lives in `app/`, a
   nested Go module that depends on the core via a replace directive; it
   is a thin binding layer (app/app.go) plus an embedded no-build vanilla
   frontend, and is deliberately outside the root module so `make check`
   never needs the Wails dependency.

9. **LocalAI's role.** LocalAI (and any OpenAI-compatible server) is a
   *model provider that coding clients point at* via base-URL config — it
   is not UI scaffolding for this app. Configure it as a provider env set
   in config.json (see docs/config.example.json); each client reads its
   own variables, so provider entries are explicit env maps rather than a
   magic base_url the app would have to translate per client.

10. **Dependencies.** Core stays stdlib-only as long as practical; `git`
    is used by shelling out (no cgo/go-git). Wails and any DB driver enter
    only at their steps, isolated from `internal/`.

## Package map

```
cmd/agentchat-cli/     headless harness: run one turn, print events
internal/adapter/      Event schema, TurnRequest/Result, Adapter iface, registry
internal/adapters/
  echo/                fake adapter used by tests and the harness
  claudecode/          Step 3
  codex/               Step 4
  aider/               Step 5
  swival/              Step 6
internal/transcript/   Store iface + FSStore: conversations, turns, event logs
internal/engine/       runs a turn via an adapter and persists it to the store
internal/workspace/    Step 7: repo/worktree/scratch + per-turn snapshots
internal/artifact/     Step 8: artifact library
app/                   Wails desktop app — NESTED module (own go.mod with a
                       replace to the core) so the root stays dependency-free;
                       vanilla JS frontend embedded from app/frontend/dist
docs/adapters.md       verified CLI invocations per client
```
