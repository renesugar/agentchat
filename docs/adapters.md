# Coding-client CLI invocations

Provider endpoints (LocalAI, OpenRouter, ...) and per-client overrides
live in `<data>/config.json` — see `docs/config.example.json` and
`internal/config`. Recipes: aider → provider env (OPENAI_API_BASE +
OPENAI_API_KEY); swival → `extra: {provider: generic, base_url: ...}`;
claude/codex → their own env/config conventions.

Context bootstrap (Step 26): when the MCP/REST callback channel is on,
the engine appends a system-prompt fragment telling the client how to
fetch the conversation transcript (get_turns tool / GET /context) and
which env var carries the bearer token (AGENTCHAT_MCP_TOKEN — injected
into every client's process env, never argv or prompt text). Delivery
per client, each verified against the installed binary:
- claude 2.1.206 → `--append-system-prompt` (appends to the preset;
  `--system-prompt` would REPLACE it). ✅ live-verified: the model
  quoted the fragment's /context path and token env var back.
- codex 0.142.5 → `-c developer_instructions="…"` (TOML-quoted; both
  `instructions` and `developer_instructions` pass --strict-config, and
  developer_instructions was ✅ live-verified: an injected marker
  instruction was followed).
- swival 1.0.25 → `--system-prompt` ("System prompt to include").
- aider 0.86.2 → NO system-prompt flag exists (external docs claiming
  `--system-prompt-extras` are wrong for this version); the fragment
  travels as a temp file attached read-only via `--read` (aider itself
  cannot execute REST calls, so for aider the fragment is orientation
  text; inlining recent turns into that file is possible future work).
Disable per turn with Extra["context_bootstrap"]="false".

Model and effort pickers are per client: adapters advertise the levels
verified against their installed versions (see each section), and
config.json can append or replace both lists (`clients.<name>.models` /
`replace_models`, `clients.<name>.efforts` / `replace_efforts`).

Record here the *verified* non-interactive invocation for each client, with
the client version you verified it against. Planned invocations (unverified)
are marked ⚠ — verify with `<client> --help` before implementing the adapter
and update this file in the same commit.

## claude (Claude Code) — Step 3 ✅ implemented, ✅ verified live against claude 2.1.206 (2026-07-10)
    claude -p --output-format stream-json --verbose \
        [--model <id>] [--resume <session_id>] \
        --permission-mode acceptEdits -- "<prompt>"
- stream-json JSON lines handled: `system/init` (session_id), `assistant`
  (text / thinking / tool_use blocks), `user` (tool_result: string or
  typed-parts content), terminal `result` (final text, is_error,
  total_cost_usd, usage, duration_ms). Unknown line types are ignored;
  malformed lines become non-fatal `error` events.
- File changes are derived from Write / Edit / MultiEdit / NotebookEdit
  tool inputs (`file_path` / `notebook_path`), relativized to the
  workspace; "created" (Write) wins over later edits of the same path.
- Permission mode defaults to `acceptEdits`; override per turn via
  `Extra["permission_mode"]` ("" omits the flag entirely).
- Model aliases offered: sonnet / opus / haiku; any full model ID passes
  through.
- ✅ Verified live (claude 2.1.206, 2026-07-10): full turn + `--resume`
  round trip. `system/init` (session_id), `assistant` thinking/text
  blocks, and the terminal `result` line (result, is_error, duration_ms,
  usage) all match the parser; resume keeps the same session_id. New
  stream line types `rate_limit_event` and `system/thinking_tokens`
  appear and are ignored (unknown types are skipped by design) — no
  fixture drift.
- Effort (Step 13): `--effort <level>`, levels low/medium/high/xhigh/max
  (claude 2.1.206 --help; live-verified with `-effort low` through the
  CLI). The launch flag is the reliable path — the interactive /effort
  command is "Not applied" on some models in non-interactive runs.
- MCP callback (Step 12): when the engine provides a channel, the
  adapter adds `--mcp-config '{"mcpServers":{"agentchat":{"type":"http",
  "url":..., "headers":{"Authorization":"Bearer <token>"}}}}'` and
  `--allowedTools mcp__agentchat` (bare server name pre-approves its
  tools; -p mode cannot prompt). ✅ Verified live on claude 2.1.206:
  the client called `mcp__agentchat__progress` and
  `mcp__agentchat__add_artifact` end to end, and (2026-07-14, Step 25)
  `mcp__agentchat__get_turns` — it fetched the conversation transcript
  mid-turn and quoted turn 1's exact prompt text back.

## codex (Codex CLI) — Step 4 ✅ implemented, ✅ verified live against codex-cli 0.142.5 (2026-07-10)
    codex exec --json --sandbox workspace-write --skip-git-repo-check \
        [--model <id>] [resume <thread_id>] -
- Prompt is piped to stdin (the trailing `-`), avoiding quoting issues.
- Models: built-in list refreshed 2026-07-10 against a live codex
  environment — gpt-5.6-sol / gpt-5.6-terra / gpt-5.6-luna / gpt-5.5 /
  gpt-5.4 / gpt-5.4-mini (the earlier gpt-5-codex / gpt-5 IDs are gone
  and error now). Which IDs actually work still depends on the codex
  configuration; differently configured installs can replace the list
  via config.json `clients.codex.models` + `replace_models`.
- AgentChat NEVER writes codex configuration files (~/.codex/*): all
  per-turn settings (sandbox, model_reasoning_effort, the MCP callback
  server) go through `-c key=value` command-line overrides, which codex
  applies to that invocation only and does not persist.
- JSONL events handled: `thread.started`/`thread.resumed` (thread_id →
  session for resume), `turn.started`, `turn.completed` (usage: input +
  cached_input → InputTokens, output → OutputTokens), `turn.failed`
  (error.message), top-level `error` ("Reconnecting... X/Y" notices are
  treated as non-fatal progress), and `item.started/updated/completed`
  for item types: agent_message (last one = final text), reasoning,
  command_execution (tool_use on start, tool_result with
  aggregated_output/exit_code on completion), file_change
  (changes[].kind add/update/delete → created/modified/deleted; skipped
  from the aggregate when status=failed), mcp_tool_call, web_search,
  todo_list (rendered as a checklist plan event), error. Both `type` and
  the legacy `item_type` key are accepted.
- `--skip-git-repo-check` is on by default because the engine controls
  workspaces (disable per turn via Extra["skip_git_repo_check"]="false").
- Sandbox defaults to workspace-write; override via Extra["sandbox"]
  (read-only | workspace-write | danger-full-access; "" omits the flag).
- ✅ Verified live (codex-cli 0.142.5, 2026-07-10): a real `--json` turn
  streams exactly the events the parser handles (thread.started,
  item.completed, turn.started, "Reconnecting... X/Y" error notices,
  turn.failed; `type` key, not the legacy `item_type`). Resume flag
  placement `exec --json --sandbox … --skip-git-repo-check resume <id> -`
  parses fine (clap accepts exec-level flags before the subcommand; the
  `resume` subcommand itself re-defines --json/--model/
  --skip-git-repo-check but NOT --sandbox, so keep --sandbox before
  `resume`). Behavior change vs. the old caveat: resuming a missing
  session now FAILS loudly ("no rollout found for thread id …",
  code -32600) instead of silently starting a new session — the turn
  errors and the transcript keeps the old session, which is the better
  outcome for continuity.
- Effort (Step 13): `-c model_reasoning_effort="<level>"`. Key verified
  on codex-cli 0.142.5: with `--strict-config` an unknown key errors
  ("unknown configuration field") while this one is accepted; the VALUE
  is not validated at config-parse time (a bogus value still launched),
  so bad levels surface as model/API errors. Documented levels:
  minimal/low/medium/high (xhigh on newer models).
- MCP callback (Step 12): `-c mcp_servers.agentchat.url="<url>" -c
  mcp_servers.agentchat.bearer_token_env_var="AGENTCHAT_MCP_TOKEN"`
  with the token passed through the environment, never argv (keys per
  `codex mcp add --url/--bearer-token-env-var` on 0.142.5). Flags sit
  before a possible `resume` subcommand; `resume` re-defines `-c`, so
  both placements parse. Config-level, so MCP tools need no
  per-tool approval in exec mode.

## aider — Step 5 ✅ implemented, ✅ flags verified against aider 0.86.2 (2026-07-10)
    aider --message "<prompt>" --yes-always --no-stream --no-pretty \
        [--model <id>] [--restore-chat-history]
- Output is line-oriented text. Parsing is heuristic: prose (including
  SEARCH/REPLACE edit blocks) → grouped text events; "Applied edit to
  <path>" → file_change; "Commit <hash> <msg>" → git-commit tool_result;
  the "Tokens: Xk sent, Yk received. Cost: $Z message" line → usage.
  Banner/housekeeping lines (Aider v…, Main model:, Repo-map:, Added …,
  etc. — see noisePrefixes in output.go) are suppressed.
- Authoritative file changes come from git, not the text: HEAD is read
  before and after the run and `git diff --name-status -M before after`
  wins when aider committed (its default). Outside a repo, or when
  nothing was committed, the Applied-edit lines are the fallback.
- No session IDs: continuity comes from aider's own history files in the
  workspace (.aider.chat.history.md). SessionID is ignored;
  Extra["restore_chat_history"]="true" adds --restore-chat-history.
- API keys/base URLs (incl. OpenAI-compatible servers like LocalAI) pass
  through the environment (OPENAI_API_KEY, OPENAI_API_BASE,
  ANTHROPIC_API_KEY, ...) via TurnRequest.Env — Step 11 wires config.
- Effort (Step 13): `--reasoning-effort <level>` (aider 0.86.2 --help:
  "Set the reasoning_effort API parameter"; free-form value — litellm
  forwards it only for models that support it, so it is best-effort).
- ✅ Verified (aider 0.86.2, 2026-07-10): all flags (--message,
  --yes-always, --no-stream, --no-pretty, --model,
  --restore-chat-history, and --reasoning-effort for Step 13) accepted
  by a live run; the run reached aider's model/key setup, so parsing is
  confirmed. Noise-prefix list extended from real output: "Warning:
  Input is not a terminal" (emitted on every piped run) and the
  first-run privacy/analytics banner. A full end-to-end turn still
  needs a configured API key/model — the line heuristics remain as
  fixtures describe.

## swival — Step 6 ✅ implemented, ✅ verified live against swival 1.0.25 (2026-07-10)
    swival [--quiet] [--profile P] [--provider P] [--base-url U] \
        [--model M] [--reasoning-effort E] [--max-turns N] \
        --report <tmpfile>        # task piped to stdin
- Output contract (documented): stdout is exclusively the final answer;
  diagnostics (turn headers, tool traces, timing) go to stderr; --report
  writes a structured JSON run report (result.answer, stats, timeline of
  llm_call/tool_call/compaction events). The task is piped to stdin —
  documented one-shot behavior when no positional task is given.
- Adapter mapping: stderr lines → thinking events (live progress; disable
  with Extra["diagnostics"]="false", which also adds --quiet); stdout →
  final text event; report → usage (stats token counters tried by several
  names, falling back to summing timeline llm_call prompt_tokens_est +
  cached_tokens — exact stats keys unpinned, so parsing is defensive) and
  fallback answer.
- File changes: swival does not auto-commit, so changes are detected by
  diffing `git status --porcelain` before/after (best-effort; Step 7
  workspace snapshots are the authoritative record). Pre-existing dirty
  paths are excluded.
- Provider selection via Extra: provider (lmstudio | llamacpp |
  huggingface | openrouter | generic | google | chatgpt | bedrock |
  command), base_url (e.g. a LocalAI endpoint with provider=generic),
  profile, reasoning_effort, max_turns. API keys go through the
  environment (HF_TOKEN, OPENROUTER_API_KEY, OPENAI_API_KEY, ...), never
  the command line.
- Effort (Step 13): first-class `req.Effort` → `--reasoning-effort
  <level>` (swival 1.0.25 --help levels: none, minimal, low, medium,
  high, xhigh, default). The legacy Extra["reasoning_effort"] still
  works and overrides the field for back-compat.
- No session resume: continuity comes from .swival/ state (memory,
  history) inside the workspace, which persists across turns under this
  app's workspace model. SessionID is ignored.
- Exit codes (documented): 0 success; 1 runtime/config failure; 2 turn
  limit reached (adapter returns the partial answer plus an explicit
  error); 130/143 signals.
- ✅ Verified live (swival 1.0.25, 2026-07-10): all flags accepted;
  task piped via stdin works; --report writes the JSON report even when
  the run fails. Report schema observed: version/mode/task/model/
  provider, `result.{outcome,answer,exit_code,error_message}`,
  `stats` (turns, tool_calls_*, llm_calls, timing — **no token
  counters in this version**), `timeline`. So the timeline
  llm_call fallback in applyReport is the operative token path; the
  defensive stats-key lookup stays for future versions. Provider list
  grew to include `geap` and `vertexai` (alias). A full end-to-end
  turn needs a running model server (default: LM Studio at
  127.0.0.1:1234).

## echo (fake) — built in
    agentchat-cli -client echo -dir <workspace> "<prompt>"
- No external binary; writes ECHO.md into the workspace.
