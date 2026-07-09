# AgentChat

A desktop chat GUI (Wails, Go) that orchestrates **terminal coding agents**
— Claude Code, Codex CLI, Aider, Swival — instead of talking to an LLM
server directly. Pick a client and model per turn; each turn runs the client
non-interactively in a shared workspace (git repo / worktree / scratch dir),
streams its output into the chat, snapshots the workspace, and files
artifacts. Conversations are grouped by project (git repo); everything can
be exported as a Markdown transcript + ZIP.

Status: scaffold. See `PLAN.md` (roadmap + step status), `ARCHITECTURE.md`
(design), `AGENTS.md` (rules for implementing agents).

## Quick start (headless)

    make check      # fmt + vet + test
    make run-echo   # run one turn with the fake adapter

The Wails GUI arrives in Step 10; until then `cmd/agentchat-cli` exercises
the whole engine.
