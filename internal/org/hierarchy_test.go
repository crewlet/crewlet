package org

import (
	"slices"
	"testing"
)

// hierarchyOrg is the shape most of these walks are interesting on: a
// department whose lead sits in one of its teams, and a second department
// whose team leads itself.
func hierarchyOrg() *Organization {
	return normalized(&Organization{
		Name: "Acme AI",
		Units: []*Unit{
			{
				Name: "Engineering", Type: UnitTypeDepartment, Lead: "VP Engineering",
				Children: []*Unit{{
					Name: "Backend", Type: UnitTypeTeam, Lead: "VP Engineering",
					Roles: []*Role{
						{Name: "VP Engineering", Manages: []string{"Tech Lead"}},
						{Name: "Tech Lead", Manages: []string{"Senior Engineer A", "Senior Engineer B", "Junior Engineer"}},
						{Name: "Senior Engineer A"},
						{Name: "Senior Engineer B"},
						{Name: "Junior Engineer"},
					},
				}},
			},
			{
				Name: "Product", Type: UnitTypeDepartment,
				Children: []*Unit{{
					Name: "PM Team", Type: UnitTypeTeam, Lead: "Product Manager",
					Roles: []*Role{{Name: "Product Manager"}},
				}},
			},
		},
	})
}

func TestManagerAndReports(t *testing.T) {
	t.Parallel()
	o := hierarchyOrg()
	for _, tc := range []struct {
		seat string
		want string // "" for no manager
	}{
		{"Senior Engineer A", "Tech Lead"},
		{"Junior Engineer", "Tech Lead"},
		{"Tech Lead", "VP Engineering"},
		{"VP Engineering", ""},
		{"Product Manager", ""},
	} {
		t.Run(tc.seat, func(t *testing.T) {
			t.Parallel()
			got := o.Manager(o.Role(tc.seat))
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("Manager(%s) = %q, want none", tc.seat, got.Name)
			case tc.want != "" && (got == nil || got.Name != tc.want):
				t.Errorf("Manager(%s) = %v, want %q", tc.seat, got, tc.want)
			}
		})
	}

	reports := roleNames(o.Reports(o.Role("Tech Lead")))
	slices.Sort(reports)
	want := []string{"Junior Engineer", "Senior Engineer A", "Senior Engineer B"}
	if !slices.Equal(reports, want) {
		t.Errorf("Reports(Tech Lead) = %v, want %v", reports, want)
	}
	if got := o.Reports(o.Role("Junior Engineer")); len(got) != 0 {
		t.Errorf("Reports(Junior Engineer) = %v, want none", roleNames(got))
	}
}

func TestReportsSkipsUnwiredNames(t *testing.T) {
	t.Parallel()
	// Half-wired is normal mid-bootstrap; the roster simply omits what has
	// not arrived.
	o := normalized(&Organization{Name: "T", Roles: []*Role{
		{Name: "CEO", Manages: []string{"Ghost", "CTO"}}, {Name: "CTO"},
	}})
	if got := roleNames(o.Reports(o.Role("CEO"))); !slices.Equal(got, []string{"CTO"}) {
		t.Errorf("Reports(CEO) = %v, want only CTO", got)
	}
}

func TestAncestorsClimbToTheTop(t *testing.T) {
	t.Parallel()
	o := hierarchyOrg()
	got := roleNames(o.Ancestors(o.Role("Junior Engineer")))
	if !slices.Equal(got, []string{"Tech Lead", "VP Engineering"}) {
		t.Errorf("Ancestors(Junior Engineer) = %v", got)
	}
	if got := o.Ancestors(o.Role("VP Engineering")); len(got) != 0 {
		t.Errorf("Ancestors(VP Engineering) = %v, want none", roleNames(got))
	}
}

// TestAncestorsSurviveACycle: a config can express a cycle, and a prompt
// builder that looped forever on one would take the whole turn with it.
func TestAncestorsSurviveACycle(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T", Roles: []*Role{
		{Name: "A", Manages: []string{"B"}},
		{Name: "B", Manages: []string{"C"}},
		{Name: "C", Manages: []string{"A"}},
	}})
	got := roleNames(o.Ancestors(o.Role("A")))
	if !slices.Equal(got, []string{"C", "B"}) {
		t.Errorf("Ancestors(A) = %v, want the chain to stop at the repeat", got)
	}
}

