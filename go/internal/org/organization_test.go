package org

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// normalized builds and wires an org in one step, which is the only state
// the rest of the engine ever sees one in.
func normalized(o *Organization) *Organization {
	o.Normalize()
	return o
}

func roleNames(seats []*Role) []string {
	out := make([]string, 0, len(seats))
	for _, r := range seats {
		out = append(out, r.Name)
	}
	return out
}

func sortedManages(t *testing.T, o *Organization, name string) []string {
	t.Helper()
	r := o.Role(name)
	if r == nil {
		t.Fatalf("no seat named %q", name)
	}
	out := slices.Clone(r.Manages)
	slices.Sort(out)
	return out
}

// ---- lead inheritance ------------------------------------------------ //

func TestLeadCascadesThroughEveryLevel(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "BigCorp",
		Units: []*OrgUnit{{
			Name: "Technology", Type: UnitTypeDivision, Lead: "CTO",
			SlackChannel: "C_TECH",
			Roles:        []*Role{{Name: "CTO"}},
			Children: []*OrgUnit{{
				Name: "Engineering", Type: UnitTypeDepartment,
				Children: []*OrgUnit{{
					Name:  "Backend",
					Roles: []*Role{{Name: "Dev"}},
				}},
			}},
		}},
	})
	for _, unit := range []string{"Engineering", "Backend"} {
		u := o.Unit(unit)
		if u == nil {
			t.Fatalf("no unit named %q", unit)
		}
		if u.Lead != "CTO" {
			t.Errorf("unit %q lead = %q, want CTO", unit, u.Lead)
		}
		// The team channel rides the same cascade: a child that names none
		// talks where its parent talks.
		if u.SlackChannel != "C_TECH" {
			t.Errorf("unit %q channel = %q, want C_TECH", unit, u.SlackChannel)
		}
	}
}

func TestExplicitLeadIsNeverOverwritten(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*OrgUnit{{
			Name: "Engineering", Lead: "VP Eng", Roles: []*Role{{Name: "VP Eng"}},
			Children: []*OrgUnit{
				{Name: "Backend", Lead: "Tech Lead", Roles: []*Role{{Name: "Tech Lead"}, {Name: "Dev"}}},
				{Name: "Frontend", Roles: []*Role{{Name: "Dev C"}}},
			},
		}},
	})
	if got := o.Unit("Backend").Lead; got != "Tech Lead" {
		t.Errorf("Backend lead = %q, want Tech Lead", got)
	}
	if got := o.Unit("Frontend").Lead; got != "VP Eng" {
		t.Errorf("Frontend lead = %q, want the inherited VP Eng", got)
	}
	// An explicit lead also stops the cascade for its own descendants.
	if got := o.Unit("Backend").LeadRole(); got == nil || got.Name != "Tech Lead" {
		t.Errorf("Backend LeadRole() = %v, want Tech Lead", got)
	}
}

func TestUnresolvedLeadIsKeptAndReported(t *testing.T) {
	t.Parallel()
	// Live config management lands a unit before the seat that leads it,
	// and the engine applies every intermediate revision — so this is a
	// warning, not a rejection.
	o := normalized(&Organization{
		Name:  "T",
		Units: []*OrgUnit{{Name: "Engineering", Lead: "Ghost", Roles: []*Role{{Name: "Dev"}}}},
	})
	if got := o.Unit("Engineering").Lead; got != "Ghost" {
		t.Errorf("lead = %q, want the reference kept verbatim", got)
	}
	if o.EffectiveLead(o.Unit("Engineering")) != nil {
		t.Error("a dangling lead resolved to a seat")
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a half-wired org", err)
	}
	want := []DanglingRef{{Kind: RefLead, From: "Engineering", To: "Ghost"}}
	if got := o.DanglingRefs(); !slices.Equal(got, want) {
		t.Errorf("DanglingRefs() = %v, want %v", got, want)
	}
}

// ---- auto-management ------------------------------------------------- //

