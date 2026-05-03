# M6 — Real-Runner Smoke Test

Run symphony-go end-to-end against a real GitHub repo using either Claude
Code or OpenAI Codex CLI in **subscription mode** (no API keys).

The orchestrator runs the agent subprocess with an isolated `HOME`, but
symlinks the user's `~/.claude/` and `~/.codex/` into that home so the
agent's subscription auth still works. You authenticate once in your
normal shell; symphony-go inherits that.

## Prerequisites

- A GitHub repository you control. **Use a sandbox repo** — symphony-go
  will create branches and open draft PRs.
- A GitHub PAT with `repo` scope, exported as `GITHUB_TOKEN`.
- One of:
  - **Claude Code** installed (`claude` in `PATH`) and authenticated
    (`claude /login` once; state lives in `~/.claude/`).
  - **OpenAI Codex CLI** (`codex` in `PATH`) and authenticated
    (`codex login` once; state lives in `~/.codex/`).
  - Or both, if you want to run with `agent.provider: claude` and a
    `auto.reviewer.provider: codex` (Symphony's recommended split).
- `git` ≥ 2.30 and Go 1.26+ in `PATH`.

## Setup

### 1. Build the binary

```sh
cd ~/Documents/Github/symphony-go
go install ./cmd/symphony-go
which symphony-go   # ~/go/bin/symphony-go on most setups
```

### 2. Create the eight labels in your sandbox repo

```sh
REPO=<owner>/<repo>
for L in ready planning awaiting-approval implementing pr-ready failed blocked stop; do
  gh -R "$REPO" label create "symphony:$L" --force
done
```

### 3. Drop your config outside the repo

```sh
mkdir -p ~/.symphony-go
cp ~/Documents/Github/symphony-go/testdata/config.example.yml \
   ~/.symphony-go/config.yml
$EDITOR ~/.symphony-go/config.yml
```

Set at minimum:

| field | value |
|---|---|
| `repo.full_name` | `<owner>/<repo>` |
| `repo.local_path` | absolute path to the cloned sandbox repo |
| `agent.provider` | `claude` or `codex` |
| `approval.mode` | `handoff` for the first smoke run |

For a first smoke, **start in `handoff` mode** so you don't have to wait
for an approval comment.

### 4. Drop `WORKFLOW.md` into your sandbox repo

```sh
cp ~/Documents/Github/symphony-go/testdata/WORKFLOW.example.md \
   /path/to/sandbox/WORKFLOW.md
git -C /path/to/sandbox add WORKFLOW.md
git -C /path/to/sandbox commit -m "add WORKFLOW.md"
git -C /path/to/sandbox push origin main
```

### 5. Verify the setup

```sh
GITHUB_TOKEN=ghp_... symphony-go doctor --config ~/.symphony-go/config.yml
```

Must print `ok`. If anything fails, fix what it reports and re-run.

## Smoke run

### 6. Open a trivial issue

Create a tiny, low-blast-radius issue in your sandbox. Example:

- **Title:** `Add hello.txt`
- **Body:** `Create a file named hello.txt at the repo root with the
  contents "hello world\n".`
- **Label:** `symphony:ready`

### 7. Run once

```sh
GITHUB_TOKEN=ghp_... symphony-go run --once --config ~/.symphony-go/config.yml
```

Expected JSON-log sequence (one line per event):

```
claim                     ready → planning
worktree_created          .symphony-go/wt/issue-N-add-hello-txt/
hook_started/completed    after_create (if configured)
planning_started
planning_completed
plan_posted               comment id saved
scope_parsed
auto_approved | rule_matched   (handoff mode skips both)
implementation_started
implementation_completed
diff_verified             auto mode only
validation_command        for each cfg.validation.commands entry
validation_completed
committed                 commit sha
pushed                    branch on origin
pr_created                PR number
implementing → pr-ready
```

### 8. Verify

```sh
gh -R "$REPO" issue view <N>          # label is symphony:pr-ready, has a PR-link comment
gh -R "$REPO" pr list --draft         # draft PR with the change
gh -R "$REPO" pr view <PR>            # body lists approval_path=handoff
```

The PR is **draft** by design — symphony-go never marks it ready or
merges it. You review and merge by hand.

## Try the other modes

### Gated mode

In `~/.symphony-go/config.yml`, set `approval.mode: gated`. Re-open a
fresh `symphony:ready` issue, run `symphony-go run`, and watch the label
move to `awaiting-approval`. Comment `/symphony approve` on the issue
from your own account (must have write permission). Run `--once` again
or leave the long-running `run` going; the next tick picks up the
approval and proceeds to implementation.

### Auto mode (rules + reviewer + diff verification)

Set:

```yaml
approval:
  mode: auto
auto:
  rules:
    - issue_labels: ["docs"]
      max_plan_files_claimed: 3
      reviewer_required: false   # rules-only auto-approve
    - issue_labels: []           # catch-all
      max_plan_files_claimed: 10
      reviewer_required: true
  reviewer:
    provider: codex              # cross-provider second opinion
agent:
  provider: claude
```

For a `docs`-labeled issue with a small claimed scope, the run skips the
reviewer and goes straight to implementation. For anything else, the
codex reviewer reads the plan and decides; reject falls back to gated
human approval (or `block`, if you set
`auto.fallback_on_reject: block`).

The post-implementation **diff verification** still runs regardless: if
the agent touched more files than `## Scope` claimed (beyond
`max_diff_drift_files`), the run is blocked before any commit.

## Cleanup

```sh
gh -R "$REPO" pr close <PR> --delete-branch
gh -R "$REPO" issue close <N>
rm -rf /path/to/sandbox/.symphony-go
```

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| `claude: command not found` (in agent stderr) | Binary not in `PATH` for the symphony-go process. Verify `which claude` from the same shell. |
| Agent says "not authenticated" / "log in first" | Run `claude /login` (or `codex login`) **outside** symphony-go once. The orchestrator symlinks your `~/.claude/` (and `~/.codex/`) into the agent's isolated HOME. |
| `doctor: config path is under repo.local_path` | Move `config.yml` outside the repo. Default is `~/.symphony-go/config.yml`. |
| `branch already exists` | Old worktree wasn't cleaned up. `git -C <repo> worktree prune` and delete `symphony/issue-N-*` locally and on origin. |
| PR posted but nothing happens for 30s+ (gated) | Comment `/symphony approve` from a user with write permission. The next poll tick (default 30s) picks it up. |
| Validation runs `go test ./...` and fails on the agent's change | Either fix the agent, narrow `validation.commands`, or relax the test. Validation failure marks the issue `symphony:failed`; no PR is created. |
| `diff drift exceeded` | Agent claimed fewer files than it touched. Either bump `auto.max_diff_drift_files`, switch the issue to `handoff` mode, or have the agent claim its real scope. |

## What this smoke run validates (DoD §16 items 4 and 5)

- `claude` runner completes a planning run (and an implementation run)
  on a real issue with subscription auth intact.
- `codex` runner (exec mode) completes a planning run on a real issue.
- Workspace isolation works without breaking subscription auth.
- Draft PR appears, orchestrator never merges.
- Labels transition correctly across `ready → planning → implementing →
  pr-ready`.
