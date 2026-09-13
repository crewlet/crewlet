package org

import (
	"errors"
	"strings"
	"testing"
)

// The admission rules: seat names and unit names are unique across the whole
// company. They are a CLASS of their own, apart from Validate, because stored
// companies predate them and still run exactly as they did; the config layer
// refuses a submitted document for breaking one and applies a stored revision
// with a warning. What these pin is the rule itself, its class, and the shape
// of what it reports.

// violations flattens a joined error into the leaf errors that wrap sentinel.
func violations(err, sentinel error) []error {
	var out []error
	var walk func(error)
	walk = func(e error) {
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			for _, inner := range joined.Unwrap() {
				walk(inner)
			}
			return
		}
		if e != nil && errors.Is(e, sentinel) {
			out = append(out, e)
		}
	}
	walk(err)
	return out
}

// A SEAT NAME IS A REFERENCE, so two seats cannot share one, even with
// distinct handles, which is exactly the collision the handle rule misses.
func TestDuplicateSeatNamesAreAnAdmissionRule(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Backend", Roles: []*Role{{Name: "Engineer", DeclaredHandle: "backend-engineer"}}},
		{Name: "Frontend", Roles: []*Role{{Name: "Engineer", DeclaredHandle: "frontend-engineer"}}},
	}})

	got := violations(o.ValidateAdmission(), ErrDuplicateSeatName)
	if len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want one duplicate seat name", o.ValidateAdmission())
	}
	for _, want := range []string{`"Engineer"`, `"backend-engineer"`, `"frontend-engineer"`,
		`in unit "Backend"`, `in unit "Frontend"`} {
		if !strings.Contains(got[0].Error(), want) {
			t.Errorf("the message does not name %s: %v", want, got[0])
		}
	}
	// NOT A RUNNABLE RULE. A stored company carrying this runs as it always
	// did, and folding the rule into Validate would refuse to apply it.
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil: a duplicate seat name is an admission rule", err)
	}
}

// A UNIT NAME IS UNIQUE ACROSS THE TREE, not among siblings: every reference
// to a unit searches the whole tree and takes the first match.
func TestDuplicateUnitNamesAnywhereInTheTreeAreAnAdmissionRule(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Engineering", Children: []*Unit{{Name: "Platform", Roles: []*Role{{Name: "Dev A"}}}}},
		{Name: "Product", Children: []*Unit{{Name: "Platform", Roles: []*Role{{Name: "Dev B"}}}}},
	}})

	got := violations(o.ValidateAdmission(), ErrDuplicateUnitName)
	if len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want one duplicate unit name", o.ValidateAdmission())
	}
	for _, want := range []string{`"Platform"`, `under unit "Engineering"`, `under unit "Product"`} {
		if !strings.Contains(got[0].Error(), want) {
			t.Errorf("the message does not name %s: %v", want, got[0])
		}
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil: a duplicate unit name is an admission rule", err)
	}
}

// ONE MESSAGE PER DUPLICATED KEY, NAMING EVERY ENTITY.
//
// The handle rule reported pairs: three seats on one handle read as two
// separate collisions, neither naming all three. One key is one mistake.
func TestADuplicatedKeyIsOneMessageNamingEveryEntity(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "Dev"}},
		Units: []*Unit{
			{Name: "Core", Roles: []*Role{{Name: "Dev"}, {Name: "dev"}}},
			{Name: "Core", Roles: []*Role{{Name: "Ops"}}},
			{Name: "Edge", Children: []*Unit{{Name: "Core", Roles: []*Role{{Name: "Ops Two"}}}}},
		},
	})

	handles := violations(o.Validate(), ErrDuplicateHandle)
	if len(handles) != 1 {
		t.Fatalf("%d duplicate handle messages, want one for the one handle: %v", len(handles), handles)
	}
	if msg := handles[0].Error(); !strings.Contains(msg, "3 seats") ||
		!strings.Contains(msg, `seat "dev" in unit "Core"`) || !strings.Contains(msg, `seat "Dev" at the root`) {
		t.Errorf("the handle message does not name all three seats: %s", msg)
	}

	names := violations(o.ValidateAdmission(), ErrDuplicateSeatName)
	if len(names) != 1 || !strings.Contains(names[0].Error(), "2 seats") {
		t.Errorf("seat name messages = %v, want one naming the two seats called \"Dev\"", names)
	}
	units := violations(o.ValidateAdmission(), ErrDuplicateUnitName)
	if len(units) != 1 || !strings.Contains(units[0].Error(), "3 units") ||
		!strings.Contains(units[0].Error(), `under unit "Edge"`) {
		t.Errorf("unit name messages = %v, want one naming all three units called \"Core\"", units)
	}
}

