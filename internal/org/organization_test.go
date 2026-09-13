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
		Units: []*Unit{{
			Name: "Technology", Type: UnitTypeDivision, Lead: "CTO",
			Channel: "C_TECH",
			Roles:   []*Role{{Name: "CTO"}},
			Children: []*Unit{{
				Name: "Engineering", Type: UnitTypeDepartment,
				Children: []*Unit{{
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
		if u.Channel != "C_TECH" {
			t.Errorf("unit %q channel = %q, want C_TECH", unit, u.Channel)
		}
	}
}

func TestExplicitLeadIsNeverOverwritten(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*Unit{{
			Name: "Engineering", Lead: "VP Eng", Roles: []*Role{{Name: "VP Eng"}},
			Children: []*Unit{
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
		Units: []*Unit{{Name: "Engineering", Lead: "Ghost", Roles: []*Role{{Name: "Dev"}}}},
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
	// The unit carrying the reference is the entity itself, which is what
	// a caller placing it in a document locates it by.
	want := []DanglingRef{{Kind: RefLead, From: "Engineering", To: "Ghost", Unit: o.Unit("Engineering")}}
	if got := o.DanglingRefs(); !slices.Equal(got, want) {
		t.Errorf("DanglingRefs() = %v, want %v", got, want)
	}
}

// TestNestedUnitsReportAnInheritedDanglingLeadOnce: the misspelling is
// written once, on the division, and reported once. DanglingRefs used to run
// over the effective leads after the cascade, so every descendant that
// inherited the name was reported as well, naming units whose authors had
// written no lead at all. A descendant that WRITES the same name is a second
// misspelling and is reported on its own.
func TestNestedUnitsReportAnInheritedDanglingLeadOnce(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*Unit{{
			Name: "Engineering", Lead: "Ghost", Roles: []*Role{{Name: "VP Eng"}},
			Children: []*Unit{
				{
					Name: "Backend", Roles: []*Role{{Name: "Dev A"}},
					Children: []*Unit{{Name: "Storage", Roles: []*Role{{Name: "Dev S"}}}},
				},
				{Name: "Infra", Roles: []*Role{{Name: "Dev I"}}},
				{Name: "Security", Lead: "Ghost", Roles: []*Role{{Name: "Dev X"}}},
			},
		}},
	})
	// The inheritance itself is unchanged: every descendant still carries
	// the reference, and every reader still treats it as no lead.
	for _, name := range []string{"Backend", "Storage", "Infra"} {
		if got := o.Unit(name).Lead; got != "Ghost" {
			t.Errorf("unit %q lead = %q, want the inherited Ghost", name, got)
		}
	}
	want := []DanglingRef{
		{Kind: RefLead, From: "Engineering", To: "Ghost", Unit: o.Unit("Engineering")},
		{Kind: RefLead, From: "Security", To: "Ghost", Unit: o.Unit("Security")},
	}
	if got := o.DanglingRefs(); !slices.Equal(got, want) {
		t.Errorf("DanglingRefs() = %v, want %v", got, want)
	}
}

// TestNormalizeRecordsWhatEachUnitDeclared: after the cascade an inherited
// lead and a declared one are the same string, so the authored value has to
// be recorded before it is overwritten, and a second pass must read that
// record rather than record the inherited value as authored.
func TestNormalizeRecordsWhatEachUnitDeclared(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*Unit{{
			Name: "Engineering", Lead: "VP Eng", Channel: "C_ENG",
			Roles: []*Role{{Name: "VP Eng"}},
			Children: []*Unit{
				{Name: "Backend", Roles: []*Role{{Name: "Dev A"}}},
				// Names the same seat and channel as its parent. Declared,
				// not inherited: changing the parent must not move it.
				{Name: "Platform", Lead: "VP Eng", Channel: "C_ENG", Roles: []*Role{{Name: "Dev P"}}},
				{Name: "Frontend", Channel: "C_WEB", Roles: []*Role{{Name: "Dev F"}}},
			},
		}},
	})
	o.Normalize()

	for _, tc := range []struct {
		unit                     string
		lead, declaredLead       string
		channel, declaredChannel string
	}{
		{unit: "Engineering", lead: "VP Eng", declaredLead: "VP Eng", channel: "C_ENG", declaredChannel: "C_ENG"},
		{unit: "Backend", lead: "VP Eng", declaredLead: "", channel: "C_ENG", declaredChannel: ""},
		{unit: "Platform", lead: "VP Eng", declaredLead: "VP Eng", channel: "C_ENG", declaredChannel: "C_ENG"},
		{unit: "Frontend", lead: "VP Eng", declaredLead: "", channel: "C_WEB", declaredChannel: "C_WEB"},
	} {
		u := o.Unit(tc.unit)
		if u.Lead != tc.lead || u.DeclaredLead != tc.declaredLead {
			t.Errorf("unit %q lead = %q declared %q, want %q declared %q",
				tc.unit, u.Lead, u.DeclaredLead, tc.lead, tc.declaredLead)
		}
		if u.Channel != tc.channel || u.DeclaredChannel != tc.declaredChannel {
			t.Errorf("unit %q channel = %q declared %q, want %q declared %q",
				tc.unit, u.Channel, u.DeclaredChannel, tc.channel, tc.declaredChannel)
		}
	}
}

// A manages entry naming neither a seat nor a unit is kept verbatim (the seat
// may not have landed yet) and reported; an entry naming a seat, a unit, or a
// unit that happens to hold no seats is not a misspelling.
func TestADanglingManagesEntryIsReported(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Roles: []*Role{
			{Name: "CEO", Manages: []string{"CTO", "Engineering", "Hiring", "Ghost"}},
			{Name: "CTO"},
			human(func(r *Role) { r.Manages = []string{"CEO", "Nobody"} }),
		},
		Units: []*Unit{
			{Name: "Engineering", Lead: "Tech Lead", Roles: []*Role{{Name: "Tech Lead"}, {Name: "Dev"}}},
			{Name: "Hiring"},
		},
	})
	want := []DanglingRef{
		{Kind: RefManages, From: "CEO", To: "Ghost", Seat: o.Role("CEO")},
		{Kind: RefManages, From: "Sarah Chen", To: "Nobody", Seat: o.Role("Sarah Chen")},
	}
	if got := o.DanglingRefs(); !slices.Equal(got, want) {
		t.Errorf("DanglingRefs() = %v, want %v", got, want)
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil: a dangling entry is a warning", err)
	}
}

// Every kind renders a message naming both ends of the reference, so a log
// line or a warning is actionable without the structured fields beside it.
func TestADanglingReferenceMessageNamesBothEnds(t *testing.T) {
	t.Parallel()
	for _, ref := range []DanglingRef{
		{Kind: RefLead, From: "Engineering", To: "Ghost"},
		{Kind: RefUnit, From: "Dev", To: "Nowhere"},
		{Kind: RefManages, From: "CEO", To: "Ghost"},
		{Kind: RefGitLabAccessLevel, From: "integrations.gitlab.provisioning.access_levels", To: "old-seat"},
		{Kind: RefKind("future_kind"), From: "somewhere", To: "something"},
	} {
		msg := ref.Message()
		if !strings.Contains(msg, ref.From) || !strings.Contains(msg, ref.To) {
			t.Errorf("%s message %q does not name %q and %q", ref.Kind, msg, ref.From, ref.To)
		}
		if strings.Contains(msg, "\u2014") {
			t.Errorf("%s message %q contains an em dash", ref.Kind, msg)
		}
	}
}

// ---- auto-management ------------------------------------------------- //

func TestLeadAutoManagesOnlyUnmanagedMembers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		unit *Unit
		lead string
		want []string
	}{
		{
			name: "flat unit",
			unit: &Unit{Name: "Backend", Lead: "Lead", Roles: []*Role{
				{Name: "Lead"}, {Name: "Dev A"}, {Name: "Dev B"},
			}},
			lead: "Lead", want: []string{"Dev A", "Dev B"},
		},
		{
			// The VP gets the tech lead, not the tech lead's reports:
			// claiming them would flatten a chart the operator drew.
			name: "skips members another member manages",
			unit: &Unit{Name: "Backend", Lead: "VP", Roles: []*Role{
				{Name: "VP"}, {Name: "Tech Lead", Manages: []string{"Dev A", "Dev B"}},
				{Name: "Dev A"}, {Name: "Dev B"},
			}},
			lead: "VP", want: []string{"Tech Lead"},
		},
		{
			name: "explicit entries are kept and extended",
			unit: &Unit{Name: "Backend", Lead: "Lead", Roles: []*Role{
				{Name: "Lead", Manages: []string{"Dev A"}}, {Name: "Dev A"}, {Name: "Dev B"},
			}},
			lead: "Lead", want: []string{"Dev A", "Dev B"},
		},
		{
			name: "a unit of one has nobody to manage",
			unit: &Unit{Name: "Solo", Lead: "PM", Roles: []*Role{{Name: "PM"}}},
			lead: "PM", want: nil,
		},
		{
			// A tech lead who manages the VP that leads their unit is an
			// ordinary chart; auto-managing back would make it a cycle.
			name: "never claims a member that manages the lead",
			unit: &Unit{Name: "Backend", Lead: "Senior Engineer A", Roles: []*Role{
				{Name: "Tech Lead", Manages: []string{"Senior Engineer A"}},
				{Name: "Senior Engineer A"},
			}},
			lead: "Senior Engineer A", want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := normalized(&Organization{Name: "T", Units: []*Unit{tc.unit}})
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
		Units: []*Unit{{
			Name: "Engineering", Lead: "VP Eng", Roles: []*Role{{Name: "VP Eng"}},
			Children: []*Unit{{Name: "Backend", Roles: []*Role{
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

// managementCycle returns the name of a seat that manages itself through a
// chain of reports, or "" when the chart is acyclic. Every manages edge
// counts, not only the primary one [Organization.Manager] reports, because a
// cycle anywhere is a loop an escalation or a roster walk can enter.
func managementCycle(o *Organization) string {
	for start := range o.AllRoles() {
		seen := map[string]struct{}{}
		frontier := slices.Clone(start.Manages)
		for len(frontier) > 0 {
			name := frontier[0]
			frontier = frontier[1:]
			if name == start.Name {
				return start.Name
			}
			if _, done := seen[name]; done {
				continue
			}
			seen[name] = struct{}{}
			if r := o.Role(name); r != nil {
				frontier = append(frontier, r.Manages...)
			}
		}
	}
	return ""
}

// TestAUnitReferenceShieldsTheMembersItReaches is the shield probe: a direct
// member that manages its own unit BY NAME has claimed those members as
// surely as one that lists them. Auto-management used to read the entry as
// written, see no member names in it, and hand the inherited lead every
// member as well, so each developer had two managers and the primary one
// (the first seat in walk order) was the VP rather than the tech lead the
// chart named.
func TestAUnitReferenceShieldsTheMembersItReaches(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*Unit{{
			Name: "Engineering", Lead: "VP Eng", Roles: []*Role{{Name: "VP Eng"}},
			Children: []*Unit{{Name: "Backend", Roles: []*Role{
				{Name: "Tech Lead", Manages: []string{"Backend"}},
				{Name: "Dev A"},
				{Name: "Dev B"},
			}}},
		}},
	})
	if got, want := sortedManages(t, o, "VP Eng"), []string{"Tech Lead"}; !slices.Equal(got, want) {
		t.Errorf("VP Eng manages %v, want %v: the unit reference shields its members", got, want)
	}
	if got, want := sortedManages(t, o, "Tech Lead"), []string{"Dev A", "Dev B"}; !slices.Equal(got, want) {
		t.Errorf("Tech Lead manages %v, want %v", got, want)
	}
	for _, dev := range []string{"Dev A", "Dev B"} {
		if m := o.Manager(o.Role(dev)); m == nil || m.Name != "Tech Lead" {
			t.Errorf("Manager(%s) = %v, want Tech Lead", dev, m)
		}
	}
}

// A name that is both a seat and a unit is the SEAT in the shield exactly as
// it is in the expansion, because the shield is only honest while it reads
// manages the way the expansion will leave it.
func TestTheShieldReadsASeatNameAsTheSeat(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "Backend", Goal: "Cross-cutting backend advisor"}},
		Units: []*Unit{{Name: "Backend", Lead: "Lead", Roles: []*Role{
			{Name: "Lead"},
			{Name: "Mentor", Manages: []string{"Backend"}},
			{Name: "Dev A"},
		}}},
	})
	if got, want := sortedManages(t, o, "Mentor"), []string{"Backend"}; !slices.Equal(got, want) {
		t.Errorf("Mentor manages %v, want %v (the seat, not the unit)", got, want)
	}
	if got, want := sortedManages(t, o, "Lead"), []string{"Dev A", "Mentor"}; !slices.Equal(got, want) {
		t.Errorf("Lead manages %v, want %v: nothing in the unit is shielded", got, want)
	}
}

// TestAMemberManagingItsOwnUnitIsNeverClaimedByItsLead is the cycle probe.
// The member's unit reference reaches the lead, so the member manages the
// lead; claiming the member back is the two-seat cycle the member-manages-
// lead guard exists to prevent, and the guard missed it because it looked
// for the lead's name among the entries as written.
func TestAMemberManagingItsOwnUnitIsNeverClaimedByItsLead(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*Unit{{Name: "Backend", Lead: "Backend Lead", Roles: []*Role{
			{Name: "Backend Lead"},
			{Name: "Engineering Manager", Manages: []string{"Backend"}},
			{Name: "Dev A"},
		}}},
	})
	if seat := managementCycle(o); seat != "" {
		t.Fatalf("seat %q manages itself through its reports: Backend Lead manages %v, Engineering Manager manages %v",
			seat, o.Role("Backend Lead").Manages, o.Role("Engineering Manager").Manages)
	}
	if got := sortedManages(t, o, "Backend Lead"); len(got) != 0 {
		t.Errorf("Backend Lead manages %v, want nobody: every member is already managed", got)
	}
	if got, want := sortedManages(t, o, "Engineering Manager"), []string{"Backend Lead", "Dev A"}; !slices.Equal(got, want) {
		t.Errorf("Engineering Manager manages %v, want %v", got, want)
	}
}

