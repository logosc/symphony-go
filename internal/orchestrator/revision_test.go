package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/logosc/symphony-go/internal/github"
	"github.com/logosc/symphony-go/internal/types"
)

// installPRReadyJob seeds an issue at the pr-ready label and persists a
// matching local job carrying a non-zero PRNumber. The PR number is also
// seeded as a fake "issue" so PostIssueComment(ctx, prNumber, ...) works
// (real GitHub treats PRs as issues for comment purposes; the fake needs
// an explicit seed).
//
// updatedAt sets Job.UpdatedAt; pass time.Time{} to leave it zero (which
// disables the staleness filter on reviews). Returns the seeded job.
func installPRReadyJob(t *testing.T, h *testHarness, issueNum, prNumber int, updatedAt time.Time) *types.Job {
	t.Helper()
	iss := types.Issue{
		Number: issueNum, Title: "feature", Description: "context", State: "open",
		Labels: []string{h.cfg.Labels.PRReady},
	}
	h.gh.SeedIssue(iss, false)
	// Seed the PR number as an issue too so issue-comments API calls
	// targeting the PR number succeed in the fake.
	h.gh.SeedIssue(types.Issue{
		Number: prNumber, Title: "[agent] feature", State: "open",
	}, true)
	job := &types.Job{
		IssueNumber:   issueNum,
		Repo:          h.cfg.Repo.FullName,
		Status:        types.StatusPRReady,
		WorkspaceRoot: t.TempDir(),
		RepoPath:      h.repo,
		Branch:        fmt.Sprintf("symphony/issue-%d-feature", issueNum),
		PRNumber:      prNumber,
		PlanText:      canonicalPlan([]string{"a.txt"}),
		UpdatedAt:     updatedAt,
	}
	if err := h.state.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}
	return job
}

