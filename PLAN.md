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

- [x] **Step 17 — Promote conversation to project & move between
  projects.** Store: `SetConversationProject(ctx, id, path)` on the
  interface + FSStore (updates conversation.json, bumps UpdatedAt;
  empty detaches). Workspace: `Manager.PromoteScratch(ctx, ws, target)`
  moves the whole directory INCLUDING .git (rename; across filesystems
  copy + verify the snapshot refs in the copy + remove source), refuses
  non-scratch kinds and non-empty targets (an existing empty dir is
  fine), returns a Kind-repo workspace at the new Dir; plus
  `Manager.OpenScratch(dir)` so a reopened scratch (app restart, CLI)
  keeps its scratch identity — only dirs under the manager's scratch
  root qualify, so user repos never gain owned-workspace rights. App:
  `PromoteConversation` (save dialog → PromoteScratch →
  SetConversationProject → cache refresh; refused mid-turn) and
  `MoveConversation` (association only; validates the repo; past turns
  keep historical refs; an old scratch stays on disk as history);
  workspaceFor now reopens managed scratch dirs via OpenScratch. GUI:
  "Make project…" (scratch conversations) and "Move…" (picker: scratch /
  known projects / other repo) in the conversation header; sidebar
  regrouping and the Step 16 creation picker follow automatically. CLI:
  `-promote <newDir>` and `-set-project <path|->` with `-conv`.
  ARCHITECTURE.md workspace-kind notes describe promotion. Tests: store
  round trip, promote preserves snapshots + cross-snapshot diffs +
  further snapshots chain on, copyTree (perms/symlinks), OpenScratch
  acceptance/refusals, engine-level move (next turn runs in the project
  repo, history untouched, Projects() grouping follows). Promote/move
  also smoke-tested on real data via the CLI.

