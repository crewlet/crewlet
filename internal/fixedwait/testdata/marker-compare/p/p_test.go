package p

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestMeasured(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	for ready := false; !ready; ready = true {
		if !time.Now().Before(deadline) { //fixedwait:measure the poll's own deadline: its condition decides
			t.Fatal("never ready")
		}
	}
	//fixedwait:measure logged for the run's report
	t.Logf("ahead of the deadline: %v", time.Now().Compare(deadline) < 0)
}

func TestStated(t *testing.T) {
	end := base.Add(time.Hour)
	if !end.After(base) { //fixedwait:stated both instants are the case's own constants
		t.Fatal("not after")
	}
	//fixedwait:stated end is base plus an hour, computed once
	if end.Compare(base) <= 0 || !base.Before(end) {
		t.Fatal("not ordered")
	}
}
