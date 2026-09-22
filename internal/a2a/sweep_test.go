package a2a_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
)

// TestTheSweepNeedsNoDirectory is the invariant the split exists for.
//
// The retention sweep runs on a node that has no directory to give — one is
// built from the running company, and the retention of a FLEET-WIDE record
// must not depend on whether this node has applied a configuration. While the
// sweep built a whole [a2a.Service], the directory becoming mandatory
// disarmed it: the construction failed, the engine logged a warning, and both
// channel jobs silently stopped existing.
func TestTheSweepNeedsNoDirectory(t *testing.T) {
	t.Parallel()
	st := a2a.NewCoordStore(memory.NewFleet())
	rec := &recorder{}
	sweeper, err := a2a.NewSweeper(st, rec, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("NewSweeper: %v", err)
	}

	// A CHANNEL OPENED BY THE ADDRESSING HALF and swept by this one, over
	// ONE store: the two capabilities are separate types, not separate
	// state. Its own publisher, so what the sweep announces is counted
	// without the open's records in the way.
	svc, err := a2a.New(st, &recorder{}, a2a.Options{
		Directory: dir{"bob": true},
		Now:       func() time.Time { return clock },
		NewID:     func() string { return "a2a-swept" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := svc.Open(ctx, a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "?",
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	swept, err := sweeper.SweepIdle(ctx, clock.Add(time.Hour))
	if err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d channels, want the one that was opened", swept)
	}
	if closes := decodeAll[*types.A2AChannelClosed](t, rec, "a2a_channel_closed"); len(closes) != 1 {
		t.Fatalf("close announcements = %d, want one — the sweep publishes "+
			"through its own queue, not the service's", len(closes))
	}
	// AND THE PURGE, on the same handle: a sweeper that could close and
	// not delete would leave the table growing with rows nothing reads.
	if _, err := sweeper.Purge(ctx, clock.Add(2*time.Hour)); err != nil {
		t.Fatalf("Purge: %v", err)
	}
}

// TestASweeperIsRefusedWithoutAStoreOrAPublisher — the two refusals [a2a.New]
// makes are the sweeper's own, because they are what it is built from.
func TestASweeperIsRefusedWithoutAStoreOrAPublisher(t *testing.T) {
	t.Parallel()
	if _, err := a2a.NewSweeper(nil, &recorder{}, nil); err == nil {
		t.Error("a sweeper with no channel store was accepted")
	}
	if _, err := a2a.NewSweeper(a2a.NewCoordStore(memory.NewFleet()), nil, nil); err == nil {
		t.Error("a sweeper with no publisher was accepted")
	}
	// A NIL CLOCK IS THE REAL PATH and is accepted, so the zero value of
	// the one optional argument is not a refusal.
	if _, err := a2a.NewSweeper(a2a.NewCoordStore(memory.NewFleet()), &recorder{}, nil); err != nil {
		t.Errorf("a sweeper with the default clock was refused: %v", err)
	}
}
