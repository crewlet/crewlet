package artifacts

import "slices"

// THE LADDER'S ORDERING, owned by the authority table that compares roles
// (internal/authz). The ladder itself, [Roles], is declared once in types.go in
// ascending order; everything here is derived from that one declaration.

// Rank is r's place on the ladder: 1 for viewer up to 5 for owner, and 0 for a
// role this build does not know, the zero included.
//
// THE POSITION IN [Roles], never a second table of numbers beside it: the
// ladder is declared once, in ascending order, and a rank derived from it
// cannot disagree with it. Zero for an unknown role is what makes every
// comparison below fail closed: a role a newer peer wrote, or a field nobody
// filled in, outranks nothing and is outranked by every real role.
func (r Role) Rank() int { return slices.Index(Roles, r) + 1 }

// Includes reports whether holding r admits everything other admits: r is on
// the ladder, other is on the ladder, and r sits at or above it.
//
// AN UNKNOWN ROLE ON EITHER SIDE IS NO, and that is the direction that fails
// safe: a role nobody filled in includes nothing, so a caller holding it is
// admitted to no row, and a requirement nobody filled in is included by
// nothing, so a row that forgot to state its role admits nobody rather than
// everybody.
func (r Role) Includes(other Role) bool {
	return r.Rank() > 0 && other.Rank() > 0 && r.Rank() >= other.Rank()
}

// Best is the higher of a and b on the ladder, a role this build does not know
// counting below every real one: the role a principal holds when two relations
// each grant one.
func Best(a, b Role) Role {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}
