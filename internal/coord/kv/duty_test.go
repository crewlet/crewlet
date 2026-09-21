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

// putOlderBuildRecord writes a lease record exactly as a build that predates
// the duty bucket wrote one: into the seat lease bucket, at that bucket's TTL,
// with no layout field at all.
//
// Through [encodeResource], because the key grammar is OLDER than the duty
// bucket: a build without the duty lane still segmented a resource into its
// key, so the record an upgrade actually meets is spelled `worker.scheduler`.
// Spelled with [encodeKey] instead it lands on `worker=3Ascheduler`, one
// segment where every lease key has two, which certifies the rolling-upgrade
// rule against a record no build ever wrote.
//
// The wrong spelling fails ASYMMETRICALLY, which is why it is worth naming
// here rather than left to whoever reads the failure. A one-segment key still
// decodes, because [decodeResource] splits on the separator, finds the one
// part and unescapes it back to `worker:scheduler`. So the full-bucket scan
// the older-build gate runs finds the record and the gate behaves correctly.
// What misses it is every read that names a key or a class: [Store.Get] looks
// under `worker.scheduler` and is told the key does not exist, and a class
// listing filters on `worker.>`, which one segment does not match. The symptom
// is then a gate that works beside reads that see nothing, which reads like a
// liveness bug and is only ever a spelling one.
func putOlderBuildRecord(ctx context.Context, t *testing.T, s *Store, resource, owner string) uint64 {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"resource": resource,
		"owner":    owner,
		"epoch":    1,
		"ttl_ns":   int64(s.ttl),
		"protocol": coord.ProtocolVersion,
	})
	if err != nil {
		t.Fatalf("encode the older build's record: %v", err)
	}
	rev, err := s.leases.kv.Put(ctx, encodeResource(resource), data)
	if err != nil {
		t.Fatalf("write the older build's record for %s: %v", resource, err)
	}
	return rev
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

// TestADutyWaitsWhileANodeOfAnOlderBuildIsLive is the rolling-upgrade rule.
//
// The older node locks duties in the seat lease bucket and cannot see the duty
// bucket, so a newer node claiming there beside it would give the fleet two
// holders of one duty. The newer node waits until the older build's last
// record is gone; seats are not held back by it.
func TestADutyWaitsWhileANodeOfAnOlderBuildIsLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	duty := coord.WorkerResource("scheduler")

	// This build's own records never gate its duties: every record it
	// writes carries the layout, presence included.
	if _, err := s.TryAcquire(ctx, coord.NodeResource("new"), coord.AcquireOptions{
		Owner: "new:1", TTL: time.Minute, Ungated: true,
	}); err != nil {
		t.Fatalf("presence claim: %v", err)
	}
	if got := rawLease(ctx, t, s.leases, coord.NodeResource("new")).Layout; got != layoutDutyLane {
		t.Fatalf("this build's presence record carries layout %d, want %d", got, layoutDutyLane)
	}

	putOlderBuildRecord(ctx, t, s, coord.NodeResource("old"), "old:1")

	lease, err := s.TryAcquire(ctx, duty, coord.AcquireOptions{
		Owner: "new:1", TTL: 30 * time.Second, Ungated: true,
	})
	if err != nil || lease != nil {
		t.Fatalf("a duty claim beside a live older build = (%v, %v), want the definite refusal (nil, nil)",
			lease, err)
	}
	gated, err := s.TryAcquire(ctx, duty, coord.AcquireOptions{Owner: "new:1", TTL: 30 * time.Second})
	if err != nil || gated != nil {
		t.Fatalf("a gated duty claim beside a live older build = (%v, %v), want (nil, nil)", gated, err)
	}
	// Refused BEFORE any write, not claimed and given back: a claim that
	// holds the duty for even one round trip is an overlap with the older
	// node, and the re-check after a write is only the backstop for an
	// older node that appeared mid-claim.
	if kve, err := s.duties.kv.Get(ctx, encodeResource(duty)); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("a refused duty claim wrote the duty bucket: (%v, %v)", kve, err)
	}
	// VISIBLY: to a duty helper the refusal reads like a peer holding the
	// duty, so the store itself has to say the node is waiting.
	if !s.dutiesWaiting.Load() {
		t.Fatal("the store refused duties for an older build and did not report that it is waiting")
	}
	if seat, err := s.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
		Owner: "new:1", TTL: time.Minute,
	}); err != nil || seat == nil {
		t.Fatalf("a seat claim beside a live older build = (%v, %v); the layout gates duties only",
			seat, err)
	}

	// The older node stops: its presence is released in place, as that
	// build's Release writes it.
	tomb, err := json.Marshal(map[string]any{
		"resource": coord.NodeResource("old"), "owner": "", "epoch": 1,
		"ttl_ns": int64(s.ttl), "protocol": coord.ProtocolVersion,
	})
	if err != nil {
		t.Fatalf("encode tombstone: %v", err)
	}
	if _, err := s.leases.kv.Put(ctx, encodeResource(coord.NodeResource("old")), tomb); err != nil {
		t.Fatalf("release the older node's presence: %v", err)
	}
	taken, err := s.TryAcquire(ctx, duty, coord.AcquireOptions{
		Owner: "new:1", TTL: 30 * time.Second, Ungated: true,
	})
	if err != nil || taken == nil {
		t.Fatalf("a duty claim once the older build is gone = (%v, %v)", taken, err)
	}
	if got := rawLease(ctx, t, s.duties, duty); got.Layout != layoutDutyLane || got.Owner != "new:1" {
		t.Fatalf("the duty record is %+v, want this build's, in the duty bucket", got)
	}
	if s.dutiesWaiting.Load() {
		t.Fatal("duties resumed and the store still reports waiting for an older build")
	}
}

