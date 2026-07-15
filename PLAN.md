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

- [x] **Step 20 — Desktop conventions: native menus, layout, Settings
  dialog.** Native menu bar via wails options.Menu (built in
  app.go:applicationMenu): File → New Conversation (Ctrl+N), Import
  Bundle (Ctrl+O), Export Transcript (Ctrl+S), Export Bundle
  (Ctrl+Shift+S), Settings (Ctrl+,), Quit (Ctrl+Q); Edit role menu on
  macOS only (Linux/Windows webviews handle the shortcuts natively);
  View → Toggle Artifacts (Ctrl+L), Reload UI (F5); Help → About.
  Items emit the Wails event "menu" with an action string dispatched
  by the frontend onto the existing handlers; the equivalent
  always-visible buttons (sidebar Import; header Artifacts / Save
  transcript / Save bundle) are gone — the header keeps only the
  contextual Make project…/Move…. The artifact panel is now an overlay
  drawer over the transcript (top-right card), no longer a layout
  column stealing conversation height. Transcript design pass: the
  user's prompt renders as a right-aligned chat bubble (capped width,
  asymmetric radius, panel-2), agent output stays a full-width column
  under the agent-colored spine. Settings is an in-page <dialog>
  (showModal + ::backdrop; wails has no cross-platform native prefs
  window) showing the theme picker (moved from the Step 21 sidebar
  footer), data/config/themes paths via the new Info() binding, and
  client availability — read-only otherwise. Help → About is a small
  <dialog>. Frontend stays no-build vanilla JS; click-through on a
  real machine via make app-dev.

