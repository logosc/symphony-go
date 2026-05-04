# New project onboarding guide

This guide is the practical checklist for bringing a new GitHub project
onto symphony-go. It assumes one operator, one target repo, and a local
host that runs `symphony-go run` continuously.

The setup pattern is:

1. Prepare the target repo.
2. Create GitHub App credentials.
3. Keep config outside the repo.
4. Add labels and workflow prompts.
5. Run `doctor`.
6. Smoke each approval path.
7. Leave a short validation run or longer soak running.

## Fast path: guided scripts

If you want the guided path, use the scripts first and then read the
rest of this guide for the reasoning behind each generated file.

```sh
cd ~/Documents/Github/symphony-go

scripts/onboard-project.sh \
  --repo owner/project \
  --local-path /Users/you/Documents/Github/project \
  --auth app \
  --approval handoff \
  --agent claude \
  --model sonnet
```

This creates labels, writes `~/.symphony-go/config.yml`, writes an env
template at `~/.symphony-go/.env`, and copies `WORKFLOW.md` into the
target repo if one does not exist.

After filling credentials in the env file and committing `WORKFLOW.md`
in the target repo, run:

```sh
set -a; source ~/.symphony-go/.env; set +a
~/go/bin/symphony-go doctor --config ~/.symphony-go/config.yml

scripts/onboard-smoke.sh \
  --repo owner/project \
  --config ~/.symphony-go/config.yml \
  --env-file ~/.symphony-go/.env \
  --run-once \
  --status
```

The scripts are intentionally small Bash wrappers around `gh` and
`symphony-go`; they do not create GitHub Apps or collect secret values
for you.

## 0. Decide the operating model

Before touching credentials, decide these knobs:

| Decision | Conservative default |
|---|---|
| Tracker | GitHub Issues in the same repo symphony-go will edit |
| GitHub auth | GitHub App installation, not PAT |
| Config location | `~/.symphony-go/config.yml` or a symlink to a private platform repo |
| First approval mode | `handoff` for the first trivial smoke, then `gated` / `auto` |
| Agent provider | `claude` for code/docs; `codex` for image/mockup axes |
| Merge policy | Draft PR only; human merges in GitHub or an optional trusted chat bridge |

Do not put `config.yml` inside the target repo. The agent can edit its
workspace; config controls its permissions and must live outside that
workspace.

## 1. Install symphony-go

```sh
cd ~/Documents/Github/symphony-go
go install ./cmd/symphony-go
~/go/bin/symphony-go --help
```

Use the explicit binary path in launch scripts so a stale `PATH` does
not accidentally run an older build.

## 2. Prepare the target repo

Clone the repo locally and make sure `main` is clean:

```sh
REPO=owner/project
LOCAL=/Users/you/Documents/Github/project

gh repo clone "$REPO" "$LOCAL"
git -C "$LOCAL" status --short --branch
```

Add the base label set:

```sh
for label in ready planning awaiting-approval implementing pr-ready failed blocked stop; do
  gh -R "$REPO" label create "symphony:$label" --force
done
```

Add any routing labels your project will use:

```sh
gh -R "$REPO" label create "type:code" --force
gh -R "$REPO" label create "type:research" --force
gh -R "$REPO" label create "budget:over-50" --force
```

## 3. Register the GitHub App

Follow [`docs/github-app-setup.md`](./github-app-setup.md). Minimum repo
permissions:

| Permission | Access |
|---|---|
| Contents | Read/write |
| Issues | Read/write |
| Pull requests | Read/write |
| Metadata | Read-only |

Install the App only on the target repo. Put the private key outside
the repo:

```sh
mkdir -p ~/.symphony-go
mv ~/Downloads/<app-name>.*.private-key.pem ~/.symphony-go/github-app.pem
chmod 600 ~/.symphony-go/github-app.pem
```

Create an env file that launch scripts can source:

```sh
cat > ~/.symphony-go/.env <<'EOF'
GITHUB_APP_ID=1234567
GITHUB_APP_INSTALLATION_ID=123456789
GITHUB_APP_PRIVATE_KEY_PATH=$HOME/.symphony-go/github-app.pem
EOF
chmod 600 ~/.symphony-go/.env
```

If your validation or workflow hooks need provider secrets, put only the
specific required variables in this env file. Avoid broad shell-profile
inheritance.

## 4. Write external config

Start from the example:

```sh
cp ~/Documents/Github/symphony-go/testdata/config.example.yml \
  ~/.symphony-go/config.yml
chmod 600 ~/.symphony-go/config.yml
```

> **Tip — multi-project setups.** If you run more than one symphony-go
> instance on the same machine, name each config after its project
> (e.g. `~/.symphony-go/myproject.yml`) instead of the implicit-default
> `~/.symphony-go/config.yml`, and always pass `--config <path>`
> explicitly to disambiguate.

