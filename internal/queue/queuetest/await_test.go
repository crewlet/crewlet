package queuetest

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// A WAIT ON A JOURNAL ENDS ON A PAUSE, NEVER ON ITS OWN LENGTH (#1204): a
// backend that keeps handing a case its deliveries, each a little short of
// stallFor after the one before, is waited for however long the whole takes —
// here three pauses of stallFor less a second, nearly three times stallFor —
// and the wait returns on the last delivery, not a moment later.
//
// In a bubble, so each pause is exactly its length and the case costs no
// wall time: the claim is about the bound's arithmetic, and a loaded runner
// would only blur it.
//
// Mutation: restart nothing on a delivery (the bound from the start, as
// settleFor's was), and both journals' waits end at stallFor with one delivery
// of three — a prefix, reported as one.
func TestAWaitThatKeepsReceivingIsNeverCut(t *testing.T) {
	const pause = stallFor - time.Second
	want := []string{"first", "second", "third"}
	took := time.Duration(len(want)) * pause
	if took <= stallFor || took >= hangFor {
		t.Fatalf("the deliveries take %s in all, which has to outlast stallFor (%s) and "+
			"stay inside hangFor (%s), or this case shows nothing", took, stallFor, hangFor)
	}

	t.Run("journal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			j := newJournal()
			// ENDED WITH THE BUBBLE, so a wait that fails leaves no
			// sleeper behind to deadlock it and bury the failure.
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				for _, label := range want {
					select {
					case <-time.After(pause):
					case <-stop:
						return
					}
					j.record(label)
				}
			}()
			start := time.Now()
			j.awaitLabels(t, "deliveries a pause short of stallFor apart", want...)
			if waited := time.Since(start); waited != took {
				t.Fatalf("the wait returned after %s, want %s: on the last delivery", waited, took)
			}
		})
	})

	t.Run("batch journal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := newBatchJournal()
			// ENDED WITH THE BUBBLE, so a wait that fails leaves no
			// sleeper behind to deadlock it and bury the failure.
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				for _, label := range want {
					select {
					case <-time.After(pause):
					case <-stop:
						return
					}
					b.record([]*events.Event{newEvent(label)})
				}
			}()
			start := time.Now()
			b.awaitSizes(t, "batches a pause short of stallFor apart", 1, 1, 1)
			if waited := time.Since(start); waited != took {
				t.Fatalf("the wait returned after %s, want %s: on the last batch", waited, took)
			}
		})
	})
}

// A WAIT WHOSE DELIVERY STOPS STILL ENDS, a whole stallFor after the last
// thing it received — at once from the start when nothing comes at all — and
// ends as a stall; and one whose deliveries never stop but never meet its
// condition ends at the backstop, hangFor from its start, as one. Bounded by
// progress must not become unbounded.
//
// Mutation: let the stall never fire, or restart it without a delivery, and
// the waits that receive nothing run on to the backstop and report it; let
// the backstop never fire, and the endless deliveries run on until they stop,
// twice hangFor in, and end as a stall.
func TestAWaitWhoseDeliveriesStopOrNeverMeetItEnds(t *testing.T) {
	never := func() bool { return false }
	// TWICE THE BACKSTOP of a delivery a second, and then a stall, so a
	// backstop that never fires ends that case as a stall rather than
	// hanging it.
	endless := make([]time.Duration, 2*hangFor/time.Second)
	for i := range endless {
		endless[i] = time.Second
	}
	cases := []struct {
		name string
		// pauses is how long each delivery comes after the one before.
		pauses []time.Duration
		want   waitEnd
		after  time.Duration
	}{
		{name: "nothing is ever delivered", want: endStalled, after: stallFor},
		{name: "delivery stops after one", pauses: []time.Duration{10 * time.Second},
			want: endStalled, after: 10*time.Second + stallFor},
		{name: "deliveries never stop and never meet the condition", pauses: endless,
			want: endBackstop, after: hangFor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				delivered := make(chan struct{}, 1)
				stop := make(chan struct{})
				go func() {
					for _, pause := range c.pauses {
						select {
						case <-time.After(pause):
						case <-stop:
							return
						}
						select {
						case delivered <- struct{}{}:
						case <-stop:
							return
						}
					}
				}()
				start := time.Now()
				ended, met := awaitDeliveries(never, delivered)
				waited := time.Since(start)
				close(stop)
				if met {
					t.Fatal("a condition that never holds was reported met")
				}
				if ended != c.want || waited != c.after {
					t.Fatalf("the wait ended as %s after %s, want %s after %s",
						endName(ended), waited, endName(c.want), c.after)
				}
			})
		})
	}
}

// A PAUSE ENDS A WAIT LONG BEFORE A WINDOW ONLY THE CLOCK ENDS COULD END ON ITS
// OWN: a backend that kept clockedWindow on the wall clock, whatever clock it
// was handed, delivers nothing while the window is open, and its case has to
// stall out before the window would deliver and pass it. Half the window, so
// a runner's pause on top of the stall still lands inside it.
func TestAPauseEndsBeforeAWindowOnlyTheClockEnds(t *testing.T) {
	if 2*stallFor > clockedWindow {
		t.Fatalf("stallFor (%s) is more than half of clockedWindow (%s), so a backend "+
			"that kept the window on the wall clock could deliver before its case stalls",
			stallFor, clockedWindow)
	}
	if stallFor >= hangFor {
		t.Fatalf("stallFor (%s) is not below hangFor (%s), so a steady delivery could "+
			"never outlast a pause before the backstop", stallFor, hangFor)
	}
}

func endName(e waitEnd) string {
	switch e {
	case endStalled:
		return "a stall"
	case endBackstop:
		return "the backstop"
	}
	return fmt.Sprintf("waitEnd(%d)", int(e))
}
