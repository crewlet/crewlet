package executor

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// These cases hold the verdict of a reading by content ([contentVerdict]) to
// its rules: directly from a walk's facts, and through OpenCode's export,
// today's one reader ([readOpenCodeHoldings]), whose helpers they share.

// ohVerdict is the verdict on calls, unlisted beside them, where parts are each
// class's counted parts' steps.
func ohVerdict(t *testing.T, parts map[sandbox.CallClass][]int, unlisted []sandbox.CallIdentity,
	calls ...sandbox.LoggedCall) []int {
	t.Helper()
	h, err := contentVerdict(sandbox.HoldingsAsk{Calls: calls, Unlisted: unlisted, Resumer: "2.3.2"}, parts, false)
	return ohHeld(t, h, err, calls...)
}

// A CLASS IS HELD ONLY WHERE EVERY CALL OF IT HAS A PART: two calls and two
// parts both held; two calls and one part neither; one call and two parts
// told, the walk counting a class's parts to one past its calls.
//
// Mutation: hold min(parts, calls), or where parts are at least calls, or
// have the walk stop counting at the class's calls, and this goes red.
func TestAClassIsHeldOnlyWhereEveryCallOfItHasAPart(t *testing.T) {
	t.Parallel()
	a, b := ohCall(1, "echo", `{"t":1}`, "same"), ohCall(2, "echo", `{"t":1}`, "same")
	class := ohClass(t, a)
	if got := ohVerdict(t, map[sandbox.CallClass][]int{class: {1, 1}}, nil, a, b); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("two calls, two parts: held %v; want both", got)
	}
	if got := ohVerdict(t, map[sandbox.CallClass][]int{class: {1, 2, 3}}, nil, a); got != nil {
		t.Errorf("one call, more parts: held %v; want none", got)
	}
	h, err := contentVerdict(sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{a}, Resumer: "2.3.2"},
		map[sandbox.CallClass][]int{class: {1, 2, 3}}, false)
	if records := h.Classes[class]; err != nil || records.Held || records.Parts != 2 {
		t.Errorf("one call, three parts: the class is %+v, %v; want two parts counted and not held", records, err)
	}
	part := ohDone("echo", `{"t":1}`, "same")
	h, err = ohRead(t, ohDoc(ohUser, ohStep(part), ohStep(part)), ohBegan, a)
	if got := ohHeld(t, h, err, a); got != nil {
		t.Errorf("one call, two parts in the export: held %v; want told", got)
	}
}

// OF TWO IDENTICAL CALLS WITH ONE PART NEITHER IS HELD: which of the two the
// part is is never decided.
//
// Mutation: pair the part with the first call, and this goes red.
func TestOfTwoIdenticalCallsWithOnePartNeitherIsHeld(t *testing.T) {
	t.Parallel()
	a, b := ohCall(1, "echo", `{"t":1}`, "same"), ohCall(2, "echo", `{"t":1}`, "same")
	if got := ohVerdict(t, map[sandbox.CallClass][]int{ohClass(t, a): {1}}, nil, a, b); got != nil {
		t.Errorf("two calls, one part: held %v; want neither", got)
	}
}

// A CALL THE LOG CANNOT STATE MAKES ITS CLASS WILD: one in flight, a dropped
// one with its digest, one with none, a listed call with cut arguments, and a
// late dropped one (the tally holds it) each tell the class of its tool; a
// dropped one of another digest, where every call of the class has one, does
// not, nor does one of another tool.
//
// Mutation: ignore the wildcards, or compare digests where the class's call
// has none, and a case goes red.
func TestACallTheLogCannotStateMakesItsClassWild(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	c.Digest = "d1"
	parts := map[sandbox.CallClass][]int{ohClass(t, c): {1}}
	cutArgs := ohCall(2, "echo", `{"t":2}`, "other")
	cutArgs.Args.Whole, cutArgs.Asked = false, false
	for name, tc := range map[string]struct {
		unlisted []sandbox.CallIdentity
		listed   []sandbox.LoggedCall
		held     bool
	}{
		"nothing unstated":        {nil, nil, true},
		"in flight":               {[]sandbox.CallIdentity{{Name: "echo", Digest: "d1", InFlight: true}}, nil, false},
		"dropped, its digest":     {[]sandbox.CallIdentity{{Name: "echo", Digest: "d1"}}, nil, false},
		"dropped, no digest":      {[]sandbox.CallIdentity{{Name: "echo"}}, nil, false},
		"a late one, dropped":     {[]sandbox.CallIdentity{{Name: "echo", Digest: "d1", OnlyFailed: true}}, nil, false},
		"cut arguments, listed":   {nil, []sandbox.LoggedCall{cutArgs}, false},
		"dropped, another digest": {[]sandbox.CallIdentity{{Name: "echo", Digest: "d2"}}, nil, true},
		"dropped, another tool":   {[]sandbox.CallIdentity{{Name: "post"}}, nil, true},
	} {
		got := ohVerdict(t, parts, tc.unlisted, append([]sandbox.LoggedCall{c}, tc.listed...)...)
		if held := slices.Contains(got, 1); held != tc.held {
			t.Errorf("%s: held %v; want call 1 held %t", name, got, tc.held)
		}
	}
	digestless := c
	digestless.Digest = ""
	got := ohVerdict(t, parts, []sandbox.CallIdentity{{Name: "echo", Digest: "d2"}}, digestless)
	if got != nil {
		t.Errorf("a class with a digestless call beside a dropped one of its tool: held %v; want told", got)
	}
}

