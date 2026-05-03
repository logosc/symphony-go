package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
)

// subscriptionAuthPaths is the list of HOME-relative directories that
// claude/codex CLIs typically use to store authenticated subscription
// state. Each one that exists in the user's real HOME is symlinked into
// the agent's isolated HOME so the subprocess can read its existing
// auth without us copying credentials.
//
// Adjust when adding support for a new CLI's auth location.
var subscriptionAuthPaths = []string{
	".claude",
	".codex",
	".config/claude",
	".config/codex",
}

// seedSubscriptionAuth symlinks the user's real-HOME subscription-auth
// directories into agentHome. Best-effort and idempotent: missing source
// paths are silently skipped, and existing symlinks/files at the
// destination are replaced.
//
// This bridges symphony-go's HOME isolation with claude/codex's
// subscription-mode auth, which lives in the user's normal HOME.
// API-key mode (allowlisted env var) does not need this — the env var is
// passed through `BuildAgentEnv` instead.
func seedSubscriptionAuth(agentHome string) error {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("user home: %w", err)
	}
	for _, rel := range subscriptionAuthPaths {
		src := filepath.Join(realHome, rel)
		info, err := os.Stat(src)
		if err != nil || !info.IsDir() {
			continue
		}
		dst := filepath.Join(agentHome, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if _, err := os.Lstat(dst); err == nil {
			// Best-effort replace; if dst is a non-empty real dir we
			// can't unlink, fall through and skip.
			if err := os.Remove(dst); err != nil {
				continue
			}
		}
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", dst, src, err)
		}
	}
	return nil
}