- [ ] **Step 18 — Client-config isolation audit.** AgentChat must NEVER
  write (or cause clients to write) the user's client configuration
  files; all per-turn settings must travel via command-line flags,
  process environment, or per-invocation overrides. Codex is already
  audited and documented (docs/adapters.md: everything goes through
  `-c key=value` per-invocation overrides; ~/.codex/* is never touched).
  Extend the same audit + written guarantee to the remaining clients:
  - claude: verify `--mcp-config` (inline JSON), `--allowedTools`,
    `--permission-mode`, `--effort`, `--model` are session-scoped and
    never persist to ~/.claude.json / ~/.claude/settings.json /
    .claude/settings*.json in the workspace; confirm no adapter path
    writes those files.
  - aider: adapter passes flags only (--message, --model,
    --reasoning-effort, ...) — verify no flag we use causes aider to
    write .aider.conf.yml or .env; note that aider DOES write its own
    history files (.aider.chat.history.md etc.) in the workspace, which
    is its normal per-workspace state, not configuration.
  - swival: flags + stdin only; verify .swival/ workspace state is the
    only thing written and no user-level config is created/modified.
  - echo: trivially clean (writes only ECHO.md in the workspace).
  - Grep-level audit of the repo for writes outside <data> and the
    workspace (os.WriteFile/Create/Rename/MkdirAll call sites) — the
    only allowed write roots are the AgentChat data dir, the managed
    workspace/scratch dirs, os.TempDir, and user-chosen export/promote
    targets from dialogs.
  - Record the per-client guarantee in docs/adapters.md (mirroring the
    codex note) and add a "Client config isolation" section to
    ARCHITECTURE.md.
  - Tests where cheap: extend the existing stub-binary RunTurn tests to
    run with HOME pointed at a temp dir and assert no config files
    appear there after a turn.

- [x] **Step 19 — Configurable model & effort pickers.** Both dropdowns
  are per-client and user-configurable. `adapter.EffortLister`
  (optional capability; core Adapter interface unchanged) lets
  adapters advertise the levels verified against their installed
  clients: claude low/medium/high/xhigh/max, codex
  minimal/low/medium/high/xhigh, aider low/medium/high (free-form
  pass-through), swival none/minimal/low/medium/high/xhigh/default,
  echo low/medium/high (plumbing tests). Config:
  `clients.<name>.efforts` + `replace_efforts` mirror the models
  mechanism (append-with-dedupe, or replace); `Config.Efforts` +
  `Set.Efforts` merge capability + config. App: AdapterInfo gains
  Efforts; the GUI effort select is populated per selected client
  (option "" = client default, previous pick kept when still offered)
  and repopulates on client change like the model select — the
  hardcoded list in index.html is gone. CLI `-list` prints efforts.
  Recipes in config.example.json; note in adapters.md. Tests: config
  merge/replace/dedupe, Set.Efforts via echo + claude, unknown client.

- [ ] **Step 20 — Desktop conventions: native menus, layout, Settings
  dialog.** Follow standard desktop-app conventions.
  - Native application menu via wails v2 options.Menu: File (New
    conversation…, Import bundle…, Export transcript…, Export
    bundle…, Quit), Edit (standard roles so copy/paste work), View
    (toggle artifact panel, reload), Help (About). Menu items emit
    Wails events the frontend already handles; remove the equivalent
    always-visible buttons from the sidebar/header to reclaim space.
  - Conversation pane real estate: artifact panel becomes an overlay
    (not a layout column), header actions collapse into the menu /
    an overflow control, composer stays compact.
  - Chat transcript design pass: clearer prompt/response bubbles
    (user prompt right-aligned bubble, agent output full-width card
    keyed by the agent spine color), consistent spacing/typography.
  - Settings dialog (in-page modal styled like a native dialog; wails
    has no cross-platform native prefs window): shows data dir, config
    path, adapter availability; edits nothing in v1 — a read-only
    surface the theme/config work can grow into.
  - Frontend remains no-build vanilla JS; verify on a real machine via
    make app-dev.

- [ ] **Step 21 — Themes: light/dark from user-editable theme files.**
  - Theme = JSON file mapping the CSS custom properties the frontend
    already uses (--ink, --panel, --text, --muted, --line, agent
    colors, …). Ship built-in "agentchat-dark" and "agentchat-light"
    themes embedded in the app; user themes live in <data>/themes/
    *.json and override/extend the built-ins by name.
  - App bindings: `Themes()` (built-in + user), `Theme(name)` (resolved
    variable map), selection persisted in ui-state.json; frontend
    applies variables to :root at startup and on change; a
    View→Theme menu (Step 20) or Settings control switches them.
  - Fix the dropdown legibility bug while at it: <option> elements
    render with the platform default (white) background but inherited
    light text — set explicit color/background on select/option for
    both themes.
  - Tests: theme file parsing/validation (unknown keys tolerated,
    non-color values rejected), built-in themes complete (every CSS
    variable the frontend uses is defined — assert against a list),
    user override precedence.

- [ ] **Step 22 — Per-client API-key environment configuration.**
  Let users choose, per client, WHICH environment variable holds the
  API key — and whether one is used at all (subscription vs. API
  billing; cf. free-claude-code-style setups that redirect clients via
  env).
  - Config: `clients.<name>.api_key_env` — the NAME of a variable in
    AgentChat's process environment whose value is forwarded to the
    client as its canonical key variable (claude → ANTHROPIC_API_KEY,
    codex → OPENAI_API_KEY, aider/swival → provider-dependent, so for
    them api_key_env pairs with `api_key_var` naming the destination;
    default destination per adapter). Empty/absent = no key injected:
    claude/codex fall back to their own subscription login, exactly as
    today.
  - The key VALUE never lands in config.json (only the variable name),
    never in argv, and never in the transcript store — same rule as
    the MCP token. Works with keyring-sourced vars (secret-tool → env).
  - Interaction with providers/env (Step 11): api_key_env is applied
    by Config.Apply after provider/client env so it wins; document
    precedence.
  - AGENTS/ARCHITECTURE note: this must respect the Step 18 guarantee —
    env only, never client config files.
  - CLI/GUI: no new surface needed (config.json only) beyond showing
    "key: from $VAR / subscription" in the adapter footer/Settings.
  - Tests: Apply injects the mapped variable when set and omits it
    entirely when unset; precedence over provider env; example recipe
    in config.example.json (documented with a secret-tool pattern).

## Definition of done for any step

1. `make check` passes (fmt, vet, test).
2. `PLAN.md` status updated; any discovered CLI flags or deviations recorded
   in `docs/adapters.md` or `ARCHITECTURE.md`.
3. A git commit on `main` with a message `step N: <summary>`.
