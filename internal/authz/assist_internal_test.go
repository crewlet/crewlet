package authz

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// A ROW STATES ITS NATURE, AND THE HUMAN-ONLY LIST IS THE ONE PLACE OF NEVER.
//
// Every row's own assist field is [AssistRead] or [AssistApprove], what the
// verb is to the assistant, and [AssistNever] is spelled only by
// [assistantNever], which names verbs the table has, each with a reason and a
// sentence for the person that names no verb. A row
// stating never would be a second human-only list nobody reads beside the
// first; a list entry naming no verb would certify a refusal of nothing.
//
// Mutation: state `assist: AssistNever` on any row, or list a verb that is not
// in the table, and this names it.
func TestARowStatesItsNatureAndTheListAloneSaysNever(t *testing.T) {
	t.Parallel()
	for a, r := range rules {
		if r.assist != AssistRead && r.assist != AssistApprove {
			t.Errorf("%s states %q on its row; a row states read or approve, and "+
				"never is the human-only list's alone", a, r.assist)
		}
	}
	for a, entry := range assistantNever {
		if _, known := rules[a]; !known {
			t.Errorf("the human-only list names %s, which the table has no row for", a)
		}
		if entry.why == "" || entry.tell == "" {
			t.Errorf("the human-only list names %s with no reason or no sentence for "+
				"the person", a)
		}
		if strings.Contains(entry.tell, string(a)) {
			t.Errorf("%s's sentence for the person names the verb itself: %q", a, entry.tell)
		}
	}
}

// A PRESENCE ROW IS REFUSED TO A READ GRANT, AND ADMITTED ONLY TO AN EXEC GRANT.
//
// A verb that needs a person at the keyboard keeps a machine token out,
// because a token proves nobody is there. An exec grant carries one request
// the person approved, from their own browser, a moment ago: that approval is
// the presence the row asks for (ADR-0051, ADR-0052). A read grant carries no
// such gesture, so it is refused every presence row, the self arm of a row
// that spares it for a machine token included, naming the scope (the remedy is
// a proposal the person approves). A verb on the human-only list is refused
// both, whatever its presence, and so is a row that states no assist class at
// all. The controls are the same rows without a presence, and the same person
// at their keyboard.
//
// Mutation: drop the presence arm from assistBar and the read grant takes a
// presence row; refuse presence to every scope and the exec grant is refused.
func TestAPresenceRowRefusesAReadGrantAndAdmitsAnApprovedOne(t *testing.T) {
	t.Parallel()
	keyboard := iam.Principal{ID: uuid.New(), Login: "jane.doe", Seat: "jane",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour)}
	through := func(scope iam.AssistScope) iam.Principal {
		p := keyboard
		p.Via, p.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), scope
		return p
	}
	// a verb the table has, and one the human-only list names.
	const open, never = ActionWorkCreate, ActionSecretReveal
	for _, c := range []struct {
		name string
		verb Action
		row  rule
		// what a read grant and an exec grant are barred with; empty is
		// no bar at all.
		read, exec Reason
	}{
		{"read beside a person on every arm", open,
			rule{assist: AssistRead, presence: PresenceRequired},
			ReasonAssistScope, ""},
		{"approve beside a person on every arm", open,
			rule{assist: AssistApprove, presence: PresenceRequired},
			ReasonAssistScope, ""},
		{"read beside a person on every arm but the self arm", open,
			rule{assist: AssistRead, presence: PresenceRequiredForOthers},
			ReasonAssistScope, ""},
		{"a listed verb beside a person", never,
			rule{assist: AssistRead, presence: PresenceRequired},
			ReasonAssistRefused, ReasonAssistRefused},
		{"a listed verb with no presence", never, rule{assist: AssistRead},
			ReasonAssistRefused, ReasonAssistRefused},
		{"no assist class at all", open, rule{}, ReasonAssistRefused, ReasonAssistRefused},
		{"a class this build does not know", open, rule{assist: "sometimes"},
			ReasonAssistRefused, ReasonAssistRefused},
		{"read with no presence (the control)", open, rule{assist: AssistRead}, "", ""},
		{"approve with no presence (the control)", open, rule{assist: AssistApprove},
			ReasonAssistScope, ""},
	} {
		for scope, want := range map[iam.AssistScope]Reason{
			iam.AssistScopeRead: c.read, iam.AssistScopeExec: c.exec,
		} {
			bar, barred := assistBar(through(scope), c.verb, c.row, false)
			if barred != (want != "") || bar.Reason != want {
				t.Errorf("%s under a %s grant answered %q (barred %v), want %q",
					c.name, scope, bar.Reason, barred, want)
			}
		}
		if _, barred := assistBar(keyboard, c.verb, c.row, false); barred {
			t.Errorf("%s barred the person at their keyboard", c.name)
		}
	}

	// AND EVERY PRESENCE ROW OF THE TABLE, through Decide, about the
	// caller's own record as well as anybody else's: a read grant is refused
	// each one, and an exec grant is decided exactly as the person at their
	// keyboard is, unless the human-only list names the verb.
	self := Object{Kind: KindPerson, Owner: keyboard.ID.String(), ID: keyboard.ID.String()}
	other := uuid.New().String()
	rows, opened := 0, 0
	for a, r := range rules {
		if r.presence == PresenceOptional {
			continue
		}
		rows++
		_, listed := assistantNever[a]
		for _, object := range []Object{self, {Kind: KindPerson, Owner: other, ID: other},
			{Kind: KindCompany}, {}} {
			if d := Decide(t.Context(), through(iam.AssistScopeRead), a, object, NoChart{},
				time.Now()); d.Allowed {
				t.Errorf("%s (%q) about %+v through a read grant was admitted", a,
					r.presence, object)
			}
			exec := Decide(t.Context(), through(iam.AssistScopeExec), a, object, NoChart{},
				time.Now())
			want := Decide(t.Context(), keyboard, a, object, NoChart{}, time.Now())
			switch {
			case listed && (exec.Allowed || exec.Reason != ReasonAssistRefused):
				t.Errorf("%s is on the human-only list and an exec grant decided %+v", a, exec)
			case !listed && (exec.Allowed != want.Allowed || exec.Reason != want.Reason):
				t.Errorf("%s (%q) about %+v decided %+v through an exec grant and %+v "+
					"at the keyboard", a, r.presence, object, exec, want)
			case !listed && exec.Allowed:
				opened++
			}
		}
	}
	if rows == 0 || opened == 0 {
		t.Fatalf("walked %d presence rows and admitted an exec grant on %d, so the "+
			"walk certifies nothing", rows, opened)
	}
}
