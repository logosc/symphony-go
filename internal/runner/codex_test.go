package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/types"
)

// newCodexRunnerForTest returns a CodexRunner with sane defaults for tests.
func newCodexRunnerForTest() *CodexRunner {
	return NewCodexRunner(
		config.AgentConfig{Provider: "codex"},
		config.CodexConfig{
			Mode:               "exec",
			PlanningArgs:       []string{"--sandbox", "read-only"},
			ImplementationArgs: []string{"--sandbox", "workspace-write"},
			ReviewArgs:         []string{"--sandbox", "read-only"},
		},
		config.EnvConfig{
			Allowlist:     []string{"OPENAI_API_KEY", "PATH"},
			BlockPatterns: []string{".*TOKEN.*", ".*SECRET.*"},
		},
		config.AuditConfig{
			RedactPatterns: []string{`sk-[A-Za-z0-9_-]+`},
		},
	)
}

func TestCodexBuildArgvSnapshot(t *testing.T) {
	cr := newCodexRunnerForTest()
	cases := []struct {
		phase types.Phase
		want  []string
	}{
		{types.PhasePlanning, []string{"exec", "--json", "--sandbox", "read-only"}},
		{types.PhaseReview, []string{"exec", "--json", "--sandbox", "read-only"}},
		{types.PhaseImplementation, []string{"exec", "--json", "--sandbox", "workspace-write"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.phase), func(t *testing.T) {
			got, err := cr.buildArgv(tc.phase, "")
			if err != nil {
				t.Fatalf("buildArgv(%q) error: %v", tc.phase, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildArgv(%q) = %v, want %v", tc.phase, got, tc.want)
			}
		})
	}
}

// TestCodexBuildArgvPerAxis verifies per-axis sandbox/argv selection.
// type:research gets read-only across all phases (incl. implementation);
// type:code keeps workspace-write for implementation. An empty AxisKey
// falls back to the "default" entry, and an unknown axis falls back too.
func TestCodexBuildArgvPerAxis(t *testing.T) {
	cr := NewCodexRunner(
		config.AgentConfig{Provider: "codex"},
		config.CodexConfig{
			Mode: "exec",
			ImplementationArgsByLabel: config.OrderedMap[[]string]{
				Keys: []string{"type:research", "type:code", "default"},
				Values: map[string][]string{
					"type:research": {"--sandbox", "read-only"},
					"type:code":     {"--sandbox", "workspace-write"},
					"default":       {"--sandbox", "workspace-write"},
				},
			},
			PlanningArgsByLabel: config.OrderedMap[[]string]{
				Keys: []string{"default"},
				Values: map[string][]string{
					"default": {"--sandbox", "read-only"},
				},
			},
			ReviewArgsByLabel: config.OrderedMap[[]string]{
				Keys: []string{"default"},
				Values: map[string][]string{
					"default": {"--sandbox", "read-only"},
				},
			},
		},
		config.EnvConfig{},
		config.AuditConfig{},
	)

	cases := []struct {
		name     string
		phase    types.Phase
		axisKey  string
		wantArgv []string
	}{
		{"research-impl-readonly", types.PhaseImplementation, "type:research", []string{"exec", "--json", "--sandbox", "read-only"}},
		{"code-impl-workspace", types.PhaseImplementation, "type:code", []string{"exec", "--json", "--sandbox", "workspace-write"}},
		{"empty-axis-default", types.PhaseImplementation, "", []string{"exec", "--json", "--sandbox", "workspace-write"}},
		{"unknown-axis-default", types.PhaseImplementation, "type:nope", []string{"exec", "--json", "--sandbox", "workspace-write"}},
		{"planning-readonly", types.PhasePlanning, "type:research", []string{"exec", "--json", "--sandbox", "read-only"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cr.buildArgv(tc.phase, tc.axisKey)
			if err != nil {
				t.Fatalf("buildArgv: %v", err)
			}
			if !reflect.DeepEqual(got, tc.wantArgv) {
				t.Errorf("buildArgv = %v, want %v", got, tc.wantArgv)
			}
		})
	}
}

func TestCodexUnknownModeRejected(t *testing.T) {
	cr := NewCodexRunner(
		config.AgentConfig{},
		config.CodexConfig{Mode: "bogus"},
		config.EnvConfig{},
		config.AuditConfig{},
	)
	_, err := cr.Run(context.Background(), types.RunRequest{Phase: types.PhasePlanning})
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
	if !strings.Contains(err.Error(), "unsupported mode") {
		t.Errorf("error message not informative: %v", err)
	}
}

// writeFakeCodex writes a bash script at <dir>/codex that emits the given
// stdout body and exits with the given code. Returns the script path.
// Skips the test if bash is not on PATH.
func writeFakeCodex(t *testing.T, dir, body string, exitCode int) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	script := "#!/usr/bin/env bash\n" +
		"# read and discard stdin so the parent doesn't get EPIPE\n" +
		"cat >/dev/null\n" +
		body + "\n" +
		"exit " + itoa(exitCode) + "\n"
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestCodexEnvDropsGitHubToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-script-based test")
	}
	dir := t.TempDir()
	// Fake codex that writes its env to a file in HOME for inspection,
	// then emits a minimal turn.completed event.
	envOut := filepath.Join(dir, "env.dump")
	body := "env > " + envOut + "\n" +
		`printf '%s\n' '{"type":"turn.completed"}'`
	path := writeFakeCodex(t, dir, body, 0)

	t.Setenv("GITHUB_TOKEN", "ghp_should_be_dropped")
	t.Setenv("OPENAI_API_KEY", "sk-test-12345")

	cr := newCodexRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		Prompt:   "hello",
		RepoPath: repo,
		HomePath: home,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Success {
		t.Errorf("expected success, got stderr=%q events=%q", res.Stderr, string(res.Events))
	}

	dumped, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	envText := string(dumped)
	if strings.Contains(envText, "GITHUB_TOKEN=") {
		t.Errorf("GITHUB_TOKEN leaked into agent env:\n%s", envText)
	}
	// Sanity: HOME and CI=true should be present.
	if !strings.Contains(envText, "HOME="+home) {
		t.Errorf("HOME not set to %q in env:\n%s", home, envText)
	}
	if !strings.Contains(envText, "CI=true") {
		t.Errorf("CI=true not set in env:\n%s", envText)
	}
}