func TestAncestorsClimbThroughARootSeat(t *testing.T) {
	t.Parallel()
	// Management is stored on the manager, which is what lets a root-level
	// seat manage into a unit with no entry anywhere near it.
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "CEO", Manages: []string{"VP"}}},
		Units: []*Unit{{Name: "Eng", Lead: "VP", Roles: []*Role{
			{Name: "VP", Manages: []string{"Dev"}}, {Name: "Dev"},
		}}},
	})
	got := roleNames(o.Ancestors(o.Role("Dev")))
	if !slices.Equal(got, []string{"VP", "CEO"}) {
		t.Errorf("Ancestors(Dev) = %v", got)
	}
}

func TestUnitForAndUnitChain(t *testing.T) {
	t.Parallel()
	o := hierarchyOrg()
	for _, tc := range []struct {
		seat      string
		wantUnit  string
		wantChain []string
	}{
		{"Senior Engineer A", "Backend", []string{"Engineering", "Backend"}},
		{"Product Manager", "PM Team", []string{"Product", "PM Team"}},
	} {
		t.Run(tc.seat, func(t *testing.T) {
			t.Parallel()
			seat := o.Role(tc.seat)
			unit := o.UnitFor(seat)
			if unit == nil || unit.Name != tc.wantUnit {
				t.Errorf("UnitFor(%s) = %v, want %q", tc.seat, unit, tc.wantUnit)
			}
			var chain []string
			for _, u := range o.UnitChainFor(seat) {
				chain = append(chain, u.Name)
			}
			if !slices.Equal(chain, tc.wantChain) {
				t.Errorf("UnitChainFor(%s) = %v, want %v", tc.seat, chain, tc.wantChain)
			}
		})
	}
}

func TestUnitChainReachesEveryLevel(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "BigCorp", Units: []*Unit{{
		Name: "Technology", Type: UnitTypeDivision,
		Children: []*Unit{{
			Name: "Engineering", Type: UnitTypeDepartment,
			Children: []*Unit{{
				Name: "Platform", Lead: "Platform Lead",
				Roles: []*Role{{Name: "Platform Lead"}, {Name: "Platform Dev"}},
			}},
		}},
	}}})
	var chain []string
	for _, u := range o.UnitChainFor(o.Role("Platform Dev")) {
		chain = append(chain, u.Name)
	}
	if !slices.Equal(chain, []string{"Technology", "Engineering", "Platform"}) {
		t.Errorf("UnitChainFor = %v, want outermost first", chain)
	}
}

func TestRootSeatHasNoUnit(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{
		Name:  "T",
		Roles: []*Role{{Name: "CEO", Manages: []string{"Dev"}}},
		Units: []*Unit{{Name: "Team", Lead: "Dev", Roles: []*Role{{Name: "Dev"}}}},
	})
	ceo := o.Role("CEO")
	if got := o.UnitFor(ceo); got != nil {
		t.Errorf("UnitFor(CEO) = %v, want nil", got)
	}
	if got := o.UnitChainFor(ceo); got != nil {
		t.Errorf("UnitChainFor(CEO) = %v, want none", got)
	}
	if o.IsUnitLead(ceo) || o.LeadDepth(ceo) != -1 {
		t.Error("a root seat reads as a unit lead")
	}
}

func TestLeadDepthRanksAuthority(t *testing.T) {
	t.Parallel()
	o := hierarchyOrg()
	// VP Engineering leads the department AND the team inside it; the
	// wider authority is the one that matters.
	if got := o.LeadDepth(o.Role("VP Engineering")); got != 0 {
		t.Errorf("LeadDepth(VP Engineering) = %d, want 0", got)
	}
	if got := o.LeadDepth(o.Role("Product Manager")); got != 1 {
		t.Errorf("LeadDepth(Product Manager) = %d, want 1", got)
	}
	if got := o.LeadDepth(o.Role("Junior Engineer")); got != -1 {
		t.Errorf("LeadDepth(Junior Engineer) = %d, want -1", got)
	}
	if !o.IsUnitLead(o.Role("VP Engineering")) || o.IsUnitLead(o.Role("Junior Engineer")) {
		t.Error("IsUnitLead disagrees with LeadDepth")
	}
}

func TestEffectiveLeadResolvesEveryPlacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		org  *Organization
		unit string
		want string
	}{
		{
			name: "a direct member",
			org: &Organization{Name: "T", Units: []*Unit{{
				Name: "Backend", Lead: "Lead", Roles: []*Role{{Name: "Lead"}, {Name: "Dev"}},
			}}},
			unit: "Backend", want: "Lead",
		},
		{
			name: "a seat in a descendant unit",
			org: &Organization{Name: "T", Units: []*Unit{{
				Name: "Engineering", Lead: "Dev Lead",
				Children: []*Unit{{Name: "Backend", Roles: []*Role{{Name: "Dev Lead"}, {Name: "Dev"}}}},
			}}},
			unit: "Engineering", want: "Dev Lead",
		},
		{
			// Inherited leads live OUTSIDE the unit's own subtree, which is
			// the case the unit-local lookup cannot answer.
			name: "a lead inherited from an ancestor",
			org: &Organization{Name: "T", Units: []*Unit{{
				Name: "Engineering", Lead: "VP Eng", Roles: []*Role{{Name: "VP Eng"}},
				Children: []*Unit{{Name: "Backend", Roles: []*Role{{Name: "Dev A"}}}},
			}}},
			unit: "Backend", want: "VP Eng",
		},
		{
			name: "no lead anywhere",
			org: &Organization{Name: "T", Units: []*Unit{{
				Name: "Backend", Roles: []*Role{{Name: "Dev"}},
			}}},
			unit: "Backend", want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := normalized(tc.org)
			got := o.EffectiveLead(o.Unit(tc.unit))
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("EffectiveLead(%s) = %q, want none", tc.unit, got.Name)
			case tc.want != "" && (got == nil || got.Name != tc.want):
				t.Errorf("EffectiveLead(%s) = %v, want %q", tc.unit, got, tc.want)
			}
		})
	}
}

func TestInheritedLeadIsAFullLead(t *testing.T) {
	t.Parallel()
	// Nothing downstream distinguishes an inherited lead from a declared
	// one — that is the point of the cascade.
	o := normalized(&Organization{Name: "T", Units: []*Unit{{
		Name: "Engineering", Lead: "VP Eng", Roles: []*Role{{Name: "VP Eng"}},
		Children: []*Unit{{Name: "Backend", Roles: []*Role{{Name: "Dev"}}}},
	}}})
	vp := o.Role("VP Eng")
	backend := o.Unit("Backend")
	if !o.IsUnitLead(vp) {
		t.Error("an inherited lead does not read as a unit lead")
	}
	if !backend.IsLedBy(vp) {
		t.Error("the child unit does not report its inherited lead")
	}
	if !slices.Contains(vp.Manages, "Dev") {
		t.Errorf("the inherited lead does not manage the child's members: %v", vp.Manages)
	}
}

func TestUnitAccessors(t *testing.T) {
	t.Parallel()
	o := hierarchyOrg()
	eng := o.Unit("Engineering")
	if got := eng.Child("Backend"); got == nil || got.Name != "Backend" {
		t.Errorf("Child(Backend) = %v", got)
	}
	if got := eng.Child("Nowhere"); got != nil {
		t.Errorf("Child(Nowhere) = %v, want nil", got)
	}
	backend := o.Unit("Backend")
	if got := backend.Role("Tech Lead"); got == nil {
		t.Error("Role(Tech Lead) = nil")
	}
	if got := backend.Role("Nobody"); got != nil {
		t.Errorf("Role(Nobody) = %v, want nil", got)
	}
	// A unit's own walk covers its descendants.
	if got := len(roleNames(slices.Collect(eng.AllRoles()))); got != 5 {
		t.Errorf("Engineering AllRoles() covered %d seats, want 5", got)
	}
	if eng.FindRole("Junior Engineer") == nil {
		t.Error("FindRole does not reach a descendant")
	}
	if eng.FindUnit("Engineering") != eng {
		t.Error("FindUnit does not include the unit itself")
	}
	if eng.FindRole("Nobody") != nil || eng.FindUnit("Nowhere") != nil {
		t.Error("a missing name resolved to something")
	}
	// A unit whose lead names nobody in its own subtree resolves to no
	// lead; only the org-wide walk can answer an inherited one.
	product := o.Unit("Product")
	if product.Lead != "" || product.LeadRole() != nil {
		t.Errorf("Product lead = %q / %v, want none", product.Lead, product.LeadRole())
	}
}
