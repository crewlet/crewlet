package sandbox

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// A CALL'S HARNESS ID SURVIVES A BUILD THAT DOES NOT KNOW IT. Mid-upgrade an
// older build rewrites the run's log whole on every step, decoding each call
// and entry into a struct with no such field and carrying unknown keys back
// verbatim in Extra (wirekeys.go). So the id is a wire key of its own,
// `call_id`, which that build's decode writes back, on a listed call and an
// entry in flight alike.
//
// Mutation: write the id under no wire key (call-id-under-no-wire-key), and
// the log never carries it.
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
