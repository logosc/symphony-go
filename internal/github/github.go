// Package github wraps the go-github client for the small set of
// operations symphony-go needs: listing ready issues, atomic label
// transitions, comments, collaborator permission checks, and draft PR
// creation. It exposes a Client interface and an InMemoryFake for tests.
package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"

	"github.com/logosc/symphony-go/internal/types"
)

// CreatePRRequest is the input to CreateDraftPR.
type CreatePRRequest struct {
	Title string
	Body  string
	Head  string
	Base  string
	Draft bool
}

// PullRequest is a normalized subset of a GitHub PR returned by Client.
type PullRequest struct {
	Number int
	URL    string
	State  string
}

// PRReview is a normalized PR review returned by Client.ListPRReviews.
// State is one of "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED",
// or "PENDING" (mirroring GitHub's review states).
type PRReview struct {
	ID          int64
	User        string
	State       string
	Body        string
	SubmittedAt time.Time
}

// Client is the minimal GitHub surface used by symphony-go.
type Client interface {
	// ListReadyIssues returns open issues carrying readyLabel. Pull
	// requests (which the issues API folds into the same listing) are
	// excluded.
	ListReadyIssues(ctx context.Context, readyLabel string) ([]types.Issue, error)
	// GetIssue fetches a single issue by number.
	GetIssue(ctx context.Context, number int) (types.Issue, error)
	// ListIssueComments lists comments on an issue posted at or after since.
	// Pass time.Time{} to list all.
	ListIssueComments(ctx context.Context, number int, since time.Time) ([]types.IssueComment, error)
	// ReplaceStateLabel atomically rewrites the labels on an issue:
	// it GETs the current labels, removes any in `remove` and adds any
	// in `add`, then PATCHes the issue with the resulting full set.
	ReplaceStateLabel(ctx context.Context, number int, remove []string, add []string) error
	// PostIssueComment creates a comment on an issue and returns it.
	PostIssueComment(ctx context.Context, number int, body string) (types.IssueComment, error)
	// EditComment overwrites the body of an existing issue comment.
	EditComment(ctx context.Context, commentID int64, body string) error
	// GetCollaboratorPermission returns one of "admin", "maintain",
	// "write", "read", or "none".
	GetCollaboratorPermission(ctx context.Context, username string) (string, error)
	// CreateDraftPR creates a draft PR (req.Draft is honored, defaulting
	// to true is the caller's responsibility).
	CreateDraftPR(ctx context.Context, req CreatePRRequest) (PullRequest, error)
	// FindPRsByHead lists open PRs whose head branch matches the given
	// branch name (in the repo's owner namespace).
	FindPRsByHead(ctx context.Context, headBranch string) ([]PullRequest, error)
	// ListPRReviews returns every review submitted on a PR, oldest first.
	ListPRReviews(ctx context.Context, prNumber int) ([]PRReview, error)
	// AddReaction adds a reaction (e.g. "+1", "-1", "eyes") to an issue
	// comment.
	AddReaction(ctx context.Context, commentID int64, reaction string) error
}

// realClient is the production go-github-backed Client.
type realClient struct {
	c     *gh.Client
	owner string
	repo  string
}

// NewClient constructs a Client backed by the GitHub REST API. fullName
// must be in the form "OWNER/REPO".
func NewClient(ctx context.Context, token, fullName string) (Client, error) {
	owner, repo, err := parseFullName(fullName)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("github: empty token")
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(ctx, ts)
	return &realClient{
		c:     gh.NewClient(httpClient),
		owner: owner,
		repo:  repo,
	}, nil
}

func parseFullName(fullName string) (string, string, error) {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("github: invalid full_name %q, expected OWNER/REPO", fullName)
	}
	return parts[0], parts[1], nil
}

