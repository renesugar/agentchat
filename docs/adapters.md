# Coding-client CLI invocations

Record here the *verified* non-interactive invocation for each client, with
the client version you verified it against. Planned invocations (unverified)
are marked ⚠ — verify with `<client> --help` before implementing the adapter
and update this file in the same commit.

## claude (Claude Code) — Step 3 ⚠ planned
    claude -p --output-format stream-json --verbose \
        [--model <id>] [--resume <session_id>] "<prompt>"
- stream-json emits JSON lines: system/init (has session_id), assistant
  messages (text + tool_use blocks), user (tool_result), and a final
  `result` payload with cost/usage.
- Permissions: non-interactive runs need a permission strategy
  (e.g. --permission-mode / allowed tools flags); decide and document.

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
