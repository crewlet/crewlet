package engine

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// ONLY A LOG WHOSE HEALTH GATES ADMISSION TELLS THE CLAIM GATE IT HAS COME
// LEVEL.
//
// The gate's strict half is that every such log is level now, so each of them
// coming level is an event that can open it — and a log whose health the gate
// never reads (a compacted one, whose gaps are its policy working) coming
// level is not, and told, it would ask the gate again on every burst of writes
// that log takes, for nothing.
//
// Mutation: hand every domain the hook, or none, and this fails naming the
// domain.
func TestOnlyTheLogsThatGateAdmissionTellTheClaimGate(t *testing.T) {
	t.Parallel()
	told := 0
	s := &stateLog{admit: func() { told++ }}
	gating := 0
	for _, d := range registeredDomains() {
		hook := s.levelHook(d)
		if !d.GatesSeatAdmission() {
			if hook != nil {
				t.Errorf("%s's health does not gate admission, and its coming "+
					"level tells the claim gate", d.Name())
			}
			continue
		}
		gating++
		if hook == nil {
			t.Errorf("%s's health gates admission, and its coming level tells "+
				"the claim gate nothing", d.Name())
			continue
		}
		before := told
		hook()
		if told != before+1 {
			t.Errorf("%s's level hook did not reach the claim gate", d.Name())
		}
	}
	if gating == 0 {
		t.Fatal("the premise: some registered domain's health gates admission")
	}
	if (&stateLog{}).levelHook(registeredDomains()[0]) != nil {
		t.Error("a log runtime with nobody to tell handed a domain a hook")
	}
}

