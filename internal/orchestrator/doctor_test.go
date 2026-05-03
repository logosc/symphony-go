package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logosc/symphony-go/internal/config"
)

// TestDoctorMissingToken: a config with no token in env should fail.
func TestDoctorMissingToken(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.GitHub.TokenEnv = "DEFINITELY_NOT_SET_XYZ"
	t.Setenv("DEFINITELY_NOT_SET_XYZ", "")
	err := Doctor(context.Background(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_XYZ") {
		t.Fatalf("expected token-empty error, got %v", err)
	}
}

// TestDoctorAutoModeMissingCatchAll: warns when neither catch-all nor
// fallback is set.
func TestDoctorAutoModeMissingCatchAll(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Auto.Rules = []config.AutoRule{
		{IssueLabels: []string{"docs"}, MaxPlanFilesClaimed: 5},
	}
	h.cfg.Auto.FallbackOnNoRuleMatch = ""
	t.Setenv(h.cfg.GitHub.TokenEnv, "")
	err := Doctor(context.Background(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "fallback_on_no_rule_match") {
		t.Fatalf("expected catch-all/fallback error, got %v", err)
	}
}

// TestDoctorBaseBranchMissing: the test repo has no fictional branch.
func TestDoctorBaseBranchMissing(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Repo.BaseBranch = "branch-that-does-not-exist"
	t.Setenv(h.cfg.GitHub.TokenEnv, "")
	err := Doctor(context.Background(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "base branch") {
		t.Fatalf("expected base-branch error, got %v", err)
	}
}

// TestDoctorPerAxisWorkflowFilesPositive: when repo.workflow_files is
// configured with valid in-repo paths and a default key, doctor must not
// flag a per-axis error. (Other doctor errors may still be present —
// we assert specifically on the per-axis messages.)
func TestDoctorPerAxisWorkflowFilesPositive(t *testing.T) {
	h := newTestHarness(t)
	// Place a real WORKFLOW file inside the repo for each axis.
	codePath := filepath.Join(h.cfg.Repo.LocalPath, "WORKFLOW.code.md")
	resPath := filepath.Join(h.cfg.Repo.LocalPath, "WORKFLOW.research.md")
	if err := os.WriteFile(codePath, []byte("# code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resPath, []byte("# research\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.cfg.Repo.WorkflowFile = ""
	h.cfg.Repo.WorkflowFiles = config.OrderedMap[string]{
		Keys: []string{"type:code", "type:research", "default"},
		Values: map[string]string{
			"type:code":     "WORKFLOW.code.md",
			"type:research": "WORKFLOW.research.md",
			"default":       "WORKFLOW.code.md",
		},
	}
	t.Setenv(h.cfg.GitHub.TokenEnv, "")
	err := Doctor(context.Background(), h.cfg)
	if err == nil {
		return
	}
	// We tolerate unrelated errors (no token, etc.) but not per-axis ones.
	if strings.Contains(err.Error(), "workflow_files") {
		t.Fatalf("unexpected per-axis error: %v", err)
	}
}

// TestDoctorPerAxisWorkflowFilesMissingFile fails when an axis points at
// a path that doesn't exist on disk.
func TestDoctorPerAxisWorkflowFilesMissingFile(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Repo.WorkflowFile = ""
	h.cfg.Repo.WorkflowFiles = config.OrderedMap[string]{
		Keys: []string{"type:code", "default"},
		Values: map[string]string{
			"type:code": "WORKFLOW.code.md",
			"default":   "does-not-exist.md",
		},
	}
	t.Setenv(h.cfg.GitHub.TokenEnv, "")
	err := Doctor(context.Background(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "does-not-exist.md") {
		t.Fatalf("expected missing-file error, got %v", err)
	}
}

// TestDoctorApprovalModeByLabelInvalidValue catches typos at doctor time.
func TestDoctorApprovalModeByLabelInvalidValue(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.ModeByLabel = config.OrderedMap[string]{
		Keys: []string{"type:marketing-ads", "default"},
		Values: map[string]string{
			"type:marketing-ads": "gateed", // typo
			"default":            "auto",
		},
	}
	t.Setenv(h.cfg.GitHub.TokenEnv, "")
	err := Doctor(context.Background(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "gated|auto|handoff") {
		t.Fatalf("expected enum violation, got %v", err)
	}
}

// TestDoctorApprovalModeByLabelMissingDefault: the doctor restates the
// missing-default error with a clear knob name.
func TestDoctorApprovalModeByLabelMissingDefault(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.ModeByLabel = config.OrderedMap[string]{
		Keys: []string{"type:marketing-ads"},
		Values: map[string]string{
			"type:marketing-ads": "gated",
		},
	}
	t.Setenv(h.cfg.GitHub.TokenEnv, "")
	err := Doctor(context.Background(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "approval.mode_by_label") {
		t.Fatalf("expected approval.mode_by_label error, got %v", err)
	}
}

// TestDoctorClaudeImplToolsByLabelMissingDefault catches missing default
// entries for any of the claude per-axis maps.
func TestDoctorClaudeImplToolsByLabelMissingDefault(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Claude.ImplementationToolsByLabel = config.OrderedMap[[]string]{
		Keys: []string{"type:research"},
		Values: map[string][]string{
			"type:research": {"Read", "Write"},
		},
	}
	t.Setenv(h.cfg.GitHub.TokenEnv, "")
	err := Doctor(context.Background(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "claude.implementation_tools_by_label") {
		t.Fatalf("expected claude.implementation_tools_by_label error, got %v", err)
	}
}
