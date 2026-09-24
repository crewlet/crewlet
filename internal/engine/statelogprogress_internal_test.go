package engine

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY FIELD THE READINESS DECISIONS READ IS ASSIGNED BY health().
//
// A field a decision reads and nothing assigns is a decision that turns on a
// zero value: a `stalled` arm that can never fire, a shed that can never
// happen, an admission gate that never withholds. The state it exists to catch
// goes unreported, and nothing about the decision itself looks wrong.
//
// # Why it reads the SOURCE rather than calling health()
//
// Calling it needs a store, a broker, a fleet and three running appliers, which
// is an integration test nobody writes for a struct field. The property being
// protected is not what the function returns for some input, it is that the
// function MENTIONS each field, and an AST walk asserts exactly that for the
// cost of parsing one file.
//
// It cannot prove the assignment is CORRECT — the behavioural cases below and
// TestEveryFieldTheDecisionsReadCanChangeTheAnswer in internal/statelog do
// that. It proves the field is not silently forgotten.
func TestEveryHealthInputIsPopulated(t *testing.T) {
	t.Parallel()

	// The fields the readiness decisions read that only health() can
	// produce — [statelog.Health.Refusal], [statelog.Health.Healthy] and
	// [statelog.Health.Established] between them. Listed here rather than
	// derived, because the list IS the claim: a field added to a decision
	// and not to this list is a field nobody asked to be produced.
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
			t.Errorf("health() never mentions Health.%s, which a readiness "+
				"decision reads — so that decision turns on a zero value and the "+
				"state it is meant to catch goes unreported", field)
		}
	}
}

// owed is a lag the broker answered with, for [progress.observe].
func owed(n uint64) *uint64 { return &n }

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
	p.observe(at, 100, owed(0), false)
	p.observe(at.Add(statelog.StallGrace*3), 100, owed(0), false)
	if p.stalled(at.Add(statelog.StallGrace * 3)) {
		t.Error("an idle caught-up node reported itself stalled")
	}

	// Behind and not moving for the same span IS a stall: this node owes
	// progress it is not making.
	var q progress
	q.observe(at, 100, owed(5), false)
	if q.stalled(at.Add(statelog.StallGrace - time.Second)) {
		t.Error("a node behind for less than the grace reported itself stalled")
	}
	if !q.stalled(at.Add(statelog.StallGrace + time.Second)) {
		t.Error("a node behind and frozen past the grace did not report a stall")
	}

	// And progress clears it, without waiting out anything.
	q.observe(at.Add(statelog.StallGrace+time.Second), 101, owed(4), false)
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

	p.observe(at, 10, owed(3), true)
	p.observe(at.Add(time.Minute), 10, owed(3), true)
	got := p.deferredSinceValue()
	if !got.Held || !got.Since.Equal(at) {
		t.Errorf("deferredSince = %+v, want held since the FIRST observation %v — "+
			"a clock that restarts every tick never reaches the grace", got, at)
	}

	p.observe(at.Add(2*time.Minute), 11, owed(3), false)
	if got := p.deferredSinceValue(); got.Held {
		t.Errorf("deferredSince = %+v after the record applied, so an upgraded "+
			"node stays shed", got)
	}
}

