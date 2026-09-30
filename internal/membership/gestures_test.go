package membership

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// THE GESTURES: out and in change the flag the map places by and the record
// of who asked; a hold and its release change no member. Each refuses what
// cannot be done by a sentinel the caller can name to an operator, and each
// leaves what it was given alone.
func TestTheGestures(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	base := first(t, roster(3), c)

	t.Run("out and in", func(t *testing.T) {
		t.Parallel()
		s, d, err := Out(base.s, base.d, "data-01", "ops@example.com", "replacing the disk", t0)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := d.Member("data-01")
		if !m.Out || s.TakenOut["data-01"] != (Gesture{By: "ops@example.com",
			Reason: "replacing the disk", At: t0}) {
			t.Fatalf("taken out: %+v, %+v", m, s.TakenOut)
		}
		if orig, _ := base.d.Member("data-01"); orig.Out || base.s.TakenOut != nil {
			t.Fatal("Out wrote into what it was given")
		}
		// A tick keeps it out and changes nothing.
		out := record{s, d}
		if next := tick(t, out, roster(3), c); !reflect.DeepEqual(next, out) {
			t.Fatalf("a tick after taking a member out changed %+v to %+v", out, next)
		}
		s, d, err = In(s, d, "data-01")
		if err != nil {
			t.Fatal(err)
		}
		if m, _ := d.Member("data-01"); m.Out || s.TakenOut != nil {
			t.Fatalf("put back: %+v, %+v", m, s.TakenOut)
		}
	})

	// A GESTURE THE MAP ALREADY SAYS CHANGES NOTHING: out of a member already
	// out keeps the first gesture's record — a retry of a gesture whose answer
	// was lost neither overwrites who took it out and why nor moves the
	// record's version under a maintainer's tick — and in of a member already
	// placed on, or a release with no hold, is what it was given.
	t.Run("a repeat changes nothing", func(t *testing.T) {
		t.Parallel()
		s, d, err := Out(base.s, base.d, "data-01", "ops@example.com", "replacing the disk", t0)
		if err != nil {
			t.Fatal(err)
		}
		out := record{s, d}
		for name, repeat := range map[string]func() (record, record, error){
			"out of a member already out": func() (record, record, error) {
				s, d, err := Out(out.s, out.d, "data-01", "someone-else", "a retry", t0.Add(time.Hour))
				return out, record{s, d}, err
			},
			"in of a member not out": func() (record, record, error) {
				s, d, err := In(base.s, base.d, "data-00")
				return base, record{s, d}, err
			},
			"a release with no hold": func() (record, record, error) {
				return base, record{Release(base.s), base.d}, nil
			},
		} {
			given, answered, err := repeat()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !reflect.DeepEqual(answered, given) {
				t.Errorf("%s changed\n%+v\nto\n%+v", name, given, answered)
			}
		}
	})

	t.Run("refusals", func(t *testing.T) {
		t.Parallel()
		if _, _, err := Out(base.s, base.d, "data-09", "ops", "", t0); !errors.Is(err, ErrUnknownMember) ||
			errors.Is(err, ErrRemovedMember) {
			t.Errorf("out of an unknown node = %v", err)
		}
		// A node removed and not seen back places nothing already: out is
		// refused by a sentinel of its own, still an unknown member.
		removed := ticks(t, base, roster(2), c, OutTicks)
		if _, _, err := Out(removed.s, removed.d, "data-02", "ops", "", t0); !errors.Is(err, ErrRemovedMember) ||
			!errors.Is(err, ErrUnknownMember) {
			t.Errorf("out of a removed node = %v, want ErrRemovedMember", err)
		}
		if _, _, err := In(base.s, base.d, "data-09"); !errors.Is(err, ErrUnknownMember) {
			t.Errorf("in of an unknown node = %v", err)
		}
		// FOUR MEMBERS FOR TWO COPIES, so every out below leaves the map
		// its two copies and only presence is in question: data-00 out,
		// and data-01 and data-02 go quiet, leaving data-03 the last
		// member present to place on.
		four := first(t, roster(4), c)
		s, d, err := Out(four.s, four.d, "data-00", "ops", "", t0)
		if err != nil {
			t.Fatal(err)
		}
		quiet := tick(t, record{s, d}, without(roster(4), "data-01", "data-02"), c)
		if _, _, err := Out(quiet.s, quiet.d, "data-03", "ops", "", t0); !errors.Is(err, ErrNothingPlaceable) {
			t.Errorf("taking out the last present placeable member = %v", err)
		}
		if _, _, err := Out(quiet.s, quiet.d, "data-01", "ops", "", t0); err != nil {
			t.Errorf("taking out a quiet member beside a present one = %v", err)
		}
		one := first(t, roster(1), c)
		if _, _, err := Out(one.s, one.d, "data-00", "ops", "", t0); !errors.Is(err, ErrNothingPlaceable) {
			t.Errorf("taking out the only member = %v", err)
		}
	})

	t.Run("hold and release", func(t *testing.T) {
		t.Parallel()
		for _, d := range []time.Duration{0, -time.Minute, MaxHold + time.Second} {
			if _, err := HoldFor(base.s, d, "ops", "", t0); !errors.Is(err, ErrHoldRange) {
				t.Errorf("a hold of %s = %v, want ErrHoldRange", d, err)
			}
		}
		held, err := HoldFor(base.s, MaxHold, "ops", "datacentre move", t0)
		if err != nil {
			t.Fatal(err)
		}
		want := Hold{Until: t0.Add(MaxHold), By: "ops", Reason: "datacentre move", At: t0}
		if held.Hold == nil || *held.Hold != want || base.s.Hold != nil {
			t.Fatalf("held: %+v", held.Hold)
		}
		if !held.Hold.Active(t0.Add(MaxHold-time.Second)) || held.Hold.Active(t0.Add(MaxHold)) {
			t.Fatal("the hold is not active exactly until it ends")
		}
		if released := Release(held); released.Hold != nil || held.Hold == nil {
			t.Fatalf("released: %+v, and the held state %+v", released.Hold, held.Hold)
		}
	})

	// A HOLD SENT AGAIN EXTENDS: it always writes a new hold ending its
	// length after THIS call, who and why included — the operator stating
	// now how much longer the maintenance needs. A retry is therefore never
	// a no-op, and the surfaces say so.
	t.Run("a hold sent again extends", func(t *testing.T) {
		t.Parallel()
		held, err := HoldFor(base.s, time.Hour, "ops", "rolling upgrade", t0)
		if err != nil {
			t.Fatal(err)
		}
		later := t0.Add(20 * time.Minute)
		again, err := HoldFor(held, time.Hour, "ops-2", "still upgrading", later)
		if err != nil {
			t.Fatal(err)
		}
		want := Hold{Until: later.Add(time.Hour), By: "ops-2", Reason: "still upgrading", At: later}
		if again.Hold == nil || *again.Hold != want || held.Hold.Until != t0.Add(time.Hour) {
			t.Fatalf("held again: %+v, want %+v", again.Hold, want)
		}
	})

	// PROBATION AND OUT ARE SEPARATE: a member on probation may be taken
	// out, and stays out once its probation ends — the operator's decision
	// outlives the maintainer's.
	t.Run("out on probation", func(t *testing.T) {
		t.Parallel()
		live := roster(3)
		probation := tick(t, ticks(t, base, roster(2), c, OutTicks), live, c)
		s, d, err := Out(probation.s, probation.d, "data-02", "ops", "retire it", t0)
		if err != nil {
			t.Fatal(err)
		}
		if m, _ := d.Member("data-02"); !m.Out || !m.Probation {
			t.Fatalf("taken out on probation: %+v", m)
		}
		trusted := ticks(t, record{s, d}, live, c, StableTicks)
		if m, _ := trusted.d.Member("data-02"); !m.Out || m.Probation || m.Placeable() {
			t.Fatalf("after its probation: %+v, want out and not placeable", m)
		}
	})
}

