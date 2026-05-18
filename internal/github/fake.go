package github

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/logosc/symphony-go/internal/types"
)

// fakeIssue is the InMemoryFake's view of an issue. It carries an
// IsPullRequest flag so tests can model the issues-API-returns-PRs case
// that ListReadyIssues must filter out.
type fakeIssue struct {
	Issue         types.Issue
	IsPullRequest bool
}

// fakePR is the InMemoryFake's view of a PR.
type fakePR struct {
	PullRequest
	Head string
}

// InMemoryFake is an in-memory Client implementation for tests in other
// packages. It is concurrency-safe.
type InMemoryFake struct {
	mu          sync.Mutex
	owner       string
	repo        string
	issues      map[int]*fakeIssue
	comments    map[int][]types.IssueComment
	permissions map[string]string
	prs         []fakePR
	reactions   map[int64][]string
	prReviews   map[int][]PRReview

	// next monotonic IDs for created comments / PRs / reviews.
	nextCommentID  int64
	nextPRNumber   int
	nextPRReviewID int64
}

// NewInMemoryFake constructs an empty InMemoryFake bound to the given
// fullName ("OWNER/REPO"). It panics on a malformed fullName because it
// is for tests.
func NewInMemoryFake(fullName string) *InMemoryFake {
	owner, repo, err := parseFullName(fullName)
	if err != nil {
		panic(err)
	}
	return &InMemoryFake{
		owner:          owner,
		repo:           repo,
		issues:         make(map[int]*fakeIssue),
		comments:       make(map[int][]types.IssueComment),
		permissions:    make(map[string]string),
		reactions:      make(map[int64][]string),
		prReviews:      make(map[int][]PRReview),
		nextCommentID:  1,
		nextPRNumber:   1000,
		nextPRReviewID: 1,
	}
}

// SeedIssue inserts or replaces an issue. Labels are lowercased and the
// State defaults to "open" if blank. Set isPullRequest to true to model
// a PR that the issues API would surface (so ListReadyIssues filters it
// out).
func (f *InMemoryFake) SeedIssue(issue types.Issue, isPullRequest bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := issue
	if cp.State == "" {
		cp.State = "open"
	}
	low := make([]string, 0, len(cp.Labels))
	for _, l := range cp.Labels {
		low = append(low, strings.ToLower(l))
	}
	cp.Labels = low
	f.issues[cp.Number] = &fakeIssue{Issue: cp, IsPullRequest: isPullRequest}
}

// SeedComment appends a comment to an issue. If c.ID is zero, a new ID
// is assigned. If c.CreatedAt is zero, time.Now() is used.
func (f *InMemoryFake) SeedComment(issueNumber int, c types.IssueComment) types.IssueComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == 0 {
		c.ID = f.nextCommentID
		f.nextCommentID++
	} else if c.ID >= f.nextCommentID {
		f.nextCommentID = c.ID + 1
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	f.comments[issueNumber] = append(f.comments[issueNumber], c)
	return c
}

// SetCollaboratorPermission sets the permission level for a user. Use
// one of "admin", "maintain", "write", "read", "none".
func (f *InMemoryFake) SetCollaboratorPermission(username, permission string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permissions[username] = normalizePermission(permission)
}

// Reactions returns the list of reactions seeded for a comment ID.
// Test helper; not part of Client.
func (f *InMemoryFake) Reactions(commentID int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.reactions[commentID]))
	copy(out, f.reactions[commentID])
	return out
}

