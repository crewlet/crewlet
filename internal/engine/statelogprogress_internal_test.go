package engine

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY FIELD THE READINESS DECISIONS READ IS ASSIGNED BY health().
//
// This is the test whose absence let the whole readiness path be dead code.
// [statelog.Health] has thirteen fields; [stateLog.health] assembled seven of
// them, and the four the decisions actually turn on — Err, Stalled, Evicted
// and Floor — were assigned by nothing at all. Every arm of
// [statelog.Health.Refusal] was therefore unreachable and
// [statelog.Health.Healthy] could never go false, so a halted applier kept
// answering reads and the `deferred_old` alarm's promise that "its seats have
// already moved" was false on every node.
//
// # Why it reads the SOURCE rather than calling health()
//
// Calling it needs a store, a broker, a fleet and three running appliers —
// which is why the gap was never covered by an ordinary test: the honest
// version is an integration test nobody writes for a struct field. The
// property being protected is not what the function returns for some input,
// it is that the function MENTIONS each field, and an AST walk asserts
// exactly that for the cost of parsing one file.
//
// It cannot prove the assignment is CORRECT — the behavioural cases below and
// TestEveryFieldTheDecisionsReadCanChangeTheAnswer in internal/statelog do
// that. It proves the field is not silently forgotten, which is the failure
// that actually happened.
func TestEveryHealthInputIsPopulated(t *testing.T) {
	t.Parallel()

	// The fields [statelog.Health.Refusal] and [statelog.Health.Healthy]
	// read. Listed here rather than derived, because the list IS the
	// claim: a field added to a decision and not to this list is a field
	// nobody asked to be produced.
	want := []string{"Err", "Stalled", "Evicted", "Floor", "CaughtUp", "Deferred",
		"LastSeq", "StreamRecreated"}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "statelog.go", nil, 0)
	if err != nil {
		t.Fatalf("parse statelog.go: %v", err)
	}
	var body *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && fn.Name.Name == "health" && fn.Recv != nil {
			body = fn
		}
		return body == nil
	})
	if body == nil {
		t.Fatal("(*stateLog).health is gone; this test names the function it guards")
	}

	assigned := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "health" {
			assigned[sel.Sel.Name] = true
		}
		return true
	})
	for _, field := range want {
		if !assigned[field] {
			t.Errorf("health() never mentions Health.%s, which Refusal or Healthy "+
				"reads — so that decision turns on a zero value and the state it "+
				"is meant to catch goes unreported", field)
		}
	}
}

// THE STALL CLOCK RESTARTS WHEN THERE IS NOTHING TO APPLY, which is what
// separates a stalled node from an idle one.
//
// Without it a company between two writes would declare every node stalled
// one StallGrace after its last record — refusing reads on a fleet whose only
// fault is that nobody has filed anything for a minute.
func TestAnIdleNodeIsNotAStalledOne(t *testing.T) {
	t.Parallel()
	var p progress
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	// Caught up, nothing to do, for well past the grace.
	p.observe(at, 100, false, false)
	p.observe(at.Add(statelog.StallGrace*3), 100, false, false)
	if p.stalled(at.Add(statelog.StallGrace * 3)) {
		t.Error("an idle caught-up node reported itself stalled")
	}

	// Behind and not moving for the same span IS a stall: this node owes
	// progress it is not making.
	var q progress
	q.observe(at, 100, true, false)
	if q.stalled(at.Add(statelog.StallGrace - time.Second)) {
		t.Error("a node behind for less than the grace reported itself stalled")
	}
	if !q.stalled(at.Add(statelog.StallGrace + time.Second)) {
		t.Error("a node behind and frozen past the grace did not report a stall")
	}

	// And progress clears it, without waiting out anything.
	q.observe(at.Add(statelog.StallGrace+time.Second), 101, true, false)
	if q.stalled(at.Add(statelog.StallGrace + 2*time.Second)) {
		t.Error("a node that applied a record was still reported stalled")
	}
}

// BEFORE THE FIRST OBSERVATION NOTHING IS STALLED. A node whose heartbeat has
// not run yet has measured nothing, and a refusal derived from no measurement
// is a refusal derived from the zero instant — which is every node, always.
func TestAnUnobservedDomainIsNotStalled(t *testing.T) {
	t.Parallel()
	var p progress
	if p.stalled(time.Now()) {
		t.Error("a domain nothing has observed reported itself stalled")
	}
	if got := p.deferredSinceValue(); got.Held {
		t.Errorf("a domain nothing has observed reported a deferral: %+v", got)
	}
}

// THE DEFERRAL CLOCK IS THE OLDEST ARRIVAL, and it clears the moment the
// record does.
//
// Both halves matter. Restarting it on every observation would mean the grace
// never elapsed and the shed never fired; leaving it set after the record
// applied would shed a node that had just been upgraded — punishing the fix.
func TestTheDeferralClockStartsOnceAndClearsAtOnce(t *testing.T) {
	t.Parallel()
	var p progress
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	p.observe(at, 10, true, true)
	p.observe(at.Add(time.Minute), 10, true, true)
	got := p.deferredSinceValue()
	if !got.Held || !got.Since.Equal(at) {
		t.Errorf("deferredSince = %+v, want held since the FIRST observation %v — "+
			"a clock that restarts every tick never reaches the grace", got, at)
	}

	p.observe(at.Add(2*time.Minute), 11, true, false)
	if got := p.deferredSinceValue(); got.Held {
		t.Errorf("deferredSince = %+v after the record applied, so an upgraded "+
			"node stays shed", got)
	}
}

