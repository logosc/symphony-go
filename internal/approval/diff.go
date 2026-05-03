package approval

import (
	"strings"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

// DriftResult reports the difference between the files actually touched
// by an implementation run and the files claimed in the plan's `## Scope`
// block.
type DriftResult struct {
	// ExtraFiles is the set of paths that were touched but not claimed
	// in scope.FilesTouched (sorted in the order they appeared in the
	// git status input).
	ExtraFiles []string
	// Drifted is true when len(ExtraFiles) > maxDrift.
	Drifted bool
	// AllowedDrift echoes the limit passed to VerifyDiff for caller
	// convenience.
	AllowedDrift int
}

// VerifyDiff compares files actually touched (from `git status --porcelain`
// output) against scope.FilesTouched. Comparison is by exact path.
// Returns Drifted=true iff len(ExtraFiles) > maxDrift.
//
// gitStatus is the raw stdout of `git status --porcelain` (one entry per
// line; the path is the trailing token after the two-character status
// prefix, possibly preceded by a renamed-from " -> " segment that is
// ignored).
func VerifyDiff(gitStatus string, scope types.PlanScope, maxDrift int) DriftResult {
	claimed := make(map[string]struct{}, len(scope.FilesTouched))
	for _, f := range scope.FilesTouched {
		claimed[f] = struct{}{}
	}
	var extra []string
	for _, p := range FilesFromGitStatus(gitStatus) {
		if _, ok := claimed[p]; !ok {
			extra = append(extra, p)
		}
	}
	return DriftResult{
		ExtraFiles:   extra,
		Drifted:      len(extra) > maxDrift,
		AllowedDrift: maxDrift,
	}
}

// FilesFromGitStatus is exposed for callers that already have the parsed
// file list (e.g., from go-git or `git diff --name-only`).
//
// It accepts the raw stdout of `git status --porcelain`. Each non-empty
// line is parsed as: a 2-char XY status, a space, then the path. For
// renames (`R  old -> new`) the post-rename path is returned. Quoted
// paths emitted with `core.quotePath=true` are not unquoted; callers
// that need that behavior should normalize upstream.
func FilesFromGitStatus(gitStatus string) []string {
	var out []string
	for _, raw := range strings.Split(gitStatus, "\n") {
		line := strings.TrimRight(raw, "\r")
		if len(line) == 0 {
			continue
		}
		// Porcelain v1: first 2 columns are status XY, then a space,
		// then the path. Strip the leading status prefix if present;
		// otherwise treat the whole line as a path (forgiving).
		var rest string
		if len(line) >= 3 && line[2] == ' ' {
			rest = line[3:]
		} else {
			rest = strings.TrimLeft(line, " ")
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue
		}
		if idx := strings.Index(rest, " -> "); idx >= 0 {
			rest = strings.TrimSpace(rest[idx+len(" -> "):])
		}
		if rest != "" {
			out = append(out, rest)
		}
	}
	return out
}
