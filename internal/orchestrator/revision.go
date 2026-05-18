package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/github"
	"github.com/logosc/symphony-go/internal/types"
)

// revisionSuffixTmpl is appended to the rendered WORKFLOW.md prompt when
// the orchestrator drives a single revision cycle in response to a
// CHANGES_REQUESTED PR review. Format args (in order): issue title,
// issue description, approved plan text, review feedback body.
const revisionSuffixTmpl = `

---

You are in REVISION phase. A reviewer has requested changes on your pull request.

Original issue:
Title: %s
Description: %s

Your approved plan:
%s

Review feedback requesting changes:
%s

Address the reviewer's feedback by making the necessary code changes. Focus only on what the reviewer asked for.
`

// PollPRRevisions services pr_ready jobs whose PRs have received a
// CHANGES_REQUESTED review since the job was finalized. Each job is
// revised at most once; Job.RevisionAttempted guards a second pass. The
// job stays in pr_ready regardless of revision outcome.
func (o *Orchestrator) PollPRRevisions(ctx context.Context) error {
	jobs, err := o.deps.State.List()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Status != types.StatusPRReady {
			continue
		}
		if job.RevisionAttempted {
			continue
		}
		if job.PRNumber == 0 {
			continue
		}
		if err := o.servicePRRevision(ctx, job); err != nil {
			o.deps.Logger.Error("revision: service", "issue", job.IssueNumber, "err", err)
		}
	}
	return nil
}

// servicePRRevision looks for a CHANGES_REQUESTED review on job.PRNumber
// submitted after job.UpdatedAt, and if one is found, runs a single
// revision cycle under a claim.
func (o *Orchestrator) servicePRRevision(ctx context.Context, job *types.Job) error {
	reviews, err := o.deps.GitHub.ListPRReviews(ctx, job.PRNumber)
	if err != nil {
		return err
	}
	since := job.UpdatedAt
	var match *github.PRReview
	for i := range reviews {
		r := reviews[i]
		if !strings.EqualFold(r.State, "CHANGES_REQUESTED") {
			continue
		}
		if !since.IsZero() && !r.SubmittedAt.After(since) {
			continue
		}
		if o.isIgnoredApprovalUser(r.User) {
			continue
		}
		match = &r
		break
	}
	if match == nil {
		return nil
	}
	if !o.tryClaim(job.IssueNumber) {
		return nil
	}
	defer o.release(job.IssueNumber)
	return o.runPRRevision(ctx, job, *match)
}

// runPRRevision executes the revision cycle: render a revision prompt,
// drive the implementation agent on the existing worktree, commit any
// uncommitted changes, push the branch (which updates the existing PR in
// place), and post a status comment on the PR. Always sets
// RevisionAttempted=true and re-saves the job so the cycle never repeats.
// The job's status (pr_ready) and label are intentionally left alone.
func (o *Orchestrator) runPRRevision(ctx context.Context, job *types.Job, review github.PRReview) error {
	cfg := o.deps.Config
	log := o.deps.Logger.With("issue", job.IssueNumber, "pr", job.PRNumber)

	finish := func(comment string) error {
		if comment != "" {
			_, _ = o.deps.GitHub.PostIssueComment(ctx, job.PRNumber, comment)
		}
		job.RevisionAttempted = true
		return o.saveJob(job)
	}

	issue, err := o.deps.GitHub.GetIssue(ctx, job.IssueNumber)
	if err != nil {
		return finish(fmt.Sprintf("[symphony-go] revision: get issue failed: %v", err))
	}

	layout := o.layoutForJob(job)
	if _, statErr := os.Stat(layout.RepoPath); statErr != nil {
		log.Warn("revision_worktree_missing", "path", layout.RepoPath, "err", statErr)
		return finish("[symphony-go] revision skipped: worktree no longer on disk.")
	}

	rendered, rerr := config.RenderPrompt(o.promptBodyFor(job.AxisKey), issue, job.Attempt)
	if rerr != nil {
		return finish(fmt.Sprintf("[symphony-go] revision: render prompt failed: %v", rerr))
	}
	revPrompt := rendered + fmt.Sprintf(revisionSuffixTmpl,
		issue.Title, issue.Description, job.PlanText, review.Body)

	headBefore, _ := gitRevParseHead(ctx, layout.RepoPath)

	log.Info("revision_started", "reviewer", review.User, "review_id", review.ID)
	revReq := types.RunRequest{
		Issue:    issue,
		RepoPath: layout.RepoPath,
		HomePath: layout.HomePath,
		Prompt:   revPrompt,
		Phase:    types.PhaseImplementation,
		Timeout:  time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
		AxisKey:  job.AxisKey,
	}
	revResult, turns, runErr := o.runImplementationAgent(ctx, log, job, revReq)
	log.Info("revision_completed", "success", revResult.Success, "turns", turns, "err", runErr)

	if runErr != nil {
		return finish(fmt.Sprintf("[symphony-go] revision agent error; PR unchanged.\n\n%s",
			truncate(summarizeRunFailure(revResult), 1500)))
	}
	if !revResult.Success {
		return finish(fmt.Sprintf("[symphony-go] revision agent reported failure; PR unchanged.\n\n%s",
			truncate(summarizeRunFailure(revResult), 1500)))
	}

	statusOut, sErr := gitStatusPorcelain(ctx, layout.RepoPath)
	if sErr != nil {
		return finish(fmt.Sprintf("[symphony-go] revision: git status failed: %v", sErr))
	}
	hasUncommitted := strings.TrimSpace(statusOut) != ""
	headAfter, _ := gitRevParseHead(ctx, layout.RepoPath)
	agentCommitted := headBefore != "" && headAfter != "" && headBefore != headAfter
	if !hasUncommitted && !agentCommitted {
		return finish("[symphony-go] revision agent produced no diff; PR unchanged.")
	}

	if hasUncommitted {
		msg := fmt.Sprintf("Address review feedback on PR #%d", job.PRNumber)
		if err := gitCommitAll(ctx, layout.RepoPath, msg); err != nil {
			return finish(fmt.Sprintf("[symphony-go] revision: commit failed: %v", err))
		}
	}

	pushToken, tokErr := o.resolveGitHubToken(ctx)
	if tokErr != nil {
		return finish(fmt.Sprintf("[symphony-go] revision: resolve token failed: %v", tokErr))
	}
	if err := o.deps.PushFunc(ctx, layout.RepoPath, job.Branch, pushToken); err != nil {
		return finish(fmt.Sprintf("[symphony-go] revision: push failed: %v", err))
	}

	log.Info("revision_pushed")
	return finish("[symphony-go] pushed revision addressing review feedback")
}

// gitRevParseHead returns the current HEAD SHA at repoPath. Returns the
// empty string and the underlying error on failure (callers can treat an
// empty result as "unknown" for diff-detection purposes).
func gitRevParseHead(ctx context.Context, repoPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "HEAD")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}
