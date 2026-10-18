package artifacts

import "testing"

// THE LADDER IS ITS DECLARED ORDER, AND EVERY ROLE INCLUDES EXACTLY ITSELF AND
// WHAT SITS BELOW IT.
//
// Walked over every ordered pair of [Roles] rather than a handful of cases, so
// a role inserted into the ladder is ranked by where it was inserted and by
// nothing else. Mutation: rank owner below editor and the pair (editor, owner)
// is admitted; compare with > instead of >= and no role includes itself.
func TestEveryRoleIncludesItselfAndEveryRoleBelow(t *testing.T) {
	t.Parallel()
	for i, r := range Roles {
		if r.Rank() != i+1 {
			t.Errorf("%s ranks %d, want %d: the rank is the ladder's own order",
				r, r.Rank(), i+1)
		}
		for j, other := range Roles {
			if got, want := r.Includes(other), i >= j; got != want {
				t.Errorf("%s includes %s = %v, want %v", r, other, got, want)
			}
		}
	}
	if len(Roles) != 5 || Roles[0] != RoleViewer || Roles[4] != RoleOwner {
		t.Fatalf("the ladder is %v, want viewer up to owner", Roles)
	}
}

// AN UNKNOWN ROLE INCLUDES NOTHING AND IS INCLUDED BY NOTHING.
//
// The zero is what a forgotten field holds and a name off a newer peer is what
// a rolling upgrade delivers, and both must fail closed on BOTH sides: holding
// one admits no row, and requiring one admits nobody. Mutation: let Rank answer
// a positive number for an unknown role and both directions go red.
func TestAnUnknownRoleIncludesNothingAndIsIncludedByNothing(t *testing.T) {
	t.Parallel()
	for _, unknown := range []Role{"", "admin", "Owner"} {
		if unknown.Rank() != 0 || unknown.Valid() {
			t.Errorf("%q ranks %d (valid %v), want 0 and invalid", unknown,
				unknown.Rank(), unknown.Valid())
		}
		for _, r := range append([]Role{unknown}, Roles...) {
			if unknown.Includes(r) {
				t.Errorf("an unknown role %q includes %q", unknown, r)
			}
			if r.Includes(unknown) {
				t.Errorf("%q includes an unknown role %q", r, unknown)
			}
		}
	}
}

// THE BEST OF TWO ROLES IS THE HIGHER, AND AN UNKNOWN ROLE NEVER WINS.
func TestTheBestOfTwoRolesIsTheHigher(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ a, b, want Role }{
		{RoleViewer, RoleEditor, RoleEditor},
		{RoleOwner, RoleCommenter, RoleOwner},
		{RoleContributor, RoleContributor, RoleContributor},
		{"", RoleViewer, RoleViewer},
		{RoleViewer, "", RoleViewer},
		{"admin", RoleCommenter, RoleCommenter},
		{"", "", ""},
	} {
		if got := Best(c.a, c.b); got != c.want {
			t.Errorf("Best(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}
