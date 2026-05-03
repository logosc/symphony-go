package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeedSubscriptionAuth_SymlinksWhenSourceExists(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	src := filepath.Join(fakeHome, ".claude")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := seedSubscriptionAuth(agentHome); err != nil {
		t.Fatalf("seed: %v", err)
	}

	link := filepath.Join(agentHome, ".claude")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("not a symlink: mode=%v", info.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != src {
		t.Fatalf("link target = %q, want %q", target, src)
	}
}

func TestSeedSubscriptionAuth_IdempotentAndSkipsMissing(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// No source dirs.

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := seedSubscriptionAuth(agentHome); err != nil {
			t.Fatalf("seed call %d: %v", i, err)
		}
	}
	// Nothing should have been created (no source dirs to mirror).
	entries, err := os.ReadDir(agentHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected agent-home empty, got %v", entries)
	}
}

func TestSeedSubscriptionAuth_ReplacesExistingSymlink(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	src := filepath.Join(fakeHome, ".codex")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(agentHome, ".codex")
	if err := os.Symlink("/nonexistent/stale", stale); err != nil {
		t.Fatal(err)
	}

	if err := seedSubscriptionAuth(agentHome); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target, err := os.Readlink(stale)
	if err != nil {
		t.Fatal(err)
	}
	if target != src {
		t.Fatalf("stale link not replaced; target=%q want %q", target, src)
	}
}

func TestSeedSubscriptionAuth_HandlesXDGConfigPath(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	src := filepath.Join(fakeHome, ".config", "claude")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := seedSubscriptionAuth(agentHome); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(agentHome, ".config", "claude")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", link, err)
	}
	if target != src {
		t.Fatalf("target=%q want %q", target, src)
	}
}
