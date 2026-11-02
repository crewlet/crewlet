package seataccess_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// namingVendor is the fake vendor drawing its steps with the lane's Options,
// as a vendor whose levels are live does.
type namingVendor struct{ *seataccesstest.Vendor }

func (v namingVendor) StepsFor(in seataccess.StepsInput) []seataccess.Step {
	name := in.Want.Level
	if level, _, ok := in.Options.Level(in.Want.Level); ok {
		name = level.Name
	}
	return []seataccess.Step{{Op: in.Op, Verb: seataccess.VerbCreate, Text: "Create it as " + name,
		By: seataccess.ByCrewlet}}
}

// THE PLAN NAMES A LEVEL BY THE VENDOR'S NAME where the vendor draws with
// the lane's Options, and a vendor that does not is drawn as before.
func TestThePlanHandsTheLanesOptionsToAVendorThatNamesItsLevels(t *testing.T) {
	t.Parallel()
	in := seataccess.SeatInput{
		Seat: seataccess.SeatRef{Handle: "sre"},
		Want: map[seataccess.Lane]seataccess.Desired{
			seataccess.LaneDatadog: {Enabled: true, Level: "role-1"},
		},
		Options: map[seataccess.Lane]seataccess.Options{seataccess.LaneDatadog: {
			Levels: []seataccess.Level{{ID: "role-1", Name: "Datadog Standard Role"}},
		}},
	}
	named := namingVendor{seataccesstest.NewVendor(integration.KindDatadog)}
	plan, err := seataccess.Plan(named, in)
	if err != nil {
		t.Fatal(err)
	}
	steps := plan[seataccess.LaneDatadog]
	if len(steps) != 1 || steps[0].Text != "Create it as Datadog Standard Role" {
		t.Fatalf("steps = %+v", steps)
	}
	plain, err := seataccess.Plan(seataccesstest.NewVendor(integration.KindDatadog), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain[seataccess.LaneDatadog]) == 0 {
		t.Fatal("a vendor drawing without Options drew nothing")
	}
}

// A SET OF LEVELS IS OFFERED ONLY WHERE EVERY ONE OF ITS LEVELS IS, and a
// single-level lane reads a level with a space in it as one id.
func TestASetOfLevelsIsOfferedOnlyWhereEveryLevelIs(t *testing.T) {
	t.Parallel()
	set := seataccess.Options{Multiple: true, Levels: []seataccess.Level{
		{ID: "read:space", Name: "View"}, {ID: "create:page", Name: "Add pages"},
		{ID: "administer:space", Name: "Admin", Administers: true},
	}}
	if levels, ok := set.LevelsOf("create:page read:space"); !ok || len(levels) != 2 {
		t.Fatalf("an offered set = %+v, %v", levels, ok)
	}
	if _, ok := set.LevelsOf("purge:space read:space"); ok {
		t.Fatal("a set naming one level not offered was accepted")
	}
	if _, ok := set.LevelsOf(""); ok {
		t.Fatal("an empty set was accepted")
	}
	single := seataccess.Options{Levels: []seataccess.Level{{ID: "a b"}}}
	if _, ok := single.LevelsOf("a b"); !ok {
		t.Fatal("a single-level lane split its level")
	}
}

// ADDING ADMIN TO A SET OF PERMISSIONS IS A RAISE (D10); keeping it, or
// adding something else beside it, is not.
func TestASetGainingAnAdministeringLevelNeedsAStepUp(t *testing.T) {
	t.Parallel()
	opts := seataccess.Options{Multiple: true, Levels: []seataccess.Level{
		{ID: "read:space"}, {ID: "create:page"}, {ID: "administer:space", Administers: true},
	}}
	on := func(level string) seataccess.Desired { return seataccess.Desired{Enabled: true, Level: level} }
	for _, tc := range []struct {
		before, after string
		want          bool
	}{
		{"read:space", "administer:space read:space", true},
		{"administer:space read:space", "administer:space create:page read:space", false},
		{"read:space", "create:page read:space", false},
	} {
		if got := seataccess.NeedsStepUp(opts, on(tc.before), on(tc.after)); got != tc.want {
			t.Errorf("%q to %q: NeedsStepUp = %v, want %v", tc.before, tc.after, got, tc.want)
		}
	}
	if !seataccess.NeedsStepUp(opts, seataccess.Desired{}, on("administer:space read:space")) {
		t.Error("turning a seat on with Admin did not need a step-up")
	}
}
