package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/types"
)

// reconcileLabels is the set of labels reconcile considers "symphony state
// labels". Any open issue that previously carried one and no longer does
// is row 17 (no symphony label).
type reconcileLabels struct {
	all              map[string]string // label string -> canonical key
	ready            string
	planning         string
	awaitingApproval string
	implementing     string
	prReady          string
	failed           string
	blocked          string
	stop             string
}

func newReconcileLabels(cfg *config.Config) reconcileLabels {
	r := reconcileLabels{
		all:              map[string]string{},
		ready:            cfg.Labels.Ready,
		planning:         cfg.Labels.Planning,
		awaitingApproval: cfg.Labels.AwaitingApproval,
		implementing:     cfg.Labels.Implementing,
		prReady:          cfg.Labels.PRReady,
		failed:           cfg.Labels.Failed,
		blocked:          cfg.Labels.Blocked,
		stop:             cfg.Labels.Stop,
	}
	for _, l := range []string{r.ready, r.planning, r.awaitingApproval, r.implementing, r.prReady, r.failed, r.blocked, r.stop} {
		r.all[strings.ToLower(l)] = l
	}
	return r
}

// pickStateLabel returns the single symphony:* state label among issue's
// labels. Returns "" when none present, or the lowercased label string
// when one is present (multiple is undefined; first wins).
func (r reconcileLabels) pickStateLabel(issueLabels []string) string {
	for _, l := range issueLabels {
		lc := strings.ToLower(l)
		if _, ok := r.all[lc]; ok && lc != strings.ToLower(r.stop) {
			return lc
		}
	}
	return ""
}

// Reconcile applies the SPEC §7 19-row table to every (issue, local-state)
// pair and any orphan local jobs.
func (o *Orchestrator) Reconcile(ctx context.Context) error {
	cfg := o.deps.Config
	rl := newReconcileLabels(cfg)
	log := o.deps.Logger

	var processed, transitioned, errCount int

	// Index local jobs by issue number.
	jobs, err := o.deps.State.List()
	if err != nil {
		return fmt.Errorf("reconcile: list jobs: %w", err)
	}
	byIssue := make(map[int]*types.Job, len(jobs))
	for _, j := range jobs {
		byIssue[j.IssueNumber] = j
	}

	// All open issues with at least one symphony:* label require visit.
	// We probe each label in turn: GitHub's API does not have an OR-on-labels
	// search; in MVP we union ListReadyIssues for each tracked label.
	stateIssues := map[int]types.Issue{}
	for _, lbl := range []string{rl.ready, rl.planning, rl.awaitingApproval, rl.implementing, rl.prReady, rl.failed, rl.blocked} {
		issues, lerr := o.deps.GitHub.ListReadyIssues(ctx, lbl)
		if lerr != nil {
			log.Error("reconcile: list issues", "label", lbl, "err", lerr)
			errCount++
			continue
		}
		for _, iss := range issues {
			stateIssues[iss.Number] = iss
		}
	}

	// Walk every (issue, local) pair we know about.
	seen := map[int]struct{}{}
	for n, iss := range stateIssues {
		seen[n] = struct{}{}
		processed++
		moved, e := o.reconcileOne(ctx, iss, byIssue[n], rl)
		if e != nil {
			errCount++
			log.Error("reconcile: row error", "issue", n, "err", e)
		}
		if moved {
			transitioned++
		}
	}
	// Local jobs whose issues we did not see (issue closed, or label
	// dropped). Verify via GetIssue.
	for n, j := range byIssue {
		if _, ok := seen[n]; ok {
			continue
		}
		processed++
		iss, gerr := o.deps.GitHub.GetIssue(ctx, n)
		if gerr != nil {
			log.Warn("reconcile: cannot fetch issue for orphan local job", "issue", n, "err", gerr)
			errCount++
			continue
		}
		moved, e := o.reconcileOne(ctx, iss, j, rl)
		if e != nil {
			errCount++
			log.Error("reconcile: row error", "issue", n, "err", e)
		}
		if moved {
			transitioned++
		}
	}

	log.Info("reconcile", "rows_processed", processed, "transitioned", transitioned, "errors", errCount)
	return nil
}

