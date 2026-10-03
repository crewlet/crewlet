package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE THAT STOPS VOUCHING FOR ITS IDENTITY ROWS DECIDES EVERY CREDENTIAL
// AGAIN, once, at the instant its request path stops serving them.
//
// An applier halted on a record it cannot read — the removal or the
// company-wide invalidation meant to end an open socket's credential, signed
// under a key this node lacks — commits no batch, so no move is ever said
// about it; and an applier frozen behind an unreachable broker says nothing
// either. What changes is the identity log's lag: its checkpoint stops moving
// while it owes records, and past the stall grace every identity read on the
// node answers unknown, so REST answers 503. The open socket has to be told at
// that same instant, or it is served on its last decision for as long as the
// node runs.
//
// The domain here is a running domain whose checkpoint was last seen moving at
// t0 with records owed — the figure the identity reader is built with — and
// the listener is the one the API registers. At exactly the grace the guard
// still serves (it compares PAST the grace), so nothing is said; one second
// past it everyone is; staying past it says nothing more; a node that catches
// up and stalls again says it again. Nothing rebuilds contact routing, which
// is rebuilt from rows a stall did not write.
//
// Mutations: fire on every stalled reading and the third reading is heard
// twice; compare at the grace rather than past it and the first reading is
// heard; drop the re-arm and the second stall is never said.
func TestANodeThatStopsVouchingDecidesEveryCredentialAgain(t *testing.T) {
	t.Parallel()
	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	heard := &heardMoves{}
	e.SetOnIdentityMoved(heard.hear)
	running := &runningDomain{}
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
