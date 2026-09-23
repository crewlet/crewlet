package iamdomain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// STALENESS STATES BOTH FACTS, SCOPED TO THE PERSON — and Resolve reads the
// same two.
//
// A Tier A token's seat binding is a directory row read on every request, and
// the engine refuses to honour one a node cannot vouch for: past the stall
// grace, or holding a newer peer's record about that person's bucket. Asked
// about the lag alone, a caught-up node holding exactly that record reads as
// current; asked about any deferral at all, one record about somebody else
// would refuse every bound credential on the node for the length of a rolling
// upgrade. So the deferral is SCOPED to the person's bucket, and an empty
// person is covered by every one, since nothing can say what a record this
// node could not decode is about.
func TestStalenessIsTheLagAndADeferralCoveringThatPerson(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	covered := uuid.New().String()
	elsewhere := uuid.New().String()
	for iamdomain.BucketOf(elsewhere) == iamdomain.BucketOf(covered) {
		elsewhere = uuid.New().String()
	}
	log, err := statelogtest.LocalReaderOver(
		iamdomain.Domain{}, rig.db.Replicated(), rig.waiter)
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB: rig.db, Log: log,
		Lag: func() time.Duration { return 90 * time.Second },
		Deferred: func() (statelog.Deferral, bool) {
			return statelog.Deferral{Scope: statelog.ScopeSet{
				Paths: []string{iamdomain.BucketOf(covered).Path()},
			}}, true
		},
	})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	for _, tc := range []struct {
		name     string
		person   string
		deferred bool
	}{
		{"the person the deferred record's bucket covers", covered, true},
		{"a person in another bucket", elsewhere, false},
		{"nobody in particular", "", true},
	} {
		lag, deferred := reader.Staleness(tc.person)
		if lag != 90*time.Second {
			t.Errorf("%s: lag %s, want the applier's 90s", tc.name, lag)
		}
		if deferred != tc.deferred {
			t.Errorf("%s: deferred %v, want %v", tc.name, deferred, tc.deferred)
		}
	}
	// AND THE SESSION TABLE READS THE SAME TWO FACTS, so a token and a
	// cookie can never be told different things about one person.
	identity, err := reader.Resolve(t.Context(), "no-such-lineage", covered)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if identity.Lag != 90*time.Second || !identity.Deferred {
		t.Errorf("Resolve answered lag %s deferred %v, want 90s and true",
			identity.Lag, identity.Deferred)
	}
}