func TestLeadAutoManagesOnlyUnmanagedMembers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		unit *OrgUnit
		lead string
		want []string
	}{
		{
			name: "flat unit",
			unit: &OrgUnit{Name: "Backend", Lead: "Lead", Roles: []*Role{
				{Name: "Lead"}, {Name: "Dev A"}, {Name: "Dev B"},
			}},
			lead: "Lead", want: []string{"Dev A", "Dev B"},
		},
		{
			// The VP gets the tech lead, not the tech lead's reports:
			// claiming them would flatten a chart the operator drew.
			name: "skips members another member manages",
			unit: &OrgUnit{Name: "Backend", Lead: "VP", Roles: []*Role{
				{Name: "VP"}, {Name: "Tech Lead", Manages: []string{"Dev A", "Dev B"}},
				{Name: "Dev A"}, {Name: "Dev B"},
			}},
			lead: "VP", want: []string{"Tech Lead"},
		},
		{
			name: "explicit entries are kept and extended",
			unit: &OrgUnit{Name: "Backend", Lead: "Lead", Roles: []*Role{
				{Name: "Lead", Manages: []string{"Dev A"}}, {Name: "Dev A"}, {Name: "Dev B"},
			}},
			lead: "Lead", want: []string{"Dev A", "Dev B"},
		},
		{
			name: "a unit of one has nobody to manage",
			unit: &OrgUnit{Name: "Solo", Lead: "PM", Roles: []*Role{{Name: "PM"}}},
			lead: "PM", want: nil,
		},
		{
			// A tech lead who manages the VP that leads their unit is an
			// ordinary chart; auto-managing back would make it a cycle.
			name: "never claims a member that manages the lead",
			unit: &OrgUnit{Name: "Backend", Lead: "Senior Engineer A", Roles: []*Role{
				{Name: "Tech Lead", Manages: []string{"Senior Engineer A"}},
				{Name: "Senior Engineer A"},
			}},
			lead: "Senior Engineer A", want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := normalized(&Organization{Name: "T", Units: []*OrgUnit{tc.unit}})
			if got := sortedManages(t, o, tc.lead); !slices.Equal(got, tc.want) {
				t.Errorf("%s manages %v, want %v", tc.lead, got, tc.want)
			}
		})
	}
}

func TestInheritedLeadAutoManagesTheChildUnit(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*OrgUnit{{
			Name: "Engineering", Lead: "VP Eng", Roles: []*Role{{Name: "VP Eng"}},
			Children: []*OrgUnit{{Name: "Backend", Roles: []*Role{
				{Name: "Tech Lead", Manages: []string{"Dev A"}},
				{Name: "Dev A"},
				{Name: "Dev B"},
			}}},
		}},
	})
	// Dev A already has a manager; the VP inherits the unit, not its people.
	want := []string{"Dev B", "Tech Lead"}
	if got := sortedManages(t, o, "VP Eng"); !slices.Equal(got, want) {
		t.Errorf("VP Eng manages %v, want %v", got, want)
	}
}

func TestHumanLeadAutoManagesAgentMembers(t *testing.T) {
	t.Parallel()
	// A human manager running an AI team is a first-class shape: the seat
	// is addressable, and everything below it routes through the hierarchy
	// exactly as it would under an agent lead.
	o := normalized(&Organization{
		Name: "Acme",
		Units: []*OrgUnit{{
			Name: "Core", Lead: "Sarah Chen",
			Roles: []*Role{human(), {Name: "Dev A"}, {Name: "Dev B"}},
		}},
	})
	want := []string{"Dev A", "Dev B"}
	if got := sortedManages(t, o, "Sarah Chen"); !slices.Equal(got, want) {
		t.Errorf("Sarah Chen manages %v, want %v", got, want)
	}
	dev := o.Role("Dev A")
	manager := o.Manager(dev)
	if manager == nil || !manager.IsHuman() {
		t.Errorf("Manager(Dev A) = %v, want the human lead", manager)
	}
}

