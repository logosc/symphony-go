package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validConfigYAML returns a YAML document covering every required field
// so it can be tweaked to focus on a single failure mode per test.
func validConfigYAML() string {
	return `
repo:
  full_name: "OWNER/REPO"
  base_branch: "main"
  local_path: "/tmp/some-repo"
  workflow_file: "WORKFLOW.md"
github:
  token_env: "GITHUB_TOKEN"
  poll_interval_seconds: 30
labels:
  ready: "symphony:ready"
  planning: "symphony:planning"
  awaiting_approval: "symphony:awaiting-approval"
  implementing: "symphony:implementing"
  pr_ready: "symphony:pr-ready"
  failed: "symphony:failed"
  blocked: "symphony:blocked"
  stop: "symphony:stop"
approval:
  mode: "auto"
  command: "/symphony approve"
  require_write_permission: true
auto:
  rules:
    - issue_labels: ["docs"]
      max_plan_files_claimed: 5
      reviewer_required: false
    - issue_labels: []
      max_plan_files_claimed: 20
      reviewer_required: true
  reviewer:
    provider: "codex"
    model: ""
    timeout_seconds: 600
  fallback_on_reject: "gated"
  fallback_on_no_rule_match: "gated"
  verify_diff_matches_plan: true
  max_diff_drift_files: 2
agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600
claude:
  max_turns: 20
  planning_tools: ["Read", "Grep"]
  implementation_tools: ["Read", "Edit"]
  review_tools: ["Read"]
  disallowed_tools: ["Bash(sudo:*)"]
codex:
  mode: "exec"
env:
  allowlist: ["OPENAI_API_KEY"]
  block_patterns: [".*TOKEN.*"]
hooks:
  after_create: ""
  before_run: ""
  after_run: ""
  timeout_seconds: 60
validation:
  commands: ["go test ./..."]
  command_timeout_seconds: 900
audit:
  redact_patterns: ["sk-[A-Za-z0-9_-]+"]
`
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeTempConfig(t, validConfigYAML())
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Repo.FullName != "OWNER/REPO" {
		t.Errorf("repo.full_name = %q", cfg.Repo.FullName)
	}
	if cfg.Approval.Mode != "auto" {
		t.Errorf("approval.mode = %q", cfg.Approval.Mode)
	}
	if len(cfg.Auto.Rules) != 2 {
		t.Errorf("auto.rules len = %d", len(cfg.Auto.Rules))
	}
	if cfg.Auto.Rules[1].IssueLabels == nil {
		// catch-all is `[]`, not nil; yaml gives empty slice
	}
	if cfg.GitHub.PollIntervalSeconds != 30 {
		t.Errorf("poll_interval_seconds = %d", cfg.GitHub.PollIntervalSeconds)
	}
}

func TestLoadDefaultsApplied(t *testing.T) {
	// Minimal YAML with required fields only.
	body := `
repo:
  full_name: "O/R"
  local_path: "/tmp/x"
approval:
  mode: "gated"
auto:
  reviewer:
    provider: "codex"
agent:
  provider: "claude"
`
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Repo.BaseBranch != "main" {
		t.Errorf("default base_branch: got %q", cfg.Repo.BaseBranch)
	}
	if cfg.Repo.WorkflowFile != "WORKFLOW.md" {
		t.Errorf("default workflow_file: got %q", cfg.Repo.WorkflowFile)
	}
	if cfg.GitHub.TokenEnv != "GITHUB_TOKEN" {
		t.Errorf("default token_env: got %q", cfg.GitHub.TokenEnv)
	}
	if cfg.GitHub.PollIntervalSeconds != 30 {
		t.Errorf("default poll_interval_seconds: got %d", cfg.GitHub.PollIntervalSeconds)
	}
	if cfg.Labels.Ready != "symphony:ready" {
		t.Errorf("default labels.ready: got %q", cfg.Labels.Ready)
	}
	if cfg.Approval.Command != "/symphony approve" {
		t.Errorf("default approval.command: got %q", cfg.Approval.Command)
	}
	if cfg.Auto.MaxDiffDriftFiles != 2 {
		t.Errorf("default max_diff_drift_files: got %d", cfg.Auto.MaxDiffDriftFiles)
	}
	if cfg.Agent.TimeoutSeconds != 3600 {
		t.Errorf("default agent.timeout_seconds: got %d", cfg.Agent.TimeoutSeconds)
	}
	if cfg.Claude.MaxTurns != 20 {
		t.Errorf("default claude.max_turns: got %d", cfg.Claude.MaxTurns)
	}
	if cfg.Codex.Mode != "exec" {
		t.Errorf("default codex.mode: got %q", cfg.Codex.Mode)
	}
	if cfg.Hooks.TimeoutSeconds != 60 {
		t.Errorf("default hooks.timeout_seconds: got %d", cfg.Hooks.TimeoutSeconds)
	}
	if cfg.Validation.CommandTimeoutSeconds != 900 {
		t.Errorf("default validation timeout: got %d", cfg.Validation.CommandTimeoutSeconds)
	}
	if cfg.Orchestrator.MaxConcurrentJobs != 1 {
		t.Errorf("default orchestrator.max_concurrent_jobs: got %d", cfg.Orchestrator.MaxConcurrentJobs)
	}
}

