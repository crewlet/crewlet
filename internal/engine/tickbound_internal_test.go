package engine

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// A TICK'S BOUND CUTS OFF A TICK THAT STOPPED ADVANCING, AND ONLY THAT.
//
// The embedding tick is bounded to cut off one that wedged, and a wedge is the
// absence of progress, not a long life: on one core the semantic index's
// training outlasts any budget a wedge should be given, reading and computing
// every moment of it. So the clock starts again from nothing each time the
// tick shows progress — a stretch of rows read, the end of an exempt step —
// stops while an exempt step runs, and cuts the tick off only once it has run
// the whole budget without either. Each case runs on a bubble's clock, so a
// minute is exact.
func TestATicksBoundCutsOffOnlyATickThatStoppedAdvancing(t *testing.T) {
	t.Parallel()
	const budget = time.Minute
	overran := func(t *testing.T, tick context.Context) bool {
		t.Helper()
		synctest.Wait()
		if tick.Err() == nil {
			return false
		}
		if !errors.Is(context.Cause(tick), errTickOverran) {
			t.Fatalf("the tick ended with cause %v, want errTickOverran", context.Cause(tick))
		}
		return true
	}

	t.Run("a tick that shows no progress is cut off at its budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tick, _, release := boundTick(t.Context(), budget)
			defer release()
			time.Sleep(budget - time.Second)
			if overran(t, tick) {
				t.Fatal("the tick was cut off before its budget")
			}
			time.Sleep(time.Second)
			if !overran(t, tick) {
				t.Fatal("the tick outlived its budget")
			}
		})
	})

	t.Run("progress starts the clock again from nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tick, bound, release := boundTick(t.Context(), budget)
			defer release()
			// TEN BUDGETS OF READING, advancing every fifty seconds: a
			// training's reading on one busy core.
			for range 12 {
				time.Sleep(50 * time.Second)
				if overran(t, tick) {
					t.Fatal("a tick advancing within every budget was cut off")
				}
				bound.Advanced()
			}
			time.Sleep(budget - time.Millisecond)
			if overran(t, tick) {
				t.Fatal("the clock did not start from nothing at the last progress")
			}
			time.Sleep(time.Millisecond)
			if !overran(t, tick) {
				t.Fatal("a tick that stopped advancing outlived its budget")
			}
			if charged, _ := bound.spent(); charged != 12*50*time.Second+budget {
				t.Fatalf("the clock ran %s in all, want %s", charged, 12*50*time.Second+budget)
			}
		})
	})

	t.Run("an exemption stops the clock and its end is progress", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tick, bound, release := boundTick(t.Context(), budget)
			defer release()
			time.Sleep(50 * time.Second)
			resume := bound.Exempt()
			// Ten times the budget, exempt: the arithmetic of a
			// training on one core.
			time.Sleep(10 * budget)
			if overran(t, tick) {
				t.Fatal("the tick was cut off while the clock was stopped")
			}
			// PROGRESS SHOWN WHILE EXEMPT CHANGES NOTHING: the clock is
			// stopped, and starts from nothing at the resume anyway.
			bound.Advanced()
			resume()
			time.Sleep(budget - time.Millisecond)
			if overran(t, tick) {
				t.Fatal("the exemption's end did not start the clock from nothing: " +
					"the tick was cut off early")
			}
			charged, exempt := bound.spent()
			if want := 50*time.Second + budget - time.Millisecond; charged != want ||
				exempt != 10*budget {
				t.Fatalf("spent %s charged and %s exempt, want %s and %s", charged,
					exempt, want, 10*budget)
			}
			time.Sleep(time.Millisecond)
			if !overran(t, tick) {
				t.Fatal("the clock never started again: the tick outlived its budget")
			}
		})
	})

	t.Run("exemptions nest, and the last one open starts the clock", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tick, bound, release := boundTick(t.Context(), budget)
			defer release()
			outer := bound.Exempt()
			inner := bound.Exempt()
			inner()
			// A RESUME TWICE IS ONE RESUME: the second must not close the
			// outer exemption it does not belong to.
			inner()
			time.Sleep(2 * budget)
			if overran(t, tick) {
				t.Fatal("the clock ran while an exemption was still open")
			}
			outer()
			time.Sleep(budget)
			if !overran(t, tick) {
				t.Fatal("the clock did not start when the last exemption closed")
			}
		})
	})

	// CANCELLATION STILL HOLDS: progress and exemptions touch this clock and
	// nothing else, so a stopping engine or a lost lease ends an exempt step
	// at once.
	t.Run("an exemption does not hold off a cancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			parent, stop := context.WithCancel(t.Context())
			tick, bound, release := boundTick(parent, budget)
			defer release()
			resume := bound.Exempt()
			defer resume()
			stop()
			synctest.Wait()
			if !errors.Is(tick.Err(), context.Canceled) ||
				errors.Is(context.Cause(tick), errTickOverran) {
				t.Fatalf("an exempt tick whose parent ended answers %v (cause %v)",
					tick.Err(), context.Cause(tick))
			}
		})
	})

	t.Run("a released tick is cancelled and never overruns", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tick, bound, release := boundTick(t.Context(), budget)
			time.Sleep(30 * time.Second)
			resume := bound.Exempt()
			release()
			resume()
			bound.Advanced()
			time.Sleep(2 * budget)
			synctest.Wait()
			if cause := context.Cause(tick); !errors.Is(cause, context.Canceled) {
				t.Fatalf("a released tick ended with cause %v, want context.Canceled", cause)
			}
			if charged, _ := bound.spent(); charged != 30*time.Second {
				t.Fatalf("a released tick went on charging: %s", charged)
			}
		})
	})
}
