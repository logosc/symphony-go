package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/logosc/symphony-go/internal/config"
	"github.com/logosc/symphony-go/internal/runner"
	"github.com/logosc/symphony-go/internal/types"
)

// fakeSession is a hand-rolled runner.Session that yields a canned
// sequence of turn results.
type fakeSession struct {
	mu        sync.Mutex
	turns     []types.RunResult
	turnErrs  []error
	turnIdx   int
	prompts   []string
	closed    bool
	openErr   error
	closeWith error
}

func (f *fakeSession) Turn(ctx context.Context, prompt string) (types.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	if f.turnIdx >= len(f.turns) {
		return types.RunResult{}, errors.New("fakeSession: ran out of canned turns")
	}
	res := f.turns[f.turnIdx]
	var err error
	if f.turnIdx < len(f.turnErrs) {
		err = f.turnErrs[f.turnIdx]
	}
	f.turnIdx++
	return res, err
}

func (f *fakeSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.closeWith
}

// fakeMultiTurnRunner implements runner.MultiTurnRunner over a fakeSession.
type fakeMultiTurnRunner struct {
	sess        *fakeSession
	sessionErr  error
	openCount   int
	openCountMu sync.Mutex
}

func (f *fakeMultiTurnRunner) Run(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
	return types.RunResult{}, errors.New("not used")
}

func (f *fakeMultiTurnRunner) OpenSession(ctx context.Context, req types.RunRequest) (runner.Session, error) {
	f.openCountMu.Lock()
	f.openCount++
	f.openCountMu.Unlock()
	if f.sessionErr != nil {
		return nil, f.sessionErr
	}
	return f.sess, nil
}

