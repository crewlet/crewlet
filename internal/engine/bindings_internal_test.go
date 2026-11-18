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
		Person: id, Login: id + ".person", Kind: iam.KindPerson,
		Stage: iam.StageActive, Seat: seat,
	}
}

// boundService is a service account bound to a seat.
func boundService(id, seat string) iamdomain.SeatBinding {
	return iamdomain.SeatBinding{
		Person: id, Login: "ci:" + id, Kind: iam.KindMachine,
		Stage: iam.StageActive, Seat: seat,
	}
}

// A BINDING DANGLES EXACTLY WHEN THE REQUEST PATH WOULD REFUSE THE PERSON FOR
// WANT OF THE SEAT, and nothing else: a chart holding a row by the handle does
// not save a person bound to an AGENT seat. This walks every row of the table.
//
// AND THE REMEDY IS THE BOUND PRINCIPAL'S: a person's names a move or a
// removal, never the unbind the directory refuses (ADR-0032); a service
// account's names the unbind its optional seat allows.
func TestABindingDanglesExactlyWhenTheSeatTableRefusesItsPerson(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		chart    seatTable
		row      iamdomain.SeatBinding
		dangling bool
		says     []string
		never    string
		unknown  bool
	}{
		"a held human seat": {
			chart: companyChart(), row: bound("p1", "platform-lead"),
		},
		"a service account on a held human seat": {
			chart: companyChart(), row: boundService("m1", "platform-lead"),
		},
		// NO BINDING IS NEVER DANGLING, whatever kind of row carries it.
		"no binding at all": {
			chart: companyChart(), row: bound("p1", ""),
		},
		"a service account with no binding": {
			chart: companyChart(), row: boundService("m1", ""),
		},
		"a person on an agent's seat": {
			chart: companyChart(), row: bound("p1", "triage-bot"),
			dangling: true, says: []string{`"agent" seat`,
				"move them to another human seat", "remove them"},
			never: "unbind",
		},
		"a person on a seat the running company does not hold": {
			chart: companyChart(), row: bound("p1", "gone-lead"),
			dangling: true, says: []string{"isn't in the org chart anymore",
				"move them to another human seat", "remove them"},
			never: "unbind",
		},
		"a service account on an agent's seat": {
			chart: companyChart(), row: boundService("m1", "triage-bot"),
			dangling: true, says: []string{`"agent" seat`, "unbind them",
				"another human seat"},
		},
		"a service account on a seat the running company does not hold": {
			chart: companyChart(), row: boundService("m1", "gone-lead"),
			dangling: true, says: []string{"isn't in the org chart anymore",
				"unbind them", "another human seat"},
		},
		// A KIND THIS BUILD DOES NOT NAME is told only what holds for
		// both — never an unbind a person would be refused.
		"a principal of a kind this build does not name": {
			chart: companyChart(),
			row: iamdomain.SeatBinding{Person: "x1", Login: "x1.someone",
				Kind: "robot", Stage: iam.StageActive, Seat: "gone-lead"},
			dangling: true, says: []string{"isn't in the org chart anymore",
				"another human seat", "remove them"},
			never: "unbind",
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
			if !strings.Contains(detail, tc.row.Seat) {
				t.Errorf("detail %q does not name the seat %q", detail,
					tc.row.Seat)
			}
			// CASE-BLIND: the remedy opens its own sentence.
			lower := strings.ToLower(detail)
			for _, want := range tc.says {
				if !strings.Contains(lower, strings.ToLower(want)) {
					t.Errorf("detail %q does not say %q", detail, want)
				}
			}
			if tc.never != "" && strings.Contains(lower, strings.ToLower(tc.never)) {
				t.Errorf("detail %q tells an administrator to %s a %s, which "+
					"the directory refuses — a person always holds a seat",
					detail, tc.never, tc.row.Kind)
			}
		})
	}
}
