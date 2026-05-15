package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logosc/symphony-go/internal/approval"
	"github.com/logosc/symphony-go/internal/config"
	gh "github.com/logosc/symphony-go/internal/github"
	"github.com/logosc/symphony-go/internal/runner"
	"github.com/logosc/symphony-go/internal/state"
	"github.com/logosc/symphony-go/internal/types"
	"github.com/logosc/symphony-go/internal/workspace"
)

// testHarness bundles every collaborator a per-issue flow needs against a
// real but tiny git repo + bare origin.
type testHarness struct {
	cfg          *config.Config
	gh           *gh.InMemoryFake
	state        *state.Store
	mgr          *workspace.Manager
	runner       *runner.FakeRunner
	reviewerRun  *runner.FakeRunner
	reviewer     *approval.Reviewer
	wsRoot       string
	repo         string
	originBare   string
	cleanupFuncs []func()
}

func (h *testHarness) cleanup() {
	for i := len(h.cleanupFuncs) - 1; i >= 0; i-- {
		h.cleanupFuncs[i]()
	}
}

// newTestHarness sets up a real local repo + bare origin so push works and
// returns a harness with sensible defaults.
func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := exec.Command("git", "init", "-b", "main", dir).Run(); err != nil {
		// Fallback for older git.
		if err2 := exec.Command("git", "init", dir).Run(); err2 != nil {
			t.Fatalf("git init: %v / %v", err, err2)
		}
		run("checkout", "-b", "main")
	}
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")

	originDir := t.TempDir()
	originPath := filepath.Join(originDir, "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", dir, originPath).CombinedOutput(); err != nil {
		t.Fatalf("clone bare: %v\n%s", err, out)
	}
	run("remote", "add", "origin", originPath)
	run("fetch", "origin")

	cfg := &config.Config{
		Repo: config.RepoConfig{
			FullName:     "OWNER/REPO",
			BaseBranch:   "main",
			LocalPath:    dir,
			WorkflowFile: "WORKFLOW.md",
		},
		GitHub: config.GitHubConfig{TokenEnv: "GITHUB_TOKEN", PollIntervalSeconds: 30},
		Labels: config.LabelsConfig{
			Ready:            "symphony:ready",
			Planning:         "symphony:planning",
			AwaitingApproval: "symphony:awaiting-approval",
			Implementing:     "symphony:implementing",
			PRReady:          "symphony:pr-ready",
			Failed:           "symphony:failed",
			Blocked:          "symphony:blocked",
			Stop:             "symphony:stop",
		},
		Approval: config.ApprovalConfig{Mode: "gated", Command: "/symphony approve", RequireWritePermission: true},
		Auto: config.AutoConfig{
			FallbackOnReject:      "gated",
			FallbackOnNoRuleMatch: "gated",
			VerifyDiffMatchesPlan: true,
			MaxDiffDriftFiles:     2,
			Reviewer:              config.ReviewerConfig{TimeoutSeconds: 60},
		},
		Agent:      config.AgentConfig{Provider: "claude", TimeoutSeconds: 60},
		Codex:      config.CodexConfig{Mode: "exec"},
		Hooks:      config.HooksConfig{TimeoutSeconds: 30},
		Validation: config.ValidationConfig{Commands: []string{"true"}, CommandTimeoutSeconds: 30},
	}

	stateRoot := filepath.Join(t.TempDir(), "state")
	store, err := state.NewStore(stateRoot)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	wsRoot := filepath.Join(t.TempDir(), "wt")
	mgr := workspace.NewManager(dir)
	rnr := runner.NewFakeRunner()
	reviewerRunner := runner.NewFakeRunner()
	rev := approval.NewReviewer(reviewerRunner, cfg.Auto.Reviewer)

	return &testHarness{
		cfg:         cfg,
		gh:          gh.NewInMemoryFake(cfg.Repo.FullName),
		state:       store,
		mgr:         mgr,
		runner:      rnr,
		reviewerRun: reviewerRunner,
		reviewer:    rev,
		wsRoot:      wsRoot,
		repo:        dir,
		originBare:  originPath,
	}
}

// pushToBare uses runGit to push from repoPath to origin (the bare clone).
// Token is ignored — origin is local.
func (h *testHarness) pushToBare(ctx context.Context, repoPath, branch, _ string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "push", "origin", branch)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push: %w: %s", err, out)
	}
	return nil
}

