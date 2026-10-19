package iamapi_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// AN IDENTITY RECORD'S REFUSAL IS ANSWERED BY THE RULE THAT MADE IT.
//
// The domain says whether a permission would lift a refusal
// ([iamdomain.Refusal]), and the surface answers the two kinds as their
// remedies differ. One a permission lifts is `403 unauthorized` exactly as the
// authority table answers a missing permission — reason `no_grant`, the grants
// that would lift it, and a sentence naming that permission alone. One the
// record's own rule made is `403 forbidden`, carrying the domain's sentence and
// no reason or grants, because no rule of the table decided it and no
// permission would change it. Both used to be one `unauthorized` naming no
// grant, whose sentence named every permission there is — so an administrator
// minting a person's token was told to ask an administrator for access. A
// bare [iamdomain.ErrRefused], which the domain never builds, is unclassified
// and answered as the fault it is.
//
// Walked over a create, an edit, an invitation and a mint, each refused in the
// record's own decide. Mutations: answer both kinds `unauthorized` and the
// forbidden rows go red; drop the grants from the first arm and the sentence
// names every permission; match the refusal arms before the token arm and the
// token rows of [TestATokenWritingAnAdministrativeRecordIsTokenRefused] go red.
func TestARecordRefusalSaysWhetherAPermissionWouldLiftIt(t *testing.T) {
	t.Parallel()
	gestures := []struct {
		name, call, method, target string
		body                       map[string]any
	}{
		{"a create", "create", http.MethodPost, "/iam/people",
			map[string]any{"email": "dana@example.com", "seat": "founder",
				"grants": iam.Apps}},
		{"a suspension", "stage", http.MethodPatch, "/iam/people/" + bob.String(),
			map[string]any{"stage": "suspended"}},
		{"an invitation", "invite", http.MethodPost, "/iam/invitations",
			map[string]any{"email": "dana@example.com", "seat": "founder",
				"grants": iam.Apps}},
		{"a mint", "mint", http.MethodPost, "/iam/credentials?person=" + bob.String(),
			map[string]any{}},
	}
	for _, g := range gestures {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()

			// A PERMISSION WOULD LIFT IT.
			r := newRig(t)
			r.writer.refusals = map[string]error{g.call: &iamdomain.Refusal{
				Grants: []iam.Grant{iam.GrantAdmin},
				Err:    errors.New("this needs admin, and you have [tracker]")}}
			got := r.as(administrator(), g.method, g.target, g.body)
			if got.status != http.StatusForbidden ||
				got.body["error"] != string(httpjson.CodeUnauthorized) ||
				got.body[authz.DetailReason] != string(authz.ReasonNoGrant) {
				t.Fatalf("a refusal admin lifts answered %d %v, want 403 "+
					"unauthorized for no_grant", got.status, got.body)
			}
			if grants, _ := got.body[authz.DetailGrants].([]any); len(grants) != 1 ||
				grants[0] != string(iam.GrantAdmin) {
				t.Errorf("the refusal names %v, want [admin]",
					got.body[authz.DetailGrants])
			}
			message, _ := got.body["message"].(string)
			if !strings.Contains(message, "(Admin)") {
				t.Errorf("the refusal says %q, want it to name Admin alone", message)
			}

			// THE RECORD'S OWN RULE.
			r = newRig(t)
			r.writer.refusals = map[string]error{g.call: &iamdomain.Refusal{
				Grants: []iam.Grant{},
				Err: errors.New("bob is a person, so only they can create " +
					"their personal access tokens")}}
			got = r.as(administrator(), g.method, g.target, g.body)
			if got.status != http.StatusForbidden ||
				got.body["error"] != string(httpjson.CodeForbidden) {
				t.Fatalf("a refusal no permission lifts answered %d %v, want 403 "+
					"forbidden", got.status, got.body)
			}
			for _, key := range []string{authz.DetailReason, authz.DetailGrants} {
				if _, named := got.body[key]; named {
					t.Errorf("the domain's own refusal carries %s: %v", key,
						got.body[key])
				}
			}
			if detail, _ := got.body["detail"].(string); !strings.Contains(detail,
				"only they can create their personal access tokens") {
				t.Errorf("the refusal's detail is %q, want the domain's sentence",
					detail)
			}
			for _, handed := range []string{"url", "token"} {
				if _, ok := got.body[handed]; ok {
					t.Errorf("a refused record handed out a %s", handed)
				}
			}

			// AND ONE NOBODY CLASSIFIED is a fault, never a 403 guessed at.
			r = newRig(t)
			r.writer.refusals = map[string]error{g.call: fmt.Errorf(
				"%w: unclassified", iamdomain.ErrRefused)}
			got = r.as(administrator(), g.method, g.target, g.body)
			if got.status != http.StatusInternalServerError {
				t.Errorf("a bare ErrRefused answered %d %v, want 500", got.status,
					got.body)
			}
		})
	}
}