// TestPollPRRevisions_HappyPath runs the full pipeline to pr-ready, then
// posts a CHANGES_REQUESTED review and asserts the revision cycle runs:
// the agent is invoked with the revision prompt, the diff is committed
// and pushed, RevisionAttempted is set, and a confirmation comment is
// posted on the PR.
func TestPollPRRevisions_HappyPath(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "handoff"
	o := h.newOrch(t, "x", false)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{
		Success: true, Text: canonicalPlan([]string{"a.txt"}),
	}
	implWriter(h.runner, []string{"a.txt"})

	iss := h.seedReadyIssue(501, "fix things")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.ProcessIssue(ctx, iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	job, err := h.state.Load(501)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if job.Status != types.StatusPRReady {
		t.Fatalf("expected pr_ready before revision, got %q", job.Status)
	}
	prNumber := job.PRNumber
	if prNumber == 0 {
		t.Fatalf("expected non-zero PRNumber after ProcessIssue")
	}
	// Seed the PR number as an issue in the fake so PostIssueComment on
	// the PR succeeds (the orchestrator targets the PR for revision
	// comments, mirroring real GitHub where PRs are issues).
	h.gh.SeedIssue(types.Issue{
		Number: prNumber, Title: "[agent] fix things", State: "open",
	}, true)

	// Swap OnRun to a revision-aware writer. We assert on req.Prompt to
	// confirm the orchestrator wired in the issue title, plan, and review
	// body. The writer drops a fresh file so we can detect the agent's
	// effect on the worktree.
	var capturedPrompt string
	h.runner.OnRun = func(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
		capturedPrompt = req.Prompt
		if err := os.WriteFile(filepath.Join(req.RepoPath, "fix.txt"),
			[]byte("revision change\n"), 0o644); err != nil {
			return types.RunResult{}, err
		}
		return types.RunResult{Success: true, Text: "done"}, nil
	}
	// Reset call log so we can assert "exactly one runner call during
	// revision" without counting the prior planning + impl invocations.
	h.runner.Reset()

	// Give the review a SubmittedAt that's unambiguously after job.UpdatedAt
	// on coarse-clock systems.
	time.Sleep(5 * time.Millisecond)
	h.gh.SeedPRReview(prNumber, gh.PRReview{
		User:        "alice",
		State:       "CHANGES_REQUESTED",
		Body:        "Please rename the helper to follow our naming convention.",
		SubmittedAt: time.Now().UTC(),
	})

	if err := o.PollPRRevisions(ctx); err != nil {
		t.Fatalf("PollPRRevisions: %v", err)
	}

	// Job: RevisionAttempted=true, status unchanged.
	got, err := h.state.Load(501)
	if err != nil {
		t.Fatalf("load after revision: %v", err)
	}
	if !got.RevisionAttempted {
		t.Errorf("expected RevisionAttempted=true")
	}
	if got.Status != types.StatusPRReady {
		t.Errorf("status = %q, want pr_ready", got.Status)
	}

	// Runner: exactly one revision call.
	calls := h.runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("runner calls during revision = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].Phase != types.PhaseImplementation {
		t.Errorf("revision call phase = %q, want implementation", calls[0].Phase)
	}

	// Prompt: contains review body, issue title, and plan text.
	if !strings.Contains(capturedPrompt, "Please rename the helper") {
		t.Errorf("revision prompt missing review body; got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "fix things") {
		t.Errorf("revision prompt missing issue title; got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "files_touched") {
		t.Errorf("revision prompt missing plan text; got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "REVISION phase") {
		t.Errorf("revision prompt missing REVISION marker; got:\n%s", capturedPrompt)
	}

	// Worktree: agent's file was committed (no uncommitted changes).
	if _, statErr := os.Stat(filepath.Join(got.RepoPath, "fix.txt")); statErr != nil {
		t.Errorf("fix.txt not present in worktree: %v", statErr)
	}
	statusOut, _ := gitStatusPorcelain(ctx, got.RepoPath)
	if strings.TrimSpace(statusOut) != "" {
		t.Errorf("expected clean worktree after revision, got:\n%s", statusOut)
	}

	// Comment: confirmation posted on the PR.
	comments, err := h.gh.ListIssueComments(ctx, prNumber, time.Time{})
	if err != nil {
		t.Fatalf("ListIssueComments(pr): %v", err)
	}
	if !containsBody(comments, "pushed revision addressing review feedback") {
		t.Errorf("expected confirmation comment on PR #%d; got %+v", prNumber, comments)
	}
}

// TestPollPRRevisions_RevisionAttemptedSkips verifies that a job with
// RevisionAttempted=true is skipped even when a fresh CHANGES_REQUESTED
// review is present.
func TestPollPRRevisions_RevisionAttemptedSkips(t *testing.T) {
	h := newTestHarness(t)
	o := h.newOrch(t, "x", false)

	job := installPRReadyJob(t, h, 502, 1502, time.Now().UTC())
	job.RevisionAttempted = true
	if err := h.state.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	time.Sleep(5 * time.Millisecond)
	h.gh.SeedPRReview(1502, gh.PRReview{
		User: "alice", State: "CHANGES_REQUESTED",
		Body: "more changes", SubmittedAt: time.Now().UTC(),
	})

	if err := o.PollPRRevisions(context.Background()); err != nil {
		t.Fatalf("PollPRRevisions: %v", err)
	}

	if calls := h.runner.Calls(); len(calls) != 0 {
		t.Errorf("expected runner not called (RevisionAttempted skip), got %d calls", len(calls))
	}
	got, _ := h.state.Load(502)
	if !got.RevisionAttempted {
		t.Errorf("RevisionAttempted should remain true")
	}
	// No revision comment should have been posted on the PR.
	comments, _ := h.gh.ListIssueComments(context.Background(), 1502, time.Time{})
	if containsBody(comments, "pushed revision") || containsBody(comments, "revision agent") {
		t.Errorf("unexpected revision comment posted: %+v", comments)
	}
}

// TestPollPRRevisions_StaleReviewIgnored verifies that a review whose
// SubmittedAt is before the job's UpdatedAt is treated as stale and the
// revision cycle is not triggered.
func TestPollPRRevisions_StaleReviewIgnored(t *testing.T) {
	h := newTestHarness(t)
	o := h.newOrch(t, "x", false)

	jobUpdatedAt := time.Now().UTC()
	installPRReadyJob(t, h, 503, 1503, jobUpdatedAt)

	// Review submitted one hour BEFORE the job last advanced.
	h.gh.SeedPRReview(1503, gh.PRReview{
		User: "alice", State: "CHANGES_REQUESTED",
		Body: "old feedback", SubmittedAt: jobUpdatedAt.Add(-1 * time.Hour),
	})

	if err := o.PollPRRevisions(context.Background()); err != nil {
		t.Fatalf("PollPRRevisions: %v", err)
	}

	if calls := h.runner.Calls(); len(calls) != 0 {
		t.Errorf("expected runner not called for stale review, got %d calls", len(calls))
	}
	got, _ := h.state.Load(503)
	if got.RevisionAttempted {
		t.Errorf("RevisionAttempted should remain false for stale-only reviews")
	}
}

// containsBody reports whether any comment's body contains substr.
func containsBody(comments []types.IssueComment, substr string) bool {
	for _, c := range comments {
		if strings.Contains(c.Body, substr) {
			return true
		}
	}
	return false
}
