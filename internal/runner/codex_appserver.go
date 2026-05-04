// This file implements the Codex AgentRunner for `codex app-server` mode.
// See SPEC.md §9 for the protocol invariants.
//
// `codex app-server` speaks JSON-RPC 2.0 over stdio with newline-delimited
// framing (no Content-Length header). Lifecycle:
//
//	initialize       (request)       -> server response
//	initialized      (notification)
//	thread/start     (request)       -> result.thread.id
//	turn/start       (request)       -> result.turn.id; events stream as
//	                                    notifications until a terminal
//	                                    `turn/completed` notification with
//	                                    turn.status ∈ {completed, interrupted, failed}.
//
// Per-thread sandbox is set at thread/start (read-only | workspace-write |
// danger-full-access). The runner derives the sandbox from the phase by
// parsing `--sandbox <value>` out of CodexConfig.PlanningArgs /
// ImplementationArgs / ReviewArgs (the same slices used in exec mode).
//
// Item events arrive as notifications; the agent's user-visible text comes
// from `item.completed` events with `item_type == "agent_message"`. This
// matches the parser in codex.go (exec mode).
//
// CodexRunner.Run dispatches to runAppServer when codex.mode == "app-server".
// runAppServer drives a single turn — multi-turn orchestration uses the
// session API exposed by Session/codexSession below.

package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/logosc/symphony-go/internal/config"
	xexec "github.com/logosc/symphony-go/internal/exec"
	"github.com/logosc/symphony-go/internal/types"
)

// appServerMaxLine bounds a single newline-delimited JSON-RPC frame at
// 16 MiB, matching the buffer cap used in claude.go for stream-json lines.
const appServerMaxLine = 16 * 1024 * 1024

// jsonrpcMessage is a permissive JSON-RPC 2.0 message. Either Method (for
// requests/notifications) or one of Result/Error (for responses) is set.
type jsonrpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *jsonrpcError    `json:"error,omitempty"`
}

// jsonrpcError is the standard JSON-RPC 2.0 error object.
type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// runAppServer dispatches a single-turn run via the JSON-RPC app-server
// backend. It opens a session, runs one turn, and closes the session.
func (cr *CodexRunner) runAppServer(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
	started := time.Now()

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	stallTimeout := time.Duration(cr.agentCfg.StallTimeoutSeconds) * time.Second
	runCtx, watchdog := newStallWatchdog(runCtx, stallTimeout)
	defer watchdog.Stop()

	sess, err := cr.openSessionWithWatchdog(runCtx, req, watchdog)
	if err != nil {
		if watchdog.Fired() {
			return types.RunResult{StartedAt: started, CompletedAt: time.Now()},
				fmt.Errorf("codex app-server: stalled (no events for %ds)", cr.agentCfg.StallTimeoutSeconds)
		}
		return types.RunResult{StartedAt: started, CompletedAt: time.Now()}, err
	}
	defer func() { _ = sess.Close() }()

	res, err := sess.Turn(runCtx, req.Prompt)
	if watchdog.Fired() {
		res.Success = false
		if res.CompletedAt.IsZero() {
			res.CompletedAt = time.Now()
		}
		if res.StartedAt.IsZero() {
			res.StartedAt = started
		}
		return res, fmt.Errorf("codex app-server: stalled (no events for %ds)", cr.agentCfg.StallTimeoutSeconds)
	}
	if res.StartedAt.IsZero() {
		res.StartedAt = started
	}
	if res.CompletedAt.IsZero() {
		res.CompletedAt = time.Now()
	}
	return res, err
}

// Session is a multi-turn conversation handle on a single codex app-server
// thread. Each Turn sends one user prompt and drains events until the
// turn reaches a terminal state. Close terminates the subprocess and
// reaps it.
//
// Implementations are not safe for concurrent use: callers must serialize
// Turn calls per session.
type Session interface {
	// Turn sends a single user prompt and returns the resulting RunResult
	// after the turn reaches a terminal state.
	Turn(ctx context.Context, prompt string) (types.RunResult, error)
	// Close shuts down the session. It is safe to call multiple times.
	Close() error
}

