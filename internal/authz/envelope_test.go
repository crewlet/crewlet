package authz_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A TOKEN REFUSED FOR WANT OF A PERSON IS TOLD SO, NOT TO ASK FOR A GRANT.
//
// The `unauthorized` envelope's message says the credential lacks a grant and
// to ask whoever runs the deployment for it, and a machine token on a verb
// that needs a person present was answered exactly that, beside an empty
// grants list: its owner may hold every grant there is, and no grant on the
// token would admit it. Its refusal carries its own sentence now. The CONTROL
// is a refusal for a missing grant, which the message describes and which
// carries none. Mutation: drop the sentence and the token's refusal is the
// generic one.
func TestATokenRefusedForWantOfAPersonIsToldSo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		d    authz.Decision
		want string
	}{
		{"a machine token", authz.Decision{Reason: authz.ReasonTokenRefused},
			"personal access token cannot do this"},
		{"a missing grant (the control)", authz.Decision{Reason: authz.ReasonNoGrant,
			Grants: []iam.Grant{iam.GrantPeopleManage}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			authz.EnvelopeRefusal(rec, nil, authz.Policy{}, tc.d)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			detail, _ := body["detail"].(string)
			if rec.Code != http.StatusForbidden || body["error"] != "unauthorized" ||
				body[authz.DetailReason] != string(tc.d.Reason) ||
				(tc.want == "") != (detail == "") || !strings.Contains(detail, tc.want) {
				t.Errorf("answered %d %v, want a 403 whose detail says %q",
					rec.Code, body, tc.want)
			}
		})
	}
}