- [x] **Step 21 — Themes: light/dark from user-editable theme files.**
  `internal/theme` (stdlib + embed): a theme is a JSON file mapping the
  frontend's CSS custom properties (theme.RequiredVars: ink, panel,
  panel-2, line, text, muted, focus, danger, agent-*; fonts are not
  themeable). Built-ins agentchat-dark and agentchat-light are
  embedded; user files in <data>/themes/*.json override a built-in by
  name or add new themes extending one via "base" (default: the
  same-named built-in, else agentchat-dark — user themes are always
  complete). Values validated as CSS colors (hex/rgb[a]/hsl[a]/name;
  anything else — including CSS injection — is a loud per-file error);
  unknown JSON keys tolerated. App bindings Themes()/Theme(name)
  (loaded fresh per call so edits apply without restart); selection
  persisted in ui-state.json; the frontend applies variables to :root
  at startup/switch via a theme select in the sidebar footer (moves
  into the Step 20 View menu/Settings later). The hardcoded #e08a8a
  became --danger, and select/option now set explicit
  background/color — the dropdown-illegibility fix. Example user theme
  in docs/theme.example.json. Tests: built-ins complete vs.
  RequiredVars and actually differ, override/extend/base precedence,
  validation table (non-colors, injection, bad names, malformed JSON),
  List order/sources.

- [ ] **Step 22 — Per-client API-key environment configuration.**
  ⚠ SUPERSEDED by Step 27 (provider model, which covers key sourcing via
  platform secret stores and per-provider env). Do not implement
  separately; kept for the design notes below.
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

- [x] **Step 23 — Headless GUI testing with Xvfb + xdotool.** Harness
  recipe (works, reusable): `cd app && go build -tags
  desktop,production,webkit2_41 -o <scratch>/agentchat-gui .`;
  `Xvfb :99 -screen 0 1400x900x24 &`; `DISPLAY=:99
  AGENTCHAT_DATA=<scratch>/e2e-data agentchat-gui &`; drive with
  `DISPLAY=:99 xdotool mousemove X Y click 1` / `key ctrl+comma`;
  capture `xwd -root -silent` → ffmpeg → PNG; read clipboard with
  `xclip -o -selection clipboard`. Wait ≥3s after interactions — the
  software-rendered webview repaints slowly, and screenshots taken at
  1-2s produced FALSE "empty transcript" bugs (chased and disproved:
  tool/MCP-event conversations render perfectly).
  Verified: menu bar renders + Ctrl+comma/Ctrl+L accelerators fire;
  empty state clean after the [hidden] fix; full transcript rendering
  (spine, effort header, prompt bubble, tool/MCP events, footer);
  Settings dialog (theme picker, paths, clients); light theme applies
  live AND persists across app restart via ui-state; artifact overlay
  floats without squeezing the transcript; per-turn copy puts exact
  TurnMarkdown on the X clipboard (verified byte-for-byte with xclip).
  Fixed along the way: `[hidden] { display:none !important }` (display
  rules on containers beat the UA rule, leaking hidden UI onto the
  empty state); window error/unhandledrejection → toast; applyTheme
  now injects a real `:root{}` <style> rule instead of inline
  setProperty so native popup surfaces resolve theme variables through
  the cascade.
  Environment limits (NOT app bugs — retest on a real desktop or after
  installing a WM + compositor, e.g. openbox + xcompmgr): GTK popup
  surfaces (select dropdown lists, native menus) render as solid black
  under WM/compositor-less Xvfb (ARGB visuals without compositing), so
  option-list CONTENTS and File-menu contents could not be visually
  verified — the option/select CSS colors and per-client effort lists
  are covered by code + unit tests. Minor quirk to check on a real
  desktop: Escape didn't close the Settings <dialog> while the theme
  select had focus.

- [ ] **Step 24 — GUI popup verification under a real WM (openbox +
  xcompmgr are now installed).** Finish the visual checks Step 23
  could not do: GTK popup surfaces rendered solid black under bare
  Xvfb because there was no window manager or compositor.
  - Harness (extends the Step 23 recipe): start `Xvfb :99 -screen 0
    1400x900x24 &`, then `DISPLAY=:99 openbox &` and `DISPLAY=:99
    xcompmgr &` BEFORE launching the app; everything else (xdotool,
    xwd → ffmpeg → PNG, xclip, ≥3s waits) unchanged.
  - Verify: (a) select dropdown OPTION lists are legible in BOTH
    themes (agentchat-dark and agentchat-light) — this was the user's
    original complaint; (b) per-client effort dropdown contents change
    when switching client (aider: low/medium/high; claude adds
    xhigh/max; codex starts at minimal; swival has none…default);
    (c) File/View/Help menu CONTENTS and accelerator labels;
    (d) the Escape-in-Settings quirk (Escape while the theme select
    has focus did not close the <dialog> — decide whether it's webkit
    popup-consumes-Escape behavior or a bug worth a keydown handler).
  - Fix anything found; screenshot evidence per finding.

- [x] **Step 25 — Conversation context over MCP/REST.** The app owns the
  transcript, per-turn records, and artifacts; coding clients can now
  pull them mid-turn. `internal/mcpserver` gains the `get_turns` tool
  (optional `last_n`; omit/0 = full transcript) returning markdown —
  header "(n of m turns)" + per-turn sections rendered with
  export.TurnMarkdown so mid-turn context matches exports byte-for-byte;
  the in-flight turn appears with whatever events have streamed so far.
  REST twin `GET /context[?last_n=N]` on the same loopback listener and
  turn-scoped bearer token (text/markdown; 400 bad last_n, 401 bad
  token, 404 when no context source) for clients without MCP support.
  Wiring: `Sink.Context func(lastN int)`; the engine supplies it via
  renderContext (Store reads + TurnMarkdown) under the emit mutex so
  reads never race event appends from HTTP goroutines. Tests: tool +
  REST auth/trim/validation matrix in mcpserver; engine round trip
  where the fake client fetches full and last_n=1 context during turn 2
  over both MCP and REST and sees turn 1's content exactly when it
  should.

- [x] **Step 26 — Context bootstrap system prompt per client.**
  `TurnRequest.SystemPrompt` (extra system text, never replacing the
  client's own); the engine appends a per-turn fragment describing the
  get_turns tool, the GET /context REST endpoint (concrete URL), and
  the AGENTCHAT_MCP_TOKEN env var — the token itself never appears in
  prompt text or argv, and every adapter now injects it via the shared
  adapter.MCPEnv (codex's private helper generalized). Delivery, each
  VERIFIED against the installed binary: claude 2.1.206
  `--append-system-prompt` (✅ live: model quoted /context + env var
  back); codex 0.142.5 `-c developer_instructions=` with a proper
  tomlQuote (Go %q emits \xNN escapes TOML rejects; both `instructions`
  and `developer_instructions` pass --strict-config, the latter ✅
  live-verified via marker-instruction probe on gpt-5.4-mini); swival
  1.0.25 `--system-prompt`; aider 0.86.2 has NO system-prompt flag
  (`--system-prompt-extras` does not exist in this version — external
  docs wrong) → fragment travels as a temp file via `--read`, created
  and cleaned by RunTurn outside the workspace so snapshots never see
  it. Extra["context_bootstrap"]="false" suppresses the fragment.
  Tests: buildArgs per adapter, tomlQuote table, MCPEnv, engine round
  trip asserting fragment contents + no token leak + suppression.

- [x] **Step 27 — Provider model (core) + platform secrets.**
  `internal/provider` (stdlib-only): `Def` (Name "" = client default,
  mirroring model ID ""), `Default(client)` builtin entry (claude/codex
  "Subscription (default)" — injects nothing; others "Default
  (inherited environment)"), `Catalog(client, configDefs, codexDefs)` —
  builtin first; for codex only codex-declared providers are usable,
  with same-named config.json entries overlaid (key secrets, models,
  labels) and config-only names dropped; other clients get config defs
  as-is. `ReadCodexConfig` parses ~/.codex/config.toml READ-ONLY with a
  minimal line-based TOML-subset reader (tables, basic/literal strings,
  escapes; multiline strings consumed, arrays/inline tables skipped —
  no dependency added; codex validates its own config) extracting
  model_providers.* name/base_url/env_key plus top-level
  model_provider/model; $CODEX_HOME honored; verified against the real
  ~/.codex/config.toml (openai / gpt-5.6-sol, 0 custom providers).
  Secrets: config providers gain label/base_url/api_key_env/
  api_key_secret/clients/models (api_key_secret without api_key_env is
  a loud config error); `Def.ResolveEnv(ctx, store)` returns sorted
  ${VAR}-expanded env plus EnvKey=<secret> fetched per turn through the
  SecretStore interface — PlatformStore() is secret-tool on Linux
  (attrs sorted deterministically; missing tool/entry/empty = loud
  error; value only ever in the pipe, never argv/disk), a
  clearly-labeled unsupported stub elsewhere. secret-tool + the
  keyring's openrouter entry verified present on this machine (length
  only). Config.ProviderDefs(client) honors per-provider client
  restrictions. Tests: catalog merge/drop rules, ResolveEnv matrix,
  real exec path via stub secret-tool on PATH, codex TOML fixture
  (quoted table keys, comments, multiline, inline tables), value
  parser table, config field parsing/validation. Supersedes Step 22.
  Adapter/engine wiring is Step 28; pickers Step 29.

- [x] **Step 28 — Adapter provider wiring.** `TurnRequest.Provider
  *ProviderInfo` (callers set Name; `Set.Prepare` — now ctx-aware and
  error-returning — resolves it via the Step 27 catalog: fills
  BaseURL/Subscription, appends the provider's env with the API key
  fetched from the platform secret store; unknown names error listing
  what exists; nil/"" = client default, resolves nothing). Adapter
  mapping: claude → providerEnv injects ANTHROPIC_BASE_URL unless the
  env map already sets it (key/AUTH_TOKEN nuances live in config env
  maps); codex → `-c model_provider="<name>"` per invocation
  (subscription omits; ✅ live-verified the override is honored via a
  bogus-name probe); aider → env only; swival → native names to
  `--provider`, non-native+base_url to `--provider generic
  --base-url`, Extra keys winning for back-compat. CLI: `-provider`
  flag; `-list` now shows each client's provider catalog. App.Run
  passes no provider until the Step 29 pickers. ✅ Live end to end
  with the real keyring: secret-tool → OPENROUTER_API_KEY → aider →
  OpenRouter authenticated (free models upstream-rate-limited; 429
  with account identity proves the auth path; bad keys 401). Tests:
  Prepare resolution matrix (secret injection/failure, unknown names,
  client restrictions, codex catalog from a config.toml fixture),
  providerEnv, codex/swival buildArgs incl. precedence rules.

- [x] **Step 29 — Cascading pickers: Provider → Model → Effort.**
  `Set.ProvidersWithModels` fills each catalog entry's model list (a
  provider's own models get a leading client-default entry; providers
  without models fall back to the client's merged list) so
  AdapterInfo.Providers carries the whole tree and the frontend
  cascades client → provider → model → effort with no round trips.
  Composer gains the Provider select (labels; "" = subscription/
  default); client change repopulates providers, provider change
  repopulates models, picks survive when still offered. App.Run gains
  the provider param; the selected provider is recorded on the turn
  (transcript.Turn.Provider, mirroring effort) and shown as "via
  <name>" in the GUI turn header, optimistic header, and TurnMarkdown
  ("client (model, effort X, via Y)" — golden updated). Tests:
  ProvidersWithModels fallback/prepend rules, engine provider
  persistence. GUI verified via the harness under openbox+xcompmgr:
  four-select composer renders and focuses; dropdown POPUP contents
  remain the Step 24 item.

- [ ] **Step 30 — Composer redesign (per agentchat_lighttheme_new.png).**
  One rounded input bubble containing: the prompt textarea (grows to a
  cap, then scrolls internally), a bottom control row with [+] attach
  (native file/dir chooser; chosen paths render as removable reference
  chips inside the bubble and are appended to the prompt as
  workspace-relative references), the Coding Client / Provider /
  Model / Model Effort selects (Step 29), and a circular ↑ run button.
  Light/dark theme toggle moves into the conversation header (sun/moon
  switch per the mock) alongside Make project…/Move…. Keep the
  Settings-dialog theme select for full theme choice; the toggle flips
  between the built-in pair. Verify with the GUI harness in both
  themes.

## Notes for the next implementing agent (handoff, 2026-07-11)

State: steps 1-17 and 19-21, 23 are done and committed on main; `make
check` is green and the tree is clean. Remaining: **18** (client-config
isolation audit), **22** (per-client API-key env), **24** (above).
Everything below is hard-won context — trust it before re-deriving.

- **Workflow rules** (AGENTS.md still applies): one step per session,
  commit per step (`step N: summary` + the Co-Authored-By trailer the
  git log shows), update this file's checkbox in the same commit, and
  `make check` green before stopping. The user wants: a ZIP after each
  step (`make zip` → ../agentchat.zip) and to be ASKED before starting
  the next step. Usage limits interrupt sessions — never leave the
  tree broken.
- **Builds**: root module is stdlib-only; the Wails app is the nested
  module in app/ (wails pinned v2.13.0 to match the installed CLI —
  keep them in sync). `make app-build-check` compiles+vets it with the
  right tags; `make app-dev` runs it. The Makefile autodetects the
  webkit2_41 tag (this machine has only webkit2gtk-4.1); `wails
  doctor`'s complaint about webkit2gtk-4.0 is expected noise.
- **GUI harness**: full recipe in Step 23/24. Gotchas: use `pkill -x
  agentchat-gui` (`-f` matches your own shell and kills it); wait ≥3s
  after every interaction before screenshotting or you will chase
  phantom rendering bugs; bare-Xvfb popups render black without
  openbox+xcompmgr.
- **Test data**: the e2e data used so far lives in the previous
  session's scratchpad (path contains a session id — it may be gone).
  Recreate cheaply: `go run ./cmd/agentchat-cli -client echo -scratch
  -data <dir> "prompt"` for free turns; a claude haiku turn is cheap
  and exercises real streaming, but ask before spending the user's
  quota on many live turns.
- **Client configs are untouchable**: AgentChat must never write
  ~/.codex/*, ~/.claude*, .aider.conf.yml, etc. — per-invocation
  flags/env only. This is documented in docs/adapters.md (codex
  section) and is the whole point of Step 18. The user has been
  explicit and burned once; do not regress it.
- **Facts verified against installed clients** (don't re-litigate,
  versions in docs/adapters.md): claude 2.1.206 (`--effort
  low…max`, `--mcp-config` inline JSON works live), codex-cli 0.142.5
  (`model_reasoning_effort` config key; current models are the
  gpt-5.6-sol/-terra/-luna, gpt-5.5, gpt-5.4[-mini] lineup — old
  gpt-5-codex/gpt-5 IDs ERROR), aider 0.86.2, swival 1.0.25. The
  user's codex uses the openai provider + subscription; no API-key env
  vars are set for it (relevant to Step 22's design: absent
  api_key_env must mean "subscription auth", injecting nothing).
- **Step 18 pointers**: grep write call sites (os.WriteFile/Create/
  Rename/MkdirAll) — allowed roots are the data dir, managed
  workspaces, os.TempDir, and user-chosen dialog/export/promote
  targets; extend the stub-binary RunTurn tests to run with HOME set
  to a temp dir and assert no config files appear.
- **Step 22 pointers**: config gets `clients.<name>.api_key_env` (and
  for aider/swival an `api_key_var` destination override); mapping is
  applied in Config.Apply AFTER provider/client env so it wins; key
  VALUES never touch config.json/argv/transcripts — only the variable
  NAME is configured. claude→ANTHROPIC_API_KEY, codex→OPENAI_API_KEY
  destinations.

## Definition of done for any step

1. `make check` passes (fmt, vet, test).
2. `PLAN.md` status updated; any discovered CLI flags or deviations recorded
   in `docs/adapters.md` or `ARCHITECTURE.md`.
3. A git commit on `main` with a message `step N: <summary>`.
