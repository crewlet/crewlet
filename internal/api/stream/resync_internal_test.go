package stream

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// written is a frameWriter that keeps what it was handed, in order, and can
// act on a frame as it goes out — which is how a case puts traffic behind a
// resync without a second goroutine.
type written struct {
	frames  []Envelope
	onWrite func(Envelope)
}

func (w *written) write(_ context.Context, env Envelope) error {
	w.frames = append(w.frames, env)
	if w.onWrite != nil {
		w.onWrite(env)
	}
	return nil
}

func (w *written) kinds(kind string) int {
	n := 0
	for _, env := range w.frames {
		if env.Kind == kind {
			n++
		}
	}
	return n
}

// countingSnapshot is a snapshot builder that counts how often it was asked.
func countingSnapshot(n *int) func() Envelope {
	return func() Envelope {
		*n++
		return Envelope{Kind: KindSnapshot, Data: map[string]any{"fresh": *n}}
	}
}

// A TAB THAT FELL BEHIND IS REPAIRED, NOT ONLY TOLD.
//
// A dropped state push leaves the tab drawing something other than the truth,
// and the running count on the next frame only helps a client that reads it.
// The writer repairs it itself: the frame that reveals the loss is followed by
// a snapshot built then. What the snapshot supersedes is discarded rather than
// delivered after it, and nothing else is — an `event` push carries a payload
// no snapshot holds, and an answer is held by no projection at all.
func TestATabThatFellBehindIsSentAFreshSnapshot(t *testing.T) {
	t.Parallel()
	c := NewClient()
	const overflow = 10
	for i := range QueueDepth + overflow {
		switch i % 3 {
		case 0:
			c.send(Envelope{Kind: KindAgents, Data: i})
		case 1:
			c.send(Envelope{Kind: KindEvent, Data: i})
		default:
			c.send(Envelope{Kind: KindResult, ID: int64(i), What: "q"})
		}
	}
	queued := drainCopy(c)
	c.Close()

	var w written
	builds := 0
	writeLoop(t.Context(), w.write, c, countingSnapshot(&builds))

	if builds != 1 || w.kinds(KindSnapshot) != 1 {
		t.Fatalf("snapshots built %d, written %d; want exactly one resync for one "+
			"burst of drops", builds, w.kinds(KindSnapshot))
	}
	last := w.frames[len(w.frames)-1]
	if last.Kind != KindSnapshot || last.Dropped != overflow {
		t.Errorf("the last frame is a %q reporting %d drops; want the snapshot, "+
			"reporting all %d", last.Kind, last.Dropped, overflow)
	}
	// NOTHING THE SNAPSHOT DOES NOT CARRY WAS DISCARDED: every event push
	// and every answer that was queued went out.
	for _, env := range queued {
		if supersededBySnapshot[env.Kind] {
			continue
		}
		if !slices.ContainsFunc(w.frames, func(got Envelope) bool {
			return got.Kind == env.Kind && got.ID == env.ID && got.Data == env.Data
		}) {
			t.Errorf("a queued %q frame (id %d, data %v) never went out; a resync "+
				"may only discard what the snapshot carries", env.Kind, env.ID, env.Data)
		}
	}
	// AND WHAT IT DOES CARRY WAS. The writer learns of the loss after the
	// first frame it writes — the queue was already full behind it — so
	// every superseded frame after that one is discarded, not delivered.
	for i, env := range w.frames[1 : len(w.frames)-1] {
		if supersededBySnapshot[env.Kind] {
			t.Errorf("frame %d, a %q push, went out after the loss was known; the "+
				"snapshot supersedes it", i+1, env.Kind)
		}
	}
	// THE COUNT NEVER GOES DOWN, in the order frames reach the client.
	prev := 0
	for i, env := range w.frames {
		if env.Dropped < prev {
			t.Fatalf("frame %d (%s) reports %d drops after one reporting %d",
				i, env.Kind, env.Dropped, prev)
		}
		prev = env.Dropped
	}
}

