package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/chenlong-seu/symphony-go/internal/types"
)

func TestFakeRunner_CannedResponse(t *testing.T) {
	t.Parallel()
	f := NewFakeRunner()
	f.Responses[types.PhasePlanning] = types.RunResult{Success: true, Text: "planned"}

	got, err := f.Run(context.Background(), types.RunRequest{Phase: types.PhasePlanning})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Success || got.Text != "planned" {
		t.Errorf("got %+v", got)
	}
	if got.StartedAt.IsZero() || got.CompletedAt.IsZero() {
		t.Errorf("expected timestamps to be filled in: %+v", got)
	}
	if calls := f.Calls(); len(calls) != 1 || calls[0].Phase != types.PhasePlanning {
		t.Errorf("calls=%+v", calls)
	}
}

func TestFakeRunner_ErrorTakesPrecedence(t *testing.T) {
	t.Parallel()
	f := NewFakeRunner()
	want := errors.New("boom")
	f.Errors[types.PhaseImplementation] = want
	f.Responses[types.PhaseImplementation] = types.RunResult{Success: true}

	_, err := f.Run(context.Background(), types.RunRequest{Phase: types.PhaseImplementation})
	if !errors.Is(err, want) {
		t.Errorf("err=%v want %v", err, want)
	}
}

func TestFakeRunner_OnRunOverrides(t *testing.T) {
	t.Parallel()
	f := NewFakeRunner()
	f.Responses[types.PhasePlanning] = types.RunResult{Text: "canned"}
	f.OnRun = func(ctx context.Context, req types.RunRequest) (types.RunResult, error) {
		return types.RunResult{Text: "from-fn-" + string(req.Phase)}, nil
	}

	got, err := f.Run(context.Background(), types.RunRequest{Phase: types.PhasePlanning})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "from-fn-planning" {
		t.Errorf("got %q", got.Text)
	}
}

func TestFakeRunner_UnconfiguredPhaseFails(t *testing.T) {
	t.Parallel()
	f := NewFakeRunner()
	_, err := f.Run(context.Background(), types.RunRequest{Phase: types.PhaseReview})
	if err == nil {
		t.Fatal("expected error for unconfigured phase")
	}
}

func TestFakeRunner_ResetClearsCalls(t *testing.T) {
	t.Parallel()
	f := NewFakeRunner()
	f.Responses[types.PhasePlanning] = types.RunResult{Success: true}
	_, _ = f.Run(context.Background(), types.RunRequest{Phase: types.PhasePlanning})
	f.Reset()
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("expected 0 calls after Reset, got %d", len(calls))
	}
}