// newOrchForMT builds a minimal Orchestrator that has just enough wiring
// for runMultiTurnImpl / runImplementationAgent — not the full state
// machine.
func newOrchForMT(t *testing.T, mt runner.AgentRunner, multiTurn bool, maxTurns int) *Orchestrator {
	t.Helper()
	cfg := &config.Config{
		Agent: config.AgentConfig{
			Provider:  "codex",
			MultiTurn: multiTurn,
			MaxTurns:  maxTurns,
		},
	}
	o := &Orchestrator{
		deps: Deps{
			Config:      cfg,
			AgentRunner: mt,
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	return o
}

func TestMultiTurn_StopsOnDoneMarker(t *testing.T) {
	t.Parallel()
	sess := &fakeSession{
		turns: []types.RunResult{
			{Success: true, Text: "step 1 done, more to do"},
			{Success: true, Text: "all good\n## Done\n"},
			{Success: true, Text: "should not be called"},
		},
	}
	mt := &fakeMultiTurnRunner{sess: sess}
	o := newOrchForMT(t, mt, true, 5)

	res, turns, err := o.runImplementationAgent(context.Background(), o.deps.Logger, types.RunRequest{
		Phase:  types.PhaseImplementation,
		Prompt: "INITIAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if turns != 2 {
		t.Errorf("turns = %d, want 2", turns)
	}
	if !res.Success {
		t.Error("expected Success on done")
	}
	if !sess.closed {
		t.Error("session not closed")
	}
	// First prompt is the initial one; subsequent are the continuation.
	if len(sess.prompts) != 2 {
		t.Fatalf("prompts = %v", sess.prompts)
	}
	if sess.prompts[0] != "INITIAL" {
		t.Errorf("first prompt = %q, want INITIAL", sess.prompts[0])
	}
	if sess.prompts[1] != multiTurnContinuePrompt {
		t.Errorf("second prompt = %q, want continuation", sess.prompts[1])
	}
}

func TestMultiTurn_HitsMaxTurns(t *testing.T) {
	t.Parallel()
	// Agent never says done; should run exactly max_turns times.
	turns := []types.RunResult{
		{Success: true, Text: "a"},
		{Success: true, Text: "b"},
		{Success: true, Text: "c"},
	}
	sess := &fakeSession{turns: turns}
	mt := &fakeMultiTurnRunner{sess: sess}
	o := newOrchForMT(t, mt, true, 3)

	res, n, err := o.runImplementationAgent(context.Background(), o.deps.Logger, types.RunRequest{
		Phase:  types.PhaseImplementation,
		Prompt: "INITIAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 3 {
		t.Errorf("turns = %d, want 3", n)
	}
	if !res.Success {
		t.Error("last turn was Success=true")
	}
}

func TestMultiTurn_FirstFailureStops(t *testing.T) {
	t.Parallel()
	sess := &fakeSession{
		turns: []types.RunResult{
			{Success: true, Text: "ok"},
			{Success: false, Text: "boom"},
			{Success: true, Text: "should not run"},
		},
	}
	mt := &fakeMultiTurnRunner{sess: sess}
	o := newOrchForMT(t, mt, true, 5)

	res, n, err := o.runImplementationAgent(context.Background(), o.deps.Logger, types.RunRequest{
		Phase:  types.PhaseImplementation,
		Prompt: "INITIAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 2 {
		t.Errorf("turns = %d, want 2", n)
	}
	if res.Success {
		t.Error("expected Success=false on second-turn failure")
	}
	if res.Text != "boom" {
		t.Errorf("Text = %q, want last-turn's text", res.Text)
	}
}

// ordinaryRunner is a runner.AgentRunner that does NOT implement
// runner.MultiTurnRunner — used to confirm the orchestrator falls back
// to single-Run.
type ordinaryRunner struct {
	calls int
	res   types.RunResult
}

func (o *ordinaryRunner) Run(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
	o.calls++
	return o.res, nil
}

func TestMultiTurn_FallsBackWhenRunnerLacksSupport(t *testing.T) {
	t.Parallel()
	or := &ordinaryRunner{res: types.RunResult{Success: true, Text: "single"}}
	o := newOrchForMT(t, or, true, 3)

	res, n, err := o.runImplementationAgent(context.Background(), o.deps.Logger, types.RunRequest{
		Phase:  types.PhaseImplementation,
		Prompt: "INITIAL",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 1 {
		t.Errorf("turns = %d, want 1 (fallback)", n)
	}
	if or.calls != 1 {
		t.Errorf("ordinary Run calls = %d, want 1", or.calls)
	}
	if res.Text != "single" {
		t.Errorf("res.Text = %q", res.Text)
	}
}

func TestMultiTurn_DisabledByConfigRunsOnce(t *testing.T) {
	t.Parallel()
	sess := &fakeSession{
		turns: []types.RunResult{{Success: true, Text: "should not run as session"}},
	}
	mt := &fakeMultiTurnRunner{sess: sess}
	o := newOrchForMT(t, mt, false /* MultiTurn off */, 5)

	_, n, err := o.runImplementationAgent(context.Background(), o.deps.Logger, types.RunRequest{
		Phase:  types.PhaseImplementation,
		Prompt: "INITIAL",
	})
	if err == nil {
		// fakeMultiTurnRunner.Run returns "not used" error; that's fine
		// for this assertion — single-Run path was taken.
		t.Logf("note: ordinary Run path returned no error")
	}
	if n != 1 {
		t.Errorf("turns = %d, want 1", n)
	}
	if mt.openCount != 0 {
		t.Errorf("OpenSession was called despite multi_turn=false")
	}
}

func TestHasDoneMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    string
		want bool
	}{
		{"## Done", true},
		{"foo\n## Done\nbar", true},
		{"foo\n  ## DONE  \n", true},
		{"## done", true},
		{"prefix ## Done", false},
		{"## Done suffix", false},
		{"completely unrelated", false},
	}
	for _, tc := range cases {
		got := hasDoneMarker(tc.s)
		if got != tc.want {
			t.Errorf("hasDoneMarker(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
