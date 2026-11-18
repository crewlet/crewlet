package sandbox

import (
	"encoding/json"
	"maps"
	"reflect"
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

// A CALL TELLING RIDES AN OLDER BUILD'S WRITE, AND A LATER BUILD'S KEYS IN IT
// RIDE THIS ONE'S. The tellings are a key of the notes section an older build
// does not declare, which its decode keeps in [RunSteer.Extra] and its write
// gives back, so a fleet mid-upgrade never drops what a confirmation recorded;
// and a telling is a section of its own a later build may add fields to
// ([CallTelling.Extra]).
//
// Mutation: drop CallTelling's codec (telling-without-extra), and the later
// build's key is gone after one write.
func TestACallTellingRidesAnOlderBuildsWrite(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"seq":2,"reads":5,"tellings":[{"round":"job-1","appended":3,"launch":"job-2","place":5,` +
		`"origin_node":"n-7"}]}`)
	var steer RunSteer
	if err := json.Unmarshal(raw, &steer); err != nil {
		t.Fatal(err)
	}
	want := CallTelling{Round: "job-1", Appended: 3, Launch: "job-2", Place: 5}
	if len(steer.Tellings) != 1 || steer.Tellings[0].Round != want.Round || steer.Tellings[0].Appended != 3 ||
		steer.Tellings[0].Launch != want.Launch || steer.Tellings[0].Place != 5 {
		t.Fatalf("the section decodes its tellings as %+v, want %+v", steer.Tellings, want)
	}
	written, err := json.Marshal(steer)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Tellings []struct {
			Origin json.RawMessage `json:"origin_node"`
		} `json:"tellings"`
	}
	if err := json.Unmarshal(written, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Tellings) != 1 || string(back.Tellings[0].Origin) != `"n-7"` {
		t.Errorf("this build wrote the section as %s: a later build's key in a telling is lost", written)
	}

	// AN OLDER BUILD'S SECTION: every key this one declares but the tellings'.
	type plain RunSteer
	older := maps.Clone(wireKeys(reflect.TypeFor[RunSteer]()))
	delete(older, "tellings")
	var read plain
	extra, err := unmarshalCarrying(written, &read, older)
	if err != nil {
		t.Fatal(err)
	}
	read.Tellings, read.Reads = nil, 6
	rewritten, err := marshalCarrying(read, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	var after RunSteer
	if err := json.Unmarshal(rewritten, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Tellings) != 1 || after.Tellings[0].Place != 5 || after.Reads != 6 {
		t.Errorf("after an older build's write the section reads tellings %+v at reads %d: %s", after.Tellings,
			after.Reads, rewritten)
	}
}
