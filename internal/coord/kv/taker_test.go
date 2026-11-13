package kv

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// A LAPSED TAKER LEASE'S KEY IS REMOVED, AND NOTHING A PROCESS START LEAVES
// ACCUMULATES. Every engine process claims a taker lease of its own
// (coord.TakerResource), so whatever the store keeps per taker resource it
// keeps per process start. Its lease key lives in the duty bucket, whose age
// removes a key once its last write is that old — a released lease's
// tombstone, and the last renewal of a process that died — and its tokens come
// from ONE counter for the whole class in the ageless epochs bucket
// (coord.CounterResource). So three processes, one released and two lapsed,
// leave one counter record between them, no lease key once the age has
// passed, and the next process's first token is still above every token they
// held.
//
// ON A DUTY BUCKET OF ITS OWN WHOSE AGE IS A SECOND, since the store's own is
// three hours old (coord.MaxDutyTTL), which no case waits out — read first, so
// the case holds that the bucket a taker's key is written to is one with an
// age.
//
// Mutation: count a taker lease on a counter of its own, and the epochs bucket
// holds a record per process; keep the taker class in a bucket with no age,
// and the first check goes red.
func TestALapsedTakerLeasesKeyIsRemoved(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	js := jsOf(embeddedNATS(t))
	s := openStoreVia(t, js, InProcess, time.Minute)

	status, err := s.laneFor(coord.TakerResource("node-a:0")).kv.Status(ctx)
	if err != nil {
		t.Fatalf("the taker lane's bucket status: %v", err)
	}
	if status.TTL() != coord.MaxDutyTTL {
		t.Fatalf("a taker lease is kept in a bucket aged %v, want the duty bucket's %v: a bucket with no "+
			"age keeps every process's key for as long as the deployment runs", status.TTL(), coord.MaxDutyTTL)
	}

	const age = time.Second
	shortenDuties(ctx, t, js, s, age)
	takers := []string{"node-a:1", "node-a:2", "node-a:3"}
	held := make([]*coord.Lease, 0, len(takers))
	for _, incarnation := range takers {
		lease, refused, err := s.TryAcquire(ctx, coord.TakerResource(incarnation), coord.AcquireOptions{
			Owner: incarnation, TTL: age, Ungated: true,
		})
		if err != nil || lease == nil {
			t.Fatalf("claim %s's taker lease = (%v, %q, %v)", incarnation, lease, refused, err)
		}
		held = append(held, lease)
	}
	// THE FIRST PROCESS EXITS CLEANLY and gives its lease back; the other
	// two die holding theirs.
	if ok, err := s.Release(ctx, coord.TakerResource(takers[0]), takers[0], held[0].Epoch); err != nil || !ok {
		t.Fatalf("release %s's taker lease = (%v, %v)", takers[0], ok, err)
	}

	counters, err := keysUnder(ctx, js, s.counter.kv, jetstream.AllKeys)
	if err != nil {
		t.Fatalf("list the epochs bucket: %v", err)
	}
	if want := []string{encodeResource(coord.CounterResource(coord.TakerResource(takers[0])))}; !slices.Equal(counters,
		want) {
		t.Fatalf("the epochs bucket holds %v after three processes claimed, want the taker class's one "+
			"counter %v", counters, want)
	}

	// THE AGE REMOVES EVERY KEY: polled, since the broker reaps on a timer
	// of its own, and bounded far past the age.
	deadline := time.Now().Add(10 * age)
	for {
		left, err := keysUnder(ctx, js, s.duties.kv, jetstream.AllKeys)
		if err != nil {
			t.Fatalf("list the duty bucket: %v", err)
		}
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("taker keys %v outlived the duty bucket's %v age by nine times over", left, age)
		}
		time.Sleep(age / 10) //fixedwait:liveness
	}

	next, refused, err := s.TryAcquire(ctx, coord.TakerResource("node-a:4"), coord.AcquireOptions{
		Owner: "node-a:4", TTL: age, Ungated: true,
	})
	if err != nil || next == nil {
		t.Fatalf("the next process's taker claim = (%v, %q, %v)", next, refused, err)
	}
	for i, lease := range held {
		if next.Epoch <= lease.Epoch {
			t.Errorf("the next process claimed at %d once every key was gone, at or below %s's %d: the "+
				"class's counter went with the keys", next.Epoch, takers[i], lease.Epoch)
		}
	}
}

// shortenDuties points s's duty lane at a bucket of its own whose age is age,
// so a case can watch the age remove a key. The lane is the store's own in
// every other respect: the contract's ceiling, judged by each record's own
// deadline.
func shortenDuties(ctx context.Context, t *testing.T, js jetstream.JetStream, s *Store, age time.Duration) {
	t.Helper()
	bucket, facts, err := openBucket(ctx, js, false, jetstream.KeyValueConfig{
		Bucket: fmt.Sprintf("t%d%s", bucketSeq.Add(1), dutiesSuffix), TTL: age,
	})
	if err != nil {
		t.Fatalf("open a duty bucket aged %v: %v", age, err)
	}
	read, err := newLeaderReader(js, bucket)
	if err != nil {
		t.Fatalf("address the duty bucket's leader: %v", err)
	}
	s.duties = &lane{kv: settled(js, bucket, InProcess), read: read, stream: facts.stream, maxTTL: coord.MaxDutyTTL}
}
