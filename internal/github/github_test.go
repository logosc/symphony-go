package github

import (
	"context"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v68/github"

	"github.com/logosc/symphony-go/internal/types"
)

func TestParseFullName(t *testing.T) {
	t.Parallel()
	owner, repo, err := parseFullName("acme/widgets")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "acme" || repo != "widgets" {
		t.Fatalf("got %q/%q", owner, repo)
	}
	for _, bad := range []string{"", "acme", "acme/", "/widgets", "acme/widgets/extra"} {
		if _, _, err := parseFullName(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestNewClientRequiresToken(t *testing.T) {
	t.Parallel()
	if _, err := NewClient(context.Background(), "", "acme/widgets"); err == nil {
		t.Fatal("expected empty-token error")
	}
	if _, err := NewClient(context.Background(), "tok", "bogus"); err == nil {
		t.Fatal("expected bad full_name error")
	}
}

func TestComputeLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		current     []string
		remove, add []string
		want        []string
	}{
		{
			name:    "remove-only",
			current: []string{"a", "b", "c"},
			remove:  []string{"b"},
			want:    []string{"a", "c"},
		},
		{
			name:    "add-only",
			current: []string{"a"},
			add:     []string{"b"},
			want:    []string{"a", "b"},
		},
		{
			name:    "remove-and-add",
			current: []string{"symphony:planning", "bug"},
			remove:  []string{"symphony:planning"},
			add:     []string{"symphony:implementing"},
			want:    []string{"bug", "symphony:implementing"},
		},
		{
			name:    "case-insensitive-remove",
			current: []string{"Bug", "Docs"},
			remove:  []string{"bug"},
			add:     []string{"DOCS"}, // duplicate (case-insensitive); should not double
			want:    []string{"docs"},
		},
		{
			name:    "no-change-when-add-already-present",
			current: []string{"a", "b"},
			add:     []string{"a"},
			want:    []string{"a", "b"},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := computeLabels(tc.current, tc.remove, tc.add)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNormalizePermission(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"admin":    "admin",
		"ADMIN":    "admin",
		"maintain": "maintain",
		"write":    "write",
		"read":     "read",
		"triage":   "read",
		"none":     "none",
		"":         "none",
		"weird":    "none",
	}
	for in, want := range cases {
		if got := normalizePermission(in); got != want {
			t.Errorf("normalizePermission(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeIssueLowercasesAndZeroes(t *testing.T) {
	t.Parallel()
	// Missing optional fields should be zero-valued; labels lowercased.
	gi := &gh.Issue{
		Number: gh.Ptr(7),
		Title:  gh.Ptr("Hi"),
		// Body, HTMLURL, State, NodeID, CreatedAt, UpdatedAt all nil.
		Labels: []*gh.Label{
			{Name: gh.Ptr("Symphony:Ready")},
			{Name: gh.Ptr("BUG")},
			nil,
			{}, // nil Name
		},
	}
	got := normalizeIssue(gi)
	if got.Number != 7 || got.Title != "Hi" {
		t.Fatalf("basic fields wrong: %+v", got)
	}
	if got.Description != "" || got.URL != "" || got.State != "" || got.ID != "" {
		t.Errorf("expected zero-value optional fields, got %+v", got)
	}
	if !got.CreatedAt.IsZero() || !got.UpdatedAt.IsZero() {
		t.Errorf("expected zero timestamps, got %+v", got)
	}
	want := []string{"symphony:ready", "bug"}
	if !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Labels, want)
	}
}

func TestNormalizeCommentZeroes(t *testing.T) {
	t.Parallel()
	gc := &gh.IssueComment{
		ID:   gh.Ptr(int64(42)),
		Body: gh.Ptr("hello"),
		// User, CreatedAt nil
	}
	got := normalizeComment(gc)
	if got.ID != 42 || got.Body != "hello" {
		t.Fatalf("basic fields wrong: %+v", got)
	}
	if got.User != "" {
		t.Errorf("expected empty user, got %q", got.User)
	}
	if !got.CreatedAt.IsZero() {
		t.Errorf("expected zero CreatedAt")
	}
}

// ---- InMemoryFake round-trip ----

func newSeededFake(t *testing.T) *InMemoryFake {
	t.Helper()
	f := NewInMemoryFake("acme/widgets")
	f.SeedIssue(types.Issue{
		Number: 1,
		Title:  "fix bug",
		Labels: []string{"Symphony:Ready", "Bug"},
		State:  "open",
	}, false)
	f.SeedIssue(types.Issue{
		Number: 2,
		Title:  "a PR pretending to be an issue",
		Labels: []string{"symphony:ready"},
		State:  "open",
	}, true)
	f.SeedIssue(types.Issue{
		Number: 3,
		Title:  "unrelated issue",
		Labels: []string{"docs"},
		State:  "open",
	}, false)
	f.SetCollaboratorPermission("alice", "WRITE")
	return f
}

func TestFake_ListReadyIssuesSkipsPRs(t *testing.T) {
	t.Parallel()
	f := newSeededFake(t)
	got, err := f.ListReadyIssues(context.Background(), "symphony:ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 ready issue (PR #2 filtered, #3 wrong label), got %d: %+v", len(got), got)
	}
	if got[0].Number != 1 {
		t.Errorf("got issue #%d, want 1", got[0].Number)
	}
	// Labels should be lowercase from seeding.
	if !reflect.DeepEqual(got[0].Labels, []string{"symphony:ready", "bug"}) {
		t.Errorf("labels = %v", got[0].Labels)
	}
}

func TestFake_GetIssue(t *testing.T) {
	t.Parallel()
	f := newSeededFake(t)
	got, err := f.GetIssue(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != 1 || got.Title != "fix bug" {
		t.Errorf("got %+v", got)
	}
	if _, err := f.GetIssue(context.Background(), 999); err == nil {
		t.Error("expected not-found error")
	}
}

func TestFake_PostAndListComments(t *testing.T) {
	t.Parallel()
	f := newSeededFake(t)
	ctx := context.Background()
	c1, err := f.PostIssueComment(ctx, 1, "first")
	if err != nil {
		t.Fatal(err)
	}
	if c1.ID == 0 || c1.Body != "first" {
		t.Errorf("bad comment: %+v", c1)
	}
	// Separate c1, mark, and c2 so the `since` filter (at-or-after, matching
	// the real GitHub API) is unambiguous on machines with coarse clocks.
	time.Sleep(2 * time.Millisecond)
	mark := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)
	c2, err := f.PostIssueComment(ctx, 1, "second")
	if err != nil {
		t.Fatal(err)
	}

	all, err := f.ListIssueComments(ctx, 1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(all))
	}

	since, err := f.ListIssueComments(ctx, 1, mark)
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 1 || since[0].ID != c2.ID {
		t.Errorf("expected only post-mark comment %d, got %+v", c2.ID, since)
	}
}

func TestFake_ReplaceStateLabel(t *testing.T) {
	t.Parallel()
	f := newSeededFake(t)
	ctx := context.Background()
	if err := f.ReplaceStateLabel(ctx, 1, []string{"symphony:ready"}, []string{"symphony:planning"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.GetIssue(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got.Labels)
	want := []string{"bug", "symphony:planning"}
	if !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels after relabel = %v, want %v", got.Labels, want)
	}
	// Confirm "bug" was untouched: only specified labels are removed.
	hasBug := false
	for _, l := range got.Labels {
		if l == "bug" {
			hasBug = true
		}
	}
	if !hasBug {
		t.Error("expected unrelated label 'bug' to remain")
	}
}

func TestFake_GetCollaboratorPermission(t *testing.T) {
	t.Parallel()
	f := newSeededFake(t)
	ctx := context.Background()
	p, err := f.GetCollaboratorPermission(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if p != "write" {
		t.Errorf("alice perm = %q, want write", p)
	}
	p, err = f.GetCollaboratorPermission(ctx, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	if p != "none" {
		t.Errorf("stranger perm = %q, want none", p)
	}
	// Validate it's always one of the canonical values.
	allowed := map[string]bool{"admin": true, "maintain": true, "write": true, "read": true, "none": true}
	for _, in := range []string{"admin", "maintain", "write", "read", "triage", "garbage", ""} {
		f.SetCollaboratorPermission("u", in)
		got, _ := f.GetCollaboratorPermission(ctx, "u")
		if !allowed[got] {
			t.Errorf("permission %q normalized to non-canonical %q", in, got)
		}
	}
}

func TestFake_CreateAndFindPR(t *testing.T) {
	t.Parallel()
	f := newSeededFake(t)
	ctx := context.Background()
	pr, err := f.CreateDraftPR(ctx, CreatePRRequest{
		Title: "[agent] fix bug",
		Body:  "body",
		Head:  "symphony/issue-1-fix-bug",
		Base:  "main",
		Draft: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number == 0 || !strings.Contains(pr.URL, "acme/widgets/pull/") {
		t.Errorf("bad PR: %+v", pr)
	}

	prs, err := f.FindPRsByHead(ctx, "symphony/issue-1-fix-bug")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].Number != pr.Number {
		t.Errorf("FindPRsByHead = %+v", prs)
	}

	none, err := f.FindPRsByHead(ctx, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("expected no PRs for nonexistent head, got %+v", none)
	}

	// Missing head/base should error.
	if _, err := f.CreateDraftPR(ctx, CreatePRRequest{Title: "x"}); err == nil {
		t.Error("expected error for missing head/base")
	}
}

func TestFake_AddReaction(t *testing.T) {
	t.Parallel()
	f := newSeededFake(t)
	ctx := context.Background()
	c, err := f.PostIssueComment(ctx, 1, "approve me")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.AddReaction(ctx, c.ID, "+1"); err != nil {
		t.Fatal(err)
	}
	if err := f.AddReaction(ctx, c.ID, "eyes"); err != nil {
		t.Fatal(err)
	}
	got := f.Reactions(c.ID)
	if !reflect.DeepEqual(got, []string{"+1", "eyes"}) {
		t.Errorf("reactions = %v", got)
	}
}

// TestRealClient_Smoke is skipped unless SYMPHONY_GO_GITHUB_E2E=1 and a
// token + repo are provided. Pure-fake test runs never hit the network.
func TestRealClient_Smoke(t *testing.T) {
	if os.Getenv("SYMPHONY_GO_GITHUB_E2E") != "1" {
		t.Skip("set SYMPHONY_GO_GITHUB_E2E=1 to run real-network smoke")
	}
	token := os.Getenv("GITHUB_TOKEN")
	full := os.Getenv("SYMPHONY_GO_GITHUB_REPO")
	if token == "" || full == "" {
		t.Skip("GITHUB_TOKEN and SYMPHONY_GO_GITHUB_REPO required")
	}
	c, err := NewClient(context.Background(), token, full)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListReadyIssues(context.Background(), "symphony:ready"); err != nil {
		t.Fatalf("list ready: %v", err)
	}
}
