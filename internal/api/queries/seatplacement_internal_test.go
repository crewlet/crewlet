package queries

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// THE TIME NO NODE HELD A SEAT IS READ FROM ITS PLACEMENTS: a release to the
// next tenure is unplaced, and a seat let go and held by no lease now is
// unplaced to the window's end; a tenure begun with none ended, one no lease
// holds now that nobody let go, and a seat whose first tenure read starts
// inside the window leave its placement unknown. Mutation: read a tenure begun over an unended one as
// continuous, and the dead node's gap reads as held.
func TestTheTimeNoNodeHeldASeatIsReadFromItsPlacements(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	to := now.Add(time.Hour)
	from := to.AddDate(0, 0, -7)
	moved, crashed, late, gone, live, dropped := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	at := func(agent uuid.UUID, when time.Time) store.EventRecord {
		return store.EventRecord{ID: uuid.NewString(), AgentID: agent.String(), Time: when}
	}
	backend := coordmemory.New()
	if _, _, err := backend.TryAcquire(t.Context(), coord.SeatResource(live), coord.AcquireOptions{
		Owner: "node-b:1", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	s := Sources{Coord: backend, Events: stopLog{rows: map[string][]store.EventRecord{
		types.SeatPlaced{}.EventType(): {
			at(moved, from.Add(-24*time.Hour)), at(moved, from.Add(26*time.Hour)),
			at(crashed, from.Add(-48*time.Hour)), at(crashed, from.Add(72*time.Hour)),
			at(late, from.Add(time.Hour)),
			at(gone, from.Add(-24*time.Hour)),
			at(live, from.Add(-24*time.Hour)),
			at(dropped, from.Add(-24*time.Hour)),
		},
		types.SeatReleased{}.EventType(): {
			at(moved, from.Add(24*time.Hour)), at(moved, from.Add(6*24*time.Hour)),
			at(gone, from.Add(5*24*time.Hour)),
			at(live, now.Add(-time.Hour)),
		},
	}}}
	unplaced, unknown, err := s.seatPlacement(t.Context(), from, to, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, whole := stoppedIn(unplaced[moved.String()], from, to); got != 2*time.Hour+24*time.Hour || !whole ||
		unknown[moved.String()] {
		t.Errorf("moved: unplaced %s, unknown %v; want the two hours between its nodes and the day since it was let go",
			got, unknown[moved.String()])
	}
	if got, _ := stoppedIn(unplaced[gone.String()], from, to); got != 2*24*time.Hour || unknown[gone.String()] {
		t.Errorf("gone: unplaced %s, unknown %v; want the two days since it was let go", got, unknown[gone.String()])
	}
	if got, _ := stoppedIn(unplaced[live.String()], from, to); got < 59*time.Minute || unknown[live.String()] {
		t.Errorf("live: unplaced %s, unknown %v; want the hour before its lease began", got, unknown[live.String()])
	}
	for name, agent := range map[string]uuid.UUID{"a tenure begun over an unended one": crashed,
		"a first tenure inside the window": late, "a tenure that ended unsaid": dropped} {
		if !unknown[agent.String()] {
			t.Errorf("%s: read as known", name)
		}
	}
}
