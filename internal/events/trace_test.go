package events

import (
	"bytes"
	"encoding/json"
	"testing"
)

type traceProbe struct {
	Note string `json:"note"`
}

func (*traceProbe) EventType() string { return "test_trace_probe" }

func init() { Register[traceProbe]() }

// THE SAMPLING DECISION CROSSES THE WIRE, and comes back through Trace as the
// trace it was published under — through both constructors, a JSON round trip
// and a requeue's Clone.
//
// It is what lets the node that continues a trace off the queue continue it as
// its root decided: before the envelope carried it, everything restored read as
// sampled. It is an ENVELOPE key, so it never lands in Extra beside its own
// field — a key carried twice is one that can disagree with itself.
//
// Mutations: drop the field from the envelope mirror, or from either
// constructor, or from Trace, and this goes red.
func TestTheSamplingDecisionSurvivesTheWire(t *testing.T) {
	published := TraceContext{
		TraceID:      "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:       "00f067aa0ba902b7",
		ParentSpanID: "b7ad6b7169203331",
		Unsampled:    true,
	}
	for name, ev := range map[string]*Event{
		"New":     New(traceProbe{Note: "n"}, published),
		"NewFrom": NewFrom(&traceProbe{Note: "n"}, published),
	} {
		t.Run(name, func(t *testing.T) {
			if got := ev.Trace(); got != published {
				t.Fatalf("constructed with %+v, reads back %+v", published, got)
			}
			if got := ev.Clone().Trace(); got != published {
				t.Errorf("a clone reads back %+v, want %+v", got, published)
			}
			wire, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(wire, &keys); err != nil {
				t.Fatalf("remap: %v", err)
			}
			if got := keys["trace_unsampled"]; !bytes.Equal(got, []byte("true")) {
				t.Errorf("trace_unsampled went out as %s, want true\n%s", got, wire)
			}
			var back Event
			if err := json.Unmarshal(wire, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := back.Trace(); got != published {
				t.Errorf("published %+v, arrived %+v", published, got)
			}
			if _, leaked := back.Extra["trace_unsampled"]; leaked {
				t.Error("the envelope's own key was carried in Extra as well")
			}
		})
	}
}

// A SAMPLED EVENT'S WIRE SHAPE IS THE ONE IT ALWAYS HAD, and an event that
// names no decision is sampled.
//
// The common case writes no key at all — the field is additive, so the bytes
// every reader of a sampled event sees are unchanged — and the reading of an
// absent key is "sampled", which is what an event meant before the decision
// travelled and what every root means under the default sampler.
//
// Mutation: drop `omitempty` from the envelope mirror and the key appears on
// every sampled event.
func TestASampledEventCarriesNoDecisionKey(t *testing.T) {
	wire, err := json.Marshal(New(traceProbe{}, TraceContext{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(wire, &keys); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if got, present := keys["trace_unsampled"]; present {
		t.Errorf("a sampled event went out with trace_unsampled=%s\n%s", got, wire)
	}

	const unnamed = `{"id":"6f1c3d2e-0000-4000-8000-000000000005","type":"test_trace_probe",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"engine",` +
		`"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7",` +
		`"parent_span_id":"","delegation_depth":0,"parent_turn_id":"","note":"n"}`
	var ev Event
	if err := json.Unmarshal([]byte(unnamed), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Trace().Unsampled {
		t.Error("an event that names no decision read as unsampled")
	}
}

// BENEATH KEEPS THE TRACE AND ITS DECISION and makes the span the parent: the
// trace of an event a relay or a coordinator publishes about something a span
// did, without opening one of its own. A hand-written copy of the two ids is
// what used to stand here, and a third field is what such a copy drops.
//
// Mutation: drop Unsampled from Beneath and the unsampled case goes red.
func TestBeneathKeepsTheTraceAndItsDecision(t *testing.T) {
	for _, unsampled := range []bool{false, true} {
		span := TraceContext{
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
			ParentSpanID: "b7ad6b7169203331", Unsampled: unsampled,
		}
		want := TraceContext{
			TraceID: span.TraceID, ParentSpanID: span.SpanID, Unsampled: unsampled,
		}
		if got := span.Beneath(); got != want {
			t.Errorf("beneath %+v is %+v, want %+v", span, got, want)
		}
	}
}
