package chart_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// AN AUTHORED COMPANY'S EMPTY RUNTIME HALF TRAVELS AS NONE.
//
// An offline `crewlet config import` stages an authored company as JSON and
// the node publishes it at its next boot. A nil runtime half encoded as
// `null`, and `null` decodes into a raw message as the four bytes `null` — so
// an object that declared no runtime arrived as one whose runtime is a value
// that decodes onto nothing, which a content write refuses, and the staged
// publish failed on every plain unit and seat in the file. Mutation: drop the
// `omitempty` and both halves come back as `null`.
func TestAnAuthoredObjectWithNoRuntimeDecodesAsNone(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(chart.Authored{
		Units: []chart.AuthoredUnit{{Key: "eng"}},
		Seats: []chart.AuthoredSeat{{Handle: "jane", Unit: "eng", Kind: chart.SeatHuman}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back chart.Authored
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Units[0].Runtime != nil || back.Seats[0].Runtime != nil {
		t.Errorf("a runtime half nobody declared came back as unit %q and "+
			"seat %q, want none", back.Units[0].Runtime, back.Seats[0].Runtime)
	}
}
