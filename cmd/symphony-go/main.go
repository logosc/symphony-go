// Command symphony-go is a local-first orchestrator that drives Codex or
// Claude Code on GitHub issues. See SPEC.md for the design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/logosc/symphony-go/internal/approval"
	"github.com/logosc/symphony-go/internal/audit"
	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/github"
	"github.com/logosc/symphony-go/internal/orchestrator"
	"github.com/logosc/symphony-go/internal/runner"
	"github.com/logosc/symphony-go/internal/state"
	"github.com/logosc/symphony-go/internal/types"
	"github.com/logosc/symphony-go/internal/workspace"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run":
		os.Exit(runCommand(args))
	case "doctor":
		os.Exit(doctorCommand(args))
	case "status":
		os.Exit(statusCommand(args))
	case "clean":
		os.Exit(cleanCommand(args))
	case "-h", "--help", "help":
		usage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, `symphony-go — drives Codex/Claude on GitHub issues

usage:
  symphony-go run    [--once] --config <path>
  symphony-go doctor          --config <path>
  symphony-go status          --config <path>
  symphony-go clean           [--config <path>] [--dry-run] [--force]

If --config is omitted, the following are searched in order:
  $SYMPHONY_GO_CONFIG
  $XDG_CONFIG_HOME/symphony-go/config.yml
  ~/.symphony-go/config.yml`)
}

func runCommand(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.yml")
	once := fs.Bool("once", false, "run a single dispatch cycle and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolved, err := resolveConfigPath(*configPath)
	if err != nil {
		slog.Error("config not found", "err", err)
		return 2
	}

	cfg, err := config.Load(resolved)
	if err != nil {
		slog.Error("config load", "path", resolved, "err", err)
		return 2
	}

	// Integrity guard: enforce config-not-under-repo and seed the SHA-256
	// baseline. The orchestrator may re-check on each tick.
	if _, err := config.NewIntegrityGuard(resolved, cfg.Repo.LocalPath); err != nil {
		slog.Error("config integrity guard", "err", err)
		return 2
	}

	// Install the per-issue audit log writer (SPEC §13). The audit handler
	// fans out to the existing stderr JSON handler and additionally appends
	// a redacted JSON line to <repo>/.symphony-go/audit/{issue}.jsonl
	// whenever a record carries an "issue" or "issue_number" int attr.
	auditDir := filepath.Join(cfg.Repo.LocalPath, ".symphony-go", "audit")
	stderrDelegate := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	auditHandler := audit.New(auditDir, cfg.Audit.RedactPatterns, stderrDelegate)
	defer func() { _ = auditHandler.Close() }()
	slog.SetDefault(slog.New(auditHandler))

	var (
		wf  string
		wfs map[string]string
	)
	if !cfg.Repo.WorkflowFiles.IsEmpty() {
		wfs = make(map[string]string, len(cfg.Repo.WorkflowFiles.Keys))
		for _, key := range cfg.Repo.WorkflowFiles.Keys {
			rel := cfg.Repo.WorkflowFiles.Values[key]
			p := filepath.Join(cfg.Repo.LocalPath, rel)
			body, err := config.LoadWorkflow(p)
			if err != nil {
				slog.Error("workflow load", "key", key, "path", p, "err", err)
				return 2
			}
			wfs[key] = body
		}
	} else {
		wfPath := filepath.Join(cfg.Repo.LocalPath, cfg.Repo.WorkflowFile)
		body, err := config.LoadWorkflow(wfPath)
		if err != nil {
			slog.Error("workflow load", "path", wfPath, "err", err)
			return 2
		}
		wf = body
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	storeRoot := filepath.Join(cfg.Repo.LocalPath, ".symphony-go", "state")
	store, err := state.NewStore(storeRoot)
	if err != nil {
		slog.Error("state init", "root", storeRoot, "err", err)
		return 2
	}
	release, err := store.AcquireLock()
	if err != nil {
		slog.Error("lock contention; another symphony-go appears to be running", "err", err)
		return 2
	}
	defer func() { _ = release() }()

	gh, tokenFn, staticToken, err := buildGitHubAuth(ctx, cfg)
	if err != nil {
		slog.Error("github auth", "err", err)
		return 2
	}

	wsMgr := workspace.NewManager(cfg.Repo.LocalPath)

	agentRunner, err := buildRunner(cfg.Agent.Provider, cfg.Agent, cfg)
	if err != nil {
		slog.Error("agent runner", "err", err)
		return 2
	}

	// Per-axis runners (Proposal 0001 §11 question #1). When either
	// agent.provider_by_label or agent.model_by_label is set, pre-build
	// one runner per axis key (the union of both maps' keys, including
	// "default") and pass them to the orchestrator. The default
	// AgentRunner above remains as the fallback.
	agentRunnersByAxis, err := buildAgentRunnersByAxis(cfg)
	if err != nil {
		slog.Error("agent runner per-axis", "err", err)
		return 2
	}
	// When per-axis is configured, prefer the "default" entry as the
	// fallback runner so a job with an empty/unknown AxisKey behaves
	// consistently with the per-axis intent.
	if r, ok := agentRunnersByAxis["default"]; ok {
		agentRunner = r
	}

	var reviewer *approval.Reviewer
	if string(cfg.Approval.Mode) == string(types.ApprovalAuto) && anyRuleNeedsReviewer(cfg.Auto.Rules) {
		revAgentCfg := config.AgentConfig{
			Provider:       cfg.Auto.Reviewer.Provider,
			Model:          cfg.Auto.Reviewer.Model,
			TimeoutSeconds: cfg.Auto.Reviewer.TimeoutSeconds,
		}
		revRunner, err := buildRunner(cfg.Auto.Reviewer.Provider, revAgentCfg, cfg)
		if err != nil {
			slog.Error("reviewer runner", "err", err)
			return 2
		}
		reviewer = approval.NewReviewer(revRunner, cfg.Auto.Reviewer)
	}

	orch, err := orchestrator.New(orchestrator.Deps{
		Config:             cfg,
		GitHub:             gh,
		State:              store,
		WorkspaceMgr:       wsMgr,
		AgentRunner:        agentRunner,
		AgentRunnersByAxis: agentRunnersByAxis,
		Reviewer:           reviewer,
		PromptTemplate:     wf,
		PromptTemplates:    wfs,
		GitHubToken:        staticToken,
		GitHubTokenFn:      tokenFn,
	})
	if err != nil {
		slog.Error("orchestrator new", "err", err)
		return 2
	}

	slog.Info("symphony-go starting", "config", resolved, "once", *once, "repo", cfg.Repo.FullName)
	if err := orch.Run(ctx, *once); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("shutdown")
			return 0
		}
		slog.Error("run", "err", err)
		return 1
	}
	return 0
}

