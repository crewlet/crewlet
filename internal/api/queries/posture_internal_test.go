package queries

import (
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// EACH THING A ROW CAN ASK OF A PERSON KEEPS A QUESTION OFF IT, ON ITS OWN.
//
// The authority table has no row today that needs a person present without
// also asking a proof, so a registration of a real verb can never show the
// presence clause doing anything: it is held here over values, clause by
// clause, with a row that asks nothing as the control.
//
// Mutation: delete any one clause from demands and its case comes back empty.
func TestEachThingARowAsksOfAPersonKeepsAQuestionOffIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		recency, own iam.Recency
		presence     authz.Presence
		asks         bool
	}{
		"a proof of everybody":        {recency: iam.RecencyStepUp, asks: true},
		"a proof of its self arm":     {recency: iam.RecencyAny, own: iam.RecencyStepUp, asks: true},
		"a person present":            {recency: iam.RecencyAny, presence: authz.PresenceRequired, asks: true},
		"a person present for others": {recency: iam.RecencyAny, presence: authz.PresenceRequiredForOthers, asks: true},
		"nothing":                     {recency: iam.RecencyAny, own: iam.RecencyAny},
	} {
		if got := demands(tc.recency, tc.own, tc.presence); (got != "") != tc.asks {
			t.Errorf("%s: demands = %q, want asking %t", name, got, tc.asks)
		}
	}
}
