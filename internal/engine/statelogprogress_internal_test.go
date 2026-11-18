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

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY FIELD A DECISION READS IS ASSIGNED BY health().
//
// Read from the SOURCE rather than by calling health(), which needs a store, a
// broker, a fleet and three running appliers: the property is that the
// functions assembling a health MENTION each field, and an AST walk asserts
// exactly that. It cannot prove an assignment correct (the cases below and
// TestEveryFieldTheDecisionsReadCanChangeTheAnswer in internal/statelog do);
// it proves no field is silently forgotten, leaving its decision on a zero value.
func TestEveryHealthInputIsPopulated(t *testing.T) {
	t.Parallel()

	// The fields [statelog.Health.Refusal], [statelog.Health.Healthy] and the
	// snapshot gate read, listed rather than derived because the list IS the
	// claim. `Drained` is read only by the gate deciding whether this node may
	// donate a snapshot (the shed deliberately ignores it), so a list of the two
	// readiness functions alone would let it go back to a zero value.
	want := []string{"Err", "Stalled", "Evicted", "Floor", "Drained", "Deferred",
		"LastSeq", "StreamRecreated", "GenerationPassed"}

	// THE FUNCTIONS A HEALTH IS ASSEMBLED IN: the entry point, the half read
	// before the published floors land, and the floor that completes it
	// ([readHealthsWith]), the log's ends read now or from memory
	// ([stateLog.observeWith]). A field any of them sets is set.
	assemblers := map[string]bool{"health": false, "observeWith": false, "floored": false}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "statelog.go", nil, 0)
	if err != nil {
		t.Fatalf("parse statelog.go: %v", err)
	}
	assigned := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if _, assembles := assemblers[fn.Name.Name]; !assembles {
			return false
		}
		assemblers[fn.Name.Name] = true
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "health" {
				assigned[sel.Sel.Name] = true
			}
			return true
		})
		return false
	})
	for name, found := range assemblers {
		if !found {
			t.Fatalf("%s is gone from statelog.go; this test names the functions a "+
				"health is assembled in", name)
		}
	}
	for _, field := range want {
		if !assigned[field] {
			t.Errorf("no function assembling a health mentions Health.%s, which "+
				"Refusal, Healthy or the snapshot gate reads — so that decision "+
				"turns on a zero value and the state it is meant to catch goes "+
				"unreported", field)
		}
	}
}

// THE STALL CLOCK RESTARTS WHEN THERE IS NOTHING TO APPLY, which separates a
// stalled node from an idle one: without it a company between two writes would
// refuse reads on every node one StallGrace after its last record.
func TestAnIdleNodeIsNotAStalledOne(t *testing.T) {
	t.Parallel()
	var p progress
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	// Caught up, nothing to do, for well past the grace.
	p.observe(at, seen(100, false), false)
	p.observe(at.Add(statelog.StallGrace*3), seen(100, false), false)
	if p.stalled(at.Add(statelog.StallGrace * 3)) {
		t.Error("an idle caught-up node reported itself stalled")
	}

	// Behind and not moving for the same span IS a stall: this node owes
	// progress it is not making.
	var q progress
	q.observe(at, seen(100, false), true)
	if q.stalled(at.Add(statelog.StallGrace - time.Second)) {
		t.Error("a node behind for less than the grace reported itself stalled")
	}
	if !q.stalled(at.Add(statelog.StallGrace + time.Second)) {
		t.Error("a node behind and frozen past the grace did not report a stall")
	}

	// And progress clears it, without waiting out anything.
	q.observe(at.Add(statelog.StallGrace+time.Second), seen(101, false), true)
	if q.stalled(at.Add(statelog.StallGrace + 2*time.Second)) {
		t.Error("a node that applied a record was still reported stalled")
	}
}

// BEFORE THE FIRST OBSERVATION NOTHING IS STALLED: a refusal derived from no
// measurement is derived from the zero instant, which is every node, always.
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
// record does: restarted on every look the grace would never elapse, and left
// set after the record applied it would shed a node that had just been upgraded.
func TestTheDeferralClockStartsOnceAndClearsAtOnce(t *testing.T) {
	t.Parallel()
	var p progress
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	p.observe(at, seen(10, true), true)
	p.observe(at.Add(time.Minute), seen(10, true), true)
	got := p.deferredSinceValue()
	if !got.Held || !got.Since.Equal(at) {
		t.Errorf("deferredSince = %+v, want held since the FIRST observation %v — "+
			"a clock that restarts every tick never reaches the grace", got, at)
	}

	p.observe(at.Add(2*time.Minute), seen(11, false), true)
	if got := p.deferredSinceValue(); got.Held {
		t.Errorf("deferredSince = %+v after the record applied, so an upgraded "+
			"node stays shed", got)
	}
}

