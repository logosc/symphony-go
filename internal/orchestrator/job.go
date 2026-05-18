package orchestrator

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/logosc/symphony-go/internal/approval"
	"github.com/logosc/symphony-go/internal/config"
	internalexec "github.com/logosc/symphony-go/internal/exec"
	"github.com/logosc/symphony-go/internal/github"
	"github.com/logosc/symphony-go/internal/runner"
	"github.com/logosc/symphony-go/internal/types"
	"github.com/logosc/symphony-go/internal/workspace"
)

// multiTurnContinuePrompt is the short guidance sent on every turn after
// the first in a multi-turn implementation session. Agents that consider
// the work complete must end with the marker `## Done` (case-insensitive,
// on its own line); the orchestrator stops driving turns when it sees it.
const multiTurnContinuePrompt = `Continue with the next step of your plan. When the implementation is
complete and tests pass, write the line ` + "`## Done`" + ` on its own and stop.`

// doneMarker is the literal sentinel emitted by the agent on its own line
// to signal end-of-multi-turn. Compared case-insensitively after trimming
// whitespace.
const doneMarker = "## Done"

// planSuffix is appended to the rendered WORKFLOW.md prompt for the
// planning phase. Asks the agent to (1) write a structured JSON file
// at $SYMPHONY_PLAN_SCOPE_PATH for the orchestrator to parse and (2)
// produce a freeform markdown plan for humans. Falls back to the
// in-prose ## Scope YAML contract if the file is missing/malformed
// (back-compat for older agents). See proposal 0004.
const planSuffix = `

---

You are in PLANNING phase. Do not edit any source files in the
repository. Produce a written plan in markdown — any structure you like
(headings, tables, bullets, prose).

Before you finish, you MUST write two side-channel files using your
file-write tool:

1. A JSON scope file at the absolute path in SYMPHONY_PLAN_SCOPE_PATH.
2. A human-readable Markdown plan at the absolute path in
   SYMPHONY_PLAN_COMMENT_PATH.

These side-channel files are required for approval routing and issue
comments. They are not source-code edits and they must not be placed
inside the repository.

SYMPHONY_PLAN_SCOPE_PATH must contain only valid JSON with this exact
shape:

  {
    "files_touched": ["relative/path/one", "relative/path/two"],
    "estimated_lines_added": <int>,
    "estimated_lines_removed": <int>,
    "risk_summary": "<one-line note>"
  }

The orchestrator parses this file to gate auto-approval. Be conservative
in files_touched — list every file you will modify, including generated
assets and rebuilt bundles. Do not mention that you wrote the side-channel
file unless there is a problem.

SYMPHONY_PLAN_COMMENT_PATH must contain only the polished, user-facing
Markdown plan. Do not include tool progress, status narration, or lines
like "now let me". Write it in English.

Only if your file-write tool is unavailable or the file write fails, fall
back to ending your markdown with this EXACT raw YAML block (heading on
its own line, no fences, no bold, no bullets):

## Scope
files_touched:
  - relative/path/one
estimated_lines_added: <int>
estimated_lines_removed: <int>
risk_summary: <one-line note>
`

// implSuffix is appended to the rendered WORKFLOW.md prompt for the
// implementation phase, with the approved plan quoted in.
const implSuffixTmpl = `

---

You are in IMPLEMENTATION phase. The following plan was approved — execute
it precisely. Do not touch files outside the listed scope; the orchestrator
will block the PR if your diff drifts beyond the configured tolerance.

Approved plan:

%s
`

