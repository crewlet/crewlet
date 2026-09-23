package iamdomain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// A VOUCH STATES BOTH FACTS, and Resolve reads the same two.
//
// A Tier A token's seat binding and the record a login keeps its holder's work
// under are directory rows read on every request, and the engine refuses to
// honour one a node cannot vouch for: past the stall grace, or holding a newer
// peer's record about that person's bucket. Asked about the lag alone, a
// caught-up node holding exactly that record reads as current; asked about any
// deferral at all, one record about somebody else would refuse every bound
// credential on the node for the length of a rolling upgrade.
//
// This case holds the LAG half and the control of the deferral half — a node
// that retained nothing covers nobody, a login nobody holds included. The
// deferral half itself needs a record the node really retained, which only a
// real applier produces: internal/engine's
// TestARetainedRecordCoversExactlyItsPersonsBucket boots one and retains two.
func TestAVouchIsTheLagAndNoDeferralWhereNothingIsRetained(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	someone := uuid.New().String()
	enrolSarah(t, rig, someone)
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
	for _, tc := range []struct{ login, holder string }{
		{"sarah.chen", someone},
		{"nobody.at.all", ""},
	} {
		seen, vouch, err := reader.PersonByLoginVouched(t.Context(), tc.login)
		if err != nil {
			t.Fatalf("PersonByLoginVouched(%s): %v", tc.login, err)
		}
		if seen.ID != tc.holder {
			t.Errorf("%s resolved to %q, want %q", tc.login, seen.ID, tc.holder)
		}
		if vouch.Lag != 90*time.Second {
			t.Errorf("%s: lag %s, want the applier's 90s", tc.login, vouch.Lag)
		}
		if vouch.Deferred {
			t.Errorf("%s: a deferral on a node that retained nothing", tc.login)
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
