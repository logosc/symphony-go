package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

func TestLoadWorkflow(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := "Hello {{ issue.title }}\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadWorkflow(p)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if got != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestLoadWorkflowMissing(t *testing.T) {
	_, err := LoadWorkflow(filepath.Join(t.TempDir(), "WORKFLOW.md"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRenderPromptAllBindings(t *testing.T) {
	tpl := `Title: {{ issue.title }}
Description: {{ issue.description }}
URL: {{ issue.url }}
Number: {{ issue.number }}
Labels: {{ issue.labels }}
`
	issue := types.Issue{
		Number:      42,
		Title:       "Fix the thing",
		Description: "It is broken.",
		URL:         "https://example.com/issues/42",
		Labels:      []string{"bug", "p0"},
	}
	got, err := RenderPrompt(tpl, issue, 1)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, want := range []string{"Fix the thing", "It is broken.", "https://example.com/issues/42", "42", "bug, p0"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderPromptEmptyIssue(t *testing.T) {
	tpl := "T:{{ issue.title }};L:{{ issue.labels }};N:{{ issue.number }}"
	got, err := RenderPrompt(tpl, types.Issue{}, 0)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if got != "T:;L:;N:0" {
		t.Errorf("got %q", got)
	}
}

func TestRenderPromptSpecialCharsInTitle(t *testing.T) {
	tpl := "{{ issue.title }}"
	issue := types.Issue{Title: "weird `{{ injection }}` & < > $VAR ✨"}
	got, err := RenderPrompt(tpl, issue, 0)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if got != issue.Title {
		t.Errorf("got %q; want %q", got, issue.Title)
	}
}

func TestRenderPromptUnknownBindingErrors(t *testing.T) {
	tpl := "Hello {{ issue.foo }}"
	_, err := RenderPrompt(tpl, types.Issue{}, 0)
	if err == nil {
		t.Fatal("expected error for unknown binding")
	}
	if !strings.Contains(err.Error(), "issue.foo") {
		t.Errorf("err = %v; want mention of issue.foo", err)
	}
}

func TestRenderPromptSubsetOfBindingsOK(t *testing.T) {
	// Templates may legally use only some of the bindings.
	tpl := "Just title: {{ issue.title }}"
	got, err := RenderPrompt(tpl, types.Issue{Title: "x"}, 0)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if got != "Just title: x" {
		t.Errorf("got %q", got)
	}
}
