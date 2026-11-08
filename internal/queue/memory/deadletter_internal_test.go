package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// steppedClock is a broker clock the case moves.
type steppedClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *steppedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *steppedClock) step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// A DEAD LETTER PAST THE RETENTION IS GONE, whether or not anybody read its
// subject in between, and a listing says when each entry goes — as the broker
// this twin stands for keeps its dead-letter stream by age.
//
// A twin that kept an entry for ever would certify a reader that replays, or
// counts down to, an entry the broker had already let go; one that let it go
// early, a wake lost that the broker still holds.
//
// Mutation: list letters without the horizon, and the expired entry is still
// there; let one go a beat early, and the younger is gone too.
func TestADeadLetterPastTheRetentionIsGone(t *testing.T) {
	t.Parallel()
	clk := &steppedClock{at: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	q := New()
	q.broker.mu.Lock()
	q.broker.now = clk.now
	q.broker.mu.Unlock()
	ctx := context.Background()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	subject := topics.DeadLetter("changefeed.work", "crewlet-tracker-feed")
	if err := q.KeepDeadLetter(ctx, subject, []byte("old")); err != nil {
		t.Fatalf("keep: %v", err)
	}
	clk.step(deadLetterRetention - time.Second)
	if err := q.KeepDeadLetter(ctx, subject, []byte("young")); err != nil {
		t.Fatalf("keep: %v", err)
	}
	kept, err := q.DeadLettersOn(ctx, subject)
	if err != nil || len(kept) != 2 {
		t.Fatalf("a second inside the retention the subject lists %d entries (%v), want both", len(kept), err)
	}
	if want := kept[0].StoredAt.Add(deadLetterRetention); !kept[0].Expires.Equal(want) {
		t.Errorf("the old entry expires at %v, want %v", kept[0].Expires, want)
	}

	clk.step(time.Second)
	kept, err = q.DeadLettersOn(ctx, subject)
	if err != nil || len(kept) != 1 || string(kept[0].Body) != "young" {
		t.Fatalf("at the old entry's retention the subject lists %d entries (%v), want the young one alone",
			len(kept), err)
	}
}
