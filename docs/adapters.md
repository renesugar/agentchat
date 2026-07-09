# Coding-client CLI invocations

Record here the *verified* non-interactive invocation for each client, with
the client version you verified it against. Planned invocations (unverified)
are marked ⚠ — verify with `<client> --help` before implementing the adapter
and update this file in the same commit.

## claude (Claude Code) — Step 3 ✅ implemented (⚠ flags unverified against a live install)
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
- TODO when a live `claude` is available: run once, confirm flags with
  `claude --help` and record the version here; capture a fresh fixture if
  the stream format drifted (fixtures: `internal/adapters/claudecode/testdata/`).

## codex (Codex CLI) — Step 4 ✅ implemented (verified against docs/web, not a live install)
    codex exec --json --sandbox workspace-write --skip-git-repo-check \
        [--model <id>] [resume <thread_id>] -
- Prompt is piped to stdin (the trailing `-`), avoiding quoting issues.
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
- ⚠ resume caveat: some codex versions reject --json/--model/--sandbox
  when resuming; flag placement here (before the `resume` subcommand)
  matches current documented behavior. On a live install run
  `codex exec --help` and one resume turn to confirm; record the
  `codex --version` here. Also note: resuming an `--ephemeral` or
  missing session silently starts a NEW session (thread_id changes) —
  the adapter reports the new session_id, so the transcript stays
  correct, but continuity is lost.

## aider — Step 5 ✅ implemented (⚠ flags unverified against a live install)
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
- TODO when a live `aider` is available: verify flags with `aider
  --help`, record the version, and sanity-check the noise-prefix list
  against real output (it varies slightly across versions).

## swival — Step 6 ⚠ planned
- Check https://swival.dev/ docs for the non-interactive/print mode and
  model flags; record findings here with the version.

## echo (fake) — built in
    agentchat-cli -client echo -dir <workspace> "<prompt>"
- No external binary; writes ECHO.md into the workspace.
