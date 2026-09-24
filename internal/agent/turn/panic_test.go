package turn_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
)

// panicking is a scripted Phases whose chosen phase panics. It wraps the
// ordinary fake so every other phase behaves exactly as the rest of this
// package's tests expect.
type panicking struct {
	*fake
	in    string // "execute", "review" or "resume"
	value any
}

func (p *panicking) Execute(ctx context.Context, round int, notes string, h []ledger.Iteration) (turn.Work, turn.Surface, error) {
	if p.in == "execute" {
		panic(p.value)
	}
	return p.fake.Execute(ctx, round, notes, h)
}

func (p *panicking) Review(ctx context.Context, round int, w turn.Work, h []ledger.Iteration) (turn.Review, error) {
	if p.in == "review" {
		panic(p.value)
	}
	return p.fake.Review(ctx, round, w, h)
}

func (p *panicking) Resume(ctx context.Context, round int, h []ledger.Iteration) (turn.Work, turn.Surface, error) {
	if p.in == "resume" {
		panic(p.value)
	}
	return p.fake.Resume(ctx, round, h)
}

// wantUnhandled asserts the shape every recovered panic must leave: an error
// the caller can recognise, a failed decision, and the guard that names it.
func wantUnhandled(t *testing.T, res turn.Result, err error, wantInDetail string) *turn.PanicError {
	t.Helper()
	var panicked *turn.PanicError
	if !errors.As(err, &panicked) {
		t.Fatalf("err = %v, want a *turn.PanicError: without it the caller cannot "+
			"tell a defect from a provider that did not answer", err)
	}
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != types.GuardUnhandledException {
		t.Fatalf("breach = %+v, want %s: the seat never goes AFK without it",
			res.Breach, types.GuardUnhandledException)
	}
	if !strings.Contains(res.Breach.Detail, wantInDetail) {
		t.Errorf("breach detail = %q, want it to name %q", res.Breach.Detail, wantInDetail)
	}
	if strings.Contains(res.Breach.Detail, "goroutine") {
		t.Errorf("breach detail carries a stack (%q): it is published, and a "+
			"stack belongs in the log", res.Breach.Detail)
	}
	if !strings.Contains(panicked.Stack, "goroutine") {
		t.Errorf("no stack was captured, so the log line cannot locate the defect")
	}
	return panicked
}

// A PANIC IN A PHASE ENDS THE TURN, it does not unwind past the loop.
//
// It used to unwind into the queue backend's handler guard, which NAKs a
// panic: the trigger came back up to the whole delivery budget, and the seat
// stayed `working` because nothing published the turn's end.
func TestAPanickingPhaseEndsTheTurnAsAnUnhandledException(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"execute", "review", "resume"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			ph := &panicking{
				fake: &fake{
					works:    []turn.Work{delivered("posted")},
					surfaces: []turn.Surface{slackSurface()},
					reviews:  []turn.Review{{Decision: phase.Done}},
				},
				in: in, value: "assignment to entry in nil map",
			}
			res, err := turn.Run(context.Background(), ph, settings(), turn.Input{
				RunID: "t1", Reply: turn.ToolReply(""), Resume: in == "resume",
			})
			wantUnhandled(t, res, err, "assignment to entry in nil map")
			if reason, abandon := turn.Abandon(res, err); !abandon || reason != turn.AbandonedPanicked {
				t.Errorf("Abandon = (%q, %v), want the panic reason: a panic "+
					"redelivered runs the same defect again", reason, abandon)
			}
		})
	}
}

// A panic whose value is an error still wraps as a PanicError, not as that
// error. Otherwise a panic(context.Canceled) would read to every caller as an
// ordinary cancellation and be retried.
func TestAPanicWithAnErrorValueIsStillAPanic(t *testing.T) {
	t.Parallel()
	ph := &panicking{fake: &fake{}, in: "execute", value: context.Canceled}
	res, err := turn.Run(context.Background(), ph, settings(), turn.Input{RunID: "t1", Reply: turn.NoReply()})
	panicked := wantUnhandled(t, res, err, "context canceled")
	if value, ok := panicked.Value.(error); !ok || !errors.Is(value, context.Canceled) {
		t.Errorf("value = %v, want the value that was panicked with", panicked.Value)
	}
}

// WHAT EARLIER ROUNDS PROVED SURVIVES THE PANIC. Round one posted and was sent
// back; round two's executor panicked. The result must still say the turn
// acted, because it did, and the rounds that closed are still the ledger.
func TestAPanicKeepsWhatEarlierRoundsRecorded(t *testing.T) {
	t.Parallel()
	ph := &roundPanic{fake: &fake{
		works:    []turn.Work{delivered("posted")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}},
	}}
	res, err := turn.Run(context.Background(), ph, settings(),
		turn.Input{RunID: "t1", Reply: turn.ToolReply("")})
	wantUnhandled(t, res, err, "round two broke")
	if !res.Acted {
		t.Error("the round-one post was forgotten by the panic in round two")
	}
	if res.Rounds != 2 || len(res.Iterations) != 1 {
		t.Errorf("rounds = %d, iterations = %d, want 2 rounds and round one closed",
			res.Rounds, len(res.Iterations))
	}
}

// roundPanic panics in its executor's second round only.
type roundPanic struct{ *fake }

func (p *roundPanic) Execute(ctx context.Context, round int, notes string, h []ledger.Iteration) (turn.Work, turn.Surface, error) {
	if round == 2 {
		panic("round two broke")
	}
	return p.fake.Execute(ctx, round, notes, h)
}

func TestAbandonIsOneRuleForBothPaths(t *testing.T) {
	t.Parallel()
	provider := errors.New("provider did not answer")
	panicked := fmt.Errorf("turn: execute round 1: %w", turn.Recovered("boom"))
	cases := []struct {
		name       string
		res        turn.Result
		err        error
		wantAbort  bool
		wantReason string
	}{
		{"a turn that did not break", turn.Result{Acted: true}, nil, false, ""},
		{"a broken turn that proved nothing", turn.Result{}, provider, false, ""},
		{"a broken turn that wrote outside", turn.Result{Acted: true}, provider, true, turn.AbandonedActed},
		{"a panic that proved nothing", turn.Result{}, panicked, true, turn.AbandonedPanicked},
		{"a panic after a write names the panic", turn.Result{Acted: true}, panicked, true, turn.AbandonedPanicked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reason, abandon := turn.Abandon(tc.res, tc.err)
			if abandon != tc.wantAbort || reason != tc.wantReason {
				t.Errorf("Abandon = (%q, %v), want (%q, %v)",
					reason, abandon, tc.wantReason, tc.wantAbort)
			}
		})
	}
}

func TestRecoveredIsNilWithoutAPanic(t *testing.T) {
	t.Parallel()
	if got := turn.Recovered(nil); got != nil {
		t.Errorf("Recovered(nil) = %v, want nil so a deferred check needs no second test", got)
	}
}
