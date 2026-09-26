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
	}
	// A HISTOGRAM IS ALWAYS AT LEAST v2, because its answer carries the
	// failed split whatever it was asked.
	if got := versionOf(QuestionSeries, seriesParams{At: time.Now()}); got < 2 {
		t.Errorf("a histogram is asked in v%d; a v1 peer's bars carry no failed split", got)
	}
}
