# symphony-go — Minimal Go Symphony for GitHub Issues

A small Go binary. Reads GitHub issues labeled `symphony:ready`, runs Codex
or Claude Code in an isolated git worktree, posts a plan, decides approval
via rules + a reviewer agent (or human), implements the approved plan,
runs validation, and opens a draft PR.

Local trusted-user tool. Not a multi-tenant sandbox. Do not run on untrusted
repositories. Do not expose production secrets.

Security-first profile of the OpenAI Symphony architecture. Preserved:
in-repo `WORKFLOW.md` prompt, per-issue workspace isolation, workspace hooks,
JSONL agent protocol, reconciliation, restart-without-DB. Diverged: config
split out of the repo for agent containment; orchestrator owns PR push;
approval has three modes (gated, auto, handoff) with a prompt-injection-immune
diff verification gate.

---

## 1. Goal

```
symphony-go run    --config ~/.symphony-go/config.yml
symphony-go run    --once --config ~/.symphony-go/config.yml
symphony-go doctor --config ~/.symphony-go/config.yml
```

One process per machine (enforced by flock). One issue at a time by default.
Resume after restart via local JSON state. GitHub labels are the user-visible
state machine.

---

## 2. Two files, two locations — intentionally

```
~/.symphony-go/config.yml   # config, never visible to the agent
<repo>/WORKFLOW.md           # prompt template, may be edited by humans
```

Splitting them prevents the agent from editing its own permission set,
validation commands, or approval rules during a run.

### config.yml

```yaml
repo:
  full_name: "OWNER/REPO"
  base_branch: "main"
  local_path: "/abs/path/to/repo"
  workflow_file: "WORKFLOW.md"

github:
  token_env: "GITHUB_TOKEN"
  poll_interval_seconds: 30

labels:
  ready: "symphony:ready"
  planning: "symphony:planning"
  awaiting_approval: "symphony:awaiting-approval"
  implementing: "symphony:implementing"
  pr_ready: "symphony:pr-ready"
  failed: "symphony:failed"
  blocked: "symphony:blocked"
  stop: "symphony:stop"

approval:
  mode: "auto"                       # gated | auto | handoff
  command: "/symphony approve"       # used in gated, and as auto's fallback
  require_write_permission: true

# Only consulted when approval.mode == "auto".
auto:
  rules:
    # First match wins. Rule matches when issue has at least one of
    # `issue_labels` AND plan's `files_touched` count <= max_plan_files_claimed.
    # `issue_labels: []` is a catch-all (matches any issue).
    - issue_labels: ["docs", "test"]
      max_plan_files_claimed: 5
      reviewer_required: false       # rules-only, no LLM judge
    - issue_labels: ["safe-change"]
      max_plan_files_claimed: 10
      reviewer_required: true
    - issue_labels: []               # catch-all default
      max_plan_files_claimed: 20
      reviewer_required: true
  reviewer:
    provider: "codex"                # ideally != agent.provider
    model: ""
    timeout_seconds: 600
  fallback_on_reject: "gated"        # gated | block
  fallback_on_no_rule_match: "gated" # gated | block
  verify_diff_matches_plan: true
  max_diff_drift_files: 2

agent:
  provider: "claude"                 # claude | codex
  model: "sonnet"
  timeout_seconds: 3600

claude:
  max_turns: 20
  planning_tools:        ["Read", "Grep", "Glob", "LS"]
  implementation_tools:
    - "Read"
    - "Edit"
    - "Write"
    - "Bash(git status:*)"
    - "Bash(git diff:*)"
    - "Bash(go test:*)"
  review_tools:          ["Read", "Grep", "Glob", "LS"]
  disallowed_tools:
    - "Bash(sudo:*)"
    - "Bash(curl:*)"
    - "Bash(wget:*)"

codex:
  mode: "exec"                       # exec | app-server
  planning_args:        ["--sandbox", "read-only"]
  implementation_args:  ["--sandbox", "workspace-write"]
  review_args:          ["--sandbox", "read-only"]

env:
  allowlist:      ["OPENAI_API_KEY", "ANTHROPIC_API_KEY"]
  block_patterns: [".*TOKEN.*", ".*SECRET.*", ".*PASSWORD.*",
                   "AWS_.*", "GCLOUD_.*", "KUBECONFIG", "SSH_AUTH_SOCK"]

hooks:
  after_create: ""    # shell, runs once per worktree creation (e.g., npm ci)
  before_run:   ""    # shell, runs before each agent invocation
  after_run:    ""    # shell, runs after each agent invocation regardless
  timeout_seconds: 60

validation:
  commands: ["go test ./..."]
  command_timeout_seconds: 900

audit:
  redact_patterns: ["sk-[A-Za-z0-9_-]+", "ghp_[A-Za-z0-9_]+", "ghs_[A-Za-z0-9_]+"]
```

