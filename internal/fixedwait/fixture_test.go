package fixedwait

import (
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// at is a finding as a fixture expects it: the rule, the position, and for
// an uncovered reference the function of package time it names.
type at struct {
	rule Rule
	file string
	line int
	name string
}

func (a at) String() string {
	return fmt.Sprintf("%s:%d %s %s", a.file, a.line, a.rule, a.name)
}

// fixture scans testdata/<name>, a module of its own, and holds the result
// to inventory.
func fixture(t *testing.T, name string, inventory ...Entry) []Finding {
	t.Helper()
	tree, err := Scan(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if tree.TestFiles == 0 {
		t.Fatalf("testdata/%s: read no test-code file", name)
	}
	return Check(tree, inventory)
}

// expect fails unless got is exactly want, each finding as many times.
func expect(t *testing.T, got []Finding, want ...at) {
	t.Helper()
	var have, wanted []string
	for _, f := range got {
		a := at{rule: f.Rule, file: f.File, line: f.Line}
		if f.Rule == Uncovered {
			a.name = f.Detail
		}
		have = append(have, a.String())
	}
	for _, a := range want {
		wanted = append(wanted, a.String())
	}
	slices.Sort(have)
	slices.Sort(wanted)
	if !slices.Equal(have, wanted) {
		var all []string
		for _, f := range got {
			all = append(all, f.String())
		}
		t.Errorf("found:\n%s\nwant:\n%s\nevery finding:\n%s", strings.Join(have, "\n"),
			strings.Join(wanted, "\n"), strings.Join(all, "\n"))
	}
}

// A BARE SLEEP IN A TEST IS REFUSED, named by its position, its function
// and the function of package time, as the listing prints it.
func TestABareSleepIsRefused(t *testing.T) {
	t.Parallel()
	got := fixture(t, "baresleep")
	expect(t, got, at{Uncovered, "p/p_test.go", 9, "time.Sleep"})
	if len(got) == 1 && got[0].Func != "TestNothingArrives" {
		t.Errorf("the reference's function is %q, want TestNothingArrives", got[0].Func)
	}
}

// A WAIT INSIDE A BUBBLE IS NOT COUNTED: synctest.Test's function,
// synctest.Run's, and the function handed to a bubble runner — a function
// that hands its parameter whole to synctest — in the runner's position.
func TestAWaitInABubbleIsAccepted(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "bubbled"))
}

// A RUNNER IS ITS OWN PACKAGE'S, AND A LOCAL IS NEVER ONE: the external test
// package's function of the runner's name, a local of the runner's name, and
// a local of synctest's name each run the literal on the machine's clock, so
// its wait is counted, while the runner itself still bubbles.
func TestARunnerIsItsOwnPackagesAndNoLocalIsOne(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "runner-not-its-own"),
		at{Uncovered, "p/p_internal_test.go", 25, "time.Sleep"},
		at{Uncovered, "p/p_internal_test.go", 36, "time.Sleep"},
		at{Uncovered, "p/p_ext_test.go", 14, "time.Sleep"})
}

// A FUNCTION THAT ALSO RUNS ITS PARAMETER ON THE MACHINE'S CLOCK IS NO
// RUNNER, so a wait in a literal handed to it is counted.
func TestAFunctionThatMayRunOutsideABubbleIsNoRunner(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "notarunner"), at{Uncovered, "p/p_test.go", 20, "time.Sleep"})
}

// EACH MARKER TAKES ITS REFERENCE OUT OF THE CLASS, trailing it on its line
// or alone on the line above; a bench marker in a function that takes a
// *testing.B, and a measure marker, with its reason, over a reading of
// elapsed time (a time.Since on its line, a time.Time's Sub above).
func TestEachMarkerIsAcceptedOnItsLineOrAbove(t *testing.T) {
	t.Parallel()
	for _, kind := range []Kind{Liveness, Simulated, Bench, Measure} {
		for _, where := range []string{"line", "above"} {
			name := fmt.Sprintf("marker-%s-%s", kind, where)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				expect(t, fixture(t, name))
			})
		}
	}
}

// A SIMULATED WAIT SAYS WHAT IT STANDS IN FOR, or the marker is refused and
// the wait stays counted.
func TestASimulatedMarkerWithNoReasonIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "simulated-noreason"),
		at{BadMarker, "p/p_test.go", 9, ""},
		at{Uncovered, "p/p_test.go", 10, "time.Sleep"})
}