// A MEMBER BACK FROM A MISSED TICK IS PRESENT to the gesture that takes
// another out. Its absence run stays open for a whole grace while it proves
// itself stable, but the latest tick saw it — placed on, holding its lease,
// serving — so it is somewhere for copies to land. Only a member the latest
// tick counted absent is absent now: reading every open run as gone refused
// taking a member out for up to ten minutes after one missed heartbeat.
func TestAMemberBackFromAMissedTickIsPresentToOut(t *testing.T) {
	t.Parallel()
	// ONE COPY, so the other member can take data-00's and presence is the
	// only question: at two, taking either of two members out would drop a
	// copy, and [Out] refuses that whoever is present.
	c := company(1, 1, "")
	live := roster(2)
	missed := tick(t, first(t, live, c), without(live, "data-01"), c)
	if _, _, err := Out(missed.s, missed.d, "data-00", "ops", "", t0); !errors.Is(err, ErrNothingPlaceable) {
		t.Fatalf("taking out the only member the latest tick saw = %v, want ErrNothingPlaceable", err)
	}

	back := tick(t, missed, live, c)
	if run, open := back.s.Absence["data-01"]; !open || run.Ticks != 1 || run.Present != 1 {
		t.Fatalf("setup: data-01's run is %+v (open %v), want one tick gone and seen back once",
			run, open)
	}
	_, d, err := Out(back.s, back.d, "data-00", "ops", "replacing the disk", t0)
	if err != nil {
		t.Fatalf("taking out a member beside one back from a missed tick = %v", err)
	}
	if m, _ := d.Member("data-00"); !m.Out {
		t.Fatalf("data-00 was not taken out: %+v", m)
	}

	// AND GONE AGAIN is absent again, on the tick that counts it.
	gone := tick(t, back, without(live, "data-01"), c)
	if _, _, err := Out(gone.s, gone.d, "data-00", "ops", "", t0); !errors.Is(err, ErrNothingPlaceable) {
		t.Fatalf("taking out the only member present once the other left again = %v", err)
	}
}

