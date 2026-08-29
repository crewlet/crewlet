package notify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// company is the fixture every registry test resolves against: two agent
// seats and one human, the shape a real company has and the shape the
// agent/human distinction only shows up in.
func company() *org.Organization {
	return &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{Name: "Engineering Lead", Email: "Lead@Example.com"},
			{Name: "Backend Engineer"},
			{
				Name: "Dana Founder", Kind: org.KindHuman,
				Email:   "dana@example.com",
				Contact: &org.HumanContact{SlackUserID: "U0FOUNDER", GitHubLogin: "DanaF"},
			},
		},
	}
}

func registry(t *testing.T) *notify.Registry {
	t.Helper()
	o := company()
	o.Normalize()
	return notify.NewRegistry(o)
}

func TestASeatResolvesByEveryAddressItHas(t *testing.T) {
	r := registry(t)

	lead, ok := r.ByHandle("engineering-lead")
	if !ok {
		t.Fatal("the lead did not resolve by handle")
	}
	if lead.Name != "Engineering Lead" || lead.Human {
		t.Fatalf("resolved the wrong party: %+v", lead)
	}
	if lead.AgentID.String() == "00000000-0000-0000-0000-000000000000" {
		t.Fatal("an agent seat resolved with no derived id")
	}

	byRole, ok := r.ByRole("Engineering Lead")
	if !ok || byRole.Handle != lead.Handle {
		t.Fatalf("role name resolved to %+v, want the lead", byRole)
	}
	byID, ok := r.ByAgentID(lead.AgentID)
	if !ok || byID.Handle != lead.Handle {
		t.Fatalf("agent id resolved to %+v, want the lead", byID)
	}
	// The declared address, case-folded — a vendor hands back whatever
	// the sender typed.
	byEmail, ok := r.ByEmail("lead@EXAMPLE.com")
	if !ok || byEmail.Handle != lead.Handle {
		t.Fatalf("email resolved to %+v, want the lead", byEmail)
	}
}

// The id is DERIVED, so a second process building its own registry from the
// same org agrees — which is what lets one node address a seat another runs.
func TestTheDerivedIDMatchesTheOrgsOwnDerivation(t *testing.T) {
	r := registry(t)
	lead, _ := r.ByHandle("engineering-lead")

	want, ok := org.DeriveAgentID("nimbus", "engineering-lead")
	if !ok {
		t.Fatal("the org could not derive the id")
	}
	if lead.AgentID != want {
		t.Fatalf("the registry derived %s, the org derives %s", lead.AgentID, want)
	}
}

// A plus-address names a seat DIRECTLY, whatever address that seat itself
// declares — which is the whole reason it is tried first.
func TestAPlusAddressNamesTheSeatItSpells(t *testing.T) {
	r := registry(t)

	p, ok := r.ByEmail("notif+backend-engineer@anything.example.com")
	if !ok {
		t.Fatal("a plus-address did not resolve")
	}
	if p.Handle != "backend-engineer" {
		t.Fatalf("resolved %q, want backend-engineer", p.Handle)
	}
	// And the ORDER is only observable when both indexes match the same
	// string, which needs a seat whose declared address is itself a
	// plus-address naming somebody else. That is a misconfiguration — but
	// it is the one the ordering rule exists to resolve, and the mail
	// system that delivered the message already routed it by the plus. The
	// registry has to agree with the transport that handed it over, or a
	// reply goes to a seat that never received anything.
	o := company()
	o.Roles[0].Email = "notif+backend-engineer@example.com"
	o.Normalize()
	contested := notify.NewRegistry(o)

	p, ok = contested.ByEmail("notif+backend-engineer@example.com")
	if !ok {
		t.Fatal("the contested address resolved to nobody")
	}
	if p.Handle != "backend-engineer" {
		t.Fatalf("the declared address beat the plus-address: %q", p.Handle)
	}
}

// A human seat is addressable and has NO agent id. Both halves matter: the
// first is why humans are indexed at all, the second is what stops a router
// treating a person as a seat it can wake.
func TestAHumanSeatIsAddressableAndHasNoAgentID(t *testing.T) {
	r := registry(t)

	dana, ok := r.ByHandle("dana-founder")
	if !ok {
		t.Fatal("the human seat did not resolve")
	}
	if !dana.Human {
		t.Fatal("the human seat resolved as an agent")
	}
	if dana.AgentID.String() != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("a human seat carries an agent id: %s", dana.AgentID)
	}
	if _, ok := r.ByAgentID(dana.AgentID); ok {
		t.Fatal("the zero agent id resolved to somebody")
	}
	if !strings.Contains(dana.Label(), "human colleague") {
		t.Fatalf("a human renders as %q", dana.Label())
	}
}

