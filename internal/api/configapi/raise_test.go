package configapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// raiseDoc is a company with Datadog provisioned and the SRE on it at the
// Standard Role.
const raiseDoc = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"datadog": {"enabled": true, "route_to": "sre",
    "webhook_token": "${DATADOG_WEBHOOK_TOKEN}", "provisioning": {"site": "datadoghq.com",
    "api_key": "${DATADOG_API_KEY}", "app_key": "${DATADOG_APP_KEY}"}}},
  "roles": [
    {"name": "SRE", "handle": "sre", "llm": "zulu",
     "integrations": {"datadog": {"enabled": true, "role": "role-std"}}},
    {"name": "Owner", "handle": "owner", "kind": "human"}
  ]
}`

// adminToken is an administrator's machine token, fresh: everything a person
// at a keyboard is, but present.
func adminToken() iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("ci"), Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour), Via: iam.MachineTokenPrefix + "ci"}
}

// adminPerson is an administrator at a keyboard, inside the step-up window.
func adminPerson() iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: "ana.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour)}
}

// toRole sets the SRE's Datadog role on a seat entity.
func toRole(level string) func(map[string]any) {
	return func(entity map[string]any) {
		in, _ := entity["integrations"].(map[string]any)
		dd, _ := in["datadog"].(map[string]any)
		dd["role"] = level
	}
}

// A RAISE WRITTEN THROUGH THE DOCUMENT IS A PERSON'S, AS IT IS ON THE TAB (D10).
//
// The SRE moved to another Datadog role through its seat's own write: with no
// vendor to say the role does not administer, the write is judged a raise, so
// an administrator's machine token is refused it (naming the seat's lane) and
// nothing is written, while the same write from a person at a keyboard lands.
// The token's check is answered, since it writes nothing, and a write that
// leaves the seat's access as it was is the token's to make. Mutation: drop the
// raise check from the document's writes, or ask it of a check, and a case
// here fails.
func TestARaiseThroughTheDocumentNeedsAPerson(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	base := s.seed(t, raiseDoc)

	res := putEntityAs(t, s, adminToken(), "roles", "sre", toRole("role-admin"), "")
	body := decode(t, res)
	if res.Code != http.StatusForbidden || body["error"] != string(httpjson.CodeTokenRefused) {
		t.Fatalf("a token's raise = %d %v, want 403 token_refused", res.Code, body)
	}
	if raises, _ := body["raises"].([]any); len(raises) != 1 {
		t.Errorf("the refusal names %v, want the SRE's Datadog", body["raises"])
	}
	if rev, _, _ := s.active(t.Context()); rev.ID != base {
		t.Fatal("a refused raise wrote a revision")
	}
	if res := putEntityAs(t, s, adminToken(), "roles", "sre", toRole("role-admin"),
		"?dry_run=true"); res.Code/100 != 2 {
		t.Errorf("a token's check of a raise = %d %s, want it answered", res.Code, res.Body)
	}
	if res := putEntityAs(t, s, adminToken(), "roles", "sre", func(e map[string]any) {
		e["name"] = "Site Reliability"
	}, ""); res.Code/100 != 2 {
		t.Errorf("a token's write that raises nobody = %d %s", res.Code, res.Body)
	}
	if res := putEntityAs(t, s, adminPerson(), "roles", "sre", toRole("role-admin"),
		""); res.Code/100 != 2 {
		t.Errorf("a person's raise = %d %s, want it written", res.Code, res.Body)
	}
}

// THE ENGINE'S OWN DECIDER JUDGES A WRITE WHERE IT IS GIVEN ONE.
//
// A decider that reads the vendor and finds the role ordinary admits the
// token's move, and one that finds it administering refuses it: what the
// surface decides by is the decider it was built with, and it is handed both
// documents. Mutation: ignore Options.Raises, and a case here fails.
func TestTheSurfaceAsksTheDeciderItIsGiven(t *testing.T) {
	t.Parallel()
	var seen [2]*config.Company
	ordinary := func(_ context.Context, prior *config.Company, _ string,
		next *config.Company) []seataccess.Raise {
		seen = [2]*config.Company{prior, next}
		return nil
	}
	s := newSurfaceWith(t, func(o *configapi.Options) { o.Raises = ordinary })
	s.seed(t, raiseDoc)
	if res := putEntityAs(t, s, adminToken(), "roles", "sre", toRole("role-ro"),
		""); res.Code/100 != 2 {
		t.Fatalf("a move the decider finds ordinary = %d %s", res.Code, res.Body)
	}
	if seen[0] == nil || seen[1] == nil {
		t.Fatal("the decider was not handed both documents")
	}
}
