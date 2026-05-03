// Package exec provides a bounded `bash -lc` runner, env sanitization,
// secret redaction, and a deny-list command policy used for hooks and
// validation commands. See SPEC.md §10, §11, §13.
package exec

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// RunOptions configures a single Run invocation.
type RunOptions struct {
	// Cwd is the working directory for the command. Empty means inherit.
	Cwd string
	// Env is the pre-built sanitized environment. Caller is responsible
	// for sanitization; see BuildAgentEnv. A nil slice means empty env.
	Env []string
	// Timeout bounds the command's wall-clock duration. Zero means no
	// explicit timeout (the parent ctx is the only bound).
	Timeout time.Duration
	// Stdin is fed to the process on stdin. Nil means no stdin.
	Stdin []byte
}

// RunResult captures the outcome of a Run invocation.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	TimedOut bool
}

// Run executes `bash -lc <command>` with the given options. On context
// cancellation or timeout the process is sent SIGTERM and given a 10s
// WaitDelay before the kernel KILLs it. ExitCode is -1 when the process
// was killed by a signal.
func Run(ctx context.Context, command string, opts RunOptions) (RunResult, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, "bash", "-lc", command)
	cmd.Dir = opts.Cwd
	if opts.Env != nil {
		cmd.Env = opts.Env
	} else {
		cmd.Env = []string{}
	}
	if len(opts.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(opts.Stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	res := RunResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: dur,
		ExitCode: 0,
	}

	if err != nil {
		// Distinguish exit code from signal kill / timeout.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.ExitCode = -1
		}
		if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			res.TimedOut = true
			res.ExitCode = -1
			return res, nil
		}
		if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.Canceled) {
			res.ExitCode = -1
			return res, nil
		}
		// Real exit-code error — return result with no go-level error.
		if exitErr != nil {
			return res, nil
		}
		return res, err
	}

	return res, nil
}

// alwaysDrop are env names dropped regardless of allowlist. See SPEC §9.
var alwaysDrop = map[string]struct{}{
	"GITHUB_TOKEN":   {},
	"GH_TOKEN":       {},
	"SSH_AUTH_SOCK":  {},
}

// BuildAgentEnv constructs the env passed to an agent or hook subprocess.
// It starts from an empty env and includes only entries from baseEnv whose
// name is in allowlist AND does not match any pattern in blockPatterns.
// GITHUB_TOKEN, GH_TOKEN, and SSH_AUTH_SOCK are always dropped regardless
// of allowlist. HOME, TMPDIR (=home/tmp), and CI=true are always set.
//
// allowlist entries are matched as literal env-var names, not regexes.
// blockPatterns are compiled as regexes; invalid patterns are skipped.
func BuildAgentEnv(allowlist []string, blockPatterns []string, baseEnv []string, home string) []string {
	allow := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		allow[name] = struct{}{}
	}

	var blockREs []*regexp.Regexp
	for _, p := range blockPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		blockREs = append(blockREs, re)
	}

	out := make([]string, 0, len(allowlist)+3)
	for _, kv := range baseEnv {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if _, ok := allow[name]; !ok {
			continue
		}
		if _, drop := alwaysDrop[name]; drop {
			continue
		}
		blocked := false
		for _, re := range blockREs {
			if re.MatchString(name) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		out = append(out, kv)
	}

	out = append(out, "HOME="+home)
	out = append(out, "TMPDIR="+home+"/tmp")
	out = append(out, "CI=true")
	return out
}

// Redact replaces every regex match across all patterns with "[REDACTED]".
// Patterns are compiled once per call. Callers should cache the patterns
// list (and ideally the compiled forms) when used in a hot path.
// Invalid regex patterns are silently skipped.
func Redact(input string, patterns []string) string {
	out := input
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}

// denyKeywords are substring-lowercased deny matches for IsCommandSafe.
var denyKeywords = []string{
	"sudo",
	"ssh ",
	"scp ",
	"nc ",
	"netcat",
	"docker.sock",
	"rm -rf /",
	"chmod 777",
	"chown ",
}

// pipeShellRE matches "curl ... | sh", "wget ... | bash", and friends,
// allowing extra whitespace around the pipe.
var pipeShellRE = regexp.MustCompile(`(?i)(curl|wget)\b[^|]*\|\s*(sh|bash)\b`)

// IsCommandSafe returns ok=false with a human-readable reason when the
// command matches one of the deny patterns. Keyword matches are
// substring-based after lowercasing. Pipe-to-shell matches tolerate
// extra whitespace around the pipe.
func IsCommandSafe(command string) (ok bool, reason string) {
	lc := strings.ToLower(command)
	for _, kw := range denyKeywords {
		if strings.Contains(lc, kw) {
			return false, "command contains denied pattern: " + strings.TrimSpace(kw)
		}
	}
	if pipeShellRE.MatchString(command) {
		return false, "command pipes downloaded content into a shell"
	}
	return true, ""
}
