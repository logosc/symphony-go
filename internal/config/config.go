// Package config parses, validates, and loads symphony-go's external
// configuration: ~/.symphony-go/config.yml and the in-repo WORKFLOW.md
// prompt template. It also implements the config integrity guard (SHA-256
// drift detection and the "config must not be inside the repo" invariant)
// described in SPEC §2.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is the parsed top-level config.yml document.
type Config struct {
	Repo       RepoConfig       `yaml:"repo"`
	GitHub     GitHubConfig     `yaml:"github"`
	Labels     LabelsConfig     `yaml:"labels"`
	Approval   ApprovalConfig   `yaml:"approval"`
	Auto       AutoConfig       `yaml:"auto"`
	Agent      AgentConfig      `yaml:"agent"`
	Claude     ClaudeConfig     `yaml:"claude"`
	Codex      CodexConfig      `yaml:"codex"`
	Env        EnvConfig        `yaml:"env"`
	Hooks      HooksConfig      `yaml:"hooks"`
	Validation ValidationConfig `yaml:"validation"`
	Audit      AuditConfig      `yaml:"audit"`
}

// RepoConfig configures the target GitHub repository and local clone.
type RepoConfig struct {
	FullName     string `yaml:"full_name"`
	BaseBranch   string `yaml:"base_branch"`
	LocalPath    string `yaml:"local_path"`
	WorkflowFile string `yaml:"workflow_file"`
	// WorkflowFiles is the optional per-axis variant of WorkflowFile.
	// When set, the orchestrator picks a body keyed by the issue's labels
	// using ResolveAxis. Required to carry a "default" key. Mutually
	// exclusive with WorkflowFile (rejected at parse). See Proposal 0001.
	WorkflowFiles OrderedMap[string] `yaml:"workflow_files"`
}

