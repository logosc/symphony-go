package approval

import (
	"testing"

	"github.com/chenlong-seu/symphony-go/internal/config"
	"github.com/chenlong-seu/symphony-go/internal/types"
)

func TestEvaluate_CatchAllRuleMatches(t *testing.T) {
	rules := []config.AutoRule{
		{IssueLabels: nil, MaxPlanFilesClaimed: 5},
	}
	scope := types.PlanScope{FilesTouched: []string{"a.go", "b.go"}}
	m := Evaluate(rules, []string{"docs"}, scope)
	if m.Index != 0 || m.Rule == nil {
		t.Errorf("expected index 0, got %+v", m)
	}
}

func TestEvaluate_LabelGate(t *testing.T) {
	rules := []config.AutoRule{
		{IssueLabels: []string{"docs"}, MaxPlanFilesClaimed: 10, ReviewerRequired: true},
	}
	scope := types.PlanScope{FilesTouched: []string{"README.md"}}
	m := Evaluate(rules, []string{"docs", "good-first-issue"}, scope)
	if m.Index != 0 {
		t.Fatalf("expected match, got %+v", m)
	}
	if !m.ReviewerRequired {
		t.Error("expected ReviewerRequired=true")
	}
}

func TestEvaluate_CaseInsensitiveLabel(t *testing.T) {
	rules := []config.AutoRule{
		{IssueLabels: []string{"Docs"}, MaxPlanFilesClaimed: 5},
	}
	scope := types.PlanScope{FilesTouched: []string{"x"}}
	m := Evaluate(rules, []string{"DOCS"}, scope)
	if m.Index != 0 {
		t.Errorf("expected case-insensitive match, got %+v", m)
	}
}

func TestEvaluate_TooManyFilesNoMatch(t *testing.T) {
	rules := []config.AutoRule{
		{IssueLabels: []string{"docs"}, MaxPlanFilesClaimed: 2},
	}
	scope := types.PlanScope{FilesTouched: []string{"a", "b", "c"}}
	m := Evaluate(rules, []string{"docs"}, scope)
	if m.Index != -1 {
		t.Errorf("expected no match, got %+v", m)
	}
}

func TestEvaluate_FirstMatchWins(t *testing.T) {
	rules := []config.AutoRule{
		{IssueLabels: []string{"docs"}, MaxPlanFilesClaimed: 1, ReviewerRequired: false},
		{IssueLabels: nil, MaxPlanFilesClaimed: 100, ReviewerRequired: true},
	}
	scope := types.PlanScope{FilesTouched: []string{"a"}}
	m := Evaluate(rules, []string{"docs"}, scope)
	if m.Index != 0 {
		t.Errorf("expected rule 0 to win, got %+v", m)
	}
	if m.ReviewerRequired {
		t.Error("expected ReviewerRequired=false from rule 0")
	}
	// And when first rule's file cap excludes us, fall through to catch-all.
	scope2 := types.PlanScope{FilesTouched: []string{"a", "b"}}
	m2 := Evaluate(rules, []string{"docs"}, scope2)
	if m2.Index != 1 {
		t.Errorf("expected fallthrough to rule 1, got %+v", m2)
	}
}