// MultiTurnRunner is implemented by runners that can drive multiple turns
// on the same agent thread for one issue's implementation phase. Single-
// turn callers continue to use AgentRunner.Run; the orchestrator uses
// MultiTurnRunner only when agent.multi_turn is enabled and the runner
// reports support.
type MultiTurnRunner interface {
	// OpenSession starts a new agent thread and returns a Session for it.
	// Returns ErrMultiTurnUnsupported if the runner does not support it
	// (e.g. exec mode or another provider).
	OpenSession(ctx context.Context, req types.RunRequest) (Session, error)
}

// ErrMultiTurnUnsupported is returned by MultiTurnRunner.OpenSession when
// the runner cannot drive a multi-turn session in its current mode.
var ErrMultiTurnUnsupported = errors.New("runner: multi-turn session unsupported")

// OpenSession implements MultiTurnRunner. Only `codex.mode == "app-server"`
// supports multi-turn sessions; other modes return ErrMultiTurnUnsupported.
func (cr *CodexRunner) OpenSession(ctx context.Context, req types.RunRequest) (Session, error) {
	if cr.codexCfg.Mode != "app-server" {
		return nil, ErrMultiTurnUnsupported
	}
	return cr.openSession(ctx, req)
}

// codexSession is the live JSON-RPC client + subprocess for one thread.
type codexSession struct {
	cr       *CodexRunner
	cmd      *osexec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	threadID string
	auditPat []string

	// reader goroutine state.
	scanErr atomic.Value // error
	readWG  sync.WaitGroup
	msgs    chan *jsonrpcMessage // notifications + responses, fanned out by id

	// Pending request demultiplexing.
	mu        sync.Mutex
	nextID    int64
	pending   map[int64]chan *jsonrpcMessage
	notifyCh  chan *jsonrpcMessage // unbuffered fan-out for notifications
	closed    bool
	closeOnce sync.Once

	// Stderr capture (mutex-guarded to avoid races with the stderr-copy
	// goroutine, mirroring the fix applied in claude.go).
	stderrMu  sync.Mutex
	stderrBuf bytes.Buffer

	startedAt time.Time

	// watchdog observes inactivity at the JSON-RPC frame level. It is
	// installed by runAppServer/openSession when agent.stall_timeout_seconds
	// > 0 and is nil otherwise; the helper methods on stallWatchdog tolerate
	// a nil receiver.
	watchdog *stallWatchdog
}

// openSession spawns `codex app-server`, drives initialize → initialized →
// thread/start, and returns a codexSession ready for Turn calls.
func (cr *CodexRunner) openSession(ctx context.Context, req types.RunRequest) (*codexSession, error) {
	return cr.openSessionWithWatchdog(ctx, req, nil)
}

// openSessionWithWatchdog is openSession but attaches an event-inactivity
// watchdog so the readLoop can touch it on every JSON-RPC frame.
func (cr *CodexRunner) openSessionWithWatchdog(ctx context.Context, req types.RunRequest, watchdog *stallWatchdog) (*codexSession, error) {
	sandbox := sandboxForPhaseAxis(cr.codexCfg, req.Phase, req.AxisKey)

	cmd := osexec.CommandContext(ctx, cr.command, "app-server")
	cmd.Dir = req.RepoPath
	cmd.Env = xexec.BuildAgentEnv(cr.envCfg.Allowlist, cr.envCfg.BlockPatterns, os.Environ(), req.HomePath)
	cmd.Env = append(cmd.Env, req.ExtraEnv...)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex app-server: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("codex app-server: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("codex app-server: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("codex app-server: start: %w", err)
	}

	s := &codexSession{
		cr:        cr,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		auditPat:  cr.auditCfg.RedactPatterns,
		pending:   make(map[int64]chan *jsonrpcMessage),
		notifyCh:  make(chan *jsonrpcMessage, 256),
		startedAt: time.Now(),
		watchdog:  watchdog,
	}

	// Stderr copier — mutex-guarded buffer.
	s.readWG.Add(1)
	go func() {
		defer s.readWG.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				s.stderrMu.Lock()
				s.stderrBuf.Write(buf[:n])
				s.stderrMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Stdout reader: parse newline-delimited JSON-RPC frames, dispatch to
	// either a pending response channel or the notifications channel.
	s.readWG.Add(1)
	go s.readLoop()

	// Lifecycle handshake.
	if _, err := s.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "symphony-go",
			"version": "0.1",
		},
	}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex app-server: initialize: %w", err)
	}
	if err := s.notify("initialized", map[string]any{}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex app-server: initialized notify: %w", err)
	}

	threadParams := map[string]any{
		"cwd":     req.RepoPath,
		"sandbox": sandbox,
	}
	threadResp, err := s.call(ctx, "thread/start", threadParams)
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex app-server: thread/start: %w", err)
	}
	s.threadID = extractThreadID(threadResp)
	return s, nil
}

