package p

import (
	"testing"
	"time"
)

func TestNothingArrives(t *testing.T) {
	time.Sleep(time.Millisecond) //fixedwait:bench
}

func waitOut(tb testing.TB) {
	//fixedwait:bench
	time.Sleep(time.Millisecond)
}
