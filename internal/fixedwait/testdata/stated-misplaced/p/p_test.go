package p

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestMarked(t *testing.T) {
	//fixedwait:stated
	_ = base.Add(time.Hour).Sub(base)
	//fixedwait:stated the deadline is the case's own constant
	_ = time.Until(base)
	time.Sleep(time.Millisecond) //fixedwait:stated a fake's clock
	//fixedwait:stated base is the case's own constant
	_ = time.Since(base) + time.Now().Sub(base)
}
