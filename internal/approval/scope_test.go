package approval

import (
	"strings"
	"testing"
)

func TestParseScope_ValidBlockAtEnd(t *testing.T) {
	body := `# Plan

Some prose.

## Scope
files_touched:
  - a.go
  - b.go
estimated_lines_added: 10
estimated_lines_removed: 2
risk_summary: low risk
`
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected scope, got nil")
	}
	if len(s.FilesTouched) != 2 || s.FilesTouched[0] != "a.go" || s.FilesTouched[1] != "b.go" {
		t.Errorf("FilesTouched = %v", s.FilesTouched)
	}
	if s.EstimatedLinesAdded != 10 || s.EstimatedLinesRemoved != 2 {
		t.Errorf("estimates = +%d -%d", s.EstimatedLinesAdded, s.EstimatedLinesRemoved)
	}
	if s.RiskSummary != "low risk" {
		t.Errorf("RiskSummary = %q", s.RiskSummary)
	}
}

func TestParseScope_SurroundingNoise(t *testing.T) {
	body := strings.Join([]string{
		"intro paragraph",
		"",
		"## Scope",
		"files_touched:",
		"  - x.go",
		"risk_summary: nope",
		"",
		"## Notes",
		"trailing prose",
	}, "\n")
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("nil scope")
	}
	if len(s.FilesTouched) != 1 || s.FilesTouched[0] != "x.go" {
		t.Errorf("FilesTouched = %v", s.FilesTouched)
	}
	if s.RiskSummary != "nope" {
		t.Errorf("RiskSummary = %q", s.RiskSummary)
	}
}

func TestParseScope_OnlyRequiredAndRisk(t *testing.T) {
	body := "## Scope\nfiles_touched: [a]\nrisk_summary: only two\n"
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.EstimatedLinesAdded != 0 || s.EstimatedLinesRemoved != 0 {
		t.Errorf("expected zero estimates, got +%d -%d", s.EstimatedLinesAdded, s.EstimatedLinesRemoved)
	}
	if s.RiskSummary != "only two" {
		t.Errorf("RiskSummary = %q", s.RiskSummary)
	}
}

func TestParseScope_MissingBlockReturnsNilNil(t *testing.T) {
	s, err := ParseScope("# Plan\n\nNo scope here.\n")
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if s != nil {
		t.Errorf("expected nil scope, got %+v", s)
	}
}

func TestParseScope_SingleFileFallback(t *testing.T) {
	body := strings.Join([]string{
		"**Plan summary / 计划摘要:**",
		"",
		"- **Only file touched:** `shopify/CLAUDE.md`",
		"",
		"Risk is minimal — doc-only edit.",
	}, "\n")
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if s == nil {
		t.Fatal("expected fallback scope, got nil")
	}
	if len(s.FilesTouched) != 1 || s.FilesTouched[0] != "shopify/CLAUDE.md" {
		t.Errorf("FilesTouched = %v", s.FilesTouched)
	}
}

func TestParseScope_MalformedYAML(t *testing.T) {
	body := "## Scope\nfiles_touched: [a, b\nrisk_summary: x\n"
	_, err := ParseScope(body)
	if err == nil {
		t.Fatal("expected error for malformed yaml")
	}
}

func TestParseScope_MissingFilesTouched(t *testing.T) {
	body := "## Scope\nrisk_summary: nothing claimed\n"
	_, err := ParseScope(body)
	if err == nil {
		t.Fatal("expected error for missing files_touched")
	}
}

func TestParseScope_CaseInsensitiveHeading(t *testing.T) {
	body := "##   SCOPE   \nfiles_touched:\n  - a.go\n"
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil || len(s.FilesTouched) != 1 {
		t.Errorf("scope = %+v", s)
	}
}

func TestParseScope_BoundedByNextH2(t *testing.T) {
	body := strings.Join([]string{
		"## Scope",
		"files_touched: [a.go]",
		"## Plan",
		"files_touched: [SHOULD_NOT_LEAK]",
	}, "\n")
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil || len(s.FilesTouched) != 1 || s.FilesTouched[0] != "a.go" {
		t.Errorf("FilesTouched = %v", s.FilesTouched)
	}
}
