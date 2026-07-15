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
   Building on stdout works uniformly; the MCP callback channel
   (`internal/mcpserver`, Steps 12 + 25) is an enhancement for clients
   that support it, never a requirement: a loopback streamable-HTTP MCP
   server offers `progress`, `add_artifact`, and `get_turns` (the
   conversation transcript as markdown — the app owns transcript/turn/
   artifact state, and a client can pull the last n turns or all of
   them to orient itself mid-turn; rendered with export.TurnMarkdown so
   it matches exports byte-for-byte). Each turn gets a revocable bearer
   token, pushes join the turn's normal event stream, and GET /context
   on the same listener is a REST twin of get_turns for clients without
   MCP support. A per-turn system-prompt fragment (Step 26) tells each
   client how to reach all of this — delivered via the client's own
   mechanism (claude --append-system-prompt, codex -c
   developer_instructions, swival --system-prompt, aider a --read
   file); the bearer token travels only in the process environment
   (AGENTCHAT_MCP_TOKEN), never argv or prompt text. Stdlib-only
   (net/http; plain JSON responses — no SSE needed since the server
   never initiates messages).

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

   A scratch workspace can be **promoted** into a project
   (`Manager.PromoteScratch`): the whole directory — including `.git`
   and the `refs/agentchat/snapshots` chain — moves to a user-chosen
   location as a plain directory move (rename, or copy+verify+remove
   across filesystems), so SnapshotIDs recorded on past turns stay
   valid; the conversation is then re-associated via
   `Store.SetConversationProject`. Conversations can likewise be moved
   between existing projects: only the association changes — future
   turns resolve to the new project repo, past turns keep their
   historical workspace refs and snapshots.

6. **Artifact library.** Content-addressed storage for user uploads and
   generated files. For very large repos, store a *link* (local path +
   optional remote URL as an archival fallback) rather than a ZIP copy.

7. **Export & import.** Any conversation can be exported as (a) a
   Markdown transcript and (b) a ZIP bundle of artifacts + final
   snapshot — portable context for another coding client or another
   chat GUI. Bundles are round-trippable (Step 15): bundle.json + the
   raw store subtree under data/ let `export.Import` restore a deleted
   or shared conversation byte-identically, re-create artifact records
   (blobs dedupe in the CAS), and materialize the bundled snapshot tree
   into a fresh scratch workspace. Imports never overwrite: an existing
   conversation ID is refused.

8. **Headless-first.** `internal/` never imports Wails. A CLI harness
   (`cmd/agentchat-cli`) exercises the whole engine, which keeps the core
   testable in CI/sandboxes without a webview. The GUI lives in `app/`, a
   nested Go module that depends on the core via a replace directive; it
   is a thin binding layer (app/app.go) plus an embedded no-build vanilla
   frontend, and is deliberately outside the root module so `make check`
   never needs the Wails dependency.

9. **Providers & secrets.** A provider (`internal/provider`, Step 27) is
   a named way for a client to reach models. Every client's catalog
   starts with a builtin default — claude/codex: the user's own
   subscription (inject nothing); aider/swival: the inherited
   environment — followed by config.json providers (explicit env maps,
   optionally base_url + api_key_env). For codex, the usable providers
   are the ones declared in ~/.codex/config.toml `[model_providers.*]`,
   parsed READ-ONLY with a minimal stdlib line-based TOML-subset reader
   (no dependency added; codex validates its own config — our reader
   only extracts name/base_url/env_key and skips the rest); same-named
   config.json entries overlay them (key secrets, models, labels).
   API-key VALUES never live in config files, argv, or transcripts:
   config carries only platform secret-store *lookup attributes*
   (Linux: secret-tool; other OS backends are future work) and
   Def.ResolveEnv fetches the value per turn, injecting it as the
   provider's env key. LocalAI or any OpenAI-compatible server is just
   such a provider entry — each client reads its own variables, so env
   maps stay explicit rather than a magic base_url the app translates.

10. **Dependencies.** Core stays stdlib-only as long as practical; `git`
    is used by shelling out (no cgo/go-git). Wails and any DB driver enter
    only at their steps, isolated from `internal/`.

11. **Client config isolation.** AgentChat never writes (or causes
    clients to write) user configuration: no ~/.claude*, ~/.codex/*
    (read-only provider parse only), .aider.conf.yml, or swival
    config. Per-turn settings travel exclusively via argv flags,
    per-invocation config overrides, stdin, process env, and temp
    files; all app writes stay inside the data dir, managed
    workspaces, os.TempDir, and user-chosen dialog targets. Enforced
    by audit (docs/adapters.md) and HOME-isolation tests.

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
internal/mcpserver/    Steps 12+25: loopback MCP/REST callback server
                       (progress, artifacts, conversation context)
internal/theme/        Step 21: UI color themes (built-in + user JSON files)
internal/provider/     Step 27: provider catalogs (config + read-only codex
                       config.toml) and platform secret-store key resolution
app/                   Wails desktop app — NESTED module (own go.mod with a
                       replace to the core) so the root stays dependency-free;
                       vanilla JS frontend embedded from app/frontend/dist
docs/adapters.md       verified CLI invocations per client
```
