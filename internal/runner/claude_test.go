package runner

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/types"
)

func newTestClaudeCfg() config.ClaudeConfig {
	return config.ClaudeConfig{
		MaxTurns:            7,
		PlanningTools:       []string{"Read", "Grep"},
		ImplementationTools: []string{"Read", "Edit", "Write"},
		ReviewTools:         []string{"Read", "Glob"},
		DisallowedTools:     []string{"Bash(sudo:*)", "Bash(curl:*)"},
	}
}

func newTestAgentCfg() config.AgentConfig {
	return config.AgentConfig{Provider: "claude", Model: "sonnet", TimeoutSeconds: 60}
}

func TestClaudeBuildArgsByPhase(t *testing.T) {
	cr := NewClaudeRunner(newTestAgentCfg(), newTestClaudeCfg(), config.EnvConfig{}, config.AuditConfig{})

	cases := []struct {
		phase            types.Phase
		wantPermission   string
		wantAllowedTools string
	}{
		{types.PhasePlanning, "acceptEdits", "Read,Grep"},
		{types.PhaseReview, "plan", "Read,Glob"},
		{types.PhaseImplementation, "acceptEdits", "Read,Edit,Write"},
	}

	for _, tc := range cases {
		t.Run(string(tc.phase), func(t *testing.T) {
			args := cr.buildArgs(tc.phase, "")

			want := []string{
				"-p",
				"--output-format", "stream-json",
				"--verbose",
				"--model", "sonnet",
				"--max-turns", "7",
				"--permission-mode", tc.wantPermission,
				"--allowedTools", tc.wantAllowedTools,
				"--disallowedTools", "Bash(sudo:*),Bash(curl:*)",
			}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("argv mismatch\n got: %#v\nwant: %#v", args, want)
			}

			// Prompt must never be in argv.
			prompt := "this is the secret prompt body"
			for _, a := range args {
				if strings.Contains(a, prompt) {
					t.Fatalf("prompt leaked into argv: %q", a)
				}
			}
		})
	}
}

// TestClaudeBuildArgsPerAxis exercises the per-axis tool selection path.
// When a `*_tools_by_label` map is set and the request carries an
// AxisKey, the runner must use the per-axis slice; an empty AxisKey must
// fall back to the map's "default" entry. When the map is empty, the
// scalar slice is used (covered by TestClaudeBuildArgsByPhase).
func TestClaudeBuildArgsPerAxis(t *testing.T) {
	cfg := config.ClaudeConfig{
		MaxTurns: 5,
		// Scalar slices intentionally empty — the per-axis maps drive
		// selection. (Validate would reject scalar+map for the same knob.)
		PlanningToolsByLabel: config.OrderedMap[[]string]{
			Keys: []string{"type:research", "default"},
			Values: map[string][]string{
				"type:research": {"Read", "WebFetch", "WebSearch"},
				"default":       {"Read"},
			},
		},
		ImplementationToolsByLabel: config.OrderedMap[[]string]{
			Keys: []string{"type:research", "default"},
			Values: map[string][]string{
				"type:research": {"Read", "Write"},
				"default":       {"Read", "Edit", "Write"},
			},
		},
		DisallowedToolsByLabel: config.OrderedMap[[]string]{
			Keys: []string{"type:research", "default"},
			Values: map[string][]string{
				"type:research": {"Bash(git:*)"},
				"default":       {"Bash(sudo:*)"},
			},
		},
	}
	cr := NewClaudeRunner(newTestAgentCfg(), cfg, config.EnvConfig{}, config.AuditConfig{})

	t.Run("axis selects per-axis slice", func(t *testing.T) {
		args := cr.buildArgs(types.PhasePlanning, "type:research")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--allowedTools Read,WebFetch,WebSearch") {
			t.Fatalf("expected research planning tools, got argv: %v", args)
		}
		if !strings.Contains(joined, "--disallowedTools Bash(git:*)") {
			t.Fatalf("expected research disallowed tools, got argv: %v", args)
		}
	})
	t.Run("empty axis falls back to default", func(t *testing.T) {
		args := cr.buildArgs(types.PhaseImplementation, "")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--allowedTools Read,Edit,Write") {
			t.Fatalf("expected default impl tools, got argv: %v", args)
		}
	})
	t.Run("unknown axis falls back to default", func(t *testing.T) {
		args := cr.buildArgs(types.PhasePlanning, "type:unknown")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--allowedTools Read") {
			t.Fatalf("expected default planning tools, got argv: %v", args)
		}
		// Make sure we don't accidentally emit the research list.
		if strings.Contains(joined, "WebFetch") {
			t.Fatalf("research tools leaked under unknown axis: %v", args)
		}
	})
}