func (r *realClient) ListReadyIssues(ctx context.Context, readyLabel string) ([]types.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{readyLabel},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []types.Issue
	for {
		issues, resp, err := r.c.Issues.ListByRepo(ctx, r.owner, r.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list ready issues: %w", err)
		}
		for _, gi := range issues {
			if gi == nil {
				continue
			}
			// Skip PRs: the issues endpoint returns them with a
			// non-nil PullRequestLinks field.
			if gi.PullRequestLinks != nil {
				continue
			}
			out = append(out, normalizeIssue(gi))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func (r *realClient) GetIssue(ctx context.Context, number int) (types.Issue, error) {
	gi, _, err := r.c.Issues.Get(ctx, r.owner, r.repo, number)
	if err != nil {
		return types.Issue{}, fmt.Errorf("github: get issue %d: %w", number, err)
	}
	return normalizeIssue(gi), nil
}

func (r *realClient) ListIssueComments(ctx context.Context, number int, since time.Time) ([]types.IssueComment, error) {
	opts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	if !since.IsZero() {
		s := since
		opts.Since = &s
	}
	var out []types.IssueComment
	for {
		comments, resp, err := r.c.Issues.ListComments(ctx, r.owner, r.repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list comments on #%d: %w", number, err)
		}
		for _, gc := range comments {
			if gc == nil {
				continue
			}
			out = append(out, normalizeComment(gc))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func (r *realClient) ReplaceStateLabel(ctx context.Context, number int, remove []string, add []string) error {
	gi, _, err := r.c.Issues.Get(ctx, r.owner, r.repo, number)
	if err != nil {
		return fmt.Errorf("github: get issue %d for relabel: %w", number, err)
	}
	current := make([]string, 0, len(gi.Labels))
	for _, l := range gi.Labels {
		if l == nil || l.Name == nil {
			continue
		}
		current = append(current, strings.ToLower(*l.Name))
	}
	next := computeLabels(current, remove, add)
	req := &gh.IssueRequest{Labels: &next}
	if _, _, err := r.c.Issues.Edit(ctx, r.owner, r.repo, number, req); err != nil {
		return fmt.Errorf("github: edit labels on #%d: %w", number, err)
	}
	return nil
}

// computeLabels returns the deduplicated set of `current` minus `remove`
// plus `add`, preserving order: kept current first (in original order),
// then newly added labels not already present. All comparisons are
// case-insensitive; results are lowercased.
func computeLabels(current, remove, add []string) []string {
	rem := make(map[string]struct{}, len(remove))
	for _, r := range remove {
		rem[strings.ToLower(r)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(current)+len(add))
	out := make([]string, 0, len(current)+len(add))
	for _, c := range current {
		lc := strings.ToLower(c)
		if _, drop := rem[lc]; drop {
			continue
		}
		if _, dup := seen[lc]; dup {
			continue
		}
		seen[lc] = struct{}{}
		out = append(out, lc)
	}
	for _, a := range add {
		la := strings.ToLower(a)
		if _, dup := seen[la]; dup {
			continue
		}
		seen[la] = struct{}{}
		out = append(out, la)
	}
	return out
}

func (r *realClient) PostIssueComment(ctx context.Context, number int, body string) (types.IssueComment, error) {
	b := body
	gc, _, err := r.c.Issues.CreateComment(ctx, r.owner, r.repo, number, &gh.IssueComment{Body: &b})
	if err != nil {
		return types.IssueComment{}, fmt.Errorf("github: post comment on #%d: %w", number, err)
	}
	return normalizeComment(gc), nil
}

func (r *realClient) EditComment(ctx context.Context, commentID int64, body string) error {
	_, _, err := r.c.Issues.EditComment(ctx, r.owner, r.repo, commentID, &gh.IssueComment{Body: gh.Ptr(body)})
	if err != nil {
		return fmt.Errorf("github: edit comment %d: %w", commentID, err)
	}
	return nil
}

func (r *realClient) GetCollaboratorPermission(ctx context.Context, username string) (string, error) {
	level, _, err := r.c.Repositories.GetPermissionLevel(ctx, r.owner, r.repo, username)
	if err != nil {
		return "", fmt.Errorf("github: get permission for %s: %w", username, err)
	}
	if level == nil || level.Permission == nil {
		return "none", nil
	}
	return normalizePermission(*level.Permission), nil
}

// normalizePermission maps GitHub's API values to the canonical set the
// rest of the codebase uses. The collaborator endpoint returns "admin",
// "write", "read", or "none"; repo settings can also surface "maintain"
// and "triage" — pass those through and clamp anything unknown to
// "none".
func normalizePermission(p string) string {
	switch strings.ToLower(p) {
	case "admin":
		return "admin"
	case "maintain":
		return "maintain"
	case "write":
		return "write"
	case "triage", "read":
		return "read"
	case "none", "":
		return "none"
	default:
		return "none"
	}
}

func (r *realClient) CreateDraftPR(ctx context.Context, req CreatePRRequest) (PullRequest, error) {
	npr := &gh.NewPullRequest{
		Title: gh.Ptr(req.Title),
		Head:  gh.Ptr(req.Head),
		Base:  gh.Ptr(req.Base),
		Body:  gh.Ptr(req.Body),
		Draft: gh.Ptr(req.Draft),
	}
	pr, _, err := r.c.PullRequests.Create(ctx, r.owner, r.repo, npr)
	if err != nil {
		return PullRequest{}, fmt.Errorf("github: create PR: %w", err)
	}
	return normalizePR(pr), nil
}

func (r *realClient) FindPRsByHead(ctx context.Context, headBranch string) ([]PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:       "open",
		Head:        r.owner + ":" + headBranch,
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []PullRequest
	for {
		prs, resp, err := r.c.PullRequests.List(ctx, r.owner, r.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list PRs by head %q: %w", headBranch, err)
		}
		for _, p := range prs {
			if p == nil {
				continue
			}
			out = append(out, normalizePR(p))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func (r *realClient) ListPRReviews(ctx context.Context, prNumber int) ([]PRReview, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var out []PRReview
	for {
		reviews, resp, err := r.c.PullRequests.ListReviews(ctx, r.owner, r.repo, prNumber, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list PR reviews on #%d: %w", prNumber, err)
		}
		for _, rv := range reviews {
			if rv == nil {
				continue
			}
			out = append(out, normalizePRReview(rv))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func (r *realClient) AddReaction(ctx context.Context, commentID int64, reaction string) error {
	if _, _, err := r.c.Reactions.CreateIssueCommentReaction(ctx, r.owner, r.repo, commentID, reaction); err != nil {
		return fmt.Errorf("github: add reaction %q to comment %d: %w", reaction, commentID, err)
	}
	return nil
}

// normalizeIssue converts a go-github *Issue into types.Issue.
func normalizeIssue(gi *gh.Issue) types.Issue {
	out := types.Issue{
		Number:      gi.GetNumber(),
		Title:       gi.GetTitle(),
		Description: gi.GetBody(),
		URL:         gi.GetHTMLURL(),
		State:       gi.GetState(),
	}
	if gi.NodeID != nil {
		out.ID = *gi.NodeID
	}
	if gi.CreatedAt != nil {
		out.CreatedAt = gi.CreatedAt.Time
	}
	if gi.UpdatedAt != nil {
		out.UpdatedAt = gi.UpdatedAt.Time
	}
	for _, l := range gi.Labels {
		if l == nil || l.Name == nil {
			continue
		}
		out.Labels = append(out.Labels, strings.ToLower(*l.Name))
	}
	return out
}

// normalizeComment converts a go-github *IssueComment into types.IssueComment.
func normalizeComment(gc *gh.IssueComment) types.IssueComment {
	out := types.IssueComment{
		ID:   gc.GetID(),
		Body: gc.GetBody(),
	}
	if gc.User != nil && gc.User.Login != nil {
		out.User = *gc.User.Login
	}
	if gc.CreatedAt != nil {
		out.CreatedAt = gc.CreatedAt.Time
	}
	return out
}

func normalizePR(p *gh.PullRequest) PullRequest {
	return PullRequest{
		Number: p.GetNumber(),
		URL:    p.GetHTMLURL(),
		State:  p.GetState(),
	}
}

// normalizePRReview converts a go-github *PullRequestReview into PRReview.
func normalizePRReview(rv *gh.PullRequestReview) PRReview {
	out := PRReview{
		ID:    rv.GetID(),
		Body:  rv.GetBody(),
		State: rv.GetState(),
	}
	if rv.User != nil && rv.User.Login != nil {
		out.User = *rv.User.Login
	}
	if rv.SubmittedAt != nil {
		out.SubmittedAt = rv.SubmittedAt.Time
	}
	return out
}
