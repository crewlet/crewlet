package waittest

import "time"

// Clock is a bubble's clock.
type Clock struct{}

// Settle lets the bubble's clock run.
//
//fixedwait:bubble
func (Clock) Settle() {
	time.Sleep(time.Second)
}
