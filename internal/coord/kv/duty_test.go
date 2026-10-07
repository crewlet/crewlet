package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// productionSeatTTL is the seat lease TTL a fleet runs on unless
// coordination.lease_ttl_seconds says otherwise (seat.SeatLeaseTTL, restated
// because this package must not import the seat layer).
const productionSeatTTL = 45 * time.Second

// TestEveryEngineDutyIsHonouredBesideTheProductionSeatTTL is the regression the
// duty bucket exists for, at the TTLs that actually failed.
//
// With duties in the seat lease bucket, a store opened at the production 45 s
// refused all of these, and each refusal was one warning per tick on a fleet
// that never swept, never retired a mailbox, never reconciled an integration
// and never curated a skill. The contract suite's duty cases certify the rule
// at coordtest.LongTTL; this pins it at the seat TTL a fleet really runs.
func TestEveryEngineDutyIsHonouredBesideTheProductionSeatTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), productionSeatTTL)

	for _, duty := range []struct {
		name string
		ttl  time.Duration
	}{
		{"scheduler", 30 * time.Second},
		{"integration-reconcile", 4*time.Minute + 30*time.Second},
		{"setup-provision-github", 5 * time.Minute},
		{"maintenance", 45 * time.Minute},
		{"skill-curator", coord.MaxDutyTTL},
	} {
		resource := coord.WorkerResource(duty.name)
		lease, _, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: "node-a:1", TTL: duty.ttl, Ungated: true,
		})
		if err != nil || lease == nil {
			t.Fatalf("claim %s at %v beside a %v seat bucket = (%v, %v)",
				resource, duty.ttl, productionSeatTTL, lease, err)
		}
		if ok, err := s.Renew(ctx, resource, "node-a:1", lease.Epoch, duty.ttl); err != nil || !ok {
			t.Fatalf("renew %s at %v = (%v, %v)", resource, duty.ttl, ok, err)
		}
	}

	// The seat lease bucket keeps its own ceiling: moving duties out of it
	// widened nothing for seats.
	_, _, err := s.TryAcquire(ctx, coord.SeatResource("ceo"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: productionSeatTTL + time.Second,
	})
	if !errors.Is(err, errTTLTooLong) {
		t.Fatalf("a seat claim above the seat bucket's TTL = %v, want errTTLTooLong", err)
	}
}

// The duty bucket's age is raised to cover the longest duty and never lowered,
// because a lowered age would have the broker reap a peer's live long duty.
func TestTheDutyBucketAgeIsOnlyEverRaised(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	for _, tc := range []struct {
		name     string
		existing time.Duration
		want     time.Duration
	}{
		{"a younger bucket is raised to the ceiling", time.Hour, coord.MaxDutyTTL},
		{"an older bucket keeps its age", 2 * coord.MaxDutyTTL, 2 * coord.MaxDutyTTL},
		{"a bucket with no age keeps none", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))
			if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
				Bucket: prefix + dutiesSuffix, TTL: tc.existing,
			}); err != nil {
				t.Fatalf("pre-create the duty bucket: %v", err)
			}
			s, err := Open(ctx, jsOf(nc), Config{TTL: productionSeatTTL, BucketPrefix: prefix})
			if err != nil {
				t.Fatalf("Open over an existing duty bucket: %v", err)
			}
			status, err := s.duties.kv.Status(ctx)
			if err != nil {
				t.Fatalf("duty bucket status: %v", err)
			}
			if got := status.TTL(); got != tc.want {
				t.Fatalf("duty bucket age after Open = %v, want %v", got, tc.want)
			}
		})
	}

	// And a bucket nobody has made yet is made at the ceiling.
	s := openStore(t, nc, productionSeatTTL)
	status, err := s.duties.kv.Status(context.Background())
	if err != nil {
		t.Fatalf("duty bucket status: %v", err)
	}
	if got := status.TTL(); got != coord.MaxDutyTTL {
		t.Fatalf("a fresh duty bucket's age = %v, want coord.MaxDutyTTL (%v)", got, coord.MaxDutyTTL)
	}
}

// rawLease reads a record back the way the store wrote it.
func rawLease(ctx context.Context, t *testing.T, l *lane, resource string) leaseValue {
	t.Helper()
	kve, err := l.kv.Get(ctx, encodeResource(resource))
	if err != nil {
		t.Fatalf("read %s from %s: %v", resource, l.kv.Bucket(), err)
	}
	var v leaseValue
	if err := json.Unmarshal(kve.Value(), &v); err != nil {
		t.Fatalf("decode %s: %v", resource, err)
	}
	return v
}

// A duty is written in the duty bucket and nowhere else, and every read of it
// reads that bucket: the seat lease bucket holds seats and presence alone, so
// a class listing of either never meets a duty and a listing of duties never
// opens the seat bucket.
func TestADutyLivesInTheDutyBucketAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	duty := coord.WorkerResource("scheduler")

	lease, _, err := s.TryAcquire(ctx, duty, coord.AcquireOptions{
		Owner: "node-a:1", TTL: 30 * time.Second, Ungated: true,
	})
	if err != nil || lease == nil {
		t.Fatalf("duty claim = (%v, %v)", lease, err)
	}
	if got := rawLease(ctx, t, s.duties, duty); got.Owner != "node-a:1" {
		t.Fatalf("duty bucket record owner = %q, want node-a:1", got.Owner)
	}
	if _, err := s.leases.kv.Get(ctx, encodeResource(duty)); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("seat lease bucket read of a duty = %v, want ErrKeyNotFound", err)
	}
	if got, err := s.Get(ctx, duty); err != nil || got == nil || got.Owner != "node-a:1" {
		t.Fatalf("Get(duty) = (%v, %v), want node-a:1's lease", got, err)
	}
	if duties, err := s.ListLive(ctx, coord.ClassWorker); err != nil || len(duties) != 1 {
		t.Fatalf("ListLive(worker) = (%v, %v), want the one duty", duties, err)
	}
	if seats, err := s.ListLive(ctx, coord.ClassSeat); err != nil || len(seats) != 0 {
		t.Fatalf("ListLive(seat) = (%v, %v), want none", seats, err)
	}
	if owned, err := s.ListOwned(ctx, "node-a:1"); err != nil || len(owned) != 1 {
		t.Fatalf("ListOwned = (%v, %v), want the one duty", owned, err)
	}
}
