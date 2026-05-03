package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	gh "github.com/logosc/symphony-go/internal/github"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/types"
)

// Doctor runs the SPEC §12 checks against cfg and the live environment.
// Returns a multi-error (errors.Join) on any failure, nil on success.
//
// Doctor is intentionally permissive: it does not require the
// orchestrator to already be wired up. It expects cfg to be already
// validated by config.Validate (Load does this for you).
func Doctor(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("doctor: nil config")
	}
	var errs []error

	// 1, 2 happen at config load + integrity-guard construction time.
	// Re-run the path-inside-repo check here so doctor catches it without
	// requiring the caller to have built a guard first.
	absCfg, _ := filepath.Abs(os.Getenv("SYMPHONY_GO_CONFIG"))
	absRepo, _ := filepath.Abs(cfg.Repo.LocalPath)
	if absCfg != "" && absRepo != "" {
		if pathInside(absCfg, absRepo) {
			errs = append(errs, fmt.Errorf("doctor: SYMPHONY_GO_CONFIG %q is under repo.local_path %q", absCfg, absRepo))
		}
	}

	// 3: workflow file exists & renders. Skipped when only the per-axis
	// workflow_files map is set (the per-axis check below handles each
	// referenced file individually).
	if cfg.Repo.WorkflowFile != "" {
		wfPath := filepath.Join(cfg.Repo.LocalPath, cfg.Repo.WorkflowFile)
		if body, err := config.LoadWorkflow(wfPath); err != nil {
			errs = append(errs, fmt.Errorf("doctor: load workflow: %w", err))
		} else if _, err := config.RenderPrompt(body, types.Issue{Number: 0}, 0); err != nil {
			errs = append(errs, fmt.Errorf("doctor: render workflow: %w", err))
		}
	}

	// 4: GitHub auth — branches between PAT and App per cfg.GitHub.Auth.
	// Surface the active mode so operators can confirm what doctor exercised.
	client, modeSummary, authErr := doctorResolveGitHubAuth(ctx, cfg)
	if modeSummary != "" {
		slog.Info("doctor: github auth resolved", "summary", modeSummary)
	}
	if authErr != nil {
		errs = append(errs, fmt.Errorf("doctor: github auth: %w", authErr))
	}

	// 5, 6, 7: live GitHub probe — list ready issues exercises auth, repo
	// reachability, and the ready label all at once.
	if client != nil {
		if _, err := client.ListReadyIssues(ctx, cfg.Labels.Ready); err != nil {
			errs = append(errs, fmt.Errorf("doctor: list ready issues (is App installed on %s?): %w",
				cfg.Repo.FullName, err))
		}
	}

	// 8: git, agent in PATH.
	if _, err := exec.LookPath("git"); err != nil {
		errs = append(errs, fmt.Errorf("doctor: git not in PATH: %w", err))
	}
	if cfg.Agent.Provider != "" {
		if _, err := exec.LookPath(cfg.Agent.Provider); err != nil {
			errs = append(errs, fmt.Errorf("doctor: agent %q not in PATH: %w", cfg.Agent.Provider, err))
		}
	}

	// 9, 10: local repo + base branch.
	gitDir := filepath.Join(cfg.Repo.LocalPath, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		errs = append(errs, fmt.Errorf("doctor: %s is not a git repo: %w", cfg.Repo.LocalPath, err))
	} else {
		// origin remote.
		out, err := exec.CommandContext(ctx, "git", "-C", cfg.Repo.LocalPath, "remote", "get-url", "origin").CombinedOutput()
		if err != nil {
			errs = append(errs, fmt.Errorf("doctor: origin remote: %w (%s)", err, out))
		}
		// base branch local + remote.
		if err := exec.CommandContext(ctx, "git", "-C", cfg.Repo.LocalPath,
			"show-ref", "--verify", "--quiet", "refs/heads/"+cfg.Repo.BaseBranch).Run(); err != nil {
			errs = append(errs, fmt.Errorf("doctor: base branch %s missing locally", cfg.Repo.BaseBranch))
		}
	}

	// 11: workspace root writable.
	wsRoot := filepath.Join(cfg.Repo.LocalPath, ".symphony-go", "wt")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		errs = append(errs, fmt.Errorf("doctor: workspace root not writable: %w", err))
	}

	// Per-axis configuration checks (Proposal 0001 §5.7). These mostly
	// duplicate Validate's checks at config-load — the goal is to surface
	// problems with clearer wording in `symphony-go doctor` output and to
	// add file-existence checks Validate can't perform.
	errs = append(errs, doctorPerAxisChecks(cfg, absCfg)...)

	// 12: auto-mode preconditions.
	if cfg.Approval.Mode == "auto" {
		hasCatchAll := false
		needReviewer := false
		for _, r := range cfg.Auto.Rules {
			if len(r.IssueLabels) == 0 {
				hasCatchAll = true
			}
			if r.ReviewerRequired {
				needReviewer = true
			}
		}
		if !hasCatchAll && cfg.Auto.FallbackOnNoRuleMatch == "" {
			errs = append(errs, errors.New("doctor: auto.rules has no catch-all and fallback_on_no_rule_match is empty"))
		}
		if needReviewer && cfg.Auto.Reviewer.Provider != "" {
			if _, err := exec.LookPath(cfg.Auto.Reviewer.Provider); err != nil {
				errs = append(errs, fmt.Errorf("doctor: reviewer %q not in PATH: %w", cfg.Auto.Reviewer.Provider, err))
			}
		}
		if cfg.Auto.Reviewer.Provider != "" && cfg.Auto.Reviewer.Provider == cfg.Agent.Provider {
			slog.Warn("doctor: auto.reviewer.provider equals agent.provider; consider using a different provider for defense in depth")
		}
	}

	return errors.Join(errs...)
}