// A REFUSAL NAMES NO PACKAGE: the map that answered puts its own name in
// front, and a surface shows the result as the detail of its refusal — so a
// package name here would be a word the operator cannot act on, twice.
func TestARefusalNamesNoPackage(t *testing.T) {
	t.Parallel()
	for _, err := range []error{ErrUnknownMember, ErrRemovedMember, ErrNothingPlaceable,
		ErrNowhereToRebuild, ErrHoldRange, Company{}.Validate()} {
		if strings.Contains(err.Error(), "membership") {
			t.Errorf("%q names the package", err)
		}
	}
	// And a company's refusal names the field an operator changes.
	for _, c := range []struct {
		company Company
		field   string
	}{
		{Company{Epoch: 1, Replicas: 0, Block: "objects"}, "objects.replicas"},
		{Company{Epoch: 1, Replicas: 3, FailureDomain: "my zone", Block: "estate"}, "estate.failure_domain"},
	} {
		if err := c.company.Validate(); err == nil || !strings.HasPrefix(err.Error(), c.field) {
			t.Errorf("%+v refused as %v, want it to name %s", c.company, err, c.field)
		}
	}
}

// AN OUT NEVER DROPS A COPY. Taking a member out means its copies are rebuilt
// on the others while it serves; where the map would place fewer copies
// without it than with it — as many members as copies, or already short —
// there is nowhere to rebuild them, and the out is refused with the record
// left as it was given, so a fleet shrinks only by the company lowering its
// copies first. The question is the draw's: a member absent right now is one
// the map still places on, and a member that places nothing already — on
// probation — drops nothing by going out.
func TestAnOutNeverDropsACopy(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, r record, node string, want string) {
		t.Helper()
		s, d, err := Out(r.s, r.d, node, "ops", "shrinking", t0)
		if !errors.Is(err, ErrNowhereToRebuild) {
			t.Fatalf("taking %s out = %v, want ErrNowhereToRebuild", node, err)
		}
		if !reflect.DeepEqual(record{s, d}, r) {
			t.Fatalf("a refused out changed the record:\n%+v\nto\n%+v", r, record{s, d})
		}
		// The detail counts the copies, says which activation the map's
		// count came from — the one an operator who lowered it compares
		// against — and names the node last, as every refusal here does.
		stamped := r.s.Config.Copies()
		if !strings.HasSuffix(err.Error(), ": "+node) || !strings.Contains(err.Error(), want) ||
			!strings.Contains(err.Error(), stamped) {
			t.Fatalf("the refusal %q does not say %q and %q and end with %s",
				err, want, stamped, node)
		}
	}
	taken := func(t *testing.T, r record, node string) record {
		t.Helper()
		s, d, err := Out(r.s, r.d, node, "ops", "shrinking", t0)
		if err != nil {
			t.Fatalf("taking %s out = %v", node, err)
		}
		if m, _ := d.Member(node); !m.Out {
			t.Fatalf("%s was not taken out: %+v", node, m)
		}
		return record{s, d}
	}

	t.Run("as many members as copies", func(t *testing.T) {
		t.Parallel()
		refused(t, first(t, roster(3), company(1, 3, "")), "data-01",
			"only 2 of the 3 copies")
	})
	t.Run("already short of copies", func(t *testing.T) {
		t.Parallel()
		// Three asked for, two placed: without either, one.
		refused(t, first(t, roster(2), company(1, 3, "")), "data-00",
			"only 1 of the 2 copies")
	})
	t.Run("one to spare, then none", func(t *testing.T) {
		t.Parallel()
		c := company(1, 3, "")
		one := taken(t, first(t, roster(4), c), "data-00")
		refused(t, one, "data-01", "only 2 of the 3 copies")
	})
	t.Run("fewer copies make room", func(t *testing.T) {
		t.Parallel()
		// The company lowering its copies is how a fleet shrinks: the
		// same three members at two copies have one to spare.
		taken(t, first(t, roster(3), company(1, 2, "")), "data-01")
	})
	t.Run("the count's activation, or none", func(t *testing.T) {
		t.Parallel()
		if got, want := (ConfigSource{Epoch: 7}).Copies(),
			"the copy count company activation 7 set"; got != want {
			t.Fatalf("activation 7's count reads %q, want %q", got, want)
		}
		if got := (ConfigSource{}).Copies(); strings.Contains(got, "activation 0") {
			t.Fatalf("a count no activation stamped reads %q, as though one had", got)
		}
	})
	t.Run("a lowered count makes room once the map has it", func(t *testing.T) {
		t.Parallel()
		// The out judges the MAP's count, which the duty stamps on its
		// tick: until then the refusal says the count is still the one
		// activation 1 set, and the tick that stamps activation 2's is
		// what lets the same out through.
		at3 := first(t, roster(3), company(1, 3, ""))
		refused(t, at3, "data-01", "only 2 of the 3 copies")
		taken(t, tick(t, at3, roster(3), company(2, 2, "")), "data-01")
	})
	t.Run("an absent member still takes copies", func(t *testing.T) {
		t.Parallel()
		c := company(1, 3, "")
		quiet := tick(t, first(t, roster(4), c), without(roster(4), "data-03"), c)
		if !absentNow(quiet.s, "data-03") {
			t.Fatalf("setup: data-03 is not absent: %+v", quiet.s.Absence)
		}
		taken(t, quiet, "data-00")
	})
	t.Run("a member on probation places nothing to drop", func(t *testing.T) {
		t.Parallel()
		c := company(1, 2, "")
		// data-02 removed for absence and seen back: on probation, so the
		// two copies sit on data-00 and data-01 alone.
		back := tick(t, ticks(t, first(t, roster(3), c), roster(2), c, OutTicks), roster(3), c)
		if m, _ := back.d.Member("data-02"); !m.Probation {
			t.Fatalf("setup: data-02 is not on probation: %+v", m)
		}
		taken(t, back, "data-02")
		refused(t, back, "data-00", "only 1 of the 2 copies")
	})
}