// CAUGHT UP IS A LATCH: SET BY A LOOK THAT FINDS NOTHING TO APPLY, KEPT WHILE
// THE NODE IS MERELY A RECORD BEHIND, AND CLEARED ONLY BY A STALL THAT BEGAN
// AFTER IT WAS SET.
//
// Every node is a record or two behind for a moment after every write. Read as
// the instant's lag, a busy node withholds admission, skips its snapshot as
// never drained and flaps on the fleet view whenever a look lands in that
// moment. The two edges are the contract: a node that has not yet drained is
// not caught up however recently it started, and a node whose prefix froze with
// work waiting is not caught up however long ago it drained.
//
// Mutation: answer the latch from the instant's lag and the second look reads
// behind; drop the stall's start from the clear and the look that drained in
// the beat after a stall is cleared again.
func TestCaughtUpIsALatchThatOnlyAStallAfterItClears(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	t.Run("not caught up before the first drain", func(t *testing.T) {
		t.Parallel()
		var p progress
		p.observe(at, 10, owed(40), false)
		if p.caughtUp(at.Add(time.Second), false) {
			t.Error("a node that has never drained reported itself caught up")
		}
		// AND A BROKER THAT DID NOT ANSWER IS NO DRAIN.
		p.observe(at.Add(2*time.Second), 10, nil, false)
		if p.caughtUp(at.Add(3*time.Second), false) {
			t.Error("an unanswered look set the latch")
		}
	})

	t.Run("a record behind a moment after draining is still caught up", func(t *testing.T) {
		t.Parallel()
		var p progress
		p.observe(at, 10, owed(0), false)
		if !p.caughtUp(at.Add(time.Second), true) {
			t.Fatal("a look that found nothing to apply did not set the latch")
		}
		// A WRITE LANDS; the next look finds a record past the checkpoint.
		p.observe(at.Add(PositionHeartbeat), 11, owed(1), false)
		if !p.caughtUp(at.Add(PositionHeartbeat+time.Second), false) {
			t.Error("a node one record behind a moment after draining lost the latch")
		}
	})

	t.Run("a stall clears it, and only a drain sets it again", func(t *testing.T) {
		t.Parallel()
		var p progress
		p.observe(at, 10, owed(0), false)
		p.caughtUp(at, true)
		// WORK ARRIVES AND THE PREFIX FREEZES.
		p.observe(at.Add(time.Second), 10, owed(5), false)
		frozen := at.Add(time.Second + statelog.StallGrace + time.Second)
		if p.caughtUp(frozen, false) {
			t.Error("a node frozen past the grace with work waiting kept the latch")
		}
		// IT MOVES AGAIN, still behind: not caught up until it drains.
		p.observe(frozen.Add(time.Second), 12, owed(3), false)
		if p.caughtUp(frozen.Add(2*time.Second), false) {
			t.Error("a node that stalled and is still behind reported itself caught up")
		}
		p.observe(frozen.Add(3*time.Second), 15, owed(0), false)
		if !p.caughtUp(frozen.Add(4*time.Second), false) {
			t.Error("a node that drained after its stall did not recover the latch")
		}
	})

	t.Run("a drain seen before the heartbeat restarts the stall clock holds", func(t *testing.T) {
		t.Parallel()
		var p progress
		// STALLED: the heartbeat's clock says so and keeps saying so until
		// its next beat.
		p.observe(at, 10, owed(5), false)
		stalled := at.Add(statelog.StallGrace + time.Second)
		p.caughtUp(stalled, false)
		// THE NODE DRAINS BETWEEN BEATS, and a read sees it.
		if !p.caughtUp(stalled.Add(time.Second), true) {
			t.Fatal("a read that found nothing to apply did not set the latch")
		}
		// A LATER READ, still before the beat, finds a record past the
		// checkpoint while the stale clock reads stalled.
		if !p.caughtUp(stalled.Add(2*time.Second), false) {
			t.Error("the stall that ended before the drain cleared the latch the " +
				"drain set")
		}
	})

	t.Run("a restart of the loops clears it", func(t *testing.T) {
		t.Parallel()
		var p progress
		p.observe(at, 10, owed(0), false)
		p.caughtUp(at, true)
		p.restart()
		if p.caughtUp(at.Add(time.Second), false) {
			t.Error("the latch survived the loops starting again over other rows")
		}
	})
}

