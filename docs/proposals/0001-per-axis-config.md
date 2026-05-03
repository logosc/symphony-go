# Proposal 0001 — Per-axis configuration

| Field | Value |
|---|---|
| Status | Draft |
| Author | print-my-ideas operator |
| Target milestone | M7.5 (between M7 and a hypothetical M8) |
| Affects | `internal/config`, `internal/runner`, `internal/approval`, `cmd/symphony-go/doctor`, `SPEC.md` §2/§9/§10 |
| Backward compatible | Yes |
| Closes gaps | G11, G12, G13, G16 (and G15 as an optional follow-up) |

## 1. Summary

Allow `workflow_file`, `validation.commands`, the agent tool/sandbox
allowlists, and `approval.mode` to vary per issue label, while keeping
the existing scalar form as the default. One symphony-go process drives
multiple work axes (code, research, marketing, deploy, catalog) on one
repo, each with its own prompt, validation, tool surface, and approval
posture.

## 2. Motivation

symphony-go today assumes one repo == one axis of work: one
`WORKFLOW.md`, one validation command set, one tool allowlist, one
approval mode. This is correct for code-only repos but artificially
narrow for shops that already use a single repo as the audit trail for
multiple kinds of work.

Concretely, the print-my-ideas monorepo runs five axes through one
orchestrator today (under the Elixir Symphony predecessor):

| Axis | Output | Right tool surface | Right approval |
|---|---|---|---|
| `type:code` | code diff + tests | Edit, Write, `Bash(git/npm/go test)` | auto |
| `type:research` | `docs/research/<n>.md` | Read, Write, WebFetch, WebSearch | auto |
| `type:catalog` | record file + Shopify mutation | `Bash(node shopify/scripts/sync.mjs)` | auto |
| `type:marketing-creative` | manifest + image assets | `Bash(node shopify/scripts/gen_creative.mjs)` | auto |
| `type:marketing-discount` | record file + Shopify discount API call | `Bash(node shopify/scripts/create_discount.mjs)` | **gated** |
| `type:marketing-ads` | record file + Meta Ads API call | `Bash(node shopify/scripts/launch_ad.mjs)` | **gated** |

Without per-axis config, each axis needs its own orchestrator instance,
or all axes share the union of every tool allowlist (unsafe) and the
strictest validation (impractical — `go test ./...` doesn't apply to a
research report).

This proposal is also useful outside print-my-ideas: any team that uses
GitHub issues to track docs, ADRs, and code on one repo benefits from
the same shape.

## 3. Goals

- Per-issue selection of `WORKFLOW.<axis>.md`.
- Per-issue selection of validation commands.
- Per-issue selection of agent tool allowlist (Claude) and sandbox args
  (Codex).
- Per-issue selection of approval mode.
- Backward compatibility: existing scalar config keeps working unchanged.
- Deterministic, auditable label → axis resolution.
- `doctor` extended to validate the new shape end-to-end.

## 4. Non-goals

- A pluggable tracker (still GitHub Issues only).
- Multiple repos per process.
- Per-axis concurrency limits (still global, see G10).
- A new prompt-templating language (literal `{{ ... }}` substitution
  unchanged).
- Hot-reload of the new sections (same integrity guard rules apply).

## 5. Design

### 5.1 Label resolution

Introduce a single resolver used by every per-axis lookup:

```
resolveAxis(issue, mapping) -> string
  for key in mapping.keys (in declared order, "default" excluded):
    if key in issue.labels: return key
  return "default" if "default" in mapping else error
```

- **First match in declared order wins.** Same shape as
  `auto.rules` (SPEC §10), reused for parser/operator familiarity.
- `default` is required for any per-axis map. Doctor enforces this.
- Resolution is performed once per job, at job start, and frozen on the
  `Job` record so that mid-run label edits don't change behavior.

### 5.2 Config schema (additions)

All new fields are optional. The existing scalar fields remain valid
and take precedence when no per-axis map is set.

