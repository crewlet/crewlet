package a2a_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// staleListing is a coordination store whose open-channel listing is a
// SNAPSHOT, which is what a KV listing actually is: the keys are enumerated,
// then each is read, and anything can happen in between. This one models the
// ordinary case — a party closing its own channel while the sweep is walking
// the list.
type staleListing struct {
	coord.Channels
	closedBy time.Time
}

func (s *staleListing) OpenChannels(ctx context.Context) ([]coord.Channel, error) {
	open, err := s.Channels.OpenChannels(ctx)
	if err != nil {
		return nil, err
	}
	for _, ch := range open {
		if _, _, err := s.Channels.CloseChannel(ctx, ch.ID, s.closedBy); err != nil {
			return nil, err
		}
	}
	return open, nil
}

// The sweep reports what IT closed, not what it found open a moment ago. A
// channel the other party closed in between comes back carrying that party's
// instant, and reporting it would publish a second a2a_channel_closed for one
// channel — two closes on a dashboard, and a "nobody answered" alert for an
// ask that was answered.
func TestTheIdleSweepDoesNotReportACloseSomebodyElseMade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

	fleet := memory.NewFleet()
	store := a2a.NewCoordStore(&staleListing{Channels: fleet, closedBy: at.Add(time.Minute)})
	if err := store.Open(ctx, a2a.Channel{
		ID: "c1", Requester: "alice", Target: "bob", OpenedAt: at, LastAt: at,
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := &recorder{}
	sweeper, err := a2a.NewSweeper(store, rec, owedTo{}, func() time.Time { return at.Add(2 * time.Hour) })
	if err != nil {
		t.Fatalf("NewSweeper: %v", err)
	}

	closed, err := sweeper.SweepIdle(ctx, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}
	if closed != 0 || len(rec.sent) != 0 {
		t.Errorf("the sweep claimed %d closes it did not make, and published %v", closed, rec.topics())
	}
	// And the other party's instant is what survives, because the first
	// close is when it actually happened.
	ch, err := store.Get(ctx, "c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ch.ClosedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("closed at %v, want the other party's %v", ch.ClosedAt, at.Add(time.Minute))
	}
}
