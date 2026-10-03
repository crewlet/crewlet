package eventfan

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
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
	// AN INSTANT IS NOT A FILTER: a page of turns carrying only the asker's
	// clock is answered by every build, which floors the window against its
	// own clock as every version before it did.
	if got := versionOf(QuestionTurns, turnsParams{SinceDays: 7, At: time.Now()}); got != 1 {
		t.Errorf("a page of turns carrying only the asker's instant is asked in v%d, want v1", got)
	}
	// ONE SEAT'S SPEND is v5: an older peer reads only the role name the
	// question used to narrow by, and would answer every seat's records.
	if got := versionOf(QuestionPhaseTokens, phaseTokenParams{AgentID: "agent-x"}); got != 5 {
		t.Errorf("one seat's spend window is asked in v%d, want v5", got)
	}
	if got := versionOf(QuestionPhaseTokens, phaseTokenParams{Limit: 10, At: time.Now()}); got != 1 {
		t.Errorf("the company's spend window is asked in v%d, want v1 — every build answers it", got)
	}
}

// A PEER MEASURES A WINDOW FROM THE ASKER'S INSTANT, never its own clock.
//
// The store refuses a windowed read with no instant, so a peer has to hand it
// one; reading its own "now" there would be the second evaluation of the clock
// the asker's label was not taken from. Here the asker's instant is two hours
// back and the only record an hour back: measured from the asker, the window
// ends before the record; measured from the peer's clock, it would hold it.
//
// Mutation: answer from askedAt(time.Time{}) instead of the request's `at`,
// and the record is counted.
func TestAPeerMeasuresTheWindowFromTheAskersInstant(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "peer.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	if err := db.Events().Append(t.Context(), store.EventRecord{
		ID: "an-hour-ago", Type: "agent_phase_completed", Category: "task",
		Time: now.Add(-time.Hour), Tags: map[string]string{"turn_id": "t-1"},
		Payload: json.RawMessage(`{}`),
		Spend:   &store.Spend{Phase: "execute", TurnID: "t-1", TotalTokens: 5},
	}); err != nil {
		t.Fatal(err)
	}
	ask := func(at time.Time) int {
		t.Helper()
		params, err := json.Marshal(phaseTokenParams{At: at})
		if err != nil {
			t.Fatal(err)
		}
		part, err := answer(t.Context(), db.Events(), QuestionPhaseTokens, params, nil)
		if err != nil {
			t.Fatal(err)
		}
		return len(part.(spendPart).Records)
	}
	if got := ask(now.Add(-2 * time.Hour)); got != 0 {
		t.Errorf("a window measured from two hours ago held %d records, want none — "+
			"the peer read its own clock rather than the asker's", got)
	}
	if got := ask(now); got != 1 {
		t.Errorf("a window measured from now held %d records, want the one", got)
	}
}
