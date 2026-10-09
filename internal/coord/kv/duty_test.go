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
		{"learning", coord.MaxDutyTTL},
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

// trackerClaimTTL and largestBulkTTL are the TTLs the tracker claims its walks
// and its bulk admission at (tracker.ClaimTTL, and twice tracker.MaxBulkTasks
// rows at the one-row-a-second drain floor the admission projects from before
// a drain is measured), restated because this package must not import the
// tracker.
const (
	trackerClaimTTL = 60 * time.Second
	largestBulkTTL  = 2 * 64 * time.Second
)

// TestEveryCallerClaimIsHonouredBesideTheProductionSeatTTL is the duty
// regression's second half: a lease of a class the fleet does not own — the
// tracker's walk and bulk claims — is honoured at the TTL its caller sized it
// to, beside a seat lease bucket at the TTL a fleet really runs on.
//
// With every class but `worker:` written to the seat lease bucket, a store at
// the shipped 45 s refused all three, so on every embedded-kv fleet a
// cross-project move and a merge failed before their first append and a large
// bulk edit's admission was silently waved through. The contract suite runs
// its seat bucket at coordtest.LongTTL, which is longer than all of them.
func TestEveryCallerClaimIsHonouredBesideTheProductionSeatTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), productionSeatTTL)

	for _, claim := range []struct {
		resource string
		ttl      time.Duration
	}{
		{"move:t", trackerClaimTTL},
		{"merge:t", trackerClaimTTL},
		{"bulk:tracker", largestBulkTTL},
	} {
		lease, refused, err := s.TryAcquire(ctx, claim.resource, coord.AcquireOptions{
			Owner: "node-a/1", TTL: claim.ttl,
		})
		if err != nil || lease == nil {
			t.Fatalf("claim %s at %v beside a %v seat bucket = (%v, %q, %v)",
				claim.resource, claim.ttl, productionSeatTTL, lease, refused, err)
		}
		if ok, err := s.Renew(ctx, claim.resource, "node-a/1", lease.Epoch, claim.ttl); err != nil || !ok {
			t.Fatalf("renew %s at %v = (%v, %v)", claim.resource, claim.ttl, ok, err)
		}
	}

	// And nothing widened for the classes the seat TTL IS for: presence
	// keeps the seat bucket's ceiling as the seats do.
	_, _, err := s.TryAcquire(ctx, coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: productionSeatTTL + time.Second, Ungated: true,
	})
	if !errors.Is(err, errTTLTooLong) {
		t.Fatalf("a presence claim above the seat bucket's TTL = %v, want errTTLTooLong", err)
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

// Every lease outside seats and presence — a duty, and a class a caller owns —
// is written in the duty bucket and nowhere else, and every read of it reads
// that bucket: the seat lease bucket holds seats and presence alone, so a
// class listing of either never meets one, a listing of another class never
// opens the seat bucket, and the gate's view of that bucket takes in none of
// their writes.
func TestEveryLeaseOutsideSeatsAndPresenceLivesInTheDutyBucketAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		resource string
		class    coord.Class
		ungated  bool
	}{
		{coord.WorkerResource("scheduler"), coord.ClassWorker, true},
		{"move:task-1", coord.Class("move"), false},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openStore(t, embeddedNATS(t), time.Minute)

			lease, _, err := s.TryAcquire(ctx, tc.resource, coord.AcquireOptions{
				Owner: "node-a:1", TTL: 30 * time.Second, Ungated: tc.ungated,
			})
			if err != nil || lease == nil {
				t.Fatalf("claim %s = (%v, %v)", tc.resource, lease, err)
			}
			if got := rawLease(ctx, t, s.duties, tc.resource); got.Owner != "node-a:1" {
				t.Fatalf("duty bucket record owner of %s = %q, want node-a:1", tc.resource, got.Owner)
			}
			if _, err := s.leases.kv.Get(ctx, encodeResource(tc.resource)); !errors.Is(err, jetstream.ErrKeyNotFound) {
				t.Fatalf("seat lease bucket read of %s = %v, want ErrKeyNotFound", tc.resource, err)
			}
			if got, err := s.Get(ctx, tc.resource); err != nil || got == nil || got.Owner != "node-a:1" {
				t.Fatalf("Get(%s) = (%v, %v), want node-a:1's lease", tc.resource, got, err)
			}
			if live, err := s.ListLive(ctx, tc.class); err != nil || len(live) != 1 {
				t.Fatalf("ListLive(%s) = (%v, %v), want the one lease", tc.class, live, err)
			}
			if seats, err := s.ListLive(ctx, coord.ClassSeat); err != nil || len(seats) != 0 {
				t.Fatalf("ListLive(seat) = (%v, %v), want none", seats, err)
			}
			if owned, err := s.ListOwned(ctx, "node-a:1"); err != nil || len(owned) != 1 {
				t.Fatalf("ListOwned = (%v, %v), want the one lease", owned, err)
			}
		})
	}
}