// A MARKER OF NO KIND THE GATE KNOWS IS REFUSED, and its wait stays counted.
func TestAnUnknownMarkerKindIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "unknown-kind"),
		at{BadMarker, "p/p_test.go", 9, ""},
		at{Uncovered, "p/p_test.go", 10, "time.Sleep"})
}

// A MARKER COVERS ONE LINE, WHICH ITS PLACE DECIDES: trailing code, that
// code's line and not the next, so a liveness timer's marker does not also
// pass the wait under it; alone on its line, the line under it and not the
// one after, where it is stale.
func TestAMarkerCoversOneLineWhichItsPlaceDecides(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "marker-reach"),
		at{Uncovered, "p/p_test.go", 10, "time.Sleep"},
		at{StaleMarker, "p/p_test.go", 14, ""},
		at{Uncovered, "p/p_test.go", 16, "time.Sleep"})
}

// A BENCH MARKER BELONGS TO A BENCHMARK: in a function that takes no
// *testing.B — a test, or a helper taking testing.TB — it is refused and its
// wait stays counted.
func TestABenchMarkerOutsideABenchmarkIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "bench-in-a-test"),
		at{BadMarker, "p/p_test.go", 9, ""},
		at{Uncovered, "p/p_test.go", 9, "time.Sleep"},
		at{BadMarker, "p/p_test.go", 13, ""},
		at{Uncovered, "p/p_test.go", 14, "time.Sleep"})
}

// A MARKER IN PRODUCTION CODE IS REFUSED, and so is a bubble declaration
// there: nothing in production code is counted, so neither marks anything.
func TestAMarkerInProductionCodeIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "production-markers"),
		at{StaleMarker, "internal/foo/foo.go", 7, ""},
		at{MisplacedBubble, "internal/foo/foo.go", 14, ""})
}

// A MARKER OVER NO COUNTED WAIT IS STALE: one over no wait at all, and one
// over a wait a bubble already takes out of the class.
func TestAStaleMarkerIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "stale-marker"),
		at{StaleMarker, "p/p_test.go", 10, ""},
		at{StaleMarker, "p/p_test.go", 14, ""})
}

// A BUBBLE DECLARATION BELONGS ON A FUNCTION'S DOC; anywhere else it is
// refused and takes nothing out of the class.
func TestABubbleDeclarationOffAFunctionsDocIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "misplaced-bubble"),
		at{MisplacedBubble, "p/p_test.go", 9, ""},
		at{Uncovered, "p/p_test.go", 10, "time.Sleep"})
}

// A BUBBLE-ONLY HELPER CALLED OUTSIDE A BUBBLE IS REFUSED at that call,
// while the call inside the bubble stands.
func TestABubbleOnlyHelperUsedOutsideABubbleIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "helper-outside"), at{BubbleEscapes, "p/p_test.go", 20, ""})
}

// AND FROM ANOTHER PACKAGE: an exported helper of a test-support package is
// held at a reference through the importing file's own name for it, in a
// file that imports neither time nor synctest — and in production code,
// which runs it on the machine's clock as surely as a test does.
func TestABubbleOnlyHelperUsedOutsideABubbleFromAnotherPackageIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "helper-outside-package"),
		at{BubbleEscapes, "internal/user/user_test.go", 10, ""},
		at{BubbleEscapes, "internal/prod/prod.go", 6, ""})
}

// A BUBBLE DECLARATION ON A METHOD WITH AN EXPORTED NAME IS REFUSED, and its
// waits are counted: such a method is reached where nothing names its
// package — here through a value another package hands back — so no use
// found by name could hold every caller to a bubble.
func TestABubbleDeclarationOnAnExportedMethodIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "helper-exported-method"),
		at{MisplacedBubble, "internal/waittest/waittest.go", 10, ""},
		at{Uncovered, "internal/waittest/waittest.go", 12, "time.Sleep"})
}

// A BUBBLE DECLARATION OVER NO WAIT IS STALE: it binds every caller and
// takes nothing out of the class.
func TestABubbleDeclarationOverNoWaitIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "stale-bubble"), at{StaleMarker, "p/p_test.go", 10, ""})
}

// A BUBBLE-ONLY HELPER USED ONLY IN BUBBLES IS ACCEPTED, and its waits are
// not counted: called inside synctest's function, from another bubble-only
// helper, handed to synctest whole, and as a method.
func TestABubbleOnlyHelperUsedOnlyInBubblesIsAccepted(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "helper-inside"))
}

