package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/logosc/symphony-go/internal/approval"
	"github.com/logosc/symphony-go/internal/config"
	internalexec "github.com/logosc/symphony-go/internal/exec"
	"github.com/logosc/symphony-go/internal/github"
	"github.com/logosc/symphony-go/internal/types"
	"github.com/logosc/symphony-go/internal/workspace"
)

// planSuffix is appended to the rendered WORKFLOW.md prompt for the
// planning phase. Tells the agent the `## Scope` block is mandatory and
// non-negotiable per SPEC §2 plan-output-contract.
const planSuffix = `

---

You are in PLANNING phase. Do not edit any files. Produce a written plan
ending with the following EXACT block — the orchestrator parses it and
will reject your plan otherwise:

## Scope
files_touched:
  - relative/path/one
  - relative/path/two
estimated_lines_added: <int>
estimated_lines_removed: <int>
risk_summary: <one-line note>

If you fail to emit this block, the run aborts. Be conservative: list every
file you will modify.`

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
	o.running[issue.Number] = struct{}{}
	defer delete(o.running, issue.Number)

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

	job := &types.Job{
		IssueNumber:   issue.Number,
		Repo:          cfg.Repo.FullName,
		Status:        types.StatusPlanning,
		WorkspaceRoot: layout.Root,
		RepoPath:      layout.RepoPath,
		Branch:        branch,
		Attempt:       1,
		UpdatedAt:     o.deps.NowFunc(),
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
		if err := o.deps.WorkspaceMgr.Create(ctx, layout, cfg.Repo.BaseBranch, branch); err != nil {
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
	rendered, err := config.RenderPrompt(o.deps.PromptTemplate, issue, job.Attempt)
	if err != nil {
		return o.markBlocked(ctx, job, fmt.Sprintf("render prompt: %v", err))
	}
	planPrompt := rendered + planSuffix

	log.Info("planning_started")
	planResult, err := o.deps.AgentRunner.Run(ctx, types.RunRequest{
		Issue:    issue,
		RepoPath: layout.RepoPath,
		HomePath: layout.HomePath,
		Prompt:   planPrompt,
		Phase:    types.PhasePlanning,
		Timeout:  time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
	})
	log.Info("planning_completed", "success", planResult.Success, "err", err)
	// 6. after_run (logged, status unchanged).
	if cfg.Hooks.AfterRun != "" {
		_ = o.runHook(ctx, "after_run.planning", cfg.Hooks.AfterRun, layout, env)
	}
	if err != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("planning agent: %v", err))
	}
	if !planResult.Success {
		return o.markFailed(ctx, job, "planning agent reported failure")
	}

	// 7. Post plan + parse scope.
	planComment, perr := o.deps.GitHub.PostIssueComment(ctx, issue.Number, planResult.Text)
	if perr != nil {
		log.Error("plan comment post failed", "err", perr)
	} else {
		job.PlanCommentID = planComment.ID
	}
	job.PlanText = planResult.Text
	scope, scopeErr := approval.ParseScope(planResult.Text)
	if scopeErr != nil {
		log.Warn("scope_parsed: error", "err", scopeErr)
	} else if scope != nil {
		log.Info("scope_parsed", "files", len(scope.FilesTouched))
		job.PlanScope = scope
	} else {
		log.Info("scope_parsed: missing")
	}
	if err := o.saveJob(job); err != nil {
		return err
	}

	// 8. Approval routing.
	mode := types.ApprovalMode(cfg.Approval.Mode)
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

	rendered, err := config.RenderPrompt(o.deps.PromptTemplate, issue, job.Attempt)
	if err != nil {
		return o.markBlocked(ctx, job, fmt.Sprintf("render impl prompt: %v", err))
	}
	implPrompt := rendered + fmt.Sprintf(implSuffixTmpl, job.PlanText)

	log.Info("implementation_started")
	implResult, ierr := o.deps.AgentRunner.Run(ctx, types.RunRequest{
		Issue:    issue,
		RepoPath: layout.RepoPath,
		HomePath: layout.HomePath,
		Prompt:   implPrompt,
		Phase:    types.PhaseImplementation,
		Timeout:  time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
	})
	log.Info("implementation_completed", "success", implResult.Success, "err", ierr)

	if cfg.Hooks.AfterRun != "" {
		_ = o.runHook(ctx, "after_run.implementation", cfg.Hooks.AfterRun, layout, env)
	}
	if ierr != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("implementation agent: %v", ierr))
	}
	if !implResult.Success {
		return o.markFailed(ctx, job, "implementation agent reported failure")
	}

	// 11. git status --porcelain.
	statusOut, err := gitStatusPorcelain(ctx, layout.RepoPath)
	if err != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("git status: %v", err))
	}
	if strings.TrimSpace(statusOut) == "" {
		_, _ = o.deps.GitHub.PostIssueComment(ctx, issue.Number,
			"[symphony-go] implementation produced no diff; marking blocked.")
		return o.markBlocked(ctx, job, "no changes produced")
	}

	// 12. Diff verification (auto only).
	if cfg.Approval.Mode == "auto" && cfg.Auto.VerifyDiffMatchesPlan && job.PlanScope != nil {
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
	for _, cmdStr := range cfg.Validation.Commands {
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
	if err := gitCommitAll(ctx, layout.RepoPath, fmt.Sprintf("Implement issue #%d", issue.Number)); err != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("git commit: %v", err))
	}
	log.Info("committed")

	// 15. Push.
	if err := o.deps.PushFunc(ctx, layout.RepoPath, job.Branch, o.deps.GitHubToken); err != nil {
		return o.markFailed(ctx, job, fmt.Sprintf("git push: %v", err))
	}
	log.Info("pushed")

	// 16. Create draft PR.
	workflowEdited := planTouchesWorkflow(statusOut, cfg.Repo.WorkflowFile)
	prBody := buildPRBody(job, results, workflowEdited)
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
// and the workflow-edited warning if applicable.
func buildPRBody(job *types.Job, results []valResult, workflowEdited bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Resolves #%d.\n\n", job.IssueNumber)
	fmt.Fprintf(&b, "Approval path: `%s`\n\n", job.ApprovalPath)
	if job.PlanText != "" {
		b.WriteString("## Plan\n\n")
		b.WriteString(job.PlanText)
		b.WriteString("\n\n")
	}
	b.WriteString("## Validation\n\n")
	b.WriteString("| command | exit |\n|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| `%s` | %d |\n", r.Cmd, r.ExitCode)
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

var _ = errors.New // silence unused import if ever
