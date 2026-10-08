package kv

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// TestTheTenureStartIsTheWinningWritesOwnTimestamp pins WHERE the stamp comes
// from: the store's timestamp on the record the claim won, persisted in the
// value, so a renewal — which rewrites the record and so moves its timestamp
// — has the claim's moment to carry rather than its own.
func TestTheTenureStartIsTheWinningWritesOwnTimestamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	seat := coord.SeatResource("ceo")

	lease, _, err := s.TryAcquire(ctx, seat, coord.AcquireOptions{Owner: "new:1", TTL: time.Minute})
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
