package signedask

import (
	"fmt"
	"math"
	"time"
)

// WaitMS is wait as a request carries it beside its deadline, for its taking
// node to count from the request's arrival ([Due]): whole milliseconds,
// ROUNDED UP, so a wait under one crosses as one rather than as none, which
// is refused ([Due]). A taking node working at most a
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
// [WaitMS]), whatever either node's clock reads. arrived is when it reached
// this process, before any wait for a slot (internal/queue's Arrived), and now
// the same clock as the answer starts. why is set, and nothing is to run, for
// a request whose wait was spent before it started, one carrying no wait, or
// one that is no duration (below zero, or too long for one): no sender of this
// build asks with either of the last two, and an older unreleased build's
// request gets no compatibility.
func Due(arrived time.Time, waitMS int64, now time.Time) (due time.Time, why string) {
	switch {
	case waitMS == 0:
		return time.Time{}, "the request carries no wait, so nothing says how long its asker waits " +
			"(an older build's asker: finish the upgrade)"
	case waitMS < 0 || waitMS > math.MaxInt64/int64(time.Millisecond):
		return time.Time{}, fmt.Sprintf("the request carries a wait of %d ms, which is no wait anybody "+
			"is waiting out", waitMS)
	}
	due = arrived.Add(time.Duration(waitMS) * time.Millisecond)
	if !now.Before(due) {
		return time.Time{}, fmt.Sprintf("its asker's wait of %d ms was spent before this node could take "+
			"it, %v after it arrived: nobody is waiting for its answer", waitMS, now.Sub(arrived).Round(time.Millisecond))
	}
	return due, ""
}