// newOrch wires up an Orchestrator from h with the given prompt template.
func (h *testHarness) newOrch(t *testing.T, prompt string, withReviewer bool) *Orchestrator {
	t.Helper()
	deps := Deps{
		Config:         h.cfg,
		GitHub:         h.gh,
		State:          h.state,
		WorkspaceMgr:   h.mgr,
		AgentRunner:    h.runner,
		PromptTemplate: prompt,
		PushFunc:       h.pushToBare,
		WorkspaceRoot:  h.wsRoot,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if withReviewer {
		deps.Reviewer = h.reviewer
	}
	o, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o
}

// canonicalPlan returns a plan body whose `## Scope` block claims the
// given files.
func canonicalPlan(files []string) string {
	var b strings.Builder
	b.WriteString("# Plan\n\nWill modify the listed files.\n\n## Scope\n")
	b.WriteString("files_touched:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("estimated_lines_added: 5\n")
	b.WriteString("estimated_lines_removed: 1\n")
	b.WriteString("risk_summary: low\n")
	return b.String()
}

// implWriter returns an OnRun that, when called for the implementation
// phase, writes the listed files (relative to req.RepoPath) with simple
// content. For other phases it falls through to canned Responses.
func implWriter(rnr *runner.FakeRunner, files []string) {
	rnr.OnRun = func(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
		if req.Phase == types.PhaseImplementation {
			for _, rel := range files {
				p := filepath.Join(req.RepoPath, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return types.RunResult{}, err
				}
				if err := os.WriteFile(p, []byte("body for "+rel+"\n"), 0o644); err != nil {
					return types.RunResult{}, err
				}
			}
			return types.RunResult{Success: true, Text: "done"}, nil
		}
		// Look up canned response for non-impl phases.
		if r, ok := rnr.Responses[req.Phase]; ok {
			return r, nil
		}
		return types.RunResult{}, fmt.Errorf("no canned response for phase %q", req.Phase)
	}
}

// committedImplWriter simulates an agent that commits its own changes
// before returning success.
func committedImplWriter(rnr *runner.FakeRunner, files []string) {
	implWriter(rnr, files)
	previous := rnr.OnRun
	rnr.OnRun = func(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
		res, err := previous(ctx, req)
		if err != nil || req.Phase != types.PhaseImplementation {
			return res, err
		}
		cmd := exec.CommandContext(ctx, "git", "-C", req.RepoPath, "add", "-A")
		if out, err := cmd.CombinedOutput(); err != nil {
			return types.RunResult{}, fmt.Errorf("git add: %w: %s", err, out)
		}
		cmd = exec.CommandContext(ctx, "git", "-C", req.RepoPath,
			"-c", "user.name=test",
			"-c", "user.email=test@example.com",
			"commit", "-m", "agent commit")
		if out, err := cmd.CombinedOutput(); err != nil {
			return types.RunResult{}, fmt.Errorf("git commit: %w: %s", err, out)
		}
		return res, nil
	}
}

// seedReadyIssue inserts a ready-labeled issue and returns it.
func (h *testHarness) seedReadyIssue(num int, title string, extraLabels ...string) types.Issue {
	labels := append([]string{h.cfg.Labels.Ready}, extraLabels...)
	iss := types.Issue{
		Number: num, Title: title, Description: "do the thing",
		URL: fmt.Sprintf("https://example.test/%d", num), State: "open",
		Labels: labels,
	}
	h.gh.SeedIssue(iss, false)
	return iss
}

// labelsFor returns the labels currently on issue n in the fake.
func (h *testHarness) labelsFor(n int) []string {
	got, _ := h.gh.GetIssue(context.Background(), n)
	return got.Labels
}

// findLabel reports whether labels contains want (case-insensitive).
func findLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

// TestGatedHappyPath: ready -> awaiting-approval -> /symphony approve -> pr-ready.
func TestGatedHappyPath(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "gated"
	o := h.newOrch(t, "Issue: {{ issue.title }}", false)

	plan := canonicalPlan([]string{"a.txt"})
	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: plan}
	implWriter(h.runner, []string{"a.txt"})

	iss := h.seedReadyIssue(1, "fix things")
	h.gh.SetCollaboratorPermission("alice", "write")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := o.ProcessIssue(ctx, iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(1), h.cfg.Labels.AwaitingApproval) {
		t.Fatalf("expected awaiting-approval, got %v", h.labelsFor(1))
	}

	// Simulate alice approving.
	h.gh.SeedComment(1, types.IssueComment{
		User: "alice", Body: "/symphony approve", CreatedAt: time.Now().UTC(),
	})

	if err := o.PollApprovals(ctx); err != nil {
		t.Fatalf("PollApprovals: %v", err)
	}

	if !findLabel(h.labelsFor(1), h.cfg.Labels.PRReady) {
		t.Fatalf("expected pr-ready, got %v", h.labelsFor(1))
	}
	job, err := h.state.Load(1)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.PRNumber == 0 {
		t.Fatalf("expected pr_number set")
	}
	if job.ApprovalPath != types.ApprovalPathHuman {
		t.Fatalf("expected approval_path=human, got %q", job.ApprovalPath)
	}
}

// TestGatedNonWriterRejected ensures that a non-writer's approve is ignored.
func TestGatedNonWriterRejected(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "gated"
	o := h.newOrch(t, "x", false)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
	implWriter(h.runner, []string{"a.txt"})

	iss := h.seedReadyIssue(2, "fix")
	h.gh.SetCollaboratorPermission("eve", "read")

	ctx := context.Background()
	if err := o.ProcessIssue(ctx, iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	c := h.gh.SeedComment(2, types.IssueComment{User: "eve", Body: "/symphony approve", CreatedAt: time.Now()})
	if err := o.PollApprovals(ctx); err != nil {
		t.Fatalf("PollApprovals: %v", err)
	}
	if !findLabel(h.labelsFor(2), h.cfg.Labels.AwaitingApproval) {
		t.Fatalf("expected to remain awaiting-approval, got %v", h.labelsFor(2))
	}
	reactions := h.gh.Reactions(c.ID)
	if len(reactions) != 1 || reactions[0] != "-1" {
		t.Fatalf("expected -1 reaction, got %v", reactions)
	}
}

// TestAutoRulesOnly: docs label + small scope -> rules path -> pr-ready, no reviewer call.
func TestAutoRulesOnly(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Auto.Rules = []config.AutoRule{
		{IssueLabels: []string{"docs"}, MaxPlanFilesClaimed: 5, ReviewerRequired: false},
	}
	h.cfg.Auto.FallbackOnNoRuleMatch = "block"
	o := h.newOrch(t, "x", false)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt", "b.txt"})}
	implWriter(h.runner, []string{"a.txt", "b.txt"})

	iss := h.seedReadyIssue(3, "fix docs", "docs")

	ctx := context.Background()
	if err := o.ProcessIssue(ctx, iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(3), h.cfg.Labels.PRReady) {
		t.Fatalf("expected pr-ready, got %v", h.labelsFor(3))
	}
	job, _ := h.state.Load(3)
	if job.ApprovalPath != types.ApprovalPathRules {
		t.Fatalf("expected approval_path=rules, got %q", job.ApprovalPath)
	}
	// Reviewer must NOT have been invoked.
	if len(h.reviewerRun.Calls()) != 0 {
		t.Fatalf("reviewer called unexpectedly: %d calls", len(h.reviewerRun.Calls()))
	}
}

// TestAutoReviewerApprove: catch-all rule with reviewer -> reviewer approves -> pr-ready.
func TestAutoReviewerApprove(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Auto.Rules = []config.AutoRule{
		{IssueLabels: []string{}, MaxPlanFilesClaimed: 20, ReviewerRequired: true},
	}
	o := h.newOrch(t, "x", true)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
	implWriter(h.runner, []string{"a.txt"})

	h.reviewerRun.Responses[types.PhaseReview] = types.RunResult{
		Success: true,
		Text:    "## Decision\n```json\n{\"decision\":\"approve\",\"reasons\":[\"ok\"]}\n```\n",
	}

	iss := h.seedReadyIssue(4, "feature")
	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(4), h.cfg.Labels.PRReady) {
		t.Fatalf("expected pr-ready, got %v", h.labelsFor(4))
	}
	job, _ := h.state.Load(4)
	if job.ApprovalPath != types.ApprovalPathReviewer {
		t.Fatalf("expected approval_path=reviewer, got %q", job.ApprovalPath)
	}
}

// TestAutoReviewerRejectFallbackGated: reviewer rejects, fallback gated -> awaiting-approval.
func TestAutoReviewerRejectFallbackGated(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Auto.Rules = []config.AutoRule{{IssueLabels: nil, MaxPlanFilesClaimed: 20, ReviewerRequired: true}}
	h.cfg.Auto.FallbackOnReject = "gated"
	o := h.newOrch(t, "x", true)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
	h.reviewerRun.Responses[types.PhaseReview] = types.RunResult{
		Success: true,
		Text:    "## Decision\n```json\n{\"decision\":\"reject\",\"reasons\":[\"unsafe\"]}\n```\n",
	}

	iss := h.seedReadyIssue(5, "feature")
	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(5), h.cfg.Labels.AwaitingApproval) {
		t.Fatalf("expected awaiting-approval, got %v", h.labelsFor(5))
	}
}