### WORKFLOW.md

Plain markdown. Bindings expanded by literal substitution:

```
{{ issue.title }}
{{ issue.description }}
{{ issue.url }}
{{ issue.number }}
{{ issue.labels }}
```

Mode suffixes (planning/implementation) are appended by the orchestrator.

### Plan output contract

The agent's planning-phase output **must** end with a structured `## Scope`
block. Without it, `auto` mode falls through to `gated`.

```
## Scope
files_touched:
  - path/to/a.go
  - path/to/b.go
estimated_lines_added: 50
estimated_lines_removed: 10
risk_summary: one-line risk note
```

Parser tolerates surrounding whitespace and YAML-ish indentation. Unknown
fields are ignored. Missing required fields → fallback to gated.

The rendered planning prompt suffix tells the agent this contract is
mandatory and that the orchestrator will reject the plan otherwise.

### Per-axis configuration (M7.5+)

Five config knobs accept an additive `*_by_label` map alongside their
scalar form so one orchestrator can drive multiple work axes (code,
research, marketing, catalog, …) on one repo. Resolution: at job claim
time, the orchestrator walks the canonical map (`repo.workflow_files`)
in declared YAML order and picks the first key that appears as a label
on the issue (`default` excluded, used only as last fallback). The
chosen key is frozen on the `Job` as `AxisKey` and read back by every
per-axis lookup; mid-run label edits cannot divert behavior. See
`docs/proposals/0001-per-axis-config.md` and `docs/per-axis-config.md`.

The five affected knobs:

- `repo.workflow_files`             — per-axis `WORKFLOW.<axis>.md`
- `validation.commands_by_label`    — per-axis validation pipelines
- `claude.{planning,implementation,review,disallowed}_tools_by_label`
  and `codex.{planning,implementation,review}_args_by_label`
  — per-axis tool/sandbox surface
- `approval.mode_by_label`          — per-axis approval mode

Example (excerpt):

```yaml
repo:
  workflow_files:
    "type:research": "operations/workflows/WORKFLOW.research.md"
    default:         "operations/workflows/WORKFLOW.code.md"
validation:
  commands_by_label:
    "type:research": ["test -f docs/research/*.md"]
    default:         ["go test ./..."]
claude:
  implementation_tools_by_label:
    "type:research": [Read, Write, WebFetch, WebSearch]
    default:         [Read, Edit, Write, "Bash(git:*)"]
approval:
  mode_by_label:
    "type:marketing-ads": "gated"
    default:               "auto"
```

Both scalar and `*_by_label` for the same knob is an error: config
parsing rejects this at startup, and `doctor` reports it. Each
`*_by_label` map MUST contain a `default` key.

### Config integrity guard

The orchestrator MUST:

1. Resolve `config.yml` to its absolute path at startup, `stat` it, compute
   SHA-256, and store both.
2. On every poll tick, re-stat and re-hash. If the hash changed and any job
   is in `planning`, `awaiting_approval`, or `implementing`: refuse to apply
   the new config until those jobs reach a terminal state. Log a warning on
   every tick until then.
3. Refuse to start the orchestrator if the resolved `config.yml` path is
   under any `repo.local_path`. Catches the case where someone moves the
   config back into a repo. Hard fail at startup; also checked by `doctor`.
4. Before commit, scan the staged diff for any change to the path resolved
   from `repo.workflow_file`. If present, do not block, but post a comment:
   `[symphony-go] agent modified WORKFLOW.md; review carefully before
   merge`. The PR body must contain the same notice under `## Warnings`.
