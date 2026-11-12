package sandbox

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// A CALL'S HARNESS ID SURVIVES A BUILD THAT DOES NOT KNOW IT. In a fleet
// mid-upgrade an older build rewrites the run's log whole on every step it
// takes of it, decoding each call and each entry in flight into a struct with
// no such field: it keeps the id only as a key it does not know, carried in the
// call's Extra and written back verbatim (wirekeys.go). So the id is a wire key
// of its own, `call_id`, and a decode that declares every key of the call but
// that one — that build's — writes it back, for this build to read as the id
// its harness gave the call again, on a listed call and on an entry alike.
//
// Mutation: write the id under no wire key (call-id-under-no-wire-key), and the
// log never carries it.
func TestACallsHarnessIDSurvivesABuildThatDoesNotKnowIt(t *testing.T) {
	t.Parallel()
	const id = "toolu_01survivesanolderbuild"
	for name, call := range map[string]BridgeCall{
		"a listed call":      {Name: "post_message", Seq: 2, Appended: 1, Digest: strings.Repeat("d", 64), CallID: id},
		"an entry in flight": keptEntry(BridgeCall{Name: "delegate", Seq: 3, Runner: "node-a:1", CallID: id}),
	} {
		raw, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"call_id":"`+id+`"`) {
			t.Fatalf("%s is written %s, with no id under the key an older build carries", name, raw)
		}

		// THE OLDER BUILD'S DECODE AND ENCODE: this build's codec, declaring
		// every key of the call but the id's, into a call that holds no id.
		type plain BridgeCall
		older := maps.Clone(wireKeys(reflect.TypeFor[BridgeCall]()))
		delete(older, "call_id")
		var held plain
		extra, err := unmarshalCarrying(raw, &held, older)
		if err != nil {
			t.Fatal(err)
		}
		held.CallID = ""
		rewritten, err := marshalCarrying(held, older, extra)
		if err != nil {
			t.Fatal(err)
		}

		var back BridgeCall
		if err := json.Unmarshal(rewritten, &back); err != nil {
			t.Fatal(err)
		}
		if back.CallID != id || back.Name != call.Name || back.Seq != call.Seq {
			t.Errorf("%s: an older build's rewrite reads back as %+v (from %s), want the call it was, its "+
				"harness's id with it", name, back, rewritten)
		}
	}
}
