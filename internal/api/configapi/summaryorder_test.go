package configapi_test

import (
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// A WRITE THE CALLER MAY NOT MAKE IS REFUSED FOR THAT, NOT FOR ITS SUMMARY.
//
// Every configuration write that stores a revision needs an audit summary, and
// it was asked for as the body was read — before the write was admitted. So a
// person bound to a seat, who passes the route because they may lead somebody,
// sending a change they could never make was answered `400 summary_required`,
// and met the `403` that was the real answer only once they had written a
// sentence for it. The summary is asked last now. The CONTROL is the company's
// own grant sending the same change, which is admitted and then asked for its
// summary. Mutation: ask for the summary as the body is read again and every
// refusal case answers 400.
func TestAWriteTheCallerMayNotMakeIsRefusedForThatNotForItsSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"a patch", http.MethodPatch, "/config", `{"mission": "x"}`},
		{"a whole document", http.MethodPut, "/config", string(companyJSON(t, leadDoc))},
		{"a unit they do not lead", http.MethodPut,
			"/config/" + configapi.EntityUnits + "/design", `{"name": "Design", "id": "design"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := leadSurface(t)
			res := doAs(t, s, platformLead(), tc.method, tc.path, tc.body, nil)
			if res.Code != http.StatusForbidden ||
				decode(t, res)["error"] != string(httpjson.CodeUnauthorized) {
				t.Errorf("a write the caller may not make answered %d: %s",
					res.Code, res.Body.String())
			}
		})
	}
	t.Run("the company's grant (the control)", func(t *testing.T) {
		t.Parallel()
		s := leadSurface(t)
		res := s.do(t, http.MethodPatch, "/config", `{"mission": "x"}`, nil)
		if res.Code != http.StatusBadRequest ||
			decode(t, res)["error"] != string(httpjson.CodeSummaryRequired) {
			t.Errorf("an admitted write with no summary answered %d: %s",
				res.Code, res.Body.String())
		}
	})
}
