package p

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestMarkedWrongly(t *testing.T) {
	start := time.Now()
	//fixedwait:stated start is the case's own instant
	if time.Since(start) > time.Hour {
		t.Fatal("slow")
	}
	//fixedwait:liveness
	if time.Now().After(start.Add(time.Hour)) {
		t.Fatal("slow")
	}
	//fixedwait:stated
	_ = base.Compare(base.Add(time.Hour))
}
