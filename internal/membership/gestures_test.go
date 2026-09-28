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
		s, d, err := Out(base.s, base.d, "data-00", "ops", "", t0)
		if err != nil {
			t.Fatal(err)
		}
		// data-01 goes quiet: data-02 is the last member present to
		// place on.
		quiet := tick(t, record{s, d}, without(roster(3), "data-01"), c)
		if _, _, err := Out(quiet.s, quiet.d, "data-02", "ops", "", t0); !errors.Is(err, ErrNothingPlaceable) {
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
	c := company(1, 2, "")
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
		ErrHoldRange, Company{}.Validate()} {
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
