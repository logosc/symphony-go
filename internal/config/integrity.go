package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IntegrityGuard implements the SPEC §2 "config integrity guard": it
// records the SHA-256 of the resolved config.yml at startup and detects
// drift on each tick. It also enforces the invariant that the config file
// MUST NOT live under repo.local_path — that would let the agent edit its
// own permission set.
type IntegrityGuard struct {
	configPath string // absolute, cleaned
	repoPath   string // absolute, cleaned
	initialSum string // hex-encoded sha256 at construction time
}

// NewIntegrityGuard resolves both paths to absolute form, hard-fails if
// configPath is under repoLocalPath, then computes and stores the initial
// SHA-256 of configPath. Returns the guard ready for CheckUnchanged calls.
func NewIntegrityGuard(configPath, repoLocalPath string) (*IntegrityGuard, error) {
	absCfg, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("integrity: resolve config path %q: %w", configPath, err)
	}
	absCfg = filepath.Clean(absCfg)
	absRepo, err := filepath.Abs(repoLocalPath)
	if err != nil {
		return nil, fmt.Errorf("integrity: resolve repo path %q: %w", repoLocalPath, err)
	}
	absRepo = filepath.Clean(absRepo)

	if pathInside(absCfg, absRepo) {
		return nil, fmt.Errorf("integrity: config path %q is under repo path %q; move config out of the repository", absCfg, absRepo)
	}

	sum, err := hashFile(absCfg)
	if err != nil {
		return nil, fmt.Errorf("integrity: hash %q: %w", absCfg, err)
	}
	return &IntegrityGuard{
		configPath: absCfg,
		repoPath:   absRepo,
		initialSum: sum,
	}, nil
}

// CheckUnchanged re-hashes the config file and reports whether it still
// matches the SHA-256 captured at construction. Returns (true, nil) when
// unchanged, (false, nil) when changed, or (false, err) when the file is
// no longer readable.
func (g *IntegrityGuard) CheckUnchanged() (bool, error) {
	if g == nil {
		return false, fmt.Errorf("integrity: nil guard")
	}
	sum, err := hashFile(g.configPath)
	if err != nil {
		return false, fmt.Errorf("integrity: re-hash %q: %w", g.configPath, err)
	}
	return sum == g.initialSum, nil
}

// ConfigPath returns the absolute, cleaned path captured at construction.
func (g *IntegrityGuard) ConfigPath() string { return g.configPath }

// InitialSum returns the hex-encoded SHA-256 captured at construction.
func (g *IntegrityGuard) InitialSum() string { return g.initialSum }

// hashFile reads path and returns its hex-encoded SHA-256.
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// pathInside reports whether child sits at-or-below parent in the
// filesystem hierarchy. Both arguments must already be absolute and
// cleaned. The comparison is purely lexical and does not resolve
// symlinks; matching SPEC's "lexical containment" semantics.
func pathInside(child, parent string) bool {
	if child == parent {
		return true
	}
	sep := string(filepath.Separator)
	parentWithSep := parent
	if !strings.HasSuffix(parentWithSep, sep) {
		parentWithSep += sep
	}
	return strings.HasPrefix(child, parentWithSep)
}
