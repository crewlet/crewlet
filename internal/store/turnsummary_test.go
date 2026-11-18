package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// segment is one completion of a parked turn: its account and the item it names.
type segment struct {
	summary   string
	item      string
	suspended bool
}

// seedSegments writes a turn's completions one minute apart, oldest first, to log.
func seedSegments(t *testing.T, log *store.EventLog, id string, at time.Time, segs []segment) {
	t.Helper()
	for i, s := range segs {
		payload := map[string]any{"turn_id": id, "duration_ms": 1000, "plan_summary": s.summary,
			"suspended": s.suspended}
		if s.item != "" {
			payload["work_item"] = map[string]string{"backend": "native", "id": s.item}
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id + "-done-" + string(rune('a'+i)), Type: "turn_completed",
			Time: at.Add(time.Duration(i) * time.Minute), Category: "lifecycle", Actor: "Designer",
			Tags: map[string]string{"turn_id": id, "agent_role": "Designer"}, Payload: raw,
		}); err != nil {
			t.Fatalf("append segment %d: %v", i, err)
		}
	}
}

// selfIterated is the shape of a turn whose first execute ran out of rounds:
// it parked at each launch, its middle segment carries the review that sent
// the work back, and its last the review that accepted the redone work. Each
// earlier account sorts above the last byte for byte, as on the live turns.
var selfIterated = map[string][]segment{
	"nothing landed": {
		{summary: "", item: "", suspended: true},
		{summary: "Nothing landed this turn. Both publish attempts failed.", item: "task-9", suspended: true},
		{summary: "Artifact published as versions 5 to 8.", item: "task-1"},
	},
	"nothing written": {
		{summary: "", suspended: true},
		{summary: "Nothing has been written yet. This round only read things.", suspended: true},
		{summary: "Fixed the import and pushed the branch."},
	},
	"moved only": {
		{summary: "", suspended: true},
		{summary: "ENG-7 was moved to in_progress. The agent read the spec.", suspended: true},
		{summary: "**Done.** ENG-7's fix is merged."},
	},
}

// A TURN'S SUMMARY IS ITS END: the newest completion's account, never the
// greatest text among its segments', which listed a turn that delivered as one
// that landed nothing. The work item follows the same rule.
//
// Mutation: take the maximum of the plain text again, and every case reads its
// middle segment.
func TestATurnsSummaryIsItsNewestCompletions(t *testing.T) {
	t.Parallel()
	for name, segs := range selfIterated {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := open(t).Events()
			seedSegments(t, log, "t-1", time.Now().UTC().Add(-time.Hour), segs)
			got, err := log.Turns(t.Context(), store.TurnQuery{})
			if err != nil || len(got) != 1 {
				t.Fatalf("Turns: %d rows, %v", len(got), err)
			}
			last := segs[len(segs)-1]
			if got[0].Summary != last.summary {
				t.Errorf("the summary is %q, want the last segment's %q", got[0].Summary, last.summary)
			}
			if want := newestItem(segs); want != "" && (got[0].WorkItem == nil || got[0].WorkItem.ID != want) {
				t.Errorf("the work item is %+v, want the newest named, %s", got[0].WorkItem, want)
			}
		})
	}
}

// newestItem is the item the newest segment naming one names.
func newestItem(segs []segment) string {
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i].item != "" {
			return segs[i].item
		}
	}
	return ""
}

// A TURN RESUMED ON ANOTHER NODE ENDS WHERE ITS LAST SEGMENT RAN: the fleet's
// fold of two logs' partials reads the newest completion across both.
//
// Mutation: combine the summaries by their plain text, and the turn reads the
// first node's account.
func TestAFleetsTurnSummaryIsItsNewestCompletionAcrossLogs(t *testing.T) {
	t.Parallel()
	segs := selfIterated["nothing landed"]
	at := time.Now().UTC().Add(-time.Hour)
	first, second := open(t).Events(), open(t).Events()
	seedSegments(t, first, "t-1", at, segs[:2])
	seedSegments(t, second, "t-1", at.Add(2*time.Minute), segs[2:])
	var parts []store.TurnPartial
	for _, log := range []*store.EventLog{second, first} {
		p, _, err := log.TurnPartials(t.Context(), store.TurnQuery{})
		if err != nil || len(p) != 1 {
			t.Fatalf("TurnPartials: %d, %v", len(p), err)
		}
		parts = append(parts, p[0])
	}
	got := store.CombineTurnPartials(parts...).Turn()
	if got.Summary != segs[2].summary || got.WorkItem == nil || got.WorkItem.ID != segs[2].item {
		t.Errorf("the fleet's turn reads %q on %+v, want the last segment's %q on %s",
			got.Summary, got.WorkItem, segs[2].summary, segs[2].item)
	}
}
