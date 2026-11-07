package p

import (
	"testing"
	"time"
)

func TestLeavesAMargin(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	if time.Until(deadline) < 500*time.Millisecond {
		t.Fatal("no margin left")
	}
}
