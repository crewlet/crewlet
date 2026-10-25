package authz_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE DASHBOARD ASSISTANT'S COLUMN OF THE AUTHORITY TABLE (ADR-0051).
//
// An assisted principal is the person, composed from their own rows, with the
// assistant grant named as the credential the request came through. So every
// case here decides one person twice, at their keyboard and through the
// assistant, and asks what the column took away: never more than the row
// says, and never anything the person's own authority did not already admit.

// assisted is p acting through an assistant grant of this scope, which is how
// the request guard composes one: the person, with the grant named beside them.
func assisted(p iam.Principal, scope iam.AssistScope) iam.Principal {
	p.Via = iam.AssistName(uuid.Must(uuid.NewV7()))
	p.AssistScope = scope
	return p
}

// assistant is the person every walk here decides: holding every permission,
// freshly proved, and bound to the seat the chart has leading a person, a
// project, a unit and a container, so the class admits them on some object of
// nearly every row, and the assistant can only be refused for being one.
func assistant() iam.Principal { return personLeading("cto", iam.AllGrants...) }

// assistObjects are the things a verb can be about, the caller's own records
// and their artifact among them.
func assistObjects(self iam.Principal) map[string]authz.Object {
	other := uuid.New().String()
	return map[string]authz.Object{
		"the caller by id": {Kind: authz.KindPerson, Owner: self.ID.String(),
			ID: self.ID.String()},
		"the caller by seat":  {Kind: authz.KindPerson, Owner: self.Seat},
		"a report":            {Kind: authz.KindPerson, Owner: "sre"},
		"somebody else":       {Kind: authz.KindPerson, Owner: other, ID: other},
		"a project they lead": {Kind: authz.KindProject, Container: "PLATFORM"},
		"a remark they wrote": {Kind: authz.KindTask, Container: "PLATFORM",
			Author: self.Seat},
		"a unit they lead":     {Kind: authz.KindUnit, Container: "sre"},
		"their own artifact":   {Kind: authz.KindArtifact, ID: "a-own", Owner: self.Seat},
		"the company":          {Kind: authz.KindCompany},
		"nobody in particular": {},
	}
}

// EVERY ROW DECIDES THE ASSISTANT, AND NONE STATES WHAT IT CANNOT MEAN.
//
// The walk that makes the column mandatory: a row with no assist class is a
// verb somebody added without deciding, and it would otherwise read as one of
// the three. Each class must be stated by some row or the walks below certify
// nothing about it. And two statements are refused because they could only
// mislead a reader: a class other than never on a row that needs a person
// present (the assistant is not a person at a keyboard, and [authz.Decide]
// refuses it there whatever the row says), and on a seat-only row (an
// assistant acts as a person, who never takes one).
//
// Mutation: drop the assist field from one row and this names it.
func TestEveryRowDecidesTheAssistant(t *testing.T) {
	t.Parallel()
	stated := map[authz.AssistClass]int{}
	for _, a := range authz.Actions() {
		class, known := authz.AssistOf(a)
		if !known {
			t.Errorf("%s is in the table and AssistOf does not know it", a)
			continue
		}
		if !class.Valid() {
			t.Errorf("%s states the assist class %q, which is not one of %v: a "+
				"row has to decide what the assistant may do with its verb",
				a, class, authz.AssistClasses)
			continue
		}
		stated[class]++
		if presence, _ := authz.PresenceOf(a); presence != authz.PresenceOptional &&
			class != authz.AssistNever {
			t.Errorf("%s needs a person present (%q) and states %q for the "+
				"assistant, which no assistant grant can ever take", a, presence, class)
		}
		if only, _ := authz.SeatOnlyOf(a); only && class != authz.AssistNever {
			t.Errorf("%s is an agent's own and states %q for the assistant, "+
				"which acts as a person", a, class)
		}
	}
	for _, c := range authz.AssistClasses {
		if stated[c] == 0 {
			t.Errorf("no row states %q, so nothing here certifies it", c)
		}
	}
	if _, known := authz.AssistOf("no.such.verb"); known {
		t.Error("AssistOf knows a verb the table has no row for")
	}
	if authz.AssistClass("").Valid() {
		t.Error("the zero assist class is valid, so a row nobody decided passes")
	}
}