// TestValidateMaxConcurrentJobsZero asserts Validate rejects an explicit
// zero or negative MaxConcurrentJobs (bypassing applyDefaults).
func TestValidateMaxConcurrentJobsZero(t *testing.T) {
	mk := func(n int) *Config {
		return &Config{
			Repo:   RepoConfig{FullName: "O/R", BaseBranch: "main", LocalPath: "/tmp/x", WorkflowFile: "WORKFLOW.md"},
			GitHub: GitHubConfig{Auth: "pat", TokenEnv: "GITHUB_TOKEN", PollIntervalSeconds: 30},
			Labels: LabelsConfig{
				Ready: "r", Planning: "p", AwaitingApproval: "aa", Implementing: "i",
				PRReady: "pr", Failed: "f", Blocked: "b", Stop: "s",
			},
			Approval:     ApprovalConfig{Mode: "gated", Command: "/x"},
			Auto:         AutoConfig{FallbackOnReject: "gated", FallbackOnNoRuleMatch: "gated"},
			Agent:        AgentConfig{Provider: "claude", TimeoutSeconds: 60},
			Codex:        CodexConfig{Mode: "exec"},
			Hooks:        HooksConfig{TimeoutSeconds: 30},
			Validation:   ValidationConfig{CommandTimeoutSeconds: 60},
			Orchestrator: OrchestratorConfig{MaxConcurrentJobs: n},
		}
	}
	for _, n := range []int{0, -1, -7} {
		err := Validate(mk(n))
		if err == nil {
			t.Errorf("Validate(MaxConcurrentJobs=%d): expected error, got nil", n)
			continue
		}
		if !strings.Contains(err.Error(), "max_concurrent_jobs") {
			t.Errorf("Validate(MaxConcurrentJobs=%d): err = %v", n, err)
		}
	}
	if err := Validate(mk(1)); err != nil {
		t.Errorf("Validate(MaxConcurrentJobs=1): unexpected error: %v", err)
	}
}

func TestValidateMissingFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(s string) string
		wantSub string
	}{
		{
			name:    "missing repo.full_name",
			mutate:  func(s string) string { return strings.Replace(s, `full_name: "OWNER/REPO"`, `full_name: ""`, 1) },
			wantSub: "repo.full_name",
		},
		{
			name: "bad full_name format",
			mutate: func(s string) string {
				return strings.Replace(s, `full_name: "OWNER/REPO"`, `full_name: "no-slash"`, 1)
			},
			wantSub: "OWNER/REPO",
		},
		{
			name:    "missing local_path",
			mutate:  func(s string) string { return strings.Replace(s, `local_path: "/tmp/some-repo"`, `local_path: ""`, 1) },
			wantSub: "repo.local_path",
		},
		{
			name: "bad poll_interval",
			mutate: func(s string) string {
				return strings.Replace(s, `poll_interval_seconds: 30`, `poll_interval_seconds: -1`, 1)
			},
			wantSub: "github.poll_interval_seconds",
		},
		{
			name:    "bad approval.mode",
			mutate:  func(s string) string { return strings.Replace(s, `mode: "auto"`, `mode: "wat"`, 1) },
			wantSub: "approval.mode",
		},
		{
			name:    "bad agent.provider",
			mutate:  func(s string) string { return strings.Replace(s, `provider: "claude"`, `provider: "gpt"`, 1) },
			wantSub: "agent.provider",
		},
		{
			name:    "bad codex.mode",
			mutate:  func(s string) string { return strings.Replace(s, `mode: "exec"`, `mode: "weird"`, 1) },
			wantSub: "codex.mode",
		},
		{
			name: "negative max_concurrent_jobs",
			mutate: func(s string) string {
				return s + "\norchestrator:\n  max_concurrent_jobs: -1\n"
			},
			wantSub: "orchestrator.max_concurrent_jobs",
		},
		{
			name: "bad redact pattern",
			mutate: func(s string) string {
				return strings.Replace(s, `redact_patterns: ["sk-[A-Za-z0-9_-]+"]`, `redact_patterns: ["[unterminated"]`, 1)
			},
			wantSub: "audit.redact_patterns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.mutate(validConfigYAML())
			p := writeTempConfig(t, body)
			_, err := Load(p)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v; want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestEnvIndirectionTokenEnvIsName(t *testing.T) {
	// token_env is a *name*, never a value. We just verify it parses verbatim.
	p := writeTempConfig(t, validConfigYAML())
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHub.TokenEnv != "GITHUB_TOKEN" {
		t.Errorf("token_env mangled: %q", cfg.GitHub.TokenEnv)
	}
}

func TestEnvIndirectionExpandsValues(t *testing.T) {
	t.Setenv("MS_TEST_LOCAL_PATH", "/expanded/repo")
	t.Setenv("MS_TEST_BRANCH", "trunk")
	body := strings.Replace(validConfigYAML(), `local_path: "/tmp/some-repo"`, `local_path: "$MS_TEST_LOCAL_PATH"`, 1)
	body = strings.Replace(body, `base_branch: "main"`, `base_branch: "${MS_TEST_BRANCH}"`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Repo.LocalPath != "/expanded/repo" {
		t.Errorf("LocalPath = %q; want /expanded/repo", cfg.Repo.LocalPath)
	}
	if cfg.Repo.BaseBranch != "trunk" {
		t.Errorf("BaseBranch = %q; want trunk", cfg.Repo.BaseBranch)
	}
}

func TestEnvIndirectionUndefinedExpandsEmpty(t *testing.T) {
	os.Unsetenv("MS_TEST_DEFINITELY_UNSET")
	body := strings.Replace(validConfigYAML(),
		`after_create: ""`,
		`after_create: "before-$MS_TEST_DEFINITELY_UNSET-after"`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Hooks.AfterCreate != "before--after" {
		t.Errorf("AfterCreate = %q; want before--after", cfg.Hooks.AfterCreate)
	}
}

func TestLoadWorkflowFilesScalarCollision(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`workflow_file: "WORKFLOW.md"`,
		`workflow_file: "WORKFLOW.md"
  workflow_files:
    "type:code": "WORKFLOW.code.md"
    default:     "WORKFLOW.code.md"`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v; want collision", err)
	}
}

func TestLoadWorkflowFilesMissingDefault(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`workflow_file: "WORKFLOW.md"`,
		`workflow_files:
    "type:code": "WORKFLOW.code.md"`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v; want missing-default error", err)
	}
}

func TestLoadWorkflowFilesOnlyMap(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`workflow_file: "WORKFLOW.md"`,
		`workflow_files:
    "type:code":     "WORKFLOW.code.md"
    "type:research": "WORKFLOW.research.md"
    default:         "WORKFLOW.code.md"`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Repo.WorkflowFile != "" {
		t.Errorf("WorkflowFile should be empty, got %q", cfg.Repo.WorkflowFile)
	}
	if cfg.Repo.WorkflowFiles.IsEmpty() {
		t.Fatal("WorkflowFiles should be populated")
	}
	want := []string{"type:code", "type:research", "default"}
	for i, k := range want {
		if cfg.Repo.WorkflowFiles.Keys[i] != k {
			t.Errorf("Keys[%d] = %q; want %q", i, cfg.Repo.WorkflowFiles.Keys[i], k)
		}
	}
}

func TestLoadValidationCommandsByLabelCollision(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`commands: ["go test ./..."]`,
		`commands: ["go test ./..."]
  commands_by_label:
    "type:code": ["go test ./..."]
    default:     []`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v; want collision", err)
	}
}

func TestLoadValidationCommandsByLabelMissingDefault(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`commands: ["go test ./..."]`,
		`commands_by_label:
    "type:code": ["go test ./..."]`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v; want missing-default error", err)
	}
}