// AND THE APPLIERS A NODE RUNS FROM BOOT ARE HANDED IT.
//
// A node's appliers start in [New] and come level as they drain their logs,
// before anything has started the seat host: each such moment is one the gate
// may open at. The nudge is buffered, so the boot's arrives whether or not the
// watch that reads it is running yet — which, before [Engine.Start], it is
// not, and nothing else sends on it.
//
// Mutation: drop OnLevel from the runner's deps, and no nudge ever arrives.
func TestALogComingLevelAtBootTellsTheClaimGate(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	SeedStore(t, &b)
	e, err := New(t.Context(), Options{Bootstrap: &b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	select {
	case <-e.claims.nudges:
	case <-time.After(15 * time.Second):
		t.Fatal("no log whose health gates admission told the claim gate it " +
			"had come level, fifteen seconds into a boot over empty logs")
	}
}

// countingHost is the seat host as the claim gate's watch asks it.
type countingHost struct {
	asks atomic.Int64
	// asked receives once per ask, for a case waiting on one.
	asked chan struct{}
}

func (h *countingHost) Resweep() {
	h.asks.Add(1)
	if h.asked != nil {
		h.asked <- struct{}{}
	}
}

// scriptedGate is a claim gate a case opens, counting how often it is asked.
type scriptedGate struct {
	open  atomic.Bool
	asked atomic.Int64
}

func (g *scriptedGate) admits(context.Context) bool {
	g.asked.Add(1)
	return g.open.Load()
}

// A REFUSAL IS ANSWERED ONCE, AND ONLY ONCE THE GATE OPENS.
//
// The watch asks the host for a pass when an event finds the gate open after
// it refused one. Still closed, an event asks nothing and leaves the refusal
// waiting; open, it asks once and disarms, so the next event neither asks for
// another pass nor even asks the gate — the pass it asked for asks the gate
// itself, and re-arms the watch if it is refused.
//
// Mutation: drop the disarm and the second event asks again; drop the gate's
// answer from readmit and the closed gate is answered with a pass.
func TestARefusalIsAnsweredOnceTheGateOpens(t *testing.T) {
	t.Parallel()
	g := &scriptedGate{}
	w := newGateWatch(g.admits)
	host := &countingHost{}
	if w.gate(t.Context()) {
		t.Fatal("the premise: a closed gate admitted a pass")
	}
	w.readmit(t.Context(), host)
	if got := host.asks.Load(); got != 0 {
		t.Fatalf("an event while the gate was still closed asked for %d pass(es)", got)
	}
	g.open.Store(true)
	w.readmit(t.Context(), host)
	if got := host.asks.Load(); got != 1 {
		t.Fatalf("the event that found the gate open after a refusal asked for %d "+
			"pass(es), want one", got)
	}
	before := g.asked.Load()
	w.readmit(t.Context(), host)
	if got := host.asks.Load(); got != 1 {
		t.Errorf("a refusal already answered was answered again: %d asks", got)
	}
	if got := g.asked.Load(); got != before {
		t.Errorf("an event after the refusal was answered asked the gate %d more "+
			"time(s)", got-before)
	}
}

// A NODE WHOSE LAST PASS WAS ADMITTED EVALUATES NOTHING ON AN EVENT.
//
// Every burst of writes a healthy node applies ends with its logs coming level,
// and each is an event: evaluated, every one would cost the broker a health
// read per log, and asked for, a listing of the fleet — for a gate that had
// nothing waiting on it. Disarmed by the pass the gate admitted, even after a
// refusal it never answered.
//
// Mutation: arm the watch on an admission, or leave a refusal armed past one,
// and this fails.
func TestAnEventAfterAnAdmittedPassAsksNothing(t *testing.T) {
	t.Parallel()
	g := &scriptedGate{}
	w := newGateWatch(g.admits)
	host := &countingHost{}
	w.gate(t.Context())
	g.open.Store(true)
	if !w.gate(t.Context()) {
		t.Fatal("the premise: an open gate refused a pass")
	}
	before := g.asked.Load()
	for range 3 {
		w.readmit(t.Context(), host)
	}
	if got := g.asked.Load(); got != before {
		t.Errorf("events after an admitted pass asked the gate %d time(s)", got-before)
	}
	if got := host.asks.Load(); got != 0 {
		t.Errorf("events after an admitted pass asked for %d pass(es)", got)
	}
}

// A PASS THE GATE ADMITS WHILE THE WATCH DECIDES IS NOT FOLLOWED BY ANOTHER.
//
// The watch reads the refusal, then asks the gate. A pass can run in between
// — the tick, or an apply's — and be admitted, which leaves nothing waiting;
// the watch's ask would be a second pass with nothing to claim, spending the
// one early pass an interval honours ahead of the next apply's.
//
// Mutation: disarm with a plain store rather than the compare-and-swap, and
// the host is asked.
func TestAPassAdmittedWhileTheWatchDecidesIsNotFollowedByAnother(t *testing.T) {
	t.Parallel()
	var w *gateWatch
	w = newGateWatch(func(context.Context) bool {
		// THE PASS that runs while the watch asks: admitted, it
		// disarms the watch first.
		w.refused.Store(false)
		return true
	})
	w.refused.Store(true)
	host := &countingHost{}
	w.readmit(t.Context(), host)
	if got := host.asks.Load(); got != 0 {
		t.Errorf("a refusal a pass had already answered was answered again: %d asks", got)
	}
}

// A REFUSAL IS ITSELF AN EVENT FOR THE WATCH.
//
// A log comes level while a pass is deciding — after the pass read its lag,
// before the refusal was recorded — and the watch, told of it then, finds
// nothing armed. With every log level no event follows, so unless the refusal
// tells the watch as it is recorded, the gate the pass raced is next asked by
// the tick, up to an interval of seats unclaimed behind an open gate.
//
// Mutation: drop the gate's nudge on a refusal, and nothing is pending.
func TestARefusalIsItselfAnEventForTheWatch(t *testing.T) {
	t.Parallel()
	g := &scriptedGate{}
	w := newGateWatch(g.admits)
	w.gate(t.Context())
	select {
	case <-w.nudges:
	default:
		t.Fatal("a pass the gate refused told the watch nothing")
	}
	g.open.Store(true)
	w.gate(t.Context())
	select {
	case <-w.nudges:
		t.Error("a pass the gate admitted told the watch, which has nothing to do")
	default:
	}
}

// AND THE WATCH, RUNNING, ASKS FOR THE PASS THE GATE'S OPENING CALLS FOR.
//
// The event loop end to end: a refusal, then the gate opening and a log saying
// so, and the host is asked — without a tick, which this watch has none of.
//
// Mutation: drop the ask from readmit, and no pass is asked for.
func TestTheWatchAsksForAPassWhenTheGateOpens(t *testing.T) {
	t.Parallel()
	g := &scriptedGate{}
	w := newGateWatch(g.admits)
	host := &countingHost{asked: make(chan struct{}, 4)}
	ctx, cancel := context.WithCancel(t.Context())
	var done sync.WaitGroup
	done.Go(func() { w.run(ctx, host, nil) })
	t.Cleanup(func() { cancel(); done.Wait() })

	w.gate(ctx)
	g.open.Store(true)
	w.nudge()
	select {
	case <-host.asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the gate opened after a refusal and the watch asked for no pass")
	}
}
