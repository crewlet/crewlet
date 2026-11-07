package waittest

import (
	"testing/synctest"
	"time"
)

// Settle lets the bubble's clock run.
//
//fixedwait:bubble
func Settle() {
	time.Sleep(time.Second)
	synctest.Wait()
}

// SettleTwice settles, twice, and runs only in a bubble too.
//
//fixedwait:bubble
func SettleTwice() {
	Settle()
	Settle()
}
