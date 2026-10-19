package artifactdata_test

import (
	"errors"
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

// take answers the events since the last take, and forgets them.
func (j *judged) take() []judgedEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.events
	j.events = nil
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
