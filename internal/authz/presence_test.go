package authz_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// throughToken is p acting through a machine token, which is how the request
// guard composes a token: the owner, with the credential named beside them.
func throughToken(p iam.Principal) iam.Principal {
	p.Via = iam.MachineTokenName(uuid.Must(uuid.NewV7()).String())
	return p
}

// presenceObjects are the four things a verb can be about: the caller's own
// record (by id, which is what the directory classes compare), somebody else's,
// the company and nobody in particular.
func presenceObjects(self iam.Principal) map[string]authz.Object {
	other := uuid.New().String()
	return map[string]authz.Object{
		"the caller": {Kind: authz.KindPerson, Owner: self.ID.String(),
			ID: self.ID.String()},
		"somebody else": {Kind: authz.KindPerson, Owner: other, ID: other},
		"the company":   {Kind: authz.KindCompany},
		"nobody":        {},
	}
}

// A ROW THAT NEEDS A PERSON REFUSES A MACHINE TOKEN ON EXACTLY THE ARMS IT
// NAMES, AND NOWHERE ELSE.
//
// A token acts as its owner and is stepped up by construction, so the table's
// own refusal is the lock between it and changing how its owner proves who
// they are — no permission is asked of that self arm at all. Walked over EVERY
// row, with a principal holding every permission (so the class admits on every
// arm and the token can only be refused for being one) and its control — the same
// principal at a keyboard — over the four things a verb can be about:
//
//   - a row needing a person on every arm refuses the token on all four,
//     with [authz.ReasonTokenRefused], where the control is admitted;
//   - a row sparing the self arm admits the token about its own owner as the
//     control is admitted, and refuses it everywhere the control is admitted
//     on anything else;
//   - and every other row decides the token exactly as it decides the control.
//
// Each value has to be stated by some row, or this certifies nothing about it.
//
// Mutation: refuse the token before the class on a row that spares the self
// arm and its owner's own sessions are refused; drop the check after the class
// and an administrator's token ends somebody else's.
func TestARowThatNeedsAPersonRefusesATokenOnExactlyTheArmsItNames(t *testing.T) {
	t.Parallel()
	stated := map[authz.Presence]int{}
	for _, a := range authz.Actions() {
		presence, _ := authz.PresenceOf(a)
		if !presence.Valid() {
			t.Errorf("%s states the presence %q, which is not one of %v", a,
				presence, authz.Presences)
			continue
		}
		stated[presence]++
		keyboard := person("jane.doe", iam.AllGrants...)
		token := throughToken(keyboard)
		admittedAtAKeyboard := false
		for name, object := range presenceObjects(keyboard) {
			want := authz.Decide(t.Context(), keyboard, a, object, nimbus(), decidedAt)
			got := authz.Decide(t.Context(), token, a, object, nimbus(), decidedAt)
			if want.Unknown() || got.Unknown() {
				t.Fatalf("%s about %s was undecidable: %v / %v", a, name,
					want.Err, got.Err)
			}
			admittedAtAKeyboard = admittedAtAKeyboard || want.Allowed
			spared := presence == authz.PresenceOptional ||
				presence == authz.PresenceRequiredForOthers &&
					want.Reason == authz.ReasonSelf
			switch {
			case spared:
				if got.Allowed != want.Allowed || got.Reason != want.Reason {
					t.Errorf("%s (%q) about %s decided %+v through a token and "+
						"%+v at a keyboard — a row that needs nobody present "+
						"there decides the two alike", a, presence, name, got, want)
				}
			case presence == authz.PresenceRequired || want.Allowed:
				if got.Allowed || got.Reason != authz.ReasonTokenRefused {
					t.Errorf("%s (%q) about %s decided %+v through a token, "+
						"want refused as a token", a, presence, name, got)
				}
				if len(got.Grants) != 0 {
					t.Errorf("%s refused a token naming the grants %v — no "+
						"capability changes that answer", a, got.Grants)
				}
			}
		}
		if presence != authz.PresenceOptional && !admittedAtAKeyboard {
			t.Errorf("%s (%q) admitted the control on nothing, so its token "+
				"refusals certify nothing", a, presence)
		}
	}
	for _, p := range []authz.Presence{authz.PresenceRequired,
		authz.PresenceRequiredForOthers} {
		if stated[p] == 0 {
			t.Errorf("no row states %q, so this walk certifies nothing about it", p)
		}
	}
	if _, known := authz.PresenceOf("no.such.verb"); known {
		t.Error("PresenceOf knows a verb the table has no row for")
	}
}