// AN ALIASED IMPORT OF time IS COUNTED through the file's own name for it.
func TestAnAliasedTimeImportIsCounted(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "alias-import"), at{Uncovered, "p/p_test.go", 9, "time.Sleep"})
}

// A DOT IMPORT OF time IS COUNTED at the bare name, and a field or key of
// the same name is not a reference.
func TestADotImportOfTimeIsCounted(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "dot-import"), at{Uncovered, "p/p_test.go", 12, "time.Sleep"})
}

// A FUNCTION VALUE IS COUNTED where it is named, called or not.
func TestAFunctionValueIsCounted(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "func-value"), at{Uncovered, "p/p_test.go", 9, "time.NewTimer"})
}

// A TEST-SUPPORT PACKAGE'S OWN FILES ARE TEST CODE — a directory whose name
// ends in "test" — while an ordinary package's non-test file is not judged.
func TestASupportPackagesFilesAreTestCodeAndAnOrdinaryPackagesAreNot(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "support-package"), at{Uncovered, "internal/footest/foo.go", 6, "time.Sleep"})
}

// AN ENTRY COVERS EXACTLY THE REFERENCES IT RECORDS: a function holding as
// many passes, by any mix of categories.
func TestAnInventoryEntryCoversItsFunctionsReferences(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "inventory",
		Entry{File: "p/p_test.go", Func: "TestTwoWaits", NegativeWindow: 1, ExemptWait: 1, Piece: 2},
		Entry{File: "p/p_test.go", Func: "suite.waitABit", HeadStart: 1, Piece: 3},
	))
}

// AN ENTRY RECORDING MORE THAN ITS FUNCTION HOLDS IS STALE, so the piece
// that removed a wait removes it from the inventory in the same diff: fewer
// references, a function that holds none, a function that is gone and a file
// that is gone.
func TestAStaleInventoryEntryIsRefused(t *testing.T) {
	t.Parallel()
	got := fixture(t, "inventory",
		Entry{File: "p/p_test.go", Func: "TestTwoWaits", NegativeWindow: 3, Piece: 2},
		Entry{File: "p/p_test.go", Func: "suite.waitABit", HeadStart: 1, Piece: 3},
		Entry{File: "p/p_test.go", Func: "TestNoWait", HeadStart: 1, Piece: 3},
		Entry{File: "p/p_test.go", Func: "TestGone", HeadStart: 1, Piece: 3},
		Entry{File: "p/gone_test.go", Func: "TestTwoWaits", HeadStart: 1, Piece: 3},
	)
	expect(t, got,
		at{StaleEntry, "p/p_test.go", 9, ""},
		at{StaleEntry, "p/p_test.go", 0, ""},
		at{StaleEntry, "p/p_test.go", 0, ""},
		at{StaleEntry, "p/gone_test.go", 0, ""},
	)
	// Each says which way it is stale, since the fix differs: lower the
	// count, or remove an entry whose function or file is gone.
	for fn, says := range map[string]string{
		"TestTwoWaits":   "holds 2 fixed waits and its entry records 3",
		"TestNoWait":     "holds 0 counted references and its entry records 1",
		"TestGone":       "no longer declares",
		"gone_test.go/*": "file that is gone",
	} {
		if !slices.ContainsFunc(got, func(f Finding) bool {
			key := f.Func
			if f.File == "p/gone_test.go" {
				key = "gone_test.go/*"
			}
			return key == fn && strings.Contains(f.Detail, says)
		}) {
			t.Errorf("no stale entry for %s says %q", fn, says)
		}
	}
}

// A FUNCTION HOLDING MORE THAN ITS ENTRY RECORDS IS REFUSED: a wait added
// beside an old one is a new wait, though its function is in the table. The
// finding carries every reference of the function, as the listing prints
// them, since which one is new the gate cannot say: so an elapsed reading
// added beside a wait the inventory holds is listed, not only counted.
func TestAnOverFullFunctionIsRefused(t *testing.T) {
	t.Parallel()
	got := fixture(t, "inventory",
		Entry{File: "p/p_test.go", Func: "TestTwoWaits", NegativeWindow: 1, Piece: 2},
		Entry{File: "p/p_test.go", Func: "suite.waitABit", HeadStart: 1, Piece: 3},
	)
	expect(t, got, at{OverFull, "p/p_test.go", 9, ""})
	if len(got) == 1 {
		var refs []string
		for _, r := range got[0].Refs {
			refs = append(refs, r.String())
		}
		want := []string{
			"p/p_test.go:9: TestTwoWaits: time.Sleep",
			"p/p_test.go:10: TestTwoWaits: time.After",
		}
		if !slices.Equal(refs, want) {
			t.Errorf("the over-full finding lists %q, want %q", refs, want)
		}
	}
}

