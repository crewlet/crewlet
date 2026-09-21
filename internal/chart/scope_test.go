package chart_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog"
)

// NO RECORD KIND PRODUCES THE ROOT SCOPE.
//
// The root path covers the whole chart, so a record that resolved to it would
// make every read and every write in the company wait behind it while it was
// deferred — and it would do so invisibly, because the deferral row an operator
// reads names a path and not a reason. The alphabet is designed so the widest
// thing an ordinary record can say is "this unit and the seats in it".
//
// THE FIRST EXCEPTION IS A BATCH PAST [chart.MaxScopeTerms], which is the case
// below: an import that rewrites more than sixty-four objects IS a
// reorganisation of the company, and the root term is the honest blast radius
// for one rather than an enumeration that cannot be stated.
//
// THE SECOND IS THE TWO GATE KINDS, which are listed here rather than skipped
// by a predicate: an eviction and a generation write no object row at all, and
// what each decides is whether records on EVERY subject count — so the root is
// what they mean rather than what they widened to. An eviction does not even
// pay the cost that makes the root dangerous, because it installs a gate: a
// version this build cannot read stops the applier instead of being filed at
// this path. A generation does pay it, and correctly, for the reason the
// alphabet gives at [chart.KindGeneration].
//
// This walks the DECLARED kinds rather than a list of its own, so a kind added
// later is covered by adding it to the enum and a verdict here.
func TestNoRecordKindProducesTheRootScope(t *testing.T) {
	t.Parallel()

	root := chart.RootPath()
	if root == "" {
		t.Fatal("the root path is empty, so every comparison below is against " +
			"nothing and this guard would pass whatever the alphabet said")
	}
	if len(chart.ObjectKinds) == 0 {
		t.Fatal("the domain declares no kinds, so this guard walks nothing")
	}

	// THE EXPECTATION IS WRITTEN OUT rather than read from the alphabet,
	// for the reason the sentinel case gives: asking the code what it
	// expects makes the walk pass for whatever the code happens to say.
	saysRoot := map[chart.ObjectKind]bool{
		chart.KindEviction: true, chart.KindGeneration: true,
	}

	for _, kind := range chart.ObjectKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			subject, scope := canonical(kind)
			if err := scope.Validate(subject); err != nil {
				t.Fatalf("the canonical scope for %s is one the writer's own "+
					"rule refuses: %v", kind, err)
			}
			paths := scope.Resolve(subject).Paths
			if len(paths) == 0 {
				t.Fatal("resolved to no path at all — an empty scope claims the " +
					"record makes nothing stale, which is the one claim a record " +
					"no build may be able to read cannot make")
			}
			at := slices.Contains(paths, root)
			if want := saysRoot[kind]; at != want {
				if want {
					t.Errorf("a %s record resolves to %v and not the root %q — "+
						"a gate decides whether records on every subject count, "+
						"and a narrower scope is a claim it does not make",
						kind, paths, root)
					return
				}
				t.Errorf("a %s record resolves to %q, the root of the whole "+
					"chart — while it is deferred, every read and every write in "+
					"the company is behind it. Only a batch past %d terms and "+
					"the two gate kinds may say that, and each says so "+
					"deliberately: see BatchScope", kind, root, chart.MaxScopeTerms)
			}
		})
	}
}

