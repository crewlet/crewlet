package usage_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// A RUNNING PUBLISHER PUBLISHES WHAT ARRIVES AFTER ITS FIRST FLUSH.
//
// The boot flush republishes today and yesterday whole, so a publisher that
// flushed once and stopped would pass every case that writes its records
// first. What only a live loop can do is find a phase recorded AFTER that
// flush — which is every phase a running node ever publishes — and it does so
// on the cadence it was given.
func TestARunningPublisherPublishesWhatArrivesAfterItsFirstFlush(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	marks := &countedMarks{DB: own}
	log := &loopback{t: t, into: own, node: "node-a"}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: marks, Log: log, NodeID: "node-a",
		Zone:  func() *time.Location { return santiago },
		Now:   func() time.Time { return at },
		Every: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// THE FIRST FLUSH HAS RUN — both days fingerprinted — before the phase
	// exists, so only a later tick can find it.
	waitFor(t, "the publisher's first flush", func() bool { return marks.n.Load() >= 2 })
	if log.sent() != 0 {
		t.Fatalf("a flush over an empty node published %d record(s)", log.sent())
	}
	(&events{t: t, db: own}).phase(at.Add(-time.Minute), "seat-1", "t1", "execute", "", 10, 1)
	waitFor(t, "a later tick to publish the phase", func() bool { return log.sent() > 0 })
}

// A NEGATIVE CADENCE IS REFUSED rather than handed to a ticker, which panics
// on it in a goroutine nobody is watching.
func TestANegativeCadenceIsRefused(t *testing.T) {
	t.Parallel()
	_, err := usage.NewPublisher(usage.PublisherDeps{
		Store: &countedMarks{}, Log: &loopback{t: t}, NodeID: "node-a",
		Zone:  func() *time.Location { return santiago },
		Every: -time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("a negative cadence built a publisher (%v)", err)
	}
}

// countedMarks counts the fingerprints a publisher takes — two per flush.
type countedMarks struct {
	*store.DB
	n atomic.Int64
}

func (c *countedMarks) UsageMark(ctx context.Context, w store.UsageWindow) (store.UsageMark, error) {
	defer c.n.Add(1)
	return c.DB.UsageMark(ctx, w)
}

// waitFor polls cond for ten seconds — a bound past any tick here by orders
// of magnitude, so only a loop that never ticks again reaches it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
