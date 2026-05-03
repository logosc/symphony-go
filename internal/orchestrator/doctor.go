package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

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

	// 4: GITHUB_TOKEN env var set.
	token := os.Getenv(cfg.GitHub.TokenEnv)
	if token == "" {
		errs = append(errs, fmt.Errorf("doctor: env %s is empty", cfg.GitHub.TokenEnv))
	}

	// 5, 6, 7: live GitHub probes (only when we have a token).
	if token != "" {
		client, err := gh.NewClient(ctx, token, cfg.Repo.FullName)
		if err != nil {
			errs = append(errs, fmt.Errorf("doctor: github client: %w", err))
		} else {
			// Cheap probe: list ready issues. Confirms repo + label existence.
			if _, err := client.ListReadyIssues(ctx, cfg.Labels.Ready); err != nil {
				errs = append(errs, fmt.Errorf("doctor: list ready issues: %w", err))
			}
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
