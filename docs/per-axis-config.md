# Per-axis configuration

Operator-facing how-to for symphony-go's per-axis (label-driven) config.
Spec: `docs/proposals/0001-per-axis-config.md` (proposal 0001), shipped
in M7.5.

## 1. Why

A single GitHub repo often serves more than one *axis* of work — code,
research notes, marketing assets, catalog updates. Each axis wants its
own prompt template, validation pipeline, agent tool surface, and
approval posture. Symphony-go lets one orchestrator process drive every
axis on one repo by keying each affected config knob off the issue's
labels, while keeping the simple scalar form unchanged for code-only
repos.

## 2. The five-axis example

The print-my-ideas operator runs five axes through one orchestrator:

| Axis | Output | Right tool surface | Right approval |
|---|---|---|---|
| `type:code` | code diff + tests | Edit, Write, `Bash(git/npm/go test)` | auto |
| `type:research` | `docs/research/<n>.md` | Read, Write, WebFetch, WebSearch | auto |
| `type:catalog` | record file + Shopify mutation | `Bash(node shopify/scripts/sync.mjs)` | auto |
| `type:marketing-creative` | manifest + image assets | `Bash(node shopify/scripts/gen_creative.mjs)` | auto |
| `type:marketing-discount` | record + Shopify discount API | `Bash(node shopify/scripts/create_discount.mjs)` | gated |
| `type:marketing-ads` | record + Meta Ads API | `Bash(node shopify/scripts/launch_ad.mjs)` | gated |

## 3. Full config snippet

```yaml
repo:
  full_name: "you/print-my-ideas"
  base_branch: "main"
  local_path: "/Users/you/code/print-my-ideas"
  workflow_files:
    "type:code":              "operations/workflows/WORKFLOW.code.md"
    "type:research":          "operations/workflows/WORKFLOW.research.md"
    "type:catalog":           "operations/workflows/WORKFLOW.catalog.md"
    "type:marketing-creative": "operations/workflows/WORKFLOW.creative.md"
    "type:marketing-discount": "operations/workflows/WORKFLOW.discount.md"
    "type:marketing-ads":      "operations/workflows/WORKFLOW.ads.md"
    default:                   "operations/workflows/WORKFLOW.code.md"

validation:
  commands_by_label:
    "type:code":              ["cd shopify && npm run typecheck", "go test ./..."]
    "type:research":          ["test -n \"$(ls docs/research/*.md 2>/dev/null)\""]
    "type:catalog":           ["node shopify/scripts/sync.mjs --dry-run"]
    "type:marketing-creative": ["node shopify/scripts/gen_creative.mjs --check"]
    "type:marketing-discount": ["node shopify/scripts/create_discount.mjs --dry-run"]
    "type:marketing-ads":      ["node shopify/scripts/launch_ad.mjs --dry-run"]
    default:                   []

claude:
  max_turns: 20
  implementation_tools_by_label:
    "type:code":               [Read, Edit, Write, "Bash(git:*)", "Bash(npm test:*)", "Bash(go test:*)"]
    "type:research":           [Read, Write, WebFetch, WebSearch]
    "type:catalog":            [Read, Write, "Bash(node shopify/scripts/sync.mjs:*)"]
    "type:marketing-creative": [Read, Write, "Bash(node shopify/scripts/gen_creative.mjs:*)"]
    "type:marketing-discount": [Read, Write, "Bash(node shopify/scripts/create_discount.mjs:*)"]
    "type:marketing-ads":      [Read, Write, "Bash(node shopify/scripts/launch_ad.mjs:*)"]
    default:                   [Read, Edit, Write]
  disallowed_tools_by_label:
    default: ["Bash(sudo:*)", "Bash(curl:*)"]

codex:
  mode: "exec"
  implementation_args_by_label:
    "type:research": ["--sandbox", "read-only"]
    "type:code":     ["--sandbox", "workspace-write"]
    default:         ["--sandbox", "workspace-write"]

approval:
  command: "/symphony approve"
  mode_by_label:
    "type:marketing-ads":      "gated"
    "type:marketing-discount": "gated"
    default:                   "auto"
```

## 4. Behavior table

