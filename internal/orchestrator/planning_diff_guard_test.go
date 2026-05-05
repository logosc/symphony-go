package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logosc/symphony-go/internal/types"
)

// TestPlanningDiffGuard_FailsOnEditedSource: a buggy/runaway planning
// agent that writes to a source file in RepoPath must be caught and
// the job marked failed before approval routing. Proposal 0005 §4.6.
func TestPlanningDiffGuard_FailsOnEditedSource(t *testing.T) {
	h := newTestHarness(t)
	o := h.newOrch(t, "x", false)

	// OnRun for planning: simulate a misbehaving agent that writes to
	// a source file inside RepoPath, then returns a "successful" plan.
	h.runner.OnRun = func(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
		if req.Phase == types.PhasePlanning {
			illegal := filepath.Join(req.RepoPath, "stowaway.txt")
			if err := os.WriteFile(illegal, []byte("planning agent should not have written this\n"), 0o644); err != nil {
				return types.RunResult{}, err
			}
			return types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}, nil
		}
		return types.RunResult{}, fmt.Errorf("unexpected phase %q", req.Phase)
	}

	iss := h.seedReadyIssue(951, "diff-guard-fail")

	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	got, err := h.state.Load(951)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Status != types.StatusFailed {
		t.Fatalf("expected failed, got %q", got.Status)
	}
	// Failure comment should mention the diff guard.
	comments, _ := h.gh.ListIssueComments(context.Background(), 951, time.Time{})
	if !commentMatchesGuard(comments) {
		t.Errorf("expected a comment naming the planning-edited-source error; got %d comment(s)", len(comments))
	}
}

// TestPlanningDiffGuard_PassesOnCleanWorktree: the normal happy-path
// (planning agent doesn't edit source) must NOT trigger the guard.
// Side-channel writes go under HomePath, not RepoPath, so a clean
// worktree is the expected invariant.
func TestPlanningDiffGuard_PassesOnCleanWorktree(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "gated"
	o := h.newOrch(t, "x", false)

	h.runner.OnRun = func(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
		if req.Phase == types.PhasePlanning {
			// Side-channel write under HomePath is fine — the guard
			// only checks RepoPath.
			sidefile := filepath.Join(req.HomePath, "scratch.json")
			if err := os.WriteFile(sidefile, []byte(`{"x":1}`), 0o644); err != nil {
				return types.RunResult{}, err
			}
			return types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}, nil
		}
		return types.RunResult{}, fmt.Errorf("unexpected phase %q", req.Phase)
	}

	iss := h.seedReadyIssue(952, "diff-guard-pass")

	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	got, err := h.state.Load(952)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Status != types.StatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval (gated path), got %q", got.Status)
	}
}

func commentMatchesGuard(comments []types.IssueComment) bool {
	for _, c := range comments {
		if strings.Contains(c.Body, "planning agent edited source files") {
			return true
		}
	}
	return false
}