// ProcessIssue drives one issue through the per-issue flow up to the next
// handoff state (awaiting_approval in gated mode, or pr_ready / blocked /
// failed terminal state otherwise).
func (o *Orchestrator) ProcessIssue(ctx context.Context, issue types.Issue) error {
	if !o.tryClaim(issue.Number) {
		return nil
	}
	defer o.release(issue.Number)

	log := o.deps.Logger.With("issue", issue.Number)
	cfg := o.deps.Config

	// 1. Claim: ready -> planning.
	slug := workspace.SanitizeSlug(issue.Title)
	branch := workspace.BranchName(issue.Number, slug)
	layout := workspace.LayoutFor(o.deps.WorkspaceRoot, issue.Number, slug)

	if err := o.deps.GitHub.ReplaceStateLabel(ctx, issue.Number,
		[]string{cfg.Labels.Ready}, []string{cfg.Labels.Planning}); err != nil {
		log.Error("claim: relabel failed", "err", err)
		return err
	}
	log.Info("claim", "branch", branch)

	axisKey, axisSrc := o.resolveAxis(issue)
	job := &types.Job{
		IssueNumber:   issue.Number,
		Repo:          cfg.Repo.FullName,
		Status:        types.StatusPlanning,
		WorkspaceRoot: layout.Root,
		RepoPath:      layout.RepoPath,
		Branch:        branch,
		Attempt:       1,
		UpdatedAt:     o.deps.NowFunc(),
		AxisKey:       axisKey,
		AxisSource:    axisSrc,
	}
	if err := o.saveJob(job); err != nil {
		return err
	}

	// 2. Worktree.
	worktreeNew := false
	if _, err := os.Stat(layout.RepoPath); err != nil {
		if !os.IsNotExist(err) {
			return o.markBlocked(ctx, job, fmt.Sprintf("worktree stat: %v", err))
		}
		// Serialize concurrent worktree creates against the shared local
		// repo: `git worktree add -b` writes branch upstream config that
		// races on `.git/config.lock`. See deps.go (worktreeCreateMu).
		o.worktreeCreateMu.Lock()
		err := o.deps.WorkspaceMgr.Create(ctx, layout, cfg.Repo.BaseBranch, branch)
		o.worktreeCreateMu.Unlock()
		if err != nil {
			log.Error("worktree create failed", "err", err)
			return o.markBlocked(ctx, job, fmt.Sprintf("worktree create: %v", err))
		}
		worktreeNew = true
		log.Info("worktree_created", "path", layout.RepoPath)
	}

	// Symlink subscription-mode auth (~/.claude, ~/.codex, etc.) into the
	// agent's isolated HOME so claude/codex can read existing auth state.
	// Idempotent; missing source paths are silently skipped. API-key mode
	// is unaffected (handled by BuildAgentEnv allowlist instead).
	if err := seedSubscriptionAuth(layout.HomePath); err != nil {
		log.Warn("seed_subscription_auth", "err", err)
	}

	env := internalexec.BuildAgentEnv(cfg.Env.Allowlist, cfg.Env.BlockPatterns, os.Environ(), layout.HomePath)

	// 3. after_create hook (only when newly created).
	if worktreeNew && cfg.Hooks.AfterCreate != "" {
		if err := o.runHook(ctx, "after_create", cfg.Hooks.AfterCreate, layout, env); err != nil {
			return o.markBlocked(ctx, job, fmt.Sprintf("after_create hook: %v", err))
		}
	}

	// 4. before_run (planning).
	if cfg.Hooks.BeforeRun != "" {
		if err := o.runHook(ctx, "before_run.planning", cfg.Hooks.BeforeRun, layout, env); err != nil {
			return o.markBlocked(ctx, job, fmt.Sprintf("before_run hook (planning): %v", err))
		}
	}

	// 5. Render planning prompt + run agent.
	rendered, err := config.RenderPrompt(o.promptBodyFor(job.AxisKey), issue, job.Attempt)
	if err != nil {
		return o.markBlocked(ctx, job, fmt.Sprintf("render prompt: %v", err))
	}
	planPrompt := rendered + planSuffix

	repoArtifactDir := filepath.Join(os.TempDir(), "symphony-go", workspace.SanitizeSlug(cfg.Repo.FullName))
	planScopePath := filepath.Join(repoArtifactDir,
		fmt.Sprintf("issue-%d-attempt-%d-plan-scope.json", issue.Number, job.Attempt))
	planCommentPath := filepath.Join(repoArtifactDir,
		fmt.Sprintf("issue-%d-attempt-%d-plan.md", issue.Number, job.Attempt))
	if err := os.MkdirAll(filepath.Dir(planScopePath), 0o700); err != nil {
		return o.markBlocked(ctx, job, fmt.Sprintf("prepare plan artifact path: %v", err))
	}
	_ = os.Remove(planScopePath) // ensure we don't read a stale file from a prior attempt
	_ = os.Remove(planCommentPath)

	log.Info("planning_started", "axis_key", job.AxisKey, "axis_source", job.AxisSource)
	planResult, err := o.runnerForJob(job).Run(ctx, types.RunRequest{
		Issue:    issue,
		RepoPath: layout.RepoPath,
		HomePath: layout.HomePath,
		Prompt:   planPrompt,
		Phase:    types.PhasePlanning,
		Timeout:  time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
		AxisKey:  job.AxisKey,
		ExtraEnv: []string{
			"SYMPHONY_PLAN_SCOPE_PATH=" + planScopePath,
			"SYMPHONY_PLAN_COMMENT_PATH=" + planCommentPath,
		},
	})
	log.Info("planning_completed", "success", planResult.Success, "err", err)
	// 6. after_run (logged, status unchanged).
	if cfg.Hooks.AfterRun != "" {
		_ = o.runHook(ctx, "after_run.planning", cfg.Hooks.AfterRun, layout, env)
	}
	if err != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("planning agent: %v\n\n%s", err, summarizeRunFailure(planResult)))
	}
	if !planResult.Success {
		diag := summarizeRunFailure(planResult)
		log.Warn("planning_failed", "diag", diag)
		return o.markFailed(ctx, job, "planning agent reported failure\n\n"+diag)
	}

	// 6b. Post-planning diff guard (proposal 0005 §4.6). Planning must
	// not edit source files — WORKFLOW.md forbids it. But the runner's
	// permission mode may technically allow writes (e.g. claude
	// --permission-mode acceptEdits, needed for the proposal-0004 side-
	// channel JSON; codex workspace-write sandbox). Side-channel files
	// live under HomePath, not RepoPath, so a clean repo after planning
	// is the right invariant. Fail loudly if the agent edited source.
	if statusOut, statusErr := gitStatusPorcelain(ctx, layout.RepoPath); statusErr == nil && strings.TrimSpace(statusOut) != "" {
		log.Warn("planning_edited_source", "status_lines", strings.Count(strings.TrimSpace(statusOut), "\n")+1)
		return o.markFailed(ctx, job, fmt.Sprintf(
			"planning agent edited source files (forbidden by WORKFLOW.md). "+
				"`git status --porcelain` after planning:\n```\n%s```",
			statusOut))
	}

	// 7. Post plan + parse scope.
	planBody := planResult.Text
	if fileBody, ferr := readSideChannelText(planCommentPath); ferr == nil {
		planBody = fileBody
		log.Info("plan_comment_loaded", "source", "file", "bytes", len(fileBody))
	} else if !os.IsNotExist(ferr) {
		log.Warn("plan_comment_file_read_failed", "err", ferr)
	}
	if cfg.Approval.RequireToken {
		token, terr := newApprovalToken()
		if terr != nil {
			return o.markFailed(ctx, job, fmt.Sprintf("approval token: %v", terr))
		}
		job.ApprovalToken = token
		planBody = appendApprovalToken(planBody, token)
	}
	if strings.TrimSpace(planBody) == "" {
		log.Warn("plan_empty")
	} else {
		planComment, perr := o.deps.GitHub.PostIssueComment(ctx, issue.Number, planBody)
		if perr != nil {
			log.Error("plan comment post failed", "err", perr)
		} else {
			job.PlanCommentID = planComment.ID
		}
	}
	job.PlanText = planBody
	// Prefer the side-channel JSON file (proposal 0004); fall back to the
	// in-prose ## Scope YAML block when the agent didn't write the file or
	// wrote it malformed.
	var scope *types.PlanScope
	if s, ferr := approval.ParseScopeFromFile(planScopePath); ferr == nil {
		scope = s
		log.Info("scope_parsed", "source", "file", "files", len(s.FilesTouched))
	} else if !os.IsNotExist(ferr) {
		log.Warn("scope_parsed: file malformed, falling back to prose", "err", ferr)
	}
	if scope == nil {
		s, scopeErr := approval.ParseScope(planResult.Text)
		switch {
		case scopeErr != nil:
			log.Warn("scope_parsed: error", "source", "prose", "err", scopeErr)
		case s != nil:
			scope = s
			log.Info("scope_parsed", "source", "prose", "files", len(s.FilesTouched))
		default:
			log.Info("scope_parsed: missing")
		}
	}
	if scope != nil {
		job.PlanScope = scope
	}
	if err := o.saveJob(job); err != nil {
		return err
	}

	// 8. Approval routing.
	mode := types.ApprovalMode(o.resolveApprovalMode(job, issue.Labels))
	switch mode {
	case types.ApprovalHandoff:
		job.ApprovalPath = types.ApprovalPathHandoff
		log.Info("auto_approved", "path", "handoff")
	case types.ApprovalGated:
		return o.transitionToAwaiting(ctx, job)
	case types.ApprovalAuto:
		// Missing/malformed scope falls through to gated.
		if scope == nil {
			_, _ = o.deps.GitHub.PostIssueComment(ctx, issue.Number,
				"[symphony-go] plan missing or malformed `## Scope` block; falling back to gated approval.")
			return o.transitionToAwaiting(ctx, job)
		}
		return o.routeAuto(ctx, job, issue, *scope)
	default:
		return o.markBlocked(ctx, job, fmt.Sprintf("unknown approval mode %q", cfg.Approval.Mode))
	}

	return o.runImplementation(ctx, job, issue, layout, env)
}

