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

// NO FIXED WAIT DECIDES A TEST'S CLAIM unless the inventory still holds it.
// Every reference in test code to time.Sleep, time.After, time.AfterFunc,
// time.NewTimer, time.NewTicker or time.Tick is inside a synctest bubble,
// inside a verified bubble-only helper, under a marker that says what it is
// for, or counted by an entry of allowed.go that names the piece of the
// series removing it. Every marker marks a wait, every bubble declaration is
// on a function every caller bubbles, and every entry holds exactly the
// references it records, so the inventory only shrinks. The package doc has
// the class, the house patterns and the markers.
//
// THE LISTING: every reference no entry covers is one line of this test's
// failure, `path:line: function: time.X`, all of them — the line the
// inventory is generated from.
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
		t.Errorf("%s\n%s", f, advice[f.Rule])
	}
	if len(uncovered) > 0 {
		t.Errorf("%d fixed waits in test code that no bubble, marker or inventory entry covers.\n"+
			"Decide the claim by a signal instead — a synctest bubble where the claim is about the "+
			"clock (#634, #644, #652), a witness signal, a fence or the producer's own count or state "+
			"for \"nothing happened\" (#642), a wait on the parked state for a head start (#646) — or, "+
			"for a wait that decides nothing, say so with //fixedwait:liveness, "+
			"//fixedwait:simulated <what it stands in for> or //fixedwait:bench, trailing the wait on its line "+
			"or alone on the line above. `go doc ./internal/fixedwait` has the rules. One per line:\n%s",
			len(uncovered), strings.Join(uncovered, "\n"))
	}
}

// advice is what a person does about each rule other than [Uncovered].
var advice = map[Rule]string{
	OverFull: "Decide the new wait's claim by a signal, or mark it; `go doc ./internal/fixedwait` " +
		"has the house patterns and the markers.",
	StaleEntry: "Lower or remove the entry in internal/fixedwait/allowed.go in the same diff " +
		"that removed the waits.",
	BadEntry: "Fix the entry in internal/fixedwait/allowed.go; its type says what each field holds.",
	BadMarker: "A marker is //fixedwait:liveness, //fixedwait:simulated <what it stands in for> " +
		"or //fixedwait:bench, the last only in a function that takes a *testing.B " +
		"(`go doc ./internal/fixedwait`).",
	StaleMarker: "A marker trailing code covers that line alone, and one alone on its line covers " +
		"the line under it alone, and only a wait the gate counts (not one in a bubble). A " +
		"//fixedwait:bubble goes only on a function that holds a wait or calls another " +
		"bubble-only helper.",
	MisplacedBubble: "//fixedwait:bubble goes in the doc comment of the test-code function whose " +
		"every caller runs it in a synctest bubble, and never on a method with an exported name.",
	BubbleEscapes: "Call the helper inside synctest.Test's function, or drop its //fixedwait:bubble " +
		"and decide its waits like any other.",
}

// EVERY RULE HAS ITS ADVICE, so no failure reaches a person bare.
func TestEveryRuleOfTheGateSaysWhatToDo(t *testing.T) {
	t.Parallel()
	for _, r := range []Rule{OverFull, StaleEntry, BadEntry, BadMarker, StaleMarker, MisplacedBubble, BubbleEscapes} {
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