Minimal single-axis config:

```yaml
repo:
  full_name: "owner/project"
  base_branch: "main"
  local_path: "/Users/you/Documents/Github/project"
  workflow_file: "WORKFLOW.md"

github:
  # Inline values are simplest. App ID and installation ID are publicly
  # visible in GitHub URLs (not secrets); the actual secret is the PEM
  # file. For shared infra/CI, swap each line for the *_env variant
  # (e.g. app_id_env: "MY_APP_ID") to read from a named env var instead.
  auth: "app"
  app_id: 1234567
  installation_id: 12345678
  private_key_path: "/Users/you/.symphony-go/myproject-app.pem"
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
  mode: "handoff"
  command: "/symphony approve"
  require_token: false
  require_write_permission: true
  ignored_users:
    - "symphony-go[bot]"
    - "github-actions[bot]"

agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600

validation:
  commands:
    - "go test ./..."
  command_timeout_seconds: 900
```

For multi-axis repos, use `workflow_files`, `mode_by_label`,
`provider_by_label`, `model_by_label`, and per-label validation. See
[`docs/per-axis-config.md`](./per-axis-config.md).

### Approval comments and trusted bridges

By default, gated approval is the static slash command in
`approval.command`. You can make gated approval require a per-plan
numeric token instead:

```yaml
approval:
  mode: "gated"
  command: "/symphony approve"
  require_token: true
  require_write_permission: true
```

With `require_token: true`, symphony-go appends an `## Approval` footer
to the plan comment. The approver must comment exactly that number. The
token changes whenever planning re-runs, so a stale approval cannot
promote a fresh plan. Existing awaiting jobs without a saved token still
fall back to `approval.command` for compatibility.

If another service authenticates the human operator and then posts the
approval comment through its own GitHub App, add that bot login to
`approval.trusted_users`:

```yaml
approval:
  command: "/symphony approve"
  require_token: true
  require_write_permission: true
  trusted_users:
    - "my-chief-of-staff[bot]"
```

Only use this for a bridge that enforces its own operator allowlist. Do
not add the symphony-go bot itself to `trusted_users`. In token mode,
the bridge must post the current plan token, not `/symphony approve`.

## 5. Add the workflow prompt

For a single-axis repo:

```sh
cp ~/Documents/Github/symphony-go/testdata/WORKFLOW.example.md \
  "$LOCAL/WORKFLOW.md"
git -C "$LOCAL" add WORKFLOW.md
git -C "$LOCAL" commit -m "add symphony workflow"
git -C "$LOCAL" push origin main
```

For a multi-axis repo, prefer one prompt per axis:

```text
operations/workflows/WORKFLOW.code.md
operations/workflows/WORKFLOW.research.md
operations/workflows/WORKFLOW.deploy.md
```

Each workflow prompt should define:

- What the agent may change.
- What output file or PR shape is expected.
- Which commands it should run before finishing.
- Any hard refusal rules.

Keep permission rules in `config.yml`, not in the prompt.

## 6. Run doctor

```sh
set -a
source ~/.symphony-go/.env
set +a

~/go/bin/symphony-go doctor --config ~/.symphony-go/config.yml
```

Do not run live tickets until `doctor` passes. It should verify:

- Config is outside `repo.local_path`.
- GitHub auth works.
- Required labels exist.
- Workflow files exist.
- Validation commands are syntactically configured.
- Agent CLIs are available for configured providers.

## 7. First smoke: handoff mode

Use the smallest possible issue:

```sh
gh -R "$REPO" issue create \
  --title "T-smoke: add hello file" \
  --body "Create docs/hello-symphony.md containing one sentence. Do not touch any other file." \
  --label "symphony:ready" \
  --label "type:code"
```

Run once:

```sh
set -a; source ~/.symphony-go/.env; set +a
~/go/bin/symphony-go run --config ~/.symphony-go/config.yml --once
```

Expected result:

- Issue moves through `symphony:planning` and `symphony:implementing`.
- A plan comment appears.
- Validation runs.
- A draft PR opens.
- Issue gets `symphony:pr-ready`.

Merge or close that PR manually.

## 8. Second smoke: gated approval

Change config:

```yaml
approval:
  mode: "gated"
  command: "/symphony approve"
  require_token: false
  require_write_permission: true
```

Create another tiny issue with `symphony:ready`. Run symphony-go. It
should stop at `symphony:awaiting-approval` after posting the plan.

Approve from a GitHub account with write permission:

```sh
gh -R "$REPO" issue comment <issue-number> --body "/symphony approve"
```

Run or keep the daemon running. Expected result: implementation resumes
and opens a draft PR.

To smoke the stronger read-the-plan gate, enable token mode:

```yaml
approval:
  mode: "gated"
  command: "/symphony approve"
  require_token: true
  require_write_permission: true
```

Create another tiny issue. The plan comment should include an
`## Approval` footer with a 4-digit token. Comment exactly that token
from a write-permission account:

```sh
gh -R "$REPO" issue comment <issue-number> --body "7392"
```

Use the real token from the plan comment, not the example above.

Also test rejection:

```sh
gh -R "$REPO" issue edit <issue-number> --add-label "symphony:stop"
```

Expected result: symphony-go marks the job blocked.

## 9. Third smoke: auto approval

Use `auto` only after handoff and gated paths work:

```yaml
approval:
  mode: "auto"
  command: "/symphony approve"
  require_token: false
  require_write_permission: true

auto:
  rules:
    - issue_labels: ["type:code"]
      max_plan_files_claimed: 5
      reviewer_required: true
  reviewer:
    provider: "codex"
    model: "gpt-5.5"
    timeout_seconds: 600
  fallback_on_reject: "gated"
  fallback_on_no_rule_match: "gated"
  verify_diff_matches_plan: true
  max_diff_drift_files: 2
```

Expected result for a tiny bounded issue:

- Rules match.
- Reviewer approves.
- Implementation runs.
- Diff verifier confirms touched files match the plan.
- Draft PR opens.

If any stage rejects, the issue should fall back to
`symphony:awaiting-approval` or `symphony:blocked`, depending on the
configured fallback and error.

## 10. Optional: chat bridge

symphony-go does not require Telegram, Google Chat, Slack, or any other
chat product. Chat is an optional convenience layer. Use the platform
your company already uses; the same bridge shape works for Telegram,
Google Chat, Slack, Teams, or an internal dashboard.

A chat bridge is a separate service that:

1. Receives GitHub webhooks for issue comments and draft PRs.
2. Sends operator buttons to chat.
3. Verifies the chat user is allowlisted.
4. Posts GitHub comments or merges PRs through its own GitHub App.

Minimum bridge callbacks:

| Button | GitHub action |
|---|---|
| Approve | Comment the required approval string on the issue: `/symphony approve` when `require_token: false`, or the current plan token when `require_token: true` |
| Reject | Add `symphony:stop` and comment the rejection reason |
| Merge | Mark draft PR ready if needed, then merge |
| Close | Close PR |

If the bridge posts approvals as `my-bridge[bot]`, configure that login
under `approval.trusted_users` as described above.

Smoke the bridge with real buttons:

1. Gated issue reaches `symphony:awaiting-approval`.
2. Approve button posts the current approval string.
3. symphony-go accepts the approval and opens a draft PR.
4. Reject button moves a separate test issue to `symphony:blocked`.
5. Merge button merges the draft PR.

For Google Chat specifically, implement cards with buttons that call
your bridge webhook. The webhook should verify the Google Workspace user
or group, then perform the GitHub action. If token mode is enabled, show
the token in the card and have the Approve action post that token.

## 11. Run continuously

Start in a detachable supervisor. A simple `tmux` session is enough for
single-operator use:

```sh
mkdir -p "$LOCAL/.symphony-go/soak"
LOG="$LOCAL/.symphony-go/soak/symphony-go-$(date -u +%Y%m%dT%H%M%SZ).log"

tmux new-session -d -s symphony-go \
  "cd '$LOCAL' && set -a; source ~/.symphony-go/.env; set +a; \
   ~/go/bin/symphony-go run --config ~/.symphony-go/config.yml 2>&1 | tee -a '$LOG'"
```

Health checks:

```sh
~/go/bin/symphony-go status --config ~/.symphony-go/config.yml
tmux capture-pane -pt symphony-go:0 -S -120 | tail -n 120
```

For a new project, validate at least one short live run that includes
the critical paths you intend to use. For production-shaped setups,
prefer a full operator-workday soak before relying on it unattended.

## 12. Completion checklist

Use this before declaring the project onboarded:

- [ ] `doctor` passes with the same config and env used by the daemon.
- [ ] Config lives outside the target repo.
- [ ] All labels exist in GitHub.
- [ ] Workflow prompt files exist on `main`.
- [ ] Handoff smoke opened a draft PR.
- [ ] Gated approval smoke paused, then resumed after approval.
- [ ] Token approval smoke paused, then resumed after commenting the
      current plan token, if `approval.require_token` is enabled.
- [ ] Stop/reject path marks an issue blocked.
- [ ] Auto mode smoke passed rules, reviewer, and diff verification, if
      auto mode is enabled.
- [ ] Merge path is proven manually or through the trusted bridge.
- [ ] Logs are written to a known path.
- [ ] Old orchestration for the same repo is stopped, if one existed.

When this checklist is complete, leave `symphony-go run` up and treat
GitHub Issues plus draft PRs as the audit trail.