```yaml
repo:
  workflow_file: "WORKFLOW.md"        # existing, scalar
  workflow_files:                     # new, optional map
    "type:code":     "operations/workflows/WORKFLOW.code.md"
    "type:research": "operations/workflows/WORKFLOW.research.md"
    default:         "operations/workflows/WORKFLOW.code.md"

approval:
  mode: "auto"                        # existing, scalar
  mode_by_label:                      # new, optional map
    "type:marketing-ads":      "gated"
    "type:marketing-discount": "gated"
    "budget:over-50":          "gated"
    default:                   "auto"

validation:
  commands: ["go test ./..."]         # existing, scalar
  commands_by_label:                  # new, optional map
    "type:code":     ["cd shopify && npm run typecheck"]
    "type:research": ["test -f docs/research/*.md"]
    default:         []

claude:
  implementation_tools: [...]         # existing, scalar
  implementation_tools_by_label:      # new, optional map
    "type:code":     [Read, Edit, Write, "Bash(git:*)", "Bash(npm test:*)"]
    "type:research": [Read, Write, WebFetch, WebSearch]
    default:         [Read, Edit, Write]
  # planning_tools, review_tools, disallowed_tools: same pattern.

codex:
  implementation_args: [...]          # existing, scalar
  implementation_args_by_label:       # new, optional map
    "type:code":     ["--sandbox", "workspace-write"]
    "type:research": ["--sandbox", "read-only"]
    default:         ["--sandbox", "workspace-write"]
```

### 5.3 Resolution precedence

For each knob (workflow file, mode, validation, tools, codex args):

1. If the `_by_label` map is present, use `resolveAxis(issue, map)`.
2. Else if the scalar field is present, use it.
3. Else fall back to existing built-in defaults (unchanged).

Crucially, the scalar and the map for the same knob **may not both be
set**. Doctor rejects this at startup as ambiguous.

### 5.4 Job record extension

`internal/types.Job` gains a frozen `AxisKey string` and `AxisSource
string` field, persisted in local state. `AxisKey` is the label that
matched (or `"default"`); `AxisSource` is one of `"by_label"` or
`"scalar"`. Both surface in `symphony-go status` and the PR body's
`## Provenance` section.

### 5.5 Reconcile-on-startup interaction

The 19-row reconcile table (§7) is unchanged. The job's frozen
`AxisKey` is read from local state, not re-resolved, so a label edit
between crash and restart can't divert an in-flight run to a different
WORKFLOW or tool surface.

### 5.6 Integrity guard (§2 item 4)

The "agent modified WORKFLOW.md" diff scan widens from one path to the
**set of all paths referenced anywhere in `repo.workflow_files`**, plus
`repo.workflow_file` if present. Any modification triggers the same
warning comment + `## Warnings` block in the PR body.

### 5.7 Doctor

`symphony-go doctor` adds these checks:

1. For each `_by_label` map: `default` key exists.
2. For each `_by_label` map: scalar twin is empty (no ambiguous config).
3. For every workflow file in the workflow-file map: file exists, lives
   under `repo.local_path`, and is not under the resolved config dir.
4. For every key in any `_by_label` map: warn (not fail) if the key
   never matches any label currently used in the repo's open issues.
   Useful "did you mean `type:code` not `type-code`?" check.
5. For `approval.mode_by_label`: every value is one of `gated | auto |
   handoff`.

### 5.8 Reviewer agent (auto mode) — unchanged contract

Reviewer prompt rendering still uses the *resolved* WORKFLOW file for
that axis. Reviewer config (`provider`, `model`, `timeout`) stays
global to avoid an explosion of knobs. Empirically the reviewer's job
is the same shape across axes: "does this plan match the issue's
declared scope?".

### 5.9 PR body

Optional follow-up (G15): per-axis PR body templates under
`pr.body_templates_by_label`. Out of scope for this proposal; mention
to confirm the schema convention generalizes.

## 6. Backward compatibility

- A config with only scalar fields parses, validates, and runs
  byte-identically to today.
- A config that mixes scalars and maps for *different* knobs is fine
  (e.g. scalar `workflow_file` + map `validation.commands_by_label`).
- A config that sets both scalar and map for the *same* knob fails
  fast at startup with a clear error.
- Existing `Job` records without `AxisKey` are migrated on read by
  filling `AxisKey = "default"` and `AxisSource = "scalar"`. No
  on-disk migration tool needed.

## 7. Test plan

Unit:
- `internal/config`: parse all map shapes, reject scalar+map collision,
  reject missing `default`, accept missing `_by_label` (scalar path).