func TestClaudeBuildArgsOmitsEmptyToolLists(t *testing.T) {
	cfg := config.ClaudeConfig{MaxTurns: 3}
	cr := NewClaudeRunner(newTestAgentCfg(), cfg, config.EnvConfig{}, config.AuditConfig{})
	args := cr.buildArgs(types.PhasePlanning, "")
	for _, a := range args {
		if a == "--allowedTools" || a == "--disallowedTools" {
			t.Fatalf("expected empty tool lists to be omitted, got argv: %v", args)
		}
	}
}

// writeFakeClaude writes a bash script to dir that, when invoked,
// produces stdout from script body. Returns the absolute path. Skips
// the test if bash is not in PATH.
func writeFakeClaude(t *testing.T, dir, body string) string {
	t.Helper()
	if _, err := osexec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH; skipping mock-binary test")
	}
	path := filepath.Join(dir, "claude")
	contents := "#!/usr/bin/env bash\n" + body + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

func TestClaudeRunExtractsResultEvent(t *testing.T) {
	dir := t.TempDir()
	// Drain stdin (so cmd.Stdin doesn't block) then emit canned JSONL.
	body := `cat >/dev/null
echo '{"type":"system","subtype":"init"}'
echo '{"type":"assistant","message":"thinking"}'
echo '{"type":"result","result":"final answer here"}'
`
	fake := writeFakeClaude(t, dir, body)

	home := t.TempDir()
	repo := t.TempDir()

	cr := NewClaudeRunner(newTestAgentCfg(), newTestClaudeCfg(), config.EnvConfig{}, config.AuditConfig{}, WithCommand(fake))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := cr.Run(ctx, types.RunRequest{
		RepoPath: repo,
		HomePath: home,
		Prompt:   "hello world prompt",
		Phase:    types.PhasePlanning,
	})
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got stderr=%q", res.Stderr)
	}
	if res.Text != "final answer here" {
		t.Fatalf("expected result text from result event, got %q", res.Text)
	}
	if !strings.Contains(string(res.Events), `"type":"result"`) {
		t.Fatalf("expected events to contain result line, got %q", res.Events)
	}
}

