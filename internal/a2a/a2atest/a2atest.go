// Package a2atest is the channel-store conformance suite.
//
// ONE implementation now — the channel record lives in the fleet's
// coordination store, and [a2a.CoordStore] is the whole of the translation —
// so what this suite covers is the seam rather than a second backend: the
// sentinel every unknown-channel path has to return, the idle listing the sweep
// decides over, and the participants a closed channel still has to be carrying
// when it comes back. The record's own semantics are certified
// against BOTH coordination backends by internal/coord/coordtest.
package a2atest

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
)

// Factory builds a fresh, empty store.
type Factory func(t *testing.T) a2a.Store

var base = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

// Run executes the suite.
//
// A SLICE, not a map. Every other conformance suite here dispatches from an
// ordered table, and this was the one that did not: a map's range order is
// randomised per run, so the same failure prints its siblings in a different
// order every time and a `-run` re-run of a flake reproduces a different
// interleaving than the one that failed. Each case builds its own store, so
// they are also genuinely independent and run in parallel.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"OpenThenGet", testOpenThenGet},
		{"UnknownChannel", testUnknownChannel},
		{"CountMessage", testCountMessage},
		{"Close", testClose},
		{"CloseIsIdempotentAndKeepsTheFirstTime", testCloseTwice},
		{"Idle", testIdle},
		{"IdleLeavesOutAClosedChannel", testIdleLeavesOutClosed},
		{"Purge", testPurge},
		{"GetReturnsACopy", testGetReturnsACopy},
		{"ReopenIsNotAReset", testReopenIsNotAReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newStore)
		})
	}
}

