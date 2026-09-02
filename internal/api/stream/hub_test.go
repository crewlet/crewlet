package stream_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/stream"
)

var clock = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

// drain reads everything currently queued for a client.
func drain(c *stream.Client) []stream.Envelope {
	var out []stream.Envelope
	for {
		select {
		case env, ok := <-c.Out():
			if !ok {
				return out
			}
			out = append(out, env)
		default:
			return out
		}
	}
}

func TestABroadcastReachesEveryClient(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	a, b := stream.NewClient(), stream.NewClient()
	h.Register(a)
	h.Register(b)
	if got := h.Clients(); got != 2 {
		t.Fatalf("clients = %d, want 2", got)
	}

	h.Broadcast(stream.Push(stream.KindEvent, map[string]any{"id": "e1"}, clock))

	for name, c := range map[string]*stream.Client{"a": a, "b": b} {
		got := drain(c)
		if len(got) != 1 || got[0].Kind != stream.KindEvent {
			t.Errorf("%s received %+v", name, got)
		}
	}
}

func TestAnUnregisteredClientStopsReceiving(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	c := stream.NewClient()
	h.Register(c)
	h.Unregister(c)

	h.Broadcast(stream.Push(stream.KindEvent, nil, clock))
	if got := drain(c); len(got) != 0 {
		t.Errorf("an unregistered client received %+v", got)
	}
	if h.Clients() != 0 {
		t.Error("the hub still counts an unregistered client")
	}
}

func TestASlowClientLosesItsOldestEnvelope(t *testing.T) {
	t.Parallel()
	// Drop-OLDEST past the queue depth, never block. What a dashboard
	// shows is the CURRENT state, so the newest envelope is the one that
	// makes the screen right — dropping it to keep an older one would
	// leave the tab further behind than doing nothing.
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
	// The NEWEST survived.
	last, _ := got[len(got)-1].Data.(map[string]any)
	if last["n"] != overflow-1 {
		t.Errorf("newest queued = %v, want %d", last["n"], overflow-1)
	}
	// The OLDEST went.
	first, _ := got[0].Data.(map[string]any)
	if first["n"] == 0 {
		t.Error("the oldest envelope survived an overflow")
	}
	if c.Dropped() != overflow-stream.QueueDepth {
		t.Errorf("dropped = %d, want %d", c.Dropped(), overflow-stream.QueueDepth)
	}
}

func TestOneSlowClientDoesNotStallAnother(t *testing.T) {
	t.Parallel()
	// The whole reason the queue drops rather than blocks: the ingest path
	// is shared by every client, so one tab that stopped reading would
	// otherwise stop the fan-out for all of them.
	h := stream.NewHub()
	slow, fast := stream.NewClient(), stream.NewClient()
	h.Register(slow)
	h.Register(fast)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range stream.QueueDepth * 3 {
			h.Broadcast(stream.Push(stream.KindEvent, nil, clock))
			// The fast client keeps up; the slow one never reads.
			<-fast.Out()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a client that stopped reading stalled the broadcast path")
	}
	if slow.Dropped() == 0 {
		t.Error("the slow client lost nothing, so it was not actually behind")
	}
}

func TestABroadcastToNoClientsIsHarmless(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	h.Broadcast(stream.Push(stream.KindHealth, nil, clock))
	if h.Clients() != 0 {
		t.Error("broadcasting invented a client")
	}
}

func TestUnregisteringReleasesTheClientsWriter(t *testing.T) {
	t.Parallel()
	// A transport's writer goroutine ranges over the client's channel and
	// exits when it closes. Removing a client from the fan-out without
	// closing it leaves that goroutine parked for the life of the process,
	// one per tab that ever connected.
	h := stream.NewHub()
	c := stream.NewClient()
	h.Register(c)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for range c.Out() {
		}
	}()

	h.Unregister(c)
	select {
	case <-writerDone:
	case <-time.After(5 * time.Second):
		t.Error("unregistering left the client's writer goroutine parked")
	}
}

func TestClosingAClientTwiceIsSafe(t *testing.T) {
	t.Parallel()
	// Both ends reach it: the transport closes when the socket dies, and
	// the hub closes when it is shutting down.
	h := stream.NewHub()
	c := stream.NewClient()
	h.Register(c)
	h.Unregister(c)
	h.Unregister(c)
	c.Close()
}

func TestABroadcastAfterCloseDoesNotPanic(t *testing.T) {
	t.Parallel()
	// A send on a closed channel panics, and the closing end is not the
	// broadcasting one — so the race is real rather than theoretical.
	c := stream.NewClient()
	h := stream.NewHub()
	h.Register(c)
	c.Close()
	h.Broadcast(stream.Push(stream.KindEvent, nil, clock))
}

func TestClosingTheHubDisconnectsEveryone(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	a, b := stream.NewClient(), stream.NewClient()
	h.Register(a)
	h.Register(b)
	h.Close()

	if h.Clients() != 0 {
		t.Errorf("clients = %d after Close", h.Clients())
	}
	for name, c := range map[string]*stream.Client{"a": a, "b": b} {
		if _, open := <-c.Out(); open {
			t.Errorf("%s's channel is still open", name)
		}
	}
}

func TestTheHubIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()
	// Register, unregister and broadcast all arrive from different
	// goroutines in production: a tab connects while an event is being
	// fanned out and another tab is going away.
	h := stream.NewHub()
	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			for range 100 {
				c := stream.NewClient()
				h.Register(c)
				go drain(c)
				h.Unregister(c)
			}
		})
	}
	wg.Go(func() {
		for range 500 {
			h.Broadcast(stream.Push(stream.KindEvent, nil, clock))
		}
	})
	wg.Wait()
	h.Close()
}

// --- the wire shape ------------------------------------------------------ //

func TestAPushEncodesAsTheClientExpects(t *testing.T) {
	t.Parallel()
	// The dashboard ships unchanged and is the compatibility reference, so
	// the frame is a contract rather than an implementation detail. A push
	// carries kind, data and ts, and NOT the query-answer fields.
	raw, err := stream.Encode(stream.Push(stream.KindAgents, []string{"ceo"}, clock))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["kind"] != stream.KindAgents {
		t.Errorf("kind = %v", got["kind"])
	}
	if got["ts"] != "2026-06-14T12:00:00Z" {
		t.Errorf("ts = %v", got["ts"])
	}
	for _, absent := range []string{"id", "what", "error"} {
		if _, present := got[absent]; present {
			t.Errorf("a push carried the query-answer field %q", absent)
		}
	}
}

func TestAQueryAnswerCarriesItsCorrelationID(t *testing.T) {
	t.Parallel()
	// id is client-minted and correlates the answer. A result that lost it
	// is an answer to a question the client can no longer identify.
	raw, err := stream.Encode(stream.Envelope{
		Kind: stream.KindResult, ID: 7, What: "agent", Data: map[string]any{"role": "CEO"},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != float64(7) || got["what"] != "agent" {
		t.Errorf("answer = %v", got)
	}
}

func TestAnErrorCarriesACodeNotProse(t *testing.T) {
	t.Parallel()
	// The client switches on the value. Prose there would make every
	// message a new case nobody handles.
	for _, code := range []string{"unknown_query", "unauthorized", "query_failed"} {
		raw, err := stream.Encode(stream.Envelope{
			Kind: stream.KindError, ID: 1, What: "config", Error: code,
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got["error"] != code {
			t.Errorf("error = %v, want %q", got["error"], code)
		}
		if _, present := got["data"]; present {
			t.Error("an error frame carried data")
		}
	}
}