// A BAR OUTLIVES MEMBERSHIP: an evicted node is placed on nothing until it is
// put back, whatever becomes of it meanwhile.
//
// Barred while a member, it is out at once; gone past the grace, it is removed
// — and a bar, unlike an out, stays; gone for another grace, it is forgotten,
// and the bar still stays; seen back and present for far longer than any
// probation, it is a member again and still placed on nothing. Only In lifts
// it. And a node the map has already removed, or never held, is barred all the
// same — the usual case, since an operator evicts a machine that is gone.
func TestABarOutlivesMembership(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	base := first(t, roster(3), c)
	s, d, err := Bar(base.s, base.d, "data-02", "ops@example.com", "evicted", t0)
	if err != nil {
		t.Fatal(err)
	}
	barred := record{s, d}
	if placeable(barred, "data-02") {
		t.Fatal("a barred member is still placed on")
	}
	gone := ticks(t, barred, without(roster(3), "data-02"), c, OutTicks)
	if gone.d.Holds("data-02") {
		t.Fatal("the premise: a barred member gone past the grace is removed")
	}
	forgotten := ticks(t, gone, without(roster(3), "data-02"), c, OutTicks)
	if _, remembered := forgotten.s.Removed["data-02"]; remembered {
		t.Fatal("the premise: a removed node gone for another grace is forgotten")
	}
	for name, r := range map[string]record{"removed": gone, "forgotten": forgotten} {
		back := ticks(t, r, roster(3), c, 2*StableTicks)
		if !back.d.Holds("data-02") {
			t.Errorf("%s: the node back for %d ticks is not a member again", name, 2*StableTicks)
		}
		if placeable(back, "data-02") {
			t.Errorf("%s: a barred node back for %d ticks is placed on", name, 2*StableTicks)
		}
		s, d, err := In(back.s, back.d, "data-02")
		if err != nil {
			t.Fatalf("%s: In: %v", name, err)
		}
		if !placeable(record{s, d}, "data-02") || s.Barred != nil {
			t.Errorf("%s: put back, the node is placeable %v with bars %v", name,
				placeable(record{s, d}, "data-02"), s.Barred)
		}
	}

	// A NODE THE MAP NO LONGER HOLDS, AND ONE IT NEVER DID, ARE BARRED ALL
	// THE SAME — where Out would have answered that there is nothing to
	// take out.
	unbarred := ticks(t, base, without(roster(3), "data-02"), c, OutTicks)
	for _, node := range []string{"data-02", "data-09"} {
		s, d, err := Bar(unbarred.s, unbarred.d, node, "ops", "evicted", t0)
		if err != nil {
			t.Fatalf("bar %s, which the map does not hold: %v", node, err)
		}
		if _, recorded := s.Barred[node]; !recorded {
			t.Errorf("the bar on %s was not recorded", node)
		}
		back := ticks(t, record{s, d}, append(roster(3), up("data-09", 1)), c, 2*StableTicks)
		if placeable(back, node) {
			t.Errorf("%s, barred while the map did not hold it, is placed on once back", node)
		}
		s, _, err = In(s, d, node)
		if err != nil || s.Barred != nil {
			t.Errorf("In of barred %s the map does not hold = (%v, %v), want the bar lifted",
				node, s.Barred, err)
		}
	}
}

