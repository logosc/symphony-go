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
	if cfg.Repo.WorkflowFile == "" && cfg.Repo.WorkflowFiles.IsEmpty() {
		return fmt.Errorf("config: repo.workflow_file is required")
	}
	// Per-axis collision: scalar and map for the same knob must not both
	// be set. See Proposal 0001 §5.3.
	if cfg.Repo.WorkflowFile != "" && !cfg.Repo.WorkflowFiles.IsEmpty() {
		return fmt.Errorf("config: repo.workflow_file and repo.workflow_files are mutually exclusive")
	}
	if !cfg.Repo.WorkflowFiles.IsEmpty() && !cfg.Repo.WorkflowFiles.HasDefault() {
		return fmt.Errorf("config: repo.workflow_files must contain a \"default\" key")
	}

	// github
	switch cfg.GitHub.Auth {
	case "", "pat":
		if cfg.GitHub.TokenEnv == "" {
			return fmt.Errorf("config: github.token_env is required when github.auth is %q", cfg.GitHub.Auth)
		}
	case "app":
		if cfg.GitHub.AppIDEnv == "" {
			return fmt.Errorf("config: github.app_id_env is required when github.auth = \"app\"")
		}
		if cfg.GitHub.InstallationIDEnv == "" {
			return fmt.Errorf("config: github.installation_id_env is required when github.auth = \"app\"")
		}
		// Exactly one of private_key_path_env / private_key_pem_env must
		// be set.
		hasPath := cfg.GitHub.PrivateKeyPathEnv != ""
		hasPEM := cfg.GitHub.PrivateKeyPEMEnv != ""
		if !hasPath && !hasPEM {
			return fmt.Errorf("config: github.private_key_path_env or github.private_key_pem_env is required when github.auth = \"app\"")
		}
		if hasPath && hasPEM {
			return fmt.Errorf("config: github.private_key_path_env and github.private_key_pem_env are mutually exclusive")
		}
	default:
		return fmt.Errorf("config: github.auth %q must be one of \"\" (= pat), \"pat\", or \"app\"", cfg.GitHub.Auth)
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
	// Per-axis collision: scalar Mode and ModeByLabel must not both be set.
	// applyDefaults populates Mode="gated" when neither is set. To keep the
	// per-axis path usable, we treat an explicitly-empty Mode as the
	// "scalar not set" sentinel by deferring this check until after seeing
	// whether ModeByLabel is non-empty.
	if !cfg.Approval.ModeByLabel.IsEmpty() {
		// When the map is set, the scalar must be empty. Because
		// applyDefaults filled "gated" already, callers that want the map
		// path must leave Mode unset; we detect ambiguity by checking
		// whether Mode disagrees with the resolved default map value.
		// Pragmatic rule (matches workflow_files / commands_by_label
		// pattern): if both are present at YAML level, reject.
		if cfg.Approval.scalarModeExplicit {
			return fmt.Errorf("config: approval.mode and approval.mode_by_label are mutually exclusive")
		}
		if !cfg.Approval.ModeByLabel.HasDefault() {
			return fmt.Errorf("config: approval.mode_by_label must contain a \"default\" key")
		}
		for k, v := range cfg.Approval.ModeByLabel.Values {
			switch v {
			case "gated", "auto", "handoff":
			default:
				return fmt.Errorf("config: approval.mode_by_label[%q] %q must be gated|auto|handoff", k, v)
			}
		}
	} else {
		switch cfg.Approval.Mode {
		case "gated", "auto", "handoff":
		default:
			return fmt.Errorf("config: approval.mode %q must be gated|auto|handoff", cfg.Approval.Mode)
		}
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

	// claude / codex per-axis collisions. Mirror the workflow_files /
	// commands_by_label pattern: a non-empty map requires a "default" key
	// AND its scalar twin must be empty. See Proposal 0001 §5.3.
	if err := checkScalarMapCollision("claude.planning_tools", cfg.Claude.PlanningTools, cfg.Claude.PlanningToolsByLabel); err != nil {
		return err
	}
	if err := checkScalarMapCollision("claude.implementation_tools", cfg.Claude.ImplementationTools, cfg.Claude.ImplementationToolsByLabel); err != nil {
		return err
	}
	if err := checkScalarMapCollision("claude.review_tools", cfg.Claude.ReviewTools, cfg.Claude.ReviewToolsByLabel); err != nil {
		return err
	}
	if err := checkScalarMapCollision("claude.disallowed_tools", cfg.Claude.DisallowedTools, cfg.Claude.DisallowedToolsByLabel); err != nil {
		return err
	}
	if err := checkScalarMapCollision("codex.planning_args", cfg.Codex.PlanningArgs, cfg.Codex.PlanningArgsByLabel); err != nil {
		return err
	}
	if err := checkScalarMapCollision("codex.implementation_args", cfg.Codex.ImplementationArgs, cfg.Codex.ImplementationArgsByLabel); err != nil {
		return err
	}
	if err := checkScalarMapCollision("codex.review_args", cfg.Codex.ReviewArgs, cfg.Codex.ReviewArgsByLabel); err != nil {
		return err
	}

	// hooks
	if cfg.Hooks.TimeoutSeconds <= 0 {
		return fmt.Errorf("config: hooks.timeout_seconds must be > 0")
	}

	// validation
	if cfg.Validation.CommandTimeoutSeconds <= 0 {
		return fmt.Errorf("config: validation.command_timeout_seconds must be > 0")
	}
	if len(cfg.Validation.Commands) > 0 && !cfg.Validation.CommandsByLabel.IsEmpty() {
		return fmt.Errorf("config: validation.commands and validation.commands_by_label are mutually exclusive")
	}
	if !cfg.Validation.CommandsByLabel.IsEmpty() && !cfg.Validation.CommandsByLabel.HasDefault() {
		return fmt.Errorf("config: validation.commands_by_label must contain a \"default\" key")
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

// checkScalarMapCollision rejects configs that set both the scalar slice
// and the per-axis map for the same knob, and rejects per-axis maps
// missing a "default" key. The knob argument names the scalar field for
// the error message (e.g. "claude.planning_tools").
func checkScalarMapCollision(knob string, scalar []string, m OrderedMap[[]string]) error {
	if len(scalar) > 0 && !m.IsEmpty() {
		return fmt.Errorf("config: %s and %s_by_label are mutually exclusive", knob, knob)
	}
	if !m.IsEmpty() && !m.HasDefault() {
		return fmt.Errorf("config: %s_by_label must contain a \"default\" key", knob)
	}
	return nil
}

// fullNameRe matches GitHub OWNER/REPO with permissive characters.
var fullNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// validFullName reports whether s looks like an OWNER/REPO pair.
func validFullName(s string) bool {
	return fullNameRe.MatchString(s)
}