// A CANCELLATION IS NOT A HALT, AND ONLY A DECLARED STOP IS ONE.
//
// A node shutting down, or ending its loops for an adoption, cancels them —
// and a node that read that as a halt would declare itself broken on the way
// out: `Health.Err` set means `Refusal` returns `stalled`, so it refuses the
// reads it is still serving, and `Healthy` goes false, so the serviceability
// gate sheds seats a drain is already handing back in order.
//
// Mutation: report any recorded error as a halt and the undeclared cases are
// halts.
func TestOnlyADeclaredStopIsAHalt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nothing recorded", nil, false},
		{"a stop the runner declared",
			fmt.Errorf("%w: a gate at version 4", statelog.ErrStopped), true},
		{"the node shutting down", context.Canceled, false},
		{"wrapped by the applier", fmt.Errorf("apply loop: %w", context.Canceled), false},
		{"a failure the runner retries in place", errors.New("the store did not answer"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := applierHalted(tc.err); got != tc.want {
				t.Errorf("applierHalted(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// failingBounds is a log whose broker does not answer for its bounds.
type failingBounds struct{ err error }

func (f failingBounds) Bounds(context.Context) (uint64, uint64, error) {
	return 0, 0, f.err
}

// A READ REFUSES AS THE HEALTH READ THAT FAILED.
//
// A broker that did not answer for the log's bounds leaves the published floor
// unread behind it, so a refusal decided from what is left names the floor
// about a failure that was the broker's. A floor published at a generation
// this node has left was read perfectly well, and it is its own code, one a
// caller is told not to come back to this node for. Every other failure leaves
// the floor unestablished and refuses as that — worth coming back for, since
// the next read usually answers.
//
// Mutation: read the bounds without marking the failure as the broker's and
// the unanswered broker refuses `floor_unknown`; map the left generation like
// every other failure and it refuses `floor_unknown`, which sends its caller
// back to a node only an adoption clears.
func TestAReadRefusesAsTheHealthReadThatFailed(t *testing.T) {
	t.Parallel()
	now := time.Now()
	_, _, unanswered := boundsOf(t.Context(), failingBounds{errors.New("nats: timeout")})
	for _, tc := range []struct {
		name  string
		err   error
		want  statelog.ReadRefusal
		retry bool
	}{
		{"the broker did not answer for the bounds", unanswered,
			statelog.RefuseBrokerUnreachable, true},
		{"a floor at a generation this node has left",
			fmt.Errorf("%w: the published floor is at generation 3", errGenerationLeft),
			statelog.RefuseGenerationLeft, false},
		{"coordination did not answer for the floor",
			errors.New("engine: read the fleet's published trim floors: nats: timeout"),
			statelog.RefuseFloorUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := readerHealth(statelog.Health{}, tc.err).Refusal(now)
			if got != tc.want || got.Retryable() != tc.retry {
				t.Errorf("refusal = %q (retryable %v), want %q (retryable %v)",
					got, got.Retryable(), tc.want, tc.retry)
			}
		})
	}
	// AND A HEALTH THAT WAS READ IS THE ONE A READ IS CERTIFIED AGAINST.
	if got := readerHealth(statelog.Health{Stalled: true}, nil); !got.Stalled {
		t.Error("a health read that answered was replaced")
	}
}

// A REPLICATION ROW IS NOT READY WHEREVER A READ ON THE NODE REFUSES.
//
// An evicted node, one below the trim floor and one whose log was rebuilt
// under it can each be caught up on what they hold, and every read on them
// refuses; a row that read ready there would be the one statement about the
// node that is false. And a stall the heartbeat's clock still reports after
// the node drained says so without counting "0 records waiting" as its
// reason.
//
// Mutation: derive the row from the applier's arms alone and the evicted, the
// below-floor and the rebuilt cases read ready; render every stall with its
// lag and the drained one counts zero records waiting.
func TestAReplicationRowIsNotReadyWhereReadsRefuse(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ready := func() statelog.Health {
		zero := uint64(0)
		end := uint64(40)
		return statelog.Health{
			Position: statelog.Position{Stream: "S", Seq: 40},
			CaughtUp: true, Lag: &zero, LastSeq: &end,
			Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: now},
		}
	}
	if row := replicationRow("tracker", ready(), nil, now); !row.Ready {
		t.Fatalf("the control reads %+v, so every case below is vacuous", row)
	}

	for _, tc := range []struct {
		name string
		set  func(*statelog.Health)
		says string
	}{
		{"an evicted node", func(h *statelog.Health) { h.Evicted = true }, "evicted"},
		{"a node below the trim floor", func(h *statelog.Health) {
			h.Floor.State = statelog.FloorBelow
		}, "below the trim floor"},
		{"a log rebuilt under the node", func(h *statelog.Health) {
			h.StreamRecreated = true
		}, "rebuilt"},
		{"a halted applier", func(h *statelog.Health) {
			h.Err = "statelog: the applier stopped: a gate at version 4"
		}, "a gate at version 4"},
		{"a stall the heartbeat reports after the node drained", func(h *statelog.Health) {
			h.Stalled = true
		}, "none are waiting now"},
		{"a stall with records waiting", func(h *statelog.Health) {
			owed := uint64(7)
			h.Stalled, h.Lag = true, &owed
		}, "7 record(s) waiting"},
	} {
		health := ready()
		tc.set(&health)
		row := replicationRow("tracker", health, nil, now)
		if row.Ready {
			t.Errorf("%s reads ready", tc.name)
			continue
		}
		if !strings.Contains(row.Detail, tc.says) {
			t.Errorf("%s reads %q, which does not say %q", tc.name, row.Detail, tc.says)
		}
		if strings.Contains(row.Detail, "0 record(s) waiting") {
			t.Errorf("%s counts zero records waiting as its reason: %q",
				tc.name, row.Detail)
		}
	}

	if row := replicationRow("tracker", statelog.Health{},
		errors.New("nats: timeout"), now); row.Ready || !strings.Contains(row.Detail, "nats: timeout") {
		t.Errorf("a health read that failed reads %+v", row)
	}
}

// THE AGE OF A COMPACTED LOG'S BACKLOG IS ITS FIRST SURVIVOR'S.
//
// A compacted log keeps one record per subject, so the record after a node's
// checkpoint can be gone — superseded by a later one on its subject before
// this node reached it, and never applied here. What this node still owes
// begins at the first record the log holds past the checkpoint, wherever that
// is, and its age is the backlog's.
//
// Mutation: read the record at checkpoint+1 by its own sequence rather than
// the first survivor at or after it, and a compacted backlog has no age.
func TestACompactedBacklogIsAsOldAsItsFirstSurvivor(t *testing.T) {
	t.Parallel()
	q, err := jetstream.Open(t.Context(), jetstream.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	spec := search.Domain{}.Stream()
	if err := q.EnsureDomainStream(t.Context(), jetstream.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: 16 << 20,
		MaxPerSubject: spec.MaxPerSubject, Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the compacted log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	// ONE, TWO, AND A THIRD THAT SUPERSEDES THE FIRST on its subject.
	for i, subject := range []string{"doc-a", "doc-b", "doc-a"} {
		if _, _, err := log.Append(t.Context(), spec.SubjectPrefix+"."+subject,
			fmt.Sprintf("op-%d", i), nil, []byte("vector")); err != nil {
			t.Fatalf("append %d: %v", i+1, err)
		}
	}
	if _, _, _, held, err := log.At(t.Context(), 1); err != nil || held {
		t.Fatalf("the superseded first record is held=%v (%v), so this log is not "+
			"the shape this case names", held, err)
	}
	_, _, survivor, held, err := log.At(t.Context(), 2)
	if err != nil || !held {
		t.Fatalf("read the first survivor: held=%v %v", held, err)
	}

	backlog, end := uint64(3), uint64(3)
	health := statelog.Health{
		Position: statelog.Position{Stream: spec.Name}, Lag: &backlog, LastSeq: &end,
	}
	running := &runningDomain{domain: search.Domain{}, log: log}
	now := survivor.Add(90 * time.Second)
	age, err := (&stateLog{}).applyAge(t.Context(), running, health, now)
	if err != nil {
		t.Fatalf("the age of a compacted backlog: %v", err)
	}
	if want := now.Sub(survivor); age != want {
		t.Errorf("the backlog reads %s old, want %s — the age of the first record "+
			"the log still holds past the checkpoint", age, want)
	}
}