// AN ASSIST-NEVER ROW REFUSES AN ASSISTED PRINCIPAL, UNDER EITHER SCOPE.
//
// Walked over every row the assistant never takes, with a person holding every
// permission and its control, the same person at their keyboard, admitted on
// some object of the row: through the assistant the row is refused on every
// object, under a read grant and under an exec grant alike, naming
// [authz.ReasonAssistRefused] and no permission (none would change it). A
// seat-only row is refused before the assistant's bar is reached, as the
// person's own click is. And the bar keeps the artifact rows' concealment: a
// person with no relation to an artifact is told it does not exist, through
// the assistant as at their keyboard.
//
// Mutation: drop the never arm from the bar and every row here is admitted.
func TestAnAssistNeverRowRefusesAnAssistedPrincipal(t *testing.T) {
	t.Parallel()
	keyboard := assistant()
	rows := 0
	for _, a := range authz.Actions() {
		if class, _ := authz.AssistOf(a); class != authz.AssistNever {
			continue
		}
		rows++
		seatOnly, _ := authz.SeatOnlyOf(a)
		admitted := false
		for name, object := range assistObjects(keyboard) {
			want := authz.Decide(t.Context(), keyboard, a, object, nimbus(), decidedAt)
			admitted = admitted || want.Allowed
			for _, scope := range iam.AssistScopes {
				got := authz.Decide(t.Context(), assisted(keyboard, scope), a, object,
					nimbus(), decidedAt)
				switch {
				case got.Allowed || got.Unknown():
					t.Errorf("%s about %s through a %s grant decided %+v, want "+
						"refused", a, name, scope, got)
				case seatOnly && got.Reason != authz.ReasonSeatOnly:
					t.Errorf("%s about %s through a %s grant refused %q, want the "+
						"person's own seat-only refusal", a, name, scope, got.Reason)
				case !seatOnly && got.Reason != authz.ReasonAssistRefused:
					t.Errorf("%s about %s through a %s grant refused %q, want %q",
						a, name, scope, got.Reason, authz.ReasonAssistRefused)
				case !seatOnly && len(got.Grants) != 0:
					t.Errorf("%s refused the assistant naming %v, and no "+
						"permission changes that answer", a, got.Grants)
				}
			}
		}
		if !seatOnly && !admitted {
			t.Errorf("%s admitted the person at their keyboard on nothing, so its "+
				"refusals through the assistant certify nothing", a)
		}
	}
	if rows < 40 {
		t.Fatalf("walked %d never rows; the column is not being read", rows)
	}

	// THE CONCEALMENT: no relation to an artifact is no artifact, through
	// the assistant as at the keyboard.
	stranger := personLeading("dana", iam.GrantArtifacts)
	private := authz.Object{Kind: authz.KindArtifact, ID: "a-private", Owner: "kai"}
	for _, a := range []authz.Action{authz.ActionArtifactTrash,
		authz.ActionArtifactExport, authz.ActionArtifactTransfer} {
		if want := authz.Decide(t.Context(), stranger, a, private, nimbus(),
			decidedAt); !want.NotFound() {
			t.Fatalf("the control decided %+v on %s, want not found", want, a)
		}
		if got := authz.Decide(t.Context(), assisted(stranger, iam.AssistScopeExec), a,
			private, nimbus(), decidedAt); !got.NotFound() {
			t.Errorf("%s on an artifact the person holds no relation to decided "+
				"%+v through the assistant, which names that it exists", a, got)
		}
	}
}

// A READ GRANT TAKES ONLY ASSIST-READ ROWS, AND DECIDES THEM AS THE PERSON.
//
// The scope half. Over every row and every object, a read grant decides an
// assist-read row exactly as the person's own click is decided (it takes
// nothing away there, and can never add), refuses an assist-approve row with
// [authz.ReasonAssistScope] (the remedy is a proposal, never a permission) and
// refuses a never row as the assistant. An exec grant decides an
// assist-approve row as the person: the request guard binds it to the one
// approved request, and the table only says the row is one an approval may
// carry. A grant whose scope is missing or unknown reaches reads alone, and a
// credential that names the assistant's class with a malformed grant id is
// the assistant's all the same.
//
// Mutation: let any scope but read take an approve row and the missing scope
// writes; key the bar on a well-formed grant id and the malformed one does.
func TestAReadGrantTakesOnlyAssistReadRows(t *testing.T) {
	t.Parallel()
	keyboard := assistant()
	reads, approves := 0, 0
	for _, a := range authz.Actions() {
		if only, _ := authz.SeatOnlyOf(a); only {
			continue
		}
		class, _ := authz.AssistOf(a)
		switch class {
		case authz.AssistRead:
			reads++
		case authz.AssistApprove:
			approves++
		}
		for name, object := range assistObjects(keyboard) {
			want := authz.Decide(t.Context(), keyboard, a, object, nimbus(), decidedAt)
			read := authz.Decide(t.Context(), assisted(keyboard, iam.AssistScopeRead), a,
				object, nimbus(), decidedAt)
			exec := authz.Decide(t.Context(), assisted(keyboard, iam.AssistScopeExec), a,
				object, nimbus(), decidedAt)
			switch class {
			case authz.AssistRead:
				if !sameDecision(read, want) || !sameDecision(exec, want) {
					t.Errorf("%s about %s decided %+v (read) and %+v (exec) "+
						"through the assistant and %+v at the keyboard: a read the "+
						"assistant may take is decided as the person's own",
						a, name, read, exec, want)
				}
			case authz.AssistApprove:
				if read.Allowed || read.Reason != authz.ReasonAssistScope {
					t.Errorf("%s about %s through a read grant decided %+v, want "+
						"refused %q", a, name, read, authz.ReasonAssistScope)
				}
				if !sameDecision(exec, want) {
					t.Errorf("%s about %s decided %+v through an exec grant and "+
						"%+v at the keyboard", a, name, exec, want)
				}
			}
			if class == authz.AssistRead {
				continue
			}
			for _, scope := range []iam.AssistScope{"", "write"} {
				got := authz.Decide(t.Context(), assisted(keyboard, scope), a, object,
					nimbus(), decidedAt)
				if got.Allowed {
					t.Errorf("%s about %s was admitted through a grant whose scope "+
						"is %q, which reaches reads alone", a, name, scope)
				}
			}
			malformed := keyboard
			malformed.Via, malformed.AssistScope = iam.AssistPrefix+"x", iam.AssistScopeExec
			if got := authz.Decide(t.Context(), malformed, a, object, nimbus(),
				decidedAt); class == authz.AssistNever && got.Allowed {
				t.Errorf("%s about %s was admitted through %q, a credential of the "+
					"assistant's class", a, name, malformed.Via)
			}
		}
	}
	if reads == 0 || approves == 0 {
		t.Fatalf("walked %d read rows and %d approve rows, so this certifies "+
			"nothing about one of them", reads, approves)
	}
}

