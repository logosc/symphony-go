package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/runner"
	"github.com/logosc/symphony-go/internal/types"
)

// TestPlanScopeFile_PreferredOverProse: when the planning agent writes
// a JSON file at $SYMPHONY_PLAN_SCOPE_PATH, the orchestrator parses it
// and ignores the in-prose ## Scope block (which here intentionally
// disagrees, to prove the file wins). Proposal 0004 phase 1.
func TestPlanScopeFile_PreferredOverProse(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Auto.Rules = []config.AutoRule{
		{IssueLabels: []string{"docs"}, MaxPlanFilesClaimed: 5, ReviewerRequired: false},
	}
	h.cfg.Auto.FallbackOnNoRuleMatch = "block"
	o := h.newOrch(t, "x", false)

	// Custom OnRun: assert the env var is set, write the JSON file with
	// FILE-only content, and emit a prose body whose ## Scope block
	// claims a DIFFERENT file. If the orchestrator parses the file (as
	// proposal 0004 mandates), PlanScope must reflect the file's claims.
	h.runner.OnRun = func(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
		if req.Phase == types.PhasePlanning {
			path := scopePathFromEnv(req.ExtraEnv)
			if path == "" {
				return types.RunResult{}, fmt.Errorf("SYMPHONY_PLAN_SCOPE_PATH not set in ExtraEnv: %v", req.ExtraEnv)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return types.RunResult{}, err
			}
			body := `{"files_touched":["from-file.txt"],"estimated_lines_added":3,"estimated_lines_removed":0,"risk_summary":"json side-channel"}`
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return types.RunResult{}, err
			}
			prose := "# Plan\n\nfreeform stuff\n\n## Scope\nfiles_touched:\n  - from-prose.txt\nrisk_summary: prose path\n"
			return types.RunResult{Success: true, Text: prose}, nil
		}
		// Implementation: write the file the JSON claimed, so diff-verify passes.
		if req.Phase == types.PhaseImplementation {
			p := filepath.Join(req.RepoPath, "from-file.txt")
			if err := os.WriteFile(p, []byte("body\n"), 0o644); err != nil {
				return types.RunResult{}, err
			}
			return types.RunResult{Success: true, Text: "done"}, nil
		}
		if r, ok := h.runner.Responses[req.Phase]; ok {
			return r, nil
		}
		return types.RunResult{}, fmt.Errorf("no canned response for phase %q", req.Phase)
	}

	iss := h.seedReadyIssue(901, "doc fix", "docs")

	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	got, err := h.state.Load(901)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PlanScope == nil {
		t.Fatal("expected PlanScope to be set from JSON file")
	}
	if len(got.PlanScope.FilesTouched) != 1 || got.PlanScope.FilesTouched[0] != "from-file.txt" {
		t.Fatalf("expected file-source scope (from-file.txt), got %v — prose path won unexpectedly", got.PlanScope.FilesTouched)
	}
	if got.PlanScope.RiskSummary != "json side-channel" {
		t.Errorf("expected risk_summary from json file, got %q", got.PlanScope.RiskSummary)
	}
}

// TestPlanScopeFile_FallbackToProse: when the agent doesn't write the
// JSON file, the orchestrator falls back to the in-prose ## Scope
// parser (back-compat for older agents / WORKFLOW.md that predate
// proposal 0004).
func TestPlanScopeFile_FallbackToProse(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Auto.Rules = []config.AutoRule{
		{IssueLabels: []string{"docs"}, MaxPlanFilesClaimed: 5, ReviewerRequired: false},
	}
	h.cfg.Auto.FallbackOnNoRuleMatch = "block"
	o := h.newOrch(t, "x", false)

	// Planning: emit a valid prose ## Scope block, do NOT write the file.
	h.runner.Responses[types.PhasePlanning] = types.RunResult{
		Success: true,
		Text:    canonicalPlan([]string{"prose-only.txt"}),
	}
	// Implementation: write the file the prose claimed.
	implWriter(h.runner, []string{"prose-only.txt"})

	iss := h.seedReadyIssue(902, "doc fix 2", "docs")

	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	got, err := h.state.Load(902)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PlanScope == nil {
		t.Fatal("expected PlanScope from prose fallback")
	}
	if len(got.PlanScope.FilesTouched) != 1 || got.PlanScope.FilesTouched[0] != "prose-only.txt" {
		t.Errorf("FilesTouched = %v", got.PlanScope.FilesTouched)
	}
}

// scopePathFromEnv extracts SYMPHONY_PLAN_SCOPE_PATH from a RunRequest's
// ExtraEnv. Returns "" when not present.
func scopePathFromEnv(extra []string) string {
	const key = "SYMPHONY_PLAN_SCOPE_PATH="
	for _, kv := range extra {
		if strings.HasPrefix(kv, key) {
			return strings.TrimPrefix(kv, key)
		}
	}
	return ""
}

// Compile-time guard: the test relies on FakeRunner's OnRun signature.
var _ = (*runner.FakeRunner)(nil)