// GitHubConfig configures GitHub API access and polling cadence.
//
// Two auth schemes are supported:
//
//   - `auth: pat` (default): use a personal access token from
//     `os.Getenv(token_env)`. Backward-compatible with pre-App configs
//     that set only `token_env`.
//   - `auth: app`: authenticate as a GitHub App installation. The App ID
//     and installation ID are read as integers from the environment via
//     `app_id_env` and `installation_id_env`; the .pem private key is
//     read from the path stored in `os.Getenv(private_key_path_env)`.
//     The orchestrator never persists the short-lived installation
//     access token.
type GitHubConfig struct {
	// Auth selects the credential scheme. Empty or "pat" → personal
	// access token (back-compat default). "app" → GitHub App
	// installation.
	Auth string `yaml:"auth"`
	// TokenEnv names the environment variable holding the PAT. Required
	// in `pat` mode.
	TokenEnv string `yaml:"token_env"`
	// AppIDEnv names the environment variable holding the integer App ID.
	// Required in `app` mode.
	AppIDEnv string `yaml:"app_id_env"`
	// InstallationIDEnv names the environment variable holding the
	// integer installation ID. Required in `app` mode.
	InstallationIDEnv string `yaml:"installation_id_env"`
	// PrivateKeyPathEnv names the environment variable holding the
	// absolute path to the App's .pem private key file. Required in
	// `app` mode. The file is read at startup and not retained on disk
	// after that.
	PrivateKeyPathEnv string `yaml:"private_key_path_env"`
	// PollIntervalSeconds is the cadence between dispatch ticks.
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`
}

// LabelsConfig is the GitHub label vocabulary used as the visible state
// machine. Every field is a fully-qualified label name.
type LabelsConfig struct {
	Ready            string `yaml:"ready"`
	Planning         string `yaml:"planning"`
	AwaitingApproval string `yaml:"awaiting_approval"`
	Implementing     string `yaml:"implementing"`
	PRReady          string `yaml:"pr_ready"`
	Failed           string `yaml:"failed"`
	Blocked          string `yaml:"blocked"`
	Stop             string `yaml:"stop"`
}

// ApprovalConfig configures the global approval policy.
type ApprovalConfig struct {
	Mode                   string `yaml:"mode"`
	Command                string `yaml:"command"`
	RequireWritePermission bool   `yaml:"require_write_permission"`
	// IgnoredUsers is a list of GitHub logins whose comments must never be
	// treated as approvals. Matching is case-insensitive. Defaults to the
	// well-known orchestrator bot logins (`symphony-go[bot]`,
	// `github-actions[bot]`) so a prompt-injected issue body cannot make
	// the orchestrator's own bot self-approve when running as a GitHub App.
	IgnoredUsers []string `yaml:"ignored_users"`
	// ModeByLabel is the optional per-axis variant of Mode. When set, the
	// orchestrator resolves an approval mode keyed by Job.AxisKey instead
	// of Mode. Required to carry a "default" key. Mutually exclusive with
	// Mode (rejected at parse). Each value must be one of `gated`, `auto`,
	// or `handoff`. See Proposal 0001 §5.
	ModeByLabel OrderedMap[string] `yaml:"mode_by_label"`

	// scalarModeExplicit is true when the YAML document set
	// approval.mode literally (vs the applyDefaults fallback). Used by
	// Validate to detect scalar+map ambiguity for this knob without
	// tripping on the legacy default. Populated by UnmarshalYAML.
	scalarModeExplicit bool `yaml:"-"`
}

// UnmarshalYAML records whether `mode` was set literally so the per-axis
// collision check can tell it apart from the applyDefaults fallback.
func (a *ApprovalConfig) UnmarshalYAML(node *yaml.Node) error {
	type rawApproval ApprovalConfig
	var raw rawApproval
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*a = ApprovalConfig(raw)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		if k.Value == "mode" {
			a.scalarModeExplicit = true
			break
		}
	}
	return nil
}

// AutoConfig configures auto-approval: rules engine, reviewer agent,
// fallback behavior, and post-implementation diff verification.
type AutoConfig struct {
	Rules                 []AutoRule     `yaml:"rules"`
	Reviewer              ReviewerConfig `yaml:"reviewer"`
	FallbackOnReject      string         `yaml:"fallback_on_reject"`
	FallbackOnNoRuleMatch string         `yaml:"fallback_on_no_rule_match"`
	VerifyDiffMatchesPlan bool           `yaml:"verify_diff_matches_plan"`
	MaxDiffDriftFiles     int            `yaml:"max_diff_drift_files"`
}

// AutoRule is a single auto-approval rule. Rules are evaluated in order and
// first match wins.
type AutoRule struct {
	IssueLabels         []string `yaml:"issue_labels"`
	MaxPlanFilesClaimed int      `yaml:"max_plan_files_claimed"`
	ReviewerRequired    bool     `yaml:"reviewer_required"`
}

// ReviewerConfig configures the reviewer agent used in auto mode when a
// rule sets reviewer_required: true.
type ReviewerConfig struct {
	Provider       string `yaml:"provider"`
	Model          string `yaml:"model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// AgentConfig configures the primary agent (planner + implementer).
type AgentConfig struct {
	Provider       string `yaml:"provider"`
	Model          string `yaml:"model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	// MultiTurn enables provider-driven multi-turn continuation during the
	// implementation phase. The orchestrator only honors this when the
	// runner reports support (currently `codex.mode == "app-server"`); for
	// other runners or modes the flag is a no-op and a single Run is used.
	MultiTurn bool `yaml:"multi_turn"`
	// MaxTurns is the upper bound on the number of turns the orchestrator
	// will drive in a multi-turn implementation session. Defaults to 8.
	// Also used by the Claude runner via claude.max_turns; this field is
	// the cross-provider knob.
	MaxTurns int `yaml:"max_turns"`
	// StallTimeoutSeconds is the per-run event-inactivity watchdog. When >0,
	// each agent runner cancels the subprocess (via the existing SIGTERM
	// path) if no event/line is observed for this many seconds, marking the
	// run as Success=false. The default (0) disables stall detection. See
	// SPEC §17.
	StallTimeoutSeconds int `yaml:"stall_timeout_seconds"`
}

// ClaudeConfig is the provider-specific configuration for the Claude
// runner. The four `*_by_label` fields are the optional per-axis
// variants of their scalar twins; when set, the runner picks a tool
// list keyed by Job.AxisKey instead of the scalar slice. Each map must
// carry a "default" key and is mutually exclusive with its scalar
// counterpart (rejected at parse). See Proposal 0001.
type ClaudeConfig struct {
	MaxTurns            int      `yaml:"max_turns"`
	PlanningTools       []string `yaml:"planning_tools"`
	ImplementationTools []string `yaml:"implementation_tools"`
	ReviewTools         []string `yaml:"review_tools"`
	DisallowedTools     []string `yaml:"disallowed_tools"`
	// PlanningToolsByLabel is the per-axis variant of PlanningTools.
	PlanningToolsByLabel OrderedMap[[]string] `yaml:"planning_tools_by_label"`
	// ImplementationToolsByLabel is the per-axis variant of
	// ImplementationTools.
	ImplementationToolsByLabel OrderedMap[[]string] `yaml:"implementation_tools_by_label"`
	// ReviewToolsByLabel is the per-axis variant of ReviewTools.
	ReviewToolsByLabel OrderedMap[[]string] `yaml:"review_tools_by_label"`
	// DisallowedToolsByLabel is the per-axis variant of DisallowedTools.
	DisallowedToolsByLabel OrderedMap[[]string] `yaml:"disallowed_tools_by_label"`
}

// CodexConfig is the provider-specific configuration for the Codex runner.
// The three `*_by_label` fields are the optional per-axis variants of
// their scalar twins; see ClaudeConfig for the contract.
type CodexConfig struct {
	Mode               string   `yaml:"mode"`
	PlanningArgs       []string `yaml:"planning_args"`
	ImplementationArgs []string `yaml:"implementation_args"`
	ReviewArgs         []string `yaml:"review_args"`
	// PlanningArgsByLabel is the per-axis variant of PlanningArgs.
	PlanningArgsByLabel OrderedMap[[]string] `yaml:"planning_args_by_label"`
	// ImplementationArgsByLabel is the per-axis variant of
	// ImplementationArgs.
	ImplementationArgsByLabel OrderedMap[[]string] `yaml:"implementation_args_by_label"`
	// ReviewArgsByLabel is the per-axis variant of ReviewArgs.
	ReviewArgsByLabel OrderedMap[[]string] `yaml:"review_args_by_label"`
}

// EnvConfig configures the agent subprocess environment: which env vars
// pass through (allowlist) and which patterns are dropped (block_patterns).
type EnvConfig struct {
	Allowlist     []string `yaml:"allowlist"`
	BlockPatterns []string `yaml:"block_patterns"`
}

// HooksConfig configures workspace hooks. Each script runs as `bash -lc`
// with cwd at the worktree repo dir.
type HooksConfig struct {
	AfterCreate    string `yaml:"after_create"`
	BeforeRun      string `yaml:"before_run"`
	AfterRun       string `yaml:"after_run"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// ValidationConfig configures the post-implementation validation gate.
type ValidationConfig struct {
	Commands              []string `yaml:"commands"`
	CommandTimeoutSeconds int      `yaml:"command_timeout_seconds"`
	// CommandsByLabel is the optional per-axis variant of Commands. When
	// set, the orchestrator runs the slice keyed by Job.AxisKey instead
	// of Commands. Required to carry a "default" key. Mutually exclusive
	// with Commands (rejected at parse). See Proposal 0001.
	CommandsByLabel OrderedMap[[]string] `yaml:"commands_by_label"`
}

// AuditConfig configures audit-log redaction.
type AuditConfig struct {
	RedactPatterns []string `yaml:"redact_patterns"`
}

// envVarPattern matches `$VAR` or `${VAR}` indirection sequences in string
// fields. Used to expand environment variables across the parsed config.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// Load reads, parses, expands env-var indirection, applies defaults, and
// validates a config.yml file at path.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve path %q: %w", path, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", abs, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", abs, err)
	}
	expandEnv(&cfg)
	applyDefaults(&cfg)
	if err := Validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyDefaults populates fields not explicitly set in the YAML document.
// Mirrors the example config in SPEC §2.
func applyDefaults(cfg *Config) {
	if cfg.Repo.BaseBranch == "" {
		cfg.Repo.BaseBranch = "main"
	}
	if cfg.Repo.WorkflowFile == "" && cfg.Repo.WorkflowFiles.IsEmpty() {
		cfg.Repo.WorkflowFile = "WORKFLOW.md"
	}
	if cfg.GitHub.TokenEnv == "" {
		cfg.GitHub.TokenEnv = "GITHUB_TOKEN"
	}
	if cfg.GitHub.PollIntervalSeconds == 0 {
		cfg.GitHub.PollIntervalSeconds = 30
	}
	if cfg.Labels.Ready == "" {
		cfg.Labels.Ready = "symphony:ready"
	}
	if cfg.Labels.Planning == "" {
		cfg.Labels.Planning = "symphony:planning"
	}
	if cfg.Labels.AwaitingApproval == "" {
		cfg.Labels.AwaitingApproval = "symphony:awaiting-approval"
	}
	if cfg.Labels.Implementing == "" {
		cfg.Labels.Implementing = "symphony:implementing"
	}
	if cfg.Labels.PRReady == "" {
		cfg.Labels.PRReady = "symphony:pr-ready"
	}
	if cfg.Labels.Failed == "" {
		cfg.Labels.Failed = "symphony:failed"
	}
	if cfg.Labels.Blocked == "" {
		cfg.Labels.Blocked = "symphony:blocked"
	}
	if cfg.Labels.Stop == "" {
		cfg.Labels.Stop = "symphony:stop"
	}
	if cfg.Approval.Mode == "" {
		cfg.Approval.Mode = "gated"
	}
	if cfg.Approval.Command == "" {
		cfg.Approval.Command = "/symphony approve"
	}
	if cfg.Approval.IgnoredUsers == nil {
		cfg.Approval.IgnoredUsers = []string{"symphony-go[bot]", "github-actions[bot]"}
	}
	if cfg.Auto.FallbackOnReject == "" {
		cfg.Auto.FallbackOnReject = "gated"
	}
	if cfg.Auto.FallbackOnNoRuleMatch == "" {
		cfg.Auto.FallbackOnNoRuleMatch = "gated"
	}
	if cfg.Auto.MaxDiffDriftFiles == 0 {
		cfg.Auto.MaxDiffDriftFiles = 2
	}
	if cfg.Auto.Reviewer.TimeoutSeconds == 0 {
		cfg.Auto.Reviewer.TimeoutSeconds = 600
	}
	if cfg.Agent.TimeoutSeconds == 0 {
		cfg.Agent.TimeoutSeconds = 3600
	}
	if cfg.Agent.MaxTurns == 0 {
		cfg.Agent.MaxTurns = 8
	}
	if cfg.Claude.MaxTurns == 0 {
		cfg.Claude.MaxTurns = 20
	}
	if cfg.Codex.Mode == "" {
		cfg.Codex.Mode = "exec"
	}
	if cfg.Hooks.TimeoutSeconds == 0 {
		cfg.Hooks.TimeoutSeconds = 60
	}
	if cfg.Validation.CommandTimeoutSeconds == 0 {
		cfg.Validation.CommandTimeoutSeconds = 900
	}
}

// expandEnv walks every string field in cfg and replaces `$VAR` /
// `${VAR}` references with the corresponding environment variable value.
// Undefined variables expand to the empty string. Note: the names stored
// in fields like GitHubConfig.TokenEnv are themselves variable *names*,
// not values to expand — those fields don't typically contain `$`.
func expandEnv(cfg *Config) {
	expandStrings(reflect.ValueOf(cfg).Elem())
}

// expandStrings recurses through a value and rewrites every settable
// string-or-string-slice using expandValue.
func expandStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			expandStrings(v.Field(i))
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String {
			for i := 0; i < v.Len(); i++ {
				v.Index(i).SetString(expandValue(v.Index(i).String()))
			}
			return
		}
		for i := 0; i < v.Len(); i++ {
			expandStrings(v.Index(i))
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(expandValue(v.String()))
		}
	}
}

// expandValue substitutes `$VAR` and `${VAR}` references in s using
// os.Getenv. Strings without any `$` are returned unchanged.
func expandValue(s string) string {
	if s == "" || !containsDollar(s) {
		return s
	}
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := envVarPattern.FindStringSubmatch(match)
		name := groups[1]
		if name == "" {
			name = groups[2]
		}
		return os.Getenv(name)
	})
}

// containsDollar reports whether s contains a `$` byte. Cheap pre-filter
// before running the regex.
func containsDollar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '$' {
			return true
		}
	}
	return false
}
