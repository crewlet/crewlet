//go:build elsewhere

package p

import (
	"testing"
	"time"
)

// TestElsewhereWaits waits on time.After, the function, which is a wait
// counted by its name and no method of time.Time: nothing in it needs a type.
func TestElsewhereWaits(t *testing.T) {
	select {
	case <-time.After(time.Second): //fixedwait:liveness only bounds the case
	default:
	}
}