// doctorPerAxisChecks runs Proposal 0001 §5.7 checks against cfg. These
// are restated here (vs only at parse time) so `symphony-go doctor`
// surfaces them with clear, knob-named messages. absConfigPath is the
// absolute path of the config file (or "") used to derive the "config
// dir" — workflow files must not live under it.
func doctorPerAxisChecks(cfg *config.Config, absConfigPath string) []error {
	var errs []error

	// Each `_by_label` map: default key required when non-empty. Already
	// enforced by Validate, but reasserted here with a doctor prefix for
	// clarity.
	if !cfg.Repo.WorkflowFiles.IsEmpty() && !cfg.Repo.WorkflowFiles.HasDefault() {
		errs = append(errs, fmt.Errorf("doctor: repo.workflow_files missing \"default\" key"))
	}
	if !cfg.Validation.CommandsByLabel.IsEmpty() && !cfg.Validation.CommandsByLabel.HasDefault() {
		errs = append(errs, fmt.Errorf("doctor: validation.commands_by_label missing \"default\" key"))
	}
	if !cfg.Approval.ModeByLabel.IsEmpty() && !cfg.Approval.ModeByLabel.HasDefault() {
		errs = append(errs, fmt.Errorf("doctor: approval.mode_by_label missing \"default\" key"))
	}
	for name, m := range claudeByLabelMaps(cfg) {
		if !m.IsEmpty() && !m.HasDefault() {
			errs = append(errs, fmt.Errorf("doctor: claude.%s missing \"default\" key", name))
		}
	}
	for name, m := range codexByLabelMaps(cfg) {
		if !m.IsEmpty() && !m.HasDefault() {
			errs = append(errs, fmt.Errorf("doctor: codex.%s missing \"default\" key", name))
		}
	}

	// approval.mode_by_label values must be valid modes.
	if !cfg.Approval.ModeByLabel.IsEmpty() {
		for k, v := range cfg.Approval.ModeByLabel.Values {
			switch v {
			case "gated", "auto", "handoff":
			default:
				errs = append(errs, fmt.Errorf(
					"doctor: approval.mode_by_label[%q] = %q must be gated|auto|handoff", k, v))
			}
		}
	}

	// repo.workflow_files: every referenced file exists, lives under
	// repo.local_path, and is NOT under the config dir.
	if !cfg.Repo.WorkflowFiles.IsEmpty() {
		absRepo, _ := filepath.Abs(cfg.Repo.LocalPath)
		var configDir string
		if absConfigPath != "" {
			configDir = filepath.Dir(absConfigPath)
		}
		for _, k := range cfg.Repo.WorkflowFiles.Keys {
			rel := cfg.Repo.WorkflowFiles.Values[k]
			if rel == "" {
				errs = append(errs, fmt.Errorf("doctor: repo.workflow_files[%q] is empty", k))
				continue
			}
			full := rel
			if !filepath.IsAbs(full) {
				full = filepath.Join(cfg.Repo.LocalPath, rel)
			}
			absFull, _ := filepath.Abs(full)
			if _, err := os.Stat(absFull); err != nil {
				errs = append(errs, fmt.Errorf(
					"doctor: repo.workflow_files[%q] %q: %w", k, rel, err))
				continue
			}
			if absRepo != "" && !pathInside(absFull, absRepo) {
				errs = append(errs, fmt.Errorf(
					"doctor: repo.workflow_files[%q] %q is outside repo.local_path %q",
					k, rel, cfg.Repo.LocalPath))
			}
			if configDir != "" && pathInside(absFull, configDir) {
				errs = append(errs, fmt.Errorf(
					"doctor: repo.workflow_files[%q] %q is under config dir %q (forbidden)",
					k, rel, configDir))
			}
		}
	}

	return errs
}

// claudeByLabelMaps returns the claude per-axis maps keyed by their YAML
// suffix name for use in doctor diagnostics.
func claudeByLabelMaps(cfg *config.Config) map[string]config.OrderedMap[[]string] {
	return map[string]config.OrderedMap[[]string]{
		"planning_tools_by_label":       cfg.Claude.PlanningToolsByLabel,
		"implementation_tools_by_label": cfg.Claude.ImplementationToolsByLabel,
		"review_tools_by_label":         cfg.Claude.ReviewToolsByLabel,
		"disallowed_tools_by_label":     cfg.Claude.DisallowedToolsByLabel,
	}
}