func TestClaudeRunUsesAssistantTextWhenResultEmpty(t *testing.T) {
	dir := t.TempDir()
	body := `cat >/dev/null
echo '{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"visible answer"}]}}'
echo '{"type":"result","result":""}'
`
	fake := writeFakeClaude(t, dir, body)

	cr := NewClaudeRunner(newTestAgentCfg(), newTestClaudeCfg(), config.EnvConfig{}, config.AuditConfig{}, WithCommand(fake))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := cr.Run(ctx, types.RunRequest{
		RepoPath: t.TempDir(),
		HomePath: t.TempDir(),
		Phase:    types.PhasePlanning,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "visible answer" {
		t.Fatalf("expected assistant text fallback, got %q", res.Text)
	}
}

func TestClaudeRunErrorEventMarksFailure(t *testing.T) {
	dir := t.TempDir()
	body := `cat >/dev/null
echo '{"type":"error","message":"boom"}'
echo '{"type":"result","result":"recovered text"}'
`
	fake := writeFakeClaude(t, dir, body)

	cr := NewClaudeRunner(newTestAgentCfg(), newTestClaudeCfg(), config.EnvConfig{}, config.AuditConfig{}, WithCommand(fake))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := cr.Run(ctx, types.RunRequest{
		RepoPath: t.TempDir(),
		HomePath: t.TempDir(),
		Phase:    types.PhasePlanning,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Success {
		t.Fatalf("expected Success=false on error event, got true")
	}
}

func TestClaudeRunFallbackWithoutResultEvent(t *testing.T) {
	dir := t.TempDir()
	body := `cat >/dev/null
echo 'plain stdout line one'
echo 'plain stdout line two'
`
	fake := writeFakeClaude(t, dir, body)
	cr := NewClaudeRunner(newTestAgentCfg(), newTestClaudeCfg(), config.EnvConfig{}, config.AuditConfig{}, WithCommand(fake))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cr.Run(ctx, types.RunRequest{
		RepoPath: t.TempDir(),
		HomePath: t.TempDir(),
		Phase:    types.PhasePlanning,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Text, "plain stdout line one") {
		t.Fatalf("expected fallback to concatenated stdout, got %q", res.Text)
	}
}

func TestClaudeRunDropsGitHubToken(t *testing.T) {
	dir := t.TempDir()
	// Print env to a sentinel file so the test can read it back.
	envFile := filepath.Join(dir, "env.txt")
	body := fmt.Sprintf(`cat >/dev/null
env > %s
echo '{"type":"result","result":"ok"}'
`, envFile)
	fake := writeFakeClaude(t, dir, body)

	t.Setenv("GITHUB_TOKEN", "should-not-leak-xyz")
	t.Setenv("ANTHROPIC_API_KEY", "anth-allowed-abc")

	cr := NewClaudeRunner(
		newTestAgentCfg(),
		newTestClaudeCfg(),
		config.EnvConfig{
			Allowlist:     []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN"}, // even allowlisted, must drop
			BlockPatterns: []string{".*TOKEN.*"},
		},
		config.AuditConfig{},
		WithCommand(fake),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cr.Run(ctx, types.RunRequest{
		RepoPath: t.TempDir(),
		HomePath: t.TempDir(),
		Phase:    types.PhasePlanning,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, stderr=%q", res.Stderr)
	}
	contents, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env sentinel: %v", err)
	}
	envStr := string(contents)
	if strings.Contains(envStr, "should-not-leak-xyz") {
		t.Fatalf("GITHUB_TOKEN value leaked to subprocess env: %s", envStr)
	}
	if strings.Contains(envStr, "GITHUB_TOKEN=") {
		t.Fatalf("GITHUB_TOKEN name leaked to subprocess env: %s", envStr)
	}
	if !strings.Contains(envStr, "ANTHROPIC_API_KEY=anth-allowed-abc") {
		t.Fatalf("expected ANTHROPIC_API_KEY in subprocess env, got: %s", envStr)
	}
	if !strings.Contains(envStr, "CI=true") {
		t.Fatalf("expected CI=true in subprocess env, got: %s", envStr)
	}
}

func TestClaudeRunTimeout(t *testing.T) {
	dir := t.TempDir()
	body := `cat >/dev/null
sleep 30
echo '{"type":"result","result":"never"}'
`
	fake := writeFakeClaude(t, dir, body)
	cr := NewClaudeRunner(newTestAgentCfg(), newTestClaudeCfg(), config.EnvConfig{}, config.AuditConfig{}, WithCommand(fake))

	start := time.Now()
	res, err := cr.Run(context.Background(), types.RunRequest{
		RepoPath: t.TempDir(),
		HomePath: t.TempDir(),
		Phase:    types.PhasePlanning,
		Timeout:  200 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Success {
		t.Fatalf("expected Success=false on timeout")
	}
	// Allow generous slack for the 10s WaitDelay grace; on timeout SIGTERM
	// should land immediately and bash should exit promptly.
	if elapsed > 12*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

// TestClaudeStallTimeout exercises the event-inactivity watchdog. The fake
// emits one event, then sleeps for 5x the configured stall window before
// emitting more lines; the watchdog should cancel the subprocess via SIGTERM
// well before the trailing events arrive.
func TestClaudeStallTimeout(t *testing.T) {
	dir := t.TempDir()
	// SIGTERM trap + background sleep + wait so bash terminates promptly
	// when the watchdog cancels the run; without this bash is blocked in
	// the foreground sleep and the orphaned sleep child holds the stdout
	// pipe open until it completes naturally.
	body := `trap 'kill $SLEEP_PID 2>/dev/null; exit 143' TERM
cat >/dev/null
echo '{"type":"system","subtype":"init"}'
sleep 5 >&- 2>&- &
SLEEP_PID=$!
wait $SLEEP_PID
echo '{"type":"result","result":"too late"}'
`
	fake := writeFakeClaude(t, dir, body)

	agentCfg := newTestAgentCfg()
	agentCfg.StallTimeoutSeconds = 1

	cr := NewClaudeRunner(agentCfg, newTestClaudeCfg(), config.EnvConfig{}, config.AuditConfig{}, WithCommand(fake))

	start := time.Now()
	res, err := cr.Run(context.Background(), types.RunRequest{
		RepoPath: t.TempDir(),
		HomePath: t.TempDir(),
		Phase:    types.PhasePlanning,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected stall error, got nil")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected stall error, got: %v", err)
	}
	if res.Success {
		t.Fatalf("expected Success=false on stall")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("stall watchdog too slow: %v", elapsed)
	}
}

// TestClaudeStallTimeoutDisabled confirms the same fake completes normally
// when StallTimeoutSeconds=0 (the watchdog must be inert in that case).
func TestClaudeStallTimeoutDisabled(t *testing.T) {
	dir := t.TempDir()
	body := `cat >/dev/null
echo '{"type":"system","subtype":"init"}'
sleep 5
echo '{"type":"result","result":"finished"}'
`
	fake := writeFakeClaude(t, dir, body)

	agentCfg := newTestAgentCfg()
	agentCfg.StallTimeoutSeconds = 0

	cr := NewClaudeRunner(agentCfg, newTestClaudeCfg(), config.EnvConfig{}, config.AuditConfig{}, WithCommand(fake))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cr.Run(ctx, types.RunRequest{
		RepoPath: t.TempDir(),
		HomePath: t.TempDir(),
		Phase:    types.PhasePlanning,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected Success=true with watchdog disabled, stderr=%q", res.Stderr)
	}
	if res.Text != "finished" {
		t.Fatalf("expected final result text, got %q", res.Text)
	}
}
