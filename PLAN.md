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
  Registered in the CLI harness. Flags still need one verification pass
  against a live install — see docs/adapters.md.

- [x] **Step 4 — Codex adapter.** `internal/adapters/codex`. Non-interactive:
  `codex exec --json --sandbox workspace-write --skip-git-repo-check
  [--model] [resume <thread_id>] -` with the prompt on stdin. Parser
  covers thread/turn/item JSONL events (agent_message, reasoning,
  command_execution, file_change, mcp_tool_call, web_search, todo_list,
  error; reconnect notices non-fatal; legacy item_type key accepted).
  Fixture + stub-binary tests as in Step 3. Resume-flag caveats recorded
  in docs/adapters.md pending one pass against a live install.

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
  and stub-binary tests. Details and live-install TODOs in
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
  adapter availability footer. ⚠ VERIFY ON A REAL MACHINE: `make
  app-tidy && make app-dev` (see app/README.md); this sandbox cannot
  fetch the wails module, so the app module is gofmt/syntax-checked but
  not compiled — expect at most minor binding-API fixes, and pin the
  exact wails version go mod tidy resolves.

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

- [ ] **Step 12 (optional) — MCP callback channel.** Expose an MCP server
  from the app so clients that support MCP (Claude Code, Codex) can push
  progress/artifacts directly. Output capture from Steps 3–6 remains the
  baseline; MCP is an enhancement, never a requirement.

## Definition of done for any step

1. `make check` passes (fmt, vet, test).
2. `PLAN.md` status updated; any discovered CLI flags or deviations recorded
   in `docs/adapters.md` or `ARCHITECTURE.md`.
3. A git commit on `main` with a message `step N: <summary>`.
