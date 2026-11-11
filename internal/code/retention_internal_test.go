package code

import (
	"testing"
	"testing/synctest"
	"time"
)

// A PASS PACES ITS STEPS ON THE WALL CLOCK BY DEFAULT: a Retention handed no
// clock and no sleep — the engine's — reads time.Now and waits on a timer, so
// its first step's turn comes at once, and after a step it waits
// retentionIdlePerStep times as long as the step took on that clock. Read in
// a bubble, where the clock the code reads moves only while every goroutine
// of it is blocked: a step of 30 ms is followed by a wait of exactly twice
// that.
//
// Mutation: wait out nothing by default (a sleep that only reads the
// context) — the next turn comes at once, red; read the steps on a clock that
// never moves — no wait after a step, red.
func TestAPassPacesOnTheWallClockByDefault(t *testing.T) {
	const step = 30 * time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		pace := (&Retention{}).pacer()
		at := time.Now()
		if turn, err := pace.wait(t.Context()); !turn || err != nil || !time.Now().Equal(at) {
			t.Fatalf("the first step's turn = %v, %v after %v, want it at once", turn, err, time.Since(at))
		}
		began := pace.clock()
		time.Sleep(step)
		pace.took(began)
		at = time.Now()
		turn, err := pace.wait(t.Context())
		want := time.Duration(float64(step) * retentionIdlePerStep)
		if waited := time.Since(at); !turn || err != nil || waited != want {
			t.Errorf("after a step of %v the next turn = %v, %v after %v, want it after %v", step, turn, err, waited,
				want)
		}
	})
}
