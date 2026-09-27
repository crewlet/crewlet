package iamdomain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SESSION THAT MAY ONLY ENROL IS WRITTEN AT THE VERSION THAT SAYS SO, AND
// READ BACK AS ONE.
//
// The restriction is the session's own fact, decided by the sign-in that opened
// it, and every node's guard reads it off the replicated row. An older build
// does not know the field: it would carry it past and serve the session whole,
// so a restricted session start is written at [iamdomain.ConditionRecordVersion]
// and such a build DEFERS it instead. An ordinary one stays at the base, or
// every sign-in of a rolling upgrade would be deferred on every older node.
//
// Mutation: drop the field from the read and the restricted row reads whole;
// write it at the base version and the first assertion fails.
func TestASessionThatMayOnlyEnrolIsWrittenAtItsOwnVersionAndReadBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		restricted bool
		version    int
	}{
		{"restricted", true, iamdomain.ConditionRecordVersion},
		{"whole (the control)", false, iamdomain.BaseRecordVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWriteRig(t)
			person := enrolForSessions(t, rig)
			lineage := uuid.Must(uuid.NewV7()).String()
			if err := rig.draining(func() error {
				_, err := rig.writer.OpenSession(t.Context(), iamdomain.SessionStart{
					Lineage: lineage, Person: person,
					AbsoluteExpiresAt: time.Now().UTC().Add(time.Hour),
					ProvedAt:          time.Now().UTC(),
					EnrolmentOnly:     tc.restricted, OpID: "session:" + lineage,
				})
				return err
			}); err != nil {
				t.Fatalf("open: %v", err)
			}
			rig.drain()
			if env := rig.lastEnvelope(); env.V != tc.version {
				t.Errorf("the session start is written at version %d, want %d",
					env.V, tc.version)
			}
			reader := rig.reader(t)
			seen, err := reader.Resolve(t.Context(), lineage, person)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !seen.Session.Found || seen.Session.EnrolmentOnly != tc.restricted {
				t.Errorf("the session reads back found %v, enrolment-only %v — "+
					"want %v", seen.Session.Found, seen.Session.EnrolmentOnly,
					tc.restricted)
			}
			// AND THE LISTING AN ADMINISTRATOR READS says the same.
			listed, err := reader.Sessions(t.Context(), person)
			if err != nil {
				t.Fatalf("Sessions: %v", err)
			}
			if len(listed) != 1 || listed[0].EnrolmentOnly != tc.restricted {
				t.Errorf("the listing holds %+v, want one session enrolment-only %v",
					listed, tc.restricted)
			}
		})
	}
}
