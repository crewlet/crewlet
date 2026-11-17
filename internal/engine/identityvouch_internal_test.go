package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE THAT STOPS VOUCHING FOR ITS IDENTITY ROWS NAMES EVERYONE, once, at the
// instant its request path stops serving them, or each open socket is served on
// its last decision for as long as the node runs ([vouching]).
//
// At exactly the grace the guard still serves, so nothing is said; one second
// past it everyone is; staying past it says nothing more; catching up and
// stalling again says it again. Nothing rebuilds contact routing.
//
// Mutations: fire on every stalled reading and the third reading is heard
// twice; compare at the grace rather than past it and the first reading is
// heard; drop the re-arm and the second stall is never said.
func TestANodeThatStopsVouchingNamesEveryoneOnce(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	running := &runningLog{}
	watch := identityVouching(e, running)

	t0 := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	running.progress.observe(t0, coord.DomainPosition{Seq: 41}, true)

	expect := func(at time.Time, want int, why string) {
		t.Helper()
		watch.observe(at)
		got := heard.take()
		if len(got) != want {
			t.Fatalf("%s: the listener heard %+v, want %d moves", why, got, want)
		}
		for _, m := range got {
			if !m.Everyone || m.Seats {
				t.Fatalf("%s: the listener heard %+v, want everyone and no seat", why, m)
			}
		}
	}
	expect(t0.Add(statelog.StallGrace), 0,
		"a node at exactly the grace, which the guard still serves")
	expect(t0.Add(statelog.StallGrace+time.Second), 1,
		"the reading that crossed the grace")
	expect(t0.Add(2*statelog.StallGrace), 0,
		"a node that stayed past the grace")

	t1 := t0.Add(3 * statelog.StallGrace)
	running.progress.observe(t1, coord.DomainPosition{Seq: 42}, false)
	expect(t1, 0, "a node that caught up")
	running.progress.observe(t1.Add(time.Second), coord.DomainPosition{Seq: 42}, true)
	expect(t1.Add(statelog.StallGrace+2*time.Second), 1,
		"the second stall's crossing")

	select {
	case <-e.directoryNudge:
		t.Error("a stall rebuilt the party registry, which is rebuilt from rows " +
			"a stall never wrote")
	default:
	}
}
