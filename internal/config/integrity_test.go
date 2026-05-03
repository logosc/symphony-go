package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestIntegrityGuardDetectsChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	repoPath := filepath.Join(dir, "repo")
	writeFile(t, cfgPath, "version: 1\n")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	g, err := NewIntegrityGuard(cfgPath, repoPath)
	if err != nil {
		t.Fatalf("NewIntegrityGuard: %v", err)
	}
	ok, err := g.CheckUnchanged()
	if err != nil {
		t.Fatalf("CheckUnchanged: %v", err)
	}
	if !ok {
		t.Fatal("expected unchanged immediately after construction")
	}

	// Modify the config file.
	writeFile(t, cfgPath, "version: 2\n")
	ok, err = g.CheckUnchanged()
	if err != nil {
		t.Fatalf("CheckUnchanged after modify: %v", err)
	}
	if ok {
		t.Fatal("expected change detection")
	}
}

func TestIntegrityGuardRejectsConfigInsideRepo(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	cfgPath := filepath.Join(repoPath, "subdir", "config.yml")
	writeFile(t, cfgPath, "version: 1\n")

	_, err := NewIntegrityGuard(cfgPath, repoPath)
	if err == nil {
		t.Fatal("expected rejection when config lives under repo")
	}
	if !strings.Contains(err.Error(), "under repo") {
		t.Errorf("err = %v; want mention of containment", err)
	}
}

func TestIntegrityGuardRejectsConfigEqualsRepo(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Use the same path for both — pathological but should still fail.
	_, err := NewIntegrityGuard(repoPath, repoPath)
	if err == nil {
		t.Fatal("expected error for config==repo")
	}
}

func TestIntegrityGuardSiblingPathsAllowed(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	cfgPath := filepath.Join(dir, "repo-config", "config.yml") // shares prefix but is a sibling
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	writeFile(t, cfgPath, "ok\n")
	if _, err := NewIntegrityGuard(cfgPath, repoPath); err != nil {
		t.Errorf("sibling path mistakenly rejected: %v", err)
	}
}

func TestIntegrityGuardMissingFile(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := NewIntegrityGuard(filepath.Join(dir, "missing.yml"), repoPath)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestIntegrityGuardCheckAfterDelete(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.yml")
	repoPath := filepath.Join(dir, "repo")
	writeFile(t, cfgPath, "x")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	g, err := NewIntegrityGuard(cfgPath, repoPath)
	if err != nil {
		t.Fatalf("NewIntegrityGuard: %v", err)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := g.CheckUnchanged(); err == nil {
		t.Fatal("expected error when file gone")
	}
}