// sameDecision reports whether two decisions say the same thing to a caller.
func sameDecision(a, b authz.Decision) bool {
	return a.Allowed == b.Allowed && a.Reason == b.Reason &&
		a.Unknown() == b.Unknown() && slices.Equal(a.Grants, b.Grants)
}

// THE ASSISTANT IS NEVER ONE STEP FROM WHAT A MODEL MUST NOT TOUCH.
//
// ADR-0051's never list, pinned by FAMILY rather than by verb where a family
// exists, so a verb added to it later is held without anybody remembering this
// test: every credential, secret, setup and fleet verb, every company document
// verb, every identity directory verb but the org chart's seat read, every
// purge, the company-wide trail, a transfer, a forced publish, an export,
// nested model spend, the agents' own verbs, the catalogue and every
// `assistant.` verb itself (which is also a person's alone, present). And the
// writes an approval may carry are PINNED exactly, because every one of them
// is a write the assistant can be one approval away from: a verb joins that
// list in a reviewed diff here, together with the catalogue row that offers it.
//
// Mutation: mark `secrets.reveal` assist-read, or `merge_work_item`
// assist-approve, and this goes red naming it.
func TestTheAssistantIsNeverOneStepFromTheNeverList(t *testing.T) {
	t.Parallel()
	never := func(a authz.Action) bool {
		s := string(a)
		for _, family := range []string{"secrets.", "setup.", "fleet.", "config.",
			"assistant."} {
			if strings.HasPrefix(s, family) {
				return true
			}
		}
		switch {
		case strings.HasPrefix(s, "iam.") && a != authz.ActionSeatsRead,
			strings.HasSuffix(s, ".purge"):
			return true
		}
		return slices.Contains([]authz.Action{
			authz.ActionAuditRead, authz.ActionArtifactTransfer,
			authz.ActionArtifactForce, authz.ActionArtifactExport,
			authz.ActionKnowledgeAnswer, authz.ActionSandboxRun,
			authz.ActionColleagueAsk, authz.ActionCatalogueWrite,
			authz.ActionSkillPageWrite,
		}, a)
	}
	pinned := 0
	for _, a := range authz.Actions() {
		if !never(a) {
			continue
		}
		pinned++
		if class, _ := authz.AssistOf(a); class != authz.AssistNever {
			t.Errorf("%s is on the never list and states %q for the assistant", a, class)
		}
		if !strings.HasPrefix(string(a), "assistant.") {
			continue
		}
		if only, _ := authz.HumanOnlyOf(a); !only {
			t.Errorf("%s is the assistant's own verb and an agent may take it", a)
		}
		if presence, _ := authz.PresenceOf(a); presence != authz.PresenceRequired {
			t.Errorf("%s is the assistant's own verb and states the presence %q, "+
				"so a machine token may take it", a, presence)
		}
	}
	if pinned < 20 {
		t.Fatalf("the never list matched %d verbs; the families no longer name "+
			"the table's verbs", pinned)
	}

	approvable := map[authz.Action]bool{}
	for _, a := range authz.Actions() {
		if class, _ := authz.AssistOf(a); class == authz.AssistApprove {
			approvable[a] = true
		}
	}
	want := []authz.Action{
		authz.ActionWorkCreate, authz.ActionWorkUpdate, authz.ActionWorkComment,
		authz.ActionWorkRoute, authz.ActionPrioritiesSet, authz.ActionInboxMark,
		authz.ActionPageCreate, authz.ActionPageSave,
		authz.ActionSeatPause, authz.ActionSeatResume, authz.ActionTurnSteer,
		authz.ActionRunAnswer, authz.ActionArtifactShare, authz.ActionArtifactUnshare,
	}
	got := slices.Sorted(maps.Keys(approvable))
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the writes an approval may carry are %v, want %v: a write joins "+
			"this list with the catalogue row that offers it", got, want)
	}
}