// A HELD CALL IS WILD ONLY FOR THE ANSWER IT WAS GIVEN: its part answers with
// the held answer, so it tells the class of its tool, failure and text, and
// leaves another class of its tool held; one whose cut answer does not read
// back is wild for every class of its tool and failure.
//
// Mutation: make a held call wild by its tool alone, or only for a text its
// log states, and a case goes red.
func TestAHeldCallIsWildOnlyForTheAnswerItWasGiven(t *testing.T) {
	t.Parallel()
	answered := ohCall(1, "post", `{"t":1}`, "Held for approval.")
	answered.Failed = true
	other := ohCall(2, "post", `{"t":2}`, "posted")
	held := sandbox.LoggedCall{ID: "3", Appended: 3, Seq: 3, Name: "post", Held: true, Failed: true,
		Output: "Held for approval."}
	parts := map[sandbox.CallClass][]int{ohClass(t, answered): {1}, ohClass(t, other): {1}}
	if got := ohVerdict(t, parts, nil, answered, other, held); !slices.Equal(got, []int{2}) {
		t.Errorf("held %v; want the other class's call alone", got)
	}
	unstated := held
	unstated.OutputBytes = 99999
	if _, stated := sandbox.TextKeyOf(unstated.Output, unstated.OutputBytes); stated {
		t.Fatal("the held call's cut answer reads back")
	}
	if got := ohVerdict(t, parts, nil, answered, other, unstated); !slices.Equal(got, []int{2}) {
		t.Errorf("a held call of no stated answer: held %v; want its tool's failed class told", got)
	}
}

// AN UNNAMED COUNT OF FAILURES IS WILD ONLY FOR FAILURES: a count every call of
// which failed tells a failed class and leaves a succeeded one held; a count
// whose outcomes are mixed tells both, and so does an unnamed call in flight
// beside a count of failures, in either order.
//
// Mutation: ignore OnlyFailed, read a count of no failure as one of failures,
// or let the last unnamed entry decide, and a case goes red.
func TestAnUnnamedCountOfFailuresIsWildOnlyForFailures(t *testing.T) {
	t.Parallel()
	failed := ohCall(1, "post", `{"t":1}`, "refused")
	failed.Failed = true
	done := ohCall(2, "post", `{"t":2}`, "posted")
	parts := map[sandbox.CallClass][]int{ohClass(t, failed): {1}, ohClass(t, done): {1}}
	if got := ohVerdict(t, parts, []sandbox.CallIdentity{{OnlyFailed: true}}, failed, done); !slices.Equal(got, []int{2}) {
		t.Errorf("a count of failures: held %v; want the succeeded call alone", got)
	}
	if got := ohVerdict(t, parts, []sandbox.CallIdentity{{}}, failed, done); got != nil {
		t.Errorf("a count of mixed outcomes: held %v; want none", got)
	}
	for name, unlisted := range map[string][]sandbox.CallIdentity{
		"in flight first": {{InFlight: true}, {OnlyFailed: true}},
		"dropped first":   {{OnlyFailed: true}, {InFlight: true}},
	} {
		ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{failed, done}, Resumer: "2.3.2", Unlisted: unlisted}
		if err := (OpenCode{}).checkHoldings(ohArchive("2.3.2"), ask); err != nil {
			t.Fatalf("%s: the ask is refused: %v", name, err)
		}
		if got := ohVerdict(t, parts, unlisted, failed, done); got != nil {
			t.Errorf("an unnamed call in flight beside a count of failures, %s: held %v; want none", name, got)
		}
	}
}