// codexByLabelMaps returns the codex per-axis maps keyed by their YAML
// suffix name for use in doctor diagnostics.
func codexByLabelMaps(cfg *config.Config) map[string]config.OrderedMap[[]string] {
	return map[string]config.OrderedMap[[]string]{
		"planning_args_by_label":       cfg.Codex.PlanningArgsByLabel,
		"implementation_args_by_label": cfg.Codex.ImplementationArgsByLabel,
		"review_args_by_label":         cfg.Codex.ReviewArgsByLabel,
	}
}

// doctorResolveGitHubAuth mirrors cmd/symphony-go/main.go::buildGitHubAuth
// but returns just the Client and a human-readable summary string. Kept
// in this package so doctor can run without importing the cmd package.
//
// The summary is one of:
//
//	github auth: pat (token_env=GITHUB_TOKEN)
//	github auth: app (app_id=12345, installation_id=67890)
//
// On error the summary may still be non-empty (so operators can see which
// mode failed).
func doctorResolveGitHubAuth(ctx context.Context, cfg *config.Config) (gh.Client, string, error) {
	switch cfg.GitHub.Auth {
	case "", "pat":
		summary := fmt.Sprintf("github auth: pat (token_env=%s)", cfg.GitHub.TokenEnv)
		token := os.Getenv(cfg.GitHub.TokenEnv)
		if token == "" {
			return nil, summary, fmt.Errorf("env %s is empty", cfg.GitHub.TokenEnv)
		}
		cli, err := gh.NewClient(ctx, token, cfg.Repo.FullName)
		if err != nil {
			return nil, summary, err
		}
		return cli, summary, nil
	case "app":
		appID, err := strconv.ParseInt(os.Getenv(cfg.GitHub.AppIDEnv), 10, 64)
		if err != nil || appID <= 0 {
			return nil, "github auth: app (app_id env not parseable)",
				fmt.Errorf("github.app_id_env %q is not a positive int64 (raw=%q)", cfg.GitHub.AppIDEnv, os.Getenv(cfg.GitHub.AppIDEnv))
		}
		instID, err := strconv.ParseInt(os.Getenv(cfg.GitHub.InstallationIDEnv), 10, 64)
		if err != nil || instID <= 0 {
			return nil, fmt.Sprintf("github auth: app (app_id=%d, installation_id env not parseable)", appID),
				fmt.Errorf("github.installation_id_env %q is not a positive int64 (raw=%q)", cfg.GitHub.InstallationIDEnv, os.Getenv(cfg.GitHub.InstallationIDEnv))
		}
		summary := fmt.Sprintf("github auth: app (app_id=%d, installation_id=%d)", appID, instID)
		pemBytes, err := doctorLoadAppPEM(cfg.GitHub)
		if err != nil {
			return nil, summary, err
		}
		cli, _, err := gh.NewAppClient(ctx, gh.AppAuth{
			AppID:          appID,
			InstallationID: instID,
			PrivateKeyPEM:  pemBytes,
		}, cfg.Repo.FullName)
		if err != nil {
			return nil, summary, err
		}
		return cli, summary, nil
	default:
		return nil, "", fmt.Errorf("unknown github.auth %q", cfg.GitHub.Auth)
	}
}

// doctorLoadAppPEM resolves PEM bytes from one of the two env
// indirections; warns on broad file mode (>0600).
func doctorLoadAppPEM(c config.GitHubConfig) ([]byte, error) {
	if c.PrivateKeyPathEnv != "" {
		path := os.Getenv(c.PrivateKeyPathEnv)
		if path == "" {
			return nil, fmt.Errorf("github.private_key_path_env %q is empty", c.PrivateKeyPathEnv)
		}
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			slog.Warn("doctor: github app pem file is group/world-readable; recommend chmod 600",
				"path", path, "mode", fmt.Sprintf("%#o", info.Mode().Perm()))
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", path, err)
		}
		return body, nil
	}
	if c.PrivateKeyPEMEnv != "" {
		raw := os.Getenv(c.PrivateKeyPEMEnv)
		if raw == "" {
			return nil, fmt.Errorf("github.private_key_pem_env %q is empty", c.PrivateKeyPEMEnv)
		}
		return []byte(raw), nil
	}
	return nil, fmt.Errorf("neither private_key_path_env nor private_key_pem_env is configured")
}

// pathInside is local to avoid an import cycle with config; it mirrors
// config.pathInside.
func pathInside(child, parent string) bool {
	if child == parent {
		return true
	}
	sep := string(os.PathSeparator)
	p := parent
	if len(p) > 0 && p[len(p)-1:] != sep {
		p += sep
	}
	return len(child) >= len(p) && child[:len(p)] == p
}
