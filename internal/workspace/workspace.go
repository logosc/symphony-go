// Package workspace manages per-issue git worktrees, branch and slug
// derivation, dirty-checks, and hook execution as specified in SPEC §8.
//
// All filesystem operations are performed via os/exec invocations of git;
// the package has no third-party dependencies.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SanitizeSlug applies the rule from SPEC §8: lowercase the title, replace
// any run of characters not matching [a-z0-9._-] with a single "-", trim
// leading and trailing "-", and truncate the result to 60 runes. If the
// resulting slug is empty, the fallback string "issue" is returned.
func SanitizeSlug(title string) string {
	lowered := strings.ToLower(title)

	var b strings.Builder
	b.Grow(len(lowered))
	prevDash := false
	for _, r := range lowered {
		if isSlugRune(r) {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteRune('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")

	// Truncate to 60 runes.
	if n := runeLen(out); n > 60 {
		out = truncateRunes(out, 60)
		out = strings.Trim(out, "-")
	}

	if out == "" {
		return "issue"
	}
	return out
}

func isSlugRune(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	if r == '.' || r == '_' || r == '-' {
		return true
	}
	return false
}

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func truncateRunes(s string, max int) string {
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// BranchName returns the canonical branch name "symphony/issue-{n}-{slug}"
// for the given issue number and (already-sanitized) slug.
func BranchName(issueNum int, slug string) string {
	return fmt.Sprintf("symphony/issue-%d-%s", issueNum, slug)
}

// Layout describes the on-disk directory layout for one per-issue
// worktree, rooted under the configured workspace root.
type Layout struct {
	// Root is <workspaceRoot>/issue-{n}-{slug}.
	Root string
	// RepoPath is Root/repo, the directory passed to "git worktree add".
	RepoPath string
	// HomePath is Root/home, used as HOME for the agent subprocess.
	HomePath string
	// TmpPath is Root/home/tmp, used as TMPDIR for the agent subprocess.
	TmpPath string
}

// LayoutFor returns the Layout for the given workspace root, issue number,
// and slug. It performs no filesystem I/O.
func LayoutFor(workspaceRoot string, issueNum int, slug string) Layout {
	root := filepath.Join(workspaceRoot, fmt.Sprintf("issue-%d-%s", issueNum, slug))
	home := filepath.Join(root, "home")
	return Layout{
		Root:     root,
		RepoPath: filepath.Join(root, "repo"),
		HomePath: home,
		TmpPath:  filepath.Join(home, "tmp"),
	}
}

// Manager creates and inspects per-issue git worktrees rooted at a single
// local clone of the target repository.
type Manager struct {
	localRepoPath string
}

// NewManager returns a Manager that operates against the given local clone.
func NewManager(localRepoPath string) *Manager {
	return &Manager{localRepoPath: localRepoPath}
}

// Create provisions a fresh worktree for one issue. It fetches the base
// branch, refuses to overwrite an existing local or origin branch matching
// "branch", runs "git worktree add -b branch repoPath origin/baseBranch",
// and ensures the home and tmp directories exist. Any failure returns a
// non-nil error and the partial state may need to be cleaned up by the caller.
func (m *Manager) Create(ctx context.Context, layout Layout, baseBranch, branch string) error {
	if m.localRepoPath == "" {
		return errors.New("workspace: empty local repo path")
	}

	// Fetch base branch so origin/<base> is current.
	if err := m.runGit(ctx, "fetch", "origin", baseBranch); err != nil {
		return fmt.Errorf("workspace: fetch %s: %w", baseBranch, err)
	}

	// Reject if branch exists locally.
	if exists, err := m.localBranchExists(ctx, branch); err != nil {
		return fmt.Errorf("workspace: check local branch: %w", err)
	} else if exists {
		return fmt.Errorf("workspace: branch %q already exists locally", branch)
	}

	// Reject if branch exists on origin.
	if exists, err := m.originBranchExists(ctx, branch); err != nil {
		return fmt.Errorf("workspace: check origin branch: %w", err)
	} else if exists {
		return fmt.Errorf("workspace: branch %q already exists on origin", branch)
	}

	// Ensure parent of RepoPath exists; "git worktree add" creates the leaf.
	if err := os.MkdirAll(layout.Root, 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir root: %w", err)
	}

	if err := m.runGit(ctx, "worktree", "add", "-b", branch, layout.RepoPath, "origin/"+baseBranch); err != nil {
		return fmt.Errorf("workspace: worktree add: %w", err)
	}

	if err := os.MkdirAll(layout.HomePath, 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir home: %w", err)
	}
	if err := os.MkdirAll(layout.TmpPath, 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir tmp: %w", err)
	}
	return nil
}

// IsClean reports whether the working tree at repoPath is clean — that is,
// "git -C repoPath status --porcelain" produces no output.
func (m *Manager) IsClean(ctx context.Context, repoPath string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "status", "--porcelain")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("workspace: git status: %w (%s)", err, out.String())
	}
	return out.Len() == 0, nil
}

func (m *Manager) runGit(ctx context.Context, args ...string) error {
	full := append([]string{"-C", m.localRepoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

func (m *Manager) localBranchExists(ctx context.Context, branch string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", m.localRepoPath,
		"show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		// show-ref exits 1 when the ref does not exist.
		if exitErr.ExitCode() == 1 {
			return false, nil
		}
	}
	return false, err
}

func (m *Manager) originBranchExists(ctx context.Context, branch string) (bool, error) {
	// First try the cached remote-tracking ref.
	cmd := exec.CommandContext(ctx, "git", "-C", m.localRepoPath,
		"show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	if err := cmd.Run(); err == nil {
		return true, nil
	}
	// Fall back to ls-remote in case origin doesn't exist or isn't reachable;
	// treat unreachable origin as "does not exist" only when ls-remote itself
	// returns no error. Any error is propagated so callers can decide.
	out, err := exec.CommandContext(ctx, "git", "-C", m.localRepoPath,
		"ls-remote", "--heads", "origin", branch).Output()
	if err != nil {
		// If origin is unreachable we cannot definitively say the branch is
		// absent; surface the error.
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// HookResult is the captured outcome of a single RunHook invocation.
type HookResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Duration time.Duration
}

// RunHook executes script via "bash -lc <script>" with the provided cwd,
// HOME, and environment. The supplied env entirely replaces the process
// environment; HOME is appended (overriding any HOME already in env).
//
// On context cancellation or timeout, the process is sent SIGTERM and
// granted a 5-second grace period before SIGKILL via cmd.WaitDelay.
//
// The returned HookResult is populated even when err is non-nil; err is
// non-nil only for setup failures (e.g., bash not in PATH). Non-zero exit
// codes and timeouts are reported via HookResult fields, not err.
//
// The name argument is used only for error wrapping and diagnostics.
func RunHook(ctx context.Context, name, script, cwd, home string, env []string, timeout time.Duration) (HookResult, error) {
	hookCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		hookCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(hookCtx, "bash", "-lc", script)
	cmd.Dir = cwd

	finalEnv := make([]string, 0, len(env)+2)
	finalEnv = append(finalEnv, env...)
	finalEnv = append(finalEnv, "HOME="+home)
	cmd.Env = finalEnv

	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	res := HookResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: dur,
	}

	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else if runErr != nil {
		res.ExitCode = -1
	}

	// Detect timeout via the hook-scoped context.
	if hookCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}

	if runErr != nil {
		// If timed out or non-zero exit, surface via HookResult, not err.
		if res.TimedOut {
			return res, nil
		}
		if _, ok := runErr.(*exec.ExitError); ok {
			return res, nil
		}
		return res, fmt.Errorf("workspace: hook %q: %w", name, runErr)
	}
	return res, nil
}
