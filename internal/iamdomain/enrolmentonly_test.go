package iamdomain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SESSION THAT MAY ONLY ENROL IS READ BACK AS ONE.
//
// The restriction is the session's own fact, decided by the sign-in that opened
// it, and every node's guard reads it off the replicated row.
//
// Mutation: drop the field from the read and the restricted row reads whole.
func TestASessionThatMayOnlyEnrolIsReadBackAsOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		restricted bool
	}{
		{"restricted", true},
		{"whole (the control)", false},
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