5. Pass neither the config path nor any config field as an env var or CLI
   arg to the agent subprocess. Only orchestrator-process code reads
   `config.yml`.

---

## 3. Flow

```
issue with label `symphony:ready`
  ├─ claim:                           replace label → `planning`
  ├─ create worktree at <repo>/.symphony-go/wt/issue-{n}-{slug}/
  ├─ run hooks.after_create (once per new worktree)
  ├─ hooks.before_run + run agent in planning + hooks.after_run
  ├─ post plan as issue comment, save plan_comment_id, parse `## Scope`
  ├─ approval (depends on approval.mode):
  │     gated   → label `awaiting-approval`, poll for `/symphony approve`
  │     auto    → eval rules; if reviewer_required, run reviewer phase
  │                approve → advance · reject → fallback_on_reject
  │     handoff → advance immediately (no gate)
  ├─ replace label → `implementing`
  ├─ hooks.before_run + run agent in implementation + hooks.after_run
  ├─ if auto.verify_diff_matches_plan:
  │     compare actual touched files vs plan.scope.files_touched
  │     if drift > max_diff_drift_files → set blocked, stop
  ├─ run validation commands; any failure → `failed` and stop
  ├─ commit, push branch, create draft PR
  └─ replace label → `pr-ready`
```

The orchestrator creates the PR. The agent never pushes, merges, or modifies
PR metadata.

---

## 4. Repo layout

```
cmd/symphony-go/main.go
internal/
  types/         # shared types: Issue, Job, Config, PlanScope, RunRequest...
  config/        # parse + validate config.yml; load WORKFLOW.md
  github/        # go-github client wrapper
  state/         # JSON job state + flock
  workspace/     # worktree create / dirty check / slug
  exec/          # command policy + bounded shell exec for hooks + validation
  runner/        # AgentRunner interface + fake.go + claude.go + codex.go
  approval/      # scope parser + rules engine + reviewer + diff verifier
  orchestrator/  # main loop, reconcile, dispatch, approval routing
testdata/
  config.example.yml
  WORKFLOW.example.md
go.mod
README.md
```

Deps: `github.com/google/go-github/v*`, `golang.org/x/oauth2`,
`gopkg.in/yaml.v3`. CLI uses stdlib `flag`. Logging uses stdlib `log/slog`.
No Cobra, no custom audit package.

---

## 5. Label state machine

```
ready → planning → awaiting-approval → implementing → pr-ready
                                                       (terminal)

