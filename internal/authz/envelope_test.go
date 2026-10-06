package authz_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A TOKEN REFUSED FOR WANT OF A PERSON IS TOLD SO, NOT TO ASK FOR A GRANT.
//
// The `unauthorized` envelope's message says the credential lacks a grant and
// to ask whoever runs the deployment for it, and a machine token on a verb
// that needs a person present was answered exactly that, beside an empty
// grants list: its owner may hold every grant there is, and no grant on the
// token would admit it. A sentence of its own as `detail` then contradicted
// the `message` beside it, so the refusal is a code of its own whose ONE
// sentence says so, and it carries no second one. The CONTROL is a refusal
// for a missing grant, which the `unauthorized` message describes. Mutation:
// answer the token's refusal `unauthorized` and its message is the grant's.
func TestATokenRefusedForWantOfAPersonIsToldSo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		d    authz.Decision
		code httpjson.Code
		want string
	}{
		{"a machine token", authz.Decision{Reason: authz.ReasonTokenRefused},
			httpjson.CodeTokenRefused, "personal access token cannot do this"},
		{"a missing grant (the control)", authz.Decision{Reason: authz.ReasonNoGrant,
			Grants: []iam.Grant{iam.GrantPeopleManage}},
			httpjson.CodeUnauthorized, "does not carry the grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			authz.EnvelopeRefusal(rec, nil, authz.Policy{}, tc.d)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			message, _ := body["message"].(string)
			_, second := body["detail"]
			if rec.Code != http.StatusForbidden || body["error"] != string(tc.code) ||
				body[authz.DetailReason] != string(tc.d.Reason) ||
				!strings.Contains(message, tc.want) || second {
				t.Errorf("answered %d %v, want a 403 %s whose one sentence says %q",
					rec.Code, body, tc.code, tc.want)
			}
		})
	}
}
