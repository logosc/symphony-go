// Package approval implements minisymphony's auto-approval logic.
//
// This file holds the reviewer-agent driver: it renders a fixed,
// non-user-controlled prompt, runs a separate AgentRunner instance in
// review (read-only) phase, and parses the agent's `## Decision` JSON
// block into a structured ReviewerDecision. See SPEC §9 (Reviewer) and
// §10 (auto mode).
package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/chenlong-seu/symphony-go/internal/config"
	"github.com/chenlong-seu/symphony-go/internal/runner"
	"github.com/chenlong-seu/symphony-go/internal/types"
)

// reviewerPromptTmpl is the fixed reviewer prompt. It is intentionally a
// package-internal constant so it cannot be influenced by the
// user-controlled WORKFLOW.md template (SPEC §9: reviewer prompt is
// trusted/fixed).
const reviewerPromptTmpl = "You are a code-review agent. You must inspect a proposed plan for a\n" +
	"GitHub issue and produce a decision.\n" +
	"\n" +
	"Issue: {{.Title}}\n" +
	"Issue body:\n" +
	"{{.Body}}\n" +
	"\n" +
	"Proposed plan:\n" +
	"{{.Plan}}\n" +
	"\n" +
	"Your job: decide whether the plan is safe to implement automatically. Reject\n" +
	"if:\n" +
	"- the plan touches files unrelated to the issue\n" +
	"- the plan does anything destructive (drops data, deletes files outside its\n" +
	"  stated scope, modifies CI/CD, modifies WORKFLOW.md or its own permissions)\n" +
	"- the plan attempts to call out to the network or install software at\n" +
	"  runtime\n" +
	"- the plan is unclear or under-specified\n" +
	"\n" +
	"Approve only when the plan is clearly bounded and matches the issue.\n" +
	"\n" +
	"You are read-only. Do not edit files. Inspect the repository if needed.\n" +
	"\n" +
	"End your response with the following EXACT block (a JSON object, with the\n" +
	"fenced code block):\n" +
	"\n" +
	"## Decision\n" +
	"```json\n" +
	"{\"decision\": \"approve\" | \"reject\", \"reasons\": [\"...\", \"...\"]}\n" +
	"```\n"

// reviewerTmpl is the parsed prompt template. Parsed once at init so that
// rendering errors at request time can only stem from data, not the
// template literal.
var reviewerTmpl = template.Must(template.New("reviewer").Parse(reviewerPromptTmpl))

// stderrTruncateLimit is the cap on how many bytes of runner stderr we
// surface in a synthesized reject reason.
const stderrTruncateLimit = 500

// Reviewer drives a separate AgentRunner instance with a fixed prompt to
// review an agent's plan and produce an approve/reject decision. It is
// safe for concurrent use as long as the underlying AgentRunner is.
type Reviewer struct {
	runner runner.AgentRunner
	cfg    config.ReviewerConfig
}

// NewReviewer constructs a Reviewer that delegates subprocess execution
// to runner. cfg supplies the per-review timeout (TimeoutSeconds == 0
// disables the timeout).
func NewReviewer(r runner.AgentRunner, cfg config.ReviewerConfig) *Reviewer {
	return &Reviewer{runner: r, cfg: cfg}
}

// ReviewInput is the per-review input bundle. RepoPath is the
// orchestrator's local repo (read-only from the reviewer's perspective);
// HomePath is the dedicated HOME for the reviewer subprocess.
type ReviewInput struct {
	Issue    types.Issue
	PlanText string
	RepoPath string
	HomePath string
}