// routeAuto handles the auto approval path: rules → optional reviewer →
// implementation or fallback.
func (o *Orchestrator) routeAuto(ctx context.Context, job *types.Job, issue types.Issue, scope types.PlanScope) error {
	cfg := o.deps.Config
	log := o.deps.Logger.With("issue", issue.Number)

	match := approval.Evaluate(cfg.Auto.Rules, issue.Labels, scope)
	if match.Index < 0 {
		log.Info("rule_matched: none", "fallback", cfg.Auto.FallbackOnNoRuleMatch)
		return o.applyFallback(ctx, job, issue, cfg.Auto.FallbackOnNoRuleMatch,
			"no auto.rule matched this issue+scope")
	}
	log.Info("rule_matched", "index", match.Index, "reviewer_required", match.ReviewerRequired)

	if !match.ReviewerRequired {
		job.ApprovalPath = types.ApprovalPathRules
		log.Info("auto_approved", "path", "rules")
		layout := workspace.LayoutFor(o.deps.WorkspaceRoot, issue.Number, workspace.SanitizeSlug(issue.Title))
		env := internalexec.BuildAgentEnv(cfg.Env.Allowlist, cfg.Env.BlockPatterns, os.Environ(), layout.HomePath)
		return o.runImplementation(ctx, job, issue, layout, env)
	}

	if o.deps.Reviewer == nil {
		return o.markBlocked(ctx, job, "auto.rules require reviewer but Deps.Reviewer is nil")
	}
	reviewerHome := filepath.Join(o.deps.WorkspaceRoot,
		fmt.Sprintf("issue-%d-%s", issue.Number, workspace.SanitizeSlug(issue.Title)), "home")
	if err := os.MkdirAll(reviewerHome, 0o755); err != nil {
		return o.markBlocked(ctx, job, fmt.Sprintf("mkdir reviewer home: %v", err))
	}

	log.Info("reviewer_started")
	dec, rerr := o.deps.Reviewer.Review(ctx, approval.ReviewInput{
		Issue:    issue,
		PlanText: job.PlanText,
		RepoPath: cfg.Repo.LocalPath,
		HomePath: reviewerHome,
	})
	if rerr != nil {
		log.Error("reviewer error", "err", rerr)
		return o.markFailed(ctx, job, fmt.Sprintf("reviewer: %v", rerr))
	}
	job.ReviewerDecision = &dec
	log.Info("reviewer_completed", "decision", dec.Decision)

	if dec.Decision == "approve" {
		job.ApprovalPath = types.ApprovalPathReviewer
		_ = o.saveJob(job)
		layout := workspace.LayoutFor(o.deps.WorkspaceRoot, issue.Number, workspace.SanitizeSlug(issue.Title))
		env := internalexec.BuildAgentEnv(cfg.Env.Allowlist, cfg.Env.BlockPatterns, os.Environ(), layout.HomePath)
		return o.runImplementation(ctx, job, issue, layout, env)
	}

	reasons := strings.Join(dec.Reasons, "; ")
	body := fmt.Sprintf("[symphony-go] reviewer rejected the plan: %s", reasons)
	_, _ = o.deps.GitHub.PostIssueComment(ctx, issue.Number, body)
	return o.applyFallback(ctx, job, issue, cfg.Auto.FallbackOnReject, "reviewer rejected: "+reasons)
}

// applyFallback applies fallback_on_* (gated|block).
func (o *Orchestrator) applyFallback(ctx context.Context, job *types.Job, issue types.Issue, mode, reason string) error {
	switch mode {
	case "gated":
		return o.transitionToAwaiting(ctx, job)
	case "block":
		return o.markBlocked(ctx, job, reason)
	default:
		return o.markBlocked(ctx, job, fmt.Sprintf("unknown fallback %q (reason: %s)", mode, reason))
	}
}

// transitionToAwaiting moves the job to awaiting_approval (label + state).
func (o *Orchestrator) transitionToAwaiting(ctx context.Context, job *types.Job) error {
	cfg := o.deps.Config
	if err := o.deps.GitHub.ReplaceStateLabel(ctx, job.IssueNumber,
		[]string{cfg.Labels.Planning}, []string{cfg.Labels.AwaitingApproval}); err != nil {
		return err
	}
	job.Status = types.StatusAwaitingApproval
	o.deps.Logger.Info("awaiting_approval", "issue", job.IssueNumber)
	return o.saveJob(job)
}