// A holder that finds an older build live stops at its next tick: its
// re-claim is refused rather than renewed, because the older node may already
// hold the same duty in the bucket it reads.
func TestADutyHolderStopsWhenAnOlderBuildAppears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	duty := coord.WorkerResource("maintenance")

	held, err := s.TryAcquire(ctx, duty, coord.AcquireOptions{
		Owner: "new:1", TTL: 45 * time.Minute, Ungated: true,
	})
	if err != nil || held == nil {
		t.Fatalf("claim = (%v, %v)", held, err)
	}
	putOlderBuildRecord(ctx, t, s, coord.NodeResource("old"), "old:1")
	before, err := s.duties.kv.Get(ctx, encodeResource(duty))
	if err != nil {
		t.Fatalf("read the held duty: %v", err)
	}

	again, err := s.TryAcquire(ctx, duty, coord.AcquireOptions{
		Owner: "new:1", TTL: 45 * time.Minute, Ungated: true,
	})
	if err != nil || again != nil {
		t.Fatalf("the holder's re-claim beside an older build = (%v, %v), want (nil, nil)", again, err)
	}
	// Refused without renewing: a renew would extend the very overlap the
	// refusal exists to end.
	after, err := s.duties.kv.Get(ctx, encodeResource(duty))
	if err != nil {
		t.Fatalf("read the duty after the refusal: %v", err)
	}
	if after.Revision() != before.Revision() {
		t.Fatalf("the refused re-claim rewrote the duty (revision %d -> %d)", before.Revision(), after.Revision())
	}
}

// The re-check after a claim: an older build that became visible between the
// check and the write makes a NEW claim give the duty straight back, and
// leaves a re-claim of a duty already held in place (the refusal still stops
// its next tick).
func TestTheLayoutReCheckGivesBackOnlyANewClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		fresh    bool
		stillOwn bool
	}{
		{"a new claim is released", true, false},
		{"a re-claim is kept", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t, embeddedNATS(t), time.Minute)
			duty := coord.WorkerResource("scheduler")
			opts := coord.AcquireOptions{Owner: "new:1", TTL: 30 * time.Second, Ungated: true}
			lease, err := s.TryAcquire(ctx, duty, opts)
			if err != nil || lease == nil {
				t.Fatalf("claim = (%v, %v)", lease, err)
			}
			putOlderBuildRecord(ctx, t, s, coord.NodeResource("old"), "old:1")

			want := leaseValue{Resource: duty, Owner: "new:1", Epoch: lease.Epoch}
			got, err := s.settle(ctx, s.duties, duty, want, opts, coord.ProtocolVersion, tc.fresh)
			if err != nil || got != nil {
				t.Fatalf("settle beside an older build = (%v, %v), want (nil, nil)", got, err)
			}
			current, err := s.Get(ctx, duty)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if own := current != nil && current.Owner == "new:1"; own != tc.stillOwn {
				t.Fatalf("after the re-check the duty reads %v, want still held = %v", current, tc.stillOwn)
			}
		})
	}
}