// A TOKEN HOLDING ADMIN IS REFUSED EVERY PERSON-PRESENT GESTURE ABOUT ANYBODY
// BUT ITS OWNER.
//
// A machine token may carry admin — the permission it is minted with is its
// owner's to choose — so no permission it cannot hold stands between it and
// revealing a credential, editing the directory, changing how somebody signs
// in or ending everybody's sessions: the row's own presence does, and this
// walk is what holds it there. Pinned to the gestures that need a person — the
// directory's every write and every gesture on somebody's credentials or
// sessions (every directory-class row but the read), writing the directory,
// ending every session in the company, and revealing a secret — because each
// is one a pipeline has no business making and nothing but this pin would
// notice a row that stopped saying so. Over every such row, a token holding
// every permission, freshly proved, is refused as a token about somebody
// else, the company and nobody; its control, the same person at a keyboard,
// is admitted somewhere on every row.
//
// Mutation: drop the presence from the machine-token write's row and an
// administrator's token revokes somebody else's credentials here.
func TestATokenHoldingAdminIsRefusedEveryPersonPresentGestureAboutAnybodyElse(t *testing.T) {
	t.Parallel()
	need := []authz.Action{authz.ActionDirectoryWrite,
		authz.ActionSessionInvalidate, authz.ActionSecretReveal}
	for _, a := range authz.Actions() {
		if class, _ := authz.ClassOf(a); class == authz.ClassDirectory &&
			a != authz.ActionDirectoryRead {
			need = append(need, a)
		}
	}
	keyboard := person("jane.doe", iam.AllGrants...)
	token := throughToken(keyboard)
	for _, a := range need {
		if presence, _ := authz.PresenceOf(a); presence == authz.PresenceOptional {
			t.Errorf("%s states no presence, and it is a gesture a pipeline has "+
				"no business making", a)
		}
		admitted := false
		for name, object := range presenceObjects(keyboard) {
			admitted = admitted || authz.Decide(t.Context(), keyboard, a, object,
				nimbus(), decidedAt).Allowed
			if name == "the caller" {
				continue
			}
			if got := authz.Decide(t.Context(), token, a, object, nimbus(),
				decidedAt); got.Allowed || got.Reason != authz.ReasonTokenRefused {
				t.Errorf("%s about %s: a token holding admin decided %+v, want "+
					"refused as a token", a, name, got)
			}
		}
		if !admitted {
			t.Errorf("%s admitted the person at a keyboard on nothing, so its "+
				"token refusals certify nothing", a)
		}
	}
	if len(need) < 4 {
		t.Fatalf("pinned %d rows; the directory's own gestures are missing", len(need))
	}
}

// A MACHINE TOKEN NEVER CHANGES HOW ITS OWNER PROVES WHO THEY ARE — and still
// contains itself.
//
// The self arm is the one no permission guards: a person changes their own
// second factor, recovery codes and password holding nothing at all, and a
// token acts as them, stepped up. So the row is the whole of the protection
// there,
// and which rows spare the self arm is the decision this pins: a token about
// its own owner may revoke a token (itself included, from wherever somebody
// found it leaked) and end its owner's sessions (which ends it), and is
// refused every change to the proof its owner signs in with. The control is
// the owner at a keyboard, admitted to all three as themselves.
//
// Mutation: spare the proof verb's self arm and the token enrols over its
// owner's second factor here.
func TestAMachineTokenNeverChangesHowItsOwnerProvesWhoTheyAre(t *testing.T) {
	t.Parallel()
	owner := person("jane.doe", iam.Apps...)
	token := throughToken(owner)
	self := authz.Object{Kind: authz.KindPerson, Owner: owner.ID.String()}
	for _, c := range []struct {
		action       authz.Action
		throughToken bool
	}{
		{authz.ActionCredentialProof, false},
		{authz.ActionCredentialWrite, true},
		{authz.ActionSessionEnd, true},
	} {
		if d := authz.Decide(t.Context(), owner, c.action, self, nimbus(),
			decidedAt); !d.Allowed || d.Reason != authz.ReasonSelf {
			t.Errorf("the owner at a keyboard decided %+v on %s about "+
				"themselves, want admitted as themselves", d, c.action)
		}
		d := authz.Decide(t.Context(), token, c.action, self, nimbus(), decidedAt)
		switch {
		case c.throughToken && (!d.Allowed || d.Reason != authz.ReasonSelf):
			t.Errorf("a token decided %+v on %s about its own owner, want "+
				"admitted as them", d, c.action)
		case !c.throughToken && (d.Allowed || d.Reason != authz.ReasonTokenRefused):
			t.Errorf("a token decided %+v on %s about its own owner, want "+
				"refused as a token", d, c.action)
		}
	}
}

// NO TOOL NEEDS A PERSON PRESENT.
//
// A verb a seat's tools are served under carries the tool's own name, with no
// dot. Those tools are also an operator's assistant's — the operator surface
// serves the same catalogue over MCP, and an assistant connects with the
// machine token its person minted for it — so a tool row needing a person
// present would refuse every assistant every call. It is also what keeps the
// act transport's answers to the codes it declares (internal/api/operator's
// ActTransportCodes, held against the dashboard): a refusal there is mapped
// by [authz.RefusalFields], and `token_refused` is not among them because no
// tool can be refused it.
func TestNoToolNeedsAPersonPresent(t *testing.T) {
	t.Parallel()
	tools := 0
	for _, a := range authz.Actions() {
		if strings.Contains(string(a), ".") {
			continue
		}
		tools++
		if p, _ := authz.PresenceOf(a); p != authz.PresenceOptional {
			t.Errorf("the tool %s needs a person present (%s), which no "+
				"assistant acting through a token can be", a, p)
		}
	}
	if tools < 20 {
		t.Fatalf("read %d tools; a walk that stopped reading the table "+
			"certifies nothing", tools)
	}
}
