package p

import (
	"testing"
	"time"
)

func TestAnswersInTime(t *testing.T) {
	start := time.Now()
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("too slow")
	}
}