// readLoop is the long-running stdout consumer. It exits when stdout EOFs
// or a non-recoverable read error occurs.
func (s *codexSession) readLoop() {
	defer s.readWG.Done()
	defer func() {
		// Signal readers waiting on notifications/responses that no more
		// frames are coming.
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		close(s.notifyCh)
		s.mu.Unlock()
	}()

	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), appServerMaxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		s.watchdog.touch()
		var msg jsonrpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			slog.Warn("codex app-server: malformed JSON-RPC line ignored", "err", err.Error())
			continue
		}
		s.dispatch(&msg)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.scanErr.Store(err)
	}
}

// dispatch routes a parsed JSON-RPC message either to the goroutine
// awaiting its response (by id) or onto the notifications channel.
func (s *codexSession) dispatch(msg *jsonrpcMessage) {
	if msg.ID != nil && msg.Method == "" {
		// Response.
		var idNum int64
		if err := json.Unmarshal(*msg.ID, &idNum); err != nil {
			slog.Warn("codex app-server: response id not numeric", "id", string(*msg.ID))
			return
		}
		s.mu.Lock()
		ch, ok := s.pending[idNum]
		if ok {
			delete(s.pending, idNum)
		}
		s.mu.Unlock()
		if !ok {
			slog.Warn("codex app-server: orphan response", "id", idNum)
			return
		}
		ch <- msg
		close(ch)
		return
	}
	// Notification or server-initiated request — currently we only consume
	// notifications. Drop server requests with a warning.
	if msg.ID != nil {
		slog.Warn("codex app-server: unsolicited request from server ignored", "method", msg.Method)
		return
	}
	select {
	case s.notifyCh <- msg:
	default:
		// Channel full. Drop oldest by draining one and re-pushing — keep
		// the loop responsive rather than block the reader and stall the
		// whole protocol.
		select {
		case <-s.notifyCh:
		default:
		}
		select {
		case s.notifyCh <- msg:
		default:
		}
	}
}

// call sends a JSON-RPC request and waits for the matching response,
// honoring ctx for cancellation.
func (s *codexSession) call(ctx context.Context, method string, params any) (*jsonrpcMessage, error) {
	id := atomic.AddInt64(&s.nextID, 1)
	ch := make(chan *jsonrpcMessage, 1)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("codex app-server: session closed")
	}
	s.pending[id] = ch
	s.mu.Unlock()

	if err := s.writeFrame(id, method, params); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	case msg, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("codex app-server: %s: connection closed", method)
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("codex app-server: %s: rpc error %d: %s", method, msg.Error.Code, msg.Error.Message)
		}
		return msg, nil
	}
}

// notify sends a JSON-RPC notification (no id).
func (s *codexSession) notify(method string, params any) error {
	return s.writeFrame(-1, method, params)
}