// TestAutoReviewerRejectFallbackBlock: reviewer rejects, fallback block -> blocked.
func TestAutoReviewerRejectFallbackBlock(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Auto.Rules = []config.AutoRule{{IssueLabels: nil, MaxPlanFilesClaimed: 20, ReviewerRequired: true}}
	h.cfg.Auto.FallbackOnReject = "block"
	o := h.newOrch(t, "x", true)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
	h.reviewerRun.Responses[types.PhaseReview] = types.RunResult{
		Success: true,
		Text:    "## Decision\n```json\n{\"decision\":\"reject\",\"reasons\":[\"nope\"]}\n```\n",
	}

	iss := h.seedReadyIssue(6, "feature")
	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(6), h.cfg.Labels.Blocked) {
		t.Fatalf("expected blocked, got %v", h.labelsFor(6))
	}
}

// TestAutoDiffDriftExceeds: planning claims [a.txt], implementation touches more.
func TestAutoDiffDriftExceeds(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "auto"
	h.cfg.Auto.Rules = []config.AutoRule{{IssueLabels: nil, MaxPlanFilesClaimed: 20, ReviewerRequired: false}}
	h.cfg.Auto.MaxDiffDriftFiles = 2
	h.cfg.Auto.VerifyDiffMatchesPlan = true
	o := h.newOrch(t, "x", false)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
	implWriter(h.runner, []string{"a.txt", "b.txt", "c.txt", "d.txt"})

	iss := h.seedReadyIssue(7, "stuff")
	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(7), h.cfg.Labels.Blocked) {
		t.Fatalf("expected blocked from drift, got %v", h.labelsFor(7))
	}
}