// ---- manages expansion ----------------------------------------------- //

func TestManagesExpansion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		org  *Organization
		seat string
		want []string // compared as a sorted set
	}{
		{
			name: "a unit name becomes its members",
			org: &Organization{
				Name:  "T",
				Roles: []*Role{{Name: "CEO", Manages: []string{"Backend"}}},
				Units: []*OrgUnit{{Name: "Backend", Lead: "Lead", Roles: []*Role{
					{Name: "Lead", Manages: []string{"Dev A", "Dev B"}}, {Name: "Dev A"}, {Name: "Dev B"},
				}}},
			},
			seat: "CEO", want: []string{"Dev A", "Dev B", "Lead"},
		},
		{
			name: "expansion reaches descendants",
			org: &Organization{
				Name:  "T",
				Roles: []*Role{{Name: "CTO", Manages: []string{"Engineering"}}},
				Units: []*OrgUnit{{
					Name: "Engineering", Roles: []*Role{{Name: "VP Eng"}},
					Children: []*OrgUnit{{Name: "Backend", Lead: "Dev", Roles: []*Role{{Name: "Dev"}}}},
				}},
			},
			seat: "CTO", want: []string{"Dev", "VP Eng"},
		},
		{
			name: "a seat inside the unit it manages does not manage itself",
			org: &Organization{
				Name: "T",
				Units: []*OrgUnit{{Name: "Team", Roles: []*Role{
					{Name: "Lead", Manages: []string{"Team"}}, {Name: "Dev A"}, {Name: "Dev B"},
				}}},
			},
			seat: "Lead", want: []string{"Dev A", "Dev B"},
		},
		{
			// The seat is the more specific reading, and an operator who
			// named a person meant that person.
			name: "a name that is both a seat and a unit stays the seat",
			org: &Organization{
				Name: "T",
				Roles: []*Role{
					{Name: "Backend", Goal: "Cross-cutting backend advisor"},
					{Name: "CEO", Manages: []string{"Backend"}},
				},
				Units: []*OrgUnit{{Name: "Backend", Lead: "Dev", Roles: []*Role{{Name: "Dev"}}}},
			},
			seat: "CEO", want: []string{"Backend"},
		},
		{
			// Dropping it would quietly rewrite the chart during the
			// bootstrap window where the seat has not arrived yet.
			name: "an unknown name is kept verbatim",
			org: &Organization{
				Name:  "T",
				Roles: []*Role{{Name: "CEO", Manages: []string{"Ghost"}}},
			},
			seat: "CEO", want: []string{"Ghost"},
		},
		{
			name: "a seat listed explicitly is not duplicated by expansion",
			org: &Organization{
				Name:  "T",
				Roles: []*Role{{Name: "CEO", Manages: []string{"Dev", "Backend"}}},
				Units: []*OrgUnit{{Name: "Backend", Lead: "Dev", Roles: []*Role{
					{Name: "Dev", Manages: []string{"Junior"}}, {Name: "Junior"},
				}}},
			},
			seat: "CEO", want: []string{"Dev", "Junior"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := normalized(tc.org)
			if got := sortedManages(t, o, tc.seat); !slices.Equal(got, tc.want) {
				t.Errorf("%s manages %v, want %v", tc.seat, got, tc.want)
			}
		})
	}
}

func TestManagesKeepsExplicitEntriesFirst(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Roles: []*Role{
			{Name: "CEO", Manages: []string{"CTO", "Product"}},
			{Name: "CTO"},
		},
		Units: []*OrgUnit{{Name: "Product", Lead: "PM", Roles: []*Role{{Name: "PM"}, {Name: "Designer"}}}},
	})
	got := o.Role("CEO").Manages
	if len(got) == 0 || got[0] != "CTO" {
		t.Fatalf("CEO manages %v, want the explicit seat first", got)
	}
	rest := slices.Clone(got[1:])
	slices.Sort(rest)
	if !slices.Equal(rest, []string{"Designer", "PM"}) {
		t.Errorf("CEO manages %v, want the unit expanded after CTO", got)
	}
}

