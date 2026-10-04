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
		lease, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
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
	_, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
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
			s, err := Open(ctx, nc, Config{TTL: productionSeatTTL, BucketPrefix: prefix})
			if err != nil {
				t.Fatalf("Open over an existing duty bucket: %v", err)
			}
			status, err := statusOf(ctx, s.duties.kv)
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
	status, err := statusOf(context.Background(), s.duties.kv)
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
