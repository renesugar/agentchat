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

- [ ] **Step 12 (optional) — MCP callback channel.** Expose an MCP server
  from the app so clients that support MCP (Claude Code, Codex) can push
  progress/artifacts directly. Output capture from Steps 3–6 remains the
  baseline; MCP is an enhancement, never a requirement.

- [ ] **Step 13 — Effort control.** Make reasoning effort a first-class
  per-turn setting alongside the model (may be implemented before Step 12).
  Today only swival honors effort, via `Extra["reasoning_effort"]`; the
  other clients have effort controls the adapters don't map.
  - `adapter.TurnRequest` gains `Effort string` (empty = client default).
    Values are passed through to the client, which owns validation; the
    common scale is none/low/medium/high (plus client-specific extremes
    like claude's xhigh/max). Adapters ignore unsupported values only if
    the client would reject the flag entirely — otherwise pass through
    and let the client's error surface as usual.
  - Adapter mapping (verify each flag against the installed client and
    record versions in docs/adapters.md, as with Steps 3-6):
    claude → `--effort <level>` at launch (levels low/medium/high/xhigh/
    max; note non-interactive `/effort` is "Not applied" on some models,
    so the launch flag is the only reliable path);
    codex → `-c model_reasoning_effort="<level>"` (verify exact config
    key with `codex exec --help` / docs);
    aider → `--reasoning-effort <level>` (best-effort; litellm forwards
    it only for models that support it);
    swival → `--reasoning-effort <level>` (migrate the existing
    Extra["reasoning_effort"] path to the new field, keeping Extra as an
    override for back-compat);
    echo → record the effort in ECHO.md so engine/UI tests can assert
    end-to-end plumbing.
  - Config: `clients.<name>.default_effort` applied by Config.Apply when
    the request doesn't set one (same precedence rule as Extra: per-turn
    wins). Optionally allow `models[].effort` later; not in scope now.
  - CLI: `-effort` flag on agentchat-cli.
  - GUI: an effort select in the composer next to the model picker
    (default option "client default"); `App.Run` gains the parameter and
    the transcript turn header shows the effort when one was set (store
    it on `transcript.Turn` so exports can include it — extend NewTurn/
    Turn and the markdown renderer).
  - Tests: buildArgs cases per adapter, config default-effort precedence,
    engine round-trip via echo, golden transcript update.

- [ ] **Step 14 — Per-turn copy.** Let users copy one turn as markdown
  without exporting the whole conversation.
  - `internal/export`: extract the private renderTurn into a public
    `TurnMarkdown(turn *transcript.Turn, events []adapter.Event) []byte`
    (same content as the full transcript's per-turn section: prompt,
    client/model/status, plan, response, file changes, snapshot/usage
    footer); `Markdown()` calls it so the two never drift.
  - App binding `TurnMarkdown(convID, turnID) (string, error)`.
  - GUI: a small "Copy" button in each turn header (visible on hover is
    fine) that fetches TurnMarkdown and writes it to the clipboard via
    navigator.clipboard.writeText (fall back to a hidden textarea +
    execCommand if the webview denies the API), with a toast on success.
  - CLI: `-export-turn <seq>` alongside -export-md (writes/prints one
    turn's markdown).
  - Tests: TurnMarkdown golden section (reuse the Step 9 fixtures);
    assert Markdown() output contains exactly the TurnMarkdown output
    for each turn.

- [ ] **Step 15 — Bundle import (round-trippable bundles + conversation
  delete).** Users can import a previously exported bundle — their own or
  one shared by another user.
  - Extend `export.Bundle` to be machine-readable, keeping the current
    human-readable contents: add `bundle.json` (format version, app
    version, conversation ID/title, export time) and `data/` containing
    the raw store subtree for the conversation (conversation.json +
    turns/<seq>-<id>/{turn.json,events.jsonl}) plus `data/artifacts/`
    with the conversation's artifact index records (blob content is
    already under artifacts/ in the bundle; links stay links).
    transcript.md remains the human view. Bump nothing for old bundles:
    Import rejects bundles without bundle.json with a clear "this bundle
    predates import support; re-export it" error.
  - `export.Import(ctx, store, lib, mgr, bundlePath)`:
    - **Collision rule: if the conversation ID already exists in the
      store, refuse and change nothing** (error names the existing
      conversation and its title). No merge, no overwrite. (A
      "duplicate as new conversation" option can come later; out of
      scope now.)
    - If the ID is absent (e.g. the user deleted the conversation),
      restore: copy the data/ subtree into the store, re-create artifact
      records (skip records whose ID already exists — content is
      identical; file blobs naturally dedupe by hash in the CAS), and if
      workspace.zip is present, materialize it into a fresh scratch
      workspace (git init + initial commit of the imported tree; the
      original snapshot refs are not recoverable from a git archive, so
      turn SnapshotIDs from before the export remain historical
      references — note this in the imported conversation via a link
      artifact or bundle.json note). Associate the new workspace so the
      next turn continues from the imported tree.
  - Conversation deletion (prerequisite for the re-import flow):
    `Store.DeleteConversation(ctx, id)` on the interface + FSStore
    (removes the conversation subtree; artifacts are NOT deleted — they
    may be shared/exported; a later step can add orphan cleanup), App
    binding + a delete action in the sidebar with a confirm step, CLI
    `-delete-conv <id>`.
  - GUI: "Import bundle" button (native open dialog) in the sidebar;
    on success select the imported conversation; on collision show the
    refusal message.
  - CLI: `-import-bundle <file>`.
  - Tests: export → delete → import round trip (turns, events, artifacts
    byte-identical; workspace tree restored and usable for a next turn);
    import with existing ID → error and store untouched (assert
    conversation.json mtime/content unchanged); old-format bundle →
    clear rejection; artifact blob dedupe on import.

- [ ] **Step 16 — Sidebar project groups & creation picker.** Make project
  grouping first-class in the UI (Firefox-tab-group interaction model).
  - GUI sidebar: each project renders as a collapsible group — a header
    row (disclosure caret + project name + conversation count) that
    toggles the group open/closed on click. Conversations with no
    project are NOT grouped: they render as individual top-level items
    interleaved after the project groups (replace the current lumped
    "Scratch" header). Collapse state persists across sessions via small
    App bindings `UIState()`/`SetUIState(json)` backed by
    `<data>/ui-state.json` (avoid webview localStorage quirks).
  - Backend: `App.Projects()` returning distinct known projects derived
    from conversations — `{path, label (basename), count}` — no separate
    project registry; conversations remain the source of truth.
  - New-conversation form: a project select populated from Projects():
    "No project (scratch)", one entry per known project, and "Other
    repo…" which reveals the existing path input + directory picker.
    Selecting a known project passes its path to CreateConversation
    (which already handles it).
  - Tests: Projects() derivation (dedupe, counts, scratch excluded);
    frontend logic is exercised on a real machine per Step 10's caveat.

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
