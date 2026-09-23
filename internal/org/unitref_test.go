package org

import (
	"errors"
	"testing"
)

// A DURABLE UNIT REFERENCE RESOLVES BY EITHER SPELLING. What a row holds is
// [Unit.Key] — the id where the unit has one, the name where it does not —
// and a company that adds an id keeps every row written before it, so both
// spellings have to reach the same unit or half the company's work stops
// answering the filter that used to find it.
func TestUnitByRefMatchesAnIDOrAName(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Engineering", ID: "eng", Roles: []*Role{{Name: "Dev"}}},
		{Name: "Product Marketing", Roles: []*Role{{Name: "PMM"}}},
	}})
	cases := []struct {
		name string
		ref  string
		want string // the unit's Name, or "" for no unit at all
	}{
		{"its id", "eng", "Engineering"},
		{"its name", "Engineering", "Engineering"},
		{"its id in another case", "ENG", "Engineering"},
		{"its name in another case", "engineering", "Engineering"},
		{"a name with space around it", "  Engineering  ", "Engineering"},
		{"a unit with no id, by name", "Product Marketing", "Product Marketing"},
		{"a unit with no id, folded", "product marketing", "Product Marketing"},
		{"a reference naming nothing", "Design", ""},
		{"the empty reference", "", ""},
		{"a blank reference", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := o.UnitByRef(tc.ref)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("UnitByRef(%q) = %q, want no unit", tc.ref, got.Name)
			case tc.want == "":
			case got == nil:
				t.Fatalf("UnitByRef(%q) = nil, want %q", tc.ref, tc.want)
			case got.Name != tc.want:
				t.Errorf("UnitByRef(%q) = %q, want %q", tc.ref, got.Name, tc.want)
			}
		})
	}
}

// AN ID WINS AN AMBIGUITY, whichever order the two units sit in. The pair is
// refused on any document submitted since the duplicate-key rule existed, so
// what this settles is what a STORED revision carrying one resolves to — and
// it must not be walk order: an id is the spelling that cannot move, and the
// unit answering by name is the one that can.
func TestUnitByRefPrefersAnIDOverAnotherUnitsName(t *testing.T) {
	t.Parallel()
	// Rebuilt per case, because the order is the case and Normalize
	// mutates what it is handed.
	pair := func(identifiedFirst bool) []*Unit {
		named := &Unit{Name: "platform", Roles: []*Role{{Name: "A"}}}
		identified := &Unit{
			Name: "Platform Services", ID: "platform",
			Roles: []*Role{{Name: "B"}},
		}
		if identifiedFirst {
			return []*Unit{identified, named}
		}
		return []*Unit{named, identified}
	}
	for _, tc := range []struct {
		name  string
		first bool
	}{
		{"the named unit first", false},
		{"the identified unit first", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := &Organization{Name: "T", Units: pair(tc.first)}
			got := o.UnitByRef("platform")
			if got == nil || got.Name != "Platform Services" {
				t.Fatalf("UnitByRef(platform) = %v, want the unit whose id it is", got)
			}
			// The unit that lost is still reachable by its own name,
			// which is the whole of what it still answers to.
			if other := o.UnitByRef("Platform Services"); other != got {
				t.Errorf("UnitByRef(Platform Services) = %v, want %v", other, got)
			}
		})
	}
	// And the collision itself is an admission rule, so a document
	// somebody submits is refused for it rather than left to the
	// precedence above.
	o := normalized(&Organization{Name: "T", Units: pair(false)})
	if err := o.ValidateAdmission(); !errors.Is(err, ErrDuplicateUnit) {
		t.Errorf("ValidateAdmission() = %v, want %v", err, ErrDuplicateUnit)
	}
}

// THE FOLD IS THE ADMISSION RULE'S OWN FOLD. [strings.EqualFold] is a
// different one over characters that are real in a team name, so a resolver
// using it would refuse to resolve a reference to a unit the chart holds —
// and would disagree with the rule that decided the pair was one team.
func TestUnitByRefFoldsAsTheDuplicateKeyRuleDoes(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "İstanbul", Roles: []*Role{{Name: "Dev"}}},
	}})
	// ToLower("İstanbul") is "istanbul", so these are one key to the
	// admission rule; EqualFold calls the pair different.
	if got := o.UnitByRef("Istanbul"); got == nil {
		t.Error("UnitByRef did not fold a name the duplicate-key rule folds")
	}
}

// THE DOCUMENT'S OWN RESOLVER STAYS EXACT. A `lead:`, a `manages:` entry and
// a root seat's `unit:` are admitted and reported under the exact string, so
// widening [Organization.Unit] would change which references dangle.
func TestUnitResolvesADocumentReferenceExactly(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{
		{Name: "Engineering", ID: "eng", Roles: []*Role{{Name: "Dev"}}},
	}})
	if o.Unit("engineering") != nil {
		t.Error("Unit() matched a name in another case")
	}
	if o.Unit("eng") != nil {
		t.Error("Unit() matched a unit's id, which no document reference names")
	}
	if o.Unit("Engineering") == nil {
		t.Error("Unit() did not match the name as written")
	}
}
