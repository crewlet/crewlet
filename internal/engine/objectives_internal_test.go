package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE ALARM READING CARRIES WHAT THE NODE'S OWN METER JUDGED BURNING, so the
// events the recording sites count reach `objective_burn`, and a node with no
// meter raises nothing. Mutation: drop the reading's burns, and the alarm never
// fires.
func TestTheAlarmReadingCarriesTheObjectivesTheMeterJudgedBurning(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	meter := newObjectives(recorder)
	for range 90 {
		meter.Record(objectives.ViewFirstByte, time.Millisecond, true)
	}
	for range 10 {
		meter.Record(objectives.ViewFirstByte, time.Millisecond, false)
	}
	r := &retention{metrics: recorder, objectives: meter}
	var reading statelog.Reading
	r.burning(&reading)
	if len(reading.ObjectiveBurns) != 1 || reading.ObjectiveBurns[0].Objective != objectives.ViewFirstByte {
		t.Fatalf("the reading carries %+v, want view_first_byte burning", reading.ObjectiveBurns)
	}
	fired := false
	for _, a := range statelog.Evaluate(reading) {
		fired = fired || a.Kind == statelog.KindObjectiveBurn
	}
	if !fired {
		t.Error("the burning objective raised no objective_burn")
	}
	var counted uint64
	for _, s := range recorder.Read() {
		if s.Name == metrics.ObjectivesEvents && s.Attrs["objective"] == "view_first_byte" {
			counted += s.Total
		}
	}
	if counted != 100 {
		t.Errorf("the recorder counted %d view events, want every one of the 100", counted)
	}

	var quiet statelog.Reading
	(&retention{}).burning(&quiet)
	if len(quiet.ObjectiveBurns) != 0 {
		t.Errorf("a node with no meter carries burns %+v", quiet.ObjectiveBurns)
	}
	if newObjectives(nil) == nil {
		t.Error("a node with no recorder was given no meter")
	}
}
