package appstoreconnect_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE FORM ASKS FOR THE ISSUER ID ALONE: each agent hands in its own key on
// the Agents tab, so the company's form holds the hidden toggle and one issuer
// ID per team, required, and nothing else.
//
// Mutation: add a key or vendor field back to Requirements, and the form asks
// the company for a setting no call uses.
func TestTheFormAsksForTheIssuerIDAlone(t *testing.T) {
	t.Parallel()
	reqs := appstoreconnect.Requirements(nil, nil)
	if len(reqs) != 2 || reqs[0].Field != "enabled" || !reqs[0].Hidden {
		t.Fatalf("the form holds %+v, want the hidden toggle and the issuer ID", reqs)
	}
	issuer := reqs[1]
	if issuer.ConfigPath != "integrations.app_store_connect.connections.main.issuer_id" || !issuer.Required ||
		issuer.Shape != setup.ShapeUUID || issuer.Hint == "" || issuer.More != "" {
		t.Errorf("the issuer ID field is %+v, want a required UUID with one line of help", issuer)
	}
	if out := setup.Outstanding(reqs); !slices.ContainsFunc(out, func(r setup.Requirement) bool {
		return r.Field == issuer.Field
	}) {
		t.Errorf("a team with no issuer has %+v outstanding, want the issuer ID", out)
	}
}