// TestHandoffMode: ready -> pr-ready, no awaiting-approval ever, no reviewer call.
func TestHandoffMode(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "handoff"
	o := h.newOrch(t, "x", false)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
	implWriter(h.runner, []string{"a.txt"})

	iss := h.seedReadyIssue(8, "ho")
	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(8), h.cfg.Labels.PRReady) {
		t.Fatalf("expected pr-ready, got %v", h.labelsFor(8))
	}
	job, _ := h.state.Load(8)
	if job.ApprovalPath != types.ApprovalPathHandoff {
		t.Fatalf("expected approval_path=handoff, got %q", job.ApprovalPath)
	}
}

func TestHandoffModeAgentCommittedDiff(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "handoff"
	o := h.newOrch(t, "x", false)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
	committedImplWriter(h.runner, []string{"a.txt"})

	iss := h.seedReadyIssue(18, "agent commits")
	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	if !findLabel(h.labelsFor(18), h.cfg.Labels.PRReady) {
		t.Fatalf("expected pr-ready, got %v", h.labelsFor(18))
	}
	job, _ := h.state.Load(18)
	if job.PRNumber == 0 {
		t.Fatalf("expected PR number after committed branch diff")
	}
}

func TestCollectProofArtifacts(t *testing.T) {
	status := strings.Join([]string{
		" M docs/proof/63/after.png",
		"?? docs/proof/63/repro.webm",
		"?? docs/proof/62/other.png",
		" M shopify/src/worker.ts",
	}, "\n")

	got := collectProofArtifacts(status, 63)
	want := []string{"docs/proof/63/after.png", "docs/proof/63/repro.webm"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("proof artifacts mismatch:\ngot  %v\nwant %v", got, want)
	}
}