any active state → blocked | failed (terminal)
```

In `auto` mode, `awaiting-approval` is only used as the fallback target —
auto-approved issues skip directly from `planning` to `implementing`. Each
transition is one atomic GitHub call: `PATCH /issues/{n}` with the full
`labels` array (`ReplaceStateLabel(remove, add)` helper). Never two separate
Add/Remove calls — a crash between them leaves the issue without a state
label.

`stop` is sticky, set by humans. Detected each tick: cancel agent, set
`blocked`, post a comment, remove `stop`.

`blocked` = a human must look. `failed` = orchestrator stopped because of a
runtime/validation/diff-drift error.

---

## 6. Local state

`.symphony-go/state/jobs/{issue_number}.json`:

```json
{
  "issue_number": 123,
  "repo": "OWNER/REPO",
  "status": "awaiting_approval",
  "workspace_root": ".symphony-go/wt/issue-123-fix-foo",
  "repo_path": ".symphony-go/wt/issue-123-fix-foo/repo",
  "branch": "symphony/issue-123-fix-foo",
  "plan_comment_id": 12345,
  "plan_text": "...",
  "plan_scope": {
    "files_touched": ["..."],
    "estimated_lines_added": 50,
    "estimated_lines_removed": 10,
    "risk_summary": "..."
  },
  "approval_path": "rules|reviewer|human|handoff",
  "approval_comment_id": null,
  "reviewer_decision": null,
  "pr_number": null,
  "attempt": 1,
  "updated_at": "2026-05-03T00:00:00Z"
}
```

`.symphony-go/lock` is `flock`'d at startup. A second `symphony-go run`
on the same machine exits with a clear error.

Atomic write: write to `{path}.tmp`, `fsync`, rename.

---

## 7. Reconcile on startup

State reconciliation runs at startup, after acquiring the lock and before
any dispatch. Inputs:

- every open issue in the repo carrying at least one `symphony:*` label
  (one paginated issues-search call)
- every file under `.symphony-go/state/jobs/`

For each (issue, local-state) pair, apply exactly one row. Match is on the
tuple `(local.status, github.symphony_label, issue.state)`. The table is
exhaustive — no row matching is a bug, fail loud.

| #  | local                | github label             | issue  | action |
|----|----------------------|--------------------------|--------|--------|
| 1  | missing              | `ready`                  | open   | normal: enter dispatch queue |
| 2  | missing              | `planning`               | open   | replace label with `blocked`, comment "orphan planning label, no local state" |
| 3  | missing              | `awaiting-approval`      | open   | replace with `blocked`, comment "orphan awaiting-approval label, no local state" |
| 4  | missing              | `implementing`           | open   | replace with `blocked`, comment "orphan implementing label; no local state, workspace not preserved" |
| 5  | missing              | `pr-ready`               | open   | leave alone (terminal, owned by humans) |
| 6  | missing              | `failed` or `blocked`    | open   | leave alone |
| 7  | `planning`           | `planning`               | open   | re-run planning from scratch. If `plan_comment_id` is set, edit that comment in place rather than posting a new one |
| 8  | `planning`           | anything else            | open   | mark blocked locally, replace github label with `blocked`, comment "label drift: local=planning, github=`<label>`" |
| 9  | `awaiting_approval`  | `awaiting-approval`      | open   | resume: poll comments for approval (gated) or re-eval auto rules |
| 10 | `awaiting_approval`  | `implementing`           | open   | crash mid-transition. If `approval_comment_id` set OR `reviewer_decision == approve`: resume implementation. Else: blocked, comment |
| 11 | `awaiting_approval`  | anything else            | open   | label drift, blocked |
| 12 | `implementing`       | `implementing`           | open   | DO NOT auto-resume. Mark blocked. Replace label with `blocked`. Comment "interrupted mid-implementation; workspace preserved at `<path>`; remove this label and add `symphony:ready` to retry from scratch" |
| 13 | `implementing`       | `pr-ready`               | open   | crash after PR creation but before state save. `GET /repos/{owner}/{repo}/pulls?head={owner}:{branch}&state=open`. If exactly one match, save `pr_number`, mark complete. Zero/multiple matches → blocked. |
| 14 | `implementing`       | `failed`                 | open   | crash during failure handling. Leave `failed`, do not retry |
| 15 | `pr_ready`           | `pr-ready`               | open   | terminal, leave |
| 16 | `pr_ready`           | anything else            | open   | someone manually relabeled. Leave local as is, do not touch github |
| 17 | any                  | (no `symphony:*` label)  | open   | mark local `blocked`, do not relabel github |
| 18 | any non-terminal     | any                      | closed | mark local complete, kill any running job, leave workspace and labels |
| 19 | terminal             | any                      | closed | mark local complete, leave |

Rules that apply to every row:

- Reconciliation never starts an agent.
- Reconciliation never deletes a workspace.
- Comments posted by reconcile are prefixed `[symphony-go reconcile]` and
  capped at 1000 chars.
- API errors are logged and counted; reconcile continues for remaining rows.
- After all rows: log `reconcile: N rows processed, K transitioned, M errors`.

---

## 8. Worktree

```
.symphony-go/wt/issue-{number}-{slug}/
├── repo/   # git worktree
└── home/   # HOME for the agent subprocess (also TMPDIR=home/tmp)
```

Slug: lowercase title; replace `[^a-z0-9._-]+` with `-`; trim leading/trailing
`-`; truncate to 60 runes; if empty, fallback to `issue-{number}`.

Branch: `symphony/issue-{number}-{slug}`.

Creation:

```
git -C <local_repo> fetch origin <base_branch>
if branch <symphony/...> already exists locally or on origin: mark blocked
git worktree add -b <branch> <wt>/repo origin/<base_branch>
```

Use `-b`, never `-B`. Before implementation: `git -C repo status
--porcelain` must be empty. If not, mark blocked.

### Hooks

Each `hooks.*` script runs as `bash -lc <script>` with `cwd = <wt>/repo`,
the same sanitized env as the agent, and `HOME=<wt>/home`. `timeout_seconds`
applies per invocation.

- `after_create`: runs exactly once per worktree creation. Failure → blocked.
- `before_run`: runs before every agent invocation (planning + implementation).
  Failure aborts that phase only.
- `after_run`: runs after every agent invocation regardless of outcome.
  Failure is logged but does not change job status.

Hook stdout/stderr are captured into the audit log, redacted. Hooks do **not**
run for the reviewer phase — reviewer runs in the orchestrator's local repo,
not a worktree.

---

## 9. Agent runner

```go
type Phase string
const (
    PhasePlanning      Phase = "planning"
    PhaseReview        Phase = "review"
    PhaseImplementation Phase = "implementation"
)

