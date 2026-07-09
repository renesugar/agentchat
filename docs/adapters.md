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

## codex (Codex CLI) — Step 4 ⚠ planned
    codex exec [--json] [--model <id>] "<prompt>"
- Verify flag names for JSON output, model selection, sandbox/approval
  settings, and session resume; pin here with `codex --version`.

## aider — Step 5 ⚠ planned
    aider --message "<prompt>" --yes-always --no-stream [--model <id>]
- Output is line-oriented text, not JSON. Aider auto-commits; derive
  file_change events from `git diff --name-status <before>..<after>`.

## swival — Step 6 ⚠ planned
- Check https://swival.dev/ docs for the non-interactive/print mode and
  model flags; record findings here with the version.

## echo (fake) — built in
    agentchat-cli -client echo -dir <workspace> "<prompt>"
- No external binary; writes ECHO.md into the workspace.
