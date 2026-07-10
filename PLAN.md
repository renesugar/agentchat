# AgentChat — Implementation Plan

AgentChat is a desktop chat GUI (Wails, Go + web frontend) that orchestrates
**terminal coding agents** (Claude Code, Codex CLI, Aider, Swival) instead of
talking to an LLM server directly. Each conversation turn runs one coding
client non-interactively against a workspace (a git repo, worktree, or ZIP
snapshot), streams its output into the chat, and records artifacts.

See `ARCHITECTURE.md` for design decisions. See `AGENTS.md` for rules that
implementing agents (Claude Code / Codex) must follow.

## Status legend

- `[x]` done — implemented, compiling, tested
- `[~]` in progress
- `[ ]` not started

Every step must leave `make check` green. One step per session is fine;
usage limits may interrupt work, so **never start a step you can't finish
in a compiling state**.

## Steps

- [x] **Step 1 — Scaffold & core contracts.** Repo layout, docs, Makefile,
  normalized event schema (`internal/adapter`), the `Adapter` interface,
  adapter registry, a fake `echo` adapter for tests, and a headless CLI
  harness (`cmd/agentchat-cli`) so the engine is runnable before any GUI
  exists. Stdlib only. Unit tests for event schema + registry + echo turn.

- [x] **Step 2 — Turn engine & transcript store.** `internal/transcript`:
  `Store` interface + `FSStore` (JSON + JSONL under `-data`, `$AGENTCHAT_DATA`,
  or `~/.agentchat`; layout: `conversations/<id>/conversation.json` +
  `turns/<seq>-<id>/{turn.json,events.jsonl}`), designed so SQLite can
  replace it later. `internal/engine`: composes registry + store, persists
  events as they stream, records result/error on the turn. CLI harness now
  creates/continues conversations (`-conv`, `-conversations`, `-data`).
  Tests round-trip conversations, the turn lifecycle, and an engine run.

- [x] **Step 3 — Claude Code adapter.** `internal/adapters/claudecode`.
  Non-interactive: `claude -p --output-format stream-json --verbose
  [--model] [--resume] --permission-mode acceptEdits -- <prompt>`.
  Parser (stream.go) is separate from process execution and covered by
  fixtures in `testdata/` (success, error, garbage-line tolerance);
  RunTurn is covered via a stub shell script standing in for the binary.
  Registered in the CLI harness. Verified live against claude 2.1.206
  (turn + resume; no stream drift) — see docs/adapters.md.

- [x] **Step 4 — Codex adapter.** `internal/adapters/codex`. Non-interactive:
  `codex exec --json --sandbox workspace-write --skip-git-repo-check
  [--model] [resume <thread_id>] -` with the prompt on stdin. Parser
  covers thread/turn/item JSONL events (agent_message, reasoning,
  command_execution, file_change, mcp_tool_call, web_search, todo_list,
  error; reconnect notices non-fatal; legacy item_type key accepted).
  Fixture + stub-binary tests as in Step 3. Verified live against
  codex-cli 0.142.5 (stream format + resume flag placement; missing
  sessions now fail loudly) — see docs/adapters.md.

- [x] **Step 5 — Aider adapter.** `internal/adapters/aider`. Non-interactive:
  `aider --message <prompt> --yes-always --no-stream --no-pretty
  [--model] [--restore-chat-history]`. Heuristic line parser (prose →
  text events, Applied-edit → file_change, Commit → tool_result,
  Tokens/Cost → usage; banner noise suppressed). Authoritative file
  changes derived from `git diff --name-status` of HEAD before/after,
  with Applied-edit fallback outside a repo. Fixture test plus
  stub-binary tests against a real temp git repo. No session IDs —
  continuity via aider's history files (see docs/adapters.md).

