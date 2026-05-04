# Proposal 0004 — JSON side-channel for structured agent output

| Field | Value |
|---|---|
| Status | Draft |
| Author | symphony-go maintainer |
| Target milestone | Follow-up to issue print-my-ideas/print-my-ideas#62 |
| Affects | `internal/orchestrator/job.go` (planSuffix, post-plan parsing), `internal/approval/scope.go` (ParseScope), `internal/approval/reviewer.go` (decision parsing), `internal/runner/*` (env injection), `WORKFLOW.md`, `SPEC.md` §2 plan-output-contract |
| Backward compatible | Yes (parser fallback retained for one release) |
| Closes gap | Format-drift class of bugs (e.g. fenced `## Scope`) |

## 1. Summary

Today the orchestrator extracts structured fields from agent prose by
asking the agent to emit a YAML block under a specific markdown
heading (`## Scope`) and parsing it out of the comment body. This is
fragile: LLMs frequently violate prose-format directives (issue #62
is one of several — Codex wrapped the YAML in a ` ```yaml ` fence
despite the prompt saying not to fence).

This proposal moves structured agent output to a **side-channel
file** written by the agent via its native `Write` tool, at a path
the orchestrator advertises through an env var. The agent's
human-facing comment body remains freeform markdown.

Validated empirically: both Claude Code and Codex reliably write
clean parseable JSON to a side-channel file when prompted; same
Codex that produced the fenced-YAML bug in #62 had no trouble.

## 2. Motivation

`approval.ParseScope` already needs:

- code-fence stripping (added in commit `8b036a8`)
- case-insensitive heading matching
- bounded-by-next-H2 logic
- single-file-fallback heuristics for sloppy plans

Each of these is a workaround for "agent didn't follow the format
spec exactly." The pattern repeats in
`internal/approval/reviewer.go`, which parses
`## Decision\napprove|reject\n## Reasons\n...` from a reviewer agent's
output. Every new structured field added to the orchestrator/agent
contract will need its own parser and its own LLM-disobedience
workarounds.

A side-channel file removes the entire class of failures: the
structured payload lives outside the prose, and `json.Unmarshal` of
a known-shaped file has zero ambiguity.

## 3. Design

### 3.1 Env var contract

For each phase that needs structured output, the orchestrator sets a
phase-specific env var on the agent run, pointing to a path in the
agent's isolated `$HOME` (which symphony-go already sets up
per-job):

| Phase | Env var | Schema |
|---|---|---|
| Planning | `SYMPHONY_PLAN_SCOPE_PATH` | `{files_touched: [string], estimated_lines_added: int, estimated_lines_removed: int, risk_summary: string}` |
| Review | `SYMPHONY_REVIEW_DECISION_PATH` | `{decision: "approve"\|"reject", reasons: [string]}` |
| (Future) Implementation | `SYMPHONY_IMPL_SUMMARY_PATH` | `{actual_files_touched: [string], summary: string}` for cross-checking against `PlanScope` |

The path is always under the per-job `HomePath`, so it's
auto-cleaned with the workspace and never lands in the PR diff.

### 3.2 Prompt change

`planSuffix` becomes:

```
You are in PLANNING phase. Do not edit any source files. Produce a
written plan in markdown — any structure you like.

Additionally, write a JSON file at the path in environment variable
SYMPHONY_PLAN_SCOPE_PATH with this exact shape:

  {
    "files_touched": ["relative/path/one", ...],
    "estimated_lines_added": <int>,
    "estimated_lines_removed": <int>,
    "risk_summary": "<one-line note>"
  }

The orchestrator parses this file to gate auto-approval. If the
file is missing or malformed, the run falls back to gated approval.
```

The "do not fence/bold/paraphrase" gymnastics go away.

`WORKFLOW.md` rule #3 ("do not edit any file under `.symphony-go/`")
stays as-is — the side-channel files live in `$HOME`, not under
`.symphony-go/`.

### 3.3 Parsing

`approval.ParseScope` gains a primary path:

```go
func ParseScopeFromFile(path string) (*types.PlanScope, error) {
    b, err := os.ReadFile(path)
    if err != nil { return nil, err }
    var s types.PlanScope
    if err := json.Unmarshal(b, &s); err != nil { return nil, err }
    if len(s.FilesTouched) == 0 {
        return nil, fmt.Errorf("plan-scope.json missing files_touched")
    }
    return &s, nil
}
```

`ProcessIssue` calls `ParseScopeFromFile` first. On error or missing
file, falls through to the existing `ParseScope(planResult.Text)`
prose parser (back-compat path; can be removed in a later release).

### 3.4 Reviewer extension

Same pattern, opt-in via a follow-up commit:

- Set `SYMPHONY_REVIEW_DECISION_PATH` on review runs.
- Add `ParseReviewerDecisionFromFile`.
- Reviewer agent's free-text output still posts as a comment for
  human auditing; the structured decision drives the gate.

## 4. Migration

- **Phase 1 (this proposal)**: Plan side-channel. Orchestrator sets
  env var, prefers file, falls back to prose parser. Existing
  WORKFLOW.md / older agents keep working.
- **Phase 2 (next release)**: Reviewer side-channel.
- **Phase 3 (eventual)**: Drop prose parsers and the `## Scope`
  contract from `planSuffix`. Single `--legacy-prose-scope` flag for
  users on very old agents.

## 5. Empirical validation

Run on 2026-05-04 against `/tmp/symphony-scope-test` with a
worker.ts-style planning prompt and `SYMPHONY_PLAN_SCOPE_PATH` set:

- **Claude Code 2.1.118**: wrote 2.5KB freeform `plan.md`, valid
  `plan-scope.json` parsed first try.
- **Codex CLI 0.128.0**: wrote freeform `plan.md` with its own
  heading layout, valid `plan-scope.json` parsed first try.

Same Codex version produced the fenced-YAML bug in
print-my-ideas/print-my-ideas#62 — confirming the failure was
specific to "structured-block-within-prose," not structured output
generally.

## 6. Cost

~120 LOC + tests:

- `internal/approval/scope.go`: `ParseScopeFromFile` (~30 LOC)
- `internal/orchestrator/job.go`: env injection at run setup,
  prefer-file-then-prose order in `ProcessIssue` (~20 LOC)
- `internal/runner/*`: env propagation (already set up via
  `BuildAgentEnv`; just one new key)
- Tests: file present / file missing / file malformed / fallback
  path (~50 LOC)

## 7. Open questions

- Should the env var carry a `file://` URL or a plain path?
  Recommendation: plain path; agents have file-write, not URL-write.
- Should we also accept the file written under a conventional
  default (`$HOME/symphony-plan-scope.json`) when the env var
  somehow doesn't propagate? Recommendation: no — env var is the
  contract, fall back to prose parser if missing.
- Could this same channel carry the markdown plan body too, instead
  of using the agent's stdout as the comment body? Recommendation:
  no — stdout-as-comment is convenient and the prose has no parsing
  contract to break.
