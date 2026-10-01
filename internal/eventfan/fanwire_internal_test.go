package eventfan

import (
	"reflect"
	"testing"
	"time"
)

// v1ListFields is every listing filter the base protocol carried. A field
// outside it is one a v1 peer ignores.
var v1ListFields = map[string]bool{
	"Type": true, "Source": true, "Category": true, "TraceID": true, "Actor": true,
	"TurnID": true, "WorkKey": true, "WorkItem": true, "RelatedAgent": true,
	"Since": true, "Until": true, "Before": true, "Limit": true,
}

// EVERY FILTER ADDED SINCE v1 RAISES THE VERSION A LISTING IS ASKED IN.
//
// A peer on an older build ignores a filter it does not know and answers a
// wider question than was asked, so a listing carrying a newer filter has to go
// out at that filter's version, which the older peer refuses. The failure this
// guards is silent — a filter added to [listParams] and left out of
// [listParams.version] answers correctly on every current node and merges an
// older node's unfiltered rows into the page during every upgrade — so the
// check is by reflection over the wire type rather than a list somebody keeps.
//
// Mutation: drop either field from [listParams.version].
func TestEveryListingFilterSinceV1RaisesTheVersion(t *testing.T) {
	t.Parallel()
	if got := versionOf(QuestionEvents, listParams{Limit: 10}); got != 1 {
		t.Errorf("a listing with only base filters is asked in v%d, want v1 — an "+
			"older peer can answer it and must not be excluded", got)
	}
	typ := reflect.TypeFor[listParams]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if v1ListFields[field.Name] {
			continue
		}
		var p listParams
		v := reflect.ValueOf(&p).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Pointer:
			// A THREE-VALUED FILTER IS SET BY BEING PRESENT, and its zero
			// value is a filter too: `suspended: false` narrows as much as
			// `true` does, so it is the value an older peer must not drop.
			v.Set(reflect.New(v.Type().Elem()))
		default:
			t.Fatalf("listParams.%s is a %s; teach this test to set it", field.Name, v.Kind())
		}
		if got := versionOf(QuestionEvents, p); got < 2 {
			t.Errorf("listParams.%s is set and the listing is asked in v%d — a peer "+
				"that ignores it answers a wider question than was asked", field.Name, got)
		}
		if got := versionOf(QuestionEvents, p); got > Protocol {
			t.Errorf("listParams.%s asks v%d, beyond this build's v%d", field.Name, got, Protocol)
		}
		// AND ITS AXIS IS ASKED IN THE SAME VERSION: a histogram carries the
		// listing's filters, and a peer that drops one sums a wider bar.
		if got, want := versionOf(QuestionSeries, seriesParams{List: p}), max(2, p.version()); got != want {
			t.Errorf("listParams.%s is set on an axis asked in v%d, want v%d", field.Name, got, want)
		}
	}
	// A HISTOGRAM IS ALWAYS AT LEAST v2, because its answer carries the
	// failed split whatever it was asked.
	if got := versionOf(QuestionSeries, seriesParams{At: time.Now()}); got < 2 {
		t.Errorf("a histogram is asked in v%d; a v1 peer's bars carry no failed split", got)
	}
	// THE COMPANY'S PHASES NARROWED TO A SEAT are v3: an older peer reads
	// only the role name that question used to carry and answers every seat.
	if got := versionOf(QuestionPhases, phasesParams{AgentID: "agent-x"}); got != 3 {
		t.Errorf("the company's phases narrowed to a seat are asked in v%d, want v3", got)
	}
	if got := versionOf(QuestionPhases, phasesParams{Limit: 10}); got != 1 {
		t.Errorf("the company's phases, unnarrowed, are asked in v%d, want v1 — every build answers them", got)
	}
	// A PAGE OF TURNS IN A WINDOW OF INSTANTS is v3: an older peer reads only
	// `since_days` and answers the last week for a bar three days ago.
	for name, p := range map[string]turnsParams{
		"since": {Since: time.Now()},
		"until": {Until: time.Now()},
	} {
		if got := versionOf(QuestionTurns, p); got != 3 {
			t.Errorf("a page of turns carrying %s is asked in v%d, want v3", name, got)
		}
	}
	if got := versionOf(QuestionTurns, turnsParams{SinceDays: 7}); got != 1 {
		t.Errorf("a page of turns by days is asked in v%d, want v1", got)
	}
}
