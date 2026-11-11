package fixedwait

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// minTestFiles is the floor under the test-code files the gate reads. The
// module held 3,901 when the gate landed; a walk started at the wrong root,
// or a rule that stopped classing `_test.go` files as test code, finds no
// fixed wait because it judged nothing.
const minTestFiles = 2000

// NO FIXED WAIT DECIDES A TEST'S CLAIM unless the inventory still holds it,
// AND NO MEASURED ELAPSED TIME DOES EITHER. Every reference in test code to
// time.Sleep, time.After, time.AfterFunc, time.NewTimer, time.NewTicker or
// time.Tick, to time.Since or time.Until, and to a time.Time's Sub, Before,
// After or Compare, is inside a synctest bubble, inside a verified
// bubble-only helper, under a marker that says what it is for, or counted by
// an entry of allowed.go that names the piece of the series removing it.
// Every marker marks a reference of its shape, every bubble declaration is
// on a function every caller bubbles, and every entry holds exactly the
// waits and the readings it records, each shape to its own count, so the
// inventory only shrinks. The package doc has the class, the house patterns
// and the markers.
//
// THE LISTING: every reference no entry covers is one line of this test's
// failure, `path:line: function: time.X` (time.Sleep, time.Since,
// time.Time.Sub, time.Time.Before and the rest), all of them — the line the
// inventory is generated from. An over-full function's references follow its
// finding in the same form, every one of them, since the gate cannot say
// which is new: so a reference of a new shape in a function the inventory
// already holds for its waits (an elapsed reading beside an old sleep) is
// listed too.
func TestNoFixedWaitDecidesATestClaimOutsideTheInventory(t *testing.T) {
	t.Parallel()
	tree, err := Scan(sourcetree.Root(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read %d test-code files, %d counted references", tree.TestFiles, len(tree.Refs))
	if tree.TestFiles < minTestFiles {
		t.Fatalf("read %d test-code files, under the floor of %d: the walk is not "+
			"reading the module", tree.TestFiles, minTestFiles)
	}
	var uncovered []string
	for _, f := range Check(tree, allowed) {
		if f.Rule == Uncovered {
			uncovered = append(uncovered, fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.Func, f.Detail))
			continue
		}
		var refs strings.Builder
		for _, r := range f.Refs {
			refs.WriteString("\n" + r.String())
		}
		t.Errorf("%s%s\n%s", f, refs.String(), advice[f.Rule])
	}
	if len(uncovered) > 0 {
		t.Errorf("%d fixed waits or readings of elapsed time in test code that no bubble, marker or "+
			"inventory entry covers.\n"+
			"Decide the claim by a signal instead — a synctest bubble where the claim is about the "+
			"clock (#634, #644, #652), a witness signal, a fence or the producer's own count or state "+
			"for \"nothing happened\" (#642), a wait on the parked state for a head start (#646), "+
			"for a bound on elapsed time the instant a wait was armed with, the bubble's answer "+
			"instant or the signal the bound stood in for (#736), and for an ordering of two "+
			"stamps the order the code records (a log position, a sequence) — or, for a reference "+
			"that decides nothing, say so with //fixedwait:liveness, //fixedwait:simulated <what it "+
			"stands in for> or //fixedwait:bench on a wait, //fixedwait:measure <why it decides "+
			"nothing> on an elapsed reading, and //fixedwait:stated <which instants or clock> on a "+
			"time.Time's Sub, Before, After or Compare between instants no wall clock separates, "+
			"trailing it on its line or alone on the line above. "+
			"`go doc ./internal/fixedwait` has the rules. One per line:\n%s",
			len(uncovered), strings.Join(uncovered, "\n"))
	}
}

// advice is what a person does about each rule other than [Uncovered].
var advice = map[Rule]string{
	OverFull: "Decide the new wait's or reading's claim by a signal, or mark it; " +
		"`go doc ./internal/fixedwait` has the house patterns and the markers.",
	StaleEntry: "Lower or remove the entry in internal/fixedwait/allowed.go in the same diff " +
		"that removed the waits or readings, in the fields of their shape.",
	BadEntry: "Fix the entry in internal/fixedwait/allowed.go; its type says what each field holds.",
	BadMarker: "A fixed wait's marker is //fixedwait:liveness, //fixedwait:simulated <what it " +
		"stands in for> or //fixedwait:bench, the last only in a function that takes a " +
		"*testing.B; a measured elapsed time's is //fixedwait:measure <why it decides nothing>; " +
		"and a time.Time's Sub, Before, After or Compare between instants no wall clock " +
		"separates takes //fixedwait:stated <which instants or clock>, never a time.Since or " +
		"time.Until, which read now, and never two independent readings of the wall clock. A " +
		"line holding a wait and a reading takes one marker alone above it and one trailing " +
		"it (`go doc ./internal/fixedwait`).",
	StaleMarker: "A marker trailing code covers that line alone, and one alone on its line covers " +
		"the line under it alone, and only a reference of its shape the gate counts (not one in " +
		"a bubble), so a second marker alone under the first leaves the first stale. A //fixedwait:bubble goes only on a function that holds a wait or an elapsed " +
		"reading or calls another bubble-only helper.",
	MisplacedBubble: "//fixedwait:bubble goes in the doc comment of the test-code function whose " +
		"every caller runs it in a synctest bubble, and never on a method with an exported name.",
	BubbleEscapes: "Call the helper inside synctest.Test's function, or drop its //fixedwait:bubble " +
		"and decide its waits like any other.",
	Untyped: "The gate cannot type this Sub, Before, After or Compare, so it cannot tell a " +
		"time.Time's, which it counts, from any other type's, which it does not: it tells them " +
		"apart only by the receiver's type. Move it into a file every platform builds, or split " +
		"the file so the part holding it builds everywhere.",
}

// EVERY RULE HAS ITS ADVICE, so no failure reaches a person bare.
func TestEveryRuleOfTheGateSaysWhatToDo(t *testing.T) {
	t.Parallel()
	for _, r := range []Rule{OverFull, StaleEntry, BadEntry, BadMarker, StaleMarker, MisplacedBubble, BubbleEscapes, Untyped} {
		if advice[r] == "" {
			t.Errorf("rule %q has no advice", r)
		}
	}
}

// THE INVENTORY IS ONE THE GATE CAN HOLD: every entry well formed and each
// function named once, whether or not the module still matches it.
func TestTheInventoryIsWellFormed(t *testing.T) {
	t.Parallel()
	for _, f := range Check(&Tree{Decls: map[string]map[string]bool{}}, allowed) {
		if f.Rule == BadEntry {
			t.Error(f)
		}
	}
}
