// This file implements the Codex AgentRunner. See SPEC §9.
//
// MVP supports `codex.mode == "exec"` only. The argv is:
//
//	codex exec --json [phase-args]
//
// where phase-args come from CodexConfig.PlanningArgs / ReviewArgs /
// ImplementationArgs. The prompt is fed to the subprocess on stdin only;
// it is never passed as a shell argument. The process inherits a sanitized
// env built by exec.BuildAgentEnv (allowlist + block patterns + always-drop
// list including GITHUB_TOKEN).
//
// `codex exec --json` emits newline-delimited JSON events on stdout. The
// runner accumulates raw bytes into RunResult.Events, parses each line, and
// considers the run successful iff the process exits 0 AND the last
// terminal event is "turn.completed" (not "turn.failed" or "error").
// RunResult.Text is the concatenation of `item.completed` events whose
// item_type is "agent_message" — falling back to any `text` fields seen, or
// to the raw stdout text if no JSON could be parsed at all.
//
// Stderr is buffered and redacted with auditCfg.RedactPatterns before
// being placed on RunResult.Stderr.

package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/logosc/symphony-go/internal/config"
	xexec "github.com/logosc/symphony-go/internal/exec"
	"github.com/logosc/symphony-go/internal/types"
)

// CodexRunner runs the OpenAI Codex CLI as a one-shot subprocess
// (`codex exec --json`) for a single phase of work. It implements
// runner.AgentRunner.
type CodexRunner struct {
	agentCfg config.AgentConfig
	codexCfg config.CodexConfig
	envCfg   config.EnvConfig
	auditCfg config.AuditConfig

	// command is the executable invoked. Defaults to "codex". Tests use
	// WithCommand to point at a fake script. Kept unexported so the public
	// API surface stays small; we accept the small ergonomic cost in tests
	// in exchange for no additional public knob.
	command string
}

// NewCodexRunner constructs a CodexRunner from the relevant config slices.
// The returned runner uses "codex" as its executable; tests can override
// via WithCommand.
func NewCodexRunner(agentCfg config.AgentConfig, codexCfg config.CodexConfig, envCfg config.EnvConfig, auditCfg config.AuditConfig) *CodexRunner {
	return &CodexRunner{
		agentCfg: agentCfg,
		codexCfg: codexCfg,
		envCfg:   envCfg,
		auditCfg: auditCfg,
		command:  "codex",
	}
}

// WithCommand overrides the executable name/path used to launch codex.
// Intended for tests; production code should leave the default "codex".
// Returns the receiver to allow fluent configuration.
func (cr *CodexRunner) WithCommand(cmd string) *CodexRunner {
	cr.command = cmd
	return cr
}

// phaseArgs returns the configured per-phase argv tail. axisKey is the
// per-axis label frozen on the Job at claim time; when non-empty, any
// configured `*_args_by_label` map is consulted first and falls back to
// the scalar slice when the map is empty or carries no matching entry.
func (cr *CodexRunner) phaseArgs(phase types.Phase, axisKey string) ([]string, error) {
	var scalar []string
	var byLabel config.OrderedMap[[]string]
	switch phase {
	case types.PhasePlanning:
		scalar = cr.codexCfg.PlanningArgs
		byLabel = cr.codexCfg.PlanningArgsByLabel
	case types.PhaseReview:
		scalar = cr.codexCfg.ReviewArgs
		byLabel = cr.codexCfg.ReviewArgsByLabel
	case types.PhaseImplementation:
		scalar = cr.codexCfg.ImplementationArgs
		byLabel = cr.codexCfg.ImplementationArgsByLabel
	default:
		return nil, fmt.Errorf("codex runner: unknown phase %q", phase)
	}
	if v, ok := resolveByAxisStrings(byLabel, axisKey); ok {
		return append([]string{}, v...), nil
	}
	return append([]string{}, scalar...), nil
}

// buildArgv constructs the full argv (excluding the executable itself)
// for the given phase: ["exec", "--json", ...phase-args...]. axisKey is
// forwarded to phaseArgs for per-axis selection.
func (cr *CodexRunner) buildArgv(phase types.Phase, axisKey string) ([]string, error) {
	pa, err := cr.phaseArgs(phase, axisKey)
	if err != nil {
		return nil, err
	}
	argv := []string{"exec", "--json"}
	argv = append(argv, pa...)
	return argv, nil
}

// codexEvent is a permissive subset of the JSON shape emitted by
// `codex exec --json`. Only fields the runner inspects are decoded.
type codexEvent struct {
	Type     string `json:"type"`
	ItemType string `json:"item_type"`
	Text     string `json:"text"`
}

