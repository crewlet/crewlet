package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE RUNNING NO COMPANY ANSWERS UNKNOWN, NEVER SEATLESS.
//
// The zero [SeatView] is what a view over no engine is, and the one answer it
// must not give is "no seat" — [session.ResolveSeat] reads that as the
// seatless arm and hands somebody bound to a lead's seat an empty handle,
// which is the silent fall-through the whole three-valued shape exists to
// prevent. Both methods error, and a person is then 503 rather than served.
func TestASeatViewWithNoCompanyRefusesRatherThanAnsweringSeatless(t *testing.T) {
	t.Parallel()
	var view SeatView
	if _, found, err := view.Seat(t.Context(), "platform-lead"); err == nil {
		t.Errorf("Seat answered found=%v with no error, so a view with no "+
			"company would report every seat in the company as gone", found)
	}
	if _, err := view.Version(t.Context()); err == nil {
		t.Error("Version answered with no error, so a view with no company " +
			"would read as one the binding watch may classify against")
	}
	// AND THROUGH THE RESOLVER, which is where it matters: the row must
	// be `stalled` rather than `seatless`.
	binding := session.ResolveSeat(t.Context(), view,
		session.PersonRow{Found: true, Seat: "platform-lead"})
	if binding.Row != session.SeatRowStalled {
		t.Errorf("row %q, want %q", binding.Row, session.SeatRowStalled)
	}
	if binding.Handle() != "" {
		t.Errorf("handle %q from a node that cannot answer", binding.Handle())
	}
}

// A BINDING'S SEAT IS THE SEAT THE RUNNING COMPANY HOLDS UNDER ITS HANDLE, and
// it is gone the moment an applied revision no longer holds it.
//
// The running company is what every surface on this node routes, attributes
// and authorizes by, so a seat it does not hold is conclusively absent — not
// "not yet": there is no log this node can be behind on. The version moves
// with the epoch, which is what lets the dangling-binding watch skip a beat on
// which nothing moved.
//
// The control is the seat before the apply: found, and human.
func TestTheSeatViewAnswersFromTheRunningCompany(t *testing.T) {
	t.Parallel()
	e := bootDirectoryNode(t, nil)
	view := SeatViewOf(e)

	seat, found, err := view.Seat(t.Context(), founderSeat)
	if err != nil || !found || seat.Handle != founderSeat ||
		seat.Kind != session.SeatKindHuman || seat.Name != "Dana Founder" {
		t.Fatalf("the founder's seat reads %+v (found %v, %v), want the human "+
			"seat the company holds", seat, found, err)
	}
	if seat, found, err := view.Seat(t.Context(), "ceo"); err != nil || !found ||
		seat.Kind == session.SeatKindHuman {
		t.Errorf("the CEO reads %+v (found %v, %v), want an agent seat", seat, found, err)
	}
	before, err := view.Version(t.Context())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}

	// THE FOUNDER'S SEAT LEAVES THE COMPANY in one apply.
	without := directoryConfig(t)
	var roles []config.Role
	for _, role := range without.Roles {
		if role.Handle != founderSeat {
			roles = append(roles, role)
		}
	}
	without.Roles = roles
	if _, _, err := e.Apply(t.Context(), without, time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if seat, found, err := view.Seat(t.Context(), founderSeat); err != nil || found {
		t.Errorf("the removed seat reads %+v (found %v, %v), want it absent — a "+
			"leaver's seat is refused at once", seat, found, err)
	}
	if after, err := view.Version(t.Context()); err != nil || after == before {
		t.Errorf("the version reads %d (%v) after an apply, the same as before: "+
			"the binding watch would skip the beat that changed every answer", after, err)
	}
}

// SeatViewOf OVER A NIL ENGINE IS THE ZERO VALUE, not a panic: the wiring
// builds it before it knows whether this node runs a company.
func TestSeatViewOfANilEngineIsTheZeroValue(t *testing.T) {
	t.Parallel()
	if view := SeatViewOf(nil); view.engine != nil {
		t.Errorf("SeatViewOf(nil) carries an engine: %+v", view)
	}
}

