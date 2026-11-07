package p

import (
	"testing"
	"time"
)

// waitOut is a benchmark's helper: it takes the *testing.B.
func waitOut(b *testing.B) {
	done := make(chan struct{})
	select {
	case <-done:
	//fixedwait:bench
	case <-time.After(10 * time.Second):
		b.Fatal("never finished")
	}
}
