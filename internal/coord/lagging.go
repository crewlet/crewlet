package coord

import (
	"context"
	"fmt"
)

// IS A ROLLING UPGRADE STILL IN PROGRESS.
//
// A seat host refuses to CLAIM while an older peer holds a live lease, because
// two nodes that disagree about what holding a lease MEANS are each
// individually correct and jointly wrong.
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
// It is an error, and it travels as one: what not knowing costs is the
// caller's to decide, and an answer of "no older peer" would have decided it
// for them.

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
