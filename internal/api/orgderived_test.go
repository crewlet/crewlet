package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
)

// The org projection carries the hierarchy the ENGINE derives, so the
// dashboard draws a chart rather than deriving one.
//
// Every rule in it is one a second implementation gets wrong: a handle is a
// slug with Go's own case mapping, a root seat carrying `unit:` moves into
// that unit, a lead and a channel cascade to child units that set none, a
// `manages` entry naming a unit stands for its seats, and a unit's lead
// manages the members nobody else manages. The client that derived these in
// TypeScript had already diverged on three of them.

// derivedCompany is written so that every one of those rules has to fire for
// the answers below to be right: the CTO is at the root with a `unit:`
// reference into Engineering, Platform inherits Engineering's lead and
// channel, and the CEO manages the Engineering unit rather than its members.
func derivedCompany() *config.Company {
	return &config.Company{
		Name: "Acme",
		// A `manages:` entry and a `unit:` reference carry a unit's KEY,
		// and a `lead:` carries a seat's HANDLE. The units declare ids
		// that differ from their names on purpose: with the two the same
		// a reference resolved either way and the fixture could not tell
		// the rules apart.
		Roles: []config.Role{
			{Name: "Chief Executive", Manages: []string{"eng"}},
			{Name: "CTO", Unit: "eng"},
		},
		Units: []config.Unit{{
			Name: "Engineering", ID: "eng", Lead: "cto", Channel: "eng",
			Roles: []config.Role{{Name: "SRE"}},
			Children: []config.Unit{{
				Name: "Platform", ID: "plat",
				Roles: []config.Role{{Name: "Platform Engineer"}},
			}},
		}},
	}
}

func orgOf(t *testing.T, a *api.App) api.OrgProjection {
	t.Helper()
	res := fetch(t, a, "/org", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /org = %d", res.StatusCode)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read /org: %v", err)
	}
	var out api.OrgProjection
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode /org: %v (%s)", err, raw)
	}
	return out
}

func seatIn(t *testing.T, derived *config.Derived, handle string) config.DerivedSeat {
	t.Helper()
	for _, seat := range derived.Seats {
		if seat.Handle == handle {
			return seat
		}
	}
	t.Fatalf("no seat %q in the derived hierarchy: %+v", handle, derived.Seats)
	return config.DerivedSeat{}
}

func unitIn(t *testing.T, derived *config.Derived, name string) config.DerivedUnit {
	t.Helper()
	for _, unit := range derived.Units {
		if unit.Name == name {
			return unit
		}
	}
	t.Fatalf("no unit %q in the derived hierarchy: %+v", name, derived.Units)
	return config.DerivedUnit{}
}

// THE PROJECTION ANSWERS THE DERIVED HIERARCHY, resolved as the engine runs it.
func TestTheOrgProjectionCarriesTheDerivedHierarchy(t *testing.T) {
	t.Parallel()
	company := derivedCompany()
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: func() *config.Company { return company }},
	})

	derived := orgOf(t, a).Derived
	if derived == nil {
		t.Fatal("GET /org carries no derived hierarchy, so a client has to derive one")
	}

	cto := seatIn(t, derived, "cto")
	if !cto.PlacedByRef || cto.Manager != "chief-executive" {
		t.Errorf("the CTO = %+v, want it placed by its unit reference and managed by the CEO", cto)
	}
	if !slices.Contains(cto.OnboardingChain, "Engineering") {
		t.Errorf("the CTO's onboarding chain = %v, want the unit it was placed in", cto.OnboardingChain)
	}
	if got := unitIn(t, derived, "Engineering"); got.Lead != "cto" ||
		got.LeadInherited || !slices.Contains(got.Seats, "cto") {
		t.Errorf("Engineering = %+v, want the CTO as its declared lead and a member", got)
	}
	// AND ITS KEY, which is what the CEO's `manages` entry beside it
	// names. Without it a client holding that entry has nothing in the
	// same response to resolve it against: it would match on the display
	// name, which is a different value here, or call the reference broken.
	if got := unitIn(t, derived, "Engineering"); got.ID != "eng" {
		t.Errorf("Engineering's key = %q, want %q — the manages entry that names "+
			"it resolves nothing otherwise", got.ID, "eng")
	}
	// The child unit sets neither, so both cascade, and the seat inside it
	// is managed by the lead it inherited.
	platform := unitIn(t, derived, "Platform")
	if platform.Lead != "cto" || !platform.LeadInherited ||
		platform.Channel != "eng" || !platform.ChannelInherited {
		t.Errorf("Platform = %+v, want the lead and channel it inherits", platform)
	}
	// The inherited lead manages the members nobody else in its unit does,
	// and the primary manager is the FIRST seat in the engine's own order
	// that manages the seat, which is the CEO through the unit it manages.
	engineer := seatIn(t, derived, "platform-engineer")
	if engineer.Manager != "chief-executive" || !slices.Contains(engineer.Managers, "cto") {
		t.Errorf("the Platform Engineer = %+v, want the CEO as primary manager and "+
			"the inherited lead among its managers", engineer)
	}
	if !slices.Contains(seatIn(t, derived, "cto").AutoReports, "platform-engineer") {
		t.Errorf("the CTO's automatic reports = %v, want the seat it leads through "+
			"the unit that inherited it", seatIn(t, derived, "cto").AutoReports)
	}
	// A manages entry naming a UNIT manages the seats in its subtree.
	if got := seatIn(t, derived, "sre").Managers; !slices.Contains(got, "chief-executive") {
		t.Errorf("the SRE's managers = %v, want the CEO, which manages its unit", got)
	}

	// WITHOUT PATHS: an anonymous reader is given no document to point
	// into, and membership is in each unit's seats.
	raw, err := json.Marshal(derived)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"path"`, `"unit_path"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("the anonymous hierarchy carries %s: %s", key, raw)
		}
	}
}

// AND A NODE WITH NO COMPANY STILL ANSWERS `{}`, which is what the dashboard
// reads as "nothing loaded": an empty hierarchy is not one.
func TestTheOrgProjectionOfAnUnconfiguredNodeStaysEmpty(t *testing.T) {
	t.Parallel()
	// Both ways a registry has no company: no source of one at all, and a
	// source that answers none (a node before its first revision).
	for name, sources := range map[string]queries.Sources{
		"no source":                {},
		"a source with no company": {Company: func() *config.Company { return nil }},
	} {
		a := newApp(t, api.Options{Sources: sources})
		res := fetch(t, a, "/org", nil)
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("%s: read /org: %v", name, err)
		}
		if strings.TrimSpace(string(raw)) != "{}" {
			t.Errorf("%s: GET /org = %s, want {}", name, raw)
		}
	}
}
