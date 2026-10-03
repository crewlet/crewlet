package authz_test

import (
	"slices"
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

// withoutPersonPresentGrants is p holding everything a machine token may carry
// and nothing it may not.
func withoutPersonPresentGrants(p iam.Principal) iam.Principal {
	var carried []iam.Grant
	for _, g := range p.Grants {
		if !slices.Contains(iam.PersonPresentGrants, g) {
			carried = append(carried, g)
		}
	}
	p.Grants = carried
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
// they are — no grant is asked of that self arm at all. Walked over EVERY row,
// with a principal holding every grant (so the class admits on every arm and
// the token can only be refused for being one) and its control — the same
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

// AN ARM ONLY A PERSON-PRESENT GRANT OPENS NEEDS A PERSON, AND ITS ROW SAYS SO.
//
// The direction that decays quietly: a new verb gated on `secrets:read` or
// `people:manage` is a gesture a pipeline has no business making, and nothing
// but memory would mark its row. Derived from the table's own decisions rather
// than a list: for every verb and every object that is not the caller, an arm
// that admits a principal holding every grant and refuses the same principal
// without the person-present ones is opened by those alone — and a token
// presenting them (as no real token can, which is what isolates the row's own
// lock) must be refused there.
//
// Mutation: drop the presence from the machine-token write's row and an
// administrator's token revokes somebody else's credentials here.
func TestAnArmOnlyAPersonPresentGrantOpensNeedsAPerson(t *testing.T) {
	t.Parallel()
	derived := 0
	for _, a := range authz.Actions() {
		keyboard := person("jane.doe", iam.AllGrants...)
		narrowed := withoutPersonPresentGrants(keyboard)
		for name, object := range presenceObjects(keyboard) {
			if name == "the caller" {
				continue
			}
			wide := authz.Decide(t.Context(), keyboard, a, object, nimbus(), decidedAt)
			narrow := authz.Decide(t.Context(), narrowed, a, object, nimbus(), decidedAt)
			if wide.Unknown() || narrow.Unknown() || !wide.Allowed || narrow.Allowed {
				continue
			}
			derived++
			if got := authz.Decide(t.Context(), throughToken(keyboard), a, object,
				nimbus(), decidedAt); got.Allowed {
				presence, _ := authz.PresenceOf(a)
				t.Errorf("%s about %s is opened only by a person-present grant, "+
					"and a token presenting one was admitted (%s): its row "+
					"states %q", a, name, got.Reason, presence)
			}
		}
	}
	if derived == 0 {
		t.Fatal("no arm of the table is opened by a person-present grant alone, " +
			"so this walk certifies nothing")
	}
}

// A GESTURE THAT NEEDS A PERSON PRESENT, ABOUT ANYBODY BUT THE CALLER, NEEDS A
// GRANT NO MACHINE TOKEN CAN CARRY.
//
// The second lock, held independently of the first: a token presenting itself
// is refused by the row ([TestARowThatNeedsAPersonRefusesATokenOnExactlyTheArmsItNames]),
// and every way into such a gesture that is not the caller acting on their OWN
// record ALSO asks a grant that needs a person present
// ([iam.PersonPresentGrants]) — which internal/iamdomain refuses at a token's
// mint and internal/iam/credential strips from what one carries on every
// request. Walked over EVERY row that states a presence, with a principal at a
// keyboard holding everything else, freshly proved, about somebody else, the
// company and nobody: none is admitted.
//
// Mutation: gate secret reveal on `secrets:write`, which a token can carry,
// and it is admitted here.
func TestAGestureThatNeedsAPersonPresentNeedsAGrantNoTokenCarries(t *testing.T) {
	t.Parallel()
	p := withoutPersonPresentGrants(person("ci.pipeline", iam.AllGrants...))
	walked := 0
	for _, a := range authz.Actions() {
		if presence, _ := authz.PresenceOf(a); presence == authz.PresenceOptional {
			continue
		}
		walked++
		for name, object := range presenceObjects(p) {
			if name == "the caller" {
				continue
			}
			if d := authz.Decide(t.Context(), p, a, object, nimbus(), decidedAt); d.Allowed {
				t.Errorf("%s about %s admitted a principal holding no grant "+
					"that needs a person present (%s) — a machine token "+
					"carrying the same would reach it but for one lock", a,
					name, d.Reason)
			}
		}
	}
	if walked == 0 {
		t.Fatal("no row needs a person present, so this walk certifies nothing")
	}
}

// A MACHINE TOKEN NEVER CHANGES HOW ITS OWNER PROVES WHO THEY ARE — and still
// contains itself.
//
// The self arm is the one no grant guards: a person changes their own second
// factor, recovery codes and password on no capability at all, and a token
// acts as them, stepped up. So the row is the whole of the protection there,
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
	owner := withoutPersonPresentGrants(person("jane.doe", iam.AllGrants...))
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