// drainCopy returns what is queued without consuming it, so a case can
// compare what went out against what was waiting.
func drainCopy(c *Client) []Envelope {
	var out []Envelope
	for range len(c.out) {
		env := <-c.out
		out = append(out, env)
	}
	for _, env := range out {
		c.out <- env
	}
	return out
}

// A CONNECTION THAT KEEPS UP IS NEVER RESYNCED, and one resync covers the
// drops it was built after: only a NEW drop earns another.
func TestOnlyANewDropEarnsAnotherResync(t *testing.T) {
	t.Parallel()

	keptUp := NewClient()
	for i := range 5 {
		keptUp.send(Envelope{Kind: KindAgents, Data: i})
	}
	keptUp.Close()
	var w written
	builds := 0
	writeLoop(t.Context(), w.write, keptUp, countingSnapshot(&builds))
	if builds != 0 {
		t.Errorf("a connection that dropped nothing was sent %d snapshots", builds)
	}

	for _, tc := range []struct {
		name    string
		after   int // frames queued once the first snapshot has gone out
		resyncs int
	}{
		{"traffic with no new drop", 3, 1},
		{"a second overflow", QueueDepth + 5, 2},
	} {
		c := NewClient()
		for i := range QueueDepth + 10 {
			c.send(Envelope{Kind: KindAgents, Data: i})
		}
		var w written
		builds := 0
		queuedMore := false
		w.onWrite = func(env Envelope) {
			if env.Kind != KindSnapshot || queuedMore {
				return
			}
			queuedMore = true
			for i := range tc.after {
				c.send(Envelope{Kind: KindEvent, Data: i})
			}
			c.Close()
		}
		writeLoop(t.Context(), w.write, c, countingSnapshot(&builds))
		if builds != tc.resyncs {
			t.Errorf("%s: %d snapshots built, want %d", tc.name, builds, tc.resyncs)
		}
	}
}

// EVERY KIND A RESYNC DISCARDS IS CARRIED BY THE SNAPSHOT.
//
// Discarding a queued push is safe only because the snapshot built after it
// holds what it said. A kind added to the set whose content the snapshot does
// not carry would turn a resync into a loss of exactly the frames that were
// NOT dropped, so each is held here to the snapshot key that carries it.
func TestEverySupersededKindIsCarriedByTheSnapshot(t *testing.T) {
	t.Parallel()
	carriedAs := map[string]string{
		KindSnapshot:  "", // the snapshot itself
		KindAgents:    "agents",
		KindSeats:     "agents",
		KindSandboxes: "sandboxes",
		KindTokens:    "tokens",
		KindBudget:    "budget",
		KindSchedules: "schedules",
		KindOrg:       "org",
		KindTools:     "tools",
		KindHealth:    "health",
	}
	svc, err := NewService(livestate.New(), Options{
		Health:    func() Health { return Health{Status: "ok"} },
		Handles:   func() map[string]string { return map[string]string{} },
		Roster:    func() []map[string]any { return nil },
		Org:       func() any { return map[string]any{} },
		Tools:     func() []map[string]any { return nil },
		Schedules: func() any { return []any{} },
		Now:       func() time.Time { return time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	snapshot := svc.Snapshot()
	for kind := range supersededBySnapshot {
		key, known := carriedAs[kind]
		if !known {
			t.Errorf("%q is discarded by a resync and this case does not know which "+
				"snapshot key carries it; name it here, or take it out of the set", kind)
			continue
		}
		if key == "" {
			continue
		}
		if _, present := snapshot[key]; !present {
			t.Errorf("%q is discarded by a resync, and the snapshot carries no %q", kind, key)
		}
	}
	if supersededBySnapshot[KindEvent] {
		t.Error("an event push is discarded by a resync; its payload is in no snapshot")
	}
}