// THE LAG IS A DURATION DERIVED FROM THIS APPLIER'S OWN DRAIN RATE.
//
// "Four hundred records behind" is not a length of time until something says
// how fast this node applies them, and what reads the answer compares it
// against [statelog.StallGrace]. Until this existed the figure handed to the
// session tables was hardcoded at zero, so the `stalled` arm of both tables
// could never fire and a node an hour behind authenticated as a caught-up
// one.
func TestTheLagIsRecordsOverTheDrainRate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		last, applied uint64
		drain         float64
		want          time.Duration
	}{
		{"caught up", 100, 100, 10, 0},
		{"a position past the head reads as caught up", 100, 101, 10, 0},
		{"sixty records at ten a second", 160, 100, 10, 6 * time.Second},
		{"one record at one a second", 101, 100, 1, time.Second},
		{
			// A RATE OF ZERO IS FLOORED AT ONE rather than divided
			// by: a node with no measured rate is the one least able
			// to claim it is nearly caught up, so one second per
			// record is the deliberately pessimistic reading.
			name: "no measured rate is one record per second",
			last: 190, applied: 100, drain: 0,
			want: 90 * time.Second,
		},
		{
			// AND A FRACTIONAL RATE TRUNCATES TOWARDS THE SAME
			// FLOOR rather than towards zero seconds, for the same
			// reason: half a record a second is slower than one, and
			// rounding it up to one understates the lag — which is
			// the direction that serves a stalled node's sessions.
			name: "a rate below one is floored at one",
			last: 130, applied: 100, drain: 0.5,
			want: 30 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lagDurationOf(tc.last, tc.applied, tc.drain); got != tc.want {
				t.Errorf("lag %s, want %s", got, tc.want)
			}
		})
	}
}

// AND IT CROSSES THE STALL GRACE, which is the only threshold that reads it.
func TestALagPastTheStallGraceIsVisibleAsOne(t *testing.T) {
	t.Parallel()
	behind := uint64(statelog.StallGrace/time.Second) + 1
	if got := lagDurationOf(100+behind, 100, 1); got <= statelog.StallGrace {
		t.Errorf("lag %s does not exceed the %s grace, so the tables that "+
			"read it can never reach their stalled arm", got,
			statelog.StallGrace)
	}
}

// THE STORE AND THE READ ARE THE SAME NUMBER, which is what makes the
// heartbeat's sample reachable from the request path at all.
func TestTheObservedLagIsWhatTheRequestPathReads(t *testing.T) {
	t.Parallel()
	var d runningDomain
	if got := d.Lag(); got != 0 {
		t.Errorf("an unobserved domain reports %s, want 0", got)
	}
	d.lagNanos.Store(int64(7 * time.Second))
	if got := d.Lag(); got != 7*time.Second {
		t.Errorf("lag %s, want 7s", got)
	}
}

// THE NODE'S OWN WRITER MAY ENROL SOMEBODY WHO HAS NO PRINCIPAL YET.
//
// An identity gesture is performed on behalf of a person who does not exist:
// the person an invitation redeems into, the company's first person included.
// It goes through this node's own writer, and the domain refuses an
// administrative record from a party without [iamdomain.AdminGrant].
//
// This pairing had already drifted: the writer held fleet:operate and the
// domain asked for config:write, so every bootstrap and every redemption was
// refused on a real deployment — invisibly, because the surface that drives
// them is exercised against a stub writer.
func TestTheNodesOwnWriterMayEnrolOnSomebodysBehalf(t *testing.T) {
	t.Parallel()
	if !slices.Contains(nodeWriterGrants, iamdomain.AdminGrant) {
		t.Errorf("the node writes identity records as %v, which does not "+
			"carry %s — so a fresh deployment cannot create its first person "+
			"and an invitation cannot be redeemed",
			nodeWriterGrants, iamdomain.AdminGrant)
	}
}
