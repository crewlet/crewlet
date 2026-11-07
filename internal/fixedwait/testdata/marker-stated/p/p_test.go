package p

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestMarked(t *testing.T) {
	end := base.Add(time.Hour)
	if end.Sub(base) != time.Hour { //fixedwait:stated both instants are the case's own constants
		t.Fatal("not an hour")
	}
	//fixedwait:stated both on the case's stepped clock
	if got := end.Sub(base); got != time.Hour {
		t.Fatal(got)
	}
}
