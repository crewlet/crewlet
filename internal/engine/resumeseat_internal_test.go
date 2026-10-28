package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// seatRecorder refuses every call, as [refusingProvider] does, and remembers
// the seat each one was bound to.
type seatRecorder struct {
	mu    sync.Mutex
	seats []string
}

func (*seatRecorder) Model() string { return "seat-recorder" }
func (r *seatRecorder) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	r.mu.Lock()
	r.seats = append(r.seats, llm.SeatOf(ctx))
	r.mu.Unlock()
	return nil, &llm.Error{Kind: llm.KindFatal, Provider: "test", Model: "seat-recorder"}
}

func (r *seatRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seats...)
}

// A RESUMED TURN'S MODEL CALLS RUN AS ITS SEAT, as a dispatched turn's do. A
// coding CLI keeps a home per seat — its transcripts, its sign-in — and a call
// bound to no seat runs in the one home every unbound call shares, so a
// resumed turn's reviewer ran beside other seats' transcripts and on whichever
// login that home last held. The dispatch path binds the seat; the resume is
// the same turn and binds it too.
//
// Mutation: continue the run's trace without binding its seat, and every call
// the resumed turn makes is the shared home's.
func TestAResumedTurnsModelCallsRunAsItsSeat(t *testing.T) {
	t.Parallel()
	recorder := &seatRecorder{}
	// THE SEAT'S PHASES ON THE RECORDER, its executor on the runner entry
	// [starting]'s seat names ([runnable]).
	e, _ := starting(t, runnable(t, []phase.Entry{{Key: "only", Provider: recorder}}))
	company := e.Company()
	// The model refuses, so the resume ends in that refusal; what is under
	// test is the seat its call was made as.
	_ = e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test",
		},
		State: execstate.State{Ask: "the login test fails on main"},
		Turn: &turnctx.Turn{RunID: "run-1", Seat: company.Org.AgentSeatByHandle("swe"),
			Org: company.Org},
		Answer: "the box fixed it",
	})
	seats := recorder.seen()
	if len(seats) == 0 {
		t.Fatal("the resumed turn made no model call, so the seat it runs as was not observed")
	}
	for _, seat := range seats {
		if seat != "swe" {
			t.Errorf("a resumed turn's model call ran as %q, want its seat swe", seat)
		}
	}
}