func open(t *testing.T, s a2a.Store, id, requester, target string, at time.Time) {
	t.Helper()
	if err := s.Open(t.Context(), a2a.Channel{
		ID: id, Requester: requester, Target: target, OpenedAt: at, LastAt: at,
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

func get(t *testing.T, s a2a.Store, id string) a2a.Channel {
	t.Helper()
	ch, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return ch
}

func testOpenThenGet(t *testing.T, newStore Factory) {
	s := newStore(t)
	open(t, s, "c1", "alice", "bob", base)
	ch := get(t, s, "c1")
	if ch.Requester != "alice" || ch.Target != "bob" {
		t.Errorf("participants = %v", ch.Participants())
	}
	if !ch.Open() {
		t.Error("a freshly opened channel reads as closed")
	}
	if !ch.OpenedAt.Equal(base) {
		t.Errorf("opened at %v, want %v — timestamps must survive the boundary", ch.OpenedAt, base)
	}
	if ch.Messages != 0 {
		t.Errorf("messages = %d, want 0", ch.Messages)
	}
}

func testUnknownChannel(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := t.Context()
	// All three paths must report the SAME sentinel. A reply that goes
	// nowhere is the failure the requester experiences as "they never
	// answered", so every route to it has to be distinguishable from a
	// transport error.
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, a2a.ErrNoChannel) {
		t.Errorf("Get: err = %v, want ErrNoChannel", err)
	}
	if _, err := s.Close(ctx, "missing", base); !errors.Is(err, a2a.ErrNoChannel) {
		t.Errorf("Close: err = %v, want ErrNoChannel", err)
	}
	if _, err := s.CountMessage(ctx, "missing", base); !errors.Is(err, a2a.ErrNoChannel) {
		t.Errorf("CountMessage: err = %v, want ErrNoChannel", err)
	}
}

func testCountMessage(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := t.Context()
	open(t, s, "c1", "alice", "bob", base)

	later := base.Add(time.Minute)
	n, err := s.CountMessage(ctx, "c1", later)
	if err != nil {
		t.Fatalf("CountMessage: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	if n, _ := s.CountMessage(ctx, "c1", later); n != 2 {
		t.Errorf("count = %d, want 2 — one ask and one answer is the whole protocol", n)
	}
	ch := get(t, s, "c1")
	if ch.Messages != 2 {
		t.Errorf("stored count = %d, want 2", ch.Messages)
	}
	// The activity stamp moves, which is what keeps an active channel out
	// of the idle sweep.
	if !ch.LastAt.Equal(later) {
		t.Errorf("last activity = %v, want %v", ch.LastAt, later)
	}
}

func testClose(t *testing.T, newStore Factory) {
	s := newStore(t)
	open(t, s, "c1", "alice", "bob", base)
	at := base.Add(time.Hour)
	ch, err := s.Close(t.Context(), "c1", at)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ch.Open() {
		t.Error("a closed channel reads as open")
	}
	if !ch.ClosedAt.Equal(at) {
		t.Errorf("closed at %v, want %v", ch.ClosedAt, at)
	}
	if again := get(t, s, "c1"); again.Open() {
		t.Error("the close was not persisted")
	}
}

func testCloseTwice(t *testing.T, newStore Factory) {
	s := newStore(t)
	open(t, s, "c1", "alice", "bob", base)
	first := base.Add(time.Hour)
	if _, err := s.Close(t.Context(), "c1", first); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Both parties may close, and the second is not a fault — but it must
	// not move the timestamp, because the first one is when it happened.
	ch, err := s.Close(t.Context(), "c1", first.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !ch.ClosedAt.Equal(first) {
		t.Errorf("closed at %v, want the first close at %v", ch.ClosedAt, first)
	}
}

func testIdle(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := t.Context()
	open(t, s, "stale", "alice", "bob", base)
	open(t, s, "fresh", "alice", "carol", base.Add(2*time.Hour))

	idle, err := s.Idle(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Idle: %v", err)
	}
	// WHICH channels are idle is the point: the sweep decides on each, and
	// one active after the cutoff is no candidate at all.
	if len(idle) != 1 || idle[0].ID != "stale" {
		t.Fatalf("idle %v, want just the stale one", ids(idle))
	}
	// AND WHO THEY ASKED: the sweep asks whether that seat still owes the
	// answer, and announces both parties of what it closes.
	if idle[0].Requester != "alice" || idle[0].Target != "bob" {
		t.Errorf("the listed channel lost its participants: %+v", idle[0])
	}
	// A listing, never a close.
	if !get(t, s, "stale").Open() {
		t.Error("listing the stale channel closed it")
	}
}

func testIdleLeavesOutClosed(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := t.Context()
	open(t, s, "c1", "alice", "bob", base)
	if _, err := s.Close(ctx, "c1", base.Add(time.Minute)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// An already-closed channel is no candidate: closed again, a second
	// close event for one channel draws two closes on a dashboard.
	idle, err := s.Idle(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Idle: %v", err)
	}
	if len(idle) != 0 {
		t.Errorf("Idle listed the closed %v", ids(idle))
	}
}

func testPurge(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := t.Context()
	open(t, s, "old", "alice", "bob", base)
	open(t, s, "recent", "alice", "carol", base)
	open(t, s, "still-open", "alice", "dave", base)
	if _, err := s.Close(ctx, "old", base); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Close(ctx, "recent", base.Add(10*time.Hour)); err != nil {
		t.Fatalf("Close: %v", err)
	}

	n, err := s.Purge(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	if _, err := s.Get(ctx, "old"); !errors.Is(err, a2a.ErrNoChannel) {
		t.Error("the old channel survived the purge")
	}
	if _, err := s.Get(ctx, "recent"); err != nil {
		t.Error("a recently-closed channel was purged")
	}
	// An OPEN channel is never purged however old, or a long-running ask
	// loses its authorization record while its answer is still in flight.
	if _, err := s.Get(ctx, "still-open"); err != nil {
		t.Error("an open channel was purged")
	}
}

func testGetReturnsACopy(t *testing.T, newStore Factory) {
	s := newStore(t)
	open(t, s, "c1", "alice", "bob", base)
	ch := get(t, s, "c1")
	ch.Requester = "mallory"
	ch.Messages = 99
	if again := get(t, s, "c1"); again.Requester != "alice" || again.Messages != 0 {
		t.Errorf("a caller mutating what it read reached the store: %+v", again)
	}
}

func testReopenIsNotAReset(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := t.Context()
	open(t, s, "c1", "alice", "bob", base)
	if _, err := s.CountMessage(ctx, "c1", base); err != nil {
		t.Fatalf("CountMessage: %v", err)
	}
	// A retried publish of one ask presents the same id. It must not wipe
	// the counter or the participants — the second Open is a duplicate, not
	// a new channel.
	if err := s.Open(ctx, a2a.Channel{
		ID: "c1", Requester: "mallory", Target: "eve",
		OpenedAt: base.Add(time.Hour), LastAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	ch := get(t, s, "c1")
	if ch.Requester != "alice" || ch.Target != "bob" || ch.Messages != 1 {
		t.Errorf("a duplicate Open rewrote the channel: %+v", ch)
	}
}

func ids(chs []a2a.Channel) []string {
	out := make([]string, 0, len(chs))
	for _, c := range chs {
		out = append(out, c.ID)
	}
	return out
}
