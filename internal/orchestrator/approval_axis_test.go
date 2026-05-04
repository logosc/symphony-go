package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/types"
)

// TestApprovalModeByLabel_GatedOverridesAuto verifies that when the
// global approval.mode is `auto` but `mode_by_label["type:marketing-ads"]`
// forces `gated`, an issue carrying the type:marketing-ads label is
// frozen as that axis at claim time and routed to awaiting-approval
// after planning — never reaching implementation without an explicit
// `/symphony approve` comment.
func TestApprovalModeByLabel_GatedOverridesAuto(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	// mode_by_label drives the per-axis decision; the canonical anchor
	// (workflow_files) must also be set so resolveAxis freezes a non-
	// default key on the Job. See orchestrator/job.go::resolveAxis.
	h.cfg.Repo.WorkflowFile = ""
	h.cfg.Repo.WorkflowFiles = config.OrderedMap[string]{
		Keys: []string{"type:marketing-ads", "default"},
		Values: map[string]string{
			"type:marketing-ads": "WORKFLOW.md",
			"default":            "WORKFLOW.md",
		},
	}
	h.cfg.Approval.ModeByLabel = config.OrderedMap[string]{
		Keys: []string{"type:marketing-ads", "default"},
		Values: map[string]string{
			"type:marketing-ads": "gated",
			"default":            "auto",
		},
	}
	h.cfg.Auto.Rules = []config.AutoRule{
		{IssueLabels: nil, MaxPlanFilesClaimed: 20, ReviewerRequired: false},
	}

	o := h.newOrch(t, "Issue: {{ issue.title }}", false)
	o.deps.PromptTemplates = map[string]string{
		"type:marketing-ads": "Issue: {{ issue.title }}",
		"default":            "Issue: {{ issue.title }}",
	}

	h.runner.Responses[types.PhasePlanning] = types.RunResult{
		Success: true,
		Text:    canonicalPlan([]string{"a.txt"}),
	}
	implWriter(h.runner, []string{"a.txt"})

	iss := h.seedReadyIssue(101, "ad campaign", "type:marketing-ads")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.ProcessIssue(ctx, iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}

	// Per-axis gated mode → must be parked at awaiting-approval, not
	// pr-ready.
	if !findLabel(h.labelsFor(101), h.cfg.Labels.AwaitingApproval) {
		t.Fatalf("expected awaiting-approval (per-axis gated), got %v", h.labelsFor(101))
	}
	job, err := h.state.Load(101)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.AxisKey != "type:marketing-ads" {
		t.Fatalf("expected AxisKey=type:marketing-ads, got %q", job.AxisKey)
	}
}

// TestApprovalModeByLabel_AutoForNonMatching: a config with
// mode_by_label[default]=auto routes a non-matching axis through the
// auto path and reaches pr-ready via rules-only.
func TestApprovalModeByLabel_AutoForNonMatching(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Repo.WorkflowFile = ""
	h.cfg.Repo.WorkflowFiles = config.OrderedMap[string]{
		Keys: []string{"type:marketing-ads", "default"},
		Values: map[string]string{
			"type:marketing-ads": "WORKFLOW.md",
			"default":            "WORKFLOW.md",
		},
	}
	h.cfg.Approval.ModeByLabel = config.OrderedMap[string]{
		Keys: []string{"type:marketing-ads", "default"},
		Values: map[string]string{
			"type:marketing-ads": "gated",
			"default":            "auto",
		},
	}
	h.cfg.Auto.Rules = []config.AutoRule{
		{IssueLabels: nil, MaxPlanFilesClaimed: 20, ReviewerRequired: false},
	}

	o := h.newOrch(t, "Issue: {{ issue.title }}", false)
	o.deps.PromptTemplates = map[string]string{
		"type:marketing-ads": "Issue: {{ issue.title }}",
		"default":            "Issue: {{ issue.title }}",
	}

	h.runner.Responses[types.PhasePlanning] = types.RunResult{
		Success: true,
		Text:    canonicalPlan([]string{"a.txt"}),
	}
	implWriter(h.runner, []string{"a.txt"})

	// `type:code` doesn't match the marketing-ads key → falls back to the
	// "default" entry which is `auto`.
	iss := h.seedReadyIssue(102, "ship feature", "type:code")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.ProcessIssue(ctx, iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(102), h.cfg.Labels.PRReady) {
		t.Fatalf("expected pr-ready (per-axis default=auto), got %v", h.labelsFor(102))
	}
	job, _ := h.state.Load(102)
	if job.ApprovalPath != types.ApprovalPathRules {
		t.Fatalf("expected approval_path=rules, got %q", job.ApprovalPath)
	}
}

func TestApprovalModeByLabel_NonAxisLabelOverridesAxisDefault(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Repo.WorkflowFile = ""
	h.cfg.Repo.WorkflowFiles = config.OrderedMap[string]{
		Keys: []string{"type:code", "default"},
		Values: map[string]string{
			"type:code": "WORKFLOW.md",
			"default":   "WORKFLOW.md",
		},
	}
	h.cfg.Approval.ModeByLabel = config.OrderedMap[string]{
		Keys: []string{"budget:over-50", "type:research", "default"},
		Values: map[string]string{
			"budget:over-50": "gated",
			"type:research":  "handoff",
			"default":        "auto",
		},
	}
	h.cfg.Auto.Rules = []config.AutoRule{
		{IssueLabels: nil, MaxPlanFilesClaimed: 20, ReviewerRequired: false},
	}

	o := h.newOrch(t, "Issue: {{ issue.title }}", false)
	o.deps.PromptTemplates = map[string]string{
		"type:code": "Issue: {{ issue.title }}",
		"default":   "Issue: {{ issue.title }}",
	}

	h.runner.Responses[types.PhasePlanning] = types.RunResult{
		Success: true,
		Text:    canonicalPlan([]string{"a.txt"}),
	}
	implWriter(h.runner, []string{"a.txt"})

	iss := h.seedReadyIssue(103, "expensive code", "type:code", "budget:over-50")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.ProcessIssue(ctx, iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(103), h.cfg.Labels.AwaitingApproval) {
		t.Fatalf("expected awaiting-approval (budget label gated), got %v", h.labelsFor(103))
	}
	job, err := h.state.Load(103)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.AxisKey != "type:code" {
		t.Fatalf("expected AxisKey=type:code, got %q", job.AxisKey)
	}
}
