package engine

import (
	"go/ast"
	"slices"
	"testing"
	"time"
)

// THE VIEW TRIGGER COMES DOWN BEFORE ANYTHING IT READS FROM.
//
// The directory trigger is not a reader so much as a WRITER of what a company
// routes by: a rebuild registers every seat's contact identities in a new
// party registry. Ended with the logs at the bottom of the teardown, an
// identity record landing after the vendor wiring had been stopped rebuilt a
// registry over transports nothing serves. And the native half comes down
// before the core whose logs it reads.
//
// Read from the SOURCE, for [TestEveryVendorReconcilerRunsOnApply]'s reason:
// the window is a race no case can open on demand. Mutation: move
// stopViewTriggers below stopScheduler, or stopCore above stopNative, and
// this fails naming the pair.
func TestTheViewTriggersStopBeforeWhatTheyReArm(t *testing.T) {
	t.Parallel()
	order := callsIn(t, "teardown")
	at := func(name string) int {
		t.Helper()
		i := slices.Index(order, name)
		if i < 0 {
			t.Fatalf("the teardown calls %v and no %s", order, name)
		}
		return i
	}
	for _, rearmed := range []string{"stopScheduler", "stopNotifications",
		"stopMaintenance", "stopSandbox"} {
		if at("stopViewTriggers") > at(rearmed) {
			t.Errorf("the teardown ends %s before the view trigger, which "+
				"rebuilds over what it stopped on the next identity record", rearmed)
		}
	}
	if at("stopNative") > at("stopCore") {
		t.Error("the teardown stops the core's logs before the native half " +
			"that reads them")
	}
}

// AND THE DUTIES COME DOWN IN THE ORDER THE DOCS STATE: the native half's
// embedding writer before the core's trim and identity duties, every one
// before the node gives its duty leases back, and all of that before the
// native half and the logs.
//
// The embedding duty publishes vector records into a log the trim is deciding
// how far to purge, so it stops first; a duty still ticking after
// [Engine.releaseDuties] runs beside the peer that has just taken its lease;
// and a duty stopped after the logs publishes into a log nothing applies. The
// control-plane page and the engine's row said the native half came down
// before the core's duties, which the teardown has never done — this holds
// the order it does, so the next sentence written about it has something to
// be checked against.
//
// Mutation: move stopRetention above stopEmbedding, or any duty below
// releaseDuties, and this fails naming the pair.
func TestTheDutiesStopBeforeTheirLeasesAndTheLogs(t *testing.T) {
	t.Parallel()
	order := callsIn(t, "teardown")
	at := func(name string) int {
		t.Helper()
		i := slices.Index(order, name)
		if i < 0 {
			t.Fatalf("the teardown calls %v and no %s", order, name)
		}
		return i
	}
	for _, core := range []string{"stopRetention", "stopIdentityDuties"} {
		if at("stopEmbedding") > at(core) {
			t.Errorf("the teardown ends %s before the embedding duty, which "+
				"publishes into a log the trim is deciding how far to purge", core)
		}
	}
	// THE USAGE PUBLISHER is no duty and holds no lease, but it writes the
	// usage log the trim purges, exactly as the embedding duty writes the
	// vectors': down before the trim, and long before the logs.
	if at("stopUsage") > at("stopRetention") {
		t.Error("the teardown ends the trim before the usage publisher, which " +
			"publishes into a log the trim is deciding how far to purge")
	}
	for _, duty := range []string{"stopEmbedding", "stopUsage", "stopRetention", "stopIdentityDuties"} {
		for _, later := range []string{"releaseDuties", "stopNative", "stopCore"} {
			if at(duty) > at(later) {
				t.Errorf("the teardown calls %s before %s, so the duty is "+
					"still ticking after it", later, duty)
			}
		}
	}
	if at("releaseDuties") > at("stopNative") {
		t.Error("the teardown stops the native half before it gives the duty " +
			"leases back")
	}
}

// EVERY NODE'S ANSWERERS AND HOLDS COME DOWN BEFORE THE CORE — and so before
// the node's own stop, which follows it and detaches the inboxes.
//
// The fleet history, the steer desk, a seat's memory and a run's live output
// are each a scatter subject this node answers for, armed at boot on every
// node; one still registered when the store and the logs close answers a
// peer's question from a file that is closing, and a note it accepts reaches
// no round. The pause watch and the budget parks take holds on mailboxes, and
// one firing into a closing client would log a release it could not make.
//
// Mutation: move any of them below stopCore, and this fails naming it.
func TestTheAnswerersAndTheHoldsStopBeforeTheCore(t *testing.T) {
	t.Parallel()
	order := callsIn(t, "teardown")
	at := func(name string) int {
		t.Helper()
		i := slices.Index(order, name)
		if i < 0 {
			t.Fatalf("the teardown calls %v and no %s", order, name)
		}
		return i
	}
	for _, answerer := range []string{"stopHistory", "stopSteer", "stopMemoryReads",
		"stopSandboxTails", "stopSeatPauses", "stopBudgetParks"} {
		if at(answerer) > at("stopCore") {
			t.Errorf("the teardown stops the core before %s", answerer)
		}
	}
}

// AND ENDING IT ENDS IT: a nudge left after stopViewTriggers is one nothing
// consumes, where a running trigger takes it within moments. The control runs
// first, on the same node, so a nudge that sat unconsumed is the stop's doing
// rather than a trigger that never ran.
func TestEndingTheViewTriggersStopsTheDirectoryRebuilding(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	consumed := func() bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if len(e.directoryNudge) == 0 {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	e.nudgeDirectory()
	if !consumed() {
		t.Fatal("the control: a running trigger never took the nudge")
	}
	e.stopViewTriggers()
	e.nudgeDirectory()
	time.Sleep(200 * time.Millisecond)
	if len(e.directoryNudge) == 0 {
		t.Error("a nudge after the view trigger was ended was consumed: " +
			"something is still rebuilding the party registry")
	}
}

// callsIn is the order of the `e.<method>(…)` calls one *Engine method's body
// makes, however deeply nested.
func callsIn(t *testing.T, method string) []string {
	t.Helper()
	for _, file := range enginePackage(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != method || !receiverIsEngine(fn) {
				continue
			}
			var out []string
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "e" {
					out = append(out, sel.Sel.Name)
				}
				return true
			})
			return out
		}
	}
	t.Fatalf("no (*Engine).%s in the engine package", method)
	return nil
}