func TestLoadValidationCommandsByLabelOnlyMap(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`commands: ["go test ./..."]`,
		`commands_by_label:
    "type:code":     ["go test ./..."]
    "type:research": ["test -f docs/research/x.md"]
    default:         []`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Validation.Commands) != 0 {
		t.Errorf("Commands should be empty, got %v", cfg.Validation.Commands)
	}
	if cfg.Validation.CommandsByLabel.IsEmpty() {
		t.Fatal("CommandsByLabel should be populated")
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	p := writeTempConfig(t, "::not yaml::\n  bad indent\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// --- Per-axis configuration: G13 + G16 (Proposal 0001) ---

// TestLoadClaudePlanningToolsByLabelCollision rejects the case where both
// scalar and per-axis maps are set for the same knob.
func TestLoadClaudePlanningToolsByLabelCollision(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`planning_tools: ["Read", "Grep"]`,
		`planning_tools: ["Read", "Grep"]
  planning_tools_by_label:
    "type:research": ["Read", "WebFetch"]
    default:         ["Read"]`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v; want collision", err)
	}
}

// TestLoadClaudeImplementationToolsByLabelMissingDefault rejects per-axis
// maps that don't include a `default` entry.
func TestLoadClaudeImplementationToolsByLabelMissingDefault(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`implementation_tools: ["Read", "Edit"]`,
		`implementation_tools_by_label:
    "type:code": ["Read", "Edit"]`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v; want missing-default", err)
	}
}

// TestLoadClaudeAllByLabelMaps populates every claude per-axis map and
// checks they parse and survive validation.
func TestLoadClaudeAllByLabelMaps(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`claude:
  max_turns: 20
  planning_tools: ["Read", "Grep"]
  implementation_tools: ["Read", "Edit"]
  review_tools: ["Read"]
  disallowed_tools: ["Bash(sudo:*)"]`,
		`claude:
  max_turns: 20
  planning_tools_by_label:
    "type:research": ["Read", "WebFetch"]
    default:         ["Read"]
  implementation_tools_by_label:
    "type:research": ["Read", "Write"]
    default:         ["Read", "Edit"]
  review_tools_by_label:
    default: ["Read"]
  disallowed_tools_by_label:
    default: ["Bash(sudo:*)"]`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Claude.PlanningToolsByLabel.IsEmpty() {
		t.Fatalf("planning_tools_by_label not parsed")
	}
	if got := cfg.Claude.PlanningToolsByLabel.Values["type:research"]; len(got) != 2 || got[0] != "Read" {
		t.Fatalf("unexpected planning research tools: %v", got)
	}
}

// TestLoadCodexImplementationArgsByLabelCollision rejects scalar+map.
func TestLoadCodexImplementationArgsByLabelCollision(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`codex:
  mode: "exec"`,
		`codex:
  mode: "exec"
  implementation_args: ["--sandbox", "workspace-write"]
  implementation_args_by_label:
    "type:research": ["--sandbox", "read-only"]
    default:         ["--sandbox", "workspace-write"]`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v; want collision", err)
	}
}

// TestLoadCodexArgsByLabelOnlyMap parses a fully per-axis codex section.
func TestLoadCodexArgsByLabelOnlyMap(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`codex:
  mode: "exec"`,
		`codex:
  mode: "exec"
  planning_args_by_label:
    default: ["--sandbox", "read-only"]
  implementation_args_by_label:
    "type:research": ["--sandbox", "read-only"]
    default:         ["--sandbox", "workspace-write"]
  review_args_by_label:
    default: ["--sandbox", "read-only"]`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Codex.ImplementationArgsByLabel.IsEmpty() {
		t.Fatalf("implementation_args_by_label empty")
	}
	v := cfg.Codex.ImplementationArgsByLabel.Values["type:research"]
	if len(v) != 2 || v[1] != "read-only" {
		t.Fatalf("unexpected research impl args: %v", v)
	}
}

// TestLoadApprovalModeByLabelCollision rejects scalar+map.
func TestLoadApprovalModeByLabelCollision(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`approval:
  mode: "auto"
  command: "/symphony approve"
  require_write_permission: true`,
		`approval:
  mode: "auto"
  mode_by_label:
    "type:marketing-ads": "gated"
    default:               "auto"
  command: "/symphony approve"
  require_write_permission: true`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v; want collision", err)
	}
}

// TestLoadApprovalModeByLabelMissingDefault rejects map without default.
func TestLoadApprovalModeByLabelMissingDefault(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`approval:
  mode: "auto"
  command: "/symphony approve"
  require_write_permission: true`,
		`approval:
  mode_by_label:
    "type:marketing-ads": "gated"
  command: "/symphony approve"
  require_write_permission: true`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v; want missing-default", err)
	}
}

