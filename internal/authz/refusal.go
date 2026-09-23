package authz

import (
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
)

// The two keys a refusal on authority carries beside the envelope's own
// `error` and `message`. Named once, because every surface that refuses on
// authority writes them and a client reads them from all of them: a screen
// that learned `missing_grant` from one route and `grants` from another would
// branch twice on one fact.
const (
	// DetailReason is the rule's own [Reason].
	DetailReason = "reason"
	// DetailGrants is [Decision.Grants]: the capabilities any one of which
	// would have admitted the caller. Always present, and an EMPTY list is
	// an answer — no capability would, and what is missing is a relation
	// the chart does not hold — rather than an omission.
	DetailGrants = "grants"
)

// RefusalDetail is the machine-readable half of a refusal the authority table
// made: the reason and the grants, under the keys every surface answers them
// with.
//
// VALUES, NOT A SENTENCE. "This needs fleet:operate" written at the call site
// is a second statement of the rule, and it is the one that goes stale the day
// the rule's grant moves; the grants here are what the deciding rule actually
// consulted. The sentence a person reads is the envelope's `message`, which
// belongs to the code.
//
// A pair rather than a [Decision], because not every refusal is one: a
// question's own declared grant is checked by internal/api/queries' registry,
// which has a reason and a grant to report and no decision to hand over.
func RefusalDetail(reason Reason, grants []iam.Grant) httpjson.Detail {
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		names = append(names, string(g))
	}
	return httpjson.Detail{DetailReason: string(reason), DetailGrants: names}
}
