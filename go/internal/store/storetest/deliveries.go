package storetest

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The delivery-claim cases. They are here rather than in a package-local test
// because the claim rests on a conditional upsert — ON CONFLICT … DO UPDATE …
// WHERE — and that is precisely the kind of statement one driver accepts and
// the other does not. A suite that ran on mainline SQLite alone would certify
// a dialect the engine's own database may not speak.

func testDeliveryClaimIsFirstWins(t *testing.T, db *store.DB) {
	log := db.DeliveryLog()
	now := base

	won, err := log.Claim(t.Context(), "github", "d-1", now)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !won {
		t.Fatal("the first caller did not win an unclaimed delivery")
	}

	// The retry the provider sends when the first response was slow. It is
	// the same delivery, and the seat must not wake twice for it.
	won, err = log.Claim(t.Context(), "github", "d-1", now.Add(time.Second))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if won {
		t.Fatal("a redelivery inside the TTL was claimed twice")
	}
}

func testDeliveryClaimIsPerSource(t *testing.T, db *store.DB) {
	// Delivery ids are unique within a provider and say nothing across
	// them. A key column shared between sources would let a GitHub
	// delivery swallow a Plane one that happened to mint the same id.
	log := db.DeliveryLog()
	if won, err := log.Claim(t.Context(), "github", "shared", base); err != nil || !won {
		t.Fatalf("github claim: won=%v err=%v", won, err)
	}
	won, err := log.Claim(t.Context(), "gitlab", "shared", base)
	if err != nil {
		t.Fatalf("gitlab claim: %v", err)
	}
	if !won {
		t.Fatal("a different source's delivery was refused on a colliding key")
	}
}

func testDeliveryClaimExpires(t *testing.T, db *store.DB) {
	// The TTL is the whole reason the claim is an upsert rather than a
	// plain DO NOTHING. Without the time predicate on the DO UPDATE the
	// claim is permanent, and an operator replaying a webhook ten minutes
	// later watches it vanish into a row nothing will ever clear.
	log := db.DeliveryLog().WithTTL(time.Minute)
	if won, err := log.Claim(t.Context(), "plane", "d-2", base); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	// Still inside the window.
	if won, _ := log.Claim(t.Context(), "plane", "d-2", base.Add(30*time.Second)); won {
		t.Fatal("a claim expired before its TTL")
	}
	won, err := log.Claim(t.Context(), "plane", "d-2", base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !won {
		t.Fatal("a claim older than the TTL was not re-claimable, so it is permanent")
	}
}

func testDeliveryClaimRefreshesTheStamp(t *testing.T, db *store.DB) {
	// A re-claim must MOVE seen_at, not merely be permitted: a row whose
	// stamp stayed at the first claim would expire on the original
	// schedule, so a delivery re-claimed at the edge of the window would be
	// immediately re-claimable again by a peer.
	log := db.DeliveryLog().WithTTL(time.Minute)
	if won, err := log.Claim(t.Context(), "jira", "d-3", base); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	if won, _ := log.Claim(t.Context(), "jira", "d-3", base.Add(2*time.Minute)); !won {
		t.Fatal("re-claim after the TTL was refused")
	}
	// 30 s after the RE-claim, which is 2m30s after the first one.
	if won, _ := log.Claim(t.Context(), "jira", "d-3", base.Add(150*time.Second)); won {
		t.Fatal("the re-claim did not refresh seen_at, so the row expires on the original schedule")
	}
}

func testDeliveryClaimWithoutAKey(t *testing.T, db *store.DB) {
	// A provider that sends no delivery id leaves nothing to dedupe on.
	// Both calls must win: a missed duplicate is a doubled reply, a wrongly
	// dropped delivery is a message nobody ever answers.
	log := db.DeliveryLog()
	for i := range 2 {
		won, err := log.Claim(t.Context(), "confluence", "", base)
		if err != nil {
			t.Fatalf("keyless claim %d: %v", i, err)
		}
		if !won {
			t.Fatalf("keyless claim %d was refused", i)
		}
	}
}

func testDeliveryPurge(t *testing.T, db *store.DB) {
	log := db.DeliveryLog()
	for i, key := range []string{"old-1", "old-2", "fresh"} {
		at := base.Add(-time.Hour)
		if key == "fresh" {
			at = base
		}
		if won, err := log.Claim(t.Context(), "github", key, at); err != nil || !won {
			t.Fatalf("seed %d: won=%v err=%v", i, won, err)
		}
	}
	n, err := log.Purge(t.Context(), base.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d rows, want the 2 older than the cutoff", n)
	}
	// The survivor is still claimed — a sweep that also dropped it would
	// re-open every recent delivery to a duplicate.
	if won, _ := log.Claim(t.Context(), "github", "fresh", base.Add(time.Second)); won {
		t.Fatal("the sweep dropped a row inside the TTL")
	}
}

func testDeliveryRelease(t *testing.T, db *store.DB) {
	// The claim is taken before the delivery is republished, so a republish
	// that fails must give the claim back — otherwise the provider's retry,
	// the only other copy of that delivery, is refused by a row nothing
	// clears inside the TTL.
	log := db.DeliveryLog()
	if won, err := log.Claim(t.Context(), "gitlab", "d-4", base); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if err := log.Release(t.Context(), "gitlab", "d-4"); err != nil {
		t.Fatalf("release: %v", err)
	}
	won, err := log.Claim(t.Context(), "gitlab", "d-4", base.Add(time.Second))
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !won {
		t.Fatal("a released delivery could not be re-claimed, so the retry is lost")
	}

	// Releasing what was never claimed is not an error: the caller reaches
	// here on a failure path and cannot know which half ran.
	if err := log.Release(t.Context(), "gitlab", "never-claimed"); err != nil {
		t.Fatalf("release of an unclaimed key: %v", err)
	}
}
