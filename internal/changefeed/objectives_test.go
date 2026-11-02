package changefeed_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/objectives"
)

// timed is an objective meter that keeps the wakes it was handed.
type timed struct {
	mu     sync.Mutex
	stamps []time.Time
	other  []objectives.Name
}

func (j *timed) RecordSince(n objectives.Name, stamp time.Time, answered bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n != objectives.WakeLatency || !answered {
		j.other = append(j.other, n)
		return
	}
	j.stamps = append(j.stamps, stamp)
}

func (j *timed) wakes() []time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.stamps)
}

// EVERY PUBLISHED WAKE IS TIMED FROM ITS RECORD'S STORED INSTANT, AND NOTHING
// ELSE IS.
//
// A wake is derived from the durable record, so the broker's instant for that
// record is where the engine's share of a wake starts, on every node alike. A
// record that wakes nobody is a decision and no wake; one another node claimed
// was that node's to time; and a publish that failed is no event of its own —
// the redelivery that lands it is timed from the same stored instant, which is
// what carries the delay the failure cost.
//
// Mutation: time the wake before the publish, and the failed publish is timed
// twice; time it from the feed's own clock, and every stamp is the wrong one.
func TestEveryPublishedWakeIsTimedFromItsRecordsStoredInstant(t *testing.T) {
	t.Parallel()
	docs, pub, cl, tr := newEstate(), &capture{}, newClaims(), newProbe()
	judge := &timed{}
	feed, err := changefeed.New(changefeed.Options{
		Opener: docs, Publisher: pub, Claims: cl, Translator: tr, Objectives: judge,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- feed.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	stored := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return stored.Add(time.Duration(i) * time.Second) }

	docs.deliver(changefeed.Record{ID: "u1", Key: "item/u1", StoredAt: at(1), Payload: []byte(`{}`)})
	settle(t, func() bool { return pub.count() == 1 }, "the change never became a wake")

	tr.set(false, nil)
	docs.deliver(changefeed.Record{ID: "u2", Key: "item/u2", StoredAt: at(2), Payload: []byte(`{}`)})
	settle(t, func() bool { return tr.translations() == 2 }, "the quiet change was never read")
	tr.set(true, nil)

	if won, _ := cl.Claim(t.Context(), changefeed.ClaimKey("work", "u3"), time.Minute,
		time.Now()); !won {
		t.Fatal("the fixture could not hold u3's claim")
	}
	docs.deliver(changefeed.Record{ID: "u3", Key: "item/u3", StoredAt: at(3), Payload: []byte(`{}`)})
	settle(t, func() bool { return tr.translations() == 3 }, "the claimed change was never read")

	pub.breakWith(errors.New("the broker is down"))
	docs.deliver(changefeed.Record{ID: "u4", Key: "item/u4", StoredAt: at(4), Payload: []byte(`{}`)})
	settle(t, func() bool { return tr.translations() == 4 }, "the change was never read")
	pub.breakWith(nil)
	settle(t, func() bool { return pub.count() == 2 },
		"the naked change never came back after the broker recovered")

	settle(t, func() bool { return len(judge.wakes()) == 2 }, "")
	if got := judge.wakes(); !slices.Equal(got, []time.Time{at(1), at(4)}) {
		t.Errorf("wakes were timed from %v, want u1's and u4's stored instants and "+
			"nothing for the quiet, the claimed or the failed publish", got)
	}
	if len(judge.other) != 0 {
		t.Errorf("the feed recorded %v beside its wakes", judge.other)
	}
}
