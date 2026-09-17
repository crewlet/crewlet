package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// noModelHolds reports which of the handles hold the no-model pause on q.
func noModelHolds(q queue.EventQueue, handles ...string) []string {
	var out []string
	for _, h := range handles {
		holds := q.(*memory.Queue).PauseHolds(topics.AgentInbox(h), topics.AgentInboxGroup(h))
		if slices.Contains(holds, pauseReasonNoTurnEngine) {
			out = append(out, h)
		}
	}
	return out
}

// recorded is the record of paused seats, read under its own lock.
func recorded(e *Engine) []string {
	e.modelHolds.mu.Lock()
	defer e.modelHolds.mu.Unlock()
	var out []string
	for h := range e.modelHolds.seats {
		out = append(out, h)
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
	if got := noModelHolds(q, "ceo", "cto"); !slices.Equal(got, []string{"ceo", "cto"}) {
		t.Fatalf("holds on %v, want both paused seats", got)
	}

	e.releaseModelHolds(t.Context())
	if got := noModelHolds(q, "ceo", "cto"); len(got) != 0 {
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
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
`))
	if err := e.pause(t.Context(), "ceo", "no turn engine"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := noModelHolds(q, "ceo"); len(got) != 0 {
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
