// Package approval implements minisymphony's auto-approval logic: parsing
// the planner's `## Scope` block, evaluating auto.rules against an issue,
// and verifying that an implementation diff stayed within the claimed
// scope (SPEC §10).
//
// This file holds the plan-scope parser. It is pure: no I/O, no logging.
package approval

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

// ParseScope extracts the `## Scope` block from a plan body. Returns
// nil, nil when the block is absent (caller decides fallback). Returns
// an error when the block is present but malformed.
//
// Recognized fields: files_touched (list of strings), estimated_lines_added
// (int), estimated_lines_removed (int), risk_summary (string). Unknown
// fields are ignored. Missing required fields (currently just
// files_touched) produce an error.
func ParseScope(planBody string) (*types.PlanScope, error) {
	body, found := extractScopeBlock(planBody)
	if !found {
		return nil, nil
	}

	var raw struct {
		FilesTouched          []string `yaml:"files_touched"`
		EstimatedLinesAdded   *int     `yaml:"estimated_lines_added"`
		EstimatedLinesRemoved *int     `yaml:"estimated_lines_removed"`
		RiskSummary           string   `yaml:"risk_summary"`
	}
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("approval: parse scope yaml: %w", err)
	}
	if raw.FilesTouched == nil {
		return nil, fmt.Errorf("approval: scope block missing required field files_touched")
	}
	scope := &types.PlanScope{
		FilesTouched: raw.FilesTouched,
		RiskSummary:  raw.RiskSummary,
	}
	if raw.EstimatedLinesAdded != nil {
		scope.EstimatedLinesAdded = *raw.EstimatedLinesAdded
	}
	if raw.EstimatedLinesRemoved != nil {
		scope.EstimatedLinesRemoved = *raw.EstimatedLinesRemoved
	}
	return scope, nil
}

// extractScopeBlock walks planBody line-by-line, finds a heading whose
// normalized form is `## scope` (case-insensitive, trailing whitespace
// allowed), and returns everything until the next `## ` heading or EOF.
// The boolean reports whether such a heading was found.
func extractScopeBlock(planBody string) (string, bool) {
	lines := strings.Split(planBody, "\n")
	start := -1
	for i, line := range lines {
		if isScopeHeading(line) {
			start = i + 1
			break
		}
	}
	if start == -1 {
		return "", false
	}
	end := len(lines)
	for j := start; j < len(lines); j++ {
		if isH2Heading(lines[j]) {
			end = j
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

// isScopeHeading reports whether line is the `## Scope` heading.
func isScopeHeading(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "## ") {
		return false
	}
	rest := strings.TrimSpace(trimmed[3:])
	return strings.EqualFold(rest, "scope")
}

// isH2Heading reports whether line begins a new `## ` H2 heading.
func isH2Heading(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, "## ")
}