// runImplementation runs steps 9-17 of the per-issue flow: relabel to
// implementing, run the agent, verify diff, validate, commit, push, PR.
func (o *Orchestrator) runImplementation(ctx context.Context, job *types.Job, issue types.Issue, layout workspace.Layout, env []string) error {
	cfg := o.deps.Config
	log := o.deps.Logger.With("issue", issue.Number)

	// 9. Replace label → implementing.
	prevLabel := cfg.Labels.Planning
	if job.Status == types.StatusAwaitingApproval {
		prevLabel = cfg.Labels.AwaitingApproval
	}
	if err := o.deps.GitHub.ReplaceStateLabel(ctx, issue.Number,
		[]string{prevLabel}, []string{cfg.Labels.Implementing}); err != nil {
		return err
	}
	job.Status = types.StatusImplementing
	if err := o.saveJob(job); err != nil {
		return err
	}

	// 10. before_run + implementation phase + after_run.
	if cfg.Hooks.BeforeRun != "" {
		if err := o.runHook(ctx, "before_run.implementation", cfg.Hooks.BeforeRun, layout, env); err != nil {
			return o.markBlocked(ctx, job, fmt.Sprintf("before_run hook (impl): %v", err))
		}
	}

	rendered, err := config.RenderPrompt(o.promptBodyFor(job.AxisKey), issue, job.Attempt)
	if err != nil {
		return o.markBlocked(ctx, job, fmt.Sprintf("render impl prompt: %v", err))
	}
	implPrompt := rendered + fmt.Sprintf(implSuffixTmpl, job.PlanText)

	log.Info("implementation_started", "axis_key", job.AxisKey)
	implReq := types.RunRequest{
		Issue:    issue,
		RepoPath: layout.RepoPath,
		HomePath: layout.HomePath,
		Prompt:   implPrompt,
		Phase:    types.PhaseImplementation,
		Timeout:  time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
		AxisKey:  job.AxisKey,
	}
	implResult, turnsUsed, ierr := o.runImplementationAgent(ctx, log, job, implReq)
	log.Info("implementation_completed", "success", implResult.Success, "turns", turnsUsed, "err", ierr)

	if cfg.Hooks.AfterRun != "" {
		_ = o.runHook(ctx, "after_run.implementation", cfg.Hooks.AfterRun, layout, env)
	}
	if ierr != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("implementation agent: %v\n\n%s", ierr, summarizeRunFailure(implResult)))
	}
	if !implResult.Success {
		diag := summarizeRunFailure(implResult)
		log.Warn("implementation_failed", "diag", diag)
		if o.resolveApprovalMode(job, issue.Labels) != "handoff" {
			return o.markFailed(ctx, job, "implementation agent reported failure\n\n"+diag)
		}
		statusOut, statusErr := gitStatusPorcelain(ctx, layout.RepoPath)
		if statusErr != nil {
			return o.markFailed(ctx, job, fmt.Sprintf("git status after failed handoff implementation: %v", statusErr))
		}
		if strings.TrimSpace(statusOut) == "" {
			return o.markFailed(ctx, job, "implementation agent reported failure\n\n"+diag)
		}
		_, _ = o.deps.GitHub.PostIssueComment(ctx, issue.Number,
			"[symphony-go] handoff implementation reported failure but produced a diff; continuing to PR for human review.")
		log.Warn("handoff_failed_with_diff_continuing")
	}

	// 11. git status --porcelain. Some runners may commit their own
	// work; in that case the working tree is clean but the branch still
	// has a reviewable diff against the base branch.
	statusOut, err := gitStatusPorcelain(ctx, layout.RepoPath)
	if err != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("git status: %v", err))
	}
	committedByAgent := false
	if strings.TrimSpace(statusOut) == "" {
		branchDiffOut, derr := gitDiffNameOnly(ctx, layout.RepoPath, cfg.Repo.BaseBranch)
		if derr != nil {
			return o.markFailed(ctx, job, fmt.Sprintf("git diff against base: %v", derr))
		}
		if strings.TrimSpace(branchDiffOut) == "" {
			_, _ = o.deps.GitHub.PostIssueComment(ctx, issue.Number,
				"[symphony-go] implementation produced no diff; marking blocked.")
			return o.markBlocked(ctx, job, "no changes produced")
		}
		statusOut = branchDiffOut
		committedByAgent = true
		log.Info("using_committed_branch_diff", "base", cfg.Repo.BaseBranch)
	}

	// 12. Diff verification (auto only). Use the resolved per-axis mode so
	// per-axis overrides honor the same gate as planning-time routing.
	if o.resolveApprovalMode(job, issue.Labels) == "auto" && cfg.Auto.VerifyDiffMatchesPlan && job.PlanScope != nil {
		drift := approval.VerifyDiff(statusOut, *job.PlanScope, cfg.Auto.MaxDiffDriftFiles)
		if drift.Drifted {
			body := fmt.Sprintf("[symphony-go] diff drift detected (%d extra files exceed max %d):\n- %s",
				len(drift.ExtraFiles), drift.AllowedDrift, strings.Join(drift.ExtraFiles, "\n- "))
			_, _ = o.deps.GitHub.PostIssueComment(ctx, issue.Number, body)
			log.Warn("diff_drift_detected", "extra", drift.ExtraFiles)
			return o.markBlocked(ctx, job, "diff drift exceeds tolerance")
		}
		log.Info("diff_verified", "extra_within_tolerance", len(drift.ExtraFiles))
	}

	// 13. Validation.
	var results []valResult
	validationCmds, vErr := o.resolveValidationCommands(job)
	if vErr != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("validation resolve: %v", vErr))
	}
	for _, cmdStr := range validationCmds {
		log.Info("validation_command", "cmd", cmdStr)
		res, vErr := internalexec.Run(ctx, cmdStr, internalexec.RunOptions{
			Cwd:     layout.RepoPath,
			Env:     env,
			Timeout: time.Duration(cfg.Validation.CommandTimeoutSeconds) * time.Second,
		})
		if vErr != nil {
			return o.markFailed(ctx, job, fmt.Sprintf("validation: %v", vErr))
		}
		stdout := internalexec.Redact(res.Stdout, cfg.Audit.RedactPatterns)
		stderr := internalexec.Redact(res.Stderr, cfg.Audit.RedactPatterns)
		results = append(results, valResult{Cmd: cmdStr, ExitCode: res.ExitCode, Stdout: stdout, Stderr: stderr})
		if res.ExitCode != 0 || res.TimedOut {
			summary := fmt.Sprintf("[symphony-go] validation failed: %q exit=%d\n```\n%s\n%s\n```",
				cmdStr, res.ExitCode, truncate(stdout, 1000), truncate(stderr, 1000))
			_, _ = o.deps.GitHub.PostIssueComment(ctx, issue.Number, summary)
			return o.markFailed(ctx, job, fmt.Sprintf("validation %q exit=%d", cmdStr, res.ExitCode))
		}
	}
	log.Info("validation_completed", "n", len(results))

	// 14. Commit.
	if !committedByAgent {
		if err := gitCommitAll(ctx, layout.RepoPath, fmt.Sprintf("Implement issue #%d", issue.Number)); err != nil {
			return o.markFailed(ctx, job, fmt.Sprintf("git commit: %v", err))
		}
		log.Info("committed")
	} else {
		log.Info("commit_skipped", "reason", "agent already committed branch diff")
	}

	// 15. Push.
	pushToken, tokErr := o.resolveGitHubToken(ctx)
	if tokErr != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("resolve github token for push: %v", tokErr))
	}
	if err := o.deps.PushFunc(ctx, layout.RepoPath, job.Branch, pushToken); err != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("git push: %v", err))
	}
	log.Info("pushed")

	// 16. Create draft PR.
	workflowEdited := planTouchesWorkflow(statusOut, cfg.Repo.WorkflowFile)
	proofArtifacts := collectProofArtifacts(statusOut, issue.Number)
	prBody := buildPRBody(job, results, workflowEdited, proofArtifacts)
	prTitle := truncatePRTitle(issue.Title)
	pr, perr := o.deps.GitHub.CreateDraftPR(ctx, github.CreatePRRequest{
		Title: prTitle,
		Body:  prBody,
		Head:  job.Branch,
		Base:  cfg.Repo.BaseBranch,
		Draft: true,
	})
	if perr != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("create PR: %v", perr))
	}
	job.PRNumber = pr.Number
	log.Info("pr_created", "number", pr.Number, "url", pr.URL)

	// 17. PR-link comment + label.
	_, _ = o.deps.GitHub.PostIssueComment(ctx, issue.Number, fmt.Sprintf("[symphony-go] opened PR #%d: %s", pr.Number, pr.URL))
	if err := o.deps.GitHub.ReplaceStateLabel(ctx, issue.Number,
		[]string{cfg.Labels.Implementing}, []string{cfg.Labels.PRReady}); err != nil {
		return err
	}
	job.Status = types.StatusPRReady
	return o.saveJob(job)
}