// ---- MCP credential inheritance --------------------------------------- //

func TestMCPEnvInheritanceAndOverride(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*OrgUnit{{
			Name:   "Engineering",
			MCPEnv: MCPEnv{"atlassian": {"JIRA_URL": "https://acme.example.com", "JIRA_API_TOKEN": "${TEAM}"}},
			Roles: []*Role{
				{Name: "VP Eng"},
				{Name: "Tech Lead", MCPEnv: MCPEnv{"atlassian": {"JIRA_API_TOKEN": "${MINE}"}}},
			},
			// Not inherited across a unit boundary: a child unit declares
			// what its own team shares.
			Children: []*OrgUnit{{Name: "Backend", Roles: []*Role{{Name: "Dev"}}}},
		}},
	})
	vp := o.Role("VP Eng").MCPEnv["atlassian"]
	if vp["JIRA_URL"] != "https://acme.example.com" || vp["JIRA_API_TOKEN"] != "${TEAM}" {
		t.Errorf("VP Eng inherited %v", vp)
	}
	lead := o.Role("Tech Lead").MCPEnv["atlassian"]
	if lead["JIRA_API_TOKEN"] != "${MINE}" {
		t.Errorf("Tech Lead token = %q, want its own", lead["JIRA_API_TOKEN"])
	}
	if lead["JIRA_URL"] != "https://acme.example.com" {
		t.Errorf("Tech Lead lost the variable it did not override: %v", lead)
	}
	if got := o.Role("Dev").MCPEnv; len(got) != 0 {
		t.Errorf("a child unit's seat inherited %v from the parent unit", got)
	}
}

// ---- the unit: soft reference ----------------------------------------- //

func TestRootSeatMovesIntoItsNamedUnit(t *testing.T) {
	t.Parallel()
	// A seat added through the per-entity config API arrives at the root
	// with a unit: reference. Left there it would miss the unit's MCP
	// credentials and be invisible to the unit lead — so the move has to
	// precede both.
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "Dev", UnitRef: "Backend"}, {Name: "CEO"}},
		Units: []*OrgUnit{{
			Name: "Backend", Lead: "Lead",
			MCPEnv: MCPEnv{"atlassian": {"JIRA_API_TOKEN": "${TEAM}"}},
			Roles:  []*Role{{Name: "Lead"}},
		}},
	})
	backend := o.Unit("Backend")
	if backend.Role("Dev") == nil {
		t.Fatal("the seat did not move into its unit")
	}
	if got := roleNames(o.Roles); !slices.Equal(got, []string{"CEO"}) {
		t.Errorf("root seats = %v, want only CEO", got)
	}
	if got := o.Role("Dev").MCPEnv["atlassian"]["JIRA_API_TOKEN"]; got != "${TEAM}" {
		t.Errorf("moved seat inherited %q, want the unit credential", got)
	}
	if !slices.Contains(o.Role("Lead").Manages, "Dev") {
		t.Errorf("the unit lead does not manage the moved seat: %v", o.Role("Lead").Manages)
	}
	if o.UnitFor(o.Role("Dev")) != backend {
		t.Error("the moved seat does not resolve to its unit")
	}
}

func TestUnresolvedUnitRefKeepsTheSeatAtRoot(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "Dev", UnitRef: "Nowhere"}},
		Units: []*OrgUnit{{Name: "Backend", Roles: []*Role{{Name: "Lead"}}}},
	})
	if got := roleNames(o.Roles); !slices.Equal(got, []string{"Dev"}) {
		t.Errorf("root seats = %v, want the seat kept", got)
	}
	want := []DanglingRef{{Kind: RefUnit, From: "Dev", To: "Nowhere"}}
	if got := o.DanglingRefs(); !slices.Equal(got, want) {
		t.Errorf("DanglingRefs() = %v, want %v", got, want)
	}
}

// ---- normalisation is idempotent -------------------------------------- //