type RunRequest struct {
    Issue    Issue
    RepoPath string
    HomePath string
    Prompt   string
    Phase    Phase
    Timeout  time.Duration
}

type RunResult struct {
    Success     bool
    Text        string
    Stderr      string
    Events      []byte
    StartedAt   time.Time
    CompletedAt time.Time
}

type AgentRunner interface {
    Run(ctx context.Context, req RunRequest) (RunResult, error)
}
```

Both implementations (Claude, Codex) follow:

- Build env from empty: allowlist entries, drop block_patterns matches.
- `HOME = req.HomePath`, `TMPDIR = req.HomePath/tmp`, `CI=true`.
- Never inherit `GITHUB_TOKEN`, `GH_TOKEN`, `SSH_AUTH_SOCK`.
- Prompt on stdin, never as a shell argument.
- `cmd.Cancel = SIGTERM`, `cmd.WaitDelay = 10s`.

### Claude

```
claude -p \
  --output-format stream-json --verbose \
  --model <model> --max-turns <n> \
  --permission-mode <plan|acceptEdits> \
  --allowedTools <list> --disallowedTools <list>
```

`--permission-mode plan` is verified valid in headless mode and restricts
Edit/Write/Bash at the tool level. Planning and review use `plan`.
Implementation uses `acceptEdits`. Never `bypassPermissions`.

### Codex

Two verified backends, choose via `codex.mode`.

**`exec` (default).** `codex exec` is headless one-shot. Prompt on stdin,
output on stdout. With `--json`, emits JSONL events. Sandbox per phase via
`--sandbox <read-only|workspace-write|danger-full-access>`.

**`app-server` (opt-in, Symphony's reference runner).** JSON-RPC 2.0 over
stdio with newline-delimited framing. Methods: `initialize` →
`initialized` → `thread/start` (with `cwd`, `sandbox`, `approvalPolicy`)
→ `turn/start` (with `threadId`, `input`, `sandboxPolicy`). Terminal:
`turn/completed` with `status: completed | interrupted | failed`. Use
this when you need streaming events, stall detection, or multi-turn
continuation. Pin a known-working version — upstream marks experimental.

### Reviewer (auto mode only)

The reviewer is an `AgentRunner` instantiated separately with:

- `provider` from `auto.reviewer.provider` (ideally different from
  `agent.provider`)
- read-only permissions (Claude: `--permission-mode plan` + `review_tools`;
  Codex: `--sandbox read-only`)
- `cwd = repo.local_path` (orchestrator's local clone, not a worktree —
  review is read-only)
- prompt = canonical reviewer prompt + plan_text + issue.title +
  issue.description (no WORKFLOW.md mode suffix; reviewer's role is fixed)

Reviewer must produce a `## Decision` block with a JSON object:

```
## Decision
{"decision": "approve" | "reject", "reasons": ["..."]}
```

If parse fails, treat as `reject` with reason `"malformed reviewer output"`.

---

## 10. Approval

Three modes via `approval.mode`. `gated` is the conservative default. `auto`
is rules + reviewer for less human friction with a prompt-injection-immune
post-impl check. `handoff` is Symphony's no-gate behavior.

The mode is also overridable per issue label via `approval.mode_by_label`
(M7.5+, see proposal 0001). Example: `mode_by_label: { "type:marketing-ads":
"gated", default: "auto" }`. The job's frozen `AxisKey` / `AxisSource`
appears in `symphony-go status` and the PR body's `## Provenance` line.