// runImplementationAgent drives the implementation phase. When multi-turn
// is enabled in config AND the runner supports MultiTurnRunner, it opens
// a session and drives up to cfg.Agent.MaxTurns turns, stopping on the
// first of: agent emits the `## Done` marker on its own line; a turn
// returns Success=false; or the cap is reached. Otherwise it falls back
// to a single AgentRunner.Run call.
//
// Returns the final RunResult (the one whose Success is reported to the
// orchestrator), the number of turns executed (1 in single-turn mode),
// and any Go-level error from the runner.
func (o *Orchestrator) runImplementationAgent(ctx context.Context, log interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}, job *types.Job, req types.RunRequest) (types.RunResult, int, error) {
	cfg := o.deps.Config
	r := o.runnerForJob(job)

	if cfg.Agent.MultiTurn {
		if mt, ok := r.(runner.MultiTurnRunner); ok {
			res, n, err := o.runMultiTurnImpl(ctx, log, mt, req)
			if !errors.Is(err, runner.ErrMultiTurnUnsupported) {
				return res, n, err
			}
			log.Info("multi_turn_unsupported_fallback")
		}
	}
	res, err := r.Run(ctx, req)
	return res, 1, err
}

// runnerForJob returns the per-axis AgentRunner when configured, falling
// back to the global Deps.AgentRunner.
func (o *Orchestrator) runnerForJob(job *types.Job) runner.AgentRunner {
	if len(o.deps.AgentRunnersByAxis) == 0 {
		return o.deps.AgentRunner
	}
	if job != nil {
		if r, ok := o.deps.AgentRunnersByAxis[job.AxisKey]; ok {
			return r
		}
	}
	if r, ok := o.deps.AgentRunnersByAxis["default"]; ok {
		return r
	}
	return o.deps.AgentRunner
}

// runMultiTurnImpl opens a Session against the multi-turn runner and
// loops as described in runImplementationAgent's doc comment. Each turn's
// raw events are concatenated into a single RunResult.Events buffer.
func (o *Orchestrator) runMultiTurnImpl(ctx context.Context, log interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}, mt runner.MultiTurnRunner, req types.RunRequest) (types.RunResult, int, error) {
	cfg := o.deps.Config
	maxTurns := cfg.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 8
	}

	sess, err := mt.OpenSession(ctx, req)
	if err != nil {
		return types.RunResult{}, 0, err
	}
	defer func() { _ = sess.Close() }()

	var (
		last       types.RunResult
		eventsBuf  bytes.Buffer
		stderrAcc  bytes.Buffer
		turns      int
		startedAll = time.Now()
	)
	prompt := req.Prompt
	for i := 0; i < maxTurns; i++ {
		log.Info("multi_turn", "turn", i+1, "max", maxTurns)
		res, terr := sess.Turn(ctx, prompt)
		turns++

		if len(res.Events) > 0 {
			eventsBuf.Write(res.Events)
			eventsBuf.WriteByte('\n')
		}
		if res.Stderr != "" {
			stderrAcc.WriteString(res.Stderr)
			stderrAcc.WriteByte('\n')
		}

		// Aggregate Text — keep the latest as the "answer" while still
		// growing across turns for audit. The final res.Text is what the
		// caller cares about; we keep it as-is.
		last = res
		if terr != nil {
			last.Events = append([]byte(nil), eventsBuf.Bytes()...)
			last.Stderr = stderrAcc.String()
			if last.StartedAt.IsZero() {
				last.StartedAt = startedAll
			}
			return last, turns, terr
		}
		if !res.Success {
			break
		}
		if hasDoneMarker(res.Text) {
			log.Info("multi_turn_done_marker")
			break
		}
		prompt = multiTurnContinuePrompt
	}

	last.Events = append([]byte(nil), eventsBuf.Bytes()...)
	if stderrAcc.Len() > 0 {
		last.Stderr = stderrAcc.String()
	}
	if last.StartedAt.IsZero() {
		last.StartedAt = startedAll
	}
	if last.CompletedAt.IsZero() {
		last.CompletedAt = time.Now()
	}
	return last, turns, nil
}