// AN IDENTITY THAT IS MISSING IS NOT A COLLISION.
//
// A nameless seat, a name that derives no handle and a nameless unit are each
// refused by their own rule already. Counting them as duplicates of each
// other reported one mistake two or three times, including a "duplicate
// handle" naming the empty string.
func TestAMissingIdentityIsNeverReportedAsADuplicate(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: ""}, {Name: " "}, {Name: "!!!"}, {Name: "???"}},
		Units: []*Unit{{Name: "", Roles: []*Role{{Name: "Dev"}}}, {Name: "  ", Roles: []*Role{{Name: "Ops"}}}},
	})

	for _, sentinel := range []error{ErrDuplicateHandle, ErrDuplicateSeatName, ErrDuplicateUnitName} {
		if got := violations(errors.Join(o.Validate(), o.ValidateAdmission()), sentinel); len(got) != 0 {
			t.Errorf("a missing identity was reported as %v: %v", sentinel, got)
		}
	}
	// And the rules that own those mistakes still report them.
	if !errors.Is(o.Validate(), ErrMissingName) || !errors.Is(o.Validate(), ErrInvalidHandle) {
		t.Errorf("Validate() = %v, want the missing name and the empty handle reported", o.Validate())
	}
}

// NAMES ARE COMPARED EXACTLY, the way a reference resolves them.
//
// Organization.Role and Organization.Unit match the exact string, so "Dev"
// and "dev" are two different references there; treating them as one here
// would refuse a document whose references resolve unambiguously. Their
// handles still collide, and that rule still says so.
func TestNamesAreComparedExactlyAsReferencesResolveThem(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform", Roles: []*Role{{Name: "Dev"}}},
		{Name: "platform", Roles: []*Role{{Name: "dev", DeclaredHandle: "dev-two"}}},
	}})
	if err := o.ValidateAdmission(); err != nil {
		t.Errorf("ValidateAdmission() = %v, want nil for names differing in case", err)
	}
}

// A ROOT SEAT MOVED INTO ITS UNIT IS ONE SEAT, counted where it now sits.
func TestAMovedRootSeatIsCountedOnce(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "Dev", UnitRef: "Core"}},
		Units: []*Unit{{Name: "Core", Roles: []*Role{{Name: "Lead"}}}},
	})
	if err := errors.Join(o.Validate(), o.ValidateAdmission()); err != nil {
		t.Errorf("a moved root seat was reported: %v", err)
	}
}

// A UNIT REFERENCE PLACES ONLY A ROOT SEAT. On a seat declared inside a unit it
// moves nothing, so one naming a different unit reads as a placement and does
// nothing, which is refused on admission. A reference that repeats the seat's
// own unit, and a root seat's reference that placed it, are fine; a root
// seat's reference naming nothing is a dangling reference, not this rule.
func TestAUnitReferenceOnANestedSeatMustNameItsUnit(t *testing.T) {
	t.Parallel()
	stray := &Role{Name: "Stray", UnitRef: "Product"}
	o := normalized(&Organization{Name: "T",
		Roles: []*Role{
			{Name: "Placed", UnitRef: "Engineering"},
			{Name: "Lost", UnitRef: "Nowhere"},
		},
		Units: []*Unit{
			{Name: "Engineering", Roles: []*Role{
				stray,
				{Name: "Repeats", UnitRef: "Engineering"},
			}},
			{Name: "Product"},
		},
	})

	got := violations(o.ValidateAdmission(), ErrMisplacedUnitRef)
	if len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want exactly the stray seat's reference", o.ValidateAdmission())
	}
	var seatErr *SeatError
	if !errors.As(got[0], &seatErr) || seatErr.Seat != stray || len(seatErr.Field) != 1 || seatErr.Field[0] != "unit" {
		t.Fatalf("the violation does not name the stray seat's unit field: %#v", got[0])
	}
	for _, want := range []string{`"Stray"`, `unit "Engineering"`, "unit: Product"} {
		if !strings.Contains(got[0].Error(), want) {
			t.Errorf("the message does not name %s: %v", want, got[0])
		}
	}
	// The seat stays where it was written: the reference never moved it.
	if o.UnitFor(stray) != o.Unit("Engineering") {
		t.Error("the nested seat was moved by its reference")
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil: the rule is an admission rule", err)
	}
}
