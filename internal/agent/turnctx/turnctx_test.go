package turnctx_test

import (
	"strconv"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
)

// THE ID A TURN REPORTS IS THE ONE THE ORG DERIVES — never a second rule.
//
// The derivation is the org's ([org.Organization.AgentIDFor]), and this type
// only spares its callers the two-value dance. A copy of the rule here would
// be a value that can disagree with the seat beside it on the same event.
func TestAgentIDIsTheOrgsOwnDerivation(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Staff Engineer"}
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}
	want, ok := o.AgentIDFor(seat)
	if !ok {
		t.Fatal("the org derives no id for an agent seat")
	}
	tn := &turnctx.Turn{RunID: "run-1", WorkKey: "t-1", Seat: seat, Org: o}
	if got := tn.AgentID(); got != want.String() {
		t.Fatalf("AgentID() = %q, want the org's %q", got, want)
	}
}

// A SEAT WITH NO AGENT ID REPORTS NONE, rather than a zero uuid.
//
// A human seat is addressable and never spawned, so it has no agent id at
// all. Answering uuid.Nil would give every human in the company the same one
// — the exact confusion [org.Organization.AgentIDFor]'s two-value answer
// exists to prevent, and it would arrive on an event as a real-looking id.
func TestAgentIDIsEmptyForASeatThatHasNone(t *testing.T) {
	t.Parallel()
	human := &org.Role{Name: "Founder", Kind: org.KindHuman}
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{human}}
	if got := (&turnctx.Turn{Seat: human, Org: o}).AgentID(); got != "" {
		t.Fatalf("AgentID() = %q for a human seat, want empty", got)
	}
}

// A TURN BUILT OUTSIDE A COMPANY ANSWERS EMPTY rather than panicking.
//
// A tool surface built for a validate command or a test driving a runner
// directly legitimately has no seat and no org, which is the same bar
// Handle and Role already meet.
func TestAgentIDIsEmptyWithoutASeatOrAnOrg(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Staff Engineer"}
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}
	for name, tn := range map[string]*turnctx.Turn{
		"nil turn": nil,
		"no seat":  {Org: o},
		"no org":   {Seat: seat},
	} {
		if got := tn.AgentID(); got != "" {
			t.Errorf("%s: AgentID() = %q, want empty", name, got)
		}
	}
}

func item(id string) types.WorkItem {
	return types.WorkItem{Backend: types.WorkNative, ID: id, Key: "ENG-" + id, Project: "ENG"}
}

// ONE ITEM IS ONE ITEM, however often and under whatever label it was written.
//
// The set answers "did this turn write to exactly one item", so the same task
// written three times — once before a project rename changed its key and twice
// after — has to count once. Counted by key, the rename alone would turn a
// turn's sole write into two, and the turn would be charged to nothing.
func TestTheWriteSetNamesTheSoleItemItWasWrittenTo(t *testing.T) {
	t.Parallel()
	var w turnctx.Written
	if _, ok := w.Sole(); ok {
		t.Fatal("an empty set names a sole item")
	}
	w.Add(item("7"))
	renamed := item("7")
	renamed.Key, renamed.Project = "CORE-7", "CORE"
	w.Add(renamed)
	w.Add(item("7"))
	got, ok := w.Sole()
	if !ok || got.Ref() != "native:7" {
		t.Fatalf("Sole() = %+v, %v; want the one item written", got, ok)
	}
	// THE FIRST WRITE'S LABEL, since that is the item as the turn found it.
	if got.Key != "ENG-7" {
		t.Errorf("the sole item reads key %q, want the first write's ENG-7", got.Key)
	}
	// A write that names no item is not an item: it must not make one sole
	// write into two.
	w.Add(types.WorkItem{Backend: types.WorkNative})
	if _, ok := w.Sole(); !ok {
		t.Error("a write naming no item was counted as a second item")
	}

	w.Add(item("8"))
	if _, ok := w.Sole(); ok {
		t.Error("a turn that wrote to two items names a sole one")
	}
	items, many := w.Items()
	if len(items) != 2 || many {
		t.Errorf("Items() = %d items, many=%v; want the two, in full", len(items), many)
	}
}

