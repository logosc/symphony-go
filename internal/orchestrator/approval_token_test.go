package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/logosc/symphony-go/internal/types"
)

// TestApprovalToken_AcceptsExactMatch: with cfg.Approval.RequireToken=true
// and a per-job token of "7392", a comment whose body is exactly "7392"
// from a writer-permission user advances the job out of awaiting_approval.
func TestApprovalToken_AcceptsExactMatch(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "gated"
	h.cfg.Approval.RequireToken = true
	o := h.newOrch(t, "x", false)

	implWriter(h.runner, []string{"a.txt"})

	installAwaitingJob(t, h, 401)
	// Stamp the per-job token after install so the seeded job carries it.
	job, err := h.state.Load(401)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	job.ApprovalToken = "7392"
	if err := h.state.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	h.gh.SetCollaboratorPermission("alice", "write")
	h.gh.SeedComment(401, types.IssueComment{
		User: "alice", Body: "7392", CreatedAt: time.Now().UTC(),
	})

	if err := o.PollApprovals(context.Background()); err != nil {
		t.Fatalf("PollApprovals: %v", err)
	}
	got, _ := h.state.Load(401)
	if got.Status == types.StatusAwaitingApproval {
		t.Fatalf("expected token-mode approval to advance, got %q", got.Status)
	}
}

// TestApprovalToken_RejectsSlashCommand: with require_token enabled, the
// legacy `/symphony approve` slash command does NOT match (the matcher
// checks against the per-plan token, not the static command).
func TestApprovalToken_RejectsSlashCommand(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "gated"
	h.cfg.Approval.RequireToken = true
	o := h.newOrch(t, "x", false)

	implWriter(h.runner, []string{"a.txt"})

	installAwaitingJob(t, h, 402)
	job, err := h.state.Load(402)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	job.ApprovalToken = "1234"
	if err := h.state.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	h.gh.SetCollaboratorPermission("alice", "write")
	h.gh.SeedComment(402, types.IssueComment{
		User: "alice", Body: "/symphony approve", CreatedAt: time.Now().UTC(),
	})

	if err := o.PollApprovals(context.Background()); err != nil {
		t.Fatalf("PollApprovals: %v", err)
	}
	got, _ := h.state.Load(402)
	if got.Status != types.StatusAwaitingApproval {
		t.Fatalf("slash command must not match in token mode; status = %q", got.Status)
	}
}

// TestApprovalToken_WrongNumberRejected: a numeric comment that doesn't
// equal the job's token is ignored, even from a writer.
func TestApprovalToken_WrongNumberRejected(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "gated"
	h.cfg.Approval.RequireToken = true
	o := h.newOrch(t, "x", false)

	implWriter(h.runner, []string{"a.txt"})

	installAwaitingJob(t, h, 403)
	job, err := h.state.Load(403)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	job.ApprovalToken = "7392"
	if err := h.state.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}

	h.gh.SetCollaboratorPermission("alice", "write")
	h.gh.SeedComment(403, types.IssueComment{
		User: "alice", Body: "1234", CreatedAt: time.Now().UTC(),
	})

	if err := o.PollApprovals(context.Background()); err != nil {
		t.Fatalf("PollApprovals: %v", err)
	}
	got, _ := h.state.Load(403)
	if got.Status != types.StatusAwaitingApproval {
		t.Fatalf("wrong-token comment must not advance; status = %q", got.Status)
	}
}

// TestApprovalToken_FallsBackToCommandWhenTokenEmpty: if RequireToken is
// on but the Job has no token (e.g., an old job persisted before this
// feature), the matcher falls back to the static slash command. This
// preserves back-compat for migration.
func TestApprovalToken_FallsBackToCommandWhenTokenEmpty(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "gated"
	h.cfg.Approval.RequireToken = true
	o := h.newOrch(t, "x", false)

	implWriter(h.runner, []string{"a.txt"})

	installAwaitingJob(t, h, 404) // Job has empty ApprovalToken.
	h.gh.SetCollaboratorPermission("alice", "write")
	h.gh.SeedComment(404, types.IssueComment{
		User: "alice", Body: "/symphony approve", CreatedAt: time.Now().UTC(),
	})

	if err := o.PollApprovals(context.Background()); err != nil {
		t.Fatalf("PollApprovals: %v", err)
	}
	got, _ := h.state.Load(404)
	if got.Status == types.StatusAwaitingApproval {
		t.Fatalf("expected fallback-to-command to advance, got %q", got.Status)
	}
}

// TestNewApprovalToken_RangeAndShape: tokens are 4-digit decimals in
// [1000, 9999] and 100 successive draws produce at least 50 distinct
// values (cheap collision smoke; cryptographic strength tested elsewhere).
func TestNewApprovalToken_RangeAndShape(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		tok, err := newApprovalToken()
		if err != nil {
			t.Fatalf("newApprovalToken: %v", err)
		}
		if len(tok) != 4 {
			t.Fatalf("token len = %d (%q); want 4", len(tok), tok)
		}
		for _, r := range tok {
			if r < '0' || r > '9' {
				t.Fatalf("non-digit in token %q", tok)
			}
		}
		seen[tok] = struct{}{}
	}
	if len(seen) < 50 {
		t.Fatalf("only %d unique tokens in 100 draws (low entropy?)", len(seen))
	}
}