func doctorCommand(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.yml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolved, err := resolveConfigPath(*configPath)
	if err != nil {
		slog.Error("config not found", "err", err)
		return 2
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		return 1
	}
	if _, err := config.NewIntegrityGuard(resolved, cfg.Repo.LocalPath); err != nil {
		fmt.Fprintf(os.Stderr, "config integrity: %v\n", err)
		return 1
	}
	if err := orchestrator.Doctor(context.Background(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "ok")
	return 0
}

// buildAgentRunnersByAxis pre-builds one runner per axis key when
// agent.provider_by_label or agent.model_by_label is set. Returns nil
// (no error) when neither is set. The union of both maps' keys is
// walked; for each key, a synthetic AgentConfig is constructed by
// substituting Provider/Model/ReasoningEffort from the maps (falling back
// to the scalar values when a key is missing from one of the maps).
func buildAgentRunnersByAxis(cfg *config.Config) (map[string]runner.AgentRunner, error) {
	pmap := cfg.Agent.ProviderByLabel
	mmap := cfg.Agent.ModelByLabel
	emap := cfg.Agent.ReasoningEffortByLabel
	if pmap.IsEmpty() && mmap.IsEmpty() && emap.IsEmpty() {
		return nil, nil
	}
	// Union of keys, preserving declaration order from provider map first.
	seen := make(map[string]struct{})
	var keys []string
	for _, k := range pmap.Keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	for _, k := range mmap.Keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	for _, k := range emap.Keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	out := make(map[string]runner.AgentRunner, len(keys))
	for _, k := range keys {
		provider := cfg.Agent.Provider
		if v, ok := pmap.Values[k]; ok {
			provider = v
		} else if v, ok := pmap.Values["default"]; ok {
			provider = v
		}
		model := cfg.Agent.Model
		if v, ok := mmap.Values[k]; ok {
			model = v
		} else if v, ok := mmap.Values["default"]; ok {
			model = v
		}
		reasoningEffort := cfg.Agent.ReasoningEffort
		if v, ok := emap.Values[k]; ok {
			reasoningEffort = v
		} else if v, ok := emap.Values["default"]; ok {
			reasoningEffort = v
		}
		synth := cfg.Agent
		synth.Provider = provider
		synth.Model = model
		synth.ReasoningEffort = reasoningEffort
		// Wipe the per-axis maps in the synthetic copy; they're not used
		// by the runner constructor and would be misleading.
		synth.ProviderByLabel = config.OrderedMap[string]{}
		synth.ModelByLabel = config.OrderedMap[string]{}
		synth.ReasoningEffortByLabel = config.OrderedMap[string]{}
		r, err := buildRunner(provider, synth, cfg)
		if err != nil {
			return nil, fmt.Errorf("axis %q: %w", k, err)
		}
		out[k] = r
	}
	return out, nil
}

func buildRunner(provider string, agentCfg config.AgentConfig, cfg *config.Config) (runner.AgentRunner, error) {
	switch provider {
	case "claude":
		return runner.NewClaudeRunner(agentCfg, cfg.Claude, cfg.Env, cfg.Audit), nil
	case "codex":
		return runner.NewCodexRunner(agentCfg, cfg.Codex, cfg.Env, cfg.Audit), nil
	default:
		return nil, fmt.Errorf("unknown agent provider %q (want claude|codex)", provider)
	}
}

func anyRuleNeedsReviewer(rules []config.AutoRule) bool {
	for _, r := range rules {
		if r.ReviewerRequired {
			return true
		}
	}
	return false
}

// buildGitHubAuth resolves credentials according to cfg.GitHub.Auth and
// returns:
//   - a github.Client (PAT- or App-installation-backed; the orchestrator
//     does not care which)
//   - tokenFn: a non-nil func that mints a fresh token for `git push`,
//     used in App mode where the installation token rotates hourly. Nil
//     in PAT mode (the static token is sufficient).
//   - staticToken: the PAT value in PAT mode; empty in App mode.
//
// On error the error message names the offending env var or config field.
func buildGitHubAuth(ctx context.Context, cfg *config.Config) (github.Client, func(context.Context) (string, error), string, error) {
	switch cfg.GitHub.Auth {
	case "", "pat":
		token := os.Getenv(cfg.GitHub.TokenEnv)
		if token == "" {
			return nil, nil, "", fmt.Errorf("github token env var %q is empty (cfg.github.token_env)", cfg.GitHub.TokenEnv)
		}
		cli, err := github.NewClient(ctx, token, cfg.Repo.FullName)
		if err != nil {
			return nil, nil, "", err
		}
		return cli, nil, token, nil
	case "app":
		appID, err := readPositiveInt64Env(cfg.GitHub.AppIDEnv, "github.app_id_env")
		if err != nil {
			return nil, nil, "", err
		}
		instID, err := readPositiveInt64Env(cfg.GitHub.InstallationIDEnv, "github.installation_id_env")
		if err != nil {
			return nil, nil, "", err
		}
		pemBytes, err := loadAppPEM(cfg.GitHub)
		if err != nil {
			return nil, nil, "", err
		}
		cli, creds, err := github.NewAppClient(ctx, github.AppAuth{
			AppID:          appID,
			InstallationID: instID,
			PrivateKeyPEM:  pemBytes,
		}, cfg.Repo.FullName)
		if err != nil {
			return nil, nil, "", err
		}
		return cli, creds.Token, "", nil
	default:
		return nil, nil, "", fmt.Errorf("unknown github.auth %q (want \"pat\" or \"app\")", cfg.GitHub.Auth)
	}
}

// readPositiveInt64Env reads a positive int64 from the named environment
// variable. Empty, non-numeric, or non-positive values are reported with
// the field name to make the operator-facing error specific.
func readPositiveInt64Env(envName, fieldName string) (int64, error) {
	raw := os.Getenv(envName)
	if raw == "" {
		return 0, fmt.Errorf("%s env var %q is empty", fieldName, envName)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s env var %q must parse as int64: %w", fieldName, envName, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s env var %q must be a positive int64, got %d", fieldName, envName, v)
	}
	return v, nil
}

