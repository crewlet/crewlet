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
		Name:  "Acme",
		Roles: []config.Role{{Name: "Chief Executive", Manages: []string{"Engineering"}}, {Name: "CTO", Unit: "Engineering"}},
		Units: []config.Unit{{
			Name: "Engineering", Lead: "CTO", Channel: "eng",
			Roles:    []config.Role{{Name: "SRE"}},
			Children: []config.Unit{{Name: "Platform", Roles: []config.Role{{Name: "Platform Engineer"}}}},
		}},
	}
}

// chartPosture is a node's Tier A for the chart's cases: the public anonymous
// posture, one admin key ("secret") and one member key ("member-secret").
func chartPosture() *config.Bootstrap {
	b := closedPosture()
	b.API.Auth.Anonymous = config.AnonymousPublic
	b.API.Auth.Tokens = append(b.API.Auth.Tokens,
		config.APIToken{ID: "ada", Role: config.RoleMember, Token: "member-secret"})
	return &b
}

// chartApp is a node over company on [chartPosture].
func chartApp(t *testing.T, company *config.Company) *api.App {
	t.Helper()
	return newApp(t, api.Options{
		Bootstrap: chartPosture(),
		Sources:   queries.Sources{Company: func() *config.Company { return company }},
	})
}

// orgOf is GET /org as the caller presenting key ("" presents none) reads it.
func orgOf(t *testing.T, a *api.App, key string) api.OrgProjection {
	t.Helper()
	headers := map[string]string{}
	if key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	res := fetch(t, a, "/org", headers)
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

func seatIn(t *testing.T, derived *api.OrgDerived, handle string) api.OrgDerivedSeat {
	t.Helper()
	for _, seat := range derived.Seats {
		if seat.Handle == handle {
			return seat
		}
	}
	t.Fatalf("no seat %q in the derived hierarchy: %+v", handle, derived.Seats)
	return api.OrgDerivedSeat{}
}

func unitIn(t *testing.T, derived *api.OrgDerived, name string) api.OrgDerivedUnit {
	t.Helper()
	for _, unit := range derived.Units {
		if unit.Name == name {
			return unit
		}
	}
	t.Fatalf("no unit %q in the derived hierarchy: %+v", name, derived.Units)
	return api.OrgDerivedUnit{}
}

// THE PROJECTION ANSWERS THE DERIVED HIERARCHY, resolved as the engine runs it
// — to a member, who reads a unit's channel; the chart's anonymous reader is
// served the same hierarchy without it.
func TestTheOrgProjectionCarriesTheDerivedHierarchy(t *testing.T) {
	t.Parallel()
	company := derivedCompany()
	a := chartApp(t, company)

	if public := orgOf(t, a, "").Derived; public == nil ||
		unitIn(t, public, "Platform").Channel != "" || unitIn(t, public, "Platform").Lead != "cto" {
		t.Errorf("the anonymous hierarchy = %+v, want Platform's inherited lead and no channel", public)
	}

	derived := orgOf(t, a, "member-secret").Derived
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

	// WITHOUT PATHS: a reader of the chart is given no document to point
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
