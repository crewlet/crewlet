package changefeed

import "time"

// ReopenBase sets a feed's reopen schedule before it runs, so a case watches
// the schedule play out in milliseconds rather than seconds. The ceiling stays
// the same multiple of the base.
func ReopenBase(f *Feed, base time.Duration) { f.reopenBase = base }

// Wait is the schedule's wait before attempt n of a series.
func Wait(f *Feed, n int) time.Duration { return f.wait(n) }

// Ceiling is the schedule's longest wait before its spread.
func Ceiling(f *Feed) time.Duration { return f.ceiling() }

// Series is the attempt the schedule last waited for.
func Series(f *Feed) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.series
}
