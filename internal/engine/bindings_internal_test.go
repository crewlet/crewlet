package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// seatTable is a [session.Chart] answering from a table.
type seatTable struct {
	seats   map[string]session.Seat
	seatErr error
}

func (c seatTable) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
	if c.seatErr != nil {
		return session.Seat{}, false, c.seatErr
	}
	seat, found := c.seats[ref]
	return seat, found, nil
}

// companyChart is an organisation holding a human seat and an agent seat.
func companyChart() seatTable {
	return seatTable{seats: map[string]session.Seat{
		"platform-lead": {Handle: "platform-lead", Kind: session.SeatKindHuman},
		"triage-bot":    {Handle: "triage-bot", Kind: "agent"},
	}}
}

// bound is a person bound to a seat.
func bound(id, seat string) iamdomain.SeatBinding {
	return iamdomain.SeatBinding{
		Person: id, Login: id + ".person", Stage: iam.StageActive, Seat: seat,
	}
}

// A BINDING DANGLES EXACTLY WHEN THE REQUEST PATH WOULD REFUSE THE PERSON FOR
// WANT OF THE SEAT, and nothing else.
//
// The predicate this replaced asked whether the chart held a row by that
// handle, so a person bound to an AGENT seat — refused 403 on every request
// they made — was one the report said nothing about. The rule is now the seat
// table itself, and this walks every row of it.
func TestABindingDanglesExactlyWhenTheSeatTableRefusesItsPerson(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		chart    seatTable
		row      iamdomain.SeatBinding
		dangling bool
		says     string
		unknown  bool
	}{
		"a held human seat": {
			chart: companyChart(), row: bound("p1", "platform-lead"),
		},
		"no binding at all": {
			chart: companyChart(), row: bound("p1", ""),
		},
		"an agent's seat": {
			chart: companyChart(), row: bound("p1", "triage-bot"),
			dangling: true, says: `"agent" seat`,
		},
		"a seat the running company does not hold": {
			chart: companyChart(), row: bound("p1", "gone-lead"),
			dangling: true, says: "not a seat of the company",
		},
		"an organisation that cannot be read": {
			chart: seatTable{seatErr: errors.New("no company yet")},
			row:   bound("p1", "platform-lead"), unknown: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dangling, detail, err := danglingBinding(t.Context(), tc.chart, tc.row)
			if tc.unknown {
				if err == nil {
					t.Fatalf("a chart that cannot answer gave dangling=%v with no "+
						"error — a guess either way sends somebody to the wrong "+
						"remedy", dangling)
				}
				return
			}
			if err != nil {
				t.Fatalf("danglingBinding: %v", err)
			}
			if dangling != tc.dangling {
				t.Fatalf("dangling = %v, want %v (%s)", dangling, tc.dangling,
					detail)
			}
			if !dangling {
				return
			}
			if !strings.Contains(detail, tc.says) ||
				!strings.Contains(detail, tc.row.Seat) {
				t.Errorf("detail %q does not say %q about %q", detail,
					tc.says, tc.row.Seat)
			}
		})
	}
}
