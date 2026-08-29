package store_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE EVENT RECORD'S WIRE NAMES ARE THE DASHBOARD'S.
//
// This struct is what /events, /events/{id} and /events/trace/{id} answer
// with. It carried no JSON tags at all, so Go marshalled it as ID, Type,
// Source, Time, TraceID, Payload — and the client reads id, type, source,
// timestamp, trace_id, payload. Every one of those screens rendered a blank
// event with an empty payload, and nothing failed: the fields were simply not
// the ones being read.
//
// A shape test rather than a screen test, because that is where the mistake
// is invisible. Nothing in Go notices a field name; only a reader on the other
// end of the wire does, and by then it renders as "this event has nothing in
// it" rather than as an error.
func TestAnEventRecordMarshalsWithTheNamesTheClientReads(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(store.EventRecord{
		ID:      "e-1",
		Type:    "chat_message_received",
		Source:  "mattermost",
		Time:    time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		TraceID: "t-1",
		Payload: json.RawMessage(`{"text":"hi"}`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Every field the event screens read, by the name they read it under.
	for _, key := range []string{
		"id", "type", "source", "timestamp", "category",
		"summary", "actor", "trace_id", "span_id", "parent_span_id",
		"failed", "payload",
	} {
		if _, present := wire[key]; !present {
			t.Errorf("the wire has no %q; the screen reading it renders blank "+
				"and reports no error. Keys: %v", key, sortedKeys(wire))
		}
	}
	// And nothing PascalCase survives, which is what the absence of tags
	// produced and what a partially-tagged struct would leave behind.
	for key := range wire {
		if key != "" && key[0] >= 'A' && key[0] <= 'Z' {
			t.Errorf("field %q is still Go-cased; the client reads snake_case", key)
		}
	}
	if wire["timestamp"] != "2026-08-25T12:00:00Z" {
		t.Errorf("timestamp = %v, want the instant the feed's own rows carry", wire["timestamp"])
	}
}

// AND IT MATCHES THE LIVE FEED'S ROW FIELD FOR FIELD.
//
// One screen shows a live row from the projection and a historical one from
// the store. Two spellings of one event would render the two halves of a
// single list differently — the exact failure the shared naming prevents.
func TestTheStoredEventMatchesTheLiveFeedsNames(t *testing.T) {
	t.Parallel()
	stored := wireKeys(t, store.EventRecord{})
	// livestate.FeedRow's names, restated here rather than imported: this
	// asserts an AGREEMENT between two packages, and reading one of them
	// through the other would make the test agree with itself.
	for _, key := range []string{
		"id", "type", "timestamp", "source", "actor",
		"summary", "category", "trace_id", "span_id", "parent_span_id", "failed",
	} {
		if !slices.Contains(stored, key) {
			t.Errorf("the stored record has no %q, which the live feed carries", key)
		}
	}
}

func wireKeys(t *testing.T, rec store.EventRecord) []string {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return sortedKeys(wire)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
