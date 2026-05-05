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
	"strconv"

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
	// Orchestrator configures dispatch-loop knobs such as how many issues
	// may be in-flight simultaneously. Defaults preserve the historical
	// serial-dispatch behavior; see OrchestratorConfig.
	Orchestrator OrchestratorConfig `yaml:"orchestrator"`
}

// OrchestratorConfig configures dispatch-loop behavior.
type OrchestratorConfig struct {
	// MaxConcurrentJobs caps how many issues the orchestrator processes
	// simultaneously. Default 1 (serial; back-compat). Each in-flight job
	// owns its own goroutine, worktree, and Job state file.
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`
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
	// Token is the literal PAT value. PAT mode only. Mutually exclusive
	// with TokenEnv. Convenient for single-machine ops; for shared infra
	// or anywhere the config file might be checked in, use TokenEnv
	// instead so secrets don't sit in plaintext yaml.
	Token string `yaml:"token"`
	// TokenEnv names the environment variable holding the PAT. PAT mode
	// only. Mutually exclusive with Token.
	TokenEnv string `yaml:"token_env"`
	// AppID is the literal numeric App ID. App mode only. Mutually
	// exclusive with AppIDEnv. App ID is publicly visible in GitHub
	// URLs, not a secret.
	AppID int64 `yaml:"app_id"`
	// AppIDEnv names the environment variable holding the integer App
	// ID. App mode only. Mutually exclusive with AppID.
	AppIDEnv string `yaml:"app_id_env"`
	// InstallationID is the literal numeric installation ID. App mode
	// only. Mutually exclusive with InstallationIDEnv. Installation ID
	// is visible in the App's installation URL, not a secret.
	InstallationID int64 `yaml:"installation_id"`
	// InstallationIDEnv names the environment variable holding the
	// integer installation ID. App mode only. Mutually exclusive with
	// InstallationID.
	InstallationIDEnv string `yaml:"installation_id_env"`
	// PrivateKeyPath is the literal absolute path to the App's .pem
	// private key file. App mode only. Mutually exclusive with
	// PrivateKeyPathEnv, PrivateKeyPEM, and PrivateKeyPEMEnv. The path
	// itself is not a secret, only the file's contents are; ensure the
	// file is chmod 600.
	PrivateKeyPath string `yaml:"private_key_path"`
	// PrivateKeyPathEnv names the environment variable holding the
	// absolute path to the App's .pem private key file. App mode only.
	// Mutually exclusive with PrivateKeyPath, PrivateKeyPEM, and
	// PrivateKeyPEMEnv. Exactly one of the four PEM-source fields must
	// be set in App mode.
	PrivateKeyPathEnv string `yaml:"private_key_path_env"`
	// PrivateKeyPEM is the literal PEM contents inline. App mode only.
	// Mutually exclusive with PrivateKeyPath, PrivateKeyPathEnv, and
	// PrivateKeyPEMEnv. Storing PEM contents in a yaml file is sensitive
	// — ensure the config file is chmod 600 and never checked in.
	PrivateKeyPEM string `yaml:"private_key_pem"`
	// PrivateKeyPEMEnv names the environment variable whose value is
	// the literal PEM contents (the `-----BEGIN RSA PRIVATE KEY-----`
	// block, newlines preserved). Escape hatch for environments
	// without a filesystem (Cloudflare Workers etc.). Mutually exclusive
	// with PrivateKeyPath, PrivateKeyPathEnv, and PrivateKeyPEM.
	PrivateKeyPEMEnv string `yaml:"private_key_pem_env"`
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
	// RequireToken switches the gated-mode approval gate from a fixed
	// slash command (Command) to a per-plan random numeric token the
	// orchestrator generates and embeds in the plan comment. Approvers
	// must comment that exact token. Forces operators to actually read
	// the plan (you cannot auto-approve from muscle memory) and
	// invalidates approvals from a stale plan when reconcile re-plans.
	// Default false; back-compat is preserved.
	RequireToken bool `yaml:"require_token"`
	// IgnoredUsers is a list of GitHub logins whose comments must never be
	// treated as approvals. Matching is case-insensitive. Defaults to the
	// well-known orchestrator bot logins (`symphony-go[bot]`,
	// `github-actions[bot]`) so a prompt-injected issue body cannot make
	// the orchestrator's own bot self-approve when running as a GitHub App.
	IgnoredUsers []string `yaml:"ignored_users"`
	// TrustedUsers is a list of GitHub logins whose exact approval comments
	// are accepted without a collaborator permission lookup. This is intended
	// for approval bridge apps that already authenticate the human operator
	// before posting the configured approval command.
	TrustedUsers []string `yaml:"trusted_users"`
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
	Provider string `yaml:"provider"`
	// ProviderByLabel is the optional per-axis variant of Provider. When
	// set, main.go pre-builds a runner per key and the orchestrator
	// dispatches each job's runs to the runner matching Job.AxisKey.
	// Required to carry a "default" key. Mutually exclusive with Provider
	// (rejected at parse). Each value must be one of `claude` or `codex`.
	// See Proposal 0001 §11 question #1.
	ProviderByLabel OrderedMap[string] `yaml:"provider_by_label"`
	Model           string             `yaml:"model"`
	// ModelByLabel is the optional per-axis variant of Model. When set,
	// main.go pre-builds a runner per key (paired with the per-axis
	// provider when present). Required to carry a "default" key.
	// Mutually exclusive with Model (rejected at parse). See Proposal
	// 0001 §11 question #1.
	ModelByLabel OrderedMap[string] `yaml:"model_by_label"`
	// ReasoningEffort is passed to model providers that expose a separate
	// reasoning-effort knob. For Codex CLI this is sent as
	// `-c model_reasoning_effort="<value>"`; it is intentionally separate
	// from Model so configs do not encode effort into model names.
	ReasoningEffort string `yaml:"reasoning_effort"`
	// ReasoningEffortByLabel is the optional per-axis variant of
	// ReasoningEffort. Required to carry a "default" key and mutually
	// exclusive with ReasoningEffort.
	ReasoningEffortByLabel OrderedMap[string] `yaml:"reasoning_effort_by_label"`
	TimeoutSeconds         int                `yaml:"timeout_seconds"`
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
	if cfg.GitHub.Auth == "" {
		cfg.GitHub.Auth = "pat"
	}
	// Default TokenEnv only when no inline token was provided; otherwise
	// validate would flag both as set.
	if cfg.GitHub.TokenEnv == "" && cfg.GitHub.Token == "" {
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
		// Default to app-server: matches upstream openai/symphony's
		// default and avoids stream-idle timeouts on long planning runs.
		// Operators can opt back into the simpler one-shot subprocess
		// behavior by setting `codex.mode: "exec"` explicitly.
		// See proposal 0005 §4.1.
		cfg.Codex.Mode = "app-server"
	}
	if cfg.Hooks.TimeoutSeconds == 0 {
		cfg.Hooks.TimeoutSeconds = 60
	}
	if cfg.Validation.CommandTimeoutSeconds == 0 {
		cfg.Validation.CommandTimeoutSeconds = 900
	}
	if cfg.Orchestrator.MaxConcurrentJobs == 0 {
		cfg.Orchestrator.MaxConcurrentJobs = 1
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

// ResolveToken returns the GitHub PAT, preferring the inline `token`
// field over `token_env` lookup. Validate has already enforced that
// exactly one is set; this assumes a `pat`-mode config.
func (g GitHubConfig) ResolveToken() (string, error) {
	if g.Token != "" {
		return g.Token, nil
	}
	if g.TokenEnv == "" {
		return "", fmt.Errorf("github: neither token nor token_env is set")
	}
	v := os.Getenv(g.TokenEnv)
	if v == "" {
		return "", fmt.Errorf("github: env %q (token_env) is empty", g.TokenEnv)
	}
	return v, nil
}

// ResolveAppID returns the App ID, preferring the inline `app_id` field
// over `app_id_env`. Returns 0,err if neither resolves to a positive
// int64.
func (g GitHubConfig) ResolveAppID() (int64, error) {
	if g.AppID > 0 {
		return g.AppID, nil
	}
	if g.AppIDEnv == "" {
		return 0, fmt.Errorf("github: neither app_id nor app_id_env is set")
	}
	raw := os.Getenv(g.AppIDEnv)
	if raw == "" {
		return 0, fmt.Errorf("github: env %q (app_id_env) is empty", g.AppIDEnv)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("github: env %q (app_id_env) must parse as int64: %w", g.AppIDEnv, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("github: env %q (app_id_env) must be positive, got %d", g.AppIDEnv, v)
	}
	return v, nil
}

// ResolveInstallationID returns the installation ID, preferring the
// inline `installation_id` field over `installation_id_env`.
func (g GitHubConfig) ResolveInstallationID() (int64, error) {
	if g.InstallationID > 0 {
		return g.InstallationID, nil
	}
	if g.InstallationIDEnv == "" {
		return 0, fmt.Errorf("github: neither installation_id nor installation_id_env is set")
	}
	raw := os.Getenv(g.InstallationIDEnv)
	if raw == "" {
		return 0, fmt.Errorf("github: env %q (installation_id_env) is empty", g.InstallationIDEnv)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("github: env %q (installation_id_env) must parse as int64: %w", g.InstallationIDEnv, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("github: env %q (installation_id_env) must be positive, got %d", g.InstallationIDEnv, v)
	}
	return v, nil
}

// ResolvePrivateKey returns the App's PEM bytes from whichever of the
// four sources is configured: `private_key_path` (inline path),
// `private_key_path_env` (path via env), `private_key_pem` (inline PEM),
// or `private_key_pem_env` (PEM via env). The second return value is a
// human-readable source label suitable for logs (the path on disk, or
// "<inline>" for inline PEM). Validate has already enforced exactly one
// source is set.
func (g GitHubConfig) ResolvePrivateKey() ([]byte, string, error) {
	switch {
	case g.PrivateKeyPath != "":
		return readPEMFile(g.PrivateKeyPath)
	case g.PrivateKeyPathEnv != "":
		path := os.Getenv(g.PrivateKeyPathEnv)
		if path == "" {
			return nil, "", fmt.Errorf("github: env %q (private_key_path_env) is empty", g.PrivateKeyPathEnv)
		}
		return readPEMFile(path)
	case g.PrivateKeyPEM != "":
		return []byte(g.PrivateKeyPEM), "<inline>", nil
	case g.PrivateKeyPEMEnv != "":
		raw := os.Getenv(g.PrivateKeyPEMEnv)
		if raw == "" {
			return nil, "", fmt.Errorf("github: env %q (private_key_pem_env) is empty", g.PrivateKeyPEMEnv)
		}
		return []byte(raw), "<inline-env:" + g.PrivateKeyPEMEnv + ">", nil
	}
	return nil, "", fmt.Errorf("github: no private key source configured")
}

// readPEMFile reads a PEM file and returns its bytes plus the path as a
// source label.
func readPEMFile(path string) ([]byte, string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("read app private key at %q: %w", path, err)
	}
	return body, path, nil
}