// loadAppPEM resolves the App's PEM bytes from one of the two env
// indirections: a path-on-disk (PrivateKeyPathEnv) or the inline PEM
// string (PrivateKeyPEMEnv). Validate has already enforced that exactly
// one is configured. Also warns when a PEM file's mode is broader than
// 0600 — narrow permissions are best-practice for App keys.
func loadAppPEM(c config.GitHubConfig) ([]byte, error) {
	if c.PrivateKeyPathEnv != "" {
		pemPath := os.Getenv(c.PrivateKeyPathEnv)
		if pemPath == "" {
			return nil, fmt.Errorf("github app private-key-path env var %q is empty (cfg.github.private_key_path_env)", c.PrivateKeyPathEnv)
		}
		info, statErr := os.Stat(pemPath)
		if statErr == nil && info.Mode().Perm()&0o077 != 0 {
			slog.Warn("github app pem file is group/world-readable; recommend chmod 600",
				"path", pemPath, "mode", fmt.Sprintf("%#o", info.Mode().Perm()))
		}
		body, err := os.ReadFile(pemPath)
		if err != nil {
			return nil, fmt.Errorf("read app private key at %q: %w", pemPath, err)
		}
		return body, nil
	}
	if c.PrivateKeyPEMEnv != "" {
		raw := os.Getenv(c.PrivateKeyPEMEnv)
		if raw == "" {
			return nil, fmt.Errorf("github app private-key-pem env var %q is empty (cfg.github.private_key_pem_env)", c.PrivateKeyPEMEnv)
		}
		return []byte(raw), nil
	}
	return nil, fmt.Errorf("github app private key: neither private_key_path_env nor private_key_pem_env is configured")
}

func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if v := os.Getenv("SYMPHONY_GO_CONFIG"); v != "" {
		return v, nil
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v + "/symphony-go/config.yml", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no --config and no fallback: %w", err)
	}
	return home + "/.symphony-go/config.yml", nil
}
