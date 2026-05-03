package config

import (
	"fmt"
	"regexp"
)

// Validate enforces required-field and enumeration constraints on cfg.
// Returns the first error encountered with the offending field name in
// the message.
func Validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config: nil config")
	}

	// repo
	if cfg.Repo.FullName == "" {
		return fmt.Errorf("config: repo.full_name is required")
	}
	if !validFullName(cfg.Repo.FullName) {
		return fmt.Errorf("config: repo.full_name %q must be OWNER/REPO", cfg.Repo.FullName)
	}
	if cfg.Repo.LocalPath == "" {
		return fmt.Errorf("config: repo.local_path is required")
	}
	if cfg.Repo.BaseBranch == "" {
		return fmt.Errorf("config: repo.base_branch is required")
	}
	if cfg.Repo.WorkflowFile == "" {
		return fmt.Errorf("config: repo.workflow_file is required")
	}

	// github
	if cfg.GitHub.TokenEnv == "" {
		return fmt.Errorf("config: github.token_env is required")
	}
	if cfg.GitHub.PollIntervalSeconds <= 0 {
		return fmt.Errorf("config: github.poll_interval_seconds must be > 0")
	}

	// labels
	if cfg.Labels.Ready == "" {
		return fmt.Errorf("config: labels.ready is required")
	}
	if cfg.Labels.Planning == "" {
		return fmt.Errorf("config: labels.planning is required")
	}
	if cfg.Labels.AwaitingApproval == "" {
		return fmt.Errorf("config: labels.awaiting_approval is required")
	}
	if cfg.Labels.Implementing == "" {
		return fmt.Errorf("config: labels.implementing is required")
	}
	if cfg.Labels.PRReady == "" {
		return fmt.Errorf("config: labels.pr_ready is required")
	}
	if cfg.Labels.Failed == "" {
		return fmt.Errorf("config: labels.failed is required")
	}
	if cfg.Labels.Blocked == "" {
		return fmt.Errorf("config: labels.blocked is required")
	}
	if cfg.Labels.Stop == "" {
		return fmt.Errorf("config: labels.stop is required")
	}

	// approval
	switch cfg.Approval.Mode {
	case "gated", "auto", "handoff":
	default:
		return fmt.Errorf("config: approval.mode %q must be gated|auto|handoff", cfg.Approval.Mode)
	}
	if cfg.Approval.Command == "" {
		return fmt.Errorf("config: approval.command is required")
	}

	// auto (validate only when mode is auto, but cheap structural checks always)
	switch cfg.Auto.FallbackOnReject {
	case "gated", "block":
	default:
		return fmt.Errorf("config: auto.fallback_on_reject %q must be gated|block", cfg.Auto.FallbackOnReject)
	}
	switch cfg.Auto.FallbackOnNoRuleMatch {
	case "gated", "block":
	default:
		return fmt.Errorf("config: auto.fallback_on_no_rule_match %q must be gated|block", cfg.Auto.FallbackOnNoRuleMatch)
	}
	if cfg.Auto.MaxDiffDriftFiles < 0 {
		return fmt.Errorf("config: auto.max_diff_drift_files must be >= 0")
	}
	for i, r := range cfg.Auto.Rules {
		if r.MaxPlanFilesClaimed < 0 {
			return fmt.Errorf("config: auto.rules[%d].max_plan_files_claimed must be >= 0", i)
		}
	}
	if cfg.Auto.Reviewer.Provider != "" {
		switch cfg.Auto.Reviewer.Provider {
		case "claude", "codex":
		default:
			return fmt.Errorf("config: auto.reviewer.provider %q must be claude|codex", cfg.Auto.Reviewer.Provider)
		}
	}
	if cfg.Auto.Reviewer.TimeoutSeconds < 0 {
		return fmt.Errorf("config: auto.reviewer.timeout_seconds must be >= 0")
	}

	// agent
	switch cfg.Agent.Provider {
	case "claude", "codex":
	default:
		return fmt.Errorf("config: agent.provider %q must be claude|codex", cfg.Agent.Provider)
	}
	if cfg.Agent.TimeoutSeconds <= 0 {
		return fmt.Errorf("config: agent.timeout_seconds must be > 0")
	}

	// codex mode
	switch cfg.Codex.Mode {
	case "exec", "app-server":
	default:
		return fmt.Errorf("config: codex.mode %q must be exec|app-server", cfg.Codex.Mode)
	}

	// hooks
	if cfg.Hooks.TimeoutSeconds <= 0 {
		return fmt.Errorf("config: hooks.timeout_seconds must be > 0")
	}

	// validation
	if cfg.Validation.CommandTimeoutSeconds <= 0 {
		return fmt.Errorf("config: validation.command_timeout_seconds must be > 0")
	}

	// audit redact patterns must compile
	for i, p := range cfg.Audit.RedactPatterns {
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("config: audit.redact_patterns[%d] %q: %w", i, p, err)
		}
	}
	// env block patterns must compile
	for i, p := range cfg.Env.BlockPatterns {
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("config: env.block_patterns[%d] %q: %w", i, p, err)
		}
	}

	return nil
}

// fullNameRe matches GitHub OWNER/REPO with permissive characters.
var fullNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// validFullName reports whether s looks like an OWNER/REPO pair.
func validFullName(s string) bool {
	return fullNameRe.MatchString(s)
}