// AN ENTRY THE INVENTORY CANNOT HOLD IS REFUSED: no reference recorded, no
// piece, a piece past the series' last, and a function named twice.
func TestAMalformedInventoryEntryIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "inventory",
		Entry{File: "p/p_test.go", Func: "TestTwoWaits", NegativeWindow: 1, ExemptWait: 1, Piece: 2},
		Entry{File: "p/p_test.go", Func: "TestTwoWaits", NegativeWindow: 1, ExemptWait: 1, Piece: 2},
		Entry{File: "p/p_test.go", Func: "suite.waitABit", Piece: 3},
		Entry{File: "p/p_test.go", Func: "TestNoWait", HeadStart: 1},
		Entry{File: "p/p_test.go", Func: "TestGone", HeadStart: 1, Piece: LastPiece + 1},
	),
		at{BadEntry, "p/p_test.go", 0, ""},
		at{BadEntry, "p/p_test.go", 0, ""},
		at{BadEntry, "p/p_test.go", 0, ""},
		at{BadEntry, "p/p_test.go", 0, ""},
		at{Uncovered, "p/p_test.go", 16, "time.Sleep"},
	)
}

// A BOUND ON time.Since IS REFUSED: a positive deadline whose clock is read
// rather than waited on, named as the listing prints it.
func TestABoundOnTimeSinceIsRefused(t *testing.T) {
	t.Parallel()
	got := fixture(t, "elapsed-since")
	expect(t, got, at{Uncovered, "p/p_test.go", 10, "time.Since"})
	if len(got) == 1 && got[0].String() != "p/p_test.go:10: TestAnswersInTime: uncovered: time.Since" {
		t.Errorf("the finding reads %q", got[0])
	}
}

// A BOUND ON time.Until IS REFUSED, the same deadline read from the other end.
func TestABoundOnTimeUntilIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "elapsed-until"), at{Uncovered, "p/p_test.go", 10, "time.Until"})
}

// A time.Time'S Sub IS REFUSED WHEREVER ITS RECEIVER IS ONE, told by the
// receiver's type: through a value, an alias (stamp = time.Time is
// time.Time), an embedding that promotes it, a conversion back from a
// defined type, and a method expression; in a file that does not import
// time; and in an external test package that reads a name only the test
// binary's variant of the package declares, which type-checks only against
// that variant; through a bound method value (start.Sub), a pointer
// (&start, and a *time.Time field), and in a support package's own non-test
// file, which reaches the type checker only as the package's own files. A
// DEFINED type over time.Time (type instant time.Time) has none of
// time.Time's methods — Go gives a defined type the methods its declaration
// lists, never its underlying type's — so it has no Sub to call until it is
// converted back, and that conversion's Sub is counted.
func TestATimeTimesSubIsRefusedByItsReceiversType(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "elapsed-sub"),
		at{Uncovered, "p/p_test.go", 10, "time.Time.Sub"},
		at{Uncovered, "p/p_test.go", 17, "time.Time.Sub"},
		at{Uncovered, "p/p_test.go", 22, "time.Time.Sub"},
		at{Uncovered, "p/p_test.go", 27, "time.Time.Sub"},
		at{Uncovered, "p/p_test.go", 31, "time.Time.Sub"},
		at{Uncovered, "p/p_test.go", 37, "time.Time.Sub"},
		at{Uncovered, "p/p_test.go", 44, "time.Time.Sub"},
		at{Uncovered, "p/p_test.go", 47, "time.Time.Sub"},
		at{Uncovered, "p/notime_test.go", 7, "time.Time.Sub"},
		at{Uncovered, "p/p_ext_test.go", 11, "time.Time.Sub"},
		at{Uncovered, "ptest/ptest.go", 8, "time.Time.Sub"})
}

// ANOTHER TYPE'S Sub IS NOT COUNTED: a big.Int's, a type of the fixture's
// own, and the Sub a defined type over time.Time declares for itself.
func TestAnotherTypesSubIsNotCounted(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "sub-other"))
}

// A MEASURE MARKER SAYS WHY ITS READING DECIDES NOTHING, or it is refused and
// the reading stays counted.
func TestAMeasureMarkerWithNoReasonIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "measure-noreason"),
		at{BadMarker, "p/p_test.go", 10, ""},
		at{Uncovered, "p/p_test.go", 11, "time.Since"})
}

