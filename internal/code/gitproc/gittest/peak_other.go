//go:build !linux

package gittest

// PeakAnon runs fn and answers false: only Linux's /proc says how much
// anonymous memory a child holds, so elsewhere a benchmark's memory arms
// report their times without their memory.
func PeakAnon(fn func()) (int64, bool) {
	fn()
	return 0, false
}
