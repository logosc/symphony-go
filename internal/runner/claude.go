package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/chenlong-seu/symphony-go/internal/config"
	"github.com/chenlong-seu/symphony-go/internal/exec"
	"github.com/chenlong-seu/symphony-go/internal/types"
)

// defaultClaudeCommand is the default executable name for the Claude Code
// CLI. Resolved via PATH at run time.
const defaultClaudeCommand = "claude"

// ClaudeRunner is an AgentRunner that drives the Claude Code CLI in
// headless stream-json mode. See SPEC.md §9 for the per-phase argv shape
// and security invariants (sanitized env, prompt-on-stdin, SIGTERM-on-cancel).
type ClaudeRunner struct {
	agentCfg  config.AgentConfig
	claudeCfg config.ClaudeConfig
	envCfg    config.EnvConfig
	auditCfg  config.AuditConfig

	// command is the executable name or path of the Claude Code CLI. It
	// defaults to "claude" and is resolved via PATH at run time. Tests
	// override it via WithCommand to swap in a fake binary.
	command string
}

// ClaudeOption mutates a ClaudeRunner during construction. Used to keep
// the public constructor stable while still allowing tests (and future
// production knobs) to override defaults like the executable path.
type ClaudeOption func(*ClaudeRunner)

// WithCommand overrides the executable name or path used to launch the
// Claude Code CLI. Pass an absolute path to a wrapper script in tests.
// Empty values are ignored.
func WithCommand(command string) ClaudeOption {
	return func(cr *ClaudeRunner) {
		if command != "" {
			cr.command = command
		}
	}
}

// NewClaudeRunner constructs a ClaudeRunner from the resolved config
// blocks. Apply ClaudeOption values (e.g., WithCommand) to override
// defaults. The constructor performs no I/O.
func NewClaudeRunner(agentCfg config.AgentConfig, claudeCfg config.ClaudeConfig, envCfg config.EnvConfig, auditCfg config.AuditConfig, opts ...ClaudeOption) *ClaudeRunner {
	cr := &ClaudeRunner{
		agentCfg:  agentCfg,
		claudeCfg: claudeCfg,
		envCfg:    envCfg,
		auditCfg:  auditCfg,
		command:   defaultClaudeCommand,
	}
	for _, opt := range opts {
		opt(cr)
	}
	return cr
}

// buildArgs constructs the argv (after the command itself) for one phase.
// Exposed package-private so tests can snapshot it directly without
// spawning a process.
func (cr *ClaudeRunner) buildArgs(phase types.Phase) []string {
	var permissionMode string
	var allowedTools []string
	switch phase {
	case types.PhasePlanning:
		permissionMode = "plan"
		allowedTools = cr.claudeCfg.PlanningTools
	case types.PhaseReview:
		permissionMode = "plan"
		allowedTools = cr.claudeCfg.ReviewTools
	case types.PhaseImplementation:
		permissionMode = "acceptEdits"
		allowedTools = cr.claudeCfg.ImplementationTools
	default:
		// Fall back to the most-restrictive mode.
		permissionMode = "plan"
		allowedTools = cr.claudeCfg.PlanningTools
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--model", cr.agentCfg.Model,
		"--max-turns", fmt.Sprintf("%d", cr.claudeCfg.MaxTurns),
		"--permission-mode", permissionMode,
	}
	if len(allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowedTools, ","))
	}
	if len(cr.claudeCfg.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(cr.claudeCfg.DisallowedTools, ","))
	}
	return args
}

// Run executes one phase of work via the Claude Code CLI. It implements
// runner.AgentRunner. The prompt is delivered on stdin only. If
// req.Timeout > 0 a derived context bounds the subprocess; cancellation
// sends SIGTERM with a 10s grace period. Stdout is parsed line-by-line
// as stream-json; the final "type":"result" event's "result" field
// becomes RunResult.Text. Stderr is buffered and redacted before return.
func (cr *ClaudeRunner) Run(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	args := cr.buildArgs(req.Phase)

	startedAt := time.Now()
	result := types.RunResult{StartedAt: startedAt}

	cmd := osexec.CommandContext(runCtx, cr.command, args...)
	cmd.Dir = req.RepoPath
	cmd.Env = exec.BuildAgentEnv(cr.envCfg.Allowlist, cr.envCfg.BlockPatterns, os.Environ(), req.HomePath)
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
		result.CompletedAt = time.Now()
		return result, fmt.Errorf("claude: stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		result.CompletedAt = time.Now()
		return result, fmt.Errorf("claude: start: %w", err)
	}

	var (
		eventsBuf      bytes.Buffer
		fallbackText   bytes.Buffer
		lastResultText string
		gotResult      bool
		sawErrorEvent  bool
	)

	scanner := bufio.NewScanner(stdoutPipe)
	// Allow long stream-json lines (e.g., large tool outputs).
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		eventsBuf.Write(line)
		eventsBuf.WriteByte('\n')
		fallbackText.Write(line)
		fallbackText.WriteByte('\n')

		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "result":
			gotResult = true
			if s, ok := ev["result"].(string); ok {
				lastResultText = s
			}
		case "error":
			sawErrorEvent = true
		}
	}
	// Drain any scanner read error into stderr-side reasoning; do not fail.
	if scanErr := scanner.Err(); scanErr != nil && !errors.Is(scanErr, io.EOF) {
		fmt.Fprintf(&stderrBuf, "\n[claude runner] stdout scan error: %v\n", scanErr)
	}

	waitErr := cmd.Wait()
	result.CompletedAt = time.Now()

	exitOK := waitErr == nil
	if waitErr != nil {
		var exitErr *osexec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitOK = false
		} else {
			exitOK = false
		}
	}

	if gotResult {
		result.Text = lastResultText
	} else {
		result.Text = fallbackText.String()
	}
	result.Events = append([]byte(nil), eventsBuf.Bytes()...)
	result.Stderr = exec.Redact(stderrBuf.String(), cr.auditCfg.RedactPatterns)
	result.Success = exitOK && !sawErrorEvent

	// Non-zero exit on its own is not a runner-level error: it's a failed
	// agent run. Surface only context-derived termination explicitly so
	// callers can distinguish timeouts.
	if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.Success = false
		return result, nil
	}
	if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.Canceled) {
		result.Success = false
		return result, nil
	}
	return result, nil
}