// EACH MARKER COVERS ITS OWN SHAPE: a measure marker over a fixed wait alone,
// and a fixed wait's marker over an elapsed reading alone, are refused and
// leave the reference counted; on a line holding both, each covers its own
// and the other stays counted.
func TestAMarkerOfOneShapeOverTheOtherIsRefused(t *testing.T) {
	t.Parallel()
	got := fixture(t, "marker-wrong-shape")
	expect(t, got,
		at{BadMarker, "p/p_test.go", 10, ""},
		at{Uncovered, "p/p_test.go", 11, "time.Sleep"},
		at{BadMarker, "p/p_test.go", 12, ""},
		at{Uncovered, "p/p_test.go", 13, "time.Since"},
		at{Uncovered, "p/p_test.go", 16, "time.After"})
	// Each refusal names the marker the reference takes instead.
	for line, says := range map[int]string{
		10: "a fixed wait that decides nothing is liveness",
		12: "//fixedwait:measure <why it decides nothing>",
	} {
		if !slices.ContainsFunc(got, func(f Finding) bool {
			return f.Rule == BadMarker && f.Line == line && strings.Contains(f.Detail, says)
		}) {
			t.Errorf("no refusal at line %d says %q", line, says)
		}
	}
}

// A MEASURE MARKER OVER NO COUNTED READING IS STALE: over nothing, and over a
// reading a bubble already takes out of the class.
func TestAStaleMeasureMarkerIsRefused(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "measure-stale"),
		at{StaleMarker, "p/p_test.go", 10, ""},
		at{StaleMarker, "p/p_test.go", 15, ""})
}

// A READING OF ELAPSED TIME IN A BUBBLE IS NOT COUNTED, where the clock is
// exact: in synctest's function — a time.Time's Before, After and Compare as
// much as its Sub — and in a bubble-only helper, one that reads it only
// through a Sub or only through a Before included, whose declaration that
// reading keeps from being stale.
func TestAnElapsedReadingInABubbleIsAccepted(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "elapsed-bubbled"))
}

// AN ENTRY COVERS A FUNCTION'S ELAPSED READINGS AS IT DOES ITS WAITS, counted
// as elapsed bounds; one recording more than the function holds is stale.
func TestAnInventoryEntryCoversElapsedReadings(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "inventory-elapsed",
		Entry{File: "p/p_test.go", Func: "TestTwoReadings", ElapsedBound: 2, Piece: 2}))
	expect(t, fixture(t, "inventory-elapsed",
		Entry{File: "p/p_test.go", Func: "TestTwoReadings", ElapsedBound: 3, Piece: 2}),
		at{StaleEntry, "p/p_test.go", 10, ""})
}

// A Sub, Before, After or Compare IN A FILE THIS PLATFORM DOES NOT BUILD IS
// REFUSED: the gate cannot type its receiver, and passing it would leave a
// reading of the clock in no count on any platform. time.After, the wait, is
// not one: it is package time's function, counted as a wait by its name, so
// a file that waits on it needs no type and is not refused.
func TestAMethodTheGateCannotTypeIsRefused(t *testing.T) {
	t.Parallel()
	got := fixture(t, "untyped-receiver")
	expect(t, got,
		at{Untyped, "p/elsewhere_test.go", 12, ""},
		at{Untyped, "p/elsewhere_before_test.go", 12, ""})
	// Each names the method it could not type, since the file holding it is
	// what the person moves or splits.
	for file, says := range map[string]string{
		"p/elsewhere_test.go":        "this Sub's receiver",
		"p/elsewhere_before_test.go": "this Before's receiver",
	} {
		if !slices.ContainsFunc(got, func(f Finding) bool {
			return f.File == file && strings.Contains(f.Detail, says)
		}) {
			t.Errorf("no refusal in %s says %q", file, says)
		}
	}
}

