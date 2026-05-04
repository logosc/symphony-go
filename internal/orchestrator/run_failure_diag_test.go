package orchestrator

import (
	"strings"
	"testing"

	"github.com/logosc/symphony-go/internal/types"
)

// TestSummarizeRunFailure: prefers Text over Stderr, falls back to a
// placeholder when both are empty, and truncates long outputs by keeping
// the tail (where the actual error typically lives).
func TestSummarizeRunFailure(t *testing.T) {
	// Real Claude API-error case from issue #1: error string lives in
	// RunResult.Text.
	t.Run("text_preferred", func(t *testing.T) {
		got := summarizeRunFailure(types.RunResult{
			Text:   `API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"model: String should have at least 1 character"}}`,
			Stderr: "irrelevant stderr",
		})
		if !strings.Contains(got, "API Error: 400") {
			t.Errorf("expected text content, got %q", got)
		}
		if strings.Contains(got, "irrelevant") {
			t.Errorf("stderr leaked despite text being present: %q", got)
		}
	})

	t.Run("stderr_fallback", func(t *testing.T) {
		got := summarizeRunFailure(types.RunResult{Stderr: "panic: nil pointer\nstack trace..."})
		if !strings.Contains(got, "panic: nil pointer") {
			t.Errorf("expected stderr content, got %q", got)
		}
	})

	t.Run("empty_fallback", func(t *testing.T) {
		got := summarizeRunFailure(types.RunResult{})
		if !strings.Contains(got, "no output") {
			t.Errorf("expected placeholder, got %q", got)
		}
	})

	t.Run("truncation_keeps_tail", func(t *testing.T) {
		// 5KB body — only the tail (where the error usually is) should survive.
		head := strings.Repeat("padding-text-that-should-be-dropped\n", 100)
		tail := "FINAL ERROR LINE THAT MATTERS"
		got := summarizeRunFailure(types.RunResult{Text: head + tail})
		if !strings.Contains(got, tail) {
			t.Errorf("tail dropped: %q", got[:200])
		}
		if strings.Contains(got, "padding-text-that-should-be-dropped") && len(got) > 2000 {
			t.Errorf("not truncated: len=%d", len(got))
		}
	})
}
