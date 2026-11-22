package queries_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A REPORT'S WEEK READS THE FLOW RECORD, AND NO WEEK BEFORE THE COMPANY: the
// week's due, held and blocked hours and its pickup wait come from the flow
// record, and a week the company's documents do not reach back over the start
// of, the one before its first boot, is unmeasured rather than read as zeros.
// Mutation: read the week without the company's history, and its first
// report is a week of zeros.
func TestAReportsWeekReadsTheFlowAndNoWeekBeforeTheCompany(t *testing.T) {
	t.Parallel()
	surface, _ := configSurface(t, companyDoc)
	work := &stubWork{flowHours: []worklet.FlowHours{{Seat: "dev", State: worklet.FlowDue, Stood: 3 * time.Hour}},
		pickups: []time.Duration{20 * time.Minute}}
	s := queries.Sources{Config: surface, Work: work}
	if _, _, err := s.WeekOf(t.Context(), pinned.AddDate(0, 0, -7), pinned); !errors.Is(err, advisor.ErrWeekUnmeasured) {
		t.Errorf("the week before the company's first document read as %v, want it unmeasured", err)
	}
	from := pinned.Add(time.Hour)
	metrics, _, err := s.WeekOf(t.Context(), from, from.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, m := range metrics {
		got[m.Key] = m.Value
	}
	if got["wait_h"] != 3 || got["pickup_p50"] != 20 {
		t.Errorf("the week's flow reads %v, want three hours waited and a 20-minute pickup", got)
	}
}
