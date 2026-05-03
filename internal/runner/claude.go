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
	"sync"
	"syscall"
	"time"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/exec"
	"github.com/logosc/symphony-go/internal/types"
)

// stallWatchdog monitors event-inactivity on a streaming runner. When
// timeout > 0, callers must invoke touch() each time a stdout line or
// JSON-RPC frame is observed; the watchdog goroutine cancels the run
// context via cancel when time.Since(lastEventAt) exceeds timeout. Stop
// must be called before the parent context can be cancelled normally to
// release resources.
//
// The watchdog is safe to use across goroutines: touch() is mutex-guarded
// against the watchdog's read of lastEventAt.
type stallWatchdog struct {
	timeout time.Duration
	cancel  context.CancelFunc
	stop    context.CancelFunc

	mu          sync.Mutex
	lastEventAt time.Time
	fired       bool
}

// newStallWatchdog spawns a watchdog goroutine. When timeout <= 0 it
// returns a no-op watchdog (touch and Stop are cheap; cancel is never
// called). The returned context is the child context callers must use
// when constructing the subprocess; it is cancelled both when Stop is
// called (normal termination) and when a stall is detected.
func newStallWatchdog(parent context.Context, timeout time.Duration) (context.Context, *stallWatchdog) {
	runCtx, cancel := context.WithCancel(parent)
	w := &stallWatchdog{
		timeout:     timeout,
		cancel:      cancel,
		lastEventAt: time.Now(),
	}
	if timeout <= 0 {
		// No-op watchdog — Stop just releases the cancel func.
		w.stop = cancel
		return runCtx, w
	}
	wdCtx, stop := context.WithCancel(parent)
	w.stop = stop
	tickEvery := timeout / 4
	if tickEvery < time.Second {
		tickEvery = time.Second
	}
	go func() {
		ticker := time.NewTicker(tickEvery)
		defer ticker.Stop()
		for {
			select {
			case <-wdCtx.Done():
				return
			case <-ticker.C:
				w.mu.Lock()
				idle := time.Since(w.lastEventAt)
				w.mu.Unlock()
				if idle > timeout {
					w.mu.Lock()
					w.fired = true
					w.mu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	return runCtx, w
}

// touch records that an event/line was just observed.
func (w *stallWatchdog) touch() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.lastEventAt = time.Now()
	w.mu.Unlock()
}

// Stop tears down the watchdog goroutine and releases the run context.
// It is safe to call multiple times.
func (w *stallWatchdog) Stop() {
	if w == nil {
		return
	}
	if w.stop != nil {
		w.stop()
	}
	if w.cancel != nil {
		w.cancel()
	}
}

// Fired reports whether the watchdog cancelled the run due to inactivity.
func (w *stallWatchdog) Fired() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

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
// axisKey is the per-axis label frozen on the Job at claim time; when
// non-empty, any configured `*_by_label` tool/disallowed map is consulted
// before falling back to the scalar slice. Exposed package-private so
// tests can snapshot it directly without spawning a process.
func (cr *ClaudeRunner) buildArgs(phase types.Phase, axisKey string) []string {
	var permissionMode string
	var allowedTools []string
	var byLabel config.OrderedMap[[]string]
	switch phase {
	case types.PhasePlanning:
		permissionMode = "plan"
		allowedTools = cr.claudeCfg.PlanningTools
		byLabel = cr.claudeCfg.PlanningToolsByLabel
	case types.PhaseReview:
		permissionMode = "plan"
		allowedTools = cr.claudeCfg.ReviewTools
		byLabel = cr.claudeCfg.ReviewToolsByLabel
	case types.PhaseImplementation:
		permissionMode = "acceptEdits"
		allowedTools = cr.claudeCfg.ImplementationTools
		byLabel = cr.claudeCfg.ImplementationToolsByLabel
	default:
		// Fall back to the most-restrictive mode.
		permissionMode = "plan"
		allowedTools = cr.claudeCfg.PlanningTools
		byLabel = cr.claudeCfg.PlanningToolsByLabel
	}
	if v, ok := resolveByAxisStrings(byLabel, axisKey); ok {
		allowedTools = v
	}
	disallowed := cr.claudeCfg.DisallowedTools
	if v, ok := resolveByAxisStrings(cr.claudeCfg.DisallowedToolsByLabel, axisKey); ok {
		disallowed = v
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
	if len(disallowed) > 0 {
		args = append(args, "--disallowedTools", strings.Join(disallowed, ","))
	}
	return args
}

// resolveByAxisStrings looks up an axis key in a per-axis OrderedMap of
// string slices and returns the slice plus ok=true. When the map is empty
// or axisKey is empty, returns ok=false so the caller falls back to the
// scalar slice. Falls back to the "default" entry when axisKey doesn't
// match a concrete key but a default is present.
func resolveByAxisStrings(m config.OrderedMap[[]string], axisKey string) ([]string, bool) {
	if m.IsEmpty() {
		return nil, false
	}
	if axisKey != "" {
		if v, ok := m.Values[axisKey]; ok {
			return v, true
		}
	}
	if v, ok := m.Values["default"]; ok {
		return v, true
	}
	return nil, false
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

	stallTimeout := time.Duration(cr.agentCfg.StallTimeoutSeconds) * time.Second
	runCtx, watchdog := newStallWatchdog(runCtx, stallTimeout)
	defer watchdog.Stop()

	args := cr.buildArgs(req.Phase, req.AxisKey)

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
		watchdog.touch()
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
	// Capture any scanner read error to append after Wait. Writing to
	// stderrBuf here would race with exec's internal stderr-copy goroutine
	// (the race detector flagged this on CI under -race).
	var scanErrSuffix string
	if scanErr := scanner.Err(); scanErr != nil && !errors.Is(scanErr, io.EOF) {
		scanErrSuffix = fmt.Sprintf("\n[claude runner] stdout scan error: %v\n", scanErr)
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
	result.Stderr = exec.Redact(stderrBuf.String()+scanErrSuffix, cr.auditCfg.RedactPatterns)
	result.Success = exitOK && !sawErrorEvent

	// Non-zero exit on its own is not a runner-level error: it's a failed
	// agent run. Surface only context-derived termination explicitly so
	// callers can distinguish timeouts.
	if watchdog.Fired() {
		result.Success = false
		return result, fmt.Errorf("claude runner: stalled (no events for %ds)", cr.agentCfg.StallTimeoutSeconds)
	}
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
