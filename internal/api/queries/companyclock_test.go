package queries_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
)

// EVERY ANSWER THAT CUTS A DAY CUTS IT ON THE COMPANY'S CLOCK, AS IT IS AT THE
// CALL (ADR-0018).
//
// Three questions on this surface are cut on a day — the board's relative
// dates, due bands and overdue marks (`work_items`), a person's own day
// (`work_my_work`) and the workload's overdue counts (`work_workload`) — and
// each hands the tracker the instant and the clock to cut it on. All three
// were handed UTC, so for the hours between the company's midnight and UTC's a
// board said "today" about a different day from the one a seat's own tool
// resolved. The tracker's own suite holds what a reader does with the clock
// (TestDueBandsCutAtCompanyMidnight); this holds that the surface passes the
// COMPANY's, read per call so an apply that moves it moves the next answer.
func TestTheWorkAnswersCutTheirDayOnTheCompanysClock(t *testing.T) {
	t.Parallel()
	company, err := config.ParseCompany([]byte(partyCompany + "timezone: America/Los_Angeles\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	current := company
	now := time.Date(2026, time.September, 24, 6, 30, 0, 0, time.UTC)
	work := &stubWork{}
	sources := queries.Sources{
		Company: func() *config.Company { return current },
		Work:    work,
		Now:     func() time.Time { return now },
	}

	for _, tc := range []struct {
		what   string
		params map[string]any
		got    func() (time.Time, *time.Location)
	}{
		{"work_items", nil, func() (time.Time, *time.Location) { return work.expandNow, work.expandZone }},
		{"work_my_work", nil, func() (time.Time, *time.Location) { return work.myWorkNow, work.myWorkZone }},
		{"work_workload", nil, func() (time.Time, *time.Location) { return work.workloadNow, work.workloadZone }},
	} {
		current = company
		if _, err := askAsOperator(t, sources, tc.what, tc.params); err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		at, zone := tc.got()
		if zone == nil || zone.String() != "America/Los_Angeles" {
			t.Errorf("%s cut its day on %v, want the company's clock America/Los_Angeles",
				tc.what, zone)
		}
		if !at.Equal(now) {
			t.Errorf("%s cut its day at %v, want this surface's own clock %v",
				tc.what, at, now)
		}

		// AN APPLY THAT DROPS THE CLOCK puts the next answer on UTC —
		// the default company's — rather than on the zone it had.
		current = &config.Company{Name: "Acme", Roles: company.Roles}
		if _, err := askAsOperator(t, sources, tc.what, tc.params); err != nil {
			t.Fatalf("%s after the apply: %v", tc.what, err)
		}
		if _, zone := tc.got(); zone != time.UTC {
			t.Errorf("%s after the clock was removed cut its day on %v, want UTC",
				tc.what, zone)
		}
	}
}