// While the older build holds a duty, the fleet view says so. Reporting it free
// would show an operator an unrun duty that another node is running.
func TestAnOlderBuildsDutyIsVisibleInEveryRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	duty := coord.WorkerResource("scheduler")
	putOlderBuildRecord(ctx, t, s, duty, "old:1")

	got, err := s.Get(ctx, duty)
	if err != nil || got == nil || got.Owner != "old:1" {
		t.Fatalf("Get(%s) = (%v, %v), want the older build's holder", duty, got, err)
	}
	// The two reads that can reach it. There is no all-classes listing to
	// ask as well: a class is what ListLive takes, and the empty one is
	// refused rather than read as "every class" (coordtest's
	// a_class_that_cannot_address_a_key_is_refused certifies that on both
	// backends).
	for name, read := range map[string]func() ([]coord.Lease, error){
		"ListLive(worker)": func() ([]coord.Lease, error) { return s.ListLive(ctx, coord.ClassWorker) },
		"ListOwned(old:1)": func() ([]coord.Lease, error) { return s.ListOwned(ctx, "old:1") },
	} {
		leases, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(leases) != 1 || leases[0].Resource != duty || leases[0].Owner != "old:1" {
			t.Fatalf("%s = %v, want exactly the older build's %s", name, leases, duty)
		}
	}
	// The membership read stays a scan of the seat lease bucket only.
	if nodes, err := s.ListLive(ctx, coord.ClassNode); err != nil || len(nodes) != 0 {
		t.Fatalf("ListLive(node:) = (%v, %v), want no presence", nodes, err)
	}
	// And it is still a DUTY, not a seat. This record physically sits in the
	// seat lease bucket, so a listing that decided a class from the bucket it
	// read rather than from the key's own leading segment would hand an
	// operator a running duty back as a live seat, and a capacity count a
	// seat nobody claimed.
	if seats, err := s.ListLive(ctx, coord.ClassSeat); err != nil || len(seats) != 0 {
		t.Fatalf("ListLive(seat:) = (%v, %v), want no seats: an older build's duty "+
			"lives in the seat lease bucket and is still listed by its own class", seats, err)
	}
}

// Every record this build writes carries its layout, so a record left without
// one can only be an older build's. A renew or a release of a record that
// somehow lost it stamps it again rather than keeping the older reading.
func TestEveryWriteStampsTheLayout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	seat := "seat:ceo"

	lease, err := s.TryAcquire(ctx, seat, coord.AcquireOptions{Owner: "new:1", TTL: time.Minute})
	if err != nil || lease == nil {
		t.Fatalf("claim = (%v, %v)", lease, err)
	}
	if got := rawLease(ctx, t, s.leases, seat).Layout; got != layoutDutyLane {
		t.Fatalf("claimed record layout = %d, want %d", got, layoutDutyLane)
	}

	strip := func() {
		t.Helper()
		v := rawLease(ctx, t, s.leases, seat)
		v.Layout = 0
		data, err := encodeValue(v)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, err := s.leases.kv.Put(ctx, encodeResource(seat), data); err != nil {
			t.Fatalf("strip the layout: %v", err)
		}
	}

	strip()
	if ok, err := s.Renew(ctx, seat, "new:1", lease.Epoch, time.Minute); err != nil || !ok {
		t.Fatalf("renew = (%v, %v)", ok, err)
	}
	if got := rawLease(ctx, t, s.leases, seat).Layout; got != layoutDutyLane {
		t.Fatalf("renewed record layout = %d, want %d", got, layoutDutyLane)
	}

	strip()
	if ok, err := s.Release(ctx, seat, "new:1", lease.Epoch); err != nil || !ok {
		t.Fatalf("release = (%v, %v)", ok, err)
	}
	if got := rawLease(ctx, t, s.leases, seat).Layout; got != layoutDutyLane {
		t.Fatalf("released record layout = %d, want %d", got, layoutDutyLane)
	}
}