// THE PREFILTER ADMITS A FILE THAT SPELLS THE IDENTIFIER Sub, Before, After
// or Compare, and no file that spells one only inside a longer name, so a
// package whose tests hold a Subscribe, an AfterFunc or a BeforeEach and none
// of the four is not parsed for them for nothing. Admitting too much costs
// only time; admitting too little would leave a time.Time's method uncounted,
// so every way a reference can sit beside its neighbours is a row.
func TestThePrefilterAdmitsTheIdentifiersAlone(t *testing.T) {
	t.Parallel()
	for src, want := range map[string]bool{
		"d := end.Sub(start)":    true,
		"sub := time.Time.Sub":   true,
		"Sub":                    true,
		"x.\n\tSub(y)":           true,
		"x./* gap */Sub(y)":      true,
		"q.Subscribe(topic)":     false,
		"msg.Subject":            false,
		"t.subSecond":            false,
		"x.SubÅ":                 false,
		"x.ÅSub":                 false,
		"x.Sub_":                 false,
		"x.Sub2(y)":              false,
		"q.Subscribe(); e.Sub()": true,
		"now.Before(deadline)":   true,
		"!now.After(deadline)":   true,
		"a.Compare(b) < 0":       true,
		"<-time.After(d)":        true,
		"x./* gap */After(y)":    true,
		"time.AfterFunc(d, f)":   false,
		"BeforeEach":             false,
		"x.Compared":             false,
		"x.ÅBefore":              false,
		"a.Equal(b)":             false,
	} {
		if got := spellsMethod([]byte(src)); got != want {
			t.Errorf("spellsMethod(%q) = %v, want %v", src, got, want)
		}
	}
}

// ONLY TIME'S OWN After IS TOLD APART WITHOUT TYPES: a selector of one of the
// four names is a candidate, whose package is type-checked, unless it is
// time.After through the file's own name for package time where nothing
// local takes that name — package time's function, the wait, never a
// method. Everything else is left to the types, another package's Compare
// included, since a file's imports do not say what a package calls itself.
// A candidate passed over would leave its method uncounted, so every way a
// method can be named is a row.
func TestOnlyTimesOwnAfterIsToldApartWithoutTypes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, src, want string }{
		{"the wait", `import "time"; func f() { <-time.After(0) }`, ""},
		{"the wait through an alias", `import tm "time"; func f() { <-tm.After(0) }`, ""},
		{"the wait through a dot import", `import . "time"; func f() { <-After(0) }`, ""},
		{"a value's", `import "time"; func f(t time.Time) bool { return t.After(t) }`, "After"},
		{"a method expression", `import "time"; var f = time.Time.After`, "After"},
		{"a local named time", `import "time"; type stamp = time.Time
			func f(time stamp) bool { return time.After(time) }`, "After"},
		{"another package under time's name", `import time "example.com/clock"
			func f() { <-time.After(0) }`, "After"},
		{"the wait, then a value's", `import "time"
			func f(t time.Time) bool { <-time.After(0); return t.Before(t) }`, "Before"},
		{"another package's own", `import "strings"; func f() int { return strings.Compare("a", "b") }`, "Compare"},
		{"in a file that does not import time", `func f(e event) bool { return e.at.Before(e.from) }`, "Before"},
		{"a Sub", `func f(e event) any { return e.at.Sub(e.from) }`, "Sub"},
		{"an Equal", `import "time"; func f(t time.Time) bool { return t.Equal(t) }`, ""},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "p_test.go", "package p; "+c.src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := ""
		if id := firstMethod(f); id != nil {
			got = id.Name
		}
		if got != c.want {
			t.Errorf("%s: the first candidate is %q, want %q", c.name, got, c.want)
		}
	}
}

// A STATED MARKER TAKES A time.Time'S Sub OUT OF THE CLASS, trailing it or
// alone above it, where no wall clock separates its instants: the reading
// may decide a claim, exactly, and nothing of it is a fixed wait.
func TestAStatedMarkerIsAcceptedOverASub(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "marker-stated"))
}

// A STATED MARKER COVERS A COMPARISON OF INSTANTS AND NOTHING ELSE, AND SAYS
// WHICH INSTANTS: one with no reason is refused and leaves its Sub counted;
// one over time.Until, which reads now at the call, and one over a fixed
// wait are refused and leave the reference counted; over a line holding a
// Since and a Sub it covers the Sub alone, and the Since stays counted.
func TestAStatedMarkerOverAnythingButAComparisonIsRefused(t *testing.T) {
	t.Parallel()
	got := fixture(t, "stated-misplaced")
	expect(t, got,
		at{BadMarker, "p/p_test.go", 11, ""},
		at{Uncovered, "p/p_test.go", 12, "time.Time.Sub"},
		at{BadMarker, "p/p_test.go", 13, ""},
		at{Uncovered, "p/p_test.go", 14, "time.Until"},
		at{BadMarker, "p/p_test.go", 15, ""},
		at{Uncovered, "p/p_test.go", 15, "time.Sleep"},
		at{Uncovered, "p/p_test.go", 17, "time.Since"})
	// Each refusal says what is wrong with it, since the fix differs: a
	// reason to write, a reading of now to decide or measure, a wait to mark
	// as a wait.
	for line, says := range map[int]string{
		11: "says which instants or which clock",
		13: "time.Since or time.Until",
		15: "holds a fixed wait and no Sub",
	} {
		if !slices.ContainsFunc(got, func(f Finding) bool {
			return f.Rule == BadMarker && f.Line == line && strings.Contains(f.Detail, says)
		}) {
			t.Errorf("no refusal at line %d says %q", line, says)
		}
	}
}