// TestNormalizeIsIdempotent: live config management re-applies whole
// revisions, so a second pass must not compound what the first derived —
// duplicated manages entries, credentials re-merged, a lead cascaded twice.
func TestNormalizeIsIdempotent(t *testing.T) {
	t.Parallel()
	build := func() *Organization {
		return &Organization{
			Name:  "Acme",
			Roles: []*Role{{Name: "CEO", Manages: []string{"Engineering"}}, {Name: "Dev C", UnitRef: "Backend"}},
			Units: []*OrgUnit{{
				Name: "Engineering", Lead: "VP Eng", SlackChannel: "C_ENG",
				MCPEnv: MCPEnv{"atlassian": {"JIRA_API_TOKEN": "${TEAM}"}},
				Roles:  []*Role{{Name: "VP Eng"}, {Name: "Analyst"}},
				Children: []*OrgUnit{{
					Name:  "Backend",
					Roles: []*Role{{Name: "Tech Lead", Manages: []string{"Dev A"}}, {Name: "Dev A"}},
				}},
			}},
		}
	}
	once, twice := build(), build()
	once.Normalize()
	twice.Normalize()
	twice.Normalize()

	first, err := yaml.Marshal(once)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := yaml.Marshal(twice)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("a second Normalize changed the org:\n--- once ---\n%s\n--- twice ---\n%s", first, second)
	}
}

// ---- seat identity ---------------------------------------------------- //

func identityOrg() *Organization {
	return normalized(&Organization{
		Name:  "TestCo",
		Roles: []*Role{{Name: "Founder Bot", Goal: "oversee"}},
		Units: []*OrgUnit{{
			Name: "eng", Lead: "Tech Lead",
			Roles: []*Role{
				{Name: "Tech Lead", Manages: []string{"Developer"}},
				{Name: "Developer"},
				human(),
			},
		}},
	})
}

func TestAgentIDHasNoProcessLocalInput(t *testing.T) {
	t.Parallel()
	o := identityOrg()
	got, ok := o.AgentIDFor(o.Role("Developer"))
	if !ok {
		t.Fatal("no id for an agent seat")
	}
	want, _ := DeriveAgentID("TestCo", "developer")
	if got != want {
		t.Errorf("AgentIDFor = %s, want %s", got, want)
	}
	// The org name is in the digest, so two companies sharing one store can
	// both have a developer.
	other := normalized(&Organization{Name: "Globex", Roles: []*Role{{Name: "Developer"}}})
	if otherID, _ := other.AgentIDFor(other.Role("Developer")); otherID == got {
		t.Error("the same handle in two orgs derived one id")
	}
}

func TestAgentIDRefusesSeatsThatHaveNone(t *testing.T) {
	t.Parallel()
	o := identityOrg()
	// A human seat is addressable but never spawned: it has no agent id at
	// all, and uuid.Nil would be an id every human shares.
	if id, ok := o.AgentIDFor(o.Role("Sarah Chen")); ok {
		t.Errorf("a human seat derived id %s", id)
	}
	if _, ok := o.AgentIDFor(nil); ok {
		t.Error("a nil seat derived an id")
	}
	unnamed := normalized(&Organization{Roles: []*Role{{Name: "Developer"}}})
	if _, ok := unnamed.AgentIDFor(unnamed.Role("Developer")); ok {
		t.Error("an unnamed org derived an id")
	}
}

func TestSeatLookupByHandle(t *testing.T) {
	t.Parallel()
	o := identityOrg()
	if got := o.AgentSeatByHandle("developer"); got != o.Role("Developer") {
		t.Error("a unit seat is not reachable by handle")
	}
	if got := o.AgentSeatByHandle("founder-bot"); got != o.Role("Founder Bot") {
		t.Error("a root seat is not reachable by handle")
	}
	// Handles are unique across kinds, but this lookup's callers publish to
	// an inbox, and a human seat has none.
	if got := o.AgentSeatByHandle("sarah-chen"); got != nil {
		t.Errorf("a human seat resolved as an agent: %v", got)
	}
	for _, handle := range []string{"", "nobody"} {
		if got := o.AgentSeatByHandle(handle); got != nil {
			t.Errorf("AgentSeatByHandle(%q) = %v, want nil", handle, got)
		}
	}
}