// A STOP THIS PROCESS ASKED FOR IS NOT A FAULT. Every applier returns its run
// context's error on shutdown; read as a halt, `Health.Err` makes `Refusal`
// answer `stalled` for reads the node still serves and `Healthy` go false, so
// the serviceability gate sheds seats a drain is already handing back in order.
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
// That path compares [runningLog.Lag] against [statelog.StallGrace], and a
// backlog over the drain rate stays a fraction of a second while a wedged
// applier runs no batch to re-measure the rate, so a revocation stuck in the
// backlog would never be honoured here. The frozen term ([progress.frozenFor])
// is the figure a wedge cannot hold down.
func TestAWedgedApplierReadsAsStalledOnTheRequestPath(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	var d runningLog
	// Five records behind at the rate this applier last drained at: a
	// fraction of a second, and the figure a wedge leaves standing.
	d.lagNanos.Store(int64(lagDurationOf(105, 100, 20)))
	d.progress.observe(at, seen(100, false), true)
	d.progress.observe(at.Add(statelog.PositionHeartbeat), seen(100, false), true)

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
	d.progress.observe(moved, seen(104, false), true)
	if got := d.lagAt(moved.Add(time.Second)); got > statelog.StallGrace {
		t.Errorf("an applier that applied a record still reads %s behind", got)
	}
}

// A QUIET COMPANY IS NOT A WEDGE: nothing owed, nothing frozen. The frozen term
// is gated on the last look having found work outstanding; without the gate
// every node would answer 503 to every session after a minute without writes.
func TestACaughtUpNodeCarriesNoFrozenLag(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var d runningLog
	d.progress.observe(at, seen(100, false), false)
	if got := d.lagAt(at.Add(statelog.StallGrace * 3)); got != 0 {
		t.Errorf("a caught-up node reads %s behind after a quiet spell", got)
	}
	// And the backlog term is still the whole answer where it is larger.
	d.lagNanos.Store(int64(90 * time.Second))
	if got := d.lagAt(at.Add(time.Minute)); got != 90*time.Second {
		t.Errorf("lag %s, want the 90s backlog term", got)
	}
}

// A LOOK THAT CANNOT READ THE LOG CARRIES THE LAST ONE FORWARD: read as
// "nothing owed", a failed statistics read restarts the stall clock every
// heartbeat, so an applier and its broker connection failing together, the
// ordinary shape of a wedge, never reads as stalled.
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
	p.observe(at, seen(100, false), true)
	for beat := range 8 {
		p.observe(at.Add(time.Duration(beat+1)*statelog.PositionHeartbeat), seen(100, false),
			backlogOf(jetstream.LogStats{}, unreadable, 100, p.owed()))
	}
	if got := p.frozenFor(at.Add(statelog.StallGrace + time.Second)); got <= statelog.StallGrace {
		t.Errorf("frozen %s after failed looks, want past the %s grace: an "+
			"unreadable log restarted the stall clock", got, statelog.StallGrace)
	}
}

// seen is the register row's position for a domain at checkpoint seq, holding
// a record it cannot decode or not — as the position heartbeat publishes it.
func seen(seq uint64, deferred bool) coord.DomainPosition {
	pos := coord.DomainPosition{Seq: seq, AppliedThrough: seq}
	if deferred {
		pos.Deferred = 1
	}
	return pos
}

// A RECORD THIS NODE HOLDS IS NOT A WEDGE.
//
// A node holding a record it cannot decode (a newer peer's mid-upgrade, or one
// under a key it was not restarted with) pins its applied-through below it
// while its checkpoint moves on. Measured on applied-through, a busy log read
// as frozen a minute after the record arrived, and the whole node answered 503
// and shed its seats, which [statelog.Health.Healthy] says a deferral must
// never cause inside its grace; what the record withholds is scoped.
//
// Mutation: measure movement on AppliedThrough and both halves read stalled.
func TestAHeldRecordIsNotAWedge(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var d runningLog
	checkpoint := uint64(100)
	for beat := range 12 {
		pos := coord.DomainPosition{
			Seq: checkpoint, AppliedThrough: 99, Deferred: 1,
		}
		d.progress.observe(at.Add(time.Duration(beat)*statelog.PositionHeartbeat), pos, true)
		checkpoint += 3
	}
	end := at.Add(11 * statelog.PositionHeartbeat)
	if end.Sub(at) <= statelog.StallGrace {
		t.Fatalf("the looks span %s, inside the %s grace: the case proves nothing",
			end.Sub(at), statelog.StallGrace)
	}
	if got := d.lagAt(end.Add(time.Second)); got > statelog.StallGrace {
		t.Errorf("a node whose checkpoint moved at every look while it held one "+
			"record reads %s behind, past the %s grace — every session on it "+
			"answers 503", got, statelog.StallGrace)
	}
	if d.progress.stalled(end.Add(time.Second)) {
		t.Error("a node whose checkpoint moved at every look while it held one " +
			"record reported itself stalled, and sheds its seats")
	}
	// AND THE DEFERRAL IS STILL AGED, for the shed that is its own.
	if got := d.progress.deferredSinceValue(); !got.Held || !got.Since.Equal(at) {
		t.Errorf("deferral clock %+v, want held since the first look %v", got, at)
	}
}