// writeFrame serializes one JSON-RPC frame and writes it as a single
// newline-delimited line. id < 0 means notification (omit id).
func (s *codexSession) writeFrame(id int64, method string, params any) error {
	frame := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if id >= 0 {
		frame["id"] = id
	}
	if params != nil {
		frame["params"] = params
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("codex app-server: marshal %s: %w", method, err)
	}
	data = append(data, '\n')
	if _, err := s.stdin.Write(data); err != nil {
		return fmt.Errorf("codex app-server: write %s: %w", method, err)
	}
	return nil
}

// Turn implements Session.Turn: send turn/start, drain notifications until
// a terminal turn/completed event, build a RunResult.
func (s *codexSession) Turn(ctx context.Context, prompt string) (types.RunResult, error) {
	turnStart := time.Now()
	res := types.RunResult{StartedAt: turnStart}

	params := map[string]any{
		"threadId": s.threadID,
		"input": []any{
			map[string]any{"type": "text", "text": prompt},
		},
	}
	if _, err := s.call(ctx, "turn/start", params); err != nil {
		res.CompletedAt = time.Now()
		res.Stderr = s.snapshotStderr()
		return res, err
	}

	var (
		eventsBuf     bytes.Buffer
		textParts     []string
		fallbackTexts []string
		anyAgentMsg   bool
		terminalKind  string // "completed" | "failed" | "interrupted"
	)

DRAIN:
	for {
		select {
		case <-ctx.Done():
			res.CompletedAt = time.Now()
			res.Stderr = s.snapshotStderr()
			res.Events = []byte(xexec.Redact(eventsBuf.String(), s.auditPat))
			return res, ctx.Err()
		case msg, ok := <-s.notifyCh:
			if !ok {
				// Reader closed; subprocess gone.
				break DRAIN
			}
			// Append raw frame to events for audit (best effort).
			if raw, err := json.Marshal(msg); err == nil {
				eventsBuf.Write(raw)
				eventsBuf.WriteByte('\n')
			}
			kind, text, isAgentMsg, isTerminal, status := classifyNotification(msg)
			if isAgentMsg && text != "" {
				textParts = append(textParts, text)
				anyAgentMsg = true
			} else if !isAgentMsg && text != "" {
				fallbackTexts = append(fallbackTexts, text)
			}
			if isTerminal {
				terminalKind = status
				_ = kind
				break DRAIN
			}
		}
	}

	res.CompletedAt = time.Now()
	switch {
	case anyAgentMsg:
		res.Text = strings.Join(textParts, "")
	case len(fallbackTexts) > 0:
		res.Text = strings.Join(fallbackTexts, "")
	}
	res.Events = []byte(xexec.Redact(eventsBuf.String(), s.auditPat))
	res.Stderr = s.snapshotStderr()
	res.Success = terminalKind == "completed"
	return res, nil
}

// snapshotStderr returns the redacted stderr captured so far.
func (s *codexSession) snapshotStderr() string {
	s.stderrMu.Lock()
	out := s.stderrBuf.String()
	s.stderrMu.Unlock()
	return xexec.Redact(out, s.auditPat)
}

// Close terminates the subprocess and waits for the reader goroutines to
// drain. Idempotent.
func (s *codexSession) Close() error {
	var first error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			// Best-effort SIGTERM; CommandContext + WaitDelay will SIGKILL
			// after 10s if the process ignores it.
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
		}
		if s.cmd != nil {
			if err := s.cmd.Wait(); err != nil {
				var exitErr *osexec.ExitError
				if !errors.As(err, &exitErr) {
					first = err
				}
			}
		}
		s.readWG.Wait()
	})
	return first
}

