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

	"github.com/chenlong-seu/symphony-go/internal/config"
	"github.com/chenlong-seu/symphony-go/internal/types"
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
		{types.PhasePlanning, "plan", "Read,Grep"},
		{types.PhaseReview, "plan", "Read,Glob"},
		{types.PhaseImplementation, "acceptEdits", "Read,Edit,Write"},
	}

	for _, tc := range cases {
		t.Run(string(tc.phase), func(t *testing.T) {
			args := cr.buildArgs(tc.phase)

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

func TestClaudeBuildArgsOmitsEmptyToolLists(t *testing.T) {
	cfg := config.ClaudeConfig{MaxTurns: 3}
	cr := NewClaudeRunner(newTestAgentCfg(), cfg, config.EnvConfig{}, config.AuditConfig{})
	args := cr.buildArgs(types.PhasePlanning)
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
