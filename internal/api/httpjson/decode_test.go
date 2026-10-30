package httpjson_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY REFUSAL THE ENVELOPE WRITES DECODES BACK TO ITS VALUES, through the
// same keys: a bare code, a text detail and hint, an authority refusal with
// its reason and grants, a step-up with its window. Mutation: spell a key in
// Decode other than the writer does and its case comes back empty.
func TestEveryEnvelopeDecodesBackToItsValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		write func(w http.ResponseWriter)
		want  httpjson.Refused
	}{{
		name:  "a bare code",
		write: func(w http.ResponseWriter) { httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound) },
		want:  httpjson.Refused{Code: httpjson.CodeNotFound},
	}, {
		name: "a text detail and hint",
		write: func(w http.ResponseWriter) {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				map[string]string{"detail": "the title is required", "hint": "name the task"})
		},
		want: httpjson.Refused{Code: httpjson.CodeInvalidBody,
			Detail: httpjson.Sentence("the title is required"), Hint: httpjson.Sentence("name the task")},
	}, {
		name: "an authority refusal",
		write: func(w http.ResponseWriter) {
			status, code, fields := authz.RefusalFields(authz.Decision{Reason: authz.ReasonAssistScope,
				Grants: []iam.Grant{iam.GrantWorklet, iam.GrantAdmin}})
			httpjson.FailWithFields(w, status, code, fields)
		},
		want: httpjson.Refused{Code: httpjson.CodeUnauthorized,
			Message: unauthorizedSays(authz.ReasonAssistScope, iam.GrantWorklet, iam.GrantAdmin),
			Reason:  string(authz.ReasonAssistScope), Grants: []string{"worklet", "admin"}},
	}, {
		name: "a step-up",
		write: func(w http.ResponseWriter) {
			status, code, fields := authz.RefusalFields(authz.Decision{Reason: authz.ReasonStepUp,
				Recency: iam.RecencyStepUp})
			httpjson.FailWithFields(w, status, code, fields)
		},
		want: httpjson.Refused{Code: httpjson.CodeStepUpRequired,
			Reason: string(authz.ReasonStepUp), Grants: []string{}},
	}}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		tc.write(rec)
		got, ok := httpjson.Decode(rec.Body.Bytes())
		// THE CODE'S OWN SENTENCE, but where a case names the one its
		// refusal chose: `unauthorized` says the rule its reason names.
		if tc.want.Message == "" {
			tc.want.Message = tc.want.Code.Message()
		}
		if !ok || got.Code != tc.want.Code || got.Message != tc.want.Message ||
			got.Detail != tc.want.Detail || got.Hint != tc.want.Hint ||
			got.Reason != tc.want.Reason || !slices.Equal(got.Grants, tc.want.Grants) {
			t.Errorf("%s decoded as %+v, %v; want %+v", tc.name, got, ok, tc.want)
		}
	}
	for _, body := range []string{"", "  ", "not json", `["a"]`, `{"message":"no code"}`} {
		if got, ok := httpjson.Decode([]byte(body)); ok {
			t.Errorf("%q decoded as an envelope: %+v", body, got)
		}
	}
}
