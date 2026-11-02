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
// Either kind's `detail` is the domain's SENTENCE ([iamdomain.Sentence]):
// never the class word in front of it, and never what the refusal was about
// by id ([iamdomain.Refusal.About]) — the person the token was asked for, the
// party's own permissions — which goes to the log.
//
// Walked over a create, an edit, an invitation and a mint, each refused in the
// record's own decide. Mutations: answer both kinds `unauthorized` and the
// forbidden rows go red; drop the grants from the first arm and the sentence
// names every permission; match the refusal arms before the token arm and the
// token rows of [TestATokenWritingAnAdministrativeRecordIsTokenRefused] go red;
// answer err.Error() as the detail again and both kinds go red on the class
// word and the id.
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
