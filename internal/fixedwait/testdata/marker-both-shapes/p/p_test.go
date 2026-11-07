package p

import (
	"testing"
	"time"
)

func bothShapes(t *testing.T, deadline time.Time, done chan struct{}) {
	select {
	case <-done:
	//fixedwait:liveness
	case <-time.After(time.Until(deadline)): //fixedwait:measure the remaining time only sizes the bound
		t.Fatal("never done")
	}
	//fixedwait:simulated a slow peer's answer
	time.Sleep(time.Until(deadline)) //fixedwait:measure the remaining time only paces the peer
}

func stacked(deadline time.Time) {
	//fixedwait:simulated a slow peer's answer
	//fixedwait:measure the remaining time only paces the peer
	time.Sleep(time.Until(deadline))
}