// A STOP THIS PROCESS ASKED FOR IS NOT A FAULT.
//
// Every applier returns its run context's error when the node shuts down, and
// reading that as a halt makes a node declare itself broken on the way out:
// `Health.Err` set means `Refusal` returns `stalled`, so it refuses the reads
// it is still serving, and `Healthy` goes false, so the serviceability gate
// sheds seats a drain is already handing back in order.
//
// Measured on the three-node e2e the moment `Err` was first populated: six
// `statelog_applier_stopped` at `context canceled` and four
// `seats_shed_unserviceable` behind them, all during teardown.
func TestACancelledApplierIsNotAHaltedOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the node shutting down", context.Canceled, false},
		{"wrapped by the applier", fmt.Errorf("apply loop: %w", context.Canceled), false},
		{"a record it cannot decode", errors.New("record at version 3"), true},
		{"a deadline, which is a real stall", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.err != nil && !errors.Is(tc.err, context.Canceled)
			if got != tc.want {
				t.Errorf("halted = %v, want %v for %v", got, tc.want, tc.err)
			}
		})
	}
}

// A WEDGED APPLIER READS AS STALLED ON THE REQUEST PATH, AND NOT ONLY ON A
// HEALTH READ.
//
// The request path — every session validated, every seat a bound credential
// acts as — compares [runningDomain.Lag] against [statelog.StallGrace], and
// the lag was a backlog over a drain rate alone. That rate is measured over
// apply time and nothing re-measures it while no batch runs, so an applier
// that wedged with five records outstanding kept reading as a fraction of a
// second behind for as long as it stayed wedged: the session table's
// `stalled` row never fired and a revocation stuck in that backlog was never
// honoured on this node. How long the prefix has been frozen is the figure a
// wedge cannot hold down.
func TestAWedgedApplierReadsAsStalledOnTheRequestPath(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	var d runningDomain
	// Five records behind at the rate this applier last drained at: a
	// fraction of a second, and the figure a wedge leaves standing.
	d.lagNanos.Store(int64(lagDurationOf(105, 100, 20)))
	d.progress.observe(at, 100, true, false)
	d.progress.observe(at.Add(PositionHeartbeat), 100, true, false)

	if got := d.lagAt(at.Add(statelog.StallGrace - time.Second)); got > statelog.StallGrace {
		t.Errorf("lag %s inside the grace: a node behind for less than it "+
			"must still be served", got)
	}
	if got := d.lagAt(at.Add(statelog.StallGrace + time.Second)); got <= statelog.StallGrace {
		t.Errorf("an applier frozen with work outstanding for %s reads %s "+
			"behind, inside the %s grace — so every table on the request path "+
			"serves it as a caught-up node", statelog.StallGrace+time.Second,
			got, statelog.StallGrace)
	}

	// PROGRESS CLEARS IT at the next look, without waiting anything out.
	moved := at.Add(statelog.StallGrace + 2*time.Second)
	d.progress.observe(moved, 104, true, false)
	if got := d.lagAt(moved.Add(time.Second)); got > statelog.StallGrace {
		t.Errorf("an applier that applied a record still reads %s behind", got)
	}
}

// A QUIET COMPANY IS NOT A WEDGE: nothing owed, nothing frozen.
//
// The frozen term is gated on the last look having found work outstanding.
// Without the gate every node's lag would climb between two writes, and a
// company nobody had filed anything in for a minute would answer 503 to every
// session on every node.
func TestACaughtUpNodeCarriesNoFrozenLag(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var d runningDomain
	d.progress.observe(at, 100, false, false)
	if got := d.lagAt(at.Add(statelog.StallGrace * 3)); got != 0 {
		t.Errorf("a caught-up node reads %s behind after a quiet spell", got)
	}
	// And the backlog term is still the whole answer where it is larger.
	d.lagNanos.Store(int64(90 * time.Second))
	if got := d.lagAt(at.Add(time.Minute)); got != 90*time.Second {
		t.Errorf("lag %s, want the 90s backlog term", got)
	}
}

// A LOOK THAT CANNOT READ THE LOG CARRIES THE LAST ONE FORWARD.
//
// Read as "nothing owed", a failed statistics read restarted the stall clock
// on every heartbeat — and an applier and its broker connection failing
// together is the ordinary shape of a wedge, so exactly that wedge never read
// as stalled.
func TestAnUnreadableLogKeepsTheLastBacklogAnswer(t *testing.T) {
	t.Parallel()
	unreadable := errors.New("stream info: nats: timeout")
	for _, tc := range []struct {
		name       string
		stats      jetstream.LogStats
		err        error
		previously bool
		want       bool
	}{
		{"behind, read", jetstream.LogStats{LastSeq: 105}, nil, false, true},
		{"caught up, read", jetstream.LogStats{LastSeq: 100}, nil, true, false},
		{"unreadable after a look that found work", jetstream.LogStats{}, unreadable, true, true},
		{"unreadable after a look that found none", jetstream.LogStats{}, unreadable, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := backlogOf(tc.stats, tc.err, 100, tc.previously); got != tc.want {
				t.Errorf("backlogOf = %v, want %v", got, tc.want)
			}
		})
	}

	// And through the tracker: a wedge whose every later look fails is
	// still frozen from the last look that could see it.
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var p progress
	p.observe(at, 100, true, false)
	for beat := range 8 {
		p.observe(at.Add(time.Duration(beat+1)*PositionHeartbeat), 100,
			backlogOf(jetstream.LogStats{}, unreadable, 100, p.owed()), false)
	}
	if got := p.frozenFor(at.Add(statelog.StallGrace + time.Second)); got <= statelog.StallGrace {
		t.Errorf("frozen %s after failed looks, want past the %s grace: an "+
			"unreadable log restarted the stall clock", got, statelog.StallGrace)
	}
}
