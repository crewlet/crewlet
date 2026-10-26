package queries

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT'S FINISHED CALLS ARE MASKED ON A COPY, never on the rows the store
// handed back: the same rows are what the next reader's mask starts from, and
// one reader's mask written through would be every later reader's answer — a
// status reader's withholding hiding an admin's transcript, or worse the other
// way round once a cache sat in front of the read.
//
// And each reach withholds exactly its half: the payload only without the
// body, the work-item tags only without Worklet, every other column kept.
//
// Mutation: delete the tags from row.Tags itself rather than from a clone, or
// clear the payload on the input slice, and the input check goes red.
func TestMaskHistoryNeverWritesThrough(t *testing.T) {
	t.Parallel()
	rows := func() []store.EventRecord {
		return []store.EventRecord{{
			ID: "p-1", Type: "agent_phase_completed", Summary: "planned",
			Tags: map[string]string{"agent_id": "a-1",
				historyWorkItemTag: "native:task-1", historyTaskTag: "worker-7"},
			Payload: json.RawMessage(`{"response":"the secret plan"}`),
		}}
	}
	for _, c := range []struct {
		name          string
		reach         livestate.Reach
		payload, tags bool
	}{
		{"a status reader without Worklet", livestate.Reach{}, false, false},
		{"a status reader with the tracker", livestate.Reach{Tracker: true}, false, true},
		{"a lead without Worklet", livestate.Reach{Body: true}, true, false},
		{"an admin", livestate.Reach{Body: true, Tracker: true}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			input := rows()
			masked := maskHistory(input, c.reach)
			if !reflect.DeepEqual(input, rows()) {
				t.Fatalf("the mask wrote through to the store's rows: %+v", input)
			}
			got := masked[0]
			if shown := len(got.Payload) > 0; shown != c.payload {
				t.Errorf("the payload is shown: %v, want %v", shown, c.payload)
			}
			for _, tag := range []string{historyWorkItemTag, historyTaskTag} {
				if _, shown := got.Tags[tag]; shown != c.tags {
					t.Errorf("the %s tag is shown: %v, want %v", tag, shown, c.tags)
				}
			}
			if got.ID != "p-1" || got.Summary != "planned" || got.Tags["agent_id"] != "a-1" {
				t.Errorf("the mask took a status column with it: %+v", got)
			}
		})
	}
}