// ListReadyIssues implements Client.
func (f *InMemoryFake) ListReadyIssues(_ context.Context, readyLabel string) ([]types.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	wanted := strings.ToLower(readyLabel)
	nums := make([]int, 0, len(f.issues))
	for n := range f.issues {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var out []types.Issue
	for _, n := range nums {
		fi := f.issues[n]
		if fi.IsPullRequest {
			continue
		}
		if fi.Issue.State != "open" {
			continue
		}
		if !containsLabel(fi.Issue.Labels, wanted) {
			continue
		}
		out = append(out, copyIssue(fi.Issue))
	}
	return out, nil
}

// GetIssue implements Client.
func (f *InMemoryFake) GetIssue(_ context.Context, number int) (types.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, ok := f.issues[number]
	if !ok {
		return types.Issue{}, fmt.Errorf("github (fake): issue %d not found", number)
	}
	return copyIssue(fi.Issue), nil
}

// ListIssueComments implements Client.
func (f *InMemoryFake) ListIssueComments(_ context.Context, number int, since time.Time) ([]types.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.comments[number]
	out := make([]types.IssueComment, 0, len(cs))
	for _, c := range cs {
		if !since.IsZero() && c.CreatedAt.Before(since) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ReplaceStateLabel implements Client.
func (f *InMemoryFake) ReplaceStateLabel(_ context.Context, number int, remove []string, add []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, ok := f.issues[number]
	if !ok {
		return fmt.Errorf("github (fake): issue %d not found", number)
	}
	fi.Issue.Labels = computeLabels(fi.Issue.Labels, remove, add)
	return nil
}

// PostIssueComment implements Client.
func (f *InMemoryFake) PostIssueComment(_ context.Context, number int, body string) (types.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.issues[number]; !ok {
		return types.IssueComment{}, fmt.Errorf("github (fake): issue %d not found", number)
	}
	c := types.IssueComment{
		ID:        f.nextCommentID,
		User:      "symphony-go",
		Body:      body,
		CreatedAt: time.Now().UTC(),
	}
	f.nextCommentID++
	f.comments[number] = append(f.comments[number], c)
	return c, nil
}

// EditComment implements Client. It scans every issue's comments slice
// for one whose ID matches commentID and overwrites its Body in place.
// Returns an error if no such comment exists.
func (f *InMemoryFake) EditComment(_ context.Context, commentID int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, cs := range f.comments {
		for i := range cs {
			if cs[i].ID == commentID {
				cs[i].Body = body
				f.comments[n] = cs
				return nil
			}
		}
	}
	return fmt.Errorf("github (fake): comment %d not found", commentID)
}

// GetComment returns the comment with the given ID. Test helper; not
// part of Client.
func (f *InMemoryFake) GetComment(commentID int64) (types.IssueComment, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cs := range f.comments {
		for _, c := range cs {
			if c.ID == commentID {
				return c, true
			}
		}
	}
	return types.IssueComment{}, false
}

// GetCollaboratorPermission implements Client.
func (f *InMemoryFake) GetCollaboratorPermission(_ context.Context, username string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.permissions[username]
	if !ok {
		return "none", nil
	}
	return p, nil
}

// CreateDraftPR implements Client.
func (f *InMemoryFake) CreateDraftPR(_ context.Context, req CreatePRRequest) (PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Head == "" || req.Base == "" {
		return PullRequest{}, errors.New("github (fake): head and base required")
	}
	pr := PullRequest{
		Number: f.nextPRNumber,
		URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", f.owner, f.repo, f.nextPRNumber),
		State:  "open",
	}
	f.nextPRNumber++
	f.prs = append(f.prs, fakePR{PullRequest: pr, Head: req.Head})
	return pr, nil
}

// FindPRsByHead implements Client.
func (f *InMemoryFake) FindPRsByHead(_ context.Context, headBranch string) ([]PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PullRequest
	for _, p := range f.prs {
		if p.Head == headBranch && p.State == "open" {
			out = append(out, p.PullRequest)
		}
	}
	return out, nil
}

// AddReaction implements Client.
func (f *InMemoryFake) AddReaction(_ context.Context, commentID int64, reaction string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions[commentID] = append(f.reactions[commentID], reaction)
	return nil
}

// SeedPRReview appends a review to a PR. If r.ID is zero, a new ID is
// assigned. If r.SubmittedAt is zero, time.Now() is used.
func (f *InMemoryFake) SeedPRReview(prNumber int, r PRReview) PRReview {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ID == 0 {
		r.ID = f.nextPRReviewID
		f.nextPRReviewID++
	} else if r.ID >= f.nextPRReviewID {
		f.nextPRReviewID = r.ID + 1
	}
	if r.SubmittedAt.IsZero() {
		r.SubmittedAt = time.Now().UTC()
	}
	f.prReviews[prNumber] = append(f.prReviews[prNumber], r)
	return r
}

// ListPRReviews implements Client. Returned reviews are sorted by
// SubmittedAt ascending (oldest first).
func (f *InMemoryFake) ListPRReviews(_ context.Context, prNumber int) ([]PRReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rs := f.prReviews[prNumber]
	out := make([]PRReview, len(rs))
	copy(out, rs)
	sort.Slice(out, func(i, j int) bool { return out[i].SubmittedAt.Before(out[j].SubmittedAt) })
	return out, nil
}

// containsLabel does a case-insensitive lookup in a label slice. The
// slice is expected to already be lowercase.
func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func copyIssue(i types.Issue) types.Issue {
	cp := i
	if i.Labels != nil {
		cp.Labels = append([]string(nil), i.Labels...)
	}
	return cp
}

// Compile-time check that InMemoryFake satisfies Client.
var _ Client = (*InMemoryFake)(nil)