### Mode: `gated`

After plan posted:
1. Replace label → `awaiting-approval`.
2. On each poll, list comments since `plan_comment_id`.
3. Match `approval.command` (default `/symphony approve`).
4. Verify commenter has `write` / `maintain` / `admin` permission.
5. On match: save `approval_comment_id`, transition to `implementing`.

### Mode: `auto`

After plan posted:

1. Parse `## Scope` block. On parse failure → fallback to gated, post a
   comment explaining the missing block.
2. Iterate `auto.rules` in order. First match wins. A rule matches when:
   - issue has at least one of the rule's `issue_labels`, **OR**
     `issue_labels: []` (catch-all)
   - **AND** `len(plan_scope.files_touched) <= max_plan_files_claimed`
3. On rule match:
   - `reviewer_required: false` → save `approval_path: "rules"`, transition
     to `implementing`.
   - `reviewer_required: true` → invoke reviewer (§9). On approve → save
     `approval_path: "reviewer"`, transition. On reject → apply
     `fallback_on_reject`:
       - `gated`: post reviewer reasons, transition to `awaiting-approval`.
       - `block`: post reviewer reasons, transition to `blocked`.
4. On no rule match: apply `fallback_on_no_rule_match` (default `gated`).

### Mode: `handoff`

After plan posted: transition immediately to `implementing`. No approval.
Symphony default. Use only when workspace + tool restrictions are sufficient
for your safety model.

### Diff verification (auto mode only)

Independent of approval path. Before commit, if
`auto.verify_diff_matches_plan: true`:

1. Compute `files_actually_touched` from `git status --porcelain`.
2. `extra = files_actually_touched - plan_scope.files_touched`.
3. If `len(extra) > max_diff_drift_files`: set `blocked`, post a comment
   listing `extra`, do not commit. Auto-approval is invalidated post-hoc.
4. If actual diff is a subset of claimed scope (within drift tolerance):
   pass.

This is the safety property that makes auto mode robust against prompt
injection: even if a malicious issue tricks the agent into approving a huge
change, the diff verification blocks the PR.

### Why `auto` is more than two LLM calls

A naive "agent self-approve" provides zero safety because the same agent
that produced the plan trivially approves it. `auto` mode adds three
independent gates:

1. **Rules engine** — pure code, immune to prompt injection. Caps
   `files_touched` claim before any LLM judges.
2. **Reviewer agent** — different provider when possible, read-only, with
   a fixed system prompt outside `WORKFLOW.md`'s control.
3. **Diff verification** — pure code, runs after implementation, catches
   any lying about scope.

Bypassing all three requires (a) the agent to produce a plan whose claimed
scope is small enough to clear rules, (b) the reviewer to approve, (c) the
actual diff to match the claimed scope. Hard to break with prompt injection
alone.

---

## 11. Implementation phase

```
verify issue still open, label still `implementing`, stop label absent
verify workspace clean
render prompt with implementation suffix and the approved plan_text
hooks.before_run                                  # abort phase if fails
run agent
hooks.after_run                                   # log only
git status --porcelain
  empty: post "no changes produced", set blocked, stop
if approval.mode == "auto" and auto.verify_diff_matches_plan:
  compute files_actually_touched
  if len(files_actually_touched - plan_scope.files_touched) > max_diff_drift_files:
    post comment listing extra files, set blocked, stop
run each validation command in repo cwd, redacted output
  any failure: post summary, set failed, stop
git add -A
git -c user.name=symphony-go -c user.email=noreply@local commit -m "Implement issue #<n>"
git push origin <branch>                          # see auth note below
create draft PR (truncate body to 60000 chars)
post comment linking PR
replace label `implementing` → `pr-ready`
save pr_number
```

PR title: `[agent] <issue title>` truncated to 70 chars.

Push uses a temporary `extraheader` for HTTPS auth with `GITHUB_TOKEN`,
scoped to the single push command, never written to disk:

```
git -c http.extraheader="AUTHORIZATION: bearer $GITHUB_TOKEN" \
    push origin <branch>
```

PR body includes the `approval_path` (`rules` / `reviewer` / `human` /
`handoff`) so reviewers know which gate let this through.

