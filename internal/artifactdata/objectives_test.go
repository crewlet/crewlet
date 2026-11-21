package artifactdata_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/objectives"
)

// judged is the objective meter as the writer sees it, keeping every event.
type judged struct {
	mu     sync.Mutex
	events []judgedEvent
	left   []objectives.Name
}

type judgedEvent struct {
	name     objectives.Name
	answered bool
}

func (j *judged) Record(n objectives.Name, _ time.Duration, answered bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, judgedEvent{n, answered})
}

func (j *judged) Abandon(n objectives.Name, _ time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.left = append(j.left, n)
}

// take answers the events since the last take, and forgets them.
func (j *judged) take() []judgedEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.events
	j.events = nil
	return out
}

// takeLeft answers the events abandoned since the last take, and forgets them.
func (j *judged) takeLeft() []objectives.Name {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.left
	j.left = nil
	return out
}

// EVERY WRITE IS ONE EVENT OF EACH DATASTORE OBJECTIVE, answered whenever the
// caller was given a verdict: the applier's, a refusal the applier recorded,
// or input refused before anything was appended.
func TestEveryWriteIsOneEventOfEachDatastoreObjective(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	n.judged.take()
	answered := []judgedEvent{{objectives.DataRoundTrip, true}, {objectives.DataResolved, true}}
	expect := func(what string, want []judgedEvent) {
		t.Helper()
		got := n.judged.take()
		if len(got) != len(want) {
			t.Fatalf("%s: events %+v, want %+v", what, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: event %d is %+v, want %+v", what, i, got[i], want[i])
			}
		}
	}
	n.applied(person("jane", artifacts.RoleEditor),
		set("review", "row-1", map[string]string{"title": `"Hero copy"`}))
	expect("an applied write", answered)
	n.refused(person("dana", artifacts.RoleViewer), artifacts.DataForbidden,
		set("review", "row-2", map[string]string{"title": `"Not mine to add"`}))
	expect("a write the applier refused", answered)
	_, err := n.writer.Write(t.Context(), person("jane", artifacts.RoleEditor), sheet,
		artifacts.DataWrite{Ops: []artifacts.DataOp{set("_hidden", "a", map[string]string{"x": `1`})}})
	if !errors.Is(err, artifactdata.ErrInvalid) {
		t.Fatalf("a write outside the grammar: %v", err)
	}
	expect("a write refused before the log", answered)
}

// A WRITE ITS CALLER WENT AWAY FROM IS ABANDONED, NOT A MISSING VERDICT: the
// page that sent it closed or its socket dropped, so the cancellation ended the
// wait, not the writer. A write that ran past a deadline failed a bound, and
// is the missing verdict both objectives count.
//
// Mutation: drop objectives.Abandoned from record, and the first write is two
// bad events.
func TestAWriteItsCallerWentAwayFromIsAbandoned(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	n.configure(person("jane", artifacts.RoleOwner), reviewSheet())
	n.judged.take()
	write := artifacts.DataWrite{Ops: []artifacts.DataOp{
		set("review", "row-1", map[string]string{"title": `"Hero copy"`})}}

	gone, leave := context.WithCancel(t.Context())
	leave()
	if _, err := n.writer.Write(gone, person("jane", artifacts.RoleEditor), sheet, write); err == nil {
		t.Fatal("a write whose caller had gone answered a verdict")
	}
	if got, left := n.judged.take(), n.judged.takeLeft(); len(got) != 0 ||
		!slices.Equal(left, []objectives.Name{objectives.DataRoundTrip, objectives.DataResolved}) {
		t.Errorf("events %+v, abandoned %v: want both objectives abandoned and nothing counted", got, left)
	}

	late, expire := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer expire()
	if _, err := n.writer.Write(late, person("jane", artifacts.RoleEditor), sheet, write); err == nil {
		t.Fatal("a write past its deadline answered a verdict")
	}
	want := []judgedEvent{{objectives.DataRoundTrip, false}, {objectives.DataResolved, false}}
	if got, left := n.judged.take(), n.judged.takeLeft(); !slices.Equal(got, want) || len(left) != 0 {
		t.Errorf("events %+v, abandoned %v: want %+v, a deadline being no caller leaving", got, left, want)
	}
}
