// Package approval implements symphony-go's auto-approval logic: parsing
// the planner's `## Scope` block, evaluating auto.rules against an issue,
// and verifying that an implementation diff stayed within the claimed
// scope (SPEC §10).
//
// This file holds the plan-scope parser. It is pure: no I/O, no logging.
package approval

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/logosc/symphony-go/internal/types"
)

// ParseScopeFromFile reads a JSON file written by the planning agent
// (via the SYMPHONY_PLAN_SCOPE_PATH side-channel) and decodes it into
// a PlanScope. Returns os.IsNotExist-wrapped errors when the file is
// missing so callers can distinguish "agent didn't write it" from
// "agent wrote garbage." Returns an error when the file is present
// but malformed or missing required field files_touched. See
// proposal 0004.
func ParseScopeFromFile(path string) (*types.PlanScope, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s types.PlanScope
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("approval: parse scope json: %w", err)
	}
	if len(s.FilesTouched) == 0 {
		return nil, fmt.Errorf("approval: scope json missing required field files_touched")
	}
	return &s, nil
}

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
		return parseSingleFileFallback(planBody), nil
	}
	body = stripCodeFences(body)

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

func parseSingleFileFallback(planBody string) *types.PlanScope {
	if path := parseOnlyFileTouched(planBody); path != "" {
		return singleFileScope(path)
	}
	if path := parseUniqueBacktickedPath(planBody); path != "" {
		return singleFileScope(path)
	}
	return nil
}

func parseOnlyFileTouched(planBody string) string {
	lower := strings.ToLower(planBody)
	idx := strings.Index(lower, "only file touched")
	if idx == -1 {
		return ""
	}
	rest := planBody[idx:]
	firstTick := strings.Index(rest, "`")
	if firstTick == -1 {
		return ""
	}
	rest = rest[firstTick+1:]
	secondTick := strings.Index(rest, "`")
	if secondTick == -1 {
		return ""
	}
	path := strings.TrimSpace(rest[:secondTick])
	if path == "" || strings.Contains(path, "\n") {
		return ""
	}
	return path
}

func parseUniqueBacktickedPath(planBody string) string {
	var paths []string
	rest := planBody
	for {
		firstTick := strings.Index(rest, "`")
		if firstTick == -1 {
			break
		}
		rest = rest[firstTick+1:]
		secondTick := strings.Index(rest, "`")
		if secondTick == -1 {
			break
		}
		token := strings.TrimSpace(rest[:secondTick])
		rest = rest[secondTick+1:]
		if looksLikeRepoPath(token) {
			paths = append(paths, token)
		}
	}
	seen := make(map[string]struct{}, len(paths))
	var unique []string
	for _, p := range paths {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		unique = append(unique, p)
	}
	if len(unique) != 1 {
		return ""
	}
	return unique[0]
}

func looksLikeRepoPath(token string) bool {
	if token == "" || strings.ContainsAny(token, " \t\n\r") {
		return false
	}
	if strings.HasPrefix(token, "/") || strings.Contains(token, "..") {
		return false
	}
	if !strings.Contains(token, "/") || !strings.Contains(token, ".") {
		return false
	}
	return true
}

func singleFileScope(path string) *types.PlanScope {
	return &types.PlanScope{
		FilesTouched: []string{path},
		RiskSummary:  "single-file plan fallback",
	}
}

// stripCodeFences removes a single leading ```[lang] line and its
// matching trailing ``` line from a scope-block body. LLMs frequently
// wrap structured output in fenced blocks despite the prompt saying
// not to; strip the wrapper before yaml.Unmarshal so we don't reject
// otherwise-valid plans.
func stripCodeFences(body string) string {
	lines := strings.Split(body, "\n")
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if end-start < 2 {
		return body
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[start]), "```") {
		return body
	}
	if strings.TrimSpace(lines[end-1]) != "```" {
		return body
	}
	return strings.Join(lines[start+1:end-1], "\n")
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
