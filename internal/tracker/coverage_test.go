package tracker_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN ANSWER THAT STATES NO COVERAGE SENDS NONE, and one that states some sends
// it. The zero coverage is a read answered below the router, by a backend with
// no partitions: sent as `{"addressed":0}` it would claim the read addressed
// nothing, which a client could only take as an answer from nowhere.
func TestAnAnswerStatingNoCoverageSendsNone(t *testing.T) {
	t.Parallel()
	stated := statelog.Coverage{Addressed: 1, Answered: []string{"tracker.000"}}
	for name, pair := range map[string][2]any{
		"tasks":    {tracker.Answer{}, tracker.Answer{Coverage: stated}},
		"my work":  {tracker.MyWork{}, tracker.MyWork{Coverage: stated}},
		"activity": {tracker.ActivityAnswer{}, tracker.ActivityAnswer{Coverage: stated}},
		"inbox":    {tracker.InboxAnswer{}, tracker.InboxAnswer{Coverage: stated}},
		"projects": {tracker.ProjectListing{}, tracker.ProjectListing{Coverage: stated}},
	} {
		if _, present := wireKeys(t, pair[0])["coverage"]; present {
			t.Errorf("%s: an answer stating no coverage sent one", name)
		}
		if _, present := wireKeys(t, pair[1])["coverage"]; !present {
			t.Errorf("%s: an answer stating its coverage did not send it", name)
		}
	}
	// A RANKED SEARCH STATES ITS PARTITIONS beside the bucket coverage its
	// outcome has always carried, under a key of their own, on the same
	// terms: none stated, none sent.
	searched := tracker.SearchAnswer{}
	searched.Partitions = stated
	if _, present := wireKeys(t, tracker.SearchAnswer{})["partitions"]; present {
		t.Error("search: an answer stating no partition coverage sent one")
	}
	if _, present := wireKeys(t, searched)["partitions"]; !present {
		t.Error("search: an answer stating its partition coverage did not send it")
	}
}

// wireKeys is the top-level keys v marshals to.
func wireKeys(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	return out
}
