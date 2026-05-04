package approval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseScopeFromFile_Valid: agent wrote a clean JSON file at the
// side-channel path; the orchestrator decodes it into a PlanScope.
// Regression for proposal 0004 phase 1.
func TestParseScopeFromFile_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scope.json")
	if err := os.WriteFile(path, []byte(`{
  "files_touched": ["a.go", "b.go"],
  "estimated_lines_added": 10,
  "estimated_lines_removed": 2,
  "risk_summary": "low risk"
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ParseScopeFromFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.FilesTouched) != 2 || s.FilesTouched[0] != "a.go" {
		t.Errorf("FilesTouched = %v", s.FilesTouched)
	}
	if s.EstimatedLinesAdded != 10 || s.EstimatedLinesRemoved != 2 {
		t.Errorf("estimates = +%d -%d", s.EstimatedLinesAdded, s.EstimatedLinesRemoved)
	}
	if s.RiskSummary != "low risk" {
		t.Errorf("RiskSummary = %q", s.RiskSummary)
	}
}

// TestParseScopeFromFile_Missing: agent didn't write the file. Caller
// distinguishes this from "wrote garbage" via os.IsNotExist on the
// returned error, then falls back to the prose parser.
func TestParseScopeFromFile_Missing(t *testing.T) {
	_, err := ParseScopeFromFile(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got %v", err)
	}
}

// TestParseScopeFromFile_MalformedJSON: agent wrote the file but the
// JSON is bad. Returns a non-IsNotExist error.
func TestParseScopeFromFile_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scope.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ParseScopeFromFile(path)
	if err == nil {
		t.Fatal("expected error for malformed json")
	}
	if os.IsNotExist(err) {
		t.Errorf("malformed should not look like missing")
	}
}

// TestParseScopeFromFile_MissingFilesTouched: required field omitted;
// must error rather than silently produce an empty scope.
func TestParseScopeFromFile_MissingFilesTouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scope.json")
	if err := os.WriteFile(path, []byte(`{"risk_summary": "x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseScopeFromFile(path); err == nil {
		t.Fatal("expected error for missing files_touched")
	}
}

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

func TestParseScope_UniqueBacktickedPathFallback(t *testing.T) {
	body := strings.Join([]string{
		"**Change:**",
		"Edit `shopify/CLAUDE.md` near the top.",
		"",
		"**Verification:** `cd shopify && npm run typecheck`",
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

func TestParseScope_UniqueBacktickedPathFallbackAmbiguous(t *testing.T) {
	body := "Edit `a/file.md` and maybe `b/file.md`."
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if s != nil {
		t.Errorf("expected nil scope for ambiguous fallback, got %+v", s)
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

// TestParseScope_FencedYAMLBlock: LLMs (e.g. Codex) sometimes wrap the
// scope yaml in a ```yaml ... ``` fence despite the prompt saying not
// to. The parser must tolerate the wrapper. Regression for issue
// print-my-ideas/print-my-ideas#62.
func TestParseScope_FencedYAMLBlock(t *testing.T) {
	body := strings.Join([]string{
		"## Scope",
		"```yaml",
		"files_touched:",
		"  - shopify/src/worker.ts",
		"  - shopify/extensions/ai-customizer-src/ai-customizer.js",
		"estimated_lines_added: 45",
		"estimated_lines_removed: 8",
		"risk_summary: additive change",
		"```",
	}, "\n")
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected scope, got nil")
	}
	if len(s.FilesTouched) != 2 {
		t.Errorf("FilesTouched = %v", s.FilesTouched)
	}
	if s.EstimatedLinesAdded != 45 || s.EstimatedLinesRemoved != 8 {
		t.Errorf("estimates = +%d -%d", s.EstimatedLinesAdded, s.EstimatedLinesRemoved)
	}
	if s.RiskSummary != "additive change" {
		t.Errorf("RiskSummary = %q", s.RiskSummary)
	}
}

// TestParseScope_FencedPlainBlock: same as above but with an unlabeled
// ``` fence (no language hint).
func TestParseScope_FencedPlainBlock(t *testing.T) {
	body := "## Scope\n```\nfiles_touched: [a.go]\n```\n"
	s, err := ParseScope(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil || len(s.FilesTouched) != 1 || s.FilesTouched[0] != "a.go" {
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
