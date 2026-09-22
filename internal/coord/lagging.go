package coord

import (
	"context"
	"fmt"
)

// IS A ROLLING UPGRADE STILL IN PROGRESS, asked once.
//
// # Why it is a function and not a comparison each caller writes
//
// Two decisions turn on it and they must not disagree. A seat host refuses to
// CLAIM while an older peer holds a live lease, because two nodes that
// disagree about what holding a lease MEANS are each individually correct and
// jointly wrong. And a whole-company import refuses to LAND while one does,
// for the same reason one level up: an import rewrites every placement in the
// chart, and a node applying that under an older reading of ownership would
// move seats it must not touch.
//
// Written as two comparisons, the day one of them changed — a `>` for a `>=`,
// an error read as "no" rather than as "cannot tell" — the fleet would start
// claiming under a rule the import did not know about, and neither side would
// report anything.
//
// # The rule, and why it is asymmetric
//
// A NEWER NODE WAITS AND AN OLDER ONE DOES NOT. An older build has no check
// at all — it cannot know about one that postdates it — so the only thing
// that can hold back is the newer side. A rolling deploy converges because
// that is what a rolling deploy does; a DOWNGRADE across a bump needs a full
// drain, and nothing here can enforce that.
//
// # An unreadable store is NOT "uniform"
//
// It is an error, and it travels as one. A caller that read a failure as "no
// older peer" would do exactly what the check exists to prevent, at the one
// moment it cannot tell.

// ProtocolFloorReader is the one method this question needs.
//
// DECLARED HERE rather than taking a whole [Backend], because a caller that
// holds only the floor — a test, a surface with a narrower seam — should be
// able to ask.
type ProtocolFloorReader interface {
	FleetProtocolFloor(ctx context.Context) (int, bool, error)
}

// Lagging reports the fleet's protocol floor when it is BELOW protocol, and
// whether there is one at all.
//
// A FLOOR OF ZERO WITH lagging FALSE is the ordinary answer: either no live
// lease exists yet, or every one of them is at this build's protocol or
// above.
func Lagging(ctx context.Context, r ProtocolFloorReader, protocol int) (
	floor int, lagging bool, err error) {

	if r == nil {
		return 0, false, fmt.Errorf("coord: no coordination backend, so " +
			"whether an older build is still running cannot be established")
	}
	got, found, err := r.FleetProtocolFloor(ctx)
	if err != nil {
		return 0, false, err
	}
	if !found || got >= protocol {
		return 0, false, nil
	}
	return got, true, nil
}

// LaggingOwner names a live node holding a lease at floor, for a message.
//
// A COURTESY RATHER THAN THE DECISION. [Lagging] is what decides, over the
// floor alone; this is a second read that may fail or find nothing, and an
// empty name never means "uniform". Reporting it changes an operator's next
// step from reading every node's version to restarting one.
func LaggingOwner(ctx context.Context, b Backend, floor int) string {
	if b == nil {
		return ""
	}
	leases, err := b.ListLive(ctx, ClassNode)
	if err != nil {
		return ""
	}
	for _, lease := range leases {
		if StoredProtocol(lease.Protocol) == floor {
			return lease.Owner
		}
	}
	return ""
}
