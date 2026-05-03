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
			name:    "bad full_name format",
			mutate:  func(s string) string { return strings.Replace(s, `full_name: "OWNER/REPO"`, `full_name: "no-slash"`, 1) },
			wantSub: "OWNER/REPO",
		},
		{
			name:    "missing local_path",
			mutate:  func(s string) string { return strings.Replace(s, `local_path: "/tmp/some-repo"`, `local_path: ""`, 1) },
			wantSub: "repo.local_path",
		},
		{
			name:    "bad poll_interval",
			mutate:  func(s string) string { return strings.Replace(s, `poll_interval_seconds: 30`, `poll_interval_seconds: -1`, 1) },
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
			name:    "bad redact pattern",
			mutate:  func(s string) string { return strings.Replace(s, `redact_patterns: ["sk-[A-Za-z0-9_-]+"]`, `redact_patterns: ["[unterminated"]`, 1) },
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