// hasDoneMarker reports whether s contains the literal `## Done` line on
// its own (case-insensitive after trimming surrounding whitespace).
func hasDoneMarker(s string) bool {
	want := strings.ToLower(doneMarker)
	for _, line := range strings.Split(s, "\n") {
		if strings.ToLower(strings.TrimSpace(line)) == want {
			return true
		}
	}
	return false
}

// runHook executes one hook script and returns a non-nil error iff the
// hook must be treated as a failure (exit != 0 or setup error).
func (o *Orchestrator) runHook(ctx context.Context, name, script string, layout workspace.Layout, env []string) error {
	cfg := o.deps.Config
	o.deps.Logger.Info("hook_started", "name", name)
	res, err := workspace.RunHook(ctx, name, script, layout.RepoPath, layout.HomePath, env,
		time.Duration(cfg.Hooks.TimeoutSeconds)*time.Second)
	o.deps.Logger.Info("hook_completed",
		"name", name,
		"exit", res.ExitCode,
		"timed_out", res.TimedOut,
		"stdout", internalexec.Redact(res.Stdout, cfg.Audit.RedactPatterns),
		"stderr", internalexec.Redact(res.Stderr, cfg.Audit.RedactPatterns),
	)
	if err != nil {
		return err
	}
	if res.TimedOut {
		return fmt.Errorf("hook %q timed out", name)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("hook %q exit=%d", name, res.ExitCode)
	}
	return nil
}

// markBlocked transitions the job to blocked, posts a comment, and saves.
func (o *Orchestrator) markBlocked(ctx context.Context, job *types.Job, reason string) error {
	cfg := o.deps.Config
	prev := labelForStatus(cfg, job.Status)
	if err := o.deps.GitHub.ReplaceStateLabel(ctx, job.IssueNumber,
		[]string{prev}, []string{cfg.Labels.Blocked}); err != nil {
		o.deps.Logger.Error("blocked: relabel", "err", err)
	}
	_, _ = o.deps.GitHub.PostIssueComment(ctx, job.IssueNumber,
		"[symphony-go] blocked: "+reason)
	job.Status = types.StatusBlocked
	o.deps.Logger.Warn("blocked", "issue", job.IssueNumber, "reason", reason)
	return o.saveJob(job)
}

// summarizeRunFailure returns a short, redacted diagnostic snippet from
// a failed RunResult, suitable for slog and a GitHub comment. Prefers
// agent Text (Claude Code emits API errors here as "API Error: 400 ..."
// in the final result event), falls back to Stderr, then a placeholder.
// Truncates from the end since errors typically appear last.
func summarizeRunFailure(r types.RunResult) string {
	const maxBytes = 1500
	pick := strings.TrimSpace(r.Text)
	if pick == "" {
		pick = strings.TrimSpace(r.Stderr)
	}
	if pick == "" {
		return "(agent produced no output; check tmux log for stderr)"
	}
	if len(pick) > maxBytes {
		pick = "...(truncated)\n" + pick[len(pick)-maxBytes:]
	}
	return pick
}

// markFailed transitions the job to failed, posts a comment, and saves.
func (o *Orchestrator) markFailed(ctx context.Context, job *types.Job, reason string) error {
	cfg := o.deps.Config
	prev := labelForStatus(cfg, job.Status)
	if err := o.deps.GitHub.ReplaceStateLabel(ctx, job.IssueNumber,
		[]string{prev}, []string{cfg.Labels.Failed}); err != nil {
		o.deps.Logger.Error("failed: relabel", "err", err)
	}
	_, _ = o.deps.GitHub.PostIssueComment(ctx, job.IssueNumber,
		"[symphony-go] failed: "+reason)
	job.Status = types.StatusFailed
	o.deps.Logger.Warn("failed", "issue", job.IssueNumber, "reason", reason)
	return o.saveJob(job)
}

// newApprovalToken returns a 4-digit token (1000-9999) for the
// require-token approval gate. Crypto/rand based so an attacker who has
// read access cannot predict the next token from the previous one.
func newApprovalToken() (string, error) {
	const lo, hi = 1000, 9999
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(hi-lo+1)))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", lo+int(n.Int64())), nil
}

// appendApprovalToken appends a `## Approval` footer to the plan body
// instructing the operator to comment back the given token.
func appendApprovalToken(planBody, token string) string {
	footer := fmt.Sprintf(`

---

## Approval

To approve this plan, comment exactly the number `+"`%s`"+` on this issue
from a write-permission account. The orchestrator will not advance to
implementation until it sees that comment from a writer.

The token is a one-time read-the-plan check: it changes every time
planning re-runs (e.g., after a crash + reconcile), so an old approval
cannot accidentally promote a new plan.
`, token)
	return planBody + footer
}

