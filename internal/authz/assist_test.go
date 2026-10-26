package authz_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE DASHBOARD ASSISTANT'S COLUMN OF THE AUTHORITY TABLE (ADR-0052).
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
// the three. Each class must be answered by some verb or the walks below
// certify nothing about it. A verb is never the assistant's exactly when the
// human-only list names it, with a reason. And a seat-only verb can only be
// never: an assistant acts as a person, who never takes one. (That a row
// states its nature and never Never, so the list is the one place the
// assistant is kept from a verb, is held inside the package.)
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
		if only, _ := authz.SeatOnlyOf(a); only && class != authz.AssistNever {
			t.Errorf("%s is an agent's own and answers %q for the assistant, "+
				"which acts as a person", a, class)
		}
		why, listed := authz.AssistNeverReason(a)
		if listed != (class == authz.AssistNever) || listed && strings.TrimSpace(why) == "" {
			t.Errorf("%s answers %q for the assistant and is listed %v with the reason "+
				"%q: a verb is never the assistant's exactly when the human-only list "+
				"names it, with its reason", a, class, listed, why)
		}
	}
	for _, c := range authz.AssistClasses {
		if stated[c] == 0 {
			t.Errorf("no row answers %q, so nothing here certifies it", c)
		}
	}
	if _, known := authz.AssistOf("no.such.verb"); known {
		t.Error("AssistOf knows a verb the table has no row for")
	}
	if authz.AssistClass("").Valid() {
		t.Error("the zero assist class is valid, so a row nobody decided passes")
	}
}

// AN ASSIST-NEVER VERB REFUSES AN ASSISTED PRINCIPAL, UNDER EITHER SCOPE.
//
// Walked over every verb on the human-only list, with a person holding every
// permission and its control, the same person at their keyboard, admitted on
// some object of the row: through the assistant the verb is refused on every
// object, under a read grant and under an exec grant alike, naming
// [authz.ReasonAssistRefused] and no permission (none would change it). A
// seat-only verb is refused before the assistant's bar is reached, as the
// person's own click is. And the bar keeps the artifact rows' concealment: a
// person with no relation to an artifact is told it does not exist, through
// the assistant as at their keyboard, under either scope.
//
// Mutation: drop the never arm from the bar and every verb here is admitted.
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
	if rows < 10 {
		t.Fatalf("walked %d never verbs; the human-only list is not being read", rows)
	}

	// THE CONCEALMENT: no relation to an artifact is no artifact, through
	// the assistant as at the keyboard, the scope bar included.
	stranger := personLeading("dana", iam.GrantArtifacts)
	private := authz.Object{Kind: authz.KindArtifact, ID: "a-private", Owner: "kai"}
	for _, a := range []authz.Action{authz.ActionArtifactTrash,
		authz.ActionArtifactForce, authz.ActionArtifactTransfer} {
		if want := authz.Decide(t.Context(), stranger, a, private, nimbus(),
			decidedAt); !want.NotFound() {
			t.Fatalf("the control decided %+v on %s, want not found", want, a)
		}
		for _, scope := range iam.AssistScopes {
			if got := authz.Decide(t.Context(), assisted(stranger, scope), a,
				private, nimbus(), decidedAt); !got.NotFound() {
				t.Errorf("%s on an artifact the person holds no relation to decided "+
					"%+v through a %s grant, which names that it exists", a, got, scope)
			}
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
		presence, _ := authz.PresenceOf(a)
		for name, object := range assistObjects(keyboard) {
			want := authz.Decide(t.Context(), keyboard, a, object, nimbus(), decidedAt)
			read := authz.Decide(t.Context(), assisted(keyboard, iam.AssistScopeRead), a,
				object, nimbus(), decidedAt)
			exec := authz.Decide(t.Context(), assisted(keyboard, iam.AssistScopeExec), a,
				object, nimbus(), decidedAt)
			switch class {
			case authz.AssistRead:
				if presence != authz.PresenceOptional {
					t.Errorf("%s needs a person present and is a read the "+
						"assistant takes unasked", a)
				}
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

// THE HUMAN-ONLY LIST IS EXACTLY THE INTENDED SET.
//
// ADR-0052's parity rule: the person's assistant takes every verb the person
// may take, as a read or as the one change they approved, but for the verbs
// on the human-only list. That list is PINNED here verb by verb, because each
// entry is a decision a reviewer makes (a sign-in or session ceremony, a
// credential or secret in clear, the assistant's own decisions, an effect a
// grant would falsify or hide, an agent seat's own verb), and the walk holds both directions: a verb leaving the
// list opens it to the assistant, and a verb joining it takes it from every
// person's assistant, so either is a diff here. The families it is drawn from
// are walked too, so a credential or session verb added to the table later is
// held without anybody remembering this test, and every `assistant.` verb is
// also a person's alone, present.
//
// Mutation: drop `secrets.reveal` from the list, or add `config.write` to it,
// and this goes red naming it.
func TestTheHumanOnlyListIsExactlyTheIntendedSet(t *testing.T) {
	t.Parallel()
	want := []authz.Action{
		// sign-in and session ceremonies
		authz.ActionCredentialWrite, authz.ActionCredentialProof, authz.ActionSignInWrite,
		authz.ActionSessionEnd, authz.ActionSessionInvalidate,
		// a credential or a secret in clear
		authz.ActionSecretReveal, authz.ActionSecretWrite, authz.ActionSetupConnect,
		authz.ActionArtifactTicket,
		// the assistant's own decisions
		authz.ActionAssistantConverse, authz.ActionAssistantConversationRead,
		authz.ActionAssistantApprove, authz.ActionAssistantDecline,
		authz.ActionAssistantAuditRead,
		// an effect a grant would falsify or hide
		authz.ActionKnowledgeAnswer, authz.ActionArtifactReady,
		// an agent seat's own
		authz.ActionColleagueAsk,
	}
	var got []authz.Action
	for _, a := range authz.Actions() {
		if class, _ := authz.AssistOf(a); class == authz.AssistNever {
			got = append(got, a)
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the human-only list is %v, want %v: a verb joins or leaves it in a "+
			"reviewed diff here", got, want)
	}
	// THE FAMILIES, so a later verb of one is held by its prefix.
	family := func(a authz.Action) bool {
		s := string(a)
		for _, prefix := range []string{"assistant.", "iam.credential.", "iam.session.",
			"iam.signin."} {
			if strings.HasPrefix(s, prefix) {
				return true
			}
		}
		only, _ := authz.SeatOnlyOf(a)
		return only || a == authz.ActionSessionInvalidate
	}
	for _, a := range authz.Actions() {
		if !family(a) {
			continue
		}
		if class, _ := authz.AssistOf(a); class != authz.AssistNever {
			t.Errorf("%s is a ceremony or a verb of the assistant's own, and answers %q "+
				"for the assistant", a, class)
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
	// AND THE PRINCIPLES ARE STILL DECIDED AS THE DOCUMENT IS: their own
	// verbs, at the class and window of config.read and config.write.
	for a, document := range map[authz.Action]authz.Action{
		authz.ActionPrinciplesRead:  authz.ActionConfigRead,
		authz.ActionPrinciplesWrite: authz.ActionConfigWrite,
	} {
		class, _ := authz.ClassOf(a)
		recency, _ := authz.RecencyOf(a)
		wantClass, _ := authz.ClassOf(document)
		wantRecency, _ := authz.RecencyOf(document)
		if class != wantClass || recency != wantRecency {
			t.Errorf("%s is decided by %s at %s, not as %s is (%s at %s)", a, class,
				recency, document, wantClass, wantRecency)
		}
	}
}