// TestSeatLookupInvertsTheDerivation is the property routing depends on: a
// node holding only an id can name the seat, whether or not it runs it.
func TestSeatLookupInvertsTheDerivation(t *testing.T) {
	t.Parallel()
	o := identityOrg()
	for r := range o.AllRoles() {
		id, ok := o.AgentIDFor(r)
		if !ok {
			continue
		}
		if got := o.AgentSeatByID(id); got != r {
			t.Errorf("AgentSeatByID(%s) = %v, want %q", id, got, r.Name)
		}
	}
	if got := o.AgentSeatByID(uuid.Nil); got != nil {
		t.Errorf("AgentSeatByID(nil) = %v, want nil", got)
	}
	elsewhere, _ := DeriveAgentID("Globex", "developer")
	if got := o.AgentSeatByID(elsewhere); got != nil {
		t.Errorf("another company's id resolved to %v", got)
	}
}

func TestExplicitHandleDrivesIdentityEverywhere(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "TestCo",
		Roles: []*Role{{Name: "Senior Backend Engineer", DeclaredHandle: "sbe"}},
	})
	seat := o.Role("Senior Backend Engineer")
	want, _ := DeriveAgentID("TestCo", "sbe")
	got, ok := o.AgentIDFor(seat)
	if !ok || got != want {
		t.Errorf("AgentIDFor = %s (%v), want %s", got, ok, want)
	}
	if o.AgentSeatByHandle("sbe") != seat {
		t.Error("the explicit handle does not resolve")
	}
	if o.AgentSeatByHandle("senior-backend-engineer") != nil {
		t.Error("the slugified name still resolves after an override")
	}
	if o.AgentSeatByID(got) != seat {
		t.Error("the derived id does not invert")
	}
}

// ---- org-wide validation ---------------------------------------------- //

func TestDuplicateHandlesAreFatal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		roles []*Role
	}{
		{
			// Two agents would share an inbox topic.
			name:  "agent and agent",
			roles: []*Role{{Name: "Dev", DeclaredHandle: "dev"}, {Name: "Dev Two", DeclaredHandle: "dev"}},
		},
		{
			// The agent would absorb the person's inbound activity.
			name: "agent and human",
			roles: []*Role{
				{Name: "Sarah", DeclaredHandle: "sarah"},
				human(func(r *Role) { r.Name = "Sarah Ops"; r.DeclaredHandle = "sarah" }),
			},
		},
		{
			name:  "derived collision across levels",
			roles: []*Role{{Name: "Tech Lead"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := &Organization{Name: "T", Roles: tc.roles}
			if tc.name == "derived collision across levels" {
				o.Units = []*OrgUnit{{Name: "Eng", Roles: []*Role{{Name: "tech lead"}}}}
			}
			o.Normalize()
			err := o.Validate()
			if !errors.Is(err, ErrDuplicateHandle) {
				t.Fatalf("Validate() = %v, want ErrDuplicateHandle", err)
			}
		})
	}
}

