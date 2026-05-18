package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/logosc/symphony-go/internal/types"
)

// drainTimeout caps how long Run waits for in-flight jobs to finish
// after ctx is cancelled before returning anyway.
const drainTimeout = 30 * time.Second

// Run starts the orchestrator's poll loop. The loop runs reconcile once
// up front, then on each tick:
//
//  1. handle stop labels
//  2. PollApprovals (gated mode)
//  3. dispatch up to Config.Orchestrator.MaxConcurrentJobs ready issues
//
// When once is true, Run executes a single tick (after reconcile), waits
// for any goroutines it spawned to finish, and returns. ctx cancellation
// triggers a graceful drain (bounded by drainTimeout) before Run returns
// ctx.Err().
func (o *Orchestrator) Run(ctx context.Context, once bool) error {
	if err := o.Reconcile(ctx); err != nil {
		return err
	}
	tick := func() {
		if err := o.handleStopLabels(ctx); err != nil {
			o.deps.Logger.Error("tick: handleStopLabels", "err", err)
		}
		if err := o.PollApprovals(ctx); err != nil {
			o.deps.Logger.Error("tick: PollApprovals", "err", err)
		}
		if err := o.PollPRRevisions(ctx); err != nil {
			o.deps.Logger.Error("tick: PollPRRevisions", "err", err)
		}
		max := o.deps.Config.Orchestrator.MaxConcurrentJobs
		if max < 1 {
			max = 1
		}
		slots := max - o.inflightCount()
		if slots <= 0 {
			return
		}
		issues, err := o.deps.GitHub.ListReadyIssues(ctx, o.deps.Config.Labels.Ready)
		if err != nil {
			o.deps.Logger.Error("tick: ListReadyIssues", "err", err)
			return
		}
		for _, iss := range issues {
			if slots <= 0 {
				break
			}
			// Pre-filter against the in-flight set so we don't fan out
			// duplicate goroutines for the same issue. ProcessIssue
			// re-acquires the claim atomically, so this is just an
			// optimisation; release is idempotent.
			o.runningMu.Lock()
			_, busy := o.running[iss.Number]
			o.runningMu.Unlock()
			if busy {
				continue
			}
			slots--
			o.inflightWG.Add(1)
			go func(iss types.Issue) {
				defer o.inflightWG.Done()
				if err := o.ProcessIssue(ctx, iss); err != nil {
					o.deps.Logger.Error("tick: ProcessIssue", "issue", iss.Number, "err", err)
				}
			}(iss)
		}
	}
	if once {
		tick()
		o.inflightWG.Wait()
		return nil
	}
	interval := time.Duration(o.deps.Config.GitHub.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			o.drainInflight()
			return ctx.Err()
		case <-ticker.C:
			tick()
		}
	}
}

// drainInflight waits for in-flight goroutines spawned by the dispatch
// loop to finish, bounded by drainTimeout. Logs a warning and returns if
// the deadline elapses.
func (o *Orchestrator) drainInflight() {
	done := make(chan struct{})
	go func() {
		o.inflightWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
		o.deps.Logger.Warn("drain: timed out waiting for in-flight jobs", "timeout", drainTimeout)
	}
}

// PollApprovals services awaiting-approval jobs. For each, list new
// comments since Job.UpdatedAt, look for the configured approve command,
// verify commenter permissions, and on match advance into implementation.
func (o *Orchestrator) PollApprovals(ctx context.Context) error {
	cfg := o.deps.Config
	jobs, err := o.deps.State.List()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Status != types.StatusAwaitingApproval {
			continue
		}
		if err := o.servicePendingApproval(ctx, job); err != nil {
			o.deps.Logger.Error("approval: service", "issue", job.IssueNumber, "err", err)
		}
		_ = cfg // keep cfg in scope for future use
	}
	return nil
}