// A BAR IS A GESTURE LIKE THE REST: barring a node already barred changes
// nothing, the first gesture's record kept; barring the last member present to
// place on is refused, as taking it out is; and what it was given is left
// alone.
func TestABarIsRepeatableAndRefusesTheLastMember(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	base := first(t, roster(3), c)
	s, d, err := Bar(base.s, base.d, "data-01", "ops@example.com", "evicted", t0)
	if err != nil {
		t.Fatal(err)
	}
	if base.s.Barred != nil {
		t.Fatal("Bar wrote into what it was given")
	}
	again, againD, err := Bar(s, d, "data-01", "someone-else", "a retry", t0.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(record{again, againD}, record{s, d}) {
		t.Errorf("a repeated bar changed the record (%v)", err)
	}
	one := first(t, roster(1), c)
	if _, _, err := Bar(one.s, one.d, "data-00", "ops", "evicted", t0); !errors.Is(err, ErrNothingPlaceable) {
		t.Errorf("barring the only member = %v, want ErrNothingPlaceable", err)
	}
}

// A BAR IS NOT REFUSED FOR THE COPIES IT LEAVES NOWHERE TO GO, where an out is:
// an out keeps the copies of a node that still serves them, and a bar records
// a machine judged gone, whose copies are not there to keep.
func TestABarIsNotRefusedForCopiesItCannotKeep(t *testing.T) {
	t.Parallel()
	full := first(t, roster(3), company(1, 3, ""))
	if _, _, err := Out(full.s, full.d, "data-01", "ops", "shrinking", t0); !errors.Is(err, ErrNowhereToRebuild) {
		t.Fatalf("the premise: taking data-01 out of three members at three copies = %v, "+
			"want ErrNowhereToRebuild", err)
	}
	s, d, err := Bar(full.s, full.d, "data-01", "ops", "evicted", t0)
	if err != nil {
		t.Fatalf("barring data-01 = %v, want it barred", err)
	}
	if placeable(record{s, d}, "data-01") {
		t.Error("the barred member is still placed on")
	}
}
