package p

import (
	"testing"
	"testing/synctest"
	"time"
)

// settle lets the bubble's clock run.
//
//fixedwait:bubble
func settle() {
	time.Sleep(time.Second)
}

func TestSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settle()
	})
	settle()
}