func TestLeadScheduleUnderAHumanLeadIsRejected(t *testing.T) {
	t.Parallel()
	report := Schedule{Name: "report", Cron: "0 17 * * 5", Task: "weekly report", Target: TargetLead}

	t.Run("direct human lead", func(t *testing.T) {
		t.Parallel()
		o := normalized(&Organization{Name: "T", Units: []*OrgUnit{{
			Name: "Team", Lead: "Sarah Chen",
			Roles:     []*Role{human(), {Name: "Dev"}},
			Schedules: []Schedule{report},
		}}})
		if err := o.Validate(); !errors.Is(err, ErrUnrunnableSchedule) {
			t.Errorf("Validate() = %v, want ErrUnrunnableSchedule", err)
		}
	})

	t.Run("inherited human lead", func(t *testing.T) {
		t.Parallel()
		// The inheritance is what makes this worth checking at org level:
		// the unit naming the schedule names no lead at all.
		o := normalized(&Organization{Name: "T", Units: []*OrgUnit{{
			Name: "Dept", Lead: "Sarah Chen", Roles: []*Role{human()},
			Children: []*OrgUnit{{
				Name: "Team", Roles: []*Role{{Name: "Dev"}}, Schedules: []Schedule{report},
			}},
		}}})
		err := o.Validate()
		if !errors.Is(err, ErrUnrunnableSchedule) {
			t.Fatalf("Validate() = %v, want ErrUnrunnableSchedule", err)
		}
		if !strings.Contains(err.Error(), "Sarah Chen") {
			t.Errorf("error does not name the effective lead: %v", err)
		}
	})

	t.Run("disabled is config an operator is holding", func(t *testing.T) {
		t.Parallel()
		held := report
		held.Enabled = Off()
		o := normalized(&Organization{Name: "T", Units: []*OrgUnit{{
			Name: "Team", Lead: "Sarah Chen",
			Roles:     []*Role{human(), {Name: "Dev"}},
			Schedules: []Schedule{held},
		}}})
		if err := o.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
}

func TestValidateWalksTheWholeTree(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "CEO", DeclaredHandle: "CEO"}},
		Units: []*OrgUnit{{Name: "Eng", Children: []*OrgUnit{{
			Name: "Backend", Roles: []*Role{human(func(r *Role) { r.TokenBudget = 5 })},
		}}}},
	})
	err := o.Validate()
	for _, want := range []error{ErrInvalidHandle, ErrHumanSeatField} {
		if !errors.Is(err, want) {
			t.Errorf("Validate() = %v, missing %v", err, want)
		}
	}
}

func TestAWellFormedOrgValidates(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "Acme AI", Mission: "Build the best widgets",
		Roles: []*Role{human(func(r *Role) { r.Name = "Jane Founder"; r.Manages = []string{"CEO"} })},
		Units: []*OrgUnit{{
			Name: "Engineering", Type: UnitTypeDepartment, Lead: "CEO",
			Roles:     []*Role{{Name: "CEO"}, {Name: "Engineer", LLM: ProviderKeys{"claude-sonnet", "gpt-4o"}}},
			Schedules: []Schedule{{Name: "standup", Cron: "0 9 * * 1-5", Task: "post standup", Timezone: "UTC"}},
		}},
	})
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if got := o.DanglingRefs(); len(got) != 0 {
		t.Errorf("DanglingRefs() = %v, want none", got)
	}
}

