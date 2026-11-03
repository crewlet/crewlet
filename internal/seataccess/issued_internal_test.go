package seataccess

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// issuedOnly is a catalog whose one lane's credential the engine issues at use
// and never seals, as a cloud's hour-long access token is.
var issuedOnly = Catalog{
	Kind:  integration.KindDatadog,
	Lanes: []LaneSpec{{Lane: LaneDatadog, Issued: "ACCESS_TOKEN"}},
}

// AN ISSUED CREDENTIAL IS THE SEAT'S WHILE IT IS ON, named by its variable,
// with no mint time: each node issues its own, so a time would be the
// answering node's and no fleet fact. A seat off, a seat with no account, and
// a lane whose credential is sealed report none.
func TestAnIssuedLaneReportsItsVariableAndNoMintTime(t *testing.T) {
	t.Parallel()
	on, made := Desired{Enabled: true}, Observed{Exists: true, Enabled: true}
	got := issuedCredentialOf(issuedOnly, LaneDatadog, "qa", on, made)
	if got == nil || got.Variable != "DATADOG_ACCESS_TOKEN_QA" || got.Source != CredentialIssued {
		t.Fatalf("credential = %+v, want DATADOG_ACCESS_TOKEN_QA, issued", got)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(wire), "minted_at") {
		t.Errorf("an issued credential is sent with a mint time: %s", wire)
	}
	for name, c := range map[string]struct {
		cat  Catalog
		want Desired
		have Observed
	}{
		"the seat off":    {issuedOnly, Desired{}, made},
		"no account yet":  {issuedOnly, on, Observed{}},
		"a sealed lane":   {Catalog{Kind: integration.KindDatadog, Credentials: []string{"APP_KEY"}, Lanes: []LaneSpec{{Lane: LaneDatadog}}}, on, made},
		"a lane not held": {Catalog{Kind: integration.KindDatadog}, on, made},
	} {
		if got := issuedCredentialOf(c.cat, LaneDatadog, "qa", c.want, c.have); got != nil {
			t.Errorf("%s: credential = %+v, want none", name, got)
		}
	}
}

// A CATALOG MAY SEAL NOTHING WHEN EVERY LANE'S CREDENTIAL IS ISSUED, and is
// refused where a lane holds no credential at all, or issues a field it also
// seals, which would show one credential twice.
func TestACatalogHoldsACredentialOnEveryLane(t *testing.T) {
	t.Parallel()
	if problem := catalogProblem(integration.KindDatadog, issuedOnly); problem != "" {
		t.Fatalf("a catalog whose every lane is issued is refused: %s", problem)
	}
	for name, c := range map[string]struct {
		cat  Catalog
		says string
	}{
		"no credential at all": {Catalog{Kind: integration.KindDatadog,
			Lanes: []LaneSpec{{Lane: LaneDatadog}}}, "names no credential field"},
		"an issued field it also seals": {Catalog{Kind: integration.KindDatadog, Credentials: []string{"TOKEN"},
			Lanes: []LaneSpec{{Lane: LaneDatadog, Issued: "TOKEN"}}}, "one of its sealed fields"},
		"a blank-padded issued field": {Catalog{Kind: integration.KindDatadog,
			Lanes: []LaneSpec{{Lane: LaneDatadog, Issued: " TOKEN"}}}, "blank-padded"},
	} {
		if problem := catalogProblem(integration.KindDatadog, c.cat); !strings.Contains(problem, c.says) {
			t.Errorf("%s: the problem is %q, want it to say %q", name, problem, c.says)
		}
	}
}

// ONLY THE FIRST SEALED FIELD IS THE CREDENTIAL, and a catalog that seals
// nothing has none, so a revoke or a forget on it never reads past its end.
func TestTheCredentialIsTheFirstSealedField(t *testing.T) {
	t.Parallel()
	sealed := Catalog{Credentials: []string{"BOT_TOKEN", "SIGNING_SECRET"}}
	if !sealed.isCredential("BOT_TOKEN") || sealed.isCredential("SIGNING_SECRET") {
		t.Error("the credential is not the first sealed field alone")
	}
	if issuedOnly.isCredential("ACCESS_TOKEN") {
		t.Error("a catalog that seals nothing named an issued field its sealed credential")
	}
}