// Every seat, in org order — an enumeration a caller can build a roster
// from without re-deriving the party mapping.
func TestAllEnumeratesBothKindsInOrgOrder(t *testing.T) {
	r := registry(t)

	var handles []string
	for p := range r.All() {
		handles = append(handles, p.Handle)
	}
	want := []string{"engineering-lead", "backend-engineer", "dana-founder"}
	if !slices.Equal(handles, want) {
		t.Fatalf("enumerated %v, want %v", handles, want)
	}
	if r.Len() != len(want) {
		t.Fatalf("Len is %d, want %d", r.Len(), len(want))
	}
}

// A node with no company answers "nobody matches" rather than panicking:
// `crewlet validate` and a node that has not applied a revision both run
// this way.
func TestAnEmptyRegistryAnswersNobody(t *testing.T) {
	r := notify.NewRegistry(nil)

	if _, ok := r.ByHandle("engineering-lead"); ok {
		t.Fatal("an empty registry resolved a handle")
	}
	if _, ok := r.ByExternalID("slack", "U123"); ok {
		t.Fatal("an empty registry resolved an external id")
	}
	if r.Len() != 0 || r.OrgName() != "" {
		t.Fatalf("an empty registry has %d parties for %q", r.Len(), r.OrgName())
	}
	// And registering into it is refused, rather than accepted and inert.
	if err := r.Register("slack", "U123", "engineering-lead"); err == nil {
		t.Fatal("registering against a company-less registry was accepted")
	}
}

// Validation rejects a duplicate handle, so this is a hand-built org — but
// FIRST WINS regardless, because a later seat displacing an earlier one
// would silently re-point an address other rows already reference.
func TestADuplicateHandleDoesNotDisplaceTheFirstSeat(t *testing.T) {
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{Name: "Engineering Lead", Email: "first@example.com"},
			{Name: "engineering lead", Email: "second@example.com"},
		},
	}
	o.Normalize()
	r := notify.NewRegistry(o)

	p, ok := r.ByHandle("engineering-lead")
	if !ok {
		t.Fatal("the handle did not resolve at all")
	}
	if p.Name != "Engineering Lead" {
		t.Fatalf("the second seat displaced the first: %+v", p)
	}
	if r.Len() != 1 {
		t.Fatalf("both seats were enumerated: %d parties", r.Len())
	}
	if _, ok := r.ByEmail("second@example.com"); ok {
		t.Fatal("the displaced seat's address resolves to the surviving seat")
	}
	if q, ok := r.ByEmail("first@example.com"); !ok || q.Name != "Engineering Lead" {
		t.Fatalf("the surviving seat lost its address: %+v", q)
	}
}

// A seat with no derivable handle cannot be addressed, named in a topic or
// given an id. It must not enter the index under an empty key, where it
// would answer for every other unaddressable seat.
func TestASeatWithNoHandleIsNotIndexed(t *testing.T) {
	o := &org.Organization{
		Name:  "nimbus",
		Roles: []*org.Role{{Name: "   "}, {Name: "Backend Engineer"}},
	}
	o.Normalize()
	r := notify.NewRegistry(o)

	if _, ok := r.ByHandle(""); ok {
		t.Fatal("an empty handle resolved to a seat")
	}
	if r.Len() != 1 {
		t.Fatalf("%d parties indexed, want only the addressable one", r.Len())
	}
}

func TestSeatEmailRefusesWhatCannotRoundTrip(t *testing.T) {
	got, err := notify.SeatEmail("backend-engineer", "example.com", "notif")
	if err != nil || got != "notif+backend-engineer@example.com" {
		t.Fatalf("SeatEmail = %q, %v", got, err)
	}
	if back := notify.PlusAddress(got); back != "backend-engineer" {
		t.Fatalf("the address did not round-trip: %q", back)
	}
	// An invalid handle would produce an address that bounces or names a
	// different seat once PlusAddress lowercases it.
	for _, bad := range []string{"Backend Engineer", "back+end", "", "-leading"} {
		if _, err := notify.SeatEmail(bad, "example.com", "notif"); err == nil {
			t.Fatalf("SeatEmail accepted the handle %q", bad)
		}
	}
	if _, err := notify.SeatEmail("backend-engineer", "  ", "notif"); err == nil {
		t.Fatal("SeatEmail accepted an empty domain")
	}
}

func TestPlusAddressIgnoresWhatIsNotOne(t *testing.T) {
	for _, in := range []string{"alice@example.com", "", "no-at-sign", "+@x.com"} {
		if got := notify.PlusAddress(in); got != "" {
			t.Fatalf("PlusAddress(%q) = %q, want empty", in, got)
		}
	}
}
