package kv

import (
	"context"
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

	if err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-1", "mention", at); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	key := followKey("slack", "agent-swe", "C1", "t-1")
	first, err := f.follows.Get(ctx, key)
	if err != nil {
		t.Fatalf("read the follow back: %v", err)
	}

	// THE RE-ASSERT, in the window: a mention landing on another node while
	// an unfollow is in flight. Every follow trigger is a Put on this key.
	if err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-1", "app_mention", at.Add(time.Minute)); err != nil {
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

	if err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-3", "mention", at); err != nil {
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

	if err := f.Follow(ctx, "slack", "agent-swe", "C1", "t-plain", "mention", at); err != nil {
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