| Axis | Workflow file | Validation | Tool surface | Approval |
|---|---|---|---|---|
| `type:code` | `WORKFLOW.code.md` | typecheck + go test | Edit, Write, restricted Bash | auto |
| `type:research` | `WORKFLOW.research.md` | research file present | Read, Write, Web* | auto |
| `type:catalog` | `WORKFLOW.catalog.md` | sync dry-run | Read, Write, sync.mjs | auto |
| `type:marketing-creative` | `WORKFLOW.creative.md` | creative dry-run | Read, Write, gen_creative.mjs | auto |
| `type:marketing-discount` | `WORKFLOW.discount.md` | discount dry-run | Read, Write, create_discount.mjs | gated |
| `type:marketing-ads` | `WORKFLOW.ads.md` | ad dry-run | Read, Write, launch_ad.mjs | gated |
| (none of the above) | `WORKFLOW.code.md` | (empty) | default | auto |

## 5. Resolution rules

1. **First match wins, in declared YAML order.** `default` is excluded
   from the search and used only as a last fallback.
2. **Required `default` key.** Every `*_by_label` map must have one.
   Both `Validate` (at config-load) and `doctor` reject missing defaults.
3. **No scalar+map collisions for the same knob.** `validation.commands`
   AND `validation.commands_by_label` is rejected; same for every other
   pair. Pick one.
4. **Frozen at claim time.** The orchestrator resolves each issue's
   axis once when the job claims `symphony:ready`, persists it as
   `Job.AxisKey`, and re-uses that frozen value for every per-axis
   lookup until the job terminates. Mid-run label edits do not divert
   behavior.

## 6. Multi-label issues

Order matters because the resolver walks `Keys` in declared order. If
an issue carries both `type:code` and `type:research`, whichever key
appears first in your `workflow_files` map wins. Recommendation: put at
most one `type:*` label on each issue, and reserve secondary axes
(`budget:*`, `area:*`) for orthogonal toggles that don't drive workflow
selection. Doctor's "label key never appears on any open issue" warning
helps catch typos like `type-code` vs `type:code`.

## 7. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Startup error: "no axis match and no default" | a per-axis map is missing a `default:` key | add `default:` last in the map |
| Startup error: "mutually exclusive" | both scalar and `*_by_label` set for the same knob | delete one of them |
| Wrong workflow file rendered | declaration order has the wrong key first | reorder `workflow_files` keys; first match wins |
| Validation runs `go test` on a research issue | `commands_by_label` missing the `type:research` key | add the key, or rely on `default` if intended |
| Issue stuck in `awaiting-approval` despite global `approval.mode: auto` | `mode_by_label["type:..."]` overrides to `gated` | remove the override or post `/symphony approve` |
| `claude` running with the wrong tool list | `Job.AxisKey` was frozen before you fixed the map | jobs are frozen at claim; cancel + relabel + re-ready |
| Doctor warns "key X never matches any open issue" | typo in label name (`type-code` vs `type:code`) | fix the label or the map key |

## 8. FAQ

**Why the `type:` prefix?** Convention only — the resolver compares
labels case-insensitively but is otherwise prefix-agnostic. Using a
shared prefix keeps the GitHub label list scannable and makes "axis"
labels visually distinct from `area:*` / `budget:*` / `priority:*`.

**Can two axes share a workflow file?** Yes. Map two keys to the same
file path. The orchestrator pre-loads each unique file once.

**Can a non-`type:*` label drive an axis?** Yes — the resolver only
looks at the keys you declared. `budget:over-50: "gated"` is a valid
entry under `approval.mode_by_label`.

**Does the axis change mid-run if I edit labels?** No. The axis is
resolved once at job claim, frozen on the `Job` record as `AxisKey`,
and read back from the local state for every per-axis lookup. Even a
crash + restart preserves it.

**How does this interact with reconcile?** The 19-row reconcile table
(SPEC §7) is unchanged. Reconcile reads `AxisKey` from local state, so
crash-recovered jobs keep using the same axis they started with.

**Does the reviewer agent vary per axis?** No. `auto.reviewer` stays
global to keep the surface area small; the reviewer's job ("does this
plan match the issue's declared scope?") is the same shape regardless
of axis.

**Can I leave per-axis maps unset?** Absolutely. Every `*_by_label`
field is optional. A config with only scalar fields parses, validates,
and runs byte-identically to symphony-go before M7.5.
