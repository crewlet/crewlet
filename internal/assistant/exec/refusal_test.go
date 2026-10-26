package exec_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// answered is what a route writing through write answers, as the dispatcher
// hands it back.
func answered(write func(http.ResponseWriter)) exec.Response {
	rec := httptest.NewRecorder()
	write(rec)
	return exec.Response{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}
}

// A REFUSAL READS AS THE API SAID IT, ON TWO CHANNELS: the code, the sentence
// the envelope says to a person, the grants that would have admitted them, and
// the route's own account of this request as the detail only the model reads;
// a body that is not the envelope is a refusal by its status alone. Mutation:
// read the detail into the message, or drop the grants, and a row fails.
func TestARefusalReadsAsTheAPISaidIt(t *testing.T) {
	t.Parallel()
	deny := func(reason authz.Reason, grants ...iam.Grant) exec.Response {
		return answered(func(w http.ResponseWriter) {
			httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized,
				authz.RefusalDetail(reason, grants))
		})
	}
	unauthorized := httpjson.CodeUnauthorized.Message()
	cases := []struct {
		name string
		resp exec.Response
		want wire.Refusal
	}{
		{"an authority deny", deny(authz.ReasonNoGrant, iam.GrantWorklet),
			wire.Refusal{Code: "unauthorized", Message: unauthorized, Grants: []string{"worklet"}}},
		{"the assistant's scope", deny(authz.ReasonAssistScope),
			wire.Refusal{Code: "unauthorized", Message: unauthorized, Grants: []string{}}},
		{"a verb never the assistant's", deny(authz.ReasonAssistRefused),
			wire.Refusal{Code: "unauthorized", Message: unauthorized, Grants: []string{}}},
		{"a 401", answered(func(w http.ResponseWriter) {
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		}), wire.Refusal{Code: "invalid_token", Message: httpjson.CodeInvalidToken.Message()}},
		{"a 404", answered(func(w http.ResponseWriter) {
			httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		}), wire.Refusal{Code: "not_found", Message: httpjson.CodeNotFound.Message()}},
		{"a 409 with its own sentence and hint", answered(func(w http.ResponseWriter) {
			httpjson.FailWith(w, http.StatusConflict, httpjson.CodeProposalStale,
				map[string]string{"detail": "the task moved to version 9", "hint": "ask again"})
		}), wire.Refusal{Code: "proposal_stale", Message: httpjson.CodeProposalStale.Message(),
			Hint: httpjson.Sentence("ask again"), Detail: httpjson.Sentence("the task moved to version 9")}},
		{"a body that is not the envelope",
			exec.Response{Status: http.StatusBadGateway, Body: []byte("<html>bad gateway</html>")},
			wire.Refusal{Code: "http_502", Message: http.StatusText(http.StatusBadGateway)}},
		{"no body", exec.Response{Status: http.StatusForbidden},
			wire.Refusal{Code: "http_403", Message: http.StatusText(http.StatusForbidden)}},
	}
	for _, tc := range cases {
		got := exec.RefusalOf(tc.resp)
		if got.Code != tc.want.Code || got.Message != tc.want.Message || got.Hint != tc.want.Hint ||
			got.Detail != tc.want.Detail || !slices.Equal(got.Grants, tc.want.Grants) {
			t.Errorf("%s read as %+v, want %+v", tc.name, got, tc.want)
		}
		if got.Message == "" {
			t.Errorf("%s reads as a refusal with no sentence", tc.name)
		}
		if want := tc.want.Detail; want != "" && got.ForModel() != want {
			t.Errorf("%s reads to the model as %q, want the route's own %q", tc.name,
				got.ForModel(), want)
		}
	}
}
