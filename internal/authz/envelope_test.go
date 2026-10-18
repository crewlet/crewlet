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
// The `unauthorized` envelope's message says the credential lacks the access
// the request needs, and a machine token on a verb that needs a person present
// was answered exactly that, beside an empty grants list: its owner may hold
// admin, and no permission on the token would admit it. A sentence of its own
// as `detail` then contradicted the `message` beside it, so the refusal is a
// code of its own whose ONE sentence says so, and it carries no second one.
// The CONTROL is a refusal for a missing permission, which the `unauthorized`
// code's sentence for that reason describes — compared with the envelope's own
// sentence for the same refusal rather than with words, which belong to
// internal/api/httpjson. Mutation: answer the token's refusal `unauthorized`
// and its message is the permission's.
func TestATokenRefusedForWantOfAPersonIsToldSo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		d    authz.Decision
		code httpjson.Code
		want string
	}{
		{"a machine token", authz.Decision{Reason: authz.ReasonTokenRefused},
			httpjson.CodeTokenRefused, httpjson.CodeTokenRefused.Message()},
		{"a missing permission (the control)", authz.Decision{Reason: authz.ReasonNoGrant,
			Grants: []iam.Grant{iam.GrantAdmin}},
			httpjson.CodeUnauthorized, "(Admin)"},
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
			sentence := httpjson.Envelope(tc.code,
				authz.RefusalDetail(tc.d.Reason, tc.d.Grants))["message"]
			if rec.Code != http.StatusForbidden || body["error"] != string(tc.code) ||
				body[authz.DetailReason] != string(tc.d.Reason) ||
				message != sentence || !strings.Contains(message, tc.want) ||
				second {
				t.Errorf("answered %d %v, want a 403 %s whose one sentence says %q",
					rec.Code, body, tc.code, tc.want)
			}
		})
	}
}
