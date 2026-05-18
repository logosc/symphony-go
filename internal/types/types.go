// Package types defines the cross-cutting data types shared across
// symphony-go packages. Config-specific types live in internal/config.
package types

import "time"

// Phase identifies which phase of an agent run is executing.
type Phase string

const (
	PhasePlanning       Phase = "planning"
	PhaseReview         Phase = "review"
	PhaseImplementation Phase = "implementation"
)

// JobStatus is the orchestrator's local view of a job's progress through
// its lifecycle. Mirrors the GitHub label state machine but uses snake_case.
type JobStatus string

const (
	StatusPlanning         JobStatus = "planning"
	StatusAwaitingApproval JobStatus = "awaiting_approval"
	StatusImplementing     JobStatus = "implementing"
	StatusPRReady          JobStatus = "pr_ready"
	StatusFailed           JobStatus = "failed"
	StatusBlocked          JobStatus = "blocked"
)

// ApprovalMode is the global approval policy from config.yml.
type ApprovalMode string

const (
	ApprovalGated   ApprovalMode = "gated"
	ApprovalAuto    ApprovalMode = "auto"
	ApprovalHandoff ApprovalMode = "handoff"
)

// ApprovalPath records which gate let a plan through, persisted on Job
// for audit and PR-body provenance.
type ApprovalPath string

const (
	ApprovalPathRules    ApprovalPath = "rules"
	ApprovalPathReviewer ApprovalPath = "reviewer"
	ApprovalPathHuman    ApprovalPath = "human"
	ApprovalPathHandoff  ApprovalPath = "handoff"
)

// Issue is the normalized GitHub issue payload used by the orchestrator
// and prompt template engine.
type Issue struct {
	Number      int       `json:"number"`
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	Labels      []string  `json:"labels"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// IssueComment is a normalized GitHub issue comment used by the approval
// poller (gated mode) and reconciliation.
type IssueComment struct {
	ID        int64     `json:"id"`
	User      string    `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// PlanScope is the structured `## Scope` block parsed from the agent's
// planning output. Drives auto-approval rule evaluation and post-impl
// diff verification.
type PlanScope struct {
	FilesTouched          []string `json:"files_touched"           yaml:"files_touched"`
	EstimatedLinesAdded   int      `json:"estimated_lines_added"   yaml:"estimated_lines_added"`
	EstimatedLinesRemoved int      `json:"estimated_lines_removed" yaml:"estimated_lines_removed"`
	RiskSummary           string   `json:"risk_summary"            yaml:"risk_summary"`
}

// ReviewerDecision is the parsed `## Decision` block from a reviewer run.
type ReviewerDecision struct {
	Decision string   `json:"decision"` // "approve" | "reject"
	Reasons  []string `json:"reasons"`
}

// Job is the on-disk job state. Persisted to
// .symphony-go/state/jobs/{issue_number}.json.
type Job struct {
	IssueNumber       int          `json:"issue_number"`
	Repo              string       `json:"repo"`
	Status            JobStatus    `json:"status"`
	WorkspaceRoot     string       `json:"workspace_root"`
	RepoPath          string       `json:"repo_path"`
	Branch            string       `json:"branch"`
	PlanCommentID     int64        `json:"plan_comment_id,omitempty"`
	PlanText          string       `json:"plan_text,omitempty"`
	PlanScope         *PlanScope   `json:"plan_scope,omitempty"`
	ApprovalPath      ApprovalPath `json:"approval_path,omitempty"`
	ApprovalCommentID int64        `json:"approval_comment_id,omitempty"`
	// ApprovalToken, when non-empty, is the random per-plan token the
	// orchestrator embeds in the plan comment. The gated-mode approval
	// poller accepts a comment whose trimmed body equals this token in
	// place of the static cfg.Approval.Command. Reset whenever planning
	// re-runs so an old approval cannot promote a stale plan. Set only
	// when cfg.Approval.RequireToken is true.
	ApprovalToken    string            `json:"approval_token,omitempty"`
	ReviewerDecision *ReviewerDecision `json:"reviewer_decision,omitempty"`
	PRNumber         int               `json:"pr_number,omitempty"`
	// RevisionAttempted is set to true once a single PR-revision cycle has
	// run on this job (triggered by a CHANGES_REQUESTED review). The
	// orchestrator only revises a PR once — subsequent CHANGES_REQUESTED
	// reviews are ignored.
	RevisionAttempted bool      `json:"revision_attempted,omitempty"`
	Attempt           int       `json:"attempt"`
	UpdatedAt         time.Time `json:"updated_at"`
	// AxisKey is the per-axis label this job was frozen against at claim
	// time. "default" when no per-axis map is configured or no concrete
	// label matched. Empty on jobs persisted before the per-axis feature
	// shipped (treat empty as "default", "scalar"). See Proposal 0001.
	AxisKey string `json:"axis_key,omitempty"`
	// AxisSource records how AxisKey was selected. One of "by_label" (a
	// `*_by_label` map drove resolution) or "scalar" (no per-axis map was
	// configured for the canonical knob).
	AxisSource string `json:"axis_source,omitempty"`
}

// RunRequest is what the orchestrator hands to an AgentRunner.
type RunRequest struct {
	Issue    Issue
	RepoPath string
	HomePath string
	Prompt   string
	Phase    Phase
	Timeout  time.Duration
	// AxisKey is the per-axis label frozen on the Job at claim time. May
	// be empty for legacy callers that haven't been updated. Runners that
	// honor per-axis tool/sandbox maps key into them with this value
	// rather than re-resolving from current issue labels (reconcile-safe).
	AxisKey string
	// ExtraEnv is appended to the agent's command env after the
	// allowlist/blocklist sanitizer has run. Each entry is "KEY=VALUE".
	// Used by the orchestrator to advertise side-channel file paths
	// (e.g. SYMPHONY_PLAN_SCOPE_PATH) to the agent without coupling them
	// to the user's env allowlist. See proposal 0004.
	ExtraEnv []string
}

// RunResult is what an AgentRunner returns. Stderr and Events are already
// redacted by the runner before being returned.
type RunResult struct {
	Success     bool
	Text        string // final agent output suitable for plan body or PR body
	Stderr      string // captured, redacted
	Events      []byte // raw event log, redacted (e.g., stream-json line buffer)
	StartedAt   time.Time
	CompletedAt time.Time
}
