package stream_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
)

// A DROP REACHES THE TAB, NOT ONLY THE LOG.
//
// [stream.QueueDepth] decides that a slow tab loses envelopes rather than
// stalling the publish path, and that is right — but a lost envelope is a
// state delta, so the tab is rendering something other than the truth. Until
// the count went on the wire the only evidence was a log line written when the
// client LEFT: an operator reading logs might see it, the reader being shown
// the wrong number never could. This asserts the frames that do land carry the
// running total, which is what lets a client say "you missed N updates" and
// refetch.
func TestAFrameThatLandsAfterADropCarriesTheRunningCount(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	c := stream.NewClient()
	h.Register(c)

	const overflow = stream.QueueDepth + 100
	for i := range overflow {
		h.Broadcast(stream.Push(stream.KindEvent, map[string]any{"n": i}, clock))
	}

	got := drain(c)
	if len(got) != stream.QueueDepth {
		t.Fatalf("queued = %d, want the depth of %d", len(got), stream.QueueDepth)
	}
	want := overflow - stream.QueueDepth
	if c.Dropped() != want {
		t.Fatalf("dropped = %d, want %d", c.Dropped(), want)
	}
	last := got[len(got)-1]
	if last.Dropped != want {
		t.Errorf("the newest frame reports %d drops, want %d — the count is on the "+
			"wire so a tab can be told it is behind", last.Dropped, want)
	}
	// MONOTONIC, so a client needs no state beyond the last value it saw.
	// It is also why the early frames still read zero: they were queued
	// before anything was dropped, and the number means "drops before this
	// frame", not "drops so far at the moment you read it".
	prev := 0
	for i, env := range got {
		if env.Dropped < prev {
			t.Fatalf("frame %d reports %d drops after a frame reporting %d; the "+
				"count must never go backwards", i, env.Dropped, prev)
		}
		prev = env.Dropped
	}
	if got[0].Dropped != 0 {
		t.Errorf("the oldest surviving frame reports %d drops; it was queued before "+
			"any drop happened", got[0].Dropped)
	}
}

// THE NOTICE COSTS NO QUEUE SLOT, which is why it is a field and not a frame
// of its own. A notice queued as an envelope would displace a real state frame
// in order to say a state frame was displaced, and under a burst the queue
// would fill with notices about notices. This pins that the queue after an
// overflow holds exactly the depth in STATE frames — none of them a notice.
func TestReportingADropDisplacesNoStateFrame(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	c := stream.NewClient()
	h.Register(c)

	for i := range stream.QueueDepth + 50 {
		h.Broadcast(stream.Push(stream.KindEvent, map[string]any{"n": i}, clock))
	}

	got := drain(c)
	if len(got) != stream.QueueDepth {
		t.Fatalf("queued = %d, want the depth of %d", len(got), stream.QueueDepth)
	}
	for i, env := range got {
		if env.Kind != stream.KindEvent {
			t.Fatalf("frame %d is a %q; a drop must not be reported as a frame of "+
				"its own, it rides on the frame that landed", i, env.Kind)
		}
	}
}

// A CONNECTION THAT KEEPS UP SENDS EXACTLY THE BYTES IT SENT BEFORE.
//
// The kinds are frozen because the dashboard is the compatibility reference,
// and a field added to every frame is the same kind of change if it is always
// present. Omitted at zero, a client that ignores it sees no difference at
// all — which is what makes this safe to add to a protocol in flight.
func TestADropFreeFrameCarriesNoDroppedFieldOnTheWire(t *testing.T) {
	t.Parallel()
	raw, err := stream.Encode(stream.Push(stream.KindEvent, map[string]any{"id": "e1"}, clock))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := frame["dropped"]; present {
		t.Errorf("a frame from a connection that lost nothing carries %q: %s",
			"dropped", raw)
	}

	raw, err = stream.Encode(stream.Envelope{Kind: stream.KindEvent, Dropped: 7})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	frame = nil
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if frame["dropped"] != float64(7) {
		t.Errorf("the wire field is %v, want 7: %s", frame["dropped"], raw)
	}
}

// ONE CLIENT'S COUNT IS ITS OWN. The envelope is broadcast by value, so a
// stamp is per connection — a tab that kept up must never be told it missed
// what another tab missed. The writer resyncs a connection on its own count,
// so a shared one would also send a full snapshot to every tab whenever any
// one of them fell behind.
func TestTheDropCountIsPerConnection(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	slow, quick := stream.NewClient(), stream.NewClient()
	h.Register(slow)

	for i := range stream.QueueDepth + 25 {
		h.Broadcast(stream.Push(stream.KindEvent, map[string]any{"n": i}, clock))
	}
	h.Register(quick)
	h.Broadcast(stream.Push(stream.KindEvent, map[string]any{"n": "after"}, clock))

	fresh := drain(quick)
	if len(fresh) != 1 {
		t.Fatalf("the fresh client received %d frames, want 1", len(fresh))
	}
	if fresh[0].Dropped != 0 {
		t.Errorf("a client that lost nothing was told it lost %d", fresh[0].Dropped)
	}
	// The slow client's queue was still full when that last frame went out,
	// so it displaced one more — 25 from the burst plus that one.
	behind := drain(slow)
	if last := behind[len(behind)-1]; last.Dropped != slow.Dropped() || last.Dropped != 26 {
		t.Errorf("the slow client's newest frame reports %d drops; the client counted "+
			"%d and the burst plus the last broadcast is 26",
			last.Dropped, slow.Dropped())
	}
}

// A QUERY ANSWER IS DROPPABLE, AND NO SNAPSHOT BRINGS IT BACK.
//
// [stream.KindResult] and [stream.KindError] travel through the very same
// per-client queue as the pushes, under the same drop-oldest rule — so the
// queue does NOT hold only state deltas, and the recoverability clause on
// [stream.Envelope]'s Dropped field cannot say that everything it loses is in
// the projection. A push is a delta the snapshot subsumes; an answer is
// correlated by an id the client minted, is in no projection, and is
// recovered only by asking again — which the dashboard reaches through its
// own answer timeout, the client-only `timeout` code this package's
// vocabulary test names.
//
// This pins the half that makes the distinction necessary: the answer really
// can be the frame that goes.
func TestAQueryAnswerIsDroppedByTheSameRuleAsAPush(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	c := stream.NewClient()
	h.Register(c)

	// The answer is queued FIRST, so it is the oldest — which is the one
	// the rule takes.
	h.Broadcast(stream.Envelope{Kind: stream.KindResult, ID: 7, What: "work_items"})
	for i := range stream.QueueDepth {
		h.Broadcast(stream.Push(stream.KindEvent, map[string]any{"n": i}, clock))
	}

	if c.Dropped() != 1 {
		t.Fatalf("dropped = %d, want exactly the one frame the depth could not hold",
			c.Dropped())
	}
	for _, env := range drain(c) {
		if env.Kind == stream.KindResult {
			t.Fatalf("the query answer survived; this test asserts nothing unless "+
				"the answer is the frame the queue gave up (id %d)", env.ID)
		}
	}
}