// AND THE ONE THING THAT DOES SAY IT, SAYS SO DELIBERATELY.
//
// A batch whose enumeration would pass the cap collapses to the root term
// rather than being refused or silently truncated. Truncation is the dangerous
// one: a scope short of what the record touches is a deferral that does not
// block the writes it makes stale, which is exactly what the field exists to
// prevent.
func TestABatchPastTheCapDeclaresTheRootAndSaysSo(t *testing.T) {
	t.Parallel()

	root := chart.RootPath()
	subject := chart.TreeSubject()

	// AT the cap it still enumerates. The boundary is asserted from both
	// sides, because an off-by-one here is a company-wide stall that only
	// appears on a chart of exactly the wrong size.
	at := chart.BatchScope(units(chart.MaxScopeTerms))
	if err := at.Validate(subject); err != nil {
		t.Fatalf("a batch of exactly %d terms is refused: %v", chart.MaxScopeTerms, err)
	}
	if paths := at.Resolve(subject).Paths; slices.Contains(paths, root) {
		t.Errorf("a batch of exactly %d terms collapsed to the root %q — the cap "+
			"is the largest enumeration that is still stated in full",
			chart.MaxScopeTerms, root)
	} else if len(paths) != chart.MaxScopeTerms {
		t.Errorf("a batch of %d terms resolved to %d paths", chart.MaxScopeTerms, len(paths))
	}

	// One past it, and at a size a real reorganisation reaches.
	for _, n := range []int{chart.MaxScopeTerms + 1, 4 * chart.MaxScopeTerms} {
		over := chart.BatchScope(units(n))
		if err := over.Validate(subject); err != nil {
			t.Fatalf("a batch of %d terms is refused rather than collapsed: %v — a "+
				"writer that has to handle the cap itself is a cap that will be "+
				"handled differently at every call site", n, err)
		}
		paths := over.Resolve(subject).Paths
		if len(paths) != 1 || paths[0] != root {
			t.Errorf("a batch of %d terms resolved to %v, want exactly [%q] — the "+
				"collapse only ever WIDENS, so a probe that would have matched a "+
				"term still matches the root", n, paths, root)
		}
	}

	// AND AN EMPTY BATCH IS THE ROOT TOO, for the type's own reason: an
	// empty scope claims the record makes nothing stale.
	if paths := chart.BatchScope(nil).Resolve(subject).Paths; len(paths) != 1 || paths[0] != root {
		t.Errorf("an empty batch resolved to %v, want [%q]", paths, root)
	}
}

// THE PATHS NEST, WHICH IS THE WHOLE OF WHAT THE FRAMEWORK KNOWS.
//
// It knows one thing about a scope path — that it is a hierarchy written left
// to right with a separator — and computes containment from that alone. So
// every claim this alphabet makes has to be true of the STRINGS: a deferral on
// a unit must block a write to a seat in it, a deferral on one seat must not
// block its neighbour, and the root must cover both. The last is the one a flat
// alphabet gets wrong for free — `g` beside `u/eng` is a SIBLING of every unit,
// so a record whose scope this build could not parse would block exactly
// nothing.
func TestTheScopeAlphabetNestsTheWayTheProbeReadsIt(t *testing.T) {
	t.Parallel()

	root := chart.RootPath()
	unit := chart.ScopeTerm{Kind: chart.TermUnit, ID: "engineering"}.Path()
	seat := chart.ScopeTerm{Kind: chart.TermSeat, Unit: "engineering", ID: "sarah-chen"}.Path()
	other := chart.ScopeTerm{Kind: chart.TermSeat, Unit: "engineering", ID: "marcus-rivera"}.Path()

	// The closure walks UPWARD: a stored deferred path found here is one
	// that COVERS what this operation is about.
	closure := statelog.ScopeSet{Paths: []string{seat}}.Closure()
	for _, want := range []string{root, unit, seat} {
		if !slices.Contains(closure, want) {
			t.Errorf("a write to %q does not probe %q — a record deferred there "+
				"would stop blocking this write, silently. Closure was %v",
				seat, want, closure)
		}
	}
	if slices.Contains(closure, other) {
		t.Errorf("a write to %q probes its neighbour %q — every seat in a unit "+
			"would then wait behind every other", seat, other)
	}

	// And the descendant direction: a record deferred BENEATH something this
	// operation is about.
	if roots := (statelog.ScopeSet{Paths: []string{unit}}).Roots(); len(roots) != 1 ||
		!strings.HasPrefix(seat, roots[0]+statelog.ScopeSeparator) {
		t.Errorf("%q is not under the unit root %v — a deferral on one seat "+
			"would not block a write that rewrites its whole unit", seat, roots)
	}
	if !strings.HasPrefix(unit, root+statelog.ScopeSeparator) {
		t.Errorf("%q is not under the root %q — the widest term the alphabet "+
			"has would cover nothing, which is the one thing it exists for",
			unit, root)
	}
}

