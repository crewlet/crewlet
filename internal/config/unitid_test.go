package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// A UNIT'S KEY IS ITS ID WHEN IT HAS ONE, and its name otherwise. An
// authored document always carries an id (one is minted at import), so the
// fallback is what keeps a tree built in Go, and a revision stored before the
// field existed, addressable at all.
func TestAUnitsKeyIsItsIdentityOrItsName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		unit org.Unit
		want string
	}{
		"an id wins":            {org.Unit{Name: "Platform", ID: "plat"}, "plat"},
		"no id falls back":      {org.Unit{Name: "Platform"}, "Platform"},
		"whitespace is trimmed": {org.Unit{Name: " Platform ", ID: "  "}, "Platform"},
	} {
		t.Run(name, func(t *testing.T) {
			u := tc.unit
			if got := u.Key(); got != tc.want {
				t.Errorf("Key() = %q, want %q", got, tc.want)
			}
		})
	}
}

// AN ID IS A KEY, SO ITS SHAPE IS NARROW. It appears in durable rows, in
// filter arguments a model types and in a subject token path, so what it may
// contain is the intersection of what all three carry safely — not what a
// name may contain.
func TestAUnitIDIsShapedLikeAKey(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		id     string
		accept bool
	}{
		"lowercase":             {"platform", true},
		"with a hyphen":         {"platform-core", true},
		"with an underscore":    {"platform_core", true},
		"with digits":           {"team2", true},
		"starting with a digit": {"2team", false},
		"upper case":            {"Platform", false},
		"with a space":          {"platform core", false},
		"with a dot":            {"platform.core", false},
		// REQUIRED, because a unit is referenced by its key: a `manages:`
		// entry and a seat's `unit:` both resolve one. A document read
		// through the parser never reaches this — every unit is minted a
		// key — so what it refuses is a unit assembled in Go with neither.
		"unset": {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			c := unitsCompany(config.Unit{Name: "Platform", ID: tc.id})
			err := c.Validate()
			refused := err != nil && strings.Contains(err.Error(), "units[0].id")
			if tc.accept && refused {
				t.Fatalf("%q was refused: %v", tc.id, err)
			}
			if !tc.accept && !refused {
				t.Fatalf("%q was accepted", tc.id)
			}
		})
	}
}

// TWO UNITS MAY NOT ANSWER TO ONE KEY, and an id colliding with another
// unit's NAME is the same collision arriving by a door nobody watches.
//
// A unit's key is what work, routing and pages are filed under, so a
// collision sends one team's work to whichever unit a reader resolved first.
func TestTwoUnitsCannotShareAKey(t *testing.T) {
	t.Parallel()
	for name, units := range map[string][]config.Unit{
		"two identical ids": {
			{Name: "Platform", ID: "core"},
			{Name: "Product", ID: "core"},
		},
		"an id equal to another unit's name": {
			{Name: "Platform", ID: "plat"},
			{Name: "Product", ID: "platform"},
		},
		"two units with one name": {
			{Name: "Platform", ID: "platform-a"},
			{Name: "platform", ID: "platform-b"},
		},
		// THE EXACT SAME NAME TWICE is the collision that looks least
		// like one and is caught by nothing else: Organization.Unit
		// resolves a name to the FIRST unit carrying it, so the second
		// team's work goes to the first team, silently, for ever.
		"two units with the identical name": {
			{Name: "Platform", ID: "plat-a"},
			{Name: "Platform", ID: "plat-b"},
		},
		// AND DEPTH IS NOT A NAMESPACE. A child unit is addressed by the
		// same key as a root one, so nesting hides nothing.
		"a child colliding with a root unit": {
			{Name: "Engineering", ID: "eng", Children: []config.Unit{{Name: "Platform", ID: "plat-a"}}},
			{Name: "Platform", ID: "plat-b"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := unitsCompany(units...)
			err := c.Validate()
			if err == nil {
				t.Fatal("two units answering to one key were accepted")
			}
			if !errors.Is(err, org.ErrDuplicateUnit) {
				t.Errorf("error = %v, want ErrDuplicateUnit", err)
			}
		})
	}

	// AND DISTINCT KEYS ARE FINE, including a unit whose id differs from
	// its own name — which is the ordinary case the field exists for.
	c := unitsCompany(
		config.Unit{Name: "Platform", ID: "core"},
		config.Unit{Name: "Product", ID: "prod"},
	)
	if err := c.Validate(); err != nil {
		t.Fatalf("distinct keys were refused: %v", err)
	}
}

// AN ID IS MINTED FROM THE NAME AT IMPORT, so a founder never writes one and
// every document the engine reads has a key that a rename cannot move. It is
// derived rather than random, so re-importing the same file mints the same
// key rather than a second identity for one team.
func TestAUnitWithNoIDIsMintedOneAtImport(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		unitName string
		want     string
	}{
		"a plain name":            {"Platform", "platform"},
		"words and punctuation":   {"R&D / Tooling", "r-d-tooling"},
		"a name starting a digit": {"2nd Line", "unit-2nd-line"},
		"a name that slugs empty": {"!!!", "unit"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := config.MintUnitID(tc.unitName); got != tc.want {
				t.Errorf("MintUnitID(%q) = %q, want %q", tc.unitName, got, tc.want)
			}
		})
	}

	// Through the parser, at any depth, and only where the document
	// declares none.
	cfg, err := config.ParseCompany([]byte(`
name: Acme
units:
  - name: Engineering
    children:
      - name: Platform Squad
      - name: Storage
        id: stor
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	eng := cfg.Units[0]
	if eng.ID != "engineering" {
		t.Errorf("top-level unit id = %q, want engineering", eng.ID)
	}
	if got := eng.Children[0].ID; got != "platform-squad" {
		t.Errorf("child unit id = %q, want platform-squad", got)
	}
	if got := eng.Children[1].ID; got != "stor" {
		t.Errorf("a declared id was overwritten: %q", got)
	}
}

// unitsCompany is a valid company carrying the units a case is about, so a
// refusal here is about the unit rather than about every other default.
func unitsCompany(units ...config.Unit) *config.Company {
	c := config.DefaultCompany()
	c.Name = "Acme"
	c.Units = units
	return &c
}