// Run implements runner.AgentRunner for the Codex CLI in `exec` mode.
// See file-level docs for protocol details.
func (cr *CodexRunner) Run(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
	started := time.Now()

	if cr.codexCfg.Mode == "app-server" {
		return cr.runAppServer(ctx, req)
	}
	if cr.codexCfg.Mode != "exec" {
		return types.RunResult{StartedAt: started, CompletedAt: time.Now()},
			fmt.Errorf("codex runner: unsupported mode %q (want exec|app-server)", cr.codexCfg.Mode)
	}

	argv, err := cr.buildArgv(req.Phase, req.AxisKey)
	if err != nil {
		return types.RunResult{StartedAt: started, CompletedAt: time.Now()}, err
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	stallTimeout := time.Duration(cr.agentCfg.StallTimeoutSeconds) * time.Second
	runCtx, watchdog := newStallWatchdog(runCtx, stallTimeout)
	defer watchdog.Stop()

	cmd := exec.CommandContext(runCtx, cr.command, argv...)
	cmd.Dir = req.RepoPath
	cmd.Env = xexec.BuildAgentEnv(cr.envCfg.Allowlist, cr.envCfg.BlockPatterns, os.Environ(), req.HomePath)
	cmd.Stdin = strings.NewReader(req.Prompt)

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return types.RunResult{StartedAt: started, CompletedAt: time.Now()},
			fmt.Errorf("codex runner: stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return types.RunResult{StartedAt: started, CompletedAt: time.Now()},
			fmt.Errorf("codex runner: start: %w", err)
	}

	// Drain stdout in this goroutine. We accumulate raw bytes for Events
	// and parse JSONL line by line to extract Text and the terminal event.
	var (
		eventsBuf     bytes.Buffer
		textParts     []string
		fallbackTexts []string
		anyJSONParsed bool
		lastTerminal  string
		anyAgentMsg   bool
	)

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		watchdog.touch()
		line := scanner.Bytes()
		eventsBuf.Write(line)
		eventsBuf.WriteByte('\n')

		trim := bytes.TrimSpace(line)
		if len(trim) == 0 {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(trim, &ev); err != nil {
			slog.Warn("codex runner: malformed JSONL line ignored", "err", err.Error())
			continue
		}
		anyJSONParsed = true

		switch ev.Type {
		case "turn.completed", "turn.failed", "error":
			lastTerminal = ev.Type
		case "item.completed":
			if ev.ItemType == "agent_message" && ev.Text != "" {
				textParts = append(textParts, ev.Text)
				anyAgentMsg = true
			} else if ev.Text != "" {
				fallbackTexts = append(fallbackTexts, ev.Text)
			}
		default:
			if ev.Text != "" {
				fallbackTexts = append(fallbackTexts, ev.Text)
			}
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		slog.Warn("codex runner: stdout scan error", "err", err.Error())
	}

	waitErr := cmd.Wait()
	completed := time.Now()

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if asExit(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	stderrStr := xexec.Redact(stderrBuf.String(), cr.auditCfg.RedactPatterns)
	eventsRedacted := xexec.Redact(eventsBuf.String(), cr.auditCfg.RedactPatterns)

	var text string
	switch {
	case anyAgentMsg:
		text = strings.Join(textParts, "")
	case len(fallbackTexts) > 0:
		text = strings.Join(fallbackTexts, "")
	case !anyJSONParsed:
		// No JSON parsed at all — fall back to raw stdout.
		text = eventsBuf.String()
	}

	success := exitCode == 0 && lastTerminal == "turn.completed"

	res := types.RunResult{
		Success:     success,
		Text:        text,
		Stderr:      stderrStr,
		Events:      []byte(eventsRedacted),
		StartedAt:   started,
		CompletedAt: completed,
	}

	// If the watchdog fired, surface as a stall error and force Success=false.
	if watchdog.Fired() {
		res.Success = false
		return res, fmt.Errorf("codex runner: stalled (no events for %ds)", cr.agentCfg.StallTimeoutSeconds)
	}
	// If the context was canceled or timed out, surface as a Go-level error.
	if runCtx.Err() != nil {
		return res, runCtx.Err()
	}
	if waitErr != nil {
		// Non-zero exit is reflected in res.Success; no Go-level error.
		var exitErr *exec.ExitError
		if !asExit(waitErr, &exitErr) {
			return res, fmt.Errorf("codex runner: wait: %w", waitErr)
		}
	}
	return res, nil
}

// asExit is a thin wrapper around errors.As for *exec.ExitError so the
// hot path stays readable. Returns true when err unwraps to *exec.ExitError.
func asExit(err error, target **exec.ExitError) bool {
	for e := err; e != nil; {
		if ee, ok := e.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
