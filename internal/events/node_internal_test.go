package events

import (
	"bytes"
	"encoding/json"
	"maps"
	"testing"
)

// olderBuild runs fn as a build that predates the envelope's `node`: one whose
// envelope does not own the key, so it reaches that build as a field of
// neither the envelope nor the payload — exactly an unknown field.
//
// NOT PARALLEL, and it must not be: it swaps the package's key set for the
// length of fn. A top-level test that never calls t.Parallel runs while every
// parallel test in the binary is still paused at its own t.Parallel, so the
// swap is observed by nothing but fn.
func olderBuild(t *testing.T, fn func()) {
	t.Helper()
	owned := envelopeKeys
	older := maps.Clone(owned)
	delete(older, "node")
	envelopeKeys = older
	defer func() { envelopeKeys = owned }()
	fn()
}

type nodeProbe struct {
	Count int `json:"count"`
}

func (*nodeProbe) EventType() string { return "test_node_probe" }

func init() { Register[nodeProbe]() }

// A ROLLING UPGRADE CARRIES THE ORIGIN THROUGH THE HALF THAT DOES NOT KNOW IT.
//
// The envelope's `node` is new, and for the length of an upgrade an older node
// decodes events a newer one published — and re-publishes some of them, a
// parked delivery handed back to the broker being the ordinary case. The older
// build has no field to decode the key into, so the only thing standing between
// the origin and oblivion is the rule that a key nobody owns is kept verbatim in
// Extra and written back out. If that rule failed for this key, every event an
// older node handed back would arrive at the newer half with no origin, and the
// newer half would stamp its OWN node on it: the row would then name a node
// whose store holds none of the rest of that turn.
//
// Both directions, because both happen: a newer event through an older build,
// and an older event — which names no node at all — through this one, which
// must not grow a key the older build never wrote.
func TestTheNodeKeyRoundTripsThroughABuildThatDoesNotKnowIt(t *testing.T) {
	const published = `{"id":"6f1c3d2e-0000-4000-8000-000000000003","type":"test_node_probe",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"engine","trace_id":"","span_id":"",` +
		`"parent_span_id":"","delegation_depth":0,"parent_turn_id":"",` +
		`"node":"node-a","count":3}`

	var republished []byte
	olderBuild(t, func() {
		var seen Event
		if err := json.Unmarshal([]byte(published), &seen); err != nil {
			t.Fatalf("the older build could not decode a newer event: %v", err)
		}
		// The older build HAS no Node field: whatever this build's decode
		// put there, the older one never did. Clearing it leaves Extra as
		// the only thing that can carry the key, which is the case.
		seen.Node = ""
		if _, carried := seen.Extra["node"]; !carried {
			t.Fatalf("the older build did not keep the key it does not own: Extra = %v", seen.Extra)
		}
		out, err := json.Marshal(&seen)
		if err != nil {
			t.Fatalf("the older build could not re-publish: %v", err)
		}
		republished = out
	})

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(republished, &keys); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if got := keys["node"]; !bytes.Equal(got, []byte(`"node-a"`)) {
		t.Fatalf("the older build re-published node as %s, want the origin's own bytes "+
			`"node-a"`+"\n%s", got, republished)
	}

	var back Event
	if err := json.Unmarshal(republished, &back); err != nil {
		t.Fatalf("decode the re-published event: %v", err)
	}
	if back.Node != "node-a" {
		t.Errorf("after an older node handed it back, the event names node %q, want the "+
			"origin node-a", back.Node)
	}
	if _, leaked := back.Extra["node"]; leaked {
		t.Error("this build carried its own envelope key in Extra as well")
	}

	// AND THE OTHER WAY: an event an older build published names no node,
	// and passing through this one must not invent the key.
	const older = `{"id":"6f1c3d2e-0000-4000-8000-000000000004","type":"test_node_probe",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"engine","count":3}`
	var fromOlder Event
	if err := json.Unmarshal([]byte(older), &fromOlder); err != nil {
		t.Fatalf("decode an older build's event: %v", err)
	}
	if fromOlder.Node != "" {
		t.Errorf("an event that named no node decoded as from %q", fromOlder.Node)
	}
	out, err := json.Marshal(&fromOlder)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var again map[string]json.RawMessage
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if node, present := again["node"]; present {
		t.Errorf("an event with no origin re-encoded with node %s — a stored row "+
			"would gain a key its publisher never wrote", node)
	}
}
