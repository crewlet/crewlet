//go:build !linux

package cache_test

// peakAnon runs fn and answers false: only Linux's /proc says how much
// anonymous memory a child holds, so elsewhere BenchmarkMidx's memory arms
// report their times without their memory.
func peakAnon(fn func()) (int64, bool) {
	fn()
	return 0, false
}
