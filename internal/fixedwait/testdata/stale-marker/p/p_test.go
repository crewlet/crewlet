package p

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestMarked(t *testing.T) {
	//fixedwait:liveness
	t.Log("nothing waits here")

	synctest.Test(t, func(t *testing.T) {
		//fixedwait:liveness
		time.Sleep(time.Millisecond)
	})
}