- Resolver: declaration-order match, default fallback, multi-label
  issue, no-match-no-default error.

Runner:
- `claude_test.go` + `codex_test.go`: per-axis tool allowlist appears
  in the spawned-process invocation; scalar path unchanged.

Approval:
- `approval/diff_test.go`: per-axis mode override is honored;
  `gated` axis bypasses rules+reviewer entirely.

Doctor:
- Each new check has a positive and a negative test.

Integration (fakes):
- A two-axis config (`type:code` + `type:research`) drives two issues
  through the orchestrator in one process; assert different
  WORKFLOW files were rendered and different validation commands ran.
- A `type:marketing-ads` issue with `mode_by_label` forcing `gated`
  cannot reach implementation without a `/symphony approve` comment,
  even though the global `approval.mode` is `auto`.

Smoke:
- One real `type:research` ticket on a sandbox repo: agent writes
  `docs/research/sample.md`, validation passes, draft PR opens.

## 8. Documentation

- `SPEC.md` §2 expanded: scalar form is presented first; `_by_label`
  shape introduced as an additive section labeled "Per-axis
  configuration (M7.5+)".
- `SPEC.md` §10 gains `mode_by_label` notes.
- `README.md` differences-table row "Run ends at..." extended to
  mention per-axis modes.
- New `docs/per-axis-config.md` user-facing how-to with the
  print-my-ideas-shaped example.

## 9. Rollout

Single PR per gap, in this order, each independently mergeable:

1. **G11** — `repo.workflow_files` (smallest, exercises the resolver
   skeleton end-to-end).
2. **G12** — `validation.commands_by_label` (reuses resolver).
3. **G13** — `claude.*_by_label` and `codex.*_by_label` (largest, but
   purely additive).
4. **G16** — `approval.mode_by_label` (touches the approval driver,
   smallest blast radius if landed last).

After G16, mark this proposal `Accepted` and bump SPEC §15 with an
"M7.5: per-axis config" line.

## 10. Risks

- **Cognitive load.** Five maps × multiple labels invites confusion.
  Mitigation: doctor's "label key never appears on any open issue"
  warning catches typos early; `symphony-go status` shows the
  resolved axis per job; PR body's `## Provenance` shows it on the
  artifact side.
- **Tool-allowlist sprawl.** Operators may grant overly broad
  `Bash(*)` permissions per axis. Mitigation: SPEC §9 already
  documents deny-by-default expectations; consider linting any
  allowlist entry containing bare `Bash(*)` in doctor (warn, not
  fail). Out of scope for this proposal but tracked separately.
- **Label-resolution surprises with multiple `type:*` labels.**
  Declaration-order resolution is deterministic but not
  intuitive. Mitigation: doctor warns on issues carrying ≥2 labels
  that both appear as keys in the same map; surface the resolved
  axis in the planning comment.

## 11. Open questions

1. **Should `agent.provider` be per-axis too?** Some shops want Claude
   for code, Codex for research. Trivial to add with the same
   pattern. Recommendation: yes, include in G13 since it's the same
   config section.
2. **Should `hooks.after_create` be per-axis?** Probably not — the
   worktree is created once, before the axis is even committed to.
   Keep global. Document explicitly in SPEC §8.
3. **Should the `## Scope` planning contract be relaxed for non-code
   axes?** Tentatively no: declaring artifact paths in
   `files_touched` works and keeps diff verification useful. Revisit
   after the smoke run if the agent struggles with the contract for
   research.

## 12. Out of scope (explicit)

- A pluggable tracker abstraction (Linear, Jira, etc).
- Per-state concurrency limits.
- Per-axis reviewer prompts.
- Per-axis budgeting / rate limits.
- Webhook-driven dispatch (still polling).

These are tracked separately and do not block this proposal.

## 13. Estimated effort

- G11: ~120 LOC + tests, ~0.5 day.
- G12: ~80 LOC + tests, ~0.25 day.
- G13: ~250 LOC + tests, ~1 day (Claude + Codex sides).
- G16: ~60 LOC + tests, ~0.25 day.
- Doctor + SPEC + docs: ~0.5 day.
- **Total: ~2.5 agent-days.**