// A LINE HOLDING A WAIT AND AN ELAPSED READING IS MARKED TWICE, one marker
// alone above it and one trailing it, each covering its own shape, and then
// nothing on it is counted. Two markers alone on consecutive lines do not
// mark it: the first covers the second's line, which holds no reference.
func TestALineHoldingBothShapesTakesAMarkerAboveAndOneTrailing(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "marker-both-shapes"),
		at{StaleMarker, "p/p_test.go", 20, ""},
		at{Uncovered, "p/p_test.go", 22, "time.Sleep"})
}

// EACH SHAPE IS HELD TO ITS OWN COUNT: an entry recording a function's waits
// and readings by shape passes, and one whose total matches but whose shapes
// do not is over-full in one shape and stale in the other — so a reading
// cannot take the place of a wait a piece removed, nor a wait a reading's.
func TestEachShapeIsHeldToItsOwnCount(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "inventory-shapes",
		Entry{File: "p/p_test.go", Func: "TestOneOfEach", NegativeWindow: 1, ElapsedBound: 1, Piece: 2}))
	expect(t, fixture(t, "inventory-shapes",
		Entry{File: "p/p_test.go", Func: "TestOneOfEach", ExemptWait: 1, ExemptElapsed: 1, Piece: 2}))
	waitsFor := fixture(t, "inventory-shapes",
		Entry{File: "p/p_test.go", Func: "TestOneOfEach", NegativeWindow: 2, Piece: 2})
	expect(t, waitsFor,
		at{OverFull, "p/p_test.go", 11, ""},
		at{StaleEntry, "p/p_test.go", 10, ""})
	readingsFor := fixture(t, "inventory-shapes",
		Entry{File: "p/p_test.go", Func: "TestOneOfEach", ElapsedBound: 1, ExemptElapsed: 1, Piece: 2})
	expect(t, readingsFor,
		at{OverFull, "p/p_test.go", 10, ""},
		at{StaleEntry, "p/p_test.go", 10, ""})
	for _, f := range append(waitsFor, readingsFor...) {
		if f.Rule == OverFull && len(f.Refs) != 1 {
			t.Errorf("the over-full finding lists %v, want the one reference of its shape", f.Refs)
		}
	}
}

// A time.Time'S Before, After AND Compare ARE REFUSED WHEREVER THE RECEIVER
// IS ONE, told by its type exactly as a Sub is: called, each the deciding
// shape a test writes (!time.Now().Before(deadline), time.Now().After(d),
// time.Now().Compare(d) >= 0); as method values and as method expressions
// (time.Time.Before, (*time.Time).After, and an alias's, stamp.Compare);
// through an alias, an embedding that promotes them and a pointer; through a
// conversion back from a DEFINED type, which has none of them itself; and in
// a package whose only test file does not import time, which only the
// prefilter's reading of every test file's bytes reaches.
func TestATimeTimesBeforeAfterAndCompareAreRefusedByTheReceiversType(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "elapsed-compare"),
		at{Uncovered, "p/p_test.go", 10, "time.Time.Before"},
		at{Uncovered, "p/p_test.go", 17, "time.Time.After"},
		at{Uncovered, "p/p_test.go", 24, "time.Time.Compare"},
		at{Uncovered, "p/p_test.go", 31, "time.Time.Before"},
		at{Uncovered, "p/p_test.go", 31, "time.Time.After"},
		at{Uncovered, "p/p_test.go", 31, "time.Time.Compare"},
		at{Uncovered, "p/p_test.go", 36, "time.Time.Before"},
		at{Uncovered, "p/p_test.go", 37, "time.Time.After"},
		at{Uncovered, "p/p_test.go", 38, "time.Time.Compare"},
		at{Uncovered, "p/p_test.go", 43, "time.Time.Before"},
		at{Uncovered, "p/p_test.go", 45, "time.Time.After"},
		at{Uncovered, "p/p_test.go", 47, "time.Time.Compare"},
		at{Uncovered, "p/p_test.go", 52, "time.Time.Before"},
		at{Uncovered, "q/q_test.go", 7, "time.Time.After"})
}

