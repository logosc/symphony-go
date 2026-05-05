package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// loadFixture reads a fixture file from
// internal/runner/testdata/parser_fixtures/. Uses os.ReadFile rather
// than testdata-via-embed so adding a fixture is just dropping a file
// — no Go-side change required.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "parser_fixtures", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

// TestParseCodexExec_FlatAgentMessage: the original `item.completed`
// shape with `item_type` and `text` at the top level. Pre-CLI-version
// drift form. Must produce the agent_message text and a "completed"
// terminal.
func TestParseCodexExec_FlatAgentMessage(t *testing.T) {
	got := ParseCodexExec(loadFixture(t, "codex_exec_item_completed_flat.jsonl"))
	if got.FinalText != "Hello from a flat agent_message event." {
		t.Errorf("FinalText = %q", got.FinalText)
	}
	if got.Stats.AssistantMessages != 1 {
		t.Errorf("AssistantMessages = %d, want 1", got.Stats.AssistantMessages)
	}
	if got.Stats.LastTerminal != "completed" {
		t.Errorf("LastTerminal = %q, want completed", got.Stats.LastTerminal)
	}
	if got.Stats.MalformedFrames != 0 {
		t.Errorf("MalformedFrames = %d, want 0", got.Stats.MalformedFrames)
	}
}

// TestParseCodexExec_NestedAgentMessage: post-drift shape where
// `item_type`/`text` moved under a nested `item` object. Regression
// for the bug fixed in commit 6456809 — runner saw success but lost
// the visible text.
func TestParseCodexExec_NestedAgentMessage(t *testing.T) {
	got := ParseCodexExec(loadFixture(t, "codex_exec_item_completed_nested.jsonl"))
	if got.FinalText != "Hello from a nested agent_message event." {
		t.Errorf("FinalText = %q", got.FinalText)
	}
	if got.Stats.AssistantMessages != 1 {
		t.Errorf("AssistantMessages = %d, want 1", got.Stats.AssistantMessages)
	}
	if got.Stats.LastTerminal != "completed" {
		t.Errorf("LastTerminal = %q", got.Stats.LastTerminal)
	}
}

// TestParseCodexExec_TurnFailed: a `turn.failed` terminal must be
// classified as such, even when prior assistant text was emitted.
// The orchestrator decides whether partial text is usable; the parser
// only reports what it saw.
func TestParseCodexExec_TurnFailed(t *testing.T) {
	got := ParseCodexExec(loadFixture(t, "codex_exec_turn_failed.jsonl"))
	if got.Stats.LastTerminal != "failed" {
		t.Errorf("LastTerminal = %q, want failed", got.Stats.LastTerminal)
	}
	// Partial text is still extracted — the orchestrator can use it
	// in the failure diagnostic via summarizeRunFailure.
	if got.FinalText == "" {
		t.Errorf("expected partial assistant text, got empty FinalText")
	}
}

// TestParseCodexExec_MalformedLineCounted: a non-JSON line in the
// middle of an otherwise-valid stream must be counted in
// Stats.MalformedFrames and otherwise tolerated. Dropping a stray
// line shouldn't fail an otherwise-good plan, but it should be
// visible in diagnostics.
func TestParseCodexExec_MalformedLineCounted(t *testing.T) {
	got := ParseCodexExec(loadFixture(t, "codex_exec_malformed_then_terminal.jsonl"))
	if got.Stats.MalformedFrames != 1 {
		t.Errorf("MalformedFrames = %d, want 1", got.Stats.MalformedFrames)
	}
	if got.Stats.LastTerminal != "completed" {
		t.Errorf("LastTerminal = %q, want completed (parser must recover)", got.Stats.LastTerminal)
	}
	if got.FinalText != "Recovered after a bogus line." {
		t.Errorf("FinalText = %q", got.FinalText)
	}
}

// TestParseCodexExec_PreferFlatOverNested: when both shapes are
// populated on the same `item.completed` event (pathological but
// possible during a CLI version cross-over), the flat fields win for
// back-compat. Tested in-line because the situation is unlikely to
// appear in real fixtures.
func TestParseCodexExec_PreferFlatOverNested(t *testing.T) {
	body := []byte(`{"type":"item.completed","item_type":"agent_message","text":"flat-wins","item":{"type":"agent_message","text":"nested-loses"}}
{"type":"turn.completed"}
`)
	got := ParseCodexExec(body)
	if got.FinalText != "flat-wins" {
		t.Errorf("FinalText = %q, want flat-wins (back-compat: top-level fields preferred)", got.FinalText)
	}
}

// TestParseCodexExec_EmptyBody: zero-input must not panic and must
// produce an empty summary.
func TestParseCodexExec_EmptyBody(t *testing.T) {
	got := ParseCodexExec(nil)
	if got.Stats.FramesTotal != 0 || got.FinalText != "" {
		t.Errorf("expected empty summary, got %+v", got.Stats)
	}
}
