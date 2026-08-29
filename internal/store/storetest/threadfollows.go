package storetest

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The thread-follow cases. Here rather than package-local because the upsert
// is the same ON CONFLICT … DO UPDATE shape the rest of this suite pins — a
// shape whose arbiter resolution the driver gets wrong in one specific way
// (d-002 §2), which is why it is asserted rather than assumed.

func testFollowRoundTrips(t *testing.T, db *store.DB) {
	f := db.ThreadFollows()

	if _, ok, err := f.Following(t.Context(), "slack", "swe", "C1", "T1"); err != nil || ok {
		t.Fatalf("an unwritten follow read back as %v, %v", ok, err)
	}
	if err := f.Follow(t.Context(), "slack", "swe", "C1", "T1", "mention", base); err != nil {
		t.Fatalf("follow: %v", err)
	}
	reason, ok, err := f.Following(t.Context(), "slack", "swe", "C1", "T1")
	if err != nil || !ok || reason != "mention" {
		t.Fatalf("Following = %q, %v, %v", reason, ok, err)
	}
}

// The reason is OVERWRITTEN on re-assert: a seat first pulled into a thread
// by a collective shout and later named personally is now following for the
// stronger reason, and an operator asking why it answered should see the
// mention rather than the shout that happened to come first.
func testFollowUpdatesTheReasonAndTheStamp(t *testing.T, db *store.DB) {
	f := db.ThreadFollows()

	if err := f.Follow(t.Context(), "slack", "swe", "C1", "T1", "collective", base); err != nil {
		t.Fatalf("follow: %v", err)
	}
	later := base.Add(time.Hour)
	if err := f.Follow(t.Context(), "slack", "swe", "C1", "T1", "mention", later); err != nil {
		t.Fatalf("re-follow: %v", err)
	}
	reason, _, err := f.Following(t.Context(), "slack", "swe", "C1", "T1")
	if err != nil || reason != "mention" {
		t.Fatalf("the reason is %q, want the stronger mention", reason)
	}

	// The stamp moved with it, so a re-asserted follow is not swept as
	// stale — it is a last-activity stamp, not a creation date.
	n, err := f.Purge(t.Context(), base.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Fatalf("the sweep took %d live follows", n)
	}
	if _, ok, _ := f.Following(t.Context(), "slack", "swe", "C1", "T1"); !ok {
		t.Fatal("a re-asserted follow was swept as stale")
	}
}

// Two backends' thread ids come from different namespaces and are not
// guaranteed distinct, so one must never satisfy the other's lookup.
func testFollowsAreScopedByBackend(t *testing.T, db *store.DB) {
	f := db.ThreadFollows()

	if err := f.Follow(t.Context(), "slack", "swe", "C1", "T1", "mention", base); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if _, ok, _ := f.Following(t.Context(), "mattermost", "swe", "C1", "T1"); ok {
		t.Fatal("a Slack follow satisfied a Mattermost lookup")
	}
	// And each seat follows for itself.
	if _, ok, _ := f.Following(t.Context(), "slack", "qa", "C1", "T1"); ok {
		t.Fatal("one seat's follow answered for another")
	}
	// A channel is part of the identity too: the same thread id under a
	// different channel is a different thread.
	if _, ok, _ := f.Following(t.Context(), "slack", "swe", "C2", "T1"); ok {
		t.Fatal("a follow leaked across channels")
	}
}

func testUnfollowRemovesExactlyOne(t *testing.T, db *store.DB) {
	f := db.ThreadFollows()
	for _, thread := range []string{"T1", "T2"} {
		if err := f.Follow(t.Context(), "slack", "swe", "C1", thread, "mention", base); err != nil {
			t.Fatalf("follow: %v", err)
		}
	}

	dropped, err := f.Unfollow(t.Context(), "slack", "swe", "C1", "T1")
	if err != nil || !dropped {
		t.Fatalf("Unfollow = %v, %v", dropped, err)
	}
	if _, ok, _ := f.Following(t.Context(), "slack", "swe", "C1", "T1"); ok {
		t.Fatal("the follow survived being dropped")
	}
	if _, ok, _ := f.Following(t.Context(), "slack", "swe", "C1", "T2"); !ok {
		t.Fatal("unfollowing one thread dropped another")
	}
	if again, _ := f.Unfollow(t.Context(), "slack", "swe", "C1", "T1"); again {
		t.Fatal("unfollowing twice reported a second removal")
	}
}

func testFollowPurgeKeepsTheLiveOnes(t *testing.T, db *store.DB) {
	f := db.ThreadFollows()
	stale := base.Add(-100 * 24 * time.Hour)
	if err := f.Follow(t.Context(), "slack", "swe", "C1", "old", "mention", stale); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if err := f.Follow(t.Context(), "slack", "swe", "C1", "new", "mention", base); err != nil {
		t.Fatalf("follow: %v", err)
	}

	n, err := f.Purge(t.Context(), base.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("the sweep took %d rows, want the one stale follow", n)
	}
	if _, ok, _ := f.Following(t.Context(), "slack", "swe", "C1", "new"); !ok {
		t.Fatal("the sweep took a live follow")
	}
}

// An incomplete identity is refused rather than written under an empty key,
// where it would answer for every other incomplete write.
func testFollowRefusesAnIncompleteIdentity(t *testing.T, db *store.DB) {
	f := db.ThreadFollows()
	for _, c := range [][4]string{
		{"", "swe", "C1", "T1"},
		{"slack", "", "C1", "T1"},
		{"slack", "swe", "C1", ""},
	} {
		if err := f.Follow(t.Context(), c[0], c[1], c[2], c[3], "mention", base); err == nil {
			t.Errorf("Follow%v was accepted", c)
		}
		if _, ok, err := f.Following(t.Context(), c[0], c[1], c[2], c[3]); err != nil || ok {
			t.Errorf("Following%v = %v, %v", c, ok, err)
		}
	}
}
