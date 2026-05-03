package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

// FakeRunner is a test double for AgentRunner. Configure either canned
// per-phase responses (Responses / Errors) or a custom OnRun function.
// All recorded calls are available via Calls() for assertions.
type FakeRunner struct {
	mu sync.Mutex

	// Responses maps a Phase to the RunResult that should be returned.
	Responses map[types.Phase]types.RunResult
	// Errors maps a Phase to an error that should be returned (takes
	// precedence over Responses).
	Errors map[types.Phase]error
	// OnRun, if non-nil, is invoked instead of consulting Responses/Errors.
	OnRun func(ctx context.Context, req types.RunRequest) (types.RunResult, error)

	calls []types.RunRequest
}

// NewFakeRunner returns a FakeRunner with empty Responses/Errors maps.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{
		Responses: make(map[types.Phase]types.RunResult),
		Errors:    make(map[types.Phase]error),
	}
}

// Run records the request and returns either the configured error, the
// canned response, or the OnRun result. If neither is configured for the
// requested phase, Run returns an error so test failures are loud.
func (f *FakeRunner) Run(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	fn := f.OnRun
	err, hasErr := f.Errors[req.Phase]
	resp, hasResp := f.Responses[req.Phase]
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, req)
	}
	if hasErr && err != nil {
		return types.RunResult{}, err
	}
	if !hasResp {
		return types.RunResult{}, fmt.Errorf("fake runner: no canned response for phase %q", req.Phase)
	}
	if resp.StartedAt.IsZero() {
		resp.StartedAt = time.Now()
	}
	if resp.CompletedAt.IsZero() {
		resp.CompletedAt = resp.StartedAt
	}
	return resp, nil
}

// Calls returns a copy of all RunRequests received, in order.
func (f *FakeRunner) Calls() []types.RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]types.RunRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// Reset clears recorded calls. Responses/Errors/OnRun are left as-is.
func (f *FakeRunner) Reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

// ErrUnconfigured is the sentinel returned when a phase has no canned
// response and no OnRun is set.
var ErrUnconfigured = errors.New("fake runner: phase not configured")