func TestCodexParsesJSONLAndExtractsText(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-script-based test")
	}
	dir := t.TempDir()
	body := `printf '%s\n' '{"type":"thread.started"}'
printf '%s\n' '{"type":"turn.started"}'
printf '%s\n' '{"type":"item.completed","item_type":"agent_message","text":"Hello "}'
printf '%s\n' '{"type":"item.completed","item_type":"agent_message","text":"world."}'
printf '%s\n' '{"type":"turn.completed"}'`
	path := writeFakeCodex(t, dir, body, 0)

	cr := newCodexRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		Prompt:   "go",
		RepoPath: repo,
		HomePath: home,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Success {
		t.Errorf("expected Success, got false; stderr=%q", res.Stderr)
	}
	if res.Text != "Hello world." {
		t.Errorf("Text = %q, want %q", res.Text, "Hello world.")
	}
	if !strings.Contains(string(res.Events), "turn.completed") {
		t.Errorf("Events missing turn.completed: %s", string(res.Events))
	}
}

func TestCodexTurnFailedNotSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-script-based test")
	}
	dir := t.TempDir()
	body := `printf '%s\n' '{"type":"turn.started"}'
printf '%s\n' '{"type":"turn.failed"}'`
	path := writeFakeCodex(t, dir, body, 0)

	cr := newCodexRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
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

func TestCodexTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-script-based test")
	}
	dir := t.TempDir()
	body := "sleep 30\n" +
		`printf '%s\n' '{"type":"turn.completed"}'`
	path := writeFakeCodex(t, dir, body, 0)

	cr := newCodexRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	start := time.Now()
	_, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		RepoPath: repo,
		HomePath: home,
		Timeout:  500 * time.Millisecond,
	})
	dur := time.Since(start)
	if err == nil {
		t.Logf("note: Run returned no error; ctx err is surfaced normally")
	}
	// SIGTERM + WaitDelay 10s upper bound; in practice should be quick.
	if dur > 15*time.Second {
		t.Errorf("Run took too long under timeout: %v", dur)
	}
}

func TestCodexMalformedJSONLDoesNotCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-script-based test")
	}
	dir := t.TempDir()
	body := `printf '%s\n' '{"type":"thread.started"}'
printf '%s\n' 'this is not json at all <<<'
printf '%s\n' '{"type":"item.completed","item_type":"agent_message","text":"after-bad-line"}'
printf '%s\n' '{"type":"turn.completed"}'`
	path := writeFakeCodex(t, dir, body, 0)

	cr := newCodexRunnerForTest().WithCommand(path)
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
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
	if res.Text != "after-bad-line" {
		t.Errorf("Text = %q, want %q", res.Text, "after-bad-line")
	}
}

// TestCodexStallTimeout exercises the event-inactivity watchdog in exec
// mode: the fake emits one event, sleeps for 5x the configured stall
// window, then emits a turn.completed event. The watchdog must cancel
// the subprocess before the trailing event arrives.
func TestCodexStallTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-script-based test")
	}
	dir := t.TempDir()
	// SIGTERM trap + background sleep + wait so bash exits promptly when
	// the watchdog signals the subprocess (see TestClaudeStallTimeout).
	body := `trap 'kill $SLEEP_PID 2>/dev/null; exit 143' TERM
printf '%s\n' '{"type":"thread.started"}'
sleep 5 >&- 2>&- &
SLEEP_PID=$!
wait $SLEEP_PID
printf '%s\n' '{"type":"turn.completed"}'`
	path := writeFakeCodex(t, dir, body, 0)

	cr := newCodexRunnerForTest().WithCommand(path)
	cr.agentCfg.StallTimeoutSeconds = 1

	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	start := time.Now()
	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		RepoPath: repo,
		HomePath: home,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected stall error, got nil")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected stall error, got: %v", err)
	}
	if res.Success {
		t.Errorf("expected Success=false on stall")
	}
	if elapsed > 4*time.Second {
		t.Errorf("stall watchdog too slow: %v", elapsed)
	}
}

// TestCodexStallTimeoutDisabled confirms the same fake completes normally
// when StallTimeoutSeconds=0.
func TestCodexStallTimeoutDisabled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash-script-based test")
	}
	dir := t.TempDir()
	body := `printf '%s\n' '{"type":"thread.started"}'
sleep 5
printf '%s\n' '{"type":"turn.completed"}'`
	path := writeFakeCodex(t, dir, body, 0)

	cr := newCodexRunnerForTest().WithCommand(path)
	cr.agentCfg.StallTimeoutSeconds = 0

	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(home, 0o755)
	_ = os.MkdirAll(repo, 0o755)

	res, err := cr.Run(context.Background(), types.RunRequest{
		Phase:    types.PhasePlanning,
		RepoPath: repo,
		HomePath: home,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Success {
		t.Errorf("expected Success=true with watchdog disabled, stderr=%q", res.Stderr)
	}
}
