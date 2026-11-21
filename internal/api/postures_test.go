package api_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE TRACKER READS A SEAT BY THE HANDLE ITS ITEMS ARE ASSIGNED TO (ADR-0162).
// The projection holds seats by agent id (ADR-0013), so the posture this app
// hands the engine names each of the company's agent seats by its handle; a
// seat with nothing to state is left out, which the flow reads as free.
//
// Mutation: key the posture by agent id, and the paused CTO's items read as on
// a free seat.
func TestThePostureNamesEachSeatByItsHandle(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{state: api.RuntimeState{Seats: []string{"ceo", "cto"}}})
	ceo, cto := rosterSeatID(t, "ceo").String(), rosterSeatID(t, "cto").String()
	s := a.State()
	s.SetPlacement(map[string]bool{ceo: true, cto: true})
	s.SeedPauses([]livestate.SeedPause{{AgentID: cto,
		Paused: livestate.Paused{By: "jane", At: "2026-11-01T09:00:00Z"}}})
	s.ReconcileExecutors(nil, time.Now())
	s.Apply(&livestate.Envelope{ID: "m1", Type: "budget_meters", Timestamp: "2026-11-01T09:00:01Z",
		Payload: map[string]any{"meter_id": "m-1", "seq": 1, "timezone": "UTC",
			"org": map[string]any{"windows": []any{}}}})

	got := a.Postures().SeatPostures()
	want := worklet.Posture{Live: true, Seats: map[string]worklet.SeatPosture{
		"cto": {Stop: worklet.FlowCausePaused, StopSince: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("posture = %+v, want %+v", got, want)
	}
}
