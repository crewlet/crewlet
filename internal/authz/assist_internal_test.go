package authz

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// A PRESENCE ROW REFUSES AN ASSISTED PRINCIPAL, WHATEVER ITS ASSIST CLASS SAYS.
//
// The assistant is not a person at a keyboard, so a verb that needs one is
// refused it on EVERY arm, the self arm of a row that spares it for a machine
// token included: a token revoking itself is containment, and the assistant
// doing the same is a model one step from the person's credentials. Every
// such row in the table states never as well (a walk holds that), which is
// exactly why the bar is asked of rows that do NOT exist yet: a row stating
// read or approve beside a presence is refused here all the same, and so is a
// row that states no assist class at all. The controls are the same rows
// without a presence, and the same person at their keyboard.
//
// Mutation: drop the presence arm from assistBar and the read rows here are
// admitted; read the zero class as read and the undecided row is.
func TestAPresenceRowRefusesAnAssistedPrincipal(t *testing.T) {
	t.Parallel()
	keyboard := iam.Principal{ID: uuid.New(), Login: "jane.doe", Seat: "jane",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour)}
	through := func(scope iam.AssistScope) iam.Principal {
		p := keyboard
		p.Via, p.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), scope
		return p
	}
	for _, c := range []struct {
		name string
		row  rule
		// what a read grant and an exec grant are barred with; empty is
		// no bar at all.
		read, exec Reason
	}{
		{"read beside a person on every arm",
			rule{assist: AssistRead, presence: PresenceRequired},
			ReasonAssistRefused, ReasonAssistRefused},
		{"approve beside a person on every arm",
			rule{assist: AssistApprove, presence: PresenceRequired},
			ReasonAssistRefused, ReasonAssistRefused},
		{"read beside a person on every arm but the self arm",
			rule{assist: AssistRead, presence: PresenceRequiredForOthers},
			ReasonAssistRefused, ReasonAssistRefused},
		{"no assist class at all", rule{}, ReasonAssistRefused, ReasonAssistRefused},
		{"a class this build does not know", rule{assist: "sometimes"},
			ReasonAssistRefused, ReasonAssistRefused},
		{"read with no presence (the control)", rule{assist: AssistRead}, "", ""},
		{"approve with no presence (the control)", rule{assist: AssistApprove},
			ReasonAssistScope, ""},
	} {
		for scope, want := range map[iam.AssistScope]Reason{
			iam.AssistScopeRead: c.read, iam.AssistScopeExec: c.exec,
		} {
			bar, barred := assistBar(through(scope), c.row)
			if barred != (want != "") || bar.Reason != want {
				t.Errorf("%s under a %s grant answered %q (barred %v), want %q",
					c.name, scope, bar.Reason, barred, want)
			}
		}
		if _, barred := assistBar(keyboard, c.row); barred {
			t.Errorf("%s barred the person at their keyboard", c.name)
		}
	}

	// AND EVERY PRESENCE ROW OF THE TABLE, through Decide, about the
	// caller's own record as well as anybody else's.
	self := Object{Kind: KindPerson, Owner: keyboard.ID.String(), ID: keyboard.ID.String()}
	other := uuid.New().String()
	rows := 0
	for a, r := range rules {
		if r.presence == PresenceOptional {
			continue
		}
		rows++
		for _, object := range []Object{self, {Kind: KindPerson, Owner: other, ID: other},
			{Kind: KindCompany}, {}} {
			for _, scope := range iam.AssistScopes {
				if d := Decide(t.Context(), through(scope), a, object, NoChart{},
					time.Now()); d.Allowed || d.Reason != ReasonAssistRefused {
					t.Errorf("%s (%q) about %+v through a %s grant decided %+v, want "+
						"refused %q", a, r.presence, object, scope, d, ReasonAssistRefused)
				}
			}
		}
	}
	if rows == 0 {
		t.Fatal("no row of the table needs a person present, so the walk " +
			"certifies nothing")
	}
}
