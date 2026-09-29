package org

import (
	"errors"
	"strings"
	"testing"
)

// The admission rules: a unit key is unique across the whole company, and a
// `unit:` reference is written only where it places something. They are a
// CLASS of their own, apart from Validate, because stored companies predate
// them and still run exactly as they did; the config layer refuses a
// submitted document for breaking one and applies a stored revision with a
// warning. What these pin is the rule itself, its class, and the shape of what
// it reports — and what it deliberately does NOT hold unique: a name.

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

// A NAME IS PROSE, AND TWO THINGS MAY SHARE ONE — two seats on distinct
// handles, two units on distinct ids, anywhere in the tree.
//
// That is the org chart's own rule: a name is content arbitrated on its own
// object's subject, so no chart write can refuse a second "Engineer", and a
// document rule the chart cannot hold made the chart a running company
// exported one its own import refused. Nothing references a seat or a unit by
// its name, so the pair is two addresses and nothing more.
func TestAnyNameMayBeSharedOnDistinctAddresses(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Engineering", ID: "engineering", Children: []*Unit{
			{Name: "Platform", ID: "eng-platform",
				Roles: []*Role{{Name: "Engineer", DeclaredHandle: "backend-engineer"}}},
		}},
		{Name: "Product", ID: "product", Children: []*Unit{
			{Name: "Platform", ID: "product-platform",
				Roles: []*Role{{Name: "Engineer", DeclaredHandle: "frontend-engineer"}}},
		}},
	}})

	if err := o.ValidateAdmission(); err != nil {
		t.Errorf("ValidateAdmission() = %v, want nil: a shared name is two addresses", err)
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil: a shared name is two addresses", err)
	}
	// AND EACH IS REACHED BY ITS ADDRESS: the pair is two units, keyed apart.
	if a, b := o.Unit("eng-platform"), o.Unit("product-platform"); a == nil || b == nil || a == b {
		t.Errorf("the two units named Platform are not two addresses: %p %p", a, b)
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

	// THREE UNITS KEYED ON ONE NAME, since none declares an id: one key,
	// one message, all three units and where each sits.
	units := violations(o.ValidateAdmission(), ErrDuplicateUnit)
	if len(units) != 1 || !strings.Contains(units[0].Error(), "3 units") ||
		!strings.Contains(units[0].Error(), `under unit "Edge"`) {
		t.Errorf("unit key messages = %v, want one naming all three units keyed \"Core\"", units)
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

	for _, sentinel := range []error{ErrDuplicateHandle, ErrDuplicateUnit} {
		if got := violations(errors.Join(o.Validate(), o.ValidateAdmission()), sentinel); len(got) != 0 {
			t.Errorf("a missing identity was reported as %v: %v", sentinel, got)
		}
	}
	// And the rules that own those mistakes still report them.
	if !errors.Is(o.Validate(), ErrMissingName) || !errors.Is(o.Validate(), ErrInvalidHandle) {
		t.Errorf("Validate() = %v, want the missing name and the empty handle reported", o.Validate())
	}
}

// A UNIT KEY IS FOLDED EXACTLY AS THE CHART FOLDS AN ADDRESS.
//
// The file is checked so its import is not declined, and the chart's
// [chart.NormalizeKey] lower-cases a key and turns its whitespace into a
// hyphen: `Product Team` and `product-team` are one address there, so they are
// one key here, and so are two cases of one name on units that declare no id.
// The fold is ToLower's, which is why "İstanbul" and "Istanbul" are one key.
func TestAUnitKeyIsFoldedAsTheChartFoldsAnAddress(t *testing.T) {
	t.Parallel()
	for name, units := range map[string][]*Unit{
		"whitespace against a hyphen": {{Name: "Product", ID: "Product Team"}, {Name: "Design", ID: "product-team"}},
		"two cases of one name":       {{Name: "Platform"}, {Name: "platform"}},
		"a dotted capital":            {{Name: "İstanbul"}, {Name: "Istanbul"}},
		"a name against an id":        {{Name: "Platform"}, {Name: "Product", ID: "platform"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := normalized(&Organization{Name: "T", Units: units})
			got := violations(o.ValidateAdmission(), ErrDuplicateUnit)
			if len(got) != 1 {
				t.Fatalf("ValidateAdmission() = %v, want the two units reported once",
					o.ValidateAdmission())
			}
			// EACH UNIT IS NAMED WITH THE KEY AS IT WROTE IT, which is the
			// spelling an operator changes.
			for _, u := range units {
				if want := `(key "` + u.Key() + `")`; !strings.Contains(got[0].Error(), want) {
					t.Errorf("the message does not name %s: %v", want, got[0])
				}
			}
			// STILL AN ADMISSION RULE: only a submitted document is refused.
			if err := o.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil: a duplicate unit key is an admission rule", err)
			}
		})
	}
}

// AN ID THAT SPELLS ANOTHER UNIT'S NAME COLLIDES WITH NOTHING, and a unit's id
// may repeat its own name.
//
// Only a key is compared: a unit that declares an id answers to the id alone,
// so its name is reachable by nothing and cannot be what another unit's id
// collides with.
func TestAnIDCollidesWithKeysAndNeverWithANameThatIsNotOne(t *testing.T) {
	t.Parallel()
	crossed := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform", ID: "core"},
		{Name: "Product", ID: "platform"},
	}})
	if err := crossed.ValidateAdmission(); err != nil {
		t.Errorf("ValidateAdmission() = %v, want nil: an id spelling another unit's name is no key of that unit's", err)
	}

	own := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform", ID: "Platform"},
		{Name: "Product", ID: "product"},
	}})
	if err := own.ValidateAdmission(); err != nil {
		t.Errorf("ValidateAdmission() = %v, want nil: a unit's id may repeat its own name", err)
	}

	ids := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Platform", ID: "Core"},
		{Name: "Product", ID: "core"},
	}})
	if got := violations(ids.ValidateAdmission(), ErrDuplicateUnit); len(got) != 1 {
		t.Fatalf("ValidateAdmission() = %v, want the two ids reported once", ids.ValidateAdmission())
	}
}

// EVERY SPELLING OF ONE KEY IS ONE GROUP, REPORTED IN THE FIRST OF THEM.
//
// The message is reported in the first spelling met, which is what an
// operator can search their document for, and it names every unit answering
// to the key: changing any one of three clears only its own share.
func TestEverySpellingOfOneKeyIsOneGroupReportedInTheFirst(t *testing.T) {
	t.Parallel()
	for name, units := range map[string][]*Unit{
		"an id spelled as the first name": {
			{Name: "Platform"}, {Name: "platform"}, {Name: "Product", ID: "Platform"},
		},
		"an id first": {
			{Name: "Product", ID: "platform"}, {Name: "Platform"}, {Name: "PLATFORM"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := normalized(&Organization{Name: "T", Units: units})

			got := violations(o.ValidateAdmission(), ErrDuplicateUnit)
			if len(got) != 1 {
				t.Fatalf("ValidateAdmission() = %v, want one collision", o.ValidateAdmission())
			}
			var dup *DuplicateError
			if !errors.As(got[0], &dup) {
				t.Fatalf("the violation is not a DuplicateError: %#v", got[0])
			}
			if dup.Key != units[0].Key() {
				t.Errorf("the collision is reported as %q, want the first spelling %q",
					dup.Key, units[0].Key())
			}
			if len(dup.Units) != 3 || dup.Units[0] != units[0] ||
				dup.Units[1] != units[1] || dup.Units[2] != units[2] {
				t.Errorf("the collision names %v, want all three units answering to the key",
					unitNames(dup.Units))
			}
		})
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
