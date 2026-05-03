package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenlong-seu/symphony-go/internal/config"
	"github.com/chenlong-seu/symphony-go/internal/runner"
	"github.com/chenlong-seu/symphony-go/internal/types"
)

func TestParseDecision_FencedApprove(t *testing.T) {
	out := "Some prose here.\n\n## Decision\n```json\n{\"decision\": \"approve\", \"reasons\": [\"plan looks bounded\"]}\n```\n"
	got, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Decision != "approve" {
		t.Errorf("decision = %q, want approve", got.Decision)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != "plan looks bounded" {
		t.Errorf("reasons = %v", got.Reasons)
	}
}

func TestParseDecision_FencedReject(t *testing.T) {
	out := "## Decision\n```json\n{\"decision\": \"reject\", \"reasons\": [\"touches unrelated files\", \"no tests\"]}\n```"
	got, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Decision != "reject" {
		t.Errorf("decision = %q, want reject", got.Decision)
	}
	if len(got.Reasons) != 2 {
		t.Errorf("reasons = %v", got.Reasons)
	}
}

func TestParseDecision_MissingHeading(t *testing.T) {
	out := "no heading here\n```json\n{\"decision\": \"approve\", \"reasons\": []}\n```\n"
	if _, err := ParseDecision(out); err == nil {
		t.Fatal("expected error for missing heading")
	}
}

func TestParseDecision_MalformedJSON(t *testing.T) {
	out := "## Decision\n```json\n{not valid json\n```"
	if _, err := ParseDecision(out); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestParseDecision_ExtraFieldsIgnored(t *testing.T) {
	out := "## Decision\n```json\n{\"decision\": \"approve\", \"reasons\": [\"ok\"], \"extra\": 42, \"nested\": {\"k\": \"v\"}}\n```"
	got, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Decision != "approve" {
		t.Errorf("decision = %q", got.Decision)
	}
}

func TestParseDecision_BareJSON(t *testing.T) {
	out := "preamble\n## Decision\n{\"decision\": \"reject\", \"reasons\": [\"unbounded\"]}\ntrailing prose\n"
	got, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Decision != "reject" {
		t.Errorf("decision = %q, want reject", got.Decision)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != "unbounded" {
		t.Errorf("reasons = %v", got.Reasons)
	}
}

// --- Review() tests ---

func newReviewInput() ReviewInput {
	return ReviewInput{
		Issue: types.Issue{
			Number:      7,
			Title:       "Fix flaky cache test",
			Description: "The cache test occasionally fails on CI due to a TOCTOU race.",
		},
		PlanText: "Plan: add a mutex around the cache lookup; touch internal/cache/cache.go only.",
		RepoPath: "/tmp/repo",
		HomePath: "/tmp/home-reviewer",
	}
}

func TestReview_Approve(t *testing.T) {
	fr := runner.NewFakeRunner()
	fr.Responses[types.PhaseReview] = types.RunResult{
		Success: true,
		Text:    "looks good\n## Decision\n```json\n{\"decision\": \"approve\", \"reasons\": [\"clearly bounded\"]}\n```\n",
	}
	rv := NewReviewer(fr, config.ReviewerConfig{TimeoutSeconds: 10})

	got, err := rv.Review(context.Background(), newReviewInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Decision != "approve" {
		t.Errorf("decision = %q, want approve", got.Decision)
	}
}

func TestReview_MalformedTreatedAsReject(t *testing.T) {
	fr := runner.NewFakeRunner()
	fr.Responses[types.PhaseReview] = types.RunResult{
		Success: true,
		Text:    "the agent forgot the heading entirely",
	}
	rv := NewReviewer(fr, config.ReviewerConfig{})

	got, err := rv.Review(context.Background(), newReviewInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Decision != "reject" {
		t.Errorf("decision = %q, want reject", got.Decision)
	}
	if len(got.Reasons) == 0 || !strings.Contains(got.Reasons[0], "malformed") {
		t.Errorf("reasons = %v, want contains 'malformed'", got.Reasons)
	}
}

func TestReview_RunnerFailureSurfacesStderr(t *testing.T) {
	fr := runner.NewFakeRunner()
	fr.Responses[types.PhaseReview] = types.RunResult{
		Success: false,
		Stderr:  "oom killed",
	}
	rv := NewReviewer(fr, config.ReviewerConfig{})

	got, err := rv.Review(context.Background(), newReviewInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Decision != "reject" {
		t.Errorf("decision = %q, want reject", got.Decision)
	}
	if len(got.Reasons) == 0 || !strings.Contains(got.Reasons[0], "oom killed") {
		t.Errorf("reasons = %v, want contains stderr text", got.Reasons)
	}
}

func TestReview_RunnerFailureTruncatesLongStderr(t *testing.T) {
	long := strings.Repeat("x", 1000)
	fr := runner.NewFakeRunner()
	fr.Responses[types.PhaseReview] = types.RunResult{
		Success: false,
		Stderr:  long,
	}
	rv := NewReviewer(fr, config.ReviewerConfig{})

	got, err := rv.Review(context.Background(), newReviewInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Decision != "reject" {
		t.Fatalf("decision = %q", got.Decision)
	}
	// reason embeds the (truncated) stderr; ensure the full 1000-byte
	// blob is not surfaced verbatim.
	if strings.Contains(got.Reasons[0], long) {
		t.Errorf("expected stderr to be truncated, got full length")
	}
}

func TestReview_RunnerErrorPropagated(t *testing.T) {
	fr := runner.NewFakeRunner()
	wantErr := errors.New("subprocess spawn failed")
	fr.Errors[types.PhaseReview] = wantErr
	rv := NewReviewer(fr, config.ReviewerConfig{})

	_, err := rv.Review(context.Background(), newReviewInput())
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestReview_TimeoutPropagated(t *testing.T) {
	fr := runner.NewFakeRunner()
	fr.Responses[types.PhaseReview] = types.RunResult{
		Success: true,
		Text:    "## Decision\n```json\n{\"decision\":\"approve\",\"reasons\":[]}\n```",
	}
	rv := NewReviewer(fr, config.ReviewerConfig{TimeoutSeconds: 42})

	if _, err := rv.Review(context.Background(), newReviewInput()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := fr.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if got, want := calls[0].Timeout, 42*time.Second; got != want {
		t.Errorf("Timeout = %v, want %v", got, want)
	}
	if calls[0].Phase != types.PhaseReview {
		t.Errorf("Phase = %q, want %q", calls[0].Phase, types.PhaseReview)
	}
}

func TestReview_ZeroTimeoutMeansNoTimeout(t *testing.T) {
	fr := runner.NewFakeRunner()
	fr.Responses[types.PhaseReview] = types.RunResult{
		Success: true,
		Text:    "## Decision\n```json\n{\"decision\":\"approve\",\"reasons\":[]}\n```",
	}
	rv := NewReviewer(fr, config.ReviewerConfig{TimeoutSeconds: 0})

	if _, err := rv.Review(context.Background(), newReviewInput()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := fr.Calls()
	if len(calls) != 1 || calls[0].Timeout != 0 {
		t.Errorf("Timeout = %v, want 0", calls[0].Timeout)
	}
}

func TestReview_PromptContainsIssueAndPlan(t *testing.T) {
	fr := runner.NewFakeRunner()
	fr.Responses[types.PhaseReview] = types.RunResult{
		Success: true,
		Text:    "## Decision\n```json\n{\"decision\":\"approve\",\"reasons\":[]}\n```",
	}
	rv := NewReviewer(fr, config.ReviewerConfig{})

	in := newReviewInput()
	if _, err := rv.Review(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := fr.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	prompt := calls[0].Prompt
	for _, want := range []string{
		in.Issue.Title,
		in.Issue.Description,
		in.PlanText,
		"## Decision",
		"You are a code-review agent",
		"You are read-only.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nprompt:\n%s", want, prompt)
		}
	}
	if calls[0].RepoPath != in.RepoPath {
		t.Errorf("RepoPath = %q, want %q", calls[0].RepoPath, in.RepoPath)
	}
	if calls[0].HomePath != in.HomePath {
		t.Errorf("HomePath = %q, want %q", calls[0].HomePath, in.HomePath)
	}
}