// TestARootSeatManagingAUnitGivesItsMembersASecondManager pins a DELIBERATE
// consequence of the shield's scope. The shield is the unit's own direct
// members' manages, never the company's: management is stored on the
// manager, so a CEO managing a division by name lists every seat in it, and
// an org-wide shield would strip every lead beneath it of their roster. The
// cost is visible and documented instead: the member has two managers, and
// [Organization.Manager] names the root seat because root seats walk first.
func TestARootSeatManagingAUnitGivesItsMembersASecondManager(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "CEO", Manages: []string{"Backend"}}},
		Units: []*Unit{{Name: "Backend", Lead: "Lead", Roles: []*Role{
			{Name: "Lead"}, {Name: "Dev A"},
		}}},
	})
	if got, want := sortedManages(t, o, "CEO"), []string{"Dev A", "Lead"}; !slices.Equal(got, want) {
		t.Errorf("CEO manages %v, want %v", got, want)
	}
	if got, want := sortedManages(t, o, "Lead"), []string{"Dev A"}; !slices.Equal(got, want) {
		t.Errorf("Lead manages %v, want %v: an outside manager does not shield a unit's members", got, want)
	}
	if m := o.Manager(o.Role("Dev A")); m == nil || m.Name != "CEO" {
		t.Errorf("Manager(Dev A) = %v, want CEO (root seats walk first)", m)
	}
	if seat := managementCycle(o); seat != "" {
		t.Errorf("seat %q manages itself", seat)
	}
}

