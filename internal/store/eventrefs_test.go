package store_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/named"
	"github.com/crewlet/crewlet/internal/store"
)

// A ROW KEEPS WHAT ITS SUMMARY NAMES (node/0069), and every read gives it back:
// a listing never selects the payload the refs were derived from, so a row read
// without them could say "ENG-287" and link to nothing. A row that names nothing
// reads back with none, never an empty list a client would draw.
//
// Mutation: drop `refs` from listColumns, or the column from the insert, and
// the named row reads back with none.
func TestARowKeepsWhatItsSummaryNames(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC()
	refs := []named.Ref{
		{Kind: named.WorkItem, ID: "0198-task", Text: "ENG-287"},
		{Kind: named.Change, ID: "web!42", Text: "web!42"},
	}
	for _, rec := range []store.EventRecord{{
		ID: "named", Type: "work_pickup", Category: "agent", Time: at.Add(-time.Minute),
		TraceID: "tr-named", Summary: "SRE was handed its next ready work item ENG-287 for web!42",
		Refs: refs, Payload: []byte(`{}`),
	}, {
		ID: "plain", Type: "agent_spawned", Category: "agent", Time: at.Add(-2 * time.Minute),
		TraceID: "tr-plain", Summary: "SRE joined the organization", Payload: []byte(`{}`),
	}} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}

	rows, err := log.List(t.Context(), store.ListQuery{At: at})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string][]named.Ref{}
	for _, r := range rows {
		got[r.ID] = r.Refs
	}
	if !reflect.DeepEqual(got["named"], refs) {
		t.Errorf("listed row names %+v, want %+v", got["named"], refs)
	}
	if got["plain"] != nil {
		t.Errorf("a row whose summary names nothing reads back naming %+v", got["plain"])
	}
	one, err := log.ByID(t.Context(), "named", at)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if !reflect.DeepEqual(one.Refs, refs) {
		t.Errorf("the row read by id names %+v, want %+v", one.Refs, refs)
	}
	trace, err := log.Trace(t.Context(), "tr-named", at)
	if err != nil || len(trace) != 1 || !reflect.DeepEqual(trace[0].Refs, refs) {
		t.Errorf("the trace's row names %+v (err %v), want %+v", trace, err, refs)
	}
}