// NO OTHER COMPARISON IS COUNTED: another type's Before, After and Compare,
// the ones a defined type over time.Time declares for itself, fields named
// After and Before, a package's own Compare (strings', bytes', cmp's,
// slices'), a time.Time's Equal, which decides nothing about elapsed time,
// and time.After, the wait, which is counted as a wait and never as a
// reading — here under its liveness marker, which covers a wait alone.
func TestAnotherTypesComparisonsAndEqualAreNotCounted(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "compare-other"))
}

// A MEASURE OR A STATED MARKER TAKES A COMPARISON OF INSTANTS OUT OF THE
// CLASS, trailing it or alone above it: measure over a poll's own deadline
// and over a logged comparison, stated over instants the case stated, one
// derived from the other included.
func TestAMeasureOrStatedMarkerIsAcceptedOverAComparison(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "marker-compare"))
}

// A MARKER OVER A COMPARISON IT DOES NOT COVER IS STILL REFUSED, and leaves
// the reference counted: a stated one over a time.Since, which reads now at
// the call and is never stated; a fixed wait's over a comparison, which is a
// reading and no wait; and a stated one with no reason over a Compare.
func TestAMarkerOverAComparisonItDoesNotCoverIsRefused(t *testing.T) {
	t.Parallel()
	got := fixture(t, "compare-misplaced")
	expect(t, got,
		at{BadMarker, "p/p_test.go", 12, ""},
		at{Uncovered, "p/p_test.go", 13, "time.Since"},
		at{BadMarker, "p/p_test.go", 16, ""},
		at{Uncovered, "p/p_test.go", 17, "time.Time.After"},
		at{BadMarker, "p/p_test.go", 20, ""},
		at{Uncovered, "p/p_test.go", 21, "time.Time.Compare"})
	for line, says := range map[int]string{
		12: "time.Since or time.Until",
		16: "//fixedwait:stated <which instants or clock>",
		20: "says which instants or which clock",
	} {
		if !slices.ContainsFunc(got, func(f Finding) bool {
			return f.Rule == BadMarker && f.Line == line && strings.Contains(f.Detail, says)
		}) {
			t.Errorf("no refusal at line %d says %q", line, says)
		}
	}
}

// A COMPARISON OF INSTANTS IS A READING IN THE INVENTORY, held with
// time.Since's and a Sub's to the readings' fields and never to the waits':
// an entry recording a function's Since and Before as elapsed bounds, or one
// of them exempt, passes; one recording a reading too few is over-full, with
// both listed; and one recording the Before as a wait is over-full in its
// readings and stale in its waits.
func TestAComparisonOfInstantsIsAReadingInTheInventory(t *testing.T) {
	t.Parallel()
	expect(t, fixture(t, "inventory-compare",
		Entry{File: "p/p_test.go", Func: "TestTwoReadings", ElapsedBound: 2, Piece: 2}))
	expect(t, fixture(t, "inventory-compare",
		Entry{File: "p/p_test.go", Func: "TestTwoReadings", ElapsedBound: 1, ExemptElapsed: 1, Piece: 2}))
	short := fixture(t, "inventory-compare",
		Entry{File: "p/p_test.go", Func: "TestTwoReadings", ElapsedBound: 1, Piece: 2})
	expect(t, short, at{OverFull, "p/p_test.go", 10, ""})
	if len(short) == 1 {
		var refs []string
		for _, r := range short[0].Refs {
			refs = append(refs, r.String())
		}
		want := []string{
			"p/p_test.go:10: TestTwoReadings: time.Since",
			"p/p_test.go:10: TestTwoReadings: time.Time.Before",
		}
		if !slices.Equal(refs, want) {
			t.Errorf("the over-full finding lists %q, want %q", refs, want)
		}
	}
	expect(t, fixture(t, "inventory-compare",
		Entry{File: "p/p_test.go", Func: "TestTwoReadings", ElapsedBound: 1, NegativeWindow: 1, Piece: 2}),
		at{OverFull, "p/p_test.go", 10, ""},
		at{StaleEntry, "p/p_test.go", 10, ""})
}
