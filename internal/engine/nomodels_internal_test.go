package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// noModelHolds reports which of the seats hold the no-model pause on q.
//
// THROUGH THE ENGINE'S OWN COMPANY, because a seat's inbox is named by its id
// and the pause is taken on that subject: a topic a case built from the handle
// would be one nothing ever paused, and every assertion over it would pass by
// finding no holds.
func noModelHolds(e *Engine, q queue.EventQueue, handles ...string) []string {
	var out []string
	for _, h := range handles {
		id, err := e.seatID(h)
		if err != nil {
			continue
		}
		holds := q.(*memory.Queue).PauseHolds(topics.AgentInbox(id), topics.AgentInboxGroup(id))
		if slices.Contains(holds, pauseReasonNoTurnEngine) {
			out = append(out, h)
		}
	}
	return out
}

// recorded is the record of paused seats, read under its own lock, by the
// HANDLE each entry carries as its label.
func recorded(e *Engine) []string {
	e.modelHolds.mu.Lock()
	defer e.modelHolds.mu.Unlock()
	var out []string
	for _, hold := range e.modelHolds.seats {
		out = append(out, hold.handle)
	}
	slices.Sort(out)
	return out
}

// THE RELEASE LIFTS EVERY HOLD THE PAUSE TOOK. The pause used to be one-way:
// nothing called ResumeTopic, so a seat parked for want of a model stayed deaf
// until the process restarted, provider or no provider.
func TestTheReleaseLiftsEveryNoModelHold(t *testing.T) {
	t.Parallel()
	e, q := engineOn(t)
	for _, h := range []string{"ceo", "cto"} {
		if err := e.pause(t.Context(), h, "no turn engine"); err != nil {
			t.Fatalf("pause %s: %v", h, err)
		}
	}
	if got := noModelHolds(e, q, "ceo", "cto"); !slices.Equal(got, []string{"ceo", "cto"}) {
		t.Fatalf("holds on %v, want both paused seats", got)
	}

	e.releaseModelHolds(t.Context())
	if got := noModelHolds(e, q, "ceo", "cto"); len(got) != 0 {
		t.Errorf("the release left %v paused", got)
	}
	if got := recorded(e); len(got) != 0 {
		t.Errorf("the record still names %v after a release lifted them", got)
	}
}

// A PAUSE THAT LOST THE RACE TO THE PROVIDER LIFTS ITSELF. The screening read
// an epoch with no model; by the time the hold is taken an apply may already
// have installed one and run its release, which found nothing to lift. The
// pause re-reads the epoch after recording its hold, so the seat is not left
// waiting for an apply that has already happened.
func TestAPauseTakenOnceAModelIsCurrentLiftsItself(t *testing.T) {
	t.Parallel()
	e, q := engineOn(t)
	e.epoch.current.Store(companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
`))
	if err := e.pause(t.Context(), "ceo", "no turn engine"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := noModelHolds(e, q, "ceo"); len(got) != 0 {
		t.Error("a seat paused after its company gained a model stays paused " +
			"with no release left to lift it")
	}
}

// A RESUME THAT FAILED IS KEPT ON THE RECORD. Forgetting it would be the one
// way to make the hold permanent: nothing but the record ever releases it.
func TestAResumeThatFailedKeepsTheSeatOnTheRecord(t *testing.T) {
	t.Parallel()
	e, q := engineOn(t)
	if err := e.pause(t.Context(), "ceo", "no turn engine"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	e.releaseModelHolds(t.Context())
	if got := recorded(e); !slices.Equal(got, []string{"ceo"}) {
		t.Errorf("record = %v after a resume the queue refused, want ceo kept", got)
	}
}

// A NO-MODEL PAUSE TAKEN WHILE THE RELEASE IS LIFTING KEEPS THE SEAT HELD.
//
// The release drops the record under its lock and resumes outside it. A
// delivery screened against a revision with no model, installed after the one
// the release is for, pauses the seat in between: the same attachment held
// again, which changes nothing on the queue, and recorded. The late resume then
// lifted that pause's hold, and the seat was recorded paused with its inbox not
// held. So each lift reads the record again once its resume has returned, and
// takes back the hold of a pause recorded meanwhile.
//
// Mutation: drop the read after the resume, and the seat is left recorded
// paused with its inbox unheld.
func TestANoModelPauseTakenAsTheReleaseLiftsKeepsTheSeatHeld(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, q := engineOn(t)
	noModel := e.Company()
	if err := e.pause(ctx, "ceo", "no turn engine"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	e.epoch.current.Store(companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
  - name: CTO
    handle: cto
`))

	staged := &foldAtResume{EventQueue: q, subject: topics.AgentInbox(seatOf("ceo")),
		hold: pauseReasonNoTurnEngine, fold: func() {
			// THE PROVIDER REMOVED AGAIN, and a delivery screened against
			// that revision pauses the seat in the release's window.
			e.epoch.current.Store(noModel)
			if err := e.pause(ctx, "ceo", "no turn engine"); err != nil {
				t.Errorf("the pause in the release's window: %v", err)
			}
		}}
	e.backends.Queue = staged
	e.releaseModelHolds(ctx)

	if !staged.fired.Load() {
		t.Fatal("the release lifted no no-model hold, so nothing was staged in its window")
	}
	if got := recorded(e); !slices.Equal(got, []string{"ceo"}) {
		t.Fatalf("record = %v, want the pause taken in the release's window", got)
	}
	if got := noModelHolds(e, q, "ceo"); !slices.Equal(got, []string{"ceo"}) {
		t.Errorf("the seat is recorded paused with its inbox unheld: the release's late "+
			"resume took the hold the pause in its window owns (held: %v)", got)
	}
}
