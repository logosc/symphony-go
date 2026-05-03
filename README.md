# symphony-go

A small Go orchestrator that drives Codex or Claude Code on GitHub issues
labeled `symphony:ready`, posts a plan, decides approval (rules + reviewer
agent + post-impl diff verification, or human, or none), implements,
validates, and opens a draft PR.

See [`SPEC.md`](./SPEC.md) for the full design and
[`docs/M6-real-runner-smoke.md`](docs/M6-real-runner-smoke.md) for the
end-to-end runbook.

## Status

Functional MVP. M0–M4 of `SPEC.md` §15 are implemented and integration-
tested via fakes. GitHub-App-installation authentication is supported
alongside personal access tokens. M7 (codex `app-server` protocol,
multi-turn continuation) is in progress.

## Differences from OpenAI Symphony

symphony-go is a security-first, GitHub-first profile of OpenAI's
[Symphony](https://github.com/openai/symphony). It preserves Symphony's
core architecture and deliberately diverges in three places: tracker,
trust posture, and approval model.

### Preserved

- Per-issue workspace isolation (one git worktree per issue)
- In-repo prompt template owned by the team (`WORKFLOW.md`)
- Workspace lifecycle hooks (`after_create`, `before_run`, `after_run`)
- Bounded poll loop with reconciliation before dispatch, restart recovery
  without a database
- JSONL agent protocol contract (Claude Code stream-json; Codex `exec`
  and — post-M7 — `app-server`)
- Sanitized workspace key, hard cwd validation, sanitized agent env

### Diverged — security-first

| Symphony | symphony-go | Why |
|---|---|---|
| `WORKFLOW.md` is self-contained (config + prompt) and lives in the repo | Config split to `~/.symphony-go/config.yml` outside any repo; `WORKFLOW.md` carries only the prompt | Prevents the agent — which has Edit/Write inside its workspace — from editing its own permission set, validation commands, or approval rules during a run. `doctor` enforces "config NOT under any repo path". |
| Agent performs ticket writes through tools | Orchestrator owns all GitHub writes (label transitions, PR creation, comments) | Smaller blast radius under prompt injection from a hostile issue. The agent subprocess never receives a GitHub token. |
| Subscription auth flows through the user's real `$HOME` | Agent `$HOME` is an isolated per-worktree directory; `~/.claude/` and `~/.codex/` are symlinked in on demand | Agent can't read unrelated dotfiles (`.ssh`, `.aws`, etc.) while subscription auth still works. |

### Diverged — product fit

| Symphony | symphony-go |
|---|---|
| Linear tracker | GitHub Issues |
| Run ends at a configured handoff state | Three approval modes via `approval.mode` — also overridable per-issue-label via `approval.mode_by_label` (M7.5; see proposal 0001): `gated` (mandatory `/symphony approve` comment), `auto` (rules engine + reviewer agent + post-implementation diff verification), `handoff` (Symphony's no-gate behavior) |
| `linear_graphql` client-side tool | None — a `github_graphql` analogue could be added |
| PAT-only auth | PAT or GitHub App installation. Pick `auth: "pat"` (default) or `auth: "app"` in `~/.symphony-go/config.yml`; see [`docs/github-app-setup.md`](docs/github-app-setup.md) for the App walkthrough. |

The `auto` mode in particular introduces three independent gates between
plan and PR:

1. **Rules engine** — pure code, immune to prompt injection. Caps the
   agent's stated `files_touched` scope by issue label.
2. **Reviewer agent** — a different LLM (e.g. Codex when the main runner
   is Claude), read-only sandbox, fixed system prompt outside
   `WORKFLOW.md`'s control.
3. **Diff verification** — pure code, runs after implementation. If the
   actual diff drifted beyond the claimed scope, the run is blocked
   before any commit.

Lying in the plan to win auto-approval doesn't help — diff verification
catches the drift before any branch is pushed. See `SPEC.md` §10.

### Acknowledgements

Architecture, terminology, and many design details (workspace isolation,
hooks, in-repo prompt, reconcile loop, JSONL agent protocol) are taken
directly from OpenAI's open-source Symphony. The deviations above are
implementation choices for a different trust model and tracker, not
improvements — Symphony explicitly leaves trust posture to
implementations.

## Quick start

```sh
# 1. Install
go install ./cmd/symphony-go

# 2. Create config (lives outside the repo on purpose)
mkdir -p ~/.symphony-go
cp testdata/config.example.yml ~/.symphony-go/config.yml
# edit repo.full_name, repo.local_path, etc.

# 3. Add WORKFLOW.md prompt template inside your repo
cp testdata/WORKFLOW.example.md /path/to/your-repo/WORKFLOW.md

# 4. Verify the setup
GITHUB_TOKEN=ghp_... symphony-go doctor --config ~/.symphony-go/config.yml

# 5. Run (handoff mode in config recommended for the first smoke run)
GITHUB_TOKEN=ghp_... symphony-go run --config ~/.symphony-go/config.yml
```

For the full end-to-end smoke walkthrough including label setup, gated
and auto modes, and troubleshooting, see
[`docs/M6-real-runner-smoke.md`](docs/M6-real-runner-smoke.md).

## Module path

The Go module path is `github.com/logosc/symphony-go`. To fork under a
different organization, run `go mod edit -module ...` and update imports.

## License

TBD.
