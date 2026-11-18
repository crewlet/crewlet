package engine

import (
	"go/ast"
	"slices"
	"testing"
	"time"
)

// THE VIEW TRIGGER COMES DOWN BEFORE ANYTHING IT READS FROM.
//
// The directory trigger WRITES what a company routes by: a rebuild registers
// every seat's contact identities in a new party registry, so one run after
// the vendor wiring stopped builds a registry over transports nothing serves.
// And the native half comes down before the core whose logs it reads.
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

// THE CLAIM GATE'S WATCH COMES DOWN BEFORE ANYTHING ITS GATE READS.
//
// The watch evaluates the seat-claim gate on its own goroutine whenever a log
// comes level or the fleet's view lists ([gateWatch]), and the gate reads the
// core's logs, the native halves the router answers from and the fleet's view
// the router routes by. Ended after any of them, an event landing in between
// evaluates a gate over something the teardown has already closed.
//
// Mutation: move stopAdmissionWatch below stopNative, stopCore or
// stopWatchingDataNodes, and this fails naming it.
func TestTheClaimGatesWatchStopsBeforeWhatItsGateReads(t *testing.T) {
	t.Parallel()
	order := callsIn(t, "teardown")
	watch := slices.Index(order, "stopAdmissionWatch")
	if watch < 0 {
		t.Fatalf("the teardown calls %v and never ends the claim gate's watch", order)
	}
	for _, read := range []string{"stopNative", "stopCore", "stopWatchingDataNodes"} {
		at := slices.Index(order, read)
		if at < 0 {
			t.Fatalf("the teardown calls %v and no %s", order, read)
		}
		if at < watch {
			t.Errorf("the teardown runs %s before it ends the claim gate's watch, "+
				"whose gate reads what it stopped", read)
		}
	}
}

// AND THE DUTIES COME DOWN IN THE ORDER THE DOCS STATE: the embedding writer
// before the core's trim and identity duties, every duty before its lease is
// given back, and all of that before the native half and the logs.
//
// The embedding duty publishes into a log the trim is deciding how far to
// purge; a duty ticking after [Engine.releaseDuties] runs beside the peer
// that has just taken its lease; one stopped after the logs publishes into a
// log nothing applies.
//
// Mutation: move stopRetention above stopEmbedding, any duty (the object
// collector included) below releaseDuties, any usage-row writer (Gitlet
// included) below stopUsage, or stopMemorySync above stopMemoryFill.
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
	// AND EVERY WRITER OF A ROW ITS DAY COUNTS IS DOWN BEFORE ITS LAST FLUSH:
	// what one writes after it is published only by a next start, which a
	// node that never starts again lacks. The writers: the learning passes'
	// auxiliary calls, the scheduler's fires, Gitlet's fetches and git reads
	// (a peer's on its git lane too), the ledgers' last flush, and the custody
	// keeper settling a stateless node's rows. (The memory fill only embeds;
	// its order is the memory sync's, below.)
	for _, writer := range []string{"stopLearning", "stopScheduler", "stopGitletHost",
		"stopLedgers", "stopKeeper"} {
		if at(writer) > at("stopUsage") {
			t.Errorf("the teardown runs the usage publisher's last flush before %s, "+
				"so what it writes after that flush is in no published day", writer)
		}
	}
	// The ledgers' last flush once every producer of their records has
	// stopped: of auxiliary spend, the learning passes and the sandbox's
	// condensations; of reads, Gitlet.
	for _, producer := range []string{"stopLearning", "stopSandbox", "stopGitletHost"} {
		if at(producer) > at("stopLedgers") {
			t.Errorf("the teardown flushes the ledgers before %s, whose records then "+
				"reach no flush", producer)
		}
	}
	// THE MEMORY SYNC carries what the memory fill and the learning passes
	// write — a filled vector takes a fresh change sequence — so it ends after
	// both, or their last rows reach no changelog.
	for _, w := range []string{"stopMemoryFill", "stopLearning"} {
		if at(w) > at("stopMemorySync") {
			t.Errorf("the teardown ends the memory sync before %s, whose rows it then "+
				"never carries", w)
		}
	}
	// THE OBJECT COLLECTOR is a duty like the trim, and it deletes bytes the
	// tracker's rows reference: still ticking past the release, it runs beside
	// the peer that has just taken its lease; past the core, it judges an
	// orphan against rows that are closing.
	for _, duty := range []string{"stopEmbedding", "stopUsage", "stopRetention",
		"stopIdentityDuties", "stopObjectCollector"} {
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

// EVERY NODE'S ANSWERERS AND HOLDS COME DOWN BEFORE THE CORE, and so before
// the node's own stop, which follows it and detaches the inboxes.
//
// The fleet history, the steer desk, a seat's memory, a run's live output and,
// on a data node, the estate service and the broker membership are scatter
// subjects this node answers; one still registered as the store and logs close
// answers from a closing file, and a note it accepts reaches no round. The
// pause watch and budget parks hold mailboxes, and one firing into a closing
// client logs a release it could not make.
//
// Mutation: move any of them below stopCore, or stopServingEstate below
// stopNative, and this fails naming it.
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
		"stopExecutorTails", "stopSandboxHost", "stopSeatPauses", "stopBudgetParks",
		"stopProviderParks", "stopServingEstate", "stopServingFleetBroker"} {
		if at(answerer) > at("stopCore") {
			t.Errorf("the teardown stops the core before %s", answerer)
		}
	}
	// The estate service answers from the native half's readers too, and stops
	// being asked before it stops being able to answer, so the next request
	// goes to a peer rather than waiting out its budget here.
	if at("stopServingEstate") > at("stopNative") {
		t.Error("the teardown stops the native half before it withdraws the " +
			"estate service that answers out of it")
	}
}

// EVERY FINAL FLUSH LANDS BEFORE THE CUSTODY THAT CARRIES IT.
//
// On a node without `data` an event reaches the history only through the
// custody buffer, which hands it to a data node's keeper; the authentication
// trail's last minute and the ledgers' last records are final flushes, so one
// run after the custody's lands in a buffer nothing empties. The custody and
// the keeper are never on one node, so their order says nothing; the keeper's
// place is TestTheDutiesStopBeforeTheirLeasesAndTheLogs's.
//
// Mutation: move stopAuthEvents or stopLedgers below stopCustody, and this
// fails naming it.
func TestEveryFinalFlushPrecedesTheCustodyThatCarriesIt(t *testing.T) {
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
	for _, flush := range []string{"stopAuthEvents", "stopLedgers"} {
		if at(flush) > at("stopCustody") {
			t.Errorf("the teardown flushes the custody before %s, whose final "+
				"records then sit in a buffer nothing empties", flush)
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
