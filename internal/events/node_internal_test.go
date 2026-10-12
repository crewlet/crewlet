package events

import (
	"bytes"
	"encoding/json"
	"maps"
	"testing"
)

// successorBuild runs fn as a build that owns one more envelope key than this
// one — `zone`, invented for the case — which is what a successor adding an
// envelope field looks like to this build: a key of neither the envelope nor
// the payload, exactly an unknown field.
//
// NOT PARALLEL, and it must not be: it swaps the package's key set for the
// length of fn. A top-level test that never calls t.Parallel runs while every
// parallel test in the binary is still paused at its own t.Parallel, so the
// swap is observed by nothing but fn.
func successorBuild(t *testing.T, fn func()) {
	t.Helper()
	owned := envelopeKeys
	newer := maps.Clone(owned)
	newer["zone"] = struct{}{}
	envelopeKeys = newer
	defer func() { envelopeKeys = owned }()
	fn()
}

type nodeProbe struct {
	Count int `json:"count"`
}

func (*nodeProbe) EventType() string { return "test_node_probe" }

func init() { Register[nodeProbe]() }

// A SUCCESSOR'S ENVELOPE KEY SURVIVES A ROUND TRIP THROUGH THIS BUILD.
//
// For the length of a rolling upgrade this build decodes events a successor
// published — and re-publishes some of them, a parked delivery handed back to
// the broker being the ordinary case. This build has no field to decode a
// successor's new envelope key into, so the only thing standing between that
// field and oblivion is the rule that a key nobody owns is kept verbatim in
// Extra and written back out. `node` is the key this build owns that a
// successor must get back the same way, so the case carries both.
//
// And the no-origin half: an event published through a queue client built with
// no node — which only a test harness builds — names no node, and passing
// through this build must not grow the key.
func TestASuccessorsEnvelopeKeyRoundTripsThroughThisBuild(t *testing.T) {
	const published = `{"id":"6f1c3d2e-0000-4000-8000-000000000003","type":"test_node_probe",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"engine","trace_id":"","span_id":"",` +
		`"parent_span_id":"","delegation_depth":0,"parent_turn_id":"",` +
		`"node":"node-a","zone":"eu-1","count":3}`

	var seen Event
	if err := json.Unmarshal([]byte(published), &seen); err != nil {
		t.Fatalf("this build could not decode a successor's event: %v", err)
	}
	if got := seen.Extra["zone"]; !bytes.Equal(got, []byte(`"eu-1"`)) {
		t.Fatalf("this build did not keep the key it does not own: Extra = %v", seen.Extra)
	}
	if seen.Node != "node-a" {
		t.Errorf("the event names node %q, want the origin node-a", seen.Node)
	}
	if _, leaked := seen.Extra["node"]; leaked {
		t.Error("this build carried its own envelope key in Extra as well")
	}
	republished, err := json.Marshal(&seen)
	if err != nil {
		t.Fatalf("this build could not re-publish: %v", err)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(republished, &keys); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if got := keys["zone"]; !bytes.Equal(got, []byte(`"eu-1"`)) {
		t.Fatalf("this build re-published zone as %s, want the successor's own bytes "+
			`"eu-1"`+"\n%s", got, republished)
	}
	if got := keys["node"]; !bytes.Equal(got, []byte(`"node-a"`)) {
		t.Fatalf("this build re-published node as %s, want the origin's own bytes "+
			`"node-a"`+"\n%s", got, republished)
	}

	// The successor reads its own key back as its own, not as a field
	// somebody else left behind.
	successorBuild(t, func() {
		var back Event
		if err := json.Unmarshal(republished, &back); err != nil {
			t.Fatalf("the successor could not decode the re-published event: %v", err)
		}
		if _, leaked := back.Extra["zone"]; leaked {
			t.Error("the successor found its own envelope key in Extra")
		}
		if back.Node != "node-a" {
			t.Errorf("after this build handed it back, the event names node %q, want "+
				"the origin node-a", back.Node)
		}
	})

	// AND AN EVENT WITH NO ORIGIN: published through a queue client built
	// with no node, it names none, and passing through this build must not
	// invent the key.
	const unstamped = `{"id":"6f1c3d2e-0000-4000-8000-000000000004","type":"test_node_probe",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"engine","count":3}`
	var noOrigin Event
	if err := json.Unmarshal([]byte(unstamped), &noOrigin); err != nil {
		t.Fatalf("decode an event with no origin: %v", err)
	}
	if noOrigin.Node != "" {
		t.Errorf("an event that named no node decoded as from %q", noOrigin.Node)
	}
	out, err := json.Marshal(&noOrigin)
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