// Review runs the reviewer agent and parses its `## Decision` JSON.
// Malformed output is treated as a reject with the reason
// "malformed reviewer output" (and a nil error, so the orchestrator's
// fallback path takes over). A non-nil error is only returned when the
// underlying runner itself errors out.
func (r *Reviewer) Review(ctx context.Context, in ReviewInput) (types.ReviewerDecision, error) {
	prompt, err := renderReviewerPrompt(in.Issue, in.PlanText)
	if err != nil {
		return types.ReviewerDecision{}, fmt.Errorf("reviewer: render prompt: %w", err)
	}

	var timeout time.Duration
	if r.cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(r.cfg.TimeoutSeconds) * time.Second
	}

	req := types.RunRequest{
		Issue:    in.Issue,
		RepoPath: in.RepoPath,
		HomePath: in.HomePath,
		Prompt:   prompt,
		Phase:    types.PhaseReview,
		Timeout:  timeout,
	}

	result, err := r.runner.Run(ctx, req)
	if err != nil {
		return types.ReviewerDecision{}, err
	}

	if !result.Success {
		stderr := truncate(result.Stderr, stderrTruncateLimit)
		reason := "reviewer runner failed"
		if stderr != "" {
			reason = fmt.Sprintf("reviewer runner failed: stderr=%q", stderr)
		}
		return types.ReviewerDecision{
			Decision: "reject",
			Reasons:  []string{reason},
		}, nil
	}

	dec, perr := ParseDecision(result.Text)
	if perr != nil || (dec.Decision != "approve" && dec.Decision != "reject") {
		return types.ReviewerDecision{
			Decision: "reject",
			Reasons:  []string{"malformed reviewer output"},
		}, nil
	}
	return dec, nil
}

// renderReviewerPrompt renders the fixed reviewer prompt with the given
// issue and plan text.
func renderReviewerPrompt(issue types.Issue, plan string) (string, error) {
	var buf strings.Builder
	data := struct {
		Title string
		Body  string
		Plan  string
	}{
		Title: issue.Title,
		Body:  issue.Description,
		Plan:  plan,
	}
	if err := reviewerTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// decisionHeadingRe matches the `## Decision` heading line. The heading
// must be at the start of a line; surrounding whitespace on the heading
// line itself is tolerated.
var decisionHeadingRe = regexp.MustCompile(`(?m)^[ \t]*##[ \t]+Decision[ \t]*$`)

// fencedJSONRe matches a ```json ... ``` fenced block. The (?s) flag
// lets `.` span newlines so we can capture multi-line JSON bodies.
var fencedJSONRe = regexp.MustCompile("(?s)```json\\s*\\n(.*?)```")

// ParseDecision is exported for testing. It looks for a `## Decision`
// heading followed by a fenced JSON block (```json ... ```), or falls
// back to bare JSON immediately after the heading. Extra fields in the
// JSON object are silently ignored.
func ParseDecision(reviewerOutput string) (types.ReviewerDecision, error) {
	loc := decisionHeadingRe.FindStringIndex(reviewerOutput)
	if loc == nil {
		return types.ReviewerDecision{}, fmt.Errorf("reviewer output: missing `## Decision` heading")
	}
	tail := reviewerOutput[loc[1]:]

	// Prefer a fenced ```json``` block.
	if m := fencedJSONRe.FindStringSubmatch(tail); m != nil {
		return decodeDecision(m[1])
	}

	// Fall back to bare JSON: scan for the first '{' and decode from there.
	idx := strings.IndexByte(tail, '{')
	if idx < 0 {
		return types.ReviewerDecision{}, fmt.Errorf("reviewer output: no JSON object after `## Decision`")
	}
	return decodeDecision(tail[idx:])
}

// decodeDecision parses a single JSON object from s using a streaming
// decoder so trailing content (e.g., closing ``` of a fence stripped by
// the caller, or extra prose) does not cause parse failure.
func decodeDecision(s string) (types.ReviewerDecision, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	var out types.ReviewerDecision
	if err := dec.Decode(&out); err != nil {
		return types.ReviewerDecision{}, fmt.Errorf("reviewer output: decode JSON: %w", err)
	}
	return out, nil
}

// truncate clips s to at most n bytes, appending an ellipsis marker if
// truncation occurred. It is byte-oriented (rather than rune-oriented)
// because the caller uses it on opaque captured stderr.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