func TestBuildPRBodyIncludesProofArtifacts(t *testing.T) {
	job := &types.Job{IssueNumber: 63, ApprovalPath: types.ApprovalPathReviewer}

	body := buildPRBody(job, nil, false, []string{
		"docs/proof/63/after.png",
		"docs/proof/63/repro.webm",
	})

	if !strings.Contains(body, "## Proof Artifacts") {
		t.Fatalf("missing proof section:\n%s", body)
	}
	if !strings.Contains(body, "![after.png](docs/proof/63/after.png)") {
		t.Fatalf("missing embedded screenshot:\n%s", body)
	}
	if !strings.Contains(body, "[docs/proof/63/repro.webm](docs/proof/63/repro.webm)") {
		t.Fatalf("missing video link:\n%s", body)
	}
}

func TestBuildPRBodyNoDuplicatePlanHeading(t *testing.T) {
	// When the agent includes "## Plan" in its plan text, buildPRBody should
	// not add a second "## Plan" heading.
	job := &types.Job{
		IssueNumber:  42,
		ApprovalPath: types.ApprovalPathReviewer,
		PlanText:     "## Plan\n\n### Steps\n- do thing one\n- do thing two",
	}
	body := buildPRBody(job, nil, false, nil)
	count := strings.Count(body, "## Plan")
	if count != 1 {
		t.Fatalf("expected exactly 1 '## Plan' heading, got %d:\n%s", count, body)
	}

	// When the agent does NOT include a heading, buildPRBody should add one.
	job2 := &types.Job{
		IssueNumber:  43,
		ApprovalPath: types.ApprovalPathReviewer,
		PlanText:     "### Steps\n- do thing one\n- do thing two",
	}
	body2 := buildPRBody(job2, nil, false, nil)
	if !strings.Contains(body2, "## Plan\n\n### Steps") {
		t.Fatalf("expected '## Plan' heading to be added:\n%s", body2)
	}
}

func TestEmptyPlanDoesNotPostBlankComment(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "handoff"
	o := h.newOrch(t, "x", false)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: "   \n"}
	implWriter(h.runner, []string{"a.txt"})

	iss := h.seedReadyIssue(19, "empty plan")
	if err := o.ProcessIssue(context.Background(), iss); err != nil {
		t.Fatalf("ProcessIssue: %v", err)
	}
	comments, err := h.gh.ListIssueComments(context.Background(), 19, time.Time{})
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	for _, c := range comments {
		if strings.TrimSpace(c.Body) == "" {
			t.Fatalf("posted blank comment: %#v", comments)
		}
	}
}

