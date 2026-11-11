package setupapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
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
