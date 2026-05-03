package approval

import (
	"strings"

	"github.com/chenlong-seu/symphony-go/internal/config"
	"github.com/chenlong-seu/symphony-go/internal/types"
)

// RuleMatch is the result of evaluating an auto.rules list against an
// issue + plan scope. Index is -1 when no rule matched.
type RuleMatch struct {
	// Index of the matching rule in the original auto.rules slice; -1
	// when no rule matched.
	Index int
	// Rule is a pointer to the matched config.AutoRule; nil when no
	// rule matched.
	Rule *config.AutoRule
	// ReviewerRequired echoes the matched rule's reviewer_required flag
	// for caller convenience.
	ReviewerRequired bool
}

// Evaluate iterates auto.Rules in order and returns the first match.
// A rule matches when:
//   - issue has at least one of rule.IssueLabels (case-insensitive),
//     OR rule.IssueLabels is empty (catch-all)
//   - AND len(scope.FilesTouched) <= rule.MaxPlanFilesClaimed
//
// Returns Index=-1 if no rule matches.
func Evaluate(rules []config.AutoRule, issueLabels []string, scope types.PlanScope) RuleMatch {
	normalizedIssue := make(map[string]struct{}, len(issueLabels))
	for _, l := range issueLabels {
		normalizedIssue[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	claimed := len(scope.FilesTouched)
	for i := range rules {
		r := &rules[i]
		if !labelsMatch(r.IssueLabels, normalizedIssue) {
			continue
		}
		if claimed > r.MaxPlanFilesClaimed {
			continue
		}
		return RuleMatch{Index: i, Rule: r, ReviewerRequired: r.ReviewerRequired}
	}
	return RuleMatch{Index: -1}
}

// labelsMatch reports whether the rule's issue_labels selector is
// satisfied by the (already lower-cased) set of issue labels. An empty
// rule selector is a catch-all and always matches.
func labelsMatch(ruleLabels []string, issueSet map[string]struct{}) bool {
	if len(ruleLabels) == 0 {
		return true
	}
	for _, rl := range ruleLabels {
		if _, ok := issueSet[strings.ToLower(strings.TrimSpace(rl))]; ok {
			return true
		}
	}
	return false
}