// TestPerAxisWorkflowAndValidation verifies the G11+G12 wiring: a
// two-axis config (type:code + type:research) with per-axis workflow
// files and per-axis validation commands. Each issue is processed in
// handoff mode and we assert the rendered prompt + the validation
// command list reflects the resolved axis.
func TestPerAxisWorkflowAndValidation(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "handoff"
	// Simulate the parsed YAML state: scalar workflow_file empty, map
	// populated with two axes + default. Same for validation.
	h.cfg.Repo.WorkflowFile = ""
	h.cfg.Repo.WorkflowFiles = config.OrderedMap[string]{
		Keys: []string{"type:code", "type:research", "default"},
		Values: map[string]string{
			"type:code":     "code-axis",
			"type:research": "research-axis",
			"default":       "code-axis",
		},
	}
	h.cfg.Validation.Commands = nil
	h.cfg.Validation.CommandsByLabel = config.OrderedMap[[]string]{
		Keys: []string{"type:code", "type:research", "default"},
		Values: map[string][]string{
			"type:code":     {"true"},
			"type:research": {"echo research-axis"},
			"default":       {"true"},
		},
	}

	// Wire deps with PromptTemplates rather than the scalar PromptTemplate.
	deps := Deps{
		Config:       h.cfg,
		GitHub:       h.gh,
		State:        h.state,
		WorkspaceMgr: h.mgr,
		AgentRunner:  h.runner,
		PromptTemplates: map[string]string{
			"type:code":     "PROMPT-CODE: {{ issue.title }}",
			"type:research": "PROMPT-RESEARCH: {{ issue.title }}",
			"default":       "PROMPT-CODE: {{ issue.title }}",
		},
		PushFunc:      h.pushToBare,
		WorkspaceRoot: h.wsRoot,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	o, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Capture the planning prompts seen per issue.
	planPrompts := make(map[int]string)
	implPrompts := make(map[int]string)
	axisKeysSeen := make(map[int][]string)
	h.runner.OnRun = func(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
		axisKeysSeen[req.Issue.Number] = append(axisKeysSeen[req.Issue.Number], req.AxisKey)
		switch req.Phase {
		case types.PhasePlanning:
			planPrompts[req.Issue.Number] = req.Prompt
			return types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}, nil
		case types.PhaseImplementation:
			implPrompts[req.Issue.Number] = req.Prompt
			p := filepath.Join(req.RepoPath, "a.txt")
			if err := os.WriteFile(p, []byte("body\n"), 0o644); err != nil {
				return types.RunResult{}, err
			}
			return types.RunResult{Success: true, Text: "done"}, nil
		}
		return types.RunResult{}, fmt.Errorf("unexpected phase %q", req.Phase)
	}

	codeIss := h.seedReadyIssue(101, "code work", "type:code")
	researchIss := h.seedReadyIssue(102, "research work", "type:research")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.ProcessIssue(ctx, codeIss); err != nil {
		t.Fatalf("ProcessIssue(code): %v", err)
	}
	if err := o.ProcessIssue(ctx, researchIss); err != nil {
		t.Fatalf("ProcessIssue(research): %v", err)
	}

	if !strings.Contains(planPrompts[101], "PROMPT-CODE: code work") {
		t.Errorf("code planning prompt = %q", planPrompts[101])
	}
	if !strings.Contains(planPrompts[102], "PROMPT-RESEARCH: research work") {
		t.Errorf("research planning prompt = %q", planPrompts[102])
	}
	if !strings.Contains(implPrompts[101], "PROMPT-CODE") {
		t.Errorf("code impl prompt = %q", implPrompts[101])
	}
	if !strings.Contains(implPrompts[102], "PROMPT-RESEARCH") {
		t.Errorf("research impl prompt = %q", implPrompts[102])
	}
	for _, key := range axisKeysSeen[101] {
		if key != "type:code" {
			t.Errorf("issue 101 saw axis_key=%q; want type:code", key)
		}
	}
	for _, key := range axisKeysSeen[102] {
		if key != "type:research" {
			t.Errorf("issue 102 saw axis_key=%q; want type:research", key)
		}
	}

	codeJob, _ := h.state.Load(101)
	if codeJob.AxisKey != "type:code" || codeJob.AxisSource != "by_label" {
		t.Errorf("code job axis = (%q,%q); want (type:code, by_label)", codeJob.AxisKey, codeJob.AxisSource)
	}
	researchJob, _ := h.state.Load(102)
	if researchJob.AxisKey != "type:research" || researchJob.AxisSource != "by_label" {
		t.Errorf("research job axis = (%q,%q); want (type:research, by_label)",
			researchJob.AxisKey, researchJob.AxisSource)
	}

	// Both should have reached pr-ready (validation passed for both).
	if !findLabel(h.labelsFor(101), h.cfg.Labels.PRReady) {
		t.Errorf("code issue not pr-ready: %v", h.labelsFor(101))
	}
	if !findLabel(h.labelsFor(102), h.cfg.Labels.PRReady) {
		t.Errorf("research issue not pr-ready: %v", h.labelsFor(102))
	}

	// Resolve validation commands directly via the orchestrator helper to
	// confirm the per-axis lookup picks the correct slice.
	codeCmds, err := o.resolveValidationCommands(codeJob)
	if err != nil {
		t.Fatalf("resolveValidationCommands(code): %v", err)
	}
	if len(codeCmds) != 1 || codeCmds[0] != "true" {
		t.Errorf("code validation cmds = %v", codeCmds)
	}
	researchCmds, _ := o.resolveValidationCommands(researchJob)
	if len(researchCmds) != 1 || researchCmds[0] != "echo research-axis" {
		t.Errorf("research validation cmds = %v", researchCmds)
	}
}

