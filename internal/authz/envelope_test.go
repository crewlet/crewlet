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

// EVERY REFUSAL IS ANSWERED BY ONE MAPPING.
//
// [authz.RefusalFields] is how a surface that adds fields of its own to a
// refusal learns the status, the code and the rule's fields; [EnvelopeRefusal]
// is the surface that adds none. The two must write the same answer for every
// reason the table refuses by, or a refusal reads one way at the router and
// another at the surface that decided it after reading a row — which is how
// the operator surface came to answer a machine token's refusal `unauthorized`.
// Mutation: change the code one of them gives any reason and its case fails.
func TestRefusalFieldsIsWhatEnvelopeRefusalWrites(t *testing.T) {
	t.Parallel()
	admissions := map[authz.Reason]bool{
		authz.ReasonGrant: true, authz.ReasonSelf: true, authz.ReasonAuthor: true,
		authz.ReasonLead: true, authz.ReasonEverybody: true, authz.ReasonAsked: true,
		authz.ReasonShared: true,
	}
	codes := map[authz.Reason]httpjson.Code{
		authz.ReasonStepUp:       httpjson.CodeStepUpRequired,
		authz.ReasonTokenRefused: httpjson.CodeTokenRefused,
		authz.ReasonNotShared:    httpjson.CodeNotFound,
	}
	refusals := 0
	for _, reason := range authz.Reasons {
		if admissions[reason] {
			continue
		}
		refusals++
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			d := authz.Decision{Reason: reason, Grants: []iam.Grant{iam.GrantAdmin}}
			if reason == authz.ReasonStepUp {
				d.Recency = iam.RecencyStepUp
			}
			router := httptest.NewRecorder()
			authz.EnvelopeRefusal(router, nil, authz.Policy{}, d)
			status, code, detail := authz.RefusalFields(d)
			surface := httptest.NewRecorder()
			httpjson.FailWithFields(surface, status, code, detail)
			want, ok := codes[reason]
			if !ok {
				want = httpjson.CodeUnauthorized
			}
			// No relation at all is an id nothing holds, never a 403.
			wantStatus := http.StatusForbidden
			if want == httpjson.CodeNotFound {
				wantStatus = http.StatusNotFound
			}
			if status != wantStatus || code != want {
				t.Errorf("RefusalFields(%s) = %d %s, want %d %s", reason, status,
					code, wantStatus, want)
			}
			if router.Code != surface.Code ||
				router.Body.String() != surface.Body.String() {
				t.Errorf("the router answers %d %s and the mapping %d %s",
					router.Code, router.Body, surface.Code, surface.Body)
			}
		})
	}
	if refusals < 10 {
		t.Fatalf("held %d refusal reasons; a walk that stopped reading the "+
			"table certifies nothing", refusals)
	}
}

// A ROLE BELOW THE VERB'S IS REFUSED NAMING THE ROLE, BY THE ONE MAPPING.
//
// The artifacts surface used to write this refusal itself beside
// [authz.RefusalFields] — the same code and reason with `role` added and a
// sentence of its own — so the router's answer to the same decision carried no
// role at all, and a client learned whom to ask for what from one surface and
// not another. The role is the mapping's now, on every surface that answers a
// refusal through it, the router's included; a decision naming no valid role
// adds no key. Mutation: drop the arm and both the mapping and the router lose
// the role.
func TestARoleRefusalNamesTheRoleItNeedsOnEverySurface(t *testing.T) {
	t.Parallel()
	d := authz.Decision{Reason: authz.ReasonRole, Role: "editor",
		Grants: []iam.Grant{iam.GrantAdmin}}
	status, code, detail := authz.RefusalFields(d)
	if status != http.StatusForbidden || code != httpjson.CodeUnauthorized {
		t.Fatalf("RefusalFields(role) = %d %s, want 403 unauthorized", status, code)
	}
	if detail[authz.DetailRole] != "editor" || detail[authz.DetailReason] != "role" {
		t.Errorf("the detail is %v, want the reason and role editor", detail)
	}
	router := httptest.NewRecorder()
	authz.EnvelopeRefusal(router, nil, authz.Policy{}, d)
	var body map[string]any
	if err := json.Unmarshal(router.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the router's answer: %v", err)
	}
	if body[authz.DetailRole] != "editor" {
		t.Errorf("the router's refusal names role %v, want editor: %s", body[authz.DetailRole],
			router.Body)
	}
	_, _, none := authz.RefusalFields(authz.Decision{Reason: authz.ReasonRole})
	if _, named := none[authz.DetailRole]; named {
		t.Errorf("a role refusal naming no valid role carries one: %v", none)
	}
}
