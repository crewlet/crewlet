package events_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// The pointer invariant: Event.Data holds the pointer form of a payload no
// matter how the event came to exist.
//
// It is pinned here rather than left to the constraint because the constraint
// only covers construction. What actually matters is that construction and
// DECODING agree — a mismatch there means the same handler reads its payload
// on one node and not on the node that published it, and nothing about the
// event looks wrong when it happens.

type sample struct {
	Work string `json:"work"`
}

func (sample) EventType() string { return "test.sample" }

func init() { events.Register[sample]() }

func TestDataIsThePointerFormEverywhere(t *testing.T) {
	t.Parallel()

	built := events.New(sample{Work: "w1"}, events.TraceContext{})
	if _, ok := events.DataAs[*sample](built); !ok {
		t.Fatalf("a constructed event carries %T, want *sample", built.Data)
	}

	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded events.Event
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, ok := events.DataAs[*sample](&decoded)
	if !ok {
		t.Fatalf("a decoded event carries %T, want *sample — construction and "+
			"decoding disagree about the payload's Go type", decoded.Data)
	}
	if got.Work != "w1" {
		t.Errorf("decoded work = %q, want w1", got.Work)
	}

	// The interface-typed path is the one that cannot be checked by the
	// compiler, so it is the one most likely to drift.
	relayed := events.NewFrom(sample{Work: "w2"}, events.TraceContext{})
	if _, ok := events.DataAs[*sample](relayed); !ok {
		t.Fatalf("NewFrom carried %T, want *sample", relayed.Data)
	}
	if events.NewFrom(nil, events.TraceContext{}) != nil {
		t.Error("NewFrom(nil) built an event with no body")
	}
}

func TestNewCopiesThePayload(t *testing.T) {
	t.Parallel()

	// A caller that reuses its struct — a loop building several events from
	// one variable — must not retroactively rewrite what it already
	// published.
	body := sample{Work: "before"}
	ev := events.New(body, events.TraceContext{})
	body.Work = "after"

	got, ok := events.DataAs[*sample](ev)
	if !ok {
		t.Fatalf("event carries %T", ev.Data)
	}
	if got.Work != "before" {
		t.Errorf("mutating the caller's struct changed the published event to %q", got.Work)
	}
}