// TestLoadApprovalModeByLabelInvalidValue catches typos like "gateed".
func TestLoadApprovalModeByLabelInvalidValue(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`approval:
  mode: "auto"
  command: "/symphony approve"
  require_write_permission: true`,
		`approval:
  mode_by_label:
    "type:marketing-ads": "gateed"
    default:               "auto"
  command: "/symphony approve"
  require_write_permission: true`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "gated|auto|handoff") {
		t.Fatalf("err = %v; want enum violation", err)
	}
}

// TestLoadAgentProviderByLabelCollision rejects setting both scalar
// agent.provider and agent.provider_by_label. See Proposal 0001 §11
// question #1.
func TestLoadAgentProviderByLabelCollision(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider: "claude"
  provider_by_label:
    "type:code":     "claude"
    default:         "claude"
  model: "sonnet"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "agent.provider") || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v; want provider+map collision", err)
	}
}

// TestLoadAgentModelByLabelCollision rejects scalar+map for agent.model.
func TestLoadAgentModelByLabelCollision(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider: "claude"
  model: "sonnet"
  model_by_label:
    "type:code": "sonnet"
    default:     "sonnet"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "agent.model") || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v; want model+map collision", err)
	}
}

// TestLoadAgentProviderByLabelMissingDefault rejects a per-axis provider
// map without a "default" key.
func TestLoadAgentProviderByLabelMissingDefault(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider_by_label:
    "type:code": "claude"
  model: "sonnet"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "provider_by_label") || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v; want missing-default", err)
	}
}

// TestLoadAgentModelByLabelMissingDefault rejects a per-axis model map
// without a "default" key.
func TestLoadAgentModelByLabelMissingDefault(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider: "claude"
  model_by_label:
    "type:code": "sonnet"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "model_by_label") || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v; want missing-default", err)
	}
}

func TestLoadAgentReasoningEffortByLabelCollision(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider: "codex"
  model: "gpt-5.5"
  reasoning_effort: "medium"
  reasoning_effort_by_label:
    "type:mockup": "medium"
    default:        "medium"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "agent.reasoning_effort") || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v; want reasoning effort+map collision", err)
	}
}

func TestLoadAgentReasoningEffortByLabelMissingDefault(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider: "codex"
  model: "gpt-5.5"
  reasoning_effort_by_label:
    "type:mockup": "medium"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "reasoning_effort_by_label") || !strings.Contains(err.Error(), "default") {
		t.Fatalf("err = %v; want missing-default", err)
	}
}

func TestLoadAgentReasoningEffortInvalid(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider: "codex"
  model: "gpt-5.5"
  reasoning_effort: "mediumest"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "low|medium|high|xhigh") {
		t.Fatalf("err = %v; want reasoning effort enum violation", err)
	}
}

// TestLoadAgentProviderModelByLabelOnlyMap parses a fully per-axis agent
// section (no scalar provider/model) and asserts the maps round-trip.
func TestLoadAgentProviderModelByLabelOnlyMap(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider_by_label:
    "type:code":     "claude"
    "type:research": "codex"
    default:         "claude"
  model_by_label:
    "type:code":     "sonnet"
    "type:research": "gpt-5-codex"
    default:         "sonnet"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Provider != "" {
		t.Errorf("Provider = %q; want empty (map mode)", cfg.Agent.Provider)
	}
	if cfg.Agent.ProviderByLabel.IsEmpty() {
		t.Fatalf("ProviderByLabel empty")
	}
	if got := cfg.Agent.ProviderByLabel.Values["type:research"]; got != "codex" {
		t.Errorf("ProviderByLabel[type:research] = %q; want codex", got)
	}
	if cfg.Agent.ModelByLabel.IsEmpty() {
		t.Fatalf("ModelByLabel empty")
	}
	if got := cfg.Agent.ModelByLabel.Values["type:research"]; got != "gpt-5-codex" {
		t.Errorf("ModelByLabel[type:research] = %q; want gpt-5-codex", got)
	}
}

func TestLoadAgentReasoningEffortByLabelOnlyMap(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`agent:
  provider: "claude"
  model: "sonnet"
  timeout_seconds: 3600`,
		`agent:
  provider_by_label:
    "type:mockup": "codex"
    default:       "claude"
  model_by_label:
    "type:mockup": "gpt-5.5"
    default:       "sonnet"
  reasoning_effort_by_label:
    "type:mockup": "medium"
    default:       "medium"
  timeout_seconds: 3600`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.ReasoningEffort != "" {
		t.Errorf("ReasoningEffort = %q; want empty (map mode)", cfg.Agent.ReasoningEffort)
	}
	if cfg.Agent.ReasoningEffortByLabel.IsEmpty() {
		t.Fatalf("ReasoningEffortByLabel empty")
	}
	if got := cfg.Agent.ReasoningEffortByLabel.Values["type:mockup"]; got != "medium" {
		t.Errorf("ReasoningEffortByLabel[type:mockup] = %q; want medium", got)
	}
}

