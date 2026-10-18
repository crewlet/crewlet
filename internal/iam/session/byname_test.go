package session_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SESSION HELD BY ITS NAME LANDS ON THE ROW A COOKIE WOULD.
//
// A view ticket names its session's lineage and never carries the bearer, so
// the artifact origin decides it from the rows alone: the epoch the row was
// opened at stands in for the bearer's, and an absent row is a session that is
// over (the caller proved it against the log's end first). The control is the
// first case; every refusal is a fact the rows state, and every unknown a node
// that cannot vouch. Mutation: read an absent session row's epoch as zero and a
// swept session is reported as an epoch move rather than as gone.
func TestASessionHeldByNameLandsOnTheRowACookieWould(t *testing.T) {
	t.Parallel()
	lineage := uuid.MustParse("018f3a9c-0000-7000-8000-00000000b0b0")
	live := func() session.Identity {
		return session.Identity{
			Applied: 4096, Generation: 2,
			Session: session.LineageRow{Found: true, Epoch: 3,
				ProvedAt: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)},
			Person: session.PersonRow{Found: true, Epoch: 3, Stage: iam.StageActive,
				Login: "sarah.chen", Seat: "platform-lead"},
		}
	}
	for _, tc := range []struct {
		name  string
		world func(*session.Identity, *error)
		row   session.Row
	}{
		{"everything checks out", func(*session.Identity, *error) {}, session.RowValid},
		{"the session row says it ended", func(i *session.Identity, _ *error) {
			i.Session.Ended = true
		}, session.RowEnded},
		{"the person signed out everywhere since", func(i *session.Identity, _ *error) {
			i.Person.Epoch = 4
		}, session.RowEnded},
		{"the person is suspended", func(i *session.Identity, _ *error) {
			i.Person.Stage = iam.StageSuspended
		}, session.RowEnded},
		{"the person is gone", func(i *session.Identity, _ *error) {
			i.Person = session.PersonRow{}
		}, session.RowGone},
		{"the session row is gone", func(i *session.Identity, _ *error) {
			i.Session = session.LineageRow{}
		}, session.RowGone},
		{"the copy is past the stall grace", func(i *session.Identity, _ *error) {
			i.Lag = 2 * statelog.StallGrace
		}, session.RowStalled},
		{"the copy holds a record about the person it cannot read", func(i *session.Identity, _ *error) {
			i.Deferred = true
		}, session.RowStalled},
		{"the rows could not be read", func(_ *session.Identity, err *error) {
			*err = errors.New("the estate is not open")
		}, session.RowStalled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			identity, err := live(), error(nil)
			tc.world(&identity, &err)
			got := session.StandingByName(lineage, "018f3a9c-0000-7000-8000-0000000000aa", identity, err)
			if got.Row != tc.row {
				t.Fatalf("row %q (%s), want %q", got.Row, got.Detail, tc.row)
			}
			if got.Row == session.RowValid {
				if got.Person.Login != "sarah.chen" || got.Session.ProvedAt.IsZero() {
					t.Errorf("a serving row did not carry the rows a principal is "+
						"composed from: %+v", got)
				}
				if got.Answer(session.NeedRead) != session.AnswerServe {
					t.Errorf("a valid row does not serve a read")
				}
			}
		})
	}
}
