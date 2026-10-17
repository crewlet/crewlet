package kv

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// AN UNFOLLOW ACTS ON THE REVISION IT READ, OR IT DOES NOT ACT.
//
// [FleetStore.deleteFollowAt] is the rule [FleetStore.Unfollow] is built from,
// and it is tested here rather than through Unfollow because the interleaving
// it exists for opens INSIDE that method — between its read and its write —
// and cannot be staged from outside it without a seam. What it must never do
// is remove a record the caller never read.
func TestAFollowDeleteBoundToAStaleRevisionDoesNotFire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), "unfollowrace")
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	if _, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-1", at, overwrite("mention")); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	key := followKey("slack", "agent-swe", "C1", "t-1")
	first, err := f.follows.Get(ctx, key)
	if err != nil {
		t.Fatalf("read the follow back: %v", err)
	}

	// THE RE-ASSERT, in the window: a mention landing on another node while
	// an unfollow is in flight. Every follow trigger is a write on this key.
	if _, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-1", at.Add(time.Minute), overwrite("app_mention")); err != nil {
		t.Fatalf("re-assert the follow: %v", err)
	}

	removed, err := f.deleteFollowAt(ctx, key, first.Revision())
	if err != nil {
		t.Fatalf("deleteFollowAt at a stale revision raised: %v", err)
	}
	if removed {
		t.Fatal("a delete bound to a revision that had already been replaced " +
			"reported that it removed the follow, so the report describes a " +
			"record the call did not act on")
	}
	// A lost race is not a failure, and this CALL left the record it lost to
	// untouched. [FleetStore.Unfollow] re-reads and converges; what is pinned
	// here is that one pass never removes a revision it did not read.
	reason, following, err := f.Following(ctx, "slack", "agent-swe", "C1", "t-1")
	if err != nil {
		t.Fatalf("Following: %v", err)
	}
	if !following || reason != "app_mention" {
		t.Errorf("after a lost race: following=%v reason=%q, want the re-assert intact",
			following, reason)
	}
	// And at the CURRENT revision it fires.
	current, err := f.follows.Get(ctx, key)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if removed, err := f.deleteFollowAt(ctx, key, current.Revision()); err != nil || !removed {
		t.Fatalf("deleteFollowAt at the current revision: removed=%v err=%v", removed, err)
	}
}

// A KEY THAT WENT AWAY UNDER THE READ IS A LOST RACE, NOT A FAILURE.
//
// This is the case that made the two backends disagree. The memory twin holds
// one mutex across its check and its delete, so an already-removed follow
// answers `false`; the unconditional delete this replaces answered `true` for
// it, because the read saw a record and the delete no-oped on an absent key.
func TestAFollowDeleteOnAKeyAPeerAlreadyRemovedIsNotAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), "unfollowgone")
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	if _, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-3", at, overwrite("mention")); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	key := followKey("slack", "agent-swe", "C1", "t-3")
	entry, err := f.follows.Get(ctx, key)
	if err != nil {
		t.Fatalf("read the follow back: %v", err)
	}
	// The peer's unfollow, between this caller's read and its write.
	if _, err := f.Unfollow(ctx, "slack", "agent-swe", "C1", "t-3"); err != nil {
		t.Fatalf("the peer's Unfollow: %v", err)
	}

	removed, err := f.deleteFollowAt(ctx, key, entry.Revision())
	if err != nil {
		t.Fatalf("deleteFollowAt on a removed key raised: %v", err)
	}
	if removed {
		t.Error("reported that it removed a follow a peer had already removed — " +
			"the memory twin answers false here, and one suite certifies both")
	}
}

// AND UNFOLLOW ITSELF STILL REMOVES WHAT IT DID SEE.
//
// The conditional delete must not make the ordinary path conservative: an
// unfollow with nothing racing it removes the follow and says so.
func TestAnUncontendedUnfollowStillRemovesTheFollow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), "unfollowplain")
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	if _, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-plain", at, overwrite("mention")); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	gone, err := f.Unfollow(ctx, "slack", "agent-swe", "C1", "t-plain")
	if err != nil {
		t.Fatalf("Unfollow: %v", err)
	}
	if !gone {
		t.Fatal("an uncontended unfollow reported it removed nothing")
	}
	if _, following, err := f.Following(ctx, "slack", "agent-swe", "C1", "t-plain"); err != nil || following {
		t.Errorf("still following after an uncontended unfollow: following=%v err=%v", following, err)
	}
}

// THE TOMBSTONE AGES OUT, which is why this bucket deletes where the
// TTL-less ones purge.
//
// [FleetStore.DeleteIntegrationStatus] purges because a bucket with no age
// keeps every tombstone for the life of the deployment. This bucket carries
// `FollowRetention`, so the broker expires a tombstone like anything else.
func TestTheFollowsBucketCarriesTheRetentionThatAgesItsTombstones(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), "followttl")

	status, err := f.follows.Status(ctx)
	if err != nil {
		t.Fatalf("bucket status: %v", err)
	}
	if status.TTL() <= 0 {
		t.Fatalf("the follows bucket has no age (TTL=%v), so a deleted follow's "+
			"tombstone would live for ever and Unfollow should purge instead",
			status.TTL())
	}
	if status.TTL() != 10*time.Minute {
		t.Errorf("bucket TTL = %v, want the configured FollowRetention", status.TTL())
	}
}

