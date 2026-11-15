package setupapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// ONE AGENT'S LANE IS READ IN WORDS, by the engine's view of the same answer
// the tab reads: the grant by the vendor's level name and the status word. An
// agent the integration does not list is refused naming the ones it does, and
// a lane the kind does not grant is refused by name. Mutation: answer the raw
// lane, or answer a seat the answer does not list, and this fails.
func TestOneAgentsLaneIsReadInWords(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	res := s.do(t, http.MethodGet, "/setup/integrations/datadog/access/seats/sre-lead/datadog", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", res.Code, res.Body)
	}
	var view seataccess.LaneView
	if err := json.Unmarshal(res.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Handle != "sre-lead" || view.Name != "SRE Lead" || view.Integration != "Datadog" ||
		view.Grant != "Datadog Standard Role" || view.Status != seataccess.StatusInSync ||
		view.StatusLabel != "In sync" || view.Holds != "No account yet" {
		t.Fatalf("view = %+v", view)
	}

	for path, want := range map[string]httpjson.Code{
		"/setup/integrations/datadog/access/seats/nobody/datadog": httpjson.CodeNoSuchSeat,
		"/setup/integrations/datadog/access/seats/owner/datadog":  httpjson.CodeNoSuchSeat,
		"/setup/integrations/datadog/access/seats/sre-lead/jira":  httpjson.CodeNoSeatAccess,
	} {
		res := s.do(t, http.MethodGet, path, "", nil)
		if res.Code != http.StatusNotFound || decode(t, res)["error"] != string(want) {
			t.Errorf("GET %s = %d %s, want 404 %s", path, res.Code, res.Body, want)
		}
		if want == httpjson.CodeNoSuchSeat && !strings.Contains(res.Body.String(), "sre-lead") {
			t.Errorf("GET %s names none of the agents listed: %s", path, res.Body)
		}
	}
}

// ONE AGENT'S LANE CARRIES ITS OWN OPEN REQUESTS AND A LOOKUP, and an over-long
// lookup is refused. Mutation: drop the requests or the lookup and this fails.
func TestOneAgentsLaneCarriesItsRequestsAndALookup(t *testing.T) {
	t.Parallel()
	s := newCloudflareSurface(t)
	s.access.requests = map[string]seataccess.GrantRequest{
		"abcde12345": {ID: "abcde12345", Kind: integration.KindCloudflare, Lane: seataccess.LaneCloudflare,
			Handle: "sre", Permission: "Origin Write", Scope: []string{"example.com"}, Hours: 4,
			Reason: "add the origin rule", Item: "ENG-58", Ask: "owner"},
		"fghij67890": {ID: "fghij67890", Kind: integration.KindCloudflare, Lane: seataccess.LaneCloudflare,
			Handle: "cto", Permission: "Origin Read", Hours: 4, Reason: "read", Item: "ENG-59", Ask: "owner"},
	}
	res := s.do(t, http.MethodGet, "/setup/integrations/cloudflare/access/seats/sre/cloudflare?permission=dns", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", res.Code, res.Body)
	}
	var view seataccess.LaneView
	if err := json.Unmarshal(res.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Requests, "request abcde12345 asks for Origin Rules: Edit on example.com") ||
		!strings.Contains(view.Requests, "grant-rabcde12345-t") || strings.Contains(view.Requests, "fghij67890") {
		t.Errorf("requests = %q, want the SRE's own request and its options", view.Requests)
	}
	if !strings.Contains(view.Permissions, "Origin Rules: Edit (Zone, changes things, in no level)") {
		t.Errorf("permissions = %q, want the requested permission worded", view.Permissions)
	}
	if len(view.Lookup) != 1 || view.Lookup[0].Name != "DNS Write" ||
		view.Lookup[0].Grantable != "The provisioner token does not hold DNS Write" ||
		view.Lookup[0].Missing != "DNS Write" {
		t.Errorf("lookup = %+v, want DNS Write and what the connection lacks", view.Lookup)
	}

	long := strings.Repeat("x", seataccess.MaxLookupChars+1)
	res = s.do(t, http.MethodGet, "/setup/integrations/cloudflare/access/seats/sre/cloudflare?permission="+long, "", nil)
	if res.Code != http.StatusBadRequest || decode(t, res)["error"] != string(httpjson.CodeInvalidQuery) {
		t.Errorf("a lookup past %d characters = %d %s, want 400 invalid_query", seataccess.MaxLookupChars,
			res.Code, res.Body)
	}
}
