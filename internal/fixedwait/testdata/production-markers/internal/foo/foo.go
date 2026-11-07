package foo

import "time"

// Retry is production code, so a marker in it marks nothing.
func Retry() {
	//fixedwait:liveness
	time.Sleep(time.Millisecond)
}

// Settle is production code, so a bubble declaration takes nothing out of
// the class.
//
//fixedwait:bubble
func Settle() { time.Sleep(time.Millisecond) }