// overwrite is the choice that writes reason whatever the record holds.
func overwrite(reason string) func(string, bool) string {
	return func(string, bool) string { return reason }
}

// A FOLLOW WRITES ONLY OVER THE REVISION IT READ, OR IT DOES NOT WRITE.
//
// [FleetStore.writeFollowAt] is the rule [FleetStore.Follow] is built from,
// tested here for the reason the delete's twin is: the interleaving opens
// between Follow's read and its write. A write that landed over a record it
// never read would put a reason chosen against one record over another — a
// broadcast decided before a mention was written, over the mention.
func TestAFollowWrittenAtAStaleRevisionDoesNotLand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), "followrace")
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	if _, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-4", at, overwrite("collective")); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	key := followKey("slack", "agent-swe", "C1", "t-4")
	_, _, read, err := f.readFollow(ctx, key)
	if err != nil {
		t.Fatalf("readFollow: %v", err)
	}
	// The peer's mention, between this caller's read and its write.
	if _, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-4", at, overwrite("mention")); err != nil {
		t.Fatalf("the peer's Follow: %v", err)
	}
	stale, err := json.Marshal(followRecord{Reason: "collective", At: at})
	if err != nil {
		t.Fatal(err)
	}
	landed, err := f.writeFollowAt(ctx, key, read, stale)
	if err != nil {
		t.Fatalf("writeFollowAt at a stale revision raised: %v", err)
	}
	if landed {
		t.Fatal("a follow written at a revision that had already been replaced landed " +
			"over the record it never read")
	}
	if reason, _, err := f.Following(ctx, "slack", "agent-swe", "C1", "t-4"); err != nil || reason != "mention" {
		t.Fatalf("after a lost race: reason=%q err=%v, want the peer's mention intact", reason, err)
	}

	// A create loses the same way: a key that holds a value is not written
	// at revision zero.
	landed, err = f.writeFollowAt(ctx, key, 0, stale)
	if err != nil || landed {
		t.Fatalf("writeFollowAt at zero over a live follow: landed=%v err=%v, want a lost race", landed, err)
	}

	// And at the CURRENT revision it lands.
	_, _, current, err := f.readFollow(ctx, key)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if landed, err := f.writeFollowAt(ctx, key, current, stale); err != nil || !landed {
		t.Fatalf("writeFollowAt at the current revision: landed=%v err=%v", landed, err)
	}
}

// A FOLLOW AFTER AN UNFOLLOW IS A FOLLOW AGAIN. The unfollow leaves a delete
// marker, which a read answers as nothing held — and the write at revision
// zero must step over it rather than report the key as taken, or a seat told
// to stop watching a thread could never be pulled back into it.
func TestAFollowAfterAnUnfollowLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), "followagain")
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	if _, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-5", at, overwrite("mention")); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if gone, err := f.Unfollow(ctx, "slack", "agent-swe", "C1", "t-5"); err != nil || !gone {
		t.Fatalf("Unfollow = %v, %v", gone, err)
	}
	var shown []bool
	got, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-5", at.Add(time.Minute),
		func(_ string, following bool) string {
			shown = append(shown, following)
			return "collective"
		})
	if err != nil || got != "collective" {
		t.Fatalf("Follow after an unfollow = (%q, %v), want the new follow written", got, err)
	}
	if len(shown) == 0 || shown[0] {
		t.Fatalf("the choice was shown following=%v over a removed follow", shown)
	}
	if reason, following, err := f.Following(ctx, "slack", "agent-swe", "C1", "t-5"); err != nil ||
		!following || reason != "collective" {
		t.Fatalf("Following = (%q, %v, %v), want the new follow", reason, following, err)
	}
}

// A DAMAGED RECORD IS REPLACED, NOT A WALL. [FleetStore.Following] raises on a
// record it cannot decode, which is right for a read; a follow is about to
// write the key whole, and refusing there would leave the seat unable ever to
// follow that thread again.
func TestAFollowOverADamagedRecordReplacesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), "followdamaged")
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	key := followKey("slack", "agent-swe", "C1", "t-6")
	if _, err := f.follows.Put(ctx, key, []byte("not a follow")); err != nil {
		t.Fatalf("plant the damaged record: %v", err)
	}
	if _, _, err := f.Following(ctx, "slack", "agent-swe", "C1", "t-6"); err == nil {
		t.Fatal("a damaged record read back without an error — the premise")
	}
	var shown []bool
	got, err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-6", at,
		func(_ string, following bool) string {
			shown = append(shown, following)
			return "mention"
		})
	if err != nil || got != "mention" {
		t.Fatalf("Follow over a damaged record = (%q, %v), want the mention written", got, err)
	}
	if len(shown) != 1 || shown[0] {
		t.Fatalf("the choice was shown following=%v over a damaged record, want once and false", shown)
	}
	if reason, following, err := f.Following(ctx, "slack", "agent-swe", "C1", "t-6"); err != nil ||
		!following || reason != "mention" {
		t.Fatalf("Following = (%q, %v, %v), want the replacement", reason, following, err)
	}
}
