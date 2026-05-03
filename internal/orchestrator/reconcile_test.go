package orchestrator

import (
	"context"
	"testing"

	"github.com/logosc/symphony-go/internal/types"
)

// TestReconcileCrashMidImplementation seeds a job pre-marked
// `implementing` with a stale label. After Reconcile, the issue should be
// blocked.
func TestReconcileCrashMidImplementation(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "handoff"
	o := h.newOrch(t, "x", false)

	// Seed an issue with the implementing label and a corresponding local job.
	iss := types.Issue{
		Number: 100, Title: "crashed", Description: "", State: "open",
		Labels: []string{h.cfg.Labels.Implementing},
	}
	h.gh.SeedIssue(iss, false)
	job := &types.Job{
		IssueNumber:   100,
		Repo:          h.cfg.Repo.FullName,
		Status:        types.StatusImplementing,
		WorkspaceRoot: "/tmp/wt-100",
		RepoPath:      "/tmp/wt-100/repo",
		Branch:        "symphony/issue-100-crashed",
	}
	if err := h.state.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := o.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !findLabel(h.labelsFor(100), h.cfg.Labels.Blocked) {
		t.Fatalf("expected blocked, got %v", h.labelsFor(100))
	}
	updated, _ := h.state.Load(100)
	if updated.Status != types.StatusBlocked {
		t.Fatalf("expected local blocked, got %q", updated.Status)
	}
}

// TestReconcileOrphanPlanning: issue carries planning label, no local job.
func TestReconcileOrphanPlanning(t *testing.T) {
	h := newTestHarness(t)
	o := h.newOrch(t, "x", false)
	iss := types.Issue{Number: 101, Title: "orphan", State: "open",
		Labels: []string{h.cfg.Labels.Planning}}
	h.gh.SeedIssue(iss, false)
	if err := o.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !findLabel(h.labelsFor(101), h.cfg.Labels.Blocked) {
		t.Fatalf("expected blocked, got %v", h.labelsFor(101))
	}
}

// TestReconcileReadyLeavesAlone: row 1 — no local, ready label, leave alone.
func TestReconcileReadyLeavesAlone(t *testing.T) {
	h := newTestHarness(t)
	o := h.newOrch(t, "x", false)
	iss := types.Issue{Number: 102, Title: "fresh", State: "open",
		Labels: []string{h.cfg.Labels.Ready}}
	h.gh.SeedIssue(iss, false)
	if err := o.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !findLabel(h.labelsFor(102), h.cfg.Labels.Ready) {
		t.Fatalf("expected ready unchanged, got %v", h.labelsFor(102))
	}
}

// TestReconcileRetryPlanning: row 7 — local + github both `planning`
// (interrupted mid-plan). Reconcile relabels back to `ready` and drops
// the local state so the dispatch loop retries fresh on the next tick.
// Reconciliation never starts an agent itself.
func TestReconcileRetryPlanning(t *testing.T) {
	h := newTestHarness(t)
	o := h.newOrch(t, "x", false)
	iss := types.Issue{Number: 104, Title: "in-flight planning", State: "open",
		Labels: []string{h.cfg.Labels.Planning}}
	h.gh.SeedIssue(iss, false)
	job := &types.Job{
		IssueNumber: 104,
		Repo:        h.cfg.Repo.FullName,
		Status:      types.StatusPlanning,
		Branch:      "symphony/issue-104-in-flight-planning",
	}
	if err := h.state.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := o.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !findLabel(h.labelsFor(104), h.cfg.Labels.Ready) {
		t.Fatalf("expected ready, got %v", h.labelsFor(104))
	}
	if findLabel(h.labelsFor(104), h.cfg.Labels.Planning) {
		t.Fatalf("expected planning label removed, got %v", h.labelsFor(104))
	}
	if _, err := h.state.Load(104); err == nil {
		t.Fatalf("expected local state deleted, got nil error")
	}
}

// TestReconcilePRReadyTerminal: row 5 / 15 — leave alone.
func TestReconcilePRReadyTerminal(t *testing.T) {
	h := newTestHarness(t)
	o := h.newOrch(t, "x", false)
	iss := types.Issue{Number: 103, Title: "done", State: "open",
		Labels: []string{h.cfg.Labels.PRReady}}
	h.gh.SeedIssue(iss, false)
	if err := o.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !findLabel(h.labelsFor(103), h.cfg.Labels.PRReady) {
		t.Fatalf("expected pr-ready unchanged, got %v", h.labelsFor(103))
	}
}