// classifyNotification inspects a JSON-RPC notification and extracts the
// relevant fields for turn-completion detection and agent-text capture.
//
// It tolerates two related shapes that codex versions emit:
//
//  1. method == "item.completed" with params containing
//     {"item":{"item_type":"agent_message","text":"..."}} OR
//     {"item_type":"agent_message","text":"..."}
//  2. method == "turn/completed" or "turn.completed" with params
//     {"turn":{"status":"completed|failed|interrupted"}} or
//     {"status":"..."}
func classifyNotification(msg *jsonrpcMessage) (kind, text string, isAgentMsg, isTerminal bool, status string) {
	if msg == nil || msg.Method == "" {
		return "", "", false, false, ""
	}
	method := msg.Method
	// Permissive shape decode.
	var p struct {
		ItemType string          `json:"item_type"`
		Text     string          `json:"text"`
		Status   string          `json:"status"`
		Item     json.RawMessage `json:"item"`
		Turn     json.RawMessage `json:"turn"`
	}
	if len(msg.Params) > 0 {
		_ = json.Unmarshal(msg.Params, &p)
	}
	// If params nest under "item" or "turn", peel one layer.
	if p.ItemType == "" && len(p.Item) > 0 {
		var inner struct {
			ItemType string `json:"item_type"`
			Text     string `json:"text"`
		}
		_ = json.Unmarshal(p.Item, &inner)
		if inner.ItemType != "" {
			p.ItemType = inner.ItemType
		}
		if p.Text == "" {
			p.Text = inner.Text
		}
	}
	if p.Status == "" && len(p.Turn) > 0 {
		var inner struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(p.Turn, &inner)
		p.Status = inner.Status
	}

	switch {
	case method == "item.completed" || method == "item/completed":
		kind = "item.completed"
		text = p.Text
		isAgentMsg = p.ItemType == "agent_message"
	case method == "turn.completed" || method == "turn/completed":
		kind = "turn.completed"
		isTerminal = true
		status = p.Status
		if status == "" {
			status = "completed"
		}
	case method == "turn.failed" || method == "turn/failed":
		kind = "turn.failed"
		isTerminal = true
		status = "failed"
	}
	return
}

// extractThreadID pulls thread id from a thread/start response. Tolerates
// both `result.thread.id` and `result.id` shapes.
func extractThreadID(resp *jsonrpcMessage) string {
	if resp == nil || len(resp.Result) == 0 {
		return ""
	}
	var r struct {
		ID     string `json:"id"`
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return ""
	}
	if r.Thread.ID != "" {
		return r.Thread.ID
	}
	return r.ID
}

// sandboxForPhase resolves the per-thread sandbox value for the given
// phase by scanning the matching CodexConfig argv slice for a `--sandbox`
// flag. When absent, defaults to the safest value, "read-only".
//
// Equivalent to sandboxForPhaseAxis with an empty axis key.
func sandboxForPhase(cfg config.CodexConfig, phase types.Phase) string {
	return sandboxForPhaseAxis(cfg, phase, "")
}

// sandboxForPhaseAxis is the per-axis variant of sandboxForPhase. When
// axisKey is non-empty and the matching `*_args_by_label` map is set, the
// resolved per-axis argv slice is scanned for `--sandbox` first; otherwise
// the scalar slice is used. Returns "read-only" when no flag is found.
func sandboxForPhaseAxis(cfg config.CodexConfig, phase types.Phase, axisKey string) string {
	var args []string
	var byLabel config.OrderedMap[[]string]
	switch phase {
	case types.PhasePlanning:
		args = cfg.PlanningArgs
		byLabel = cfg.PlanningArgsByLabel
	case types.PhaseImplementation:
		args = cfg.ImplementationArgs
		byLabel = cfg.ImplementationArgsByLabel
	case types.PhaseReview:
		args = cfg.ReviewArgs
		byLabel = cfg.ReviewArgsByLabel
	}
	if v, ok := resolveByAxisStrings(byLabel, axisKey); ok {
		args = v
	}
	if v := sandboxFromArgs(args); v != "" {
		return v
	}
	return "read-only"
}

// sandboxFromArgs scans an argv slice for `--sandbox VALUE` or
// `--sandbox=VALUE` and returns VALUE if found.
func sandboxFromArgs(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--sandbox" && i+1 < len(args) {
			return args[i+1]
		}
		const pfx = "--sandbox="
		if strings.HasPrefix(a, pfx) {
			return strings.TrimPrefix(a, pfx)
		}
	}
	return ""
}
