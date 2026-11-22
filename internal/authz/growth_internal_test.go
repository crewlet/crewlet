package authz

import (
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
)

// A NAME THE TABLE AND A GROWTH CATALOG BOTH CLAIM IS NO VERB AT ALL: either
// row would decide the other's call, so neither does. A name only one claims
// is that one's.
//
// Mutation: answer the table's row first, as a lookup that stops at the table
// would, and the claimed name is decided by everybody's row.
func TestANameTheTableAndACatalogBothClaimIsNoVerb(t *testing.T) {
	t.Parallel()
	everybody := rule{class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead}
	table := map[Action]rule{"claimed": everybody, "tabled": everybody}
	grown := rule{class: ClassAdmin, seatGrant: "shop_read", recency: iam.RecencyAny, assist: AssistRead}
	growthOf := func(a Action) (rule, bool) { return grown, a == "claimed" || a == "grown" }
	for _, c := range []struct {
		name  Action
		known bool
		class Class
	}{
		{"claimed", false, ""},
		{"tabled", true, ClassEverybody},
		{"grown", true, ClassAdmin},
		{"nothing", false, ""},
	} {
		r, known := ruleIn(table, growthOf, c.name)
		if known != c.known || r.class != c.class {
			t.Errorf("%s: known %v as %q, want %v as %q", c.name, known, r.class, c.known, c.class)
		}
	}
}
