package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A JOIN PINS THE TAIL IN THE ARTEFACT'S GENERATION, NEVER ITS OWN.
//
// A sequence names a number space, and the pin is only a pin when it is
// stated in the log's. The artefact's generation is the log's — the join
// refuses an offer at any other — while the joiner's own checkpoint is the one
// position a join knows to be wrong: a node that never ran has none, which
// read as generation 0, and a node a reanchor passed holds the old one. Stated
// in either, the hold sat below the log's generation, and the trim reads such
// a hold as an UNKNOWN term — it blocked every trim of the log for a minute
// and then, gone stale, pinned nothing at all.
//
// The joiner here has no estate open, as a node that never ran has none worth
// reading: the pin must not need one.
func TestAJoinPinsTheTailInTheArtefactsGeneration(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	s := &stateLog{nodeID: "node-join", fleet: fleet}
	at := map[string]statelog.DomainPosition{}
	for _, domain := range registeredDomains() {
		at[domain.Name()] = statelog.DomainPosition{
			Stream: domain.Stream().Name, Generation: 3, Seq: 2,
		}
	}
	release, err := s.holdTail(t.Context(), at)
	if err != nil {
		t.Fatalf("holdTail: %v", err)
	}
	defer release()

	holds, err := fleet.Holds(t.Context())
	if err != nil {
		t.Fatalf("read the holds: %v", err)
	}
	for _, domain := range registeredDomains() {
		in := statelog.TrimInputs{
			Generation: 3, Now: time.Now().UTC(),
			HoldsReadable: true, HoldStale: statelog.TrimHoldStale,
			Holds: holdsFor(holds, domain.Stream().Name),
		}
		var held statelog.Term
		for _, term := range in.Terms() {
			if term.Name == statelog.TermMinHold {
				held = term
			}
		}
		if !held.Known || held.Seq != 2 {
			t.Fatalf("on %s, a log at generation 3 reads the join's pin as %+v, "+
				"want it known at 2 — a pin in another number space blocks the "+
				"trim while it is fresh and pins nothing once it is stale",
				domain.Name(), held)
		}
	}
}
