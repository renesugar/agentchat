# Instructions for implementing agents (Claude Code / Codex / others)

You are implementing AgentChat according to `PLAN.md`. Rules:

1. **One step at a time.** Pick the first `[ ]` step in `PLAN.md` unless the
   user says otherwise. Do not start work you cannot leave compiling —
   sessions may be cut off by usage limits.
2. **`make check` must pass before you stop.** That is `gofmt` clean,
   `go vet ./...`, `go test ./...`.
3. **Update `PLAN.md`** (`[ ]` → `[x]`) and commit with message
   `step N: <summary>` when done.
4. **Keep `internal/` free of GUI/framework imports.** Wails code lives only
   in `app/`, which is a nested Go module: root `make check` does not build
   it. Work on the app with `make app-tidy` / `make app-dev` on a machine
   that can fetch modules; keep `app/frontend/dist` free of build tooling
   (it is embedded as-is).
5. **Never call real coding-client binaries or networks in unit tests.**
   Adapters are tested against recorded fixtures in `testdata/`. Verify real
   CLI flags manually (`<client> --help`) and record what you verified, with
   the client version, in `docs/adapters.md`.
6. **Stdlib-only until a step explicitly introduces a dependency.** When one
   does, pin it in `go.mod` and note why in `ARCHITECTURE.md`.
7. **Don't rename the module or packages** without updating all references;
   module path is `github.com/renesugar/agentchat` (rename once a real home
   exists — a single `go.mod` edit + goimports).
8. If reality disagrees with the plan (a CLI flag changed, an approach is
   wrong), fix the plan in the same commit and say so in the commit message.
