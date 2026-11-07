package p

import (
	"testing"
	"time"
)

func TestTwoReadings(t *testing.T) {
	start := time.Now()
	if time.Since(start) > time.Second || time.Now().Sub(start) > time.Second {
		t.Fatal("slow")
	}
}
