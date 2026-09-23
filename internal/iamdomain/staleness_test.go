package iamdomain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// STALENESS STATES BOTH FACTS, and Resolve reads the same two.
//
// A Tier A token's seat binding is a directory row read on every request, and
// the engine refuses to honour one a node cannot vouch for: past the stall
// grace, or holding a newer peer's record about that person's bucket. Asked
// about the lag alone, a caught-up node holding exactly that record reads as
// current; asked about any deferral at all, one record about somebody else
// would refuse every bound credential on the node for the length of a rolling
// upgrade.
//
// This case holds the LAG half and the control of the deferral half — a node
// that retained nothing covers nobody, the empty person included. The deferral
// half itself needs a record the node really retained, which only a real
// applier produces: internal/engine's
// TestARetainedRecordCoversExactlyItsPersonsBucket boots one and retains two.
func TestStalenessIsTheLagAndNoDeferralWhereNothingIsRetained(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	log, err := statelogtest.LocalReaderOver(
		iamdomain.Domain{}, rig.db.Replicated(), rig.waiter)
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB: rig.db, Log: log,
		Lag: func() time.Duration { return 90 * time.Second },
	})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	someone := uuid.New().String()
	for _, person := range []string{someone, ""} {
		lag, deferred, err := reader.Staleness(t.Context(), person)
		if err != nil {
			t.Fatalf("Staleness(%q): %v", person, err)
		}
		if lag != 90*time.Second {
			t.Errorf("Staleness(%q): lag %s, want the applier's 90s", person, lag)
		}
		if deferred {
			t.Errorf("Staleness(%q) reports a deferral on a node that retained "+
				"nothing", person)
		}
	}
	// AND THE SESSION TABLE READS THE SAME TWO FACTS, so a token and a
	// cookie can never be told different things about one person.
	identity, err := reader.Resolve(t.Context(), "no-such-lineage", someone)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if identity.Lag != 90*time.Second || identity.Deferred {
		t.Errorf("Resolve answered lag %s deferred %v, want 90s and false",
			identity.Lag, identity.Deferred)
	}
}
