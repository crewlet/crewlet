package tracker_test

import (
	"math"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A MINTED KEY IS STRICTLY BETWEEN ITS NEIGHBOURS, AT EVERY SHAPE.
//
// The whole of the manual order rests on this one property: a drag writes one
// row, and it is correct only if the key it wrote sorts where the person
// dropped it.
func TestAMintedKeyLandsStrictlyBetweenItsNeighbours(t *testing.T) {
	t.Parallel()
	pairs := [][2]tracker.Rank{
		{"", ""},
		{"", "a0"},
		{"a0", ""},
		{"a0", "a1"},
		{"a0", "a0V"},
		{"Zz", "a0"},
		{"azz", "b100"},
		{"a0", "b00"},
		{"a0V", "a1"},
	}
	for _, p := range pairs {
		a, b := p[0], p[1]
		got, err := tracker.KeyBetween(a, b)
		if err != nil {
			t.Errorf("KeyBetween(%q, %q): %v", a, b, err)
			continue
		}
		if !got.Valid() {
			t.Errorf("KeyBetween(%q, %q) minted %q, which is not a well-formed key",
				a, b, got)
		}
		if a != "" && string(got) <= string(a) {
			t.Errorf("KeyBetween(%q, %q) = %q, which does not sort above %q", a, b, got, a)
		}
		if b != "" && string(got) >= string(b) {
			t.Errorf("KeyBetween(%q, %q) = %q, which does not sort below %q", a, b, got, b)
		}
	}
}

// THE TWO ORDERINGS THE MAGNITUDE HEAD EXISTS FOR.
//
// Both are plain byte comparisons and both are false for any encoding that
// decrements the first character instead of borrowing through it. They are
// asserted as literals because they are what a reader checks the design
// against.
func TestTheHeadOrdersAcrossItsOwnBoundaries(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{{"Zz", "a0"}, {"azz", "b100"}, {"A0", "Zz"}} {
		if !(pair[0] < pair[1]) {
			t.Errorf("%q does not sort below %q, so the magnitude head is not "+
				"doing the one thing it exists for", pair[0], pair[1])
		}
	}
}

// REPEATED HEAD INSERTION GROWS THE KEY LOGARITHMICALLY, NOT LINEARLY.
//
// This is the capacity the flat encoding did not have: decrementing the first
// character gave THIRTY head placements per project, ever. The bound is
// 2 + ceil(log62(n)); at 100 000 insertions that is 5 and the real answer is
// 4, so it is a bound rather than an equality — which is the honest way to
// state it and the reason the test asserts ≤ rather than ==.
func TestRepeatedHeadInsertionGrowsLogarithmically(t *testing.T) {
	t.Parallel()
	const inserts = 100_000
	head := tracker.Rank(tracker.RankOrigin)
	longest := len(head)
	for i := range inserts {
		next, err := tracker.KeyBetween("", head)
		if err != nil {
			t.Fatalf("head insertion %d: %v", i, err)
		}
		if string(next) >= string(head) {
			t.Fatalf("head insertion %d minted %q, which does not sort below %q",
				i, next, head)
		}
		head = next
		if len(head) > longest {
			longest = len(head)
		}
	}
	bound := 2 + int(math.Ceil(math.Log(inserts)/math.Log(62)))
	if longest > bound {
		t.Fatalf("%d head insertions grew the key to %d characters against a "+
			"bound of %d", inserts, longest, bound)
	}
	if longest > 4 {
		t.Errorf("%d head insertions reached %d characters; the measured figure "+
			"this design is stated against is 4, and a regression here is the "+
			"encoding quietly changing shape", inserts, longest)
	}
}

// A TAIL DRAG NEVER WIDENS THE INTEGER PART.
//
// A drag to the end subdivides the gap below the next integer position rather
// than consuming one, so it cannot exhaust the create lattice however many
// times it runs — which is what makes "a create can never collide with a
// move" a statement about the value rather than about a counter.
func TestRepeatedTailDragsDoNotConsumeTheCreateLattice(t *testing.T) {
	t.Parallel()
	max, err := tracker.IntegerAt(1000)
	if err != nil {
		t.Fatalf("IntegerAt: %v", err)
	}
	ceiling, err := tracker.IntegerAt(1001)
	if err != nil {
		t.Fatalf("IntegerAt: %v", err)
	}
	cur := max
	for i := range 500 {
		next, err := tracker.KeyBetween(cur, ceiling)
		if err != nil {
			t.Fatalf("tail drag %d: %v", i, err)
		}
		if string(next) <= string(cur) || string(next) >= string(ceiling) {
			t.Fatalf("tail drag %d minted %q outside (%q, %q)", i, next, cur, ceiling)
		}
		if next.Fraction() == "" {
			t.Fatalf("tail drag %d minted %q, a PURE INTEGER — that is a create's "+
				"shape, and the two mint sets are disjoint only because a move "+
				"never produces one above the origin", i, next)
		}
		cur = next
	}
}

// THE TWO MINT SETS ARE DISJOINT BY THE SHAPE OF THE VALUE ALONE.
//
// The proof mentions no counter, no high-water mark, no staleness and no
// in-flight create — which is why it survives out-of-order create landings
// and head placements, on which the bound this replaces failed with no
// concurrency at all.
func TestTheTwoMintSetsAreDisjointByShape(t *testing.T) {
	t.Parallel()
	// Every create's key.
	for n := uint64(1); n <= 5_000; n++ {
		key, err := tracker.IntegerAt(n)
		if err != nil {
			t.Fatalf("IntegerAt(%d): %v", n, err)
		}
		if !key.FromCreate() {
			t.Fatalf("create %d minted %q, which is not in the create lattice", n, key)
		}
		if key.HeadPlacement() {
			t.Fatalf("create %d minted %q, which is also a head placement", n, key)
		}
	}
	// Every head placement.
	head := tracker.Rank(tracker.RankOrigin)
	for i := range 200 {
		next, err := tracker.KeyBetween("", head)
		if err != nil {
			t.Fatalf("head insertion %d: %v", i, err)
		}
		if !next.HeadPlacement() {
			t.Fatalf("head insertion %d minted %q, which is not a head placement",
				i, next)
		}
		if next.FromCreate() {
			t.Fatalf("head insertion %d minted %q, which is also a create's key",
				i, next)
		}
		head = next
	}
	// And every other move.
	between, err := tracker.KeysBetween("a0", "a1", 64)
	if err != nil {
		t.Fatalf("KeysBetween: %v", err)
	}
	for _, k := range between {
		if k.Fraction() == "" {
			t.Fatalf("%q was minted between two neighbours and carries no "+
				"fraction, so it is indistinguishable from a create's key", k)
		}
		if k.FromCreate() || k.HeadPlacement() {
			t.Fatalf("%q is in a mint set it was not minted by", k)
		}
	}
}

// A CREATE'S KEY IS STRICTLY THE NEW MAXIMUM.
//
// New tasks land at the tail in creation order, absolutely and with no
// arbitration of their own — and a drag can never overtake one, because every
// move's key is strictly below the next integer a create will mint.
func TestACreatesKeyIsStrictlyTheNewMaximum(t *testing.T) {
	t.Parallel()
	prev := tracker.Rank("")
	for n := uint64(1); n <= 4_000; n++ {
		key, err := tracker.IntegerAt(n)
		if err != nil {
			t.Fatalf("IntegerAt(%d): %v", n, err)
		}
		if prev != "" && string(key) <= string(prev) {
			t.Fatalf("create %d minted %q, which does not sort above create "+
				"%d's %q", n, key, n-1, prev)
		}
		prev = key
	}
	// A drag between the last two creates cannot reach the next one.
	last, _ := tracker.IntegerAt(4_000)
	next, _ := tracker.IntegerAt(4_001)
	drag, err := tracker.KeyBetween(last, next)
	if err != nil {
		t.Fatalf("KeyBetween: %v", err)
	}
	if string(drag) >= string(next) {
		t.Fatalf("a drag minted %q, at or above the next create's %q", drag, next)
	}
}

// THE INTEGER LATTICE IS THE SAME SEQUENCE WHETHER IT IS COMPUTED OR WALKED.
//
// A create computes its key directly from the counter, because walking would
// make every create on a mature project O(its own age). The two must agree, or
// a project's order silently depends on how its keys were produced.
func TestTheComputedLatticeMatchesTheWalkedOne(t *testing.T) {
	t.Parallel()
	walked := tracker.Rank(tracker.RankOrigin)
	for n := uint64(1); n <= 8_000; n++ {
		computed, err := tracker.IntegerAt(n)
		if err != nil {
			t.Fatalf("IntegerAt(%d): %v", n, err)
		}
		if computed != walked {
			t.Fatalf("IntegerAt(%d) = %q and walking from the origin reaches %q",
				n, computed, walked)
		}
		next, err := tracker.KeyBetween(walked, "")
		if err != nil {
			t.Fatalf("walk past %q: %v", walked, err)
		}
		walked = next
	}
}

// NO MINTED KEY'S FRACTION ENDS IN '0'.
//
// A trailing zero is a key with a shorter equal, so one position would have
// two spellings — and the index that is supposed to make an order unique
// would accept both.
func TestNoMintedFractionEndsInZero(t *testing.T) {
	t.Parallel()
	keys, err := tracker.KeysBetween("a0", "a1", 256)
	if err != nil {
		t.Fatalf("KeysBetween: %v", err)
	}
	head := tracker.Rank(tracker.RankOrigin)
	for range 300 {
		next, err := tracker.KeyBetween("", head)
		if err != nil {
			t.Fatalf("head insertion: %v", err)
		}
		keys = append(keys, next)
		head = next
	}
	for _, k := range keys {
		if strings.HasSuffix(k.Fraction(), "0") {
			t.Errorf("%q was minted with a fraction ending in '0'", k)
		}
	}
}

// A SPREAD OF N KEYS IS ASCENDING, DISTINCT AND SHORT.
//
// Short is the point: a chain of 256 keys each minted after the last would
// make the final one 256 characters long, which is the length the re-spread
// exists to remove. Splitting at the midpoint is what keeps them near the
// gap's own depth.
func TestASpreadIsAscendingDistinctAndShort(t *testing.T) {
	t.Parallel()
	keys, err := tracker.KeysBetween("a0", "a1", tracker.RankRespreadInline)
	if err != nil {
		t.Fatalf("KeysBetween: %v", err)
	}
	if len(keys) != tracker.RankRespreadInline {
		t.Fatalf("asked for %d keys and got %d", tracker.RankRespreadInline, len(keys))
	}
	prev := tracker.Rank("a0")
	longest := 0
	for i, k := range keys {
		if string(k) <= string(prev) {
			t.Fatalf("key %d (%q) does not sort above %q", i, k, prev)
		}
		if !k.Valid() {
			t.Fatalf("key %d (%q) is not well-formed", i, k)
		}
		prev = k
		if len(k) > longest {
			longest = len(k)
		}
	}
	if string(prev) >= "a1" {
		t.Fatalf("the last key %q is not below the upper neighbour", prev)
	}
	if longest >= tracker.RankRenormaliseAt {
		t.Fatalf("a re-spread of %d rows produced a %d-character key, at or past "+
			"the threshold that triggered it — the repair would trigger itself",
			len(keys), longest)
	}
	// AND IT IS AS SHORT AS THE GAP ALLOWS, which is the property the
	// midpoint split buys over a chain. Fitting n keys into one gap needs
	// ceil(log62(n+1)) fractional characters; the bound below allows the
	// integer part, that minimum and one character of slack. A chain
	// minted from the left satisfies the threshold above and blows
	// straight through this, which is the difference the split is for.
	minimum := 0
	for span := 1; span <= len(keys); span *= len(tracker.RankDigits) {
		minimum++
	}
	if bound := len(tracker.RankOrigin) + minimum + 1; longest > bound {
		t.Fatalf("a re-spread of %d rows produced a %d-character key against a "+
			"bound of %d — the keys are not being placed at the gap's own "+
			"depth", len(keys), longest, bound)
	}
}

// THE RE-SPREAD WINDOW IS DERIVED, AND IT STOPS.
//
// One constant, one derivation. The window doubles until the keys it would
// produce fall under the threshold, and it never exceeds the inline cap —
// past which the drag still succeeds and the duty's paced walk finishes the
// tidying.
func TestTheRespreadWindowIsDerivedAndBounded(t *testing.T) {
	t.Parallel()
	window, err := tracker.RespreadWindow("a0", "a1")
	if err != nil {
		t.Fatalf("RespreadWindow: %v", err)
	}
	if window < tracker.RankRenormaliseAt {
		t.Fatalf("the window is %d, below the threshold it starts at (%d)",
			window, tracker.RankRenormaliseAt)
	}
	if window > tracker.RankRespreadInline {
		t.Fatalf("the window is %d, past the inline cap (%d) — beyond that the "+
			"repair belongs to the duty rather than to the drag's own commit",
			window, tracker.RankRespreadInline)
	}
}

// AN ILL-FORMED KEY IS REFUSED RATHER THAN COMPARED.
//
// A length-invalid key still compares, which is exactly why it has to be
// caught here: nothing downstream would notice, and the order would be wrong
// in a way no index could report.
func TestAnIllFormedKeyIsNotValid(t *testing.T) {
	t.Parallel()
	for _, bad := range []tracker.Rank{
		"",
		"0",   // no magnitude head
		"a",   // the head declares one digit and none follows
		"b0",  // the head declares two digits and one follows
		"a0!", // outside the alphabet
		"a00", // a fraction ending in '0'
		tracker.Rank("a0" + strings.Repeat("z", tracker.RankRefuseAt)),
	} {
		if bad.Valid() {
			t.Errorf("%q is accepted as a well-formed rank key", bad)
		}
	}
}