// AN UNREADABLE SCOPE IS THE ROOT, AND AN UNKNOWN TERM KIND IS TOO.
//
// Both run on a node decoding a record a NEWER build wrote, and the honest
// reading of a blast radius this build cannot bound is "everything". The
// failure the other way is silent: a scope narrowed to whatever this build
// recognised is a deferral that blocks less than the record makes stale.
func TestAnUnreadableScopeWidensToTheWholeChart(t *testing.T) {
	t.Parallel()

	root := chart.RootPath()
	subject := chart.SeatSubject("sarah-chen")

	for name, raw := range map[string]string{
		"a shape this build has never seen":   `{"future":"shape"}`,
		"a sentinel this build does not know": `"z/engineering"`,
		"an empty enumeration":                `[]`,
		"a term kind from a later build":      `[{"k":"squad","i":"platform"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var scope chart.ScopeSet
			if err := json.Unmarshal([]byte(raw), &scope); err != nil {
				t.Fatalf("decoding %s returned an error: %v — returning one here "+
					"would take the whole two-pass decode down with it, and the "+
					"record could then only be dropped", raw, err)
			}
			paths := scope.Resolve(subject).Paths
			if len(paths) != 1 || paths[0] != root {
				t.Errorf("%s resolved to %v, want [%q]", raw, paths, root)
			}
		})
	}
}

// THE SENTINEL IS REFUSED WHERE IT WOULD WIDEN SILENTLY.
//
// A unit and a seat are rows the alphabet renders, and the barrier's sentinel
// resolves to the framework's own scope. An eviction and a generation are the
// two whose sentinel DOES resolve to the whole chart and are allowed it
// anyway, because that is what a gate is about — each decides whether records
// on every subject count, so anything narrower would be a claim the record
// does not make.
//
// The structure and a key claim name no row at all — so a sentinel there has
// no narrower path than the whole chart either, and there it is a widening
// rather than a statement: a writer that took the shortcut would produce a
// record that blocks the company while looking, in the row an operator reads,
// like one that touched one seat.
func TestTheSentinelIsRefusedOnASubjectThatNamesNoRow(t *testing.T) {
	t.Parallel()

	// THE EXPECTATION IS WRITTEN OUT rather than read from
	// [chart.ObjectKind.SentinelScoped], which is the whole difference
	// between a check and a restatement: asking the predicate what it
	// expects makes the case pass for every value the predicate could take,
	// including "true for everything".
	want := map[chart.ObjectKind]bool{
		chart.KindUnit:       true,
		chart.KindSeat:       true,
		chart.KindBarrier:    true,
		chart.KindEviction:   true,
		chart.KindGeneration: true,
		chart.KindTree:       false,
		chart.KindRekey:      false,
	}
	if len(want) != len(chart.ObjectKinds) {
		t.Fatalf("this case classifies %d kinds and the domain declares %d — a "+
			"kind added without a verdict here would be skipped rather than "+
			"checked", len(want), len(chart.ObjectKinds))
	}

	sentinel := chart.ScopeSet{Subject: true}
	for _, kind := range chart.ObjectKinds {
		allowed, classified := want[kind]
		if !classified {
			t.Errorf("%s is declared and this case has no verdict for it", kind)
			continue
		}
		if got := kind.SentinelScoped(); got != allowed {
			t.Errorf("%s.SentinelScoped() = %v, want %v", kind, got, allowed)
		}
		subject := chart.Subject{Kind: kind, ID: "engineering"}
		if kind == chart.KindTree || kind == chart.KindBarrier {
			subject.ID = ""
		}
		err := sentinel.Validate(subject)
		switch {
		case allowed && err != nil:
			t.Errorf("a %s record may state the sentinel and it was refused: %v",
				kind, err)
		case !allowed && err == nil:
			t.Errorf("a %s subject names no row and the sentinel was accepted — "+
				"it would resolve to the whole chart with nothing saying so", kind)
		}
	}
	// The barrier's sentinel leaves this alphabet entirely, which is what
	// keeps a linearizable read from waiting behind every other one.
	if got := sentinel.Resolve(chart.BarrierSubject()).Paths; len(got) != 1 ||
		got[0] != statelog.BarrierScope {
		t.Errorf("a barrier resolves to %v, want [%q] — a scope that intersected "+
			"anything would make every linearizable read wait behind every other",
			got, statelog.BarrierScope)
	}
}

// THE SENTINEL COSTS ONE BYTE, AND A UNIT RIDES IT.
//
// The unit is on the RECORD rather than derived from the subject, because a
// seat's unit is a fact about the row and moving a seat between units is the
// whole point of this domain. Leaving it out is silent: every seat record would
// file its deferral under the org root while every unit-scoped read probed its
// own, and the containment probe would simply never match.
func TestTheSentinelCarriesTheUnitItWasWrittenIn(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		scope chart.ScopeSet
		wire  string
		want  string
	}{
		"a seat in a unit": {
			scope: chart.ScopeSet{Subject: true, Unit: "engineering"},
			wire:  `"s/engineering"`,
			want:  "g/u/engineering/s/sarah-chen",
		},
		"a seat at the org root": {
			scope: chart.ScopeSet{Subject: true},
			wire:  `"s"`,
			want:  "g/u/" + chart.RootUnit + "/s/sarah-chen",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(tc.scope)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(body) != tc.wire {
				t.Errorf("encoded as %s, want %s", body, tc.wire)
			}
			var back chart.ScopeSet
			if err := json.Unmarshal(body, &back); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := back.Resolve(chart.SeatSubject("sarah-chen")).Paths
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("resolved to %v, want [%q]", got, tc.want)
			}
		})
	}
}

// canonical is the subject and the scope a WRITER of one kind states.
//
// It is the shape the guard above is about: what this build publishes, not what
// it will accept off the wire from a peer. Every kind is covered, because the
// walk is over the declared enum — a kind added without a case here fails
// rather than being skipped.
func canonical(kind chart.ObjectKind) (chart.Subject, chart.ScopeSet) {
	switch kind {
	case chart.KindTree:
		// A structural record ENUMERATES: the object that moved and the
		// units at both ends of the move.
		return chart.TreeSubject(), chart.BatchScope([]chart.ScopeTerm{
			{Kind: chart.TermSeat, Unit: "engineering", ID: "sarah-chen"},
			{Kind: chart.TermUnit, ID: "engineering"},
			{Kind: chart.TermUnit, ID: "product"},
		})
	case chart.KindUnit:
		return chart.UnitSubject("engineering"), chart.ScopeSet{Subject: true}
	case chart.KindSeat:
		return chart.SeatSubject("sarah-chen"),
			chart.ScopeSet{Subject: true, Unit: "engineering"}
	case chart.KindBarrier:
		return chart.BarrierSubject(), chart.ScopeSet{Subject: true}
	case chart.KindRekey:
		return chart.RekeySubject("platform"), chart.BatchScope([]chart.ScopeTerm{
			{Kind: chart.TermUnit, ID: "platform"},
		})
	case chart.KindEviction:
		return chart.EvictionSubject("node-b"), chart.ScopeSet{Subject: true}
	case chart.KindGeneration:
		return chart.GenerationSubject(2), chart.ScopeSet{Subject: true}
	}
	panic(fmt.Sprintf("chart: the scope guard has no canonical record for kind %q "+
		"— a kind added to the enum without one would be skipped rather than "+
		"checked", kind))
}

// units builds n distinct unit terms.
func units(n int) []chart.ScopeTerm {
	out := make([]chart.ScopeTerm, 0, n)
	for i := range n {
		out = append(out, chart.ScopeTerm{Kind: chart.TermUnit, ID: fmt.Sprintf("unit-%d", i)})
	}
	return out
}