func readSideChannelText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// resolveGitHubToken returns a fresh GitHub access token suitable for
// `git push` over HTTPS. Prefers Deps.GitHubTokenFn (App-installation
// auth, rotates hourly via ghinstallation) when non-nil; falls back to
// the static Deps.GitHubToken (PAT auth) otherwise. Returns an empty
// string and nil error if neither is set — the push will fail at git
// level with a clearer error than panicking here.
func (o *Orchestrator) resolveGitHubToken(ctx context.Context) (string, error) {
	if o.deps.GitHubTokenFn != nil {
		return o.deps.GitHubTokenFn(ctx)
	}
	return o.deps.GitHubToken, nil
}

// saveJob updates Job.UpdatedAt and persists.
func (o *Orchestrator) saveJob(j *types.Job) error {
	j.UpdatedAt = o.deps.NowFunc()
	return o.deps.State.Save(j)
}

// labelForStatus returns the GitHub label corresponding to a JobStatus.
// For terminal statuses (failed, blocked, pr_ready), returns the matching
// terminal label. For unknown statuses, returns the empty string.
func labelForStatus(cfg *config.Config, s types.JobStatus) string {
	switch s {
	case types.StatusPlanning:
		return cfg.Labels.Planning
	case types.StatusAwaitingApproval:
		return cfg.Labels.AwaitingApproval
	case types.StatusImplementing:
		return cfg.Labels.Implementing
	case types.StatusPRReady:
		return cfg.Labels.PRReady
	case types.StatusBlocked:
		return cfg.Labels.Blocked
	case types.StatusFailed:
		return cfg.Labels.Failed
	}
	return ""
}

// gitStatusPorcelain runs `git -C repoPath status --porcelain`.
func gitStatusPorcelain(ctx context.Context, repoPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "status", "--porcelain")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, errb.String())
	}
	return out.String(), nil
}

// gitDiffNameOnly returns files changed by HEAD compared with origin/base.
func gitDiffNameOnly(ctx context.Context, repoPath, baseBranch string) (string, error) {
	baseRef := "origin/" + baseBranch
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "diff", "--name-only", baseRef+"...HEAD")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, errb.String())
	}
	return out.String(), nil
}

// gitCommitAll stages all changes and commits with the given message.
func gitCommitAll(ctx context.Context, repoPath, message string) error {
	if err := runGit(ctx, repoPath, "add", "-A"); err != nil {
		return err
	}
	return runGit(ctx, repoPath,
		"-c", "user.name=symphony-go",
		"-c", "user.email=noreply@local",
		"commit", "-m", message)
}

func runGit(ctx context.Context, repoPath string, args ...string) error {
	full := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

// DefaultPushFunc pushes branch to origin via `git -c http.extraheader=...
// push origin <branch>`. The token is passed via -c so it is never written
// to disk and the command env does not need to inherit GITHUB_TOKEN.
func DefaultPushFunc(ctx context.Context, repoPath, branch, token string) error {
	args := []string{"-C", repoPath}
	if token != "" {
		args = append(args, "-c", "http.extraheader=AUTHORIZATION: bearer "+token)
	}
	args = append(args, "push", "origin", branch)
	cmd := exec.CommandContext(ctx, "git", args...)
	// Do NOT inherit env (especially GITHUB_TOKEN) — the credential is in args.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: %w: %s", err, stderr.String())
	}
	return nil
}

// valResult is a small per-command record used for building the PR body.
type valResult struct {
	Cmd      string
	ExitCode int
	Stdout   string
	Stderr   string
}

// buildPRBody composes the PR body from job state, validation results,
// proof artifacts, and the workflow-edited warning if applicable.
func buildPRBody(job *types.Job, results []valResult, workflowEdited bool, proofArtifacts []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Resolves #%d.\n\n", job.IssueNumber)
	fmt.Fprintf(&b, "Approval path: `%s`\n\n", job.ApprovalPath)
	if job.PlanText != "" {
		planText := strings.TrimSpace(job.PlanText)
		// The agent may already include a "## Plan" heading in its output.
		// Only add our own if the text doesn't already start with one.
		if !strings.HasPrefix(planText, "## Plan") {
			b.WriteString("## Plan\n\n")
		}
		b.WriteString(planText)
		b.WriteString("\n\n")
	}
	b.WriteString("## Validation\n\n")
	b.WriteString("| command | exit |\n|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| `%s` | %d |\n", r.Cmd, r.ExitCode)
	}
	if len(proofArtifacts) > 0 {
		b.WriteString("\n## Proof Artifacts\n\n")
		for _, p := range proofArtifacts {
			name := path.Base(p)
			if isEmbeddableProofImage(p) {
				fmt.Fprintf(&b, "![%s](%s)\n\n", name, p)
				continue
			}
			fmt.Fprintf(&b, "- [%s](%s)\n", p, p)
		}
	}
	if workflowEdited {
		b.WriteString("\n## Warnings\n\n")
		b.WriteString("[symphony-go] agent modified WORKFLOW.md; review carefully before merge.\n")
	}
	out := b.String()
	if len(out) > 60000 {
		out = out[:60000]
	}
	return out
}

