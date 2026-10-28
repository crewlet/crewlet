package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
)

// A PROVIDER-PARKED SEAT WITH RESUMES WAITING ON IT STAYS QUIET, and resumes
// each of them once the park releases.
//
// Two answers the seat owes are both refused at the turn gate while every model
// of its chain is at its limit. A refusal there comes after the node admitted
// the turn, so the admission is released at once, and a release that woke the
// seat's other waiting resumes could set them waking each other for as long as
// the limit lasts. Each refused resume waits on the provider from then on, and
// the node announces a release only to a turn refused behind it
// (node.Config.SeatTurnEnded), so the gate is asked once per resume while the
// park holds, and once more each when it releases.
func TestAProviderParkedSeatWithWaitingResumesStaysQuiet(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	var asked atomic.Int32
	r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
		return func(ctx context.Context, handle string) (context.Context, func(), error) {
			asked.Add(1)
			return next(ctx, handle)
		}
	}
	e := r.start(t)
	entry := &spentEntry{retry: time.Now().Add(time.Hour)}
	entry.limited.Store(true)
	withModels(t, e, entry)
	stopWaiters(e)

	// TWO ANSWERS THE SEAT OWES, taken over as its holder recovers it.
	rt := e.sandbox.Load()
	answeredRun(t, e.answers, rt.pending, "a1")
	answeredRun(t, e.answers, rt.pending, "a2")
	host := e.node.Host()
	if !host.Release(t.Context(), "swe", seat.ReasonPlacement) {
		t.Fatal("the seat was not released")
	}
	host.Sweep(t.Context())
	waitHeld(t, e, "swe")
	eventually(t, "both answers to be refused at the gate", func() bool { return asked.Load() >= 2 })

	time.Sleep(300 * time.Millisecond)
	settled := asked.Load()
	time.Sleep(2 * time.Second)
	if more := asked.Load() - settled; more > 2 {
		t.Fatalf("the gate was asked %d more times in 2s while the park held: the seat's "+
			"waiting resumes woke each other", more)
	}

	// THE PARK RELEASES: both are tried again, and claimed.
	entry.limited.Store(false)
	e.readmitResumes(waitProvider)
	eventually(t, "both answers to be claimed for their resumes", func() bool {
		for _, id := range []string{"a1", "a2"} {
			run, found, err := rt.pending.Get(t.Context(), id)
			if err != nil || (found && run.Status == sandbox.StatusAnswered && run.UpdatedAt.Equal(run.Answer.RecordedAt)) {
				return false
			}
		}
		return true
	})
}