// servicePendingApproval polls comments for one awaiting-approval job and
// resumes the implementation on a valid approve command.
func (o *Orchestrator) servicePendingApproval(ctx context.Context, job *types.Job) error {
	cfg := o.deps.Config
	since := job.UpdatedAt
	// Match target is either the per-plan token (when RequireToken is on
	// AND the plan post produced one) or the static slash command. The
	// token mode forces the approver to actually read the plan and
	// rotates with reconcile re-plans so a stale approval cannot promote
	// a fresh plan.
	matchTarget := cfg.Approval.Command
	if cfg.Approval.RequireToken && job.ApprovalToken != "" {
		matchTarget = job.ApprovalToken
	}
	comments, err := o.deps.GitHub.ListIssueComments(ctx, job.IssueNumber, since)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if strings.TrimSpace(c.Body) != matchTarget {
			continue
		}
		if o.isIgnoredApprovalUser(c.User) {
			o.deps.Logger.Info("approval: ignored user", "user", c.User)
			continue
		}
		if !o.isTrustedApprovalUser(c.User) && cfg.Approval.RequireWritePermission {
			perm, err := o.deps.GitHub.GetCollaboratorPermission(ctx, c.User)
			if err != nil {
				o.deps.Logger.Warn("approval: permission lookup", "user", c.User, "err", err)
				continue
			}
			if !canApprove(perm) {
				_ = o.deps.GitHub.AddReaction(ctx, c.ID, "-1")
				o.deps.Logger.Info("approval: rejected (no permission)", "user", c.User, "perm", perm)
				continue
			}
		}
		// Approved.
		job.ApprovalCommentID = c.ID
		job.ApprovalPath = types.ApprovalPathHuman
		o.deps.Logger.Info("approved", "issue", job.IssueNumber, "user", c.User)

		issue, err := o.deps.GitHub.GetIssue(ctx, job.IssueNumber)
		if err != nil {
			return err
		}
		layout := o.layoutForJob(job)
		env := o.envForJob(layout)
		if !o.tryClaim(job.IssueNumber) {
			// Another goroutine is already processing this issue; let
			// that one finish. The next PollApprovals tick will retry.
			return nil
		}
		err = o.runImplementation(ctx, job, issue, layout, env)
		o.release(job.IssueNumber)
		return err
	}
	return nil
}

// isIgnoredApprovalUser reports whether a comment authored by `user`
// must be skipped before any permission check during approval polling.
// Matches case-insensitively against cfg.Approval.IgnoredUsers and (if
// set) Deps.SelfUsername. An empty user is also treated as ignored.
func (o *Orchestrator) isIgnoredApprovalUser(user string) bool {
	u := strings.ToLower(strings.TrimSpace(user))
	if u == "" {
		return true
	}
	if self := strings.ToLower(strings.TrimSpace(o.deps.SelfUsername)); self != "" && self == u {
		return true
	}
	for _, ig := range o.deps.Config.Approval.IgnoredUsers {
		if strings.ToLower(strings.TrimSpace(ig)) == u {
			return true
		}
	}
	return false
}

// isTrustedApprovalUser reports whether a comment authored by `user`
// can approve without a collaborator permission lookup. This is for bridge
// apps that authenticate the human operator before posting the command.
func (o *Orchestrator) isTrustedApprovalUser(user string) bool {
	u := strings.ToLower(strings.TrimSpace(user))
	if u == "" {
		return false
	}
	for _, trusted := range o.deps.Config.Approval.TrustedUsers {
		if strings.ToLower(strings.TrimSpace(trusted)) == u {
			return true
		}
	}
	return false
}

// canApprove reports whether perm is sufficient to issue an approve.
func canApprove(perm string) bool {
	switch strings.ToLower(perm) {
	case "admin", "maintain", "write":
		return true
	}
	return false
}

// handleStopLabels scans local jobs in active states; for any that carry
// the stop label on github, transition them to blocked, post a comment,
// and remove the stop label.
func (o *Orchestrator) handleStopLabels(ctx context.Context) error {
	cfg := o.deps.Config
	jobs, err := o.deps.State.List()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if isTerminal(job.Status) {
			continue
		}
		iss, err := o.deps.GitHub.GetIssue(ctx, job.IssueNumber)
		if err != nil {
			o.deps.Logger.Warn("stop: get issue", "issue", job.IssueNumber, "err", err)
			continue
		}
		if !hasLabel(iss.Labels, cfg.Labels.Stop) {
			continue
		}
		// Replace [active+stop] with [blocked]; computeLabels removes both.
		prev := labelForStatus(cfg, job.Status)
		_ = o.deps.GitHub.ReplaceStateLabel(ctx, job.IssueNumber,
			[]string{prev, cfg.Labels.Stop}, []string{cfg.Labels.Blocked})
		_, _ = o.deps.GitHub.PostIssueComment(ctx, job.IssueNumber,
			"[symphony-go] stop label observed; marking blocked")
		job.Status = types.StatusBlocked
		_ = o.saveJob(job)
		o.deps.Logger.Info("stop", "issue", job.IssueNumber)
	}
	return nil
}

func isTerminal(s types.JobStatus) bool {
	switch s {
	case types.StatusPRReady, types.StatusFailed, types.StatusBlocked:
		return true
	}
	return false
}

func hasLabel(labels []string, want string) bool {
	w := strings.ToLower(want)
	for _, l := range labels {
		if strings.ToLower(l) == w {
			return true
		}
	}
	return false
}
