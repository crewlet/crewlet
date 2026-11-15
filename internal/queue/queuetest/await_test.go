package queuetest

import (
	"fmt"
	"strings"
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
				ended := awaitDeliveries(never, nil, delivered)
				waited := time.Since(start)
				close(stop)
				if ended != c.want || waited != c.after {
					t.Fatalf("the wait ended as %s after %s, want %s after %s",
						endName(ended), waited, endName(c.want), c.after)
				}
			})
		})
	}
}

// A WAIT FOR AN EXACT EXPECTATION ENDS THE MOMENT WHAT IT SAW CAN NO LONGER
// BECOME IT, as diverged, rather than a stallFor later: a journal only grows,
// so a sequence that went wrong, or a label delivered more often than
// expected, is wrong for good. A record still on its way — a prefix, a part of
// the multiset — never ends a wait early, and a record that meets the
// condition is met however the judge would read it.
//
// In a bubble, so "the moment" is exact: the wrong delivery comes a second in
// and the wait returns a second in.
//
// Mutation: drop the judge from a journal's wait, and the diverged waits run
// on to a stall; have a judge call a prefix diverged, and the waits still on
// their way end early; let a multiset take a label twice, and the surplus
// runs on to a stall; read the judge before the condition, and the met
// record ends as diverged.
func TestAWaitEndsAsSoonAsItsExpectationCannotBeMet(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		judged judge[string]
		want   []string
		ends   waitEnd
		after  time.Duration
	}{
		{name: "a sequence that goes wrong", labels: []string{"e1", "e3"},
			judged: inOrder([]string{"e1", "e2", "e3"}), want: []string{"e1", "e2", "e3"},
			ends: endDiverged, after: 2 * time.Second},
		{name: "a sequence one too long", labels: []string{"e1", "e2", "e3"},
			judged: inOrder([]string{"e1", "e2"}), want: []string{"e1", "e2"},
			ends: endMet, after: 2 * time.Second},
		{name: "a multiset given a label it does not hold", labels: []string{"b", "z"},
			judged: inAnyOrder([]string{"a", "b"}), want: []string{"a", "b"},
			ends: endDiverged, after: 2 * time.Second},
		{name: "a multiset given a label twice", labels: []string{"b", "b"},
			judged: inAnyOrder([]string{"a", "b"}), want: []string{"a", "b"},
			ends: endDiverged, after: 2 * time.Second},
		{name: "a prefix that stops", labels: []string{"e1"},
			judged: inOrder([]string{"e1", "e2"}), want: []string{"e1", "e2"},
			ends: endStalled, after: time.Second + stallFor},
		{name: "a multiset out of order", labels: []string{"b", "a"},
			judged: inAnyOrder([]string{"a", "b"}), want: []string{"a", "b"},
			ends: endMet, after: 2 * time.Second},
		// A FENCE's shape: the condition is met by the delivery that also
		// makes the record one the judge calls diverged, and met wins, so
		// the case's own exact read after the wait is what fails it.
		{name: "a record that meets the condition whatever a judge makes of it",
			labels: []string{"x"}, judged: inOrder([]string{"a"}), want: []string{"x"},
			ends: endMet, after: time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				j := newJournal()
				stop := make(chan struct{})
				defer close(stop)
				go func() {
					for _, label := range c.labels {
						select {
						case <-time.After(time.Second):
						case <-stop:
							return
						}
						j.record(label)
					}
				}()
				start := time.Now()
				// The condition a wait for the first len(want) labels has:
				// met once that many are in, so the record one too long is
				// met at its second delivery and the judge never asked.
				ended := j.waitOn(func(seen []string) bool {
					if len(seen) < len(c.want) {
						return false
					}
					return sameLabels(seen[:len(c.want)], c.want)
				}, c.judged)
				if waited := time.Since(start); ended != c.ends || waited != c.after {
					t.Fatalf("the wait ended as %s after %s, want %s after %s",
						endName(ended), waited, endName(c.ends), c.after)
				}
			})
		})
	}

	t.Run("a batch shape that goes wrong", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := newBatchJournal()
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				for _, size := range []int{1, 2} {
					select {
					case <-time.After(time.Second):
					case <-stop:
						return
					}
					evs := make([]*events.Event, size)
					for i := range evs {
						evs[i] = newEvent(fmt.Sprintf("e%d", i))
					}
					b.record(evs)
				}
			}()
			start := time.Now()
			want := []int{1, 1, 1}
			ended := b.waitOn(func([][]string) bool { return equalInts(b.sizes(), want) },
				inOrder(want))
			if waited := time.Since(start); ended != endDiverged || waited != 2*time.Second {
				t.Fatalf("the wait ended as %s after %s, want %s after %s",
					endName(ended), waited, endName(endDiverged), 2*time.Second)
			}
		})
	})
}

// A FAILED WAIT NAMES WHAT ENDED IT, so its reader starts from the right
// suspect: the pause for a wait that saw nothing for stallFor, the backstop for
// one that kept receiving, and no bound at all for one that went wrong. And
// what a judge concludes for a timeout follows what was seen: nothing before
// anything else, unknown with no judge, the judge's word otherwise.
//
// Mutation: name the pause on every end, or let a judged record with nothing
// in it read as partial, and a row fails.
func TestAFailedWaitNamesWhatEndedIt(t *testing.T) {
	for _, c := range []struct {
		end   waitEnd
		unmet bool
		names string
	}{
		{endStalled, false, "nothing was delivered for 30s (stallFor)"},
		{endStalled, true, "the condition did not hold for 30s (stallFor)"},
		{endBackstop, false, "deliveries kept arriving for 2m0s (hangFor, the backstop)"},
		{endDiverged, false, "can no longer become what was expected"},
	} {
		if got := c.end.how(c.unmet); !strings.Contains(got, c.names) {
			t.Errorf("%s (unmet %t) is told as %q, want it to say %q",
				endName(c.end), c.unmet, got, c.names)
		}
	}
	exact := inOrder([]string{"a", "b"})
	for _, c := range []struct {
		judged judge[string]
		seen   []string
		want   deliveryVerdict
	}{
		{exact, nil, verdictNothing},
		{nil, []string{"a"}, verdictUnknown},
		{exact, []string{"a"}, verdictPartial},
		{exact, []string{"b"}, verdictDiverged},
	} {
		if got := c.judged.verdict(c.seen); got != c.want {
			t.Errorf("%v judged %d, want %d", c.seen, got, c.want)
		}
	}
}

// STALLFOR IS AT MOST HALF A CLOCKED WINDOW, AND UNDER THE BACKSTOP. A backend
// that kept clockedWindow on the wall clock, whatever clock it was handed,
// delivers nothing while the window is open, and a case's wait for that
// delivery has to stall out before the window could end on its own and pass
// it. The constants are all this holds: the window opens before the case's
// setup waits inside it, so the order holds for a case whose setup takes less
// than the other half of the window, as a healthy build's does by far, and
// nothing here measures that.
func TestStallForIsAtMostHalfAClockedWindowAndUnderTheBackstop(t *testing.T) {
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
	case endMet:
		return "met"
	case endStalled:
		return "a stall"
	case endBackstop:
		return "the backstop"
	case endDiverged:
		return "diverged"
	}
	return fmt.Sprintf("waitEnd(%d)", int(e))
}
