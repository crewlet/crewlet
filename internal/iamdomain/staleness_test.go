package iamdomain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A VOUCH STATES BOTH FACTS, the lag and a deferral covering that person's
// bucket, and Resolve reads the same two. Lag alone misses a caught-up node
// holding that person's newer record; any deferral at all would refuse every
// bound credential for a whole rolling upgrade. This case holds the LAG half
// and the deferral half's control (a node that retained nothing covers
// nobody); the deferral half needs a real applier: internal/engine's
// TestARetainedRecordCoversExactlyItsPersonsBucket.
func TestAVouchIsTheLagAndNoDeferralWhereNothingIsRetained(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	someone := uuid.Must(uuid.NewV7()).String()
	enrolSarah(t, rig, someone)
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB:  rig.db.Reader(),
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
	// AND RESOLVE READS THE SAME TWO, so a token and a cookie are never told
	// different things about one person.
	identity, err := reader.Resolve(t.Context(), "no-such-lineage", someone)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if identity.Lag != 90*time.Second || identity.Deferred {
		t.Errorf("Resolve answered lag %s deferred %v, want 90s and false",
			identity.Lag, identity.Deferred)
	}
}