// reconcileOne dispatches one (issue, local) pair to the matching row of
// SPEC §7. Returns moved=true when this row caused a state transition.
func (o *Orchestrator) reconcileOne(ctx context.Context, issue types.Issue, job *types.Job, rl reconcileLabels) (bool, error) {
	cfg := o.deps.Config
	stateLabel := rl.pickStateLabel(issue.Labels)
	open := strings.EqualFold(issue.State, "open") || issue.State == ""

	// Rows 18 & 19: closed issue.
	if !open {
		if job == nil {
			return false, nil
		}
		// Mark complete (= leave terminal as is, or set blocked for non-terminal).
		// SPEC: "mark local complete, kill any running job, leave workspace and labels"
		// We treat "complete" as keeping the file but no further action.
		return false, nil
	}

	// Row 17: no symphony:* state label on the issue.
	if stateLabel == "" {
		if job == nil {
			return false, nil
		}
		if job.Status != types.StatusBlocked {
			job.Status = types.StatusBlocked
			_ = o.saveJob(job)
			return true, nil
		}
		return false, nil
	}

	lc := stateLabel

	if job == nil {
		// Rows 1-6: no local state.
		switch lc {
		case strings.ToLower(rl.ready):
			// Row 1: dispatch loop will pick this up.
			return false, nil
		case strings.ToLower(rl.planning):
			return o.reconcileOrphan(ctx, issue, lc, "orphan planning label, no local state", rl)
		case strings.ToLower(rl.awaitingApproval):
			return o.reconcileOrphan(ctx, issue, lc, "orphan awaiting-approval label, no local state", rl)
		case strings.ToLower(rl.implementing):
			return o.reconcileOrphan(ctx, issue, lc,
				"orphan implementing label; no local state, workspace not preserved", rl)
		case strings.ToLower(rl.prReady), strings.ToLower(rl.failed), strings.ToLower(rl.blocked):
			// Rows 5, 6: leave alone.
			return false, nil
		}
		return false, nil
	}

	// job != nil. Branch on local status.
	switch job.Status {
	case types.StatusPlanning:
		// Row 7: planning was interrupted. Replace label `planning → ready`
		// and drop local state so the dispatch loop re-claims the issue
		// and runs planning fresh on the next tick. Reconciliation never
		// starts an agent itself.
		if lc == strings.ToLower(rl.planning) {
			return o.reconcileRetryPlanning(ctx, issue, job)
		}
		// Row 8: label drift.
		return o.driftBlock(ctx, issue, job, lc, "local=planning")

	case types.StatusAwaitingApproval:
		switch lc {
		case strings.ToLower(rl.awaitingApproval):
			// Row 9: poller will resume on next tick.
			return false, nil
		case strings.ToLower(rl.implementing):
			// Row 10.
			if job.ApprovalCommentID != 0 || (job.ReviewerDecision != nil && job.ReviewerDecision.Decision == "approve") {
				// Caller (PollApprovals or routeAuto) already approved; leave label.
				return false, nil
			}
			return o.driftBlock(ctx, issue, job, lc, "implementing label without approval")
		default:
			return o.driftBlock(ctx, issue, job, lc, "local=awaiting_approval")
		}

	case types.StatusImplementing:
		switch lc {
		case strings.ToLower(rl.implementing):
			// Row 12: do NOT auto-resume. Mark blocked.
			body := fmt.Sprintf("interrupted mid-implementation; workspace preserved at `%s`; "+
				"remove this label and add `%s` to retry from scratch", job.RepoPath, cfg.Labels.Ready)
			return o.driftBlock(ctx, issue, job, lc, body)
		case strings.ToLower(rl.prReady):
			// Row 13: PR may have been created before state save.
			prs, err := o.deps.GitHub.FindPRsByHead(ctx, job.Branch)
			if err != nil {
				return false, fmt.Errorf("find PRs: %w", err)
			}
			if len(prs) == 1 {
				job.PRNumber = prs[0].Number
				job.Status = types.StatusPRReady
				return true, o.saveJob(job)
			}
			return o.driftBlock(ctx, issue, job, lc, fmt.Sprintf("expected exactly one PR for %s, found %d", job.Branch, len(prs)))
		case strings.ToLower(rl.failed):
			// Row 14: leave failed.
			job.Status = types.StatusFailed
			return true, o.saveJob(job)
		default:
			return o.driftBlock(ctx, issue, job, lc, "local=implementing")
		}

	case types.StatusPRReady:
		// Rows 15, 16.
		return false, nil

	case types.StatusFailed, types.StatusBlocked:
		return false, nil
	}
	return false, errors.New("reconcile: no row matched (bug)")
}

