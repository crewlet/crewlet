package signedask

import (
	"fmt"
	"math"
	"time"
)

// WaitMS is wait as a request carries it beside its deadline, for its taking
// node to count from the request's arrival ([Due]): whole milliseconds,
// ROUNDED UP, so a wait under one crosses as one rather than as none, which
// reads as a request carrying no wait. A taking node working at most a
// millisecond past its asker is inside every channel's reply margin. Zero for
// a wait already spent, which no sender asks with.
func WaitMS(wait time.Duration) int64 {
	if wait <= 0 {
		return 0
	}
	ms := int64(wait / time.Millisecond)
	if wait%time.Millisecond != 0 {
		ms++
	}
	return ms
}

// Due is the instant, on this node's runtime clock, an answer to a request
// that arrived at arrived is due: arrived plus the wait it carries (waitMS,
// [WaitMS]), whatever either node's clock reads. A request carrying no wait,
// an older build's in a rolling upgrade, is due at its deadline judged on now,
// the clock it was admitted by, read at its arrival: what that build's taking
// nodes did. why is set for a carried wait that is no duration (below zero, or
// too long for one), which nobody is waiting out, and then nothing is to run.
func Due(arrived time.Time, waitMS int64, deadline, now time.Time) (due time.Time, why string) {
	switch {
	case waitMS == 0:
		return arrived.Add(deadline.Sub(now)), ""
	case waitMS < 0 || waitMS > math.MaxInt64/int64(time.Millisecond):
		return time.Time{}, fmt.Sprintf("the request carries a wait of %d ms, which is no wait anybody "+
			"is waiting out", waitMS)
	}
	return arrived.Add(time.Duration(waitMS) * time.Millisecond), ""
}