// TestPerAxisAgentRunners verifies G19 wiring: with two issues carrying
// different type:* labels and Deps.AgentRunnersByAxis populated with
// distinct fakes per axis, each issue's RunRequests reach only the
// runner registered for that axis. See Proposal 0001 §11 question #1.
func TestPerAxisAgentRunners(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "handoff"
	// Mark per-axis mode by setting WorkflowFiles (the canonical axis
	// anchor used by resolveAxis). Empty scalar workflow_file.
	h.cfg.Repo.WorkflowFile = ""
	h.cfg.Repo.WorkflowFiles = config.OrderedMap[string]{
		Keys: []string{"type:code", "type:research", "default"},
		Values: map[string]string{
			"type:code":     "code-axis",
			"type:research": "research-axis",
			"default":       "code-axis",
		},
	}

	codeRunner := runner.NewFakeRunner()
	researchRunner := runner.NewFakeRunner()
	for _, r := range []*runner.FakeRunner{codeRunner, researchRunner} {
		r.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
		implWriter(r, []string{"a.txt"})
	}

	deps := Deps{
		Config:       h.cfg,
		GitHub:       h.gh,
		State:        h.state,
		WorkspaceMgr: h.mgr,
		// Default AgentRunner unused when a matching axis runner exists,
		// but New() requires it to be non-nil. Use a third fake so a
		// stray call would be visible.
		AgentRunner: runner.NewFakeRunner(),
		AgentRunnersByAxis: map[string]runner.AgentRunner{
			"type:code":     codeRunner,
			"type:research": researchRunner,
			"default":       codeRunner,
		},
		PromptTemplates: map[string]string{
			"type:code":     "PROMPT-CODE: {{ issue.title }}",
			"type:research": "PROMPT-RESEARCH: {{ issue.title }}",
			"default":       "PROMPT-CODE: {{ issue.title }}",
		},
		PushFunc:      h.pushToBare,
		WorkspaceRoot: h.wsRoot,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	o, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	codeIss := h.seedReadyIssue(201, "code work", "type:code")
	researchIss := h.seedReadyIssue(202, "research work", "type:research")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.ProcessIssue(ctx, codeIss); err != nil {
		t.Fatalf("ProcessIssue(code): %v", err)
	}
	if err := o.ProcessIssue(ctx, researchIss); err != nil {
		t.Fatalf("ProcessIssue(research): %v", err)
	}

	// Each per-axis runner saw exactly its axis's issue and no other.
	for _, c := range codeRunner.Calls() {
		if c.Issue.Number != 201 {
			t.Errorf("codeRunner saw issue %d; want 201 only", c.Issue.Number)
		}
		if c.AxisKey != "type:code" {
			t.Errorf("codeRunner call AxisKey = %q; want type:code", c.AxisKey)
		}
	}
	if len(codeRunner.Calls()) == 0 {
		t.Errorf("codeRunner saw no calls")
	}
	for _, c := range researchRunner.Calls() {
		if c.Issue.Number != 202 {
			t.Errorf("researchRunner saw issue %d; want 202 only", c.Issue.Number)
		}
		if c.AxisKey != "type:research" {
			t.Errorf("researchRunner call AxisKey = %q; want type:research", c.AxisKey)
		}
	}
	if len(researchRunner.Calls()) == 0 {
		t.Errorf("researchRunner saw no calls")
	}
	// The fallback (Deps.AgentRunner) must NOT have been hit.
	if fallback := deps.AgentRunner.(*runner.FakeRunner); len(fallback.Calls()) != 0 {
		t.Errorf("default AgentRunner unexpectedly called %d times", len(fallback.Calls()))
	}
}

// TestRunOnce dispatches one issue via Run(once=true).
func TestRunOnce(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.Approval.Mode = "handoff"
	o := h.newOrch(t, "x", false)

	h.runner.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: canonicalPlan([]string{"a.txt"})}
	implWriter(h.runner, []string{"a.txt"})

	h.seedReadyIssue(9, "feature")
	if err := o.Run(context.Background(), true); err != nil {
		t.Fatalf("Run(once): %v", err)
	}
	if !findLabel(h.labelsFor(9), h.cfg.Labels.PRReady) {
		t.Fatalf("expected pr-ready, got %v", h.labelsFor(9))
	}
}
