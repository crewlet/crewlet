package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// A UNIT'S KEY IS ITS ID WHEN IT HAS ONE, and its name otherwise — which is
// what makes the field optional rather than a migration. A company that sets
// no ids behaves exactly as it did.
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
		"unset":                 {"", true},
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
			{Name: "Platform"},
			{Name: "Product", ID: "platform"},
		},
		"two units with one name": {
			{Name: "Platform"},
			{Name: "platform"},
		},
		// THE EXACT SAME NAME TWICE is the collision that looks least
		// like one and is caught by nothing else: Organization.Unit
		// resolves a name to the FIRST unit carrying it, so the second
		// team's work goes to the first team, silently, for ever.
		"two units with the identical name": {
			{Name: "Platform"},
			{Name: "Platform"},
		},
		// AND DEPTH IS NOT A NAMESPACE. A child unit is addressed by the
		// same key as a root one, so nesting hides nothing.
		"a child colliding with a root unit": {
			{Name: "Engineering", Children: []config.Unit{{Name: "Platform"}}},
			{Name: "Platform"},
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

// unitsCompany is a valid company carrying the units a case is about, so a
// refusal here is about the unit rather than about every other default.
func unitsCompany(units ...config.Unit) *config.Company {
	c := config.DefaultCompany()
	c.Name = "Acme"
	c.Units = units
	return &c
}