// TestOrganizationDecodesFromYAML pins the wire names an operator writes.
// A renamed field decodes to a zero value in silence — a seat that loses
// its budget, its credentials or its placement and still boots.
func TestOrganizationDecodesFromYAML(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme AI
mission: Build the best widgets
confluence_spaces: [ENG, PRODUCT]
roles:
  - name: Jane Founder
    kind: human
    manages: [CEO]
    availability: CET business hours
    contact:
      slack_user_id: U0FOUNDER
      github_login: JaneDoe
      plane_user_id: "${PLANE_FOUNDER_USER_ID}"
  - name: Dev C
    unit: Backend
units:
  - name: Engineering
    type: department
    lead: CEO
    slack_channel: C_ENG
    mcp_env:
      atlassian:
        JIRA_API_TOKEN: "${JIRA_TOKEN_TEAM}"
    roles:
      - name: CEO
        handle: chief
        token_budget: 100000
        llm: [claude-sonnet, gpt-4o]
        llm_judge: claude-haiku
        learning_enabled: false
        placement:
          node: node-a
          labels:
            zone: eu-west
        sandbox:
          enabled: true
          coding_agent: opencode
          env:
            GITHUB_TOKEN: "${GITHUB_TOKEN_CEO}"
    children:
      - name: Backend
        schedules:
          - name: standup
            cron: "0 9 * * 1-5"
            task: post the standup
            timezone: Europe/Amsterdam
            enabled: false
`
	var o Organization
	if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	o.Normalize()
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}

	ceo := o.Role("CEO")
	if ceo == nil {
		t.Fatal("no CEO")
	}
	if got := ceo.Handle(); got != "chief" {
		t.Errorf("handle = %q, want chief", got)
	}
	if !slices.Equal(ceo.LLM, ProviderKeys{"claude-sonnet", "gpt-4o"}) || !slices.Equal(ceo.LLMJudge, ProviderKeys{"claude-haiku"}) {
		t.Errorf("llm = %v, llm_judge = %v", ceo.LLM, ceo.LLMJudge)
	}
	if ceo.TokenBudget != 100000 || ceo.LearningEnabled.Or(true) {
		t.Errorf("token_budget = %d, learning_enabled = %v", ceo.TokenBudget, ceo.LearningEnabled)
	}
	if ceo.Placement.Node != "node-a" || ceo.Placement.Labels["zone"] != "eu-west" {
		t.Errorf("placement = %+v", ceo.Placement)
	}
	if ceo.Sandbox == nil || !ceo.Sandbox.Enabled || ceo.Sandbox.Env["GITHUB_TOKEN"] != "${GITHUB_TOKEN_CEO}" {
		t.Errorf("sandbox = %+v", ceo.Sandbox)
	}
	if got := ceo.MCPEnv["atlassian"]["JIRA_API_TOKEN"]; got != "${JIRA_TOKEN_TEAM}" {
		t.Errorf("inherited credential = %q", got)
	}

	founder := o.Role("Jane Founder")
	if founder == nil || !founder.IsHuman() {
		t.Fatalf("founder = %v", founder)
	}
	if got := founder.Contact.GitHubLogin; got != "janedoe" {
		t.Errorf("github_login = %q, want it lowercased", got)
	}
	if got := founder.Contact.PlaneUserID; got != "${PLANE_FOUNDER_USER_ID}" {
		t.Errorf("plane_user_id = %q, want the reference verbatim", got)
	}
	if o.Unit("Backend").Role("Dev C") == nil {
		t.Error("the unit: reference did not move the seat")
	}
	if got := o.Unit("Backend").SlackChannel; got != "C_ENG" {
		t.Errorf("child channel = %q, want the inherited C_ENG", got)
	}
	if got := o.Unit("Backend").Schedules[0]; got.IsEnabled() || got.Timeout() != DefaultScheduleTimeout {
		t.Errorf("schedule = %+v", got)
	}
	if !slices.Equal(o.ConfluenceSpaces, []string{"ENG", "PRODUCT"}) {
		t.Errorf("confluence_spaces = %v", o.ConfluenceSpaces)
	}
}

func TestTraversalCoversRootAndUnits(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "CEO"}, {Name: "CTO"}},
		Units: []*OrgUnit{{
			Name: "Engineering", Roles: []*Role{{Name: "VP Eng"}},
			Children: []*OrgUnit{{Name: "Backend", Lead: "Dev", Roles: []*Role{{Name: "Dev"}}}},
		}},
	})
	if got := roleNames(slices.Collect(o.AllRoles())); !slices.Equal(got, []string{"CEO", "CTO", "VP Eng", "Dev"}) {
		t.Errorf("AllRoles() = %v", got)
	}
	var units []string
	for u := range o.AllUnits() {
		units = append(units, u.Name)
	}
	if !slices.Equal(units, []string{"Engineering", "Backend"}) {
		t.Errorf("AllUnits() = %v, want parents before children", units)
	}
	// Early exit must actually stop the walk, not just discard the rest.
	var seen int
	for range o.AllRoles() {
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("breaking out of AllRoles visited %d seats", seen)
	}
	if o.Role("Nobody") != nil || o.Unit("Nowhere") != nil {
		t.Error("a missing name resolved to something")
	}
}
