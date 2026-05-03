package state

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	s, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, root
}

func sampleJob(n int) *types.Job {
	return &types.Job{
		IssueNumber:   n,
		Repo:          "OWNER/REPO",
		Status:        types.StatusAwaitingApproval,
		WorkspaceRoot: ".minisymphony/wt/issue-" + itoa(n) + "-x",
		RepoPath:      ".minisymphony/wt/issue-" + itoa(n) + "-x/repo",
		Branch:        "symphony/issue-" + itoa(n) + "-x",
		PlanCommentID: 12345,
		PlanText:      "do the thing",
		PlanScope: &types.PlanScope{
			FilesTouched:          []string{"a.go", "b.go"},
			EstimatedLinesAdded:   50,
			EstimatedLinesRemoved: 10,
			RiskSummary:           "low",
		},
		ApprovalPath: types.ApprovalPathRules,
		Attempt:      1,
		UpdatedAt:    time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC),
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestNewStoreCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "nested", "state")
	if _, err := NewStore(root); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, "jobs")); err != nil || !fi.IsDir() {
		t.Fatalf("jobs dir missing: stat err=%v fi=%v", err, fi)
	}
	// Lock parent dir must also exist.
	if fi, err := os.Stat(filepath.Dir(root)); err != nil || !fi.IsDir() {
		t.Fatalf("lock parent dir missing: stat err=%v fi=%v", err, fi)
	}
}

func TestNewStoreEmptyRoot(t *testing.T) {
	if _, err := NewStore(""); err == nil {
		t.Fatal("expected error for empty rootDir")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	cases := []*types.Job{
		sampleJob(1),
		sampleJob(42),
		{
			IssueNumber: 7,
			Repo:        "x/y",
			Status:      types.StatusPlanning,
			Attempt:     2,
			UpdatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
	}
	for _, j := range cases {
		j := j
		t.Run(itoa(j.IssueNumber), func(t *testing.T) {
			if err := s.Save(j); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, err := s.Load(j.IssueNumber)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(j, got) {
				t.Fatalf("round-trip mismatch:\nwant=%+v\n got=%+v", j, got)
			}
		})
	}
}

func TestSaveRejectsNilAndZero(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Save(nil); err == nil {
		t.Error("expected error on nil job")
	}
	if err := s.Save(&types.Job{IssueNumber: 0}); err == nil {
		t.Error("expected error on zero IssueNumber")
	}
}

func TestSaveAtomicOverwritesStaleTmp(t *testing.T) {
	s, _ := newTestStore(t)

	// Simulate a crash that left a stale .tmp file behind.
	stalePath := filepath.Join(s.jobsDir, "1.json.tmp")
	if err := os.WriteFile(stalePath, []byte("garbage-from-prior-crash"), 0o644); err != nil {
		t.Fatalf("seed stale tmp: %v", err)
	}

	j := sampleJob(1)
	if err := s.Save(j); err != nil {
		t.Fatalf("Save after stale tmp: %v", err)
	}

	// No .tmp should remain.
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale .tmp not cleaned up: err=%v", err)
	}

	// Final state must reflect the new job, not the garbage.
	got, err := s.Load(1)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(j, got) {
		t.Fatalf("final state mismatch:\nwant=%+v\n got=%+v", j, got)
	}
}

func TestLoadMissingReturnsErrNotExist(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.Load(999)
	if err == nil {
		t.Fatal("expected error on missing job")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error does not wrap os.ErrNotExist: %v", err)
	}
}

func TestList(t *testing.T) {
	s, _ := newTestStore(t)

	// Empty directory: empty list, no error.
	got, err := s.List()
	if err != nil {
		t.Fatalf("List on empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %d", len(got))
	}

	// Save in non-sorted order; expect ascending.
	for _, n := range []int{42, 1, 10, 7} {
		if err := s.Save(sampleJob(n)); err != nil {
			t.Fatalf("Save %d: %v", n, err)
		}
	}

	// Drop in noise files that should be ignored.
	noise := []string{"README", "notes.txt", "foo.json", "1.json.tmp"}
	for _, name := range noise {
		if err := os.WriteFile(filepath.Join(s.jobsDir, name), []byte("noise"), 0o644); err != nil {
			t.Fatalf("seed noise %q: %v", name, err)
		}
	}

	jobs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []int{1, 7, 10, 42}
	if len(jobs) != len(want) {
		t.Fatalf("len mismatch: want %d, got %d (%+v)", len(want), len(jobs), jobs)
	}
	for i, n := range want {
		if jobs[i].IssueNumber != n {
			t.Fatalf("position %d: want issue %d, got %d", i, n, jobs[i].IssueNumber)
		}
	}
}

func TestDeleteMissingIsNil(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Delete(123); err != nil {
		t.Fatalf("Delete on missing: %v", err)
	}
}

func TestDeleteThenLoadReturnsNotExist(t *testing.T) {
	s, _ := newTestStore(t)
	j := sampleJob(5)
	if err := s.Save(j); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := s.Load(5); err != nil {
		t.Fatalf("Load before delete: %v", err)
	}
	if err := s.Delete(5); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := s.Load(5)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist after delete, got %v", err)
	}
	// Double-delete is also fine.
	if err := s.Delete(5); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestAcquireLockBasic(t *testing.T) {
	s, root := newTestStore(t)
	rel, err := s.AcquireLock()
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	// Lock file should exist next to state/.
	lockPath := filepath.Join(filepath.Dir(root), "lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Double release is a no-op.
	if err := rel(); err != nil {
		t.Fatalf("second release: %v", err)
	}

	// After release, we can re-acquire.
	rel2, err := s.AcquireLock()
	if err != nil {
		t.Fatalf("re-AcquireLock: %v", err)
	}
	_ = rel2()
}

func TestAcquireLockContention(t *testing.T) {
	// Two Stores pointing at the same root simulate two processes —
	// flock is per-open-file-description, so this exercises real
	// contention even within one process.
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	a, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore a: %v", err)
	}
	b, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore b: %v", err)
	}

	// Hold the lock from a goroutine, signal when held.
	held := make(chan struct{})
	releaseSignal := make(chan struct{})
	releasedAck := make(chan error, 1)
	go func() {
		rel, err := a.AcquireLock()
		if err != nil {
			releasedAck <- err
			close(held)
			return
		}
		close(held)
		<-releaseSignal
		releasedAck <- rel()
	}()

	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("first AcquireLock never returned")
	}

	// Second acquisition must fail quickly (non-blocking flock).
	done := make(chan error, 1)
	go func() {
		_, err := b.AcquireLock()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected contention error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second AcquireLock blocked instead of failing fast")
	}

	// Release the holder; acquisition should now succeed.
	close(releaseSignal)
	if err := <-releasedAck; err != nil {
		t.Fatalf("holder release: %v", err)
	}
	rel2, err := b.AcquireLock()
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	_ = rel2()
}
