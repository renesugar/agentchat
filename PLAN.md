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

- [ ] **Step 3 — Claude Code adapter.** `internal/adapters/claudecode`.
  Non-interactive invocation: `claude -p --output-format stream-json
  --verbose` (plus `--model`, `--resume <session>` when continuing).
  Parse the stream-json events into the normalized schema. **Do not call
  the real binary in unit tests** — use recorded fixture streams under
  `testdata/`. Detect availability via `exec.LookPath("claude")`.

- [ ] **Step 4 — Codex adapter.** `internal/adapters/codex`. Non-interactive:
  `codex exec --json` (verify current flags against `codex exec --help` at
  implementation time; pin what you find in `docs/adapters.md`). Fixture
  tests as in Step 3.

- [ ] **Step 5 — Aider adapter.** `internal/adapters/aider`. Non-interactive:
  `aider --message <prompt> --yes-always --no-stream` plus `--model`.
  Aider's output is line-oriented text + git commits, not JSON: emit
  `text` events for output and synthesize `file_change` events from the
  git diff of the commit(s) aider makes. Fixture tests.

- [ ] **Step 6 — Swival adapter.** `internal/adapters/swival`. Check
  swival's non-interactive/print mode and model flags from its docs/help
  at implementation time; record findings in `docs/adapters.md`. Fixture
  tests.

- [ ] **Step 7 — Workspace manager.** `internal/workspace`. Workspace kinds:
  `repo` (existing local git repo), `worktree` (created per conversation
  from a repo), `scratch` (no repo → git-init a temp dir; ZIP snapshot per
  turn). Auto-snapshot: commit (or record) the tree after every turn so
  each turn has a pinned tree hash; expose per-turn diffs. Shell out to
  `git` (no cgo, no go-git dependency). Tests use throwaway repos in
  `t.TempDir()`.

- [ ] **Step 8 — Artifact library.** `internal/artifact`. Content-addressed
  store under the data dir for user uploads and client outputs. Large-repo
  policy: store a *link* record (local path + optional remote URL) instead
  of copying; ZIP snapshots reference turns. Tests.

- [ ] **Step 9 — Export.** Markdown transcript of a whole conversation
  (prompt/response per turn, file-change summaries, per-turn tree hashes)
  and a ZIP bundle (transcript + artifacts + final workspace snapshot),
  suitable as context for another coding client or chat GUI. Tests golden-
  file the markdown.

- [ ] **Step 10 — Wails shell & chat frontend.** Initialize Wails v2 app
  (`wails init -n agentchat -t svelte` or react; document the choice).
  Bind: list adapters/models, start turn, stream events (Wails events),
  conversation list grouped by project/repo, artifact panel, export
  buttons. This step introduces the Wails dependency; keep `internal/`
  buildable without it (`make check` must still pass headless).

- [ ] **Step 11 — Providers & config.** Config file (`config.yaml` or JSON)
  for provider base URLs / API keys env passthrough, including
  OpenAI-compatible endpoints such as LocalAI, and per-adapter model lists.
  Surface in the GUI model picker.

- [ ] **Step 12 (optional) — MCP callback channel.** Expose an MCP server
  from the app so clients that support MCP (Claude Code, Codex) can push
  progress/artifacts directly. Output capture from Steps 3–6 remains the
  baseline; MCP is an enhancement, never a requirement.

## Definition of done for any step

1. `make check` passes (fmt, vet, test).
2. `PLAN.md` status updated; any discovered CLI flags or deviations recorded
   in `docs/adapters.md` or `ARCHITECTURE.md`.
3. A git commit on `main` with a message `step N: <summary>`.