---

## 12. Doctor

Run `symphony-go doctor` to verify:

1. `<resolved config>` exists, parses, validates.
2. `<resolved config>` is NOT inside any `repo.local_path`. Hard fail.
3. `<local_path>/<workflow_file>` exists and renders with empty bindings.
4. `GITHUB_TOKEN` env var set.
5. `GET /repos/{full_name}` returns 200.
6. Token has `write`/`maintain`/`admin` permission on the repo.
7. All 8 labels exist (lists missing).
8. `git`, `<agent.command>` in PATH.
9. `local_path` is a git repo with `origin` remote pointing at `full_name`.
10. `base_branch` exists locally and on origin.
11. Workspace root writable.
12. If `approval.mode == "auto"`:
    - `auto.rules` has at least one entry with `issue_labels: []` OR
      `fallback_on_no_rule_match` is set.
    - `auto.reviewer.provider` is in PATH (if any rule has
      `reviewer_required: true`).
    - Warn if `auto.reviewer.provider == agent.provider`.

Non-zero exit on any failure. No `--fix` flag in MVP.

---

## 13. Audit

`.symphony-go/audit/{issue_number}.jsonl`. One event per action via `slog`
JSON handler. Redact configured patterns and the values of any allowlist
env vars before writing.

Events: `claim`, `worktree_created`, `hook_started`, `hook_completed`,
`planning_started`, `planning_completed`, `plan_posted`, `scope_parsed`,
`rule_matched`, `reviewer_started`, `reviewer_completed`, `auto_approved`,
`approved`, `implementation_started`, `implementation_completed`,
`diff_verified`, `diff_drift_detected`, `validation_command`,
`validation_completed`, `committed`, `pushed`, `pr_created`, `failed`,
`blocked`, `stop`, `reconcile_action`.

---

## 14. Tests

Unit:

- config parser + defaults, including `auto` block validation
- slug generation (CJK, emoji, leading dash, all punctuation, very long)
- branch name generation
- reconcile table — one test per row of the 19-row table
- approval permission logic (gated mode)
- redaction
- prompt template substitution
- claude argv construction (snapshot, three phases including review)
- codex argv construction (snapshot, exec and app-server, three phases)
- env builder (allowlist + block patterns; `GITHUB_TOKEN` always dropped)
- hook command execution (timeout + per-phase failure semantics)
- `## Scope` parser (valid, missing fields, malformed YAML, no block)
- rules engine evaluation (label match, catch-all, file-count cap, no match)
- reviewer JSON output parser (valid approve, valid reject, malformed)
- diff verification (subset, drift within tolerance, drift exceeds)
- config integrity guard (path-inside-repo rejection, hash drift detection)

Integration with fakes:

- `fakeGitHub`: in-memory issue + label + comment store
- `fakeRunner`: returns canned plan / canned diff with configurable scope
- `fakeReviewer`: returns canned `approve` / `reject` / malformed
- `fakeExec`: scripted validation and hook results
- gated end-to-end: ready → awaiting-approval → approve comment → pr-ready
- auto rules-only end-to-end: docs label + small scope → pr-ready
- auto reviewer approve end-to-end: catch-all + reviewer approve → pr-ready
- auto reviewer reject + fallback gated → human approval flow
- auto reviewer reject + fallback block → blocked
- auto diff drift exceeds → blocked even with clean approval path
- handoff end-to-end: ready → pr-ready with no approval
- crash mid-implementation → blocked on restart (not resumed)
- after_create failure on first run → blocked

---

## 15. Implementation milestones

Build in this order. Phases marked `[parallel]` can be worked on
independently once shared types are defined.

**M0. Skeleton.**
- `git init`, `go mod init`, `.gitignore`, `README.md`.
- `cmd/symphony-go/main.go` with stub subcommands.
- `internal/types/types.go` defining: `Issue`, `IssueComment`, `Job`,
  `JobStatus`, `Config`, `PlanScope`, `RunRequest`, `RunResult`, `Phase`,
  `ReviewerDecision`, `ApprovalMode`, `ApprovalPath`.

**M1. Foundation packages.** [parallel]
- `internal/config`: parse + validate `config.yml`, load `WORKFLOW.md`,
  Config integrity guard, env-var indirection.
