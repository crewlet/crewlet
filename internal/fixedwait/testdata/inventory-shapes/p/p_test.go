package p

import (
	"testing"
	"time"
)

func TestOneOfEach(t *testing.T) {
	start := time.Now()
	time.Sleep(time.Millisecond)
	if time.Since(start) > time.Second {
		t.Fatal("slow")
	}
}