// PAST THE CAP THE SET SAYS "MANY" AND STOPS GROWING, and never says "one".
//
// The cap bounds what a runaway fan-out can make a turn hold; the flag is what
// keeps that bound from changing an answer. A set that silently stopped at 64
// would read as a turn that wrote to 64 items, which is a claim, and a turn
// that wrote to a thousand is not charged to any one of them.
func TestPastTheCapTheWriteSetSaysMany(t *testing.T) {
	t.Parallel()
	var full turnctx.Written
	for i := range turnctx.MaxWritten {
		full.Add(item(strconv.Itoa(i)))
	}
	if items, many := full.Items(); len(items) != turnctx.MaxWritten || many {
		t.Fatalf("at the cap: %d items, many=%v; want %d listed in full",
			len(items), many, turnctx.MaxWritten)
	}
	// Re-writing one the set already holds is not a new item.
	full.Add(item("0"))
	if _, many := full.Items(); many {
		t.Fatal("re-writing a listed item marked the set as having more")
	}
	full.Add(item("overflow"))
	items, many := full.Items()
	if len(items) != turnctx.MaxWritten || !many {
		t.Errorf("past the cap: %d items, many=%v; want %d listed and many",
			len(items), many, turnctx.MaxWritten)
	}
	if _, ok := full.Sole(); ok {
		t.Error("a set past its cap names a sole item")
	}
}

// EVERY CONCURRENT WRITE IS RECORDED. Tool calls and delegate workers of one
// turn run at once and share this set, so it is exercised the way they use it,
// under the race detector.
func TestTheWriteSetRecordsConcurrentWrites(t *testing.T) {
	t.Parallel()
	var w turnctx.Written
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			w.Add(item(strconv.Itoa(i)))
			_, _ = w.Items()
			_, _ = w.Sole()
		})
	}
	wg.Wait()
	if items, _ := w.Items(); len(items) != 8 {
		t.Errorf("recorded %d of 8 concurrent writes", len(items))
	}
}

// A TURN BUILT WITHOUT A SET RECORDS NOTHING rather than panicking — a tool
// surface built for a validate command or a test has no turn to charge.
func TestANilWriteSetRecordsNothing(t *testing.T) {
	t.Parallel()
	var w *turnctx.Written
	w.Add(item("1"))
	if items, many := w.Items(); items != nil || many {
		t.Errorf("a nil set reports %v, many=%v", items, many)
	}
	if _, ok := w.Sole(); ok {
		t.Error("a nil set names a sole item")
	}
}

// A RESUMED TURN'S SET CONTINUES THE PARKED ONE'S — the items in their order,
// and the "wrote more than it lists" mark — so the segment that finishes the
// turn judges a sole write over the whole turn rather than over its last half.
func TestAWriteSetRebuiltFromAParkContinuesIt(t *testing.T) {
	t.Parallel()
	w := turnctx.WrittenFrom([]types.WorkItem{item("1")}, false)
	if got, ok := w.Sole(); !ok || got != item("1") {
		t.Fatalf("Sole = %+v, %v, want the item written before the park", got, ok)
	}
	w.Add(item("2"))
	if _, ok := w.Sole(); ok {
		t.Error("a second item after the park still reads as a sole write")
	}
	if _, ok := turnctx.WrittenFrom([]types.WorkItem{item("1")}, true).Sole(); ok {
		t.Error("a set that had passed its cap before the park reads as one item")
	}
	if items, many := turnctx.WrittenFrom(nil, false).Items(); len(items) != 0 || many {
		t.Errorf("an empty carry rebuilt %v, %v", items, many)
	}
}

// A PHASE-BOUND TURN IS A COPY THAT SHARES WHAT THE TURN SHARES.
//
// The phase is what lets a tool say which leg of the turn acted, and binding
// it must not become a write every leg sees: the executor and a delegate
// worker hold the same turn at once. What the copy points at stays shared,
// or a write a worker committed would be missing from the set the turn is
// charged by.
func TestAPhaseBoundTurnIsACopySharingItsWriteSet(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{RunID: "run-1", WorkKey: "wk", Written: &turnctx.Written{}}
	bound := turn.InPhase(types.PhaseSubagent)
	if bound == turn {
		t.Fatal("InPhase returned the turn itself; binding a phase wrote every leg's view")
	}
	if bound.Phase != types.PhaseSubagent || bound.RunID != "run-1" || bound.WorkKey != "wk" {
		t.Errorf("bound turn = %+v; want the turn's values naming the subagent phase", bound)
	}
	if turn.Phase != "" {
		t.Errorf("the original turn reads phase %q after a bind; want none", turn.Phase)
	}
	bound.Written.Add(item("9"))
	if got, ok := turn.Written.Sole(); !ok || got.Ref() != "native:9" {
		t.Error("a write recorded through the phase-bound turn is missing from the turn's own set")
	}
	var none *turnctx.Turn
	if none.InPhase(types.PhaseExecute) != nil {
		t.Error("binding a phase to no turn produced one")
	}
}