// collectProofArtifacts returns changed proof assets under docs/proof/<issue>/.
func collectProofArtifacts(porcelain string, issueNumber int) []string {
	prefix := fmt.Sprintf("docs/proof/%d/", issueNumber)
	var out []string
	seen := map[string]struct{}{}
	for _, p := range approval.FilesFromGitStatus(porcelain) {
		if !strings.HasPrefix(p, prefix) || strings.HasSuffix(p, "/") {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func isEmbeddableProofImage(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	default:
		return false
	}
}

// planTouchesWorkflow reports whether the porcelain output mentions the
// workflow file path (relative to repo root).
func planTouchesWorkflow(porcelain, workflowFile string) bool {
	for _, p := range approval.FilesFromGitStatus(porcelain) {
		if p == workflowFile {
			return true
		}
	}
	return false
}

// truncatePRTitle returns "[agent] <title>" truncated to 70 runes total.
func truncatePRTitle(title string) string {
	prefix := "[agent] "
	full := prefix + title
	const max = 70
	if utf8RuneLen(full) <= max {
		return full
	}
	// truncate to max runes.
	out := []rune(full)
	if len(out) > max {
		out = out[:max]
	}
	return string(out)
}

func utf8RuneLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// layoutForJob reconstructs the per-issue Layout from the persisted Job
// (used after a restart or when servicing approvals where the slug came
// from the issue title at planning time).
func (o *Orchestrator) layoutForJob(job *types.Job) workspace.Layout {
	// We trust Job.RepoPath / WorkspaceRoot rather than re-deriving the slug
	// from a possibly-renamed issue title.
	return workspace.Layout{
		Root:     job.WorkspaceRoot,
		RepoPath: job.RepoPath,
		HomePath: filepath.Join(job.WorkspaceRoot, "home"),
		TmpPath:  filepath.Join(job.WorkspaceRoot, "home", "tmp"),
	}
}

// envForJob builds the agent env using Layout.HomePath.
func (o *Orchestrator) envForJob(layout workspace.Layout) []string {
	return internalexec.BuildAgentEnv(o.deps.Config.Env.Allowlist, o.deps.Config.Env.BlockPatterns, os.Environ(), layout.HomePath)
}

// resolveAxis freezes the axis identity for a new Job. The canonical
// mapping is cfg.Repo.WorkflowFiles (the workflow-file map): if any
// per-axis map is configured anywhere, the orchestrator resolves
// against this canonical map so every per-axis knob (validation, tools,
// approval) keys off the same axis. When no per-axis map is configured,
// the job carries ("default", "scalar"). On a no-match-no-default
// situation, also fall back to ("default", "scalar") — Validate already
// rejects maps without a "default" key, so this only happens for
// non-canonical maps configured by Agent Y in later gaps.
func (o *Orchestrator) resolveAxis(issue types.Issue) (string, string) {
	cfg := o.deps.Config
	if !o.anyPerAxisMapSet() {
		return "default", "scalar"
	}
	if cfg.Repo.WorkflowFiles.IsEmpty() {
		// A non-canonical per-axis map exists but the workflow map does
		// not. Treat as scalar identity for now; future gaps may pick a
		// different canonical anchor.
		return "default", "scalar"
	}
	key, _, err := config.ResolveAxis(issue, cfg.Repo.WorkflowFiles)
	if err != nil {
		return "default", "by_label"
	}
	return key, "by_label"
}

// anyPerAxisMapSet reports whether any `*_by_label` configuration map is
// non-empty. Used to decide whether the orchestrator is operating in
// per-axis mode at all.
func (o *Orchestrator) anyPerAxisMapSet() bool {
	cfg := o.deps.Config
	if !cfg.Repo.WorkflowFiles.IsEmpty() {
		return true
	}
	if !cfg.Validation.CommandsByLabel.IsEmpty() {
		return true
	}
	if !cfg.Approval.ModeByLabel.IsEmpty() {
		return true
	}
	if !cfg.Claude.PlanningToolsByLabel.IsEmpty() ||
		!cfg.Claude.ImplementationToolsByLabel.IsEmpty() ||
		!cfg.Claude.ReviewToolsByLabel.IsEmpty() ||
		!cfg.Claude.DisallowedToolsByLabel.IsEmpty() {
		return true
	}
	if !cfg.Codex.PlanningArgsByLabel.IsEmpty() ||
		!cfg.Codex.ImplementationArgsByLabel.IsEmpty() ||
		!cfg.Codex.ReviewArgsByLabel.IsEmpty() {
		return true
	}
	return false
}

// resolveApprovalMode returns the effective approval mode for a job.
// When mode_by_label is configured, concrete issue labels win in
// configured order. If no concrete label matches, the job's frozen
// AxisKey is used for back-compat with per-axis configs, then "default".
func (o *Orchestrator) resolveApprovalMode(job *types.Job, issueLabels []string) string {
	cfg := o.deps.Config
	if cfg.Approval.ModeByLabel.IsEmpty() {
		return cfg.Approval.Mode
	}
	issueLabelSet := make(map[string]struct{}, len(issueLabels))
	for _, label := range issueLabels {
		issueLabelSet[strings.ToLower(label)] = struct{}{}
	}
	for _, key := range cfg.Approval.ModeByLabel.Keys {
		if key == "default" {
			continue
		}
		if _, ok := issueLabelSet[strings.ToLower(key)]; ok {
			return cfg.Approval.ModeByLabel.Values[key]
		}
	}
	axis := job.AxisKey
	if axis != "" {
		if v, ok := cfg.Approval.ModeByLabel.Values[axis]; ok {
			return v
		}
	}
	if v, ok := cfg.Approval.ModeByLabel.Values["default"]; ok {
		return v
	}
	return cfg.Approval.Mode
}

// promptBodyFor returns the WORKFLOW.md prompt body to render for a job
// with the given frozen axis key. Falls back to the scalar template when
// no per-axis body exists for the key. AxisKey == "" (legacy job) also
// falls back to scalar.
func (o *Orchestrator) promptBodyFor(axisKey string) string {
	if axisKey != "" && o.deps.PromptTemplates != nil {
		if body, ok := o.deps.PromptTemplates[axisKey]; ok && body != "" {
			return body
		}
	}
	return o.deps.PromptTemplate
}

// resolveValidationCommands returns the validation command list for a
// job, honoring the frozen Job.AxisKey. When CommandsByLabel is empty,
// returns cfg.Validation.Commands (legacy behavior).
func (o *Orchestrator) resolveValidationCommands(job *types.Job) ([]string, error) {
	cfg := o.deps.Config
	if cfg.Validation.CommandsByLabel.IsEmpty() {
		return cfg.Validation.Commands, nil
	}
	axis := job.AxisKey
	if axis == "" {
		axis = "default"
	}
	if v, ok := cfg.Validation.CommandsByLabel.Values[axis]; ok {
		return v, nil
	}
	if v, ok := cfg.Validation.CommandsByLabel.Values["default"]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("validation.commands_by_label has no entry for %q and no default", axis)
}

var _ = errors.New // silence unused import if ever