- `internal/state`: JSON job-state, flock, atomic writes, status helpers.
- `internal/github`: go-github wrapper (`ListReadyIssues`,
  `ReplaceStateLabel`, `PostIssueComment`, `ListIssueComments`,
  `GetCollaboratorPermission`, `CreateDraftPR`, `FindPRsByHead`).
- `internal/workspace`: slug, branch name, worktree create, dirty check.
- `internal/exec`: `bash -lc` runner with timeout, redaction, command
  policy (deny `sudo`, `curl|sh`, ssh, etc).

**M2. Runner layer.** [partly parallel]
- `internal/runner` interface + `fakeRunner` (table-driven canned outputs).
- `internal/runner/claude.go` (planning/review/implementation).
- `internal/runner/codex.go` (`exec` mode for MVP; `app-server` later).

**M3. Approval layer.** [depends on M2]
- `internal/approval/scope.go`: `## Scope` block parser.
- `internal/approval/rules.go`: rules engine (label + count match).
- `internal/approval/reviewer.go`: reviewer driver, parses `## Decision`.
- `internal/approval/diff.go`: diff verifier vs `plan_scope.files_touched`.

**M4. Orchestrator.** [depends on M1–M3]
- `internal/orchestrator/loop.go`: poll loop, reconcile, dispatch.
- `internal/orchestrator/job.go`: per-job state machine (claim → planning
  → approval → implementation → PR).
- `internal/orchestrator/reconcile.go`: 19-row table.
- `cmd/symphony-go/main.go`: wire `run`, `run --once`, `doctor`.

**M5. Tests + hardening.** [continuous]
- Unit tests per package.
- Integration tests with all fakes covering §14 scenarios.
- `SIGTERM` → cancel agent → mark blocked → save state → exit clean.
- Redaction across all log paths.

**M6. Real-runner smoke.** [final, manual]
- `claude` planning + reviewer + implementation against a sandbox repo issue.
- `codex exec` same.

**M7. Optional, post-MVP.**
- `codex app-server` runner with streaming events.
- `symphony-go status` command.
- Multi-turn continuation.
- Workspace cleanup command.

**M7.5. Per-axis configuration (proposal 0001).**
- `repo.workflow_files`, `validation.commands_by_label`,
  `claude.*_by_label`, `codex.*_by_label`, `approval.mode_by_label`.
- `Job.AxisKey` / `AxisSource` frozen at claim time and surfaced in
  status + PR body provenance.
- Doctor: per-axis collision/default checks, workflow-file existence.

---

## 16. Definition of Done

```
1. go test ./... passes
2. doctor exits 0 on a real repo
3. symphony-go run --once processes a real issue end-to-end with fakeRunner
4. claude runner completes a planning run on a real issue
5. codex runner (exec mode) completes a planning run on a real issue
6. config in ~/.symphony-go/, never inside the workspace; doctor enforces
7. agent subprocess receives neither GITHUB_TOKEN nor SSH_AUTH_SOCK
8. PRs are draft; orchestrator never merges
9. flock prevents two instances on the same machine
10. crash + restart hits the reconcile table, not the dispatch loop
11. workspace hooks run in correct order with correct cwd, env, and timeout
12. auto mode: rules + reviewer + diff verification all work end-to-end
13. handoff mode skips approval cleanly; gated mode polls for approval
```

---

## 17. Non-goals

**Out of MVP scope, supported as opt-in:** Codex `app-server` streaming.

**Deferred:** multi-turn continuation, per-state concurrency limits,
blocker-aware dispatch, event-inactivity stall detection, dynamic config
reload during runs.

**Out of scope, by design:** Linear, web UI, TUI, multi-agent teams, MCP,
dashboards, automatic merge, deployment, distributed workers, model
budgeting, container sandboxing, automatic dependency updates,
agent-controlled tracker writes, naive single-agent self-approve.

---

## 18. Hard rule

Boring Go. Small interfaces. Explicit state transitions. Deny by default.
Atomic label changes. Three independent gates in auto mode (rules,
reviewer, diff verification) — no single LLM call decides whether code
ships. One developer should be able to read this whole repo in an
afternoon.
