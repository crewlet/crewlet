package sandbox

import (
	"encoding/json"
	"testing"
)

// A LATER BUILD'S KEYS IN A LATE-CALL ITEM RIDE THIS BUILD'S WRITE. The item's
// late call is a section of the record a later build may add fields to, at its
// own level ([LateCall.Extra]) and in the call it carries ([BridgeCall.Extra]):
// a node of this build reads and writes the record whole on every write, so a
// key it does not know survives only where its decode kept it. And the item
// itself is a note, whose own Extra carries `late_call` through an older build
// that does not know the field, as it carries any key ([RunNote.Extra]).
//
// Mutation: drop LateCall's codec (late-call-without-extra), and the later
// build's key at its level is gone after one write.
func TestALateCallKeepsALaterBuildsKeysThroughThisBuildsWrite(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"id":"call:job-2:3","seq":4,"sealed":null,"at":"2026-11-18T09:00:00Z","for":"job-2",` +
		`"late_call":{"round":"job-1","call":{"name":"delegate","at":"2026-11-18T09:00:00Z","seq":3,` +
		`"appended":3,"late":true,"span":"s-1"},"origin_node":"n-7"}}`)
	var note RunNote
	if err := json.Unmarshal(raw, &note); err != nil {
		t.Fatal(err)
	}
	if note.LateCall == nil || note.LateCall.Call.Appended != 3 || note.For != "job-2" {
		t.Fatalf("the item decodes as %+v, want its late call of the job's", note)
	}
	written, err := json.Marshal(note)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		LateCall struct {
			Origin json.RawMessage `json:"origin_node"`
			Call   struct {
				Span json.RawMessage `json:"span"`
			} `json:"call"`
		} `json:"late_call"`
	}
	if err := json.Unmarshal(written, &back); err != nil {
		t.Fatal(err)
	}
	if string(back.LateCall.Origin) != `"n-7"` || string(back.LateCall.Call.Span) != `"s-1"` {
		t.Errorf("this build wrote the item as %s: a later build's keys at the late call (%s) and its call (%s) "+
			"are lost", written, back.LateCall.Origin, back.LateCall.Call.Span)
	}
}