func TestHumanLeadAutoManagesAgentMembers(t *testing.T) {
	t.Parallel()
	// A human manager running an AI team is a first-class shape: the seat
	// is addressable, and everything below it routes through the hierarchy
	// exactly as it would under an agent lead.
	o := normalized(&Organization{
		Name: "Acme",
		Units: []*Unit{{
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
				Units: []*Unit{{Name: "Backend", Lead: "Lead", Roles: []*Role{
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
				Units: []*Unit{{
					Name: "Engineering", Roles: []*Role{{Name: "VP Eng"}},
					Children: []*Unit{{Name: "Backend", Lead: "Dev", Roles: []*Role{{Name: "Dev"}}}},
				}},
			},
			seat: "CTO", want: []string{"Dev", "VP Eng"},
		},
		{
			name: "a seat inside the unit it manages does not manage itself",
			org: &Organization{
				Name: "T",
				Units: []*Unit{{Name: "Team", Roles: []*Role{
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
				Units: []*Unit{{Name: "Backend", Lead: "Dev", Roles: []*Role{{Name: "Dev"}}}},
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
				Units: []*Unit{{Name: "Backend", Lead: "Dev", Roles: []*Role{
					{Name: "Dev", Manages: []string{"Junior"}}, {Name: "Junior"},
				}}},
			},
			seat: "CEO", want: []string{"Dev", "Junior"},
		},
		{
			// The same pair in the other order. The expansion used to
			// deduplicate only the names a unit expanded to, so a seat
			// entry AFTER a unit reaching it was listed twice.
			name: "a seat listed after a unit that reaches it is not duplicated",
			org: &Organization{
				Name:  "T",
				Roles: []*Role{{Name: "CEO", Manages: []string{"Backend", "Dev"}}},
				Units: []*Unit{{Name: "Backend", Lead: "Dev", Roles: []*Role{
					{Name: "Dev", Manages: []string{"Junior"}}, {Name: "Junior"},
				}}},
			},
			seat: "CEO", want: []string{"Dev", "Junior"},
		},
		{
			// Organization.Unit answers with the first unit of a name, and
			// so must the expansion: a stored revision can still hold two.
			name: "a duplicated unit name expands to the first unit",
			org: &Organization{
				Name:  "T",
				Roles: []*Role{{Name: "CEO", Manages: []string{"Core"}}},
				Units: []*Unit{
					{Name: "Core", Roles: []*Role{{Name: "First"}}},
					{Name: "Core", Roles: []*Role{{Name: "Second"}}},
				},
			},
			seat: "CEO", want: []string{"First"},
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
		Units: []*Unit{{Name: "Product", Lead: "PM", Roles: []*Role{{Name: "PM"}, {Name: "Designer"}}}},
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
		Units: []*Unit{{
			Name:   "Engineering",
			MCPEnv: MCPEnv{"atlassian": {"JIRA_URL": "https://acme.example.com", "JIRA_API_TOKEN": "${TEAM}"}},
			Roles: []*Role{
				{Name: "VP Eng"},
				{Name: "Tech Lead", MCPEnv: MCPEnv{"atlassian": {"JIRA_API_TOKEN": "${MINE}"}}},
			},
			// Not inherited across a unit boundary: a child unit declares
			// what its own team shares.
			Children: []*Unit{{Name: "Backend", Roles: []*Role{{Name: "Dev"}}}},
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

// TestHumanMembersInheritNoToolCredentials: a human seat runs no tools and is
// refused an mcp_env of its own. Layering the unit's block under a human
// member put that forbidden field on a seat whose author never wrote it, so a
// human lead of a team sharing a tracker token failed validation with an
// error about a field nobody could remove.
func TestHumanMembersInheritNoToolCredentials(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name: "T",
		Units: []*Unit{{
			Name: "Engineering", Lead: "Sarah Chen",
			MCPEnv: MCPEnv{"tracker": {"TOKEN": "${TRACKER_TOKEN}"}},
			Roles:  []*Role{human(), {Name: "Dev"}},
		}},
	})
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil: the human seat authored no mcp_env", err)
	}
	if got := o.Role("Sarah Chen").MCPEnv; len(got) != 0 {
		t.Errorf("the human seat inherited %v", got)
	}
	if got := o.Role("Dev").MCPEnv["tracker"]["TOKEN"]; got != "${TRACKER_TOKEN}" {
		t.Errorf("the agent member inherited %q, want the unit credential", got)
	}

	// What a human seat WRITES is still refused: the fix is to stop
	// inventing the field, not to stop checking it.
	authored := normalized(&Organization{
		Name: "T",
		Units: []*Unit{{Name: "Engineering", Roles: []*Role{
			human(func(r *Role) { r.MCPEnv = MCPEnv{"tracker": {"TOKEN": "${MINE}"}} }),
		}}},
	})
	if err := authored.Validate(); !errors.Is(err, ErrHumanSeatField) {
		t.Errorf("Validate() = %v, want ErrHumanSeatField for an authored mcp_env", err)
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
		Units: []*Unit{{
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
		Units: []*Unit{{Name: "Backend", Roles: []*Role{{Name: "Lead"}}}},
	})
	if got := roleNames(o.Roles); !slices.Equal(got, []string{"Dev"}) {
		t.Errorf("root seats = %v, want the seat kept", got)
	}
	want := []DanglingRef{{Kind: RefUnit, From: "Dev", To: "Nowhere", Seat: o.Role("Dev")}}
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
			Units: []*Unit{{
				Name: "Engineering", Lead: "VP Eng", Channel: "C_ENG",
				MCPEnv: MCPEnv{"atlassian": {"JIRA_API_TOKEN": "${TEAM}"}},
				Roles:  []*Role{{Name: "VP Eng"}, {Name: "Analyst"}},
				Children: []*Unit{
					{
						Name:  "Backend",
						Roles: []*Role{{Name: "Tech Lead", Manages: []string{"Dev A"}}, {Name: "Dev A"}},
					},
					{
						// A unit reference that shields its members: a
						// second pass reads the expanded names and must
						// reach the same roster.
						Name: "Frontend", Lead: "Frontend Lead",
						Roles: []*Role{
							{Name: "Frontend Lead"},
							{Name: "Frontend Manager", Manages: []string{"Frontend"}},
							{Name: "Dev F"},
						},
					},
				},
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
	// The declared record is not part of the wire form, so the marshal
	// above cannot see it: a second pass that promoted an inherited lead
	// to a declared one would pass that comparison and still report
	// every descendant of a misspelled lead.
	onceUnits, twiceUnits := slices.Collect(once.AllUnits()), slices.Collect(twice.AllUnits())
	for i := range onceUnits {
		a, b := onceUnits[i], twiceUnits[i]
		if a.DeclaredLead != b.DeclaredLead || a.DeclaredChannel != b.DeclaredChannel {
			t.Errorf("unit %q declared lead %q channel %q after one pass, %q and %q after two",
				a.Name, a.DeclaredLead, a.DeclaredChannel, b.DeclaredLead, b.DeclaredChannel)
		}
	}
	if got := twice.Unit("Backend").DeclaredLead; got != "" {
		t.Errorf("Backend declared lead = %q after two passes, want none: it inherits", got)
	}
}

// ---- seat identity ---------------------------------------------------- //

func identityOrg() *Organization {
	return normalized(&Organization{
		Name:  "TestCo",
		Roles: []*Role{{Name: "Founder Bot", Goal: "oversee"}},
		Units: []*Unit{{
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
				o.Units = []*Unit{{Name: "Eng", Roles: []*Role{{Name: "tech lead"}}}}
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
		o := normalized(&Organization{Name: "T", Units: []*Unit{{
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
		o := normalized(&Organization{Name: "T", Units: []*Unit{{
			Name: "Dept", Lead: "Sarah Chen", Roles: []*Role{human()},
			Children: []*Unit{{
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
		o := normalized(&Organization{Name: "T", Units: []*Unit{{
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
		Units: []*Unit{{Name: "Eng", Children: []*Unit{{
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
		Units: []*Unit{{
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
      gitlab_username: "${GL_FOUNDER_USERNAME}"
  - name: Dev C
    unit: Backend
units:
  - name: Engineering
    type: department
    lead: CEO
    channel: C_ENG
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
	if got := founder.Contact.GitLabUsername; got != "${GL_FOUNDER_USERNAME}" {
		t.Errorf("gitlab_username = %q, want the reference verbatim", got)
	}
	if o.Unit("Backend").Role("Dev C") == nil {
		t.Error("the unit: reference did not move the seat")
	}
	if got := o.Unit("Backend").Channel; got != "C_ENG" {
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
		Units: []*Unit{{
			Name: "Engineering", Roles: []*Role{{Name: "VP Eng"}},
			Children: []*Unit{{Name: "Backend", Lead: "Dev", Roles: []*Role{{Name: "Dev"}}}},
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

// WHAT AUTO-MANAGEMENT ADDED IS RECORDED, and a second pass records nothing
// more. Once normalized, an entry the operator wrote and one the lead gained
// read identically in Manages, and a chart that tells a person which reports
// they wrote needs the difference.
func TestAutoManagementRecordsWhatItAdded(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Units: []*Unit{{
		Name: "Backend", Lead: "Lead",
		Roles: []*Role{
			{Name: "Lead", Manages: []string{"Written"}},
			{Name: "Written"},
			{Name: "Derived A"},
			{Name: "Derived B"},
		},
	}}})
	lead := o.Role("Lead")
	if want := []string{"Written", "Derived A", "Derived B"}; !slices.Equal(lead.Manages, want) {
		t.Errorf("Manages = %v, want %v", lead.Manages, want)
	}
	if want := []string{"Derived A", "Derived B"}; !slices.Equal(lead.AutoManaged, want) {
		t.Errorf("AutoManaged = %v, want %v", lead.AutoManaged, want)
	}
	o.Normalize()
	if want := []string{"Derived A", "Derived B"}; !slices.Equal(lead.AutoManaged, want) {
		t.Errorf("after a second pass AutoManaged = %v, want %v", lead.AutoManaged, want)
	}
}
