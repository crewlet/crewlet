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

// A UNIT NAME IS FOLDED AND A SEAT NAME IS NOT, because a unit's name is its
// key and a seat's name is not.
//
// Admission folds a unit's name because a name is prose: "Platform" and
// "platform" are one team to every reader, and Unit.Key files work, routing
// and pages under whichever spelling a document happened to be written with.
// That Organization.Unit resolves the name EXACTLY is what makes the mistake
// QUIET rather than what makes it safe, so the pair is refused, naming both
// and where each one sits.
//
// A seat's key is its HANDLE, held unique by a runnable rule, so nothing is
// ever filed under a seat's name: "Dev" and "dev" are two seats wherever they
// declare two handles. Declaring none, they derive ONE, and the handle rule
// reports that instead, which is why the seat rule needs no fold of its own.
func TestAUnitNameIsFoldedAndASeatNameIsNot(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform", Roles: []*Role{{Name: "Dev"}}},
		{Name: "platform", Roles: []*Role{{Name: "dev", DeclaredHandle: "dev-two"}}},
	}})

	got := violations(o.ValidateAdmission(), ErrDuplicateUnit)
	if len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want the two spellings reported once", o.ValidateAdmission())
	}
	// A NAME IS THE KEY IT DUPLICATES, so the one error carries both
	// sentinels and sends the operator to rename one of the two teams.
	if !errors.Is(got[0], ErrDuplicateUnitName) {
		t.Errorf("two units of one name were not reported as a duplicate name: %v", got[0])
	}
	for _, want := range []string{`"Platform"`, `unit "Platform" at the top level`,
		`unit "platform" at the top level`} {
		if !strings.Contains(got[0].Error(), want) {
			t.Errorf("the message does not name %s: %v", want, got[0])
		}
	}
	// STILL AN ADMISSION RULE. A stored company carrying the pair runs as
	// it always did, so only a submitted document is refused for it.
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil: a duplicate unit name is an admission rule", err)
	}
	// AND THE SEATS INSIDE THOSE UNITS ARE FINE: two names differing in
	// case, two handles, nothing filed under either name.
	if seats := violations(o.ValidateAdmission(), ErrDuplicateSeatName); len(seats) != 0 {
		t.Errorf("seat names differing in case were reported: %v", seats)
	}
	if handles := violations(o.Validate(), ErrDuplicateHandle); len(handles) != 0 {
		t.Errorf("seats declaring two handles were reported as sharing one: %v", handles)
	}

	// DECLARING NO HANDLE, that same pair derives one, and the handle rule
	// refuses it as a RUNNABLE rule: the collision a fold would have caught
	// on the seat side is already covered, and covered more strictly.
	derived := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Core", Roles: []*Role{{Name: "Dev"}, {Name: "dev"}}},
	}})
	if seats := violations(derived.ValidateAdmission(), ErrDuplicateSeatName); len(seats) != 0 {
		t.Errorf("seat names differing in case were reported: %v", seats)
	}
	if handles := violations(derived.Validate(), ErrDuplicateHandle); len(handles) != 1 {
		t.Fatalf("Validate() = %v, want the one handle both seats derive", derived.Validate())
	}
}

// AN ID IS COMPARED FOLDED, against names and other ids alike.
//
// An id is the other vocabulary: a lowercase key by rule, where a name is
// prose. Comparing the two exactly would let `id: platform` sit beside
// `name: Platform` unreported, and [Unit.Key] then files one team's work
// under the key the other answers to. It is a duplicate KEY and not a
// duplicate name: nothing here is named twice, and saying so would send an
// operator to rename a team that is named once.
func TestAnIDIsComparedFoldedAgainstAName(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform", Roles: []*Role{{Name: "Dev"}}},
		{Name: "Product", ID: "platform", Roles: []*Role{{Name: "Ops"}}},
	}})

	got := violations(o.ValidateAdmission(), ErrDuplicateUnit)
	if len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want one duplicate unit key", o.ValidateAdmission())
	}
	if errors.Is(got[0], ErrDuplicateUnitName) {
		t.Errorf("an id collision was reported as a duplicate name: %v", got[0])
	}
	for _, want := range []string{`"Platform"`, `unit "Product"`, `(id "platform")`} {
		if !strings.Contains(got[0].Error(), want) {
			t.Errorf("the message does not name %s: %v", want, got[0])
		}
	}
}

// TWO IDS ARE ONE KEY WHEN THEY DIFFER ONLY IN CASE, and a unit whose id
// repeats its own name answers to that key alone.
func TestIDsCollideWithEachOtherAndNeverWithTheirOwnUnit(t *testing.T) {
	t.Parallel()
	folded := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform", ID: "Core"},
		{Name: "Product", ID: "core"},
	}})
	if got := violations(folded.ValidateAdmission(), ErrDuplicateUnit); len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want the two ids reported once", folded.ValidateAdmission())
	}

	own := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform", ID: "Platform"},
		{Name: "Product", ID: "product"},
	}})
	if err := own.ValidateAdmission(); err != nil {
		t.Errorf("ValidateAdmission() = %v, want nil: a unit's id may repeat its own name", err)
	}
}