// reconcileOrphan handles rows 2-4 (no local state, mid-active label).
func (o *Orchestrator) reconcileOrphan(ctx context.Context, issue types.Issue, currentLabel, reason string, rl reconcileLabels) (bool, error) {
	cfg := o.deps.Config
	if err := o.deps.GitHub.ReplaceStateLabel(ctx, issue.Number,
		[]string{currentLabel}, []string{cfg.Labels.Blocked}); err != nil {
		return false, err
	}
	body := capReconcileComment("[symphony-go reconcile] " + reason)
	if _, err := o.deps.GitHub.PostIssueComment(ctx, issue.Number, body); err != nil {
		o.deps.Logger.Warn("reconcile: post comment", "issue", issue.Number, "err", err)
	}
	o.deps.Logger.Info("reconcile_action", "issue", issue.Number, "action", "orphan_block", "reason", reason)
	return true, nil
}

// driftBlock implements the generic "mark blocked locally + replace github
// label + comment" drift path used by rows 8, 10, 11, 12.
func (o *Orchestrator) driftBlock(ctx context.Context, issue types.Issue, job *types.Job, currentLabel, reason string) (bool, error) {
	cfg := o.deps.Config
	if err := o.deps.GitHub.ReplaceStateLabel(ctx, issue.Number,
		[]string{currentLabel}, []string{cfg.Labels.Blocked}); err != nil {
		return false, err
	}
	body := capReconcileComment(fmt.Sprintf("[symphony-go reconcile] label drift: %s, github=`%s`", reason, currentLabel))
	if _, err := o.deps.GitHub.PostIssueComment(ctx, issue.Number, body); err != nil {
		o.deps.Logger.Warn("reconcile: post comment", "issue", issue.Number, "err", err)
	}
	job.Status = types.StatusBlocked
	if err := o.saveJob(job); err != nil {
		return false, err
	}
	o.deps.Logger.Info("reconcile_action", "issue", issue.Number, "action", "drift_block", "reason", reason)
	return true, nil
}

// capReconcileComment caps a comment body at 1000 chars per SPEC §7.
func capReconcileComment(s string) string {
	const max = 1000
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// reconcileRetryPlanning implements SPEC §7 row 7. Planning was
// interrupted (process died after claim, before posting a plan, or after
// posting a plan but before transitioning to awaiting/implementing).
// Reset the GitHub label `planning → ready`, drop the local job state,
// and post a brief comment. The dispatch loop reclaims the `ready`
// label on the next tick and runs planning fresh. Reconciliation never
// starts an agent itself, satisfying both "re-run planning from scratch"
// (SPEC §7) and "Reconciliation never starts an agent" (SPEC §7 rules).
//
// The previous plan comment, if any, is left in place. We do not edit it
// because the github.Client interface deliberately omits comment-edit
// (M7 follow-up); a fresh planning run will post a new plan comment that
// supersedes it visually.
func (o *Orchestrator) reconcileRetryPlanning(ctx context.Context, issue types.Issue, job *types.Job) (bool, error) {
	cfg := o.deps.Config
	if err := o.deps.GitHub.ReplaceStateLabel(ctx, issue.Number,
		[]string{cfg.Labels.Planning}, []string{cfg.Labels.Ready}); err != nil {
		return false, err
	}
	body := capReconcileComment("[symphony-go reconcile] planning was interrupted; resetting label to ready and retrying.")
	if _, err := o.deps.GitHub.PostIssueComment(ctx, issue.Number, body); err != nil {
		o.deps.Logger.Warn("reconcile: post comment", "issue", issue.Number, "err", err)
	}
	if err := o.deps.State.Delete(job.IssueNumber); err != nil {
		return false, fmt.Errorf("delete state: %w", err)
	}
	o.deps.Logger.Info("reconcile_action", "issue", issue.Number, "action", "retry_planning")
	return true, nil
}

// (this trailing helper here just to keep imports honest if removed elsewhere)
var _ = os.Stat
