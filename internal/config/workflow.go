package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

// LoadWorkflow reads the WORKFLOW.md prompt template at path verbatim.
// No expansion happens here — bindings are substituted later by
// RenderPrompt for a specific issue.
func LoadWorkflow(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("workflow: resolve path %q: %w", path, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("workflow: read %q: %w", abs, err)
	}
	return string(data), nil
}

// bindingRe matches `{{ binding }}` substitutions with optional whitespace
// around the binding name. The binding form must be `issue.<field>`.
var bindingRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.]+)\s*\}\}`)

// RenderPrompt performs literal substitution of WORKFLOW.md bindings for
// a specific issue and attempt. Supported bindings:
//
//	{{ issue.title }}, {{ issue.description }}, {{ issue.url }},
//	{{ issue.number }}, {{ issue.labels }}
//
// Unknown bindings (e.g. {{ issue.foo }}) cause RenderPrompt to return an
// error. Templates may legitimately reference a subset of the supported
// bindings — missing bindings in the template are not an error.
//
// The attempt counter is currently used only to validate the call shape;
// it may be exposed as a binding in a future revision.
func RenderPrompt(template string, issue types.Issue, attempt int) (string, error) {
	_ = attempt // reserved for future {{ run.attempt }} binding
	var rerr error
	out := bindingRe.ReplaceAllStringFunc(template, func(match string) string {
		groups := bindingRe.FindStringSubmatch(match)
		key := groups[1]
		val, ok := resolveBinding(key, issue)
		if !ok {
			if rerr == nil {
				rerr = fmt.Errorf("workflow: unknown binding %q", key)
			}
			return match
		}
		return val
	})
	if rerr != nil {
		return "", rerr
	}
	return out, nil
}

// resolveBinding returns the value of a single supported binding, or
// (\"\", false) when the binding name is unknown.
func resolveBinding(key string, issue types.Issue) (string, bool) {
	switch key {
	case "issue.title":
		return issue.Title, true
	case "issue.description":
		return issue.Description, true
	case "issue.url":
		return issue.URL, true
	case "issue.number":
		return fmt.Sprintf("%d", issue.Number), true
	case "issue.labels":
		return strings.Join(issue.Labels, ", "), true
	default:
		return "", false
	}
}
