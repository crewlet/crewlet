package authz_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// AN AGENT'S ACCESS IS SAVED BY ANY ADMIN, AND RAISED TO A VENDOR'S
// ADMINISTRATION ROLE ONLY BY A PERSON (D10).
//
// The save's own verb admits an administrator's machine token, as every other
// setup write does; the raise's verb refuses that same token, because a token
// proves nobody is at a keyboard. A person's assistant carrying the one request
// the person approved is that person deciding it (ADR-0051), so it may raise;
// a read grant may not. Mutation: drop the raise row's presence, or open the
// save to the assistant's read grant, and a case here fails.
func TestARaiseNeedsAPersonAndASaveDoesNot(t *testing.T) {
	t.Parallel()
	now := time.Now()
	fresh := now.Add(time.Hour)
	admin := []iam.Grant{iam.GrantAdmin}
	token := iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("ci"), Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: admin, ReauthAt: fresh,
		Via: iam.MachineTokenPrefix + "ci"}
	person := iam.Principal{ID: uuid.New(), Login: "ana.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: admin, ReauthAt: fresh}
	approved := person
	approved.Via, approved.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), iam.AssistScopeExec
	reading := approved
	reading.AssistScope = iam.AssistScopeRead

	for name, tc := range map[string]struct {
		p      iam.Principal
		action authz.Action
		allow  bool
		reason authz.Reason
	}{
		"a token saves":              {token, authz.ActionSetupAccessWrite, true, ""},
		"a token is refused a raise": {token, authz.ActionSetupAccessRaise, false, authz.ReasonTokenRefused},
		"a person raises":            {person, authz.ActionSetupAccessRaise, true, ""},
		"an approved request saves":  {approved, authz.ActionSetupAccessWrite, true, ""},
		"an approved request raises": {approved, authz.ActionSetupAccessRaise, true, ""},
		"a read grant cannot save":   {reading, authz.ActionSetupAccessWrite, false, authz.ReasonAssistScope},
		"a read grant cannot raise":  {reading, authz.ActionSetupAccessRaise, false, authz.ReasonAssistScope},
	} {
		d := authz.Decide(t.Context(), tc.p, tc.action, authz.Object{}, authz.NoChart{}, now)
		if d.Allowed != tc.allow || (!tc.allow && d.Reason != tc.reason) {
			t.Errorf("%s: decided %+v, want allowed=%v reason=%q", name, d, tc.allow, tc.reason)
		}
	}
	stale := person
	stale.ReauthAt = now.Add(-time.Minute)
	for _, a := range []authz.Action{authz.ActionSetupAccessWrite, authz.ActionSetupAccessRaise} {
		if d := authz.Decide(t.Context(), stale, a, authz.Object{}, authz.NoChart{}, now); d.Reason !=
			authz.ReasonStepUp {
			t.Errorf("%s with a stale proof decided %+v, want a step-up", a, d)
		}
	}
}
