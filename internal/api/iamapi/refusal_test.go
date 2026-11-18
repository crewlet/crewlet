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
	"github.com/crewlet/crewlet/internal/words"
)

// AN IDENTITY RECORD'S REFUSAL IS ANSWERED BY THE RULE THAT MADE IT.
//
// A refusal a permission lifts ([iamdomain.Refusal]) is `403 unauthorized`,
// reason `no_grant` and the grants; the record's own rule is `403 forbidden`
// with neither. `detail` is the domain's SENTENCE ([iamdomain.Sentence]), never
// the class word nor [iamdomain.Refusal.About]'s ids, which are logged.
// Mutations: answer both kinds `unauthorized` and the forbidden rows go red;
// drop the grants from the first arm and the sentence names every permission;
// match the refusal arms before the token arm and the token rows of
// [TestATokenWritingAnAdministrativeRecordIsTokenRefused] go red; answer
// err.Error() as the detail and both kinds go red.
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
				Err:    errors.New("only an admin can do that"),
				About:  "update, by a party holding tracker"}}
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
			if detail, _ := got.body["detail"].(string); detail != "Only an admin can do that." {
				t.Errorf("the refusal's detail is %q, want the domain's sentence "+
					"alone", detail)
			}

			// THE RECORD'S OWN RULE.
			r = newRig(t)
			r.writer.refusals = map[string]error{g.call: &iamdomain.Refusal{
				Grants: []iam.Grant{},
				Err: errors.New("only they can create their own access " +
					"tokens"),
				About: "owner " + bob.String() + ", a person"}}
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
			if detail, _ := got.body["detail"].(string); detail !=
				"Only they can create their own access tokens." {
				t.Errorf("the refusal's detail is %q, want the domain's sentence "+
					"alone, with neither its class word nor the owner's id", detail)
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

// A STATUS THIS BUILD DOES NOT KNOW IS REFUSED NAMING EVERY ONE IT DOES, as a
// sentence says a set: each quoted as the value typed beside it is, the last
// two joined by "or". It was joined by hand, unquoted, so a person read
// `Use invited, enrolling, active, suspended or retired` beside the quoted
// value they had typed. Mutation: put back the hand join in unknownStage and
// this fails.
func TestAnUnknownStatusIsRefusedNamingEveryStatus(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodGet, "/iam/people?stage=asleep", nil)
	if got.status != http.StatusBadRequest {
		t.Fatalf("an unknown status answered %d, want 400: %v", got.status, got.body)
	}
	want := `"asleep" isn't a valid status. Use ` + words.Or(iam.Stages) + "."
	if got.body["detail"] != want {
		t.Errorf("the detail is %q, want %q", got.body["detail"], want)
	}
}
