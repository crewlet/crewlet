package engine_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A HIRE THIS NODE'S VIEW DOES NOT CARRY YET IS NOT AN ABSENT SEAT.
//
// The mailbox sweep retires a seat's mail once the seat has been missing from
// its roster for a day, and it was gated on the SETTINGS epoch alone — while
// the seats come from the org chart's own log, which a hire moves with no
// revision anywhere in it. A duty holder whose published company predated a
// hire made on another node read the new seat as gone. The roster is unknown
// until the company this node serves was composed from rows holding every
// record the chart log holds.
func TestTheMailboxRosterWaitsForTheViewThatCarriesAHire(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)
	if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	before := e.Company()
	if got := rosterHandles(t, e); !slices.Equal(got, []string{"ceo", "dev"}) {
		t.Fatalf("roster before the hire = %v, want the seeded agent seats", got)
	}

	writer := e.ChartWriter()
	if _, err := writer.WriteBatch(t.Context(), "test:hire:cfo", chart.Batch{
		Operations: []chart.Operation{{Kind: chart.OpCreateSeat,
			Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "cfo"},
			SeatKind: chart.SeatAgent}},
	}); err != nil {
		t.Fatalf("hire: %v", err)
	}
	if _, err := writer.WriteSeat(t.Context(), "test:content:cfo", chart.SeatContent{
		Handle: "cfo", Name: "CFO", Runtime: json.RawMessage(`{"llm":["zulu"]}`),
	}); err != nil {
		t.Fatalf("write the seat: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if e.Company().Org.AgentSeatByHandle("cfo") != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	after := e.Company()
	if got := rosterHandles(t, e); !slices.Contains(got, "cfo") {
		t.Fatalf("roster once the view carries the hire = %v, want cfo in it", got)
	}

	// THE VIEW FROM BEFORE THE HIRE, served while the rows already hold it —
	// the window between a record committing and the rebuild that carries
	// it. Answered, the sweep would stamp cfo absent.
	engine.PublishForTest(e, before)
	if seats, err := engine.ChartRosterForTest(t.Context(), e); err == nil {
		t.Fatalf("a view predating the hire produced a roster %v; the new seat reads as gone",
			rosterSeatHandles(seats))
	}
	engine.PublishForTest(e, after)
}

func rosterHandles(t *testing.T, e *engine.Engine) []string {
	t.Helper()
	seats, err := engine.ChartRosterForTest(t.Context(), e)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	return rosterSeatHandles(seats)
}

func rosterSeatHandles(seats []placement.Seat) []string {
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		out = append(out, s.Handle)
	}
	slices.Sort(out)
	return out
}
