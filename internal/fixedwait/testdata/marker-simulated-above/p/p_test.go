package p

import (
	"testing"
	"time"
)

func TestMarked(t *testing.T) {
	done := make(chan struct{})
	select {
	case <-done:
	//fixedwait:simulated a slow peer's answer
	case <-time.After(10 * time.Second):
		t.Fatal("never finished")
	}
}