// A CALL HELD AT THE SAVE POINT STAYS IN THE POOL: an unasked call identical
// to an asked run call makes the class two calls, and one part tells both.
//
// Mutation: leave unasked calls out of the pool, and this goes red.
func TestACallHeldAtTheSavePointStaysInThePool(t *testing.T) {
	t.Parallel()
	saved, run := ohCall(1, "echo", `{"t":1}`, "same"), ohCall(2, "echo", `{"t":1}`, "same")
	saved.Asked = false
	if got := ohVerdict(t, map[sandbox.CallClass][]int{ohClass(t, run): {1}}, nil, saved, run); got != nil {
		t.Errorf("held %v; want the run's call told", got)
	}
}

// A PART MATCHING TWO CLASSES COUNTS FOR NEITHER: two cut calls of one tool,
// arguments and size, whose kept openings are 4,090 and 4,091 bytes of one
// text, are two classes one part matches; it holds neither.
//
// Mutation: count the part for each class it matches, and this goes red.
func TestAPartMatchingTwoClassesCountsForNeither(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("x", 9000)
	short, long := ohCut(ohCall(1, "echo", `{"t":1}`, text)), ohCut(ohCall(2, "echo", `{"t":1}`, text))
	opening, _ := kept.Record("the run's record").Opening(long.Output, long.OutputBytes)
	trimmed, _ := kept.Record("the run's record").Opening(short.Output, short.OutputBytes)
	if len(opening) != len(trimmed) {
		t.Fatal("the log kept two openings of one text alike")
	}
	// The log keeps one opening for one text; a shorter one is a different
	// cut of the same text, as a release's other budget would make.
	short.Output = strings.Replace(short.Output, opening, opening[:len(opening)-1], 1)
	if _, ok := sandbox.ClassOf(short); !ok {
		t.Fatal("the shorter cut states no class")
	}
	h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, text))), ohBegan, short, long)
	if got := ohHeld(t, h, err, short, long); got != nil {
		t.Errorf("held %v; want neither", got)
	}
	h, err = ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, text))), ohBegan, long)
	if got := ohHeld(t, h, err, long); !slices.Equal(got, []int{2}) {
		t.Errorf("one class: held %v; want it", got)
	}
}

// CALLS IN AN ORDER THE EXPORT CONTRADICTS LEAVE THE SESSION UNREAD: calls of
// K admitted 1 and 3 both in step 1 and a call of L admitted 2 in step 2 are no
// order a run makes; L in step 1 is.
//
// Mutation: drop the order check, and this goes red.
func TestCallsInAnOrderTheExportContradictsLeaveTheSessionUnread(t *testing.T) {
	t.Parallel()
	k1, l, k2 := ohCall(1, "echo", `{"t":"k"}`, "k"), ohCall(2, "echo", `{"t":"l"}`, "l"),
		ohCall(3, "echo", `{"t":"k"}`, "k")
	kPart, lPart := ohDone("echo", `{"t":"k"}`, "k"), ohDone("echo", `{"t":"l"}`, "l")
	h, err := ohRead(t, ohDoc(ohUser, ohStep(kPart, kPart), ohStep(lPart)), ohBegan, k1, l, k2)
	ohNone(t, h, err)
	h, err = ohRead(t, ohDoc(ohUser, ohStep(kPart, lPart, kPart)), ohBegan, k1, l, k2)
	if got := ohHeld(t, h, err, k1, l, k2); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("one step: held %v; want all", got)
	}
}

// PARALLEL CALLS OF ONE STEP ARE READ IN ANY ORDER: a step's parts in another
// order than the calls were admitted in are a reading.
//
// Mutation: compare steps strictly, and this goes red.
func TestParallelCallsOfOneStepAreReadInAnyOrder(t *testing.T) {
	t.Parallel()
	a, b := ohCall(1, "echo", `{"t":"a"}`, "a"), ohCall(2, "echo", `{"t":"b"}`, "b")
	h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":"b"}`, "b"), ohDone("echo", `{"t":"a"}`, "a"))),
		ohBegan, a, b)
	if got := ohHeld(t, h, err, a, b); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("held %v; want both", got)
	}
}