// EVERY SPELLING OF ONE KEY IS ONE GROUP, REPORTED IN THE FIRST OF THEM.
//
// Both cases of a name and an id folding onto them are one key and one
// mistake, so they are one message naming all three units: every pair among
// them answers to the key, and changing any one of the three clears only its
// own share. The message is reported in the first spelling met, which is what
// an operator can search their document for, and the id's own spelling never
// reopens the key: `id: Platform` and `id: platform` join the same group as
// each other and as every unit named either way.
func TestEverySpellingOfOneKeyIsOneGroupReportedInTheFirst(t *testing.T) {
	t.Parallel()
	for name, units := range map[string][]*Unit{
		"an id spelled as the first name": {
			{Name: "Platform"}, {Name: "platform"}, {Name: "Product", ID: "Platform"},
		},
		"an id spelled as the second name": {
			{Name: "platform"}, {Name: "Platform"}, {Name: "Product", ID: "Platform"},
		},
		"an id folding onto two names": {
			{Name: "Platform"}, {Name: "PLATFORM"}, {Name: "Product", ID: "platform"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := normalized(&Organization{Name: "T", Units: units})

			got := violations(o.ValidateAdmission(), ErrDuplicateUnit)
			if len(got) != 1 {
				t.Fatalf("ValidateAdmission() = %v, want one collision", o.ValidateAdmission())
			}
			var dup *DuplicateError
			if !errors.As(got[0], &dup) {
				t.Fatalf("the violation is not a DuplicateError: %#v", got[0])
			}
			if dup.Key != units[0].Name {
				t.Errorf("the collision is reported as %q, want the first spelling %q",
					dup.Key, units[0].Name)
			}
			if len(dup.Units) != 3 || dup.Units[0] != units[0] ||
				dup.Units[1] != units[1] || dup.Units[2] != units[2] {
				t.Errorf("the collision names %v, want all three units answering to the key",
					unitNames(dup.Units))
			}
		})
	}
}

// WHICH UNITS CARRY THE NAME IS DECIDED UNDER THE SAME FOLD, and that decides
// both the second sentinel and what the message tells the operator to change.
//
// A key TWO units are named for is a duplicate NAME as well, in whichever case
// each of them is written: the way out is to rename a team, and the error says
// so. A key only one unit is named for arrived through an id, so it carries
// ErrDuplicateUnit alone and the message names the id that joined the group,
// rather than sending somebody to rename a team that is named once. Measuring
// a member's name against the key EXACTLY gets both halves wrong: the pair
// above would lose its name sentinel, and the unit spelled unlike the key
// would be described as answering by an id it never declared.
func TestWhichUnitsCarryTheNameIsDecidedUnderTheSameFold(t *testing.T) {
	t.Parallel()
	named := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform"},
		{Name: "platform"},
		{Name: "Product", ID: "platform"},
	}})

	got := violations(named.ValidateAdmission(), ErrDuplicateUnit)
	if len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want one duplicate unit key", named.ValidateAdmission())
	}
	if !errors.Is(got[0], ErrDuplicateUnitName) {
		t.Errorf("a key two units are named for was not reported as a duplicate name: %v", got[0])
	}
	if !strings.Contains(got[0].Error(), `unit "Product" at the top level (id "platform")`) ||
		strings.Contains(got[0].Error(), `unit "platform" at the top level (id`) {
		t.Errorf("the message does not name the id against the one unit that joined by it: %v", got[0])
	}

	byID := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "platform"},
		{Name: "Product", ID: "platform"},
	}})

	got = violations(byID.ValidateAdmission(), ErrDuplicateUnit)
	if len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want one duplicate unit key", byID.ValidateAdmission())
	}
	if errors.Is(got[0], ErrDuplicateUnitName) {
		t.Errorf("a key carried by an id was reported as a duplicate name: %v", got[0])
	}
	if !strings.Contains(got[0].Error(), `unit "Product" at the top level (id "platform")`) {
		t.Errorf("the message does not say which id joined the group: %v", got[0])
	}

	// AND THE FOLD IS foldUnitKey, NOT [strings.EqualFold]. The two agree on
	// every case a person types by hand, which is what makes the wrong one
	// read as correct, and they part over characters that are real in a team
	// name: EqualFold folds by [unicode.SimpleFold], where "İstanbul" and
	// "Istanbul" are two keys, while the claim folds by [unicode.ToLower],
	// where they are one. Claimed as one key and asked about as two, the pair
	// loses the name sentinel and the unit written second is described as
	// answering by an id it never declared.
	divergent := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "İstanbul"},
		{Name: "Istanbul"},
	}})

	got = violations(divergent.ValidateAdmission(), ErrDuplicateUnit)
	if len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want the two spellings reported once",
			divergent.ValidateAdmission())
	}
	if !errors.Is(got[0], ErrDuplicateUnitName) {
		t.Errorf("two units named for one key were not reported as a duplicate name: %v", got[0])
	}
	if strings.Contains(got[0].Error(), `(id `) {
		t.Errorf("a unit named for the key was described as answering by an id: %v", got[0])
	}
	for _, want := range []string{`unit "İstanbul" at the top level`, `unit "Istanbul" at the top level`} {
		if !strings.Contains(got[0].Error(), want) {
			t.Errorf("the message does not name %s: %v", want, got[0])
		}
	}
}

// unitNames renders units for a failure message.
func unitNames(units []*Unit) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = u.Name
	}
	return out
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
