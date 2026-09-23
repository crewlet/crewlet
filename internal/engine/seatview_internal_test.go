package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE WITH NO CHART DOMAIN ANSWERS UNKNOWN, NEVER SEATLESS.
//
// The zero [SeatView] is what a seats-only satellite passes, and the one
// answer it must not give is "no seat" — [session.ResolveSeat] reads that as
// the seatless arm and hands somebody bound to a lead's seat an empty handle,
// which is the silent fall-through the whole three-valued shape exists to
// prevent. Both methods error, and a person is then 503 rather than served.
func TestASeatViewWithNoChartRefusesRatherThanAnsweringSeatless(t *testing.T) {
	t.Parallel()
	var view SeatView
	if _, found, err := view.Seat(t.Context(), "platform-lead"); err == nil {
		t.Errorf("Seat answered found=%v with no error, so a satellite would "+
			"report every seat in the company as gone", found)
	}
	if _, _, err := view.Position(t.Context()); err == nil {
		t.Error("Position answered with no error, so a satellite's zero " +
			"would read as a node that has applied everything")
	}
	// AND THROUGH THE RESOLVER, which is where it matters: the row must
	// be `stalled` rather than `seatless`.
	binding := session.ResolveSeat(t.Context(), view,
		session.PersonRow{Found: true, Seat: "platform-lead", SeatAt: 900})
	if binding.Row != session.SeatRowStalled {
		t.Errorf("row %q, want %q", binding.Row, session.SeatRowStalled)
	}
	if binding.Handle() != "" {
		t.Errorf("handle %q from a node that cannot answer", binding.Handle())
	}
}

// SeatViewOf OVER A NIL ENGINE IS THE ZERO VALUE, not a panic: the wiring
// builds it before it knows whether this node runs a chart domain.
func TestSeatViewOfANilEngineIsTheZeroValue(t *testing.T) {
	t.Parallel()
	if view := SeatViewOf(nil); view.reader != nil {
		t.Errorf("SeatViewOf(nil) carries a reader: %+v", view)
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
// Two identity gestures are performed on behalf of a person who does not
// exist: the first person a bootstrap code creates, and the person an
// invitation redeems into. Both go through this node's own writer, and the
// domain refuses an administrative record from a party without
// [iamdomain.AdminGrant].
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
