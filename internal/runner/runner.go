// Package runner defines the AgentRunner interface and provides
// implementations for Claude Code (claude.go) and OpenAI Codex (codex.go),
// plus a FakeRunner for tests.
package runner

import (
	"context"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

// AgentRunner runs an agent subprocess for one phase of work and returns
// its captured output. Implementations are responsible for:
//   - constructing the subprocess argv and stdin
//   - sandbox/permission flags appropriate to the phase
//   - SIGTERM-on-cancel and a bounded grace period before SIGKILL
//   - capturing and redacting stdout/stderr/events
//   - never inheriting GITHUB_TOKEN, GH_TOKEN, or SSH_AUTH_SOCK
//
// The subprocess receives prompt input on stdin only; it must never appear
// as a shell argument.
type AgentRunner interface {
	Run(ctx context.Context, req types.RunRequest) (types.RunResult, error)
}
