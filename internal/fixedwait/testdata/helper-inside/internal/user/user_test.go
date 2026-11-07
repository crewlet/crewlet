package user

import (
	"testing"
	"testing/synctest"
	"time"

	"example.com/fixture/internal/waittest"
)

type clock struct{}

// tick advances the bubble.
//
//fixedwait:bubble
func (clock) tick() {
	<-time.After(time.Second)
}

func TestSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		waittest.Settle()
		waittest.SettleTwice()
		clock{}.tick()
	})
	synctest.Run(waittest.Settle)
}
