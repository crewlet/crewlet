package waittest

import "time"

// Settle lets the bubble's clock run.
//
//fixedwait:bubble
func Settle() {
	time.Sleep(time.Second)
}