// TestLoadGitHubAuthModes covers the auth selector + per-mode required
// fields added by the App-installation wire-up. Each case mutates
// validConfigYAML's `github:` block.
func TestLoadGitHubAuthModes(t *testing.T) {
	type tc struct {
		name       string
		github     string
		wantErr    bool
		wantSubstr string
	}
	cases := []tc{
		{
			name: "default empty auth = pat",
			github: `github:
  token_env: "GITHUB_TOKEN"
  poll_interval_seconds: 30`,
		},
		{
			name: "explicit pat",
			github: `github:
  auth: "pat"
  token_env: "GITHUB_TOKEN"
  poll_interval_seconds: 30`,
		},
		{
			name: "app with all required fields",
			github: `github:
  auth: "app"
  app_id_env: "SG_APP_ID"
  installation_id_env: "SG_INSTALL_ID"
  private_key_path_env: "SG_APP_PEM"
  poll_interval_seconds: 30`,
		},
		{
			name: "app missing app_id_env",
			github: `github:
  auth: "app"
  installation_id_env: "SG_INSTALL_ID"
  private_key_path_env: "SG_APP_PEM"
  poll_interval_seconds: 30`,
			wantErr:    true,
			wantSubstr: "app_id_env",
		},
		{
			name: "app missing installation_id_env",
			github: `github:
  auth: "app"
  app_id_env: "SG_APP_ID"
  private_key_path_env: "SG_APP_PEM"
  poll_interval_seconds: 30`,
			wantErr:    true,
			wantSubstr: "installation_id_env",
		},
		{
			name: "app missing both pem env vars",
			github: `github:
  auth: "app"
  app_id_env: "SG_APP_ID"
  installation_id_env: "SG_INSTALL_ID"
  poll_interval_seconds: 30`,
			wantErr:    true,
			wantSubstr: "private_key_path_env or",
		},
		{
			name: "app with both path and pem env vars (mutex)",
			github: `github:
  auth: "app"
  app_id_env: "SG_APP_ID"
  installation_id_env: "SG_INSTALL_ID"
  private_key_path_env: "SG_APP_PEM"
  private_key_pem_env: "SG_APP_PEM_INLINE"
  poll_interval_seconds: 30`,
			wantErr:    true,
			wantSubstr: "mutually exclusive",
		},
		{
			name: "app with inline pem only",
			github: `github:
  auth: "app"
  app_id_env: "SG_APP_ID"
  installation_id_env: "SG_INSTALL_ID"
  private_key_pem_env: "SG_APP_PEM_INLINE"
  poll_interval_seconds: 30`,
		},
		{
			name: "unknown auth",
			github: `github:
  auth: "vault"
  token_env: "GITHUB_TOKEN"
  poll_interval_seconds: 30`,
			wantErr:    true,
			wantSubstr: "github.auth",
		},
	}
	original := `github:
  token_env: "GITHUB_TOKEN"
  poll_interval_seconds: 30`
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			body := strings.Replace(validConfigYAML(), original, c.github, 1)
			p := writeTempConfig(t, body)
			_, err := Load(p)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", c.wantSubstr)
				}
				if !strings.Contains(err.Error(), c.wantSubstr) {
					t.Fatalf("err = %v; want substring %q", err, c.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestLoadApprovalModeByLabelOnlyMap parses successfully.
func TestLoadApprovalModeByLabelOnlyMap(t *testing.T) {
	body := strings.Replace(validConfigYAML(),
		`approval:
  mode: "auto"
  command: "/symphony approve"
  require_write_permission: true`,
		`approval:
  mode_by_label:
    "type:marketing-ads": "gated"
    "type:code":          "auto"
    default:               "auto"
  command: "/symphony approve"
  require_write_permission: true`, 1)
	p := writeTempConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Approval.ModeByLabel.IsEmpty() {
		t.Fatalf("mode_by_label empty")
	}
	if got := cfg.Approval.ModeByLabel.Values["type:marketing-ads"]; got != "gated" {
		t.Fatalf("unexpected mode for marketing-ads: %q", got)
	}
}
