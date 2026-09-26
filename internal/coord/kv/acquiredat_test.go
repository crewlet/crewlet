package kv

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// TestAnOlderBuildsLeaseReadsAsAnUnknownTenure is the rolling-upgrade half of
// coord.Lease.AcquiredAt.
//
// A protocol-3 node wrote its seat records before the field existed. Its
// tenure began at a moment nobody recorded, so the honest reading is ZERO —
// which every reader renders as absent — rather than a moment this build
// makes up: the record's own timestamp is the last heartbeat, and stamping
// that would tell an operator the seat moved seconds ago. A renewal carries
// the absence forward for the same reason, and the wire keeps the key out
// entirely rather than writing Go's zero instant.
func TestAnOlderBuildsLeaseReadsAsAnUnknownTenure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	seat := coord.SeatResource("ceo")

	data, err := json.Marshal(map[string]any{
		"resource": seat,
		"owner":    "old:1",
		"epoch":    1,
		"ttl_ns":   int64(time.Minute),
		"protocol": 3,
	})
	if err != nil {
		t.Fatalf("encode the older build's record: %v", err)
	}
	if _, err := s.leases.kv.Put(ctx, encodeResource(seat), data); err != nil {
		t.Fatalf("write the older build's record: %v", err)
	}

	lease, err := s.Get(ctx, seat)
	if err != nil || lease == nil {
		t.Fatalf("Get = (%v, %v), want the older build's live lease", lease, err)
	}
	if !lease.AcquiredAt.IsZero() {
		t.Fatalf("a record with no acquired_at read as tenure start %v, want zero (unknown)",
			lease.AcquiredAt)
	}

	if ok, err := s.Renew(ctx, seat, "old:1", lease.Epoch, time.Minute); err != nil || !ok {
		t.Fatalf("renew = (%v, %v)", ok, err)
	}
	kve, err := s.leases.kv.Get(ctx, encodeResource(seat))
	if err != nil {
		t.Fatalf("read the renewed record: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(kve.Value(), &raw); err != nil {
		t.Fatalf("decode the renewed record: %v", err)
	}
	if v, present := raw["acquired_at"]; present {
		t.Fatalf("a renewal wrote acquired_at %v onto a tenure whose start nobody recorded", v)
	}
}

// TestTheTenureStartIsTheWinningWritesOwnTimestamp pins WHERE the stamp comes
// from: the store's timestamp on the record the claim won, persisted in the
// value, so a renewal — which rewrites the record and so moves its timestamp
// — has the claim's moment to carry rather than its own.
func TestTheTenureStartIsTheWinningWritesOwnTimestamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	seat := coord.SeatResource("ceo")

	lease, err := s.TryAcquire(ctx, seat, coord.AcquireOptions{Owner: "new:1", TTL: time.Minute})
	if err != nil || lease == nil {
		t.Fatalf("claim = (%v, %v)", lease, err)
	}
	if got := rawLease(ctx, t, s.leases, seat).AcquiredAt; !got.Equal(lease.AcquiredAt) {
		t.Fatalf("the stored record says %v, the claim answered %v", got, lease.AcquiredAt)
	}
	kve, err := s.leases.kv.Get(ctx, encodeResource(seat))
	if err != nil {
		t.Fatalf("read the committed record: %v", err)
	}
	// The committed record is the claim's SECOND write (claim, then commit
	// the token), so its own timestamp is at or after the stamp — never
	// before it, which would be a clock that is not the store's.
	if kve.Created().Before(lease.AcquiredAt) {
		t.Fatalf("tenure start %v is after the store's commit of it at %v",
			lease.AcquiredAt, kve.Created())
	}
}
