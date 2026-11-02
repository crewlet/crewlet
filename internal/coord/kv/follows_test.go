package kv

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// THE BUCKET'S AGE IS WHAT ENDS A FOLLOW, because nothing else does.
//
// There is no unfollow ([coord.Follows]): a follow is rewritten by every
// re-assert and removed only by the broker expiring what has gone quiet. A
// bucket opened without an age would therefore keep every follow any seat
// ever recorded, read on the hot path of every inbound chat message, for the
// life of the deployment.
func TestTheFollowsBucketsAgeIsWhatEndsAFollow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), "followttl")

	status, err := f.follows.Status(ctx)
	if err != nil {
		t.Fatalf("bucket status: %v", err)
	}
	if status.TTL() <= 0 {
		t.Fatalf("the follows bucket has no age (TTL=%v), so a follow, which "+
			"nothing else removes, would live for ever", status.TTL())
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
// and it is tested here rather than through Follow because the interleaving
// it exists for opens INSIDE that method — between its read and its write —
// and cannot be staged from outside it without a seam. A write that landed
// over a record it never read would put a reason chosen against one record
// over another — a broadcast decided before a mention was written, over the
// mention.
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
