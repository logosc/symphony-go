// Package state provides on-disk JSON job state persistence and a
// process-wide flock for the minisymphony orchestrator.
//
// State layout under rootDir (typically ".minisymphony/state"):
//
//	<rootDir>/jobs/{issue_number}.json   // one file per job
//	<rootDir>/../lock                    // sibling flock file (".minisymphony/lock")
//
// All writes are atomic: write to a sibling .tmp file, fsync, rename.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

// Store is a thread-safe (within a process) handle to the on-disk job
// state directory. Cross-process exclusion is provided by AcquireLock.
type Store struct {
	rootDir string // absolute or relative path to the state root (e.g. ".minisymphony/state")
	jobsDir string // <rootDir>/jobs
	lockDir string // <rootDir>/.. (where the "lock" file lives)

	// mu serializes Save/Delete/Load within a single process. Cross-process
	// safety is via AcquireLock (flock).
	mu sync.Mutex
}

// NewStore opens (and creates if needed) the state directory layout.
// rootDir is typically ".minisymphony/state". The jobs/ subdirectory and
// the parent directory (used for the flock file) are created with 0o755.
func NewStore(rootDir string) (*Store, error) {
	if rootDir == "" {
		return nil, errors.New("state: rootDir must not be empty")
	}
	jobsDir := filepath.Join(rootDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		return nil, fmt.Errorf("state: create jobs dir: %w", err)
	}
	lockDir := filepath.Dir(rootDir)
	if lockDir == "" || lockDir == "." {
		// rootDir was a single component like "state"; lock alongside it.
		lockDir = "."
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("state: create lock parent dir: %w", err)
	}
	return &Store{
		rootDir: rootDir,
		jobsDir: jobsDir,
		lockDir: lockDir,
	}, nil
}

// AcquireLock takes an exclusive non-blocking flock on
// <rootDir>/../lock. On contention it returns an error wrapping the
// underlying errno (typically EWOULDBLOCK).
//
// The returned release function unlocks and closes the lock file. It is
// safe to call release more than once; subsequent calls return nil.
func (s *Store) AcquireLock() (func() error, error) {
	lockPath := filepath.Join(s.lockDir, "lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("state: open lock file %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("state: another minisymphony process holds %q: %w", lockPath, err)
	}
	var once sync.Once
	release := func() error {
		var rerr error
		once.Do(func() {
			// Best-effort unlock; even if it fails, we still want to close.
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
				rerr = err
			}
			if err := f.Close(); err != nil && rerr == nil {
				rerr = err
			}
		})
		return rerr
	}
	return release, nil
}

// Save atomically writes j to <rootDir>/jobs/{IssueNumber}.json. The
// write goes to a sibling .tmp file, is fsync'd, then renamed into
// place. Any pre-existing stale .tmp file from a prior crash is
// overwritten.
func (s *Store) Save(j *types.Job) error {
	if j == nil {
		return errors.New("state: Save: nil job")
	}
	if j.IssueNumber <= 0 {
		return fmt.Errorf("state: Save: invalid IssueNumber %d", j.IssueNumber)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	final := s.pathFor(j.IssueNumber)
	tmp := final + ".tmp"

	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal job %d: %w", j.IssueNumber, err)
	}
	// Append a trailing newline for friendliness with text tools.
	data = append(data, '\n')

	// O_TRUNC ensures any stale .tmp left by a prior crash is overwritten.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("state: open tmp %q: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("state: write tmp %q: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("state: fsync tmp %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: close tmp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: rename %q -> %q: %w", tmp, final, err)
	}
	// Best-effort directory fsync so the rename is durable. Ignore errors
	// on platforms / filesystems that don't support it.
	if dir, derr := os.Open(s.jobsDir); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Load reads and decodes the job for issueNum. If no job file exists,
// Load returns an error wrapping os.ErrNotExist.
func (s *Store) Load(issueNum int) (*types.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.pathFor(issueNum)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("state: job %d: %w", issueNum, os.ErrNotExist)
		}
		return nil, fmt.Errorf("state: read job %d: %w", issueNum, err)
	}
	var j types.Job
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("state: decode job %d: %w", issueNum, err)
	}
	return &j, nil
}

// List returns every job in jobsDir, sorted by IssueNumber ascending.
// Files that don't match the {N}.json pattern, and stray .tmp files, are
// silently skipped. A decode error on any single file aborts List.
func (s *Store) List() ([]*types.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.jobsDir)
	if err != nil {
		return nil, fmt.Errorf("state: read jobs dir: %w", err)
	}
	jobs := make([]*types.Job, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		base := strings.TrimSuffix(name, ".json")
		if _, err := strconv.Atoi(base); err != nil {
			// Not a {issue_number}.json file; skip.
			continue
		}
		path := filepath.Join(s.jobsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("state: read %q: %w", path, err)
		}
		var j types.Job
		if err := json.Unmarshal(data, &j); err != nil {
			return nil, fmt.Errorf("state: decode %q: %w", path, err)
		}
		jobs = append(jobs, &j)
	}
	sort.Slice(jobs, func(i, k int) bool {
		return jobs[i].IssueNumber < jobs[k].IssueNumber
	})
	return jobs, nil
}

// Delete removes the job file for issueNum. A missing file is not an
// error (best-effort semantics).
func (s *Store) Delete(issueNum int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.pathFor(issueNum)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("state: delete job %d: %w", issueNum, err)
	}
	return nil
}

// pathFor returns the canonical on-disk path for a given issue number's
// job file. Caller must hold s.mu when using the path for I/O.
func (s *Store) pathFor(issueNum int) string {
	return filepath.Join(s.jobsDir, strconv.Itoa(issueNum)+".json")
}
