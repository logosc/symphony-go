package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/types"
)

// newAppServerRunnerForTest returns a CodexRunner pre-wired for the
// app-server backend, with sandbox flags in the per-phase argv slices so
// sandboxForPhase can resolve them.
func newAppServerRunnerForTest() *CodexRunner {
	return NewCodexRunner(
		config.AgentConfig{Provider: "codex"},
		config.CodexConfig{
			Mode:               "app-server",
			PlanningArgs:       []string{"--sandbox", "read-only"},
			ImplementationArgs: []string{"--sandbox", "workspace-write"},
			ReviewArgs:         []string{"--sandbox", "read-only"},
		},
		config.EnvConfig{
			Allowlist:     []string{"OPENAI_API_KEY", "PATH"},
			BlockPatterns: []string{".*TOKEN.*"},
		},
		config.AuditConfig{},
	)
}

// writeFakeAppServer writes a bash script that emulates `codex app-server`.
// `frames` is a sequence of literal stdout response lines (already JSON,
// one per stdin frame received). The script reads exactly len(frames)
// lines from stdin, emitting frames[i] after reading line i. After the
// last frame it exits 0.
//
// To support arbitrary delays and "drain remaining stdin without writing
// more", a frame may be the special string "@@SLEEP=<seconds>" which
// causes the script to sleep that many seconds without sending a frame
// and without consuming another stdin line.
func writeFakeAppServer(t *testing.T, dir string, frames []string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("set -u\n")
	for _, f := range frames {
		if strings.HasPrefix(f, "@@SLEEP=") {
			secs := strings.TrimPrefix(f, "@@SLEEP=")
			b.WriteString("sleep " + secs + "\n")
			continue
		}
		// Read one line, ignoring its content; respond with f if non-empty.
		b.WriteString("IFS= read -r _line\n")
		if f != "" {
			// Use printf %s\n with the JSON quoted via single quotes;
			// escape any single quotes inside.
			b.WriteString("printf '%s\\n' " + shellQuote(f) + "\n")
		}
	}
	// Drain remaining stdin so the parent never blocks on Write.
	b.WriteString("cat >/dev/null\n")
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

// shellQuote returns s wrapped in single quotes, escaping internal single
// quotes via the standard `'\”` idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// rpcID returns a JSON-RPC response frame for id with the given result.
func rpcResponse(id int, result string) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"result":` + result + `}`
}

func TestCodexAppServer_HappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-based test")
	}
	dir := t.TempDir()
	// Stdin sequence: initialize(req), initialized(notif), thread/start(req),
	// turn/start(req). One frame entry per stdin line read. Empty entry
	// means "consume the line but emit nothing".
	frames := []string{
		// after initialize request
		rpcResponse(1, `{"protocolVersion":"1"}`),
		// after initialized notification — no response.
		"",
		// after thread/start request
		rpcResponse(2, `{"thread":{"id":"th_abc"}}`),
		// after turn/start request — emit response + notifications.
		rpcResponse(3, `{"turn":{"id":"tr_1"}}`) +
			"\n" + `{"jsonrpc":"2.0","method":"item.completed","params":{"item_type":"agent_message","text":"hello"}}` +
			"\n" + `{"jsonrpc":"2.0","method":"turn.completed","params":{"status":"completed"}}`,
	}
	path := writeFakeAppServer(t, dir, frames)

	cr := newAppServerRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		Prompt:   "do it",
		RepoPath: repo,
		HomePath: home,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Success {
		t.Errorf("expected Success, stderr=%q events=%q", res.Stderr, string(res.Events))
	}
	if res.Text != "hello" {
		t.Errorf("Text = %q, want %q", res.Text, "hello")
	}
	if !strings.Contains(string(res.Events), "turn.completed") {
		t.Errorf("Events missing turn.completed: %s", string(res.Events))
	}
}

func TestCodexAppServer_TurnFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-based test")
	}
	dir := t.TempDir()
	frames := []string{
		rpcResponse(1, `{"protocolVersion":"1"}`),
		"",
		rpcResponse(2, `{"thread":{"id":"th_abc"}}`),
		rpcResponse(3, `{"turn":{"id":"tr_1"}}`) +
			"\n" + `{"jsonrpc":"2.0","method":"turn.failed","params":{"reason":"explosion"}}`,
	}
	path := writeFakeAppServer(t, dir, frames)
	cr := newAppServerRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		Prompt:   "x",
		RepoPath: repo,
		HomePath: home,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Success {
		t.Errorf("expected Success=false on turn.failed")
	}
}

func TestCodexAppServer_CancelTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-based test")
	}
	dir := t.TempDir()
	frames := []string{
		rpcResponse(1, `{"protocolVersion":"1"}`),
		"",
		rpcResponse(2, `{"thread":{"id":"th_abc"}}`),
		// turn/start ack but no terminal event — sleep until killed.
		rpcResponse(3, `{"turn":{"id":"tr_1"}}`),
		"@@SLEEP=30",
	}
	path := writeFakeAppServer(t, dir, frames)
	cr := newAppServerRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	start := time.Now()
	_, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		Prompt:   "x",
		RepoPath: repo,
		HomePath: home,
		Timeout:  500 * time.Millisecond,
	})
	dur := time.Since(start)
	if err == nil {
		t.Logf("note: Run returned nil error (ctx err propagated as Result.Success=false)")
	}
	if dur > 15*time.Second {
		t.Errorf("Run took too long under timeout: %v", dur)
	}
}

func TestCodexAppServer_MalformedLineSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-based test")
	}
	dir := t.TempDir()
	frames := []string{
		rpcResponse(1, `{"protocolVersion":"1"}`),
		"",
		rpcResponse(2, `{"thread":{"id":"th_abc"}}`),
		rpcResponse(3, `{"turn":{"id":"tr_1"}}`) +
			"\n" + `not-valid-json-at-all <<<` +
			"\n" + `{"jsonrpc":"2.0","method":"item.completed","params":{"item_type":"agent_message","text":"after-bad"}}` +
			"\n" + `{"jsonrpc":"2.0","method":"turn.completed","params":{"status":"completed"}}`,
	}
	path := writeFakeAppServer(t, dir, frames)
	cr := newAppServerRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		Prompt:   "x",
		RepoPath: repo,
		HomePath: home,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Success {
		t.Errorf("expected Success despite malformed line; stderr=%q", res.Stderr)
	}
	if res.Text != "after-bad" {
		t.Errorf("Text = %q, want %q", res.Text, "after-bad")
	}
}

func TestSandboxFromArgs(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--sandbox", "workspace-write"}, "workspace-write"},
		{[]string{"--sandbox=danger-full-access"}, "danger-full-access"},
		{[]string{"--other", "x"}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		got := sandboxFromArgs(tc.args)
		if got != tc.want {
			t.Errorf("sandboxFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestSandboxForPhase_Defaults(t *testing.T) {
	cfg := config.CodexConfig{}
	for _, p := range []types.Phase{types.PhasePlanning, types.PhaseImplementation, types.PhaseReview} {
		got := sandboxForPhase(cfg, p)
		if got != "read-only" {
			t.Errorf("phase %q: got %q, want read-only (default)", p, got)
		}
	}
}

func TestCodexRunner_OpenSession_RejectsExecMode(t *testing.T) {
	cr := NewCodexRunner(
		config.AgentConfig{},
		config.CodexConfig{Mode: "exec"},
		config.EnvConfig{},
		config.AuditConfig{},
	)
	_, err := cr.OpenSession(context.Background(), types.RunRequest{Phase: types.PhasePlanning})
	if !errors.Is(err, ErrMultiTurnUnsupported) {
		t.Errorf("err = %v, want ErrMultiTurnUnsupported", err)
	}
}