- [x] **Step 6 — Swival adapter.** `internal/adapters/swival`. One-shot
  mode with the task on stdin and `--report <tmpfile>`; stdout (the final
  answer, per swival's contract) → text event, stderr diagnostics →
  thinking events, report JSON → usage + fallback answer; file changes
  from a `git status --porcelain` before/after diff; documented exit
  codes handled (2 = turn limit → partial result + explicit error).
  Provider/base-url/profile via Extra. Report-fixture, porcelain-diff,
  and stub-binary tests. Flags + report schema verified live against
  swival 1.0.25 (aider flags likewise against 0.86.2) — details in
  docs/adapters.md.

- [x] **Step 7 — Workspace manager.** `internal/workspace`. Kinds: repo /
  worktree / scratch. Snapshots are non-invasive for ALL kinds (temp
  index → write-tree → commit-tree → refs/agentchat/snapshots/<n>; the
  user's HEAD, index, branches, and worktree are never touched — tested
  explicitly), capture untracked files and deletions, chain as parents,
  and report Changed=false for no-op turns. Diff (name-status -M),
  Restore (owned kinds only; refuses user repos), Zip (git archive) of
  any snapshot. Engine integration: turns run in a workspace get
  snapshotted even on failure, SnapshotID lands on the Turn (FinishTurn
  gained the param), and when an adapter reports no file changes the
  snapshot diff fills Result.FilesChanged authoritatively. CLI: -scratch
  creates a managed workspace; a git repo at -dir is auto-managed;
  snapshot hash printed per turn.

- [x] **Step 8 — Artifact library.** `internal/artifact`. Content-addressed
  store (SHA-256 CAS: `cas/<aa>/<hash>`, one JSON record per artifact in
  `index/`): AddFile streams + hashes + dedupes blobs across records,
  AddLink stores large-repo references (local path + optional remote URL
  as archival fallback; dangling links surface the fallback on Open),
  Get/Open/BlobPath/List (newest-first, filter by conversation), Delete
  with blob GC when the last referencing record goes. Provenance fields:
  conversation, turn, origin, note. Consumed by Step 9 (export) and the
  Step 10 GUI artifact panel. Tests: round-trip, dedupe + GC, links,
  list filtering.

- [x] **Step 9 — Export.** `internal/export`. Markdown(): full-transcript
  document — per turn: prompt, client/model/status, last plan checklist,
  response text (event stream, falling back to Result.FinalText),
  file-change list, snapshot hash, usage/session; plus a conversation
  artifacts section (stored files and link records). Bundle(): ZIP with
  transcript.md, the conversation's file artifacts under artifacts/
  (links listed in artifacts/links.md, not copied), and workspace.zip +
  workspace.txt with the latest snapshot tree when a workspace is given.
  Golden-file test (IDs/dates/hashes normalized; regenerate with
  `go test ./internal/export/ -update`) and a bundle round-trip test.
  CLI: `-conv <id> -export-md f.md` / `-export-bundle f.zip` (bundle
  picks up the workspace from -dir when it's a managed repo).

- [x] **Step 10 — Wails shell & chat frontend.** `app/` is a NESTED Go
  module (wails v2 + a replace directive to the core) so `make check` on
  the root stays green in environments that can't fetch Wails/webkit.
  Frontend is plain HTML/CSS/JS embedded from `app/frontend/dist` — no
  npm, no bundler. Bindings (app/app.go): Adapters (availability +
  models), Conversations/Turns/Events, CreateConversation (repo picker
  via native dialog, or scratch), Run (streams every normalized event to
  the frontend as the Wails event "turn-event"; reuses the last session
  ID per client; one turn in flight per conversation), Artifacts +
  AttachFile, ExportMarkdown/ExportBundle via save dialogs (exports are
  recorded as link artifacts). Workspace resolution per conversation:
  cached → project repo → last turn's dir reopened → fresh scratch.
  UI: sidebar grouped by project, transcript ledger where each turn
  carries a colored spine keyed to its agent, live event streaming with
  an optimistic turn block replaced by the authoritative record,
  composer with client+model pickers (Ctrl+Enter runs), artifact panel,
  adapter availability footer. Verified on a real machine (2026-07-10):
  compiles with wails **v2.13.0** (pinned in app/go.mod, matching the
  installed wails CLI) with no binding-API fixes needed. Linux with only
  webkit2gtk-4.1 needs `-tags webkit2_41`; the root Makefile autodetects
  this via pkg-config for app-dev/app-build and the new app-build-check
  target (see app/README.md).

- [x] **Step 11 — Providers & config.** `internal/config` +
  `internal/clients`. JSON config at `<data>/config.json` (stdlib-only,
  so JSON not YAML; missing file = valid empty config, malformed file =
  loud error). Providers are named env-var sets with `${VAR}` expansion
  (secrets stay in the process env, e.g. a LocalAI endpoint via
  OPENAI_API_BASE); clients get a default provider, extra env, default
  TurnRequest.Extra (per-turn values win), binary overrides, and model
  picker additions (append-with-dedupe or replace_models). `clients.New`
  assembles the one registry both the CLI and the app use; `Set.Models`
  merges lists, `Set.Prepare` applies defaults before every turn. Example
  recipes (LocalAI, OpenRouter) in docs/config.example.json. Tests cover
  load/validation, expansion, apply precedence, model merging, and
  binary overrides.

- [x] **Step 12 (optional) — MCP callback channel.** `internal/mcpserver`:
  a stdlib-only MCP server (streamable HTTP, JSON-RPC over POST on a
  loopback listener) with two tools — `progress` (→ thinking event in
  the live turn stream) and `add_artifact` (→ artifact library, path
  confined to the workspace). Each turn gets a token-scoped channel
  (Authorization: Bearer; revoked when the turn ends) whose pushes flow
  through the engine's per-turn emit — persisted and streamed like any
  adapter event (emit is now mutex-serialized since MCP calls arrive on
  HTTP goroutines). `TurnRequest.MCP` carries {name,url,token}; the
  claude adapter maps it to `--mcp-config <inline json>` +
  `--allowedTools mcp__agentchat`, codex to `-c mcp_servers.agentchat.
  url/bearer_token_env_var` with the token in the env. aider/swival/echo
  ignore it — output capture stays the baseline, MCP is never required.
  Engine gains optional `MCP`/`ArtifactSink` fields; CLI wires them
  behind `-mcp` (default on), the app in NewApp (artifact origin
  "mcp"). Tests: mcpserver protocol suite, per-adapter buildArgs, and
  an engine round trip with a fake MCP-client adapter (progress event
  persisted, artifact path resolution + workspace-escape refusal, token
  revocation). Verified live: claude 2.1.206 called both tools over the
  channel end to end (docs/adapters.md).

- [x] **Step 13 — Effort control.** Reasoning effort is a first-class
  per-turn setting alongside the model. `adapter.TurnRequest.Effort`
  (empty = client default) passes through to the client, which owns
  validation. Adapter mapping (all verified against installed clients,
  versions in docs/adapters.md): claude → `--effort` (low…max;
  live-verified), codex → `-c model_reasoning_effort="…"` (key verified
  via --strict-config probing; value validated by the model, not at
  parse time), aider → `--reasoning-effort` (best-effort via litellm),
  swival → `--reasoning-effort` with Extra["reasoning_effort"] kept as
  back-compat override, echo → records `effort:` in ECHO.md for
  end-to-end tests. Config: `clients.<name>.default_effort` applied by
  Config.Apply (per-turn wins). Effort is stored on transcript.Turn,
  shown in the markdown turn header ("client (model, effort X)"), the
  GUI turn header, and offered as a composer select; CLI `-effort`;
  `App.Run(conv, client, model, effort, prompt)`. Tests: buildArgs per
  adapter, config precedence, engine echo round trip, golden update.

- [x] **Step 14 — Per-turn copy.** `export.TurnMarkdown(turn, events)
  []byte` is the public per-turn renderer (header with seq/client/
  model/effort/status, prompt, plan, response, file changes,
  snapshot/usage footer — the section now starts at "## Turn", with the
  "---" separator written by Markdown(), so a copied turn is clean
  standalone markdown); `Markdown()` embeds it verbatim so the two
  can't drift, pinned by TestTurnMarkdownEmbedded. App binding
  `TurnMarkdown(convID, turnID)`; GUI hover-visible "copy" button in
  each turn header (navigator.clipboard with hidden-textarea
  execCommand fallback, toast on success/failure). CLI: `-export-turn
  <seq>` with -conv prints one turn's markdown to stdout.

- [x] **Step 15 — Bundle import (round-trippable bundles + conversation
  delete).** Bundles now carry `bundle.json` (format 1, conversation
  ID/title, export time, bundled snapshot), `data/conversation/` (the
  raw store subtree, copied verbatim via FSStore.ConversationDir), and
  `data/artifacts/` (raw index records via Library.ExportRecord); the
  store subtree lives under data/conversation/ rather than data/ itself
  so store files and artifact records can't collide. transcript.md and
  artifacts/ stay the human view. `export.Import(ctx, store, lib, mgr,
  path)`: refuses missing-bundle.json bundles ("predates import
  support"), newer formats, and — changing nothing — ID collisions
  (error names the existing conversation's title); otherwise restores
  the subtree byte-identically (FSStore.ImportConversation, path-safety
  checked, half-imports rolled back), re-creates artifact records
  (Library.RestoreRecord skips existing IDs, verifies blob hashes,
  dedupes blobs in the CAS), and materializes workspace.zip into a
  fresh scratch workspace pinned by an initial snapshot + recorded as
  an origin-"import" link artifact (old turn SnapshotIDs remain
  historical references; durable re-association can use Step 17's
  promotion). Deletion prerequisite: Store.DeleteConversation (FSStore:
  removes the subtree, keeps artifacts), App binding + two-click
  confirm delete in the sidebar, CLI `-delete-conv`. GUI "Import"
  button (native dialog, selects the imported conversation, surfaces
  refusals as toasts); CLI `-import-bundle`. Tests: export → delete →
  import round trip (byte-identical subtree, surviving artifacts, echo
  turn runs in the restored workspace), fresh-machine import (records
  re-created, identical content → one CAS blob), collision refusal
  (store byte-untouched), old-bundle rejection. Verified on real data
  via the CLI.

- [x] **Step 16 — Sidebar project groups & creation picker.**
  `transcript.Projects(convs)` derives distinct projects ({path, label
  = basename, count}, sorted by label then path; scratch excluded) so
  the derivation lives in the core and is tested by root `make check`;
  `App.Projects()` wraps it. Sidebar: each project is a collapsible
  group (caret + label + count header, click/Enter toggles), and
  conversations without a project render as plain top-level items after
  the groups (the lumped "Scratch" header is gone). Collapse state
  persists via `UIState()`/`SetUIState(json)` backed by
  `<data>/ui-state.json` (webview localStorage is unreliable).
  New-conversation form: a project select — "No project (scratch)",
  each known project, "Other repo…" revealing the path input +
  directory picker — feeding CreateConversation. Tests: Projects()
  dedupe/counts/scratch-exclusion/label edge cases; frontend exercised
  on a real machine per the Step 10 caveat.

- [ ] **Step 17 — Promote conversation to project & move between
  projects.** A scratch conversation can become a project; conversations
  can be re-associated with existing projects.
  - Store: add `SetConversationProject(ctx, id, projectPath string)` to
    the Store interface + FSStore (updates conversation.json, bumps
    UpdatedAt; empty path detaches). Tests.
  - Workspace: `Manager.PromoteScratch(ctx, ws, targetDir)` — relocate a
    scratch workspace to a user-chosen directory as the project repo.
    Requirements: targetDir must not exist (or be empty); move the whole
    directory INCLUDING .git — a plain directory move, not `git clone`,
    so the refs/agentchat snapshot chain and turn SnapshotIDs stay valid
    (rename when same filesystem, copy+verify+remove across
    filesystems); returned workspace has Kind repo and the new Dir.
    Refuse for non-scratch kinds. Test asserts snapshot refs and
    Diff(oldSnapshotID, ...) still work in the new location.
  - App bindings + GUI:
    - "Create project from this conversation…" action on a scratch
      conversation (native directory save dialog → PromoteScratch →
      SetConversationProject → refresh workspace cache and sidebar; the
      conversation now appears under its new project group, and the new
      project is offered in the Step 16 creation picker).
    - "Move to project…" on any conversation (choose from Projects() or
      pick a repo): calls SetConversationProject only; future turns
      resolve to the project repo via workspaceFor (which already
      prefers ProjectPath) — prior turns keep their historical
      WorkspaceRefs/SnapshotIDs untouched. If the conversation had a
      scratch workspace, it is left in place as history (a later cleanup
      step may offer removal).
  - CLI: `-set-project <path>` and `-promote <targetDir>` with `-conv`.
  - Update ARCHITECTURE.md workspace-kind notes to describe promotion.
  - Tests: store update round trip; promote preserves snapshots; moving
    a conversation changes grouping and future workspace resolution
    (engine test: next turn runs in the project repo).

## Definition of done for any step

1. `make check` passes (fmt, vet, test).
2. `PLAN.md` status updated; any discovered CLI flags or deviations recorded
   in `docs/adapters.md` or `ARCHITECTURE.md`.
3. A git commit on `main` with a message `step N: <summary>`.
