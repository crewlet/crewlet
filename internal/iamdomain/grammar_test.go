package iamdomain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A LOGIN IS HELD TO ITS HOLDER'S KIND AT ENROLMENT.
//
// A person's login is dotted and a machine's is coloned, and "either one, for
// either kind" was the hole: `token:<id>` is the login a Tier A token acts
// under, and the directory row holding it is what binds that token to a seat —
// so a person enrolled as `token:ops` (an invitation's redeemer types their
// own login) made the deployment's `ops` credential act as their seat. The
// refusals are asserted on the SENTINEL and on the ESTATE, because an
// enrolment that failed for an unrelated reason would pass an "it errored"
// check whatever the rule said.
func TestAnEnrolmentHoldsALoginToItsHoldersKind(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	for i, tc := range []struct {
		name  string
		kind  iam.Kind
		login string
		email string
	}{
		{"a person taking a Tier A token's login", iam.KindPerson,
			"token:ops", "mallory@example.com"},
		{"a person taking any machine handle", iam.KindPerson,
			"ci:release", "mallory@example.com"},
		{"a machine taking a person's login", iam.KindMachine,
			"dana.sre", ""},
		// THE BOUND IS THE GRAMMAR'S, so the domain inherits it rather
		// than restating it: sixty-five bytes of dotted login.
		{"a person's login past the bound", iam.KindPerson,
			strings.Repeat("d", iam.MaxLogin-3) + ".sre", "dana@example.com"},
	} {
		_, err := rig.writer.Enrol(rig.t.Context(), iamdomain.Enrolment{
			PersonID: "018f3a9c-0000-7000-8000-00000000010" + string(rune('a'+i)),
			Kind:     tc.kind, Stage: iam.StageActive,
			Name: "Somebody", Email: tc.email, Login: tc.login,
			OpID: "op-refused-" + string(rune('a'+i)), Reason: "a hire",
		})
		if !errors.Is(err, iamdomain.ErrInvalidLogin) {
			t.Errorf("%s: refused with %v, want %v", tc.name, err,
				iamdomain.ErrInvalidLogin)
		}
	}
	// NOTHING WAS CLAIMED. The grammar is checked before the first
	// append, so a refused enrolment leaves no reservation holding the
	// name — a reservation under `token:ops` would be half of the binding
	// this rule exists to refuse.
	rig.drain()
	if got := rig.column(`SELECT login FROM iam_people`); len(got) != 0 {
		t.Errorf("a refused enrolment left logins %v in the estate", got)
	}

	// THE DIRECTORY ENROLS PEOPLE AND MACHINES AND NOTHING ELSE. A seat is
	// the chart's and the engine is the node, and neither has a login
	// grammar to hold one to.
	for _, kind := range []iam.Kind{iam.KindSeat, iam.KindEngine} {
		_, err := rig.writer.Enrol(rig.t.Context(), iamdomain.Enrolment{
			PersonID: "018f3a9c-0000-7000-8000-0000000002ff",
			Kind:     kind, Stage: iam.StageActive,
			Name: "Somebody", Email: "somebody@example.com",
			OpID: "op-" + string(kind), Reason: "a hire",
		})
		if !errors.Is(err, iamdomain.ErrNotEnrollable) {
			t.Errorf("enrolling kind %q was refused with %v, want %v", kind, err,
				iamdomain.ErrNotEnrollable)
		}
	}

	// THE CONTROLS: each kind takes its own grammar, including the name the
	// rule is about — a MACHINE holding `token:ops` is exactly how an
	// operator binds that token to a seat.
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000003a1",
		Kind:     iam.KindMachine, Stage: iam.StageActive,
		Name: "The ops credential", Login: "token:ops",
		OpID: "op-machine", Reason: "bind the ops token",
	}); err != nil {
		t.Errorf("a machine could not take token:ops: %v", err)
	}
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000003a2",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-person", Reason: "a hire",
	}); err != nil {
		t.Errorf("a person could not take sarah.chen: %v", err)
	}
}

// EVERY PERSON ENROLS WITH A LOGIN, although their address already finds them.
//
// A login is the name a principal acts and is written under while they hold
// no seat. A person enrolled by address alone was recorded as `anonymous`
// beside every change they made — iam.ActorFor had no name to write — and
// failed iam.Principal.Validate on every request they sent, so the directory
// admitted a person the rest of the engine could not name. Refused before the
// first claim, so the refusal leaves nothing holding the address.
func TestEveryPersonEnrolsWithALogin(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	_, err := rig.writer.Enrol(rig.t.Context(), iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000005a1",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Sre", Email: "dana@example.com",
		OpID: "op-loginless", Reason: "a hire",
	})
	if !errors.Is(err, iamdomain.ErrInvalidLogin) {
		t.Errorf("a person with no login was refused with %v, want %v — "+
			"every change they made would be recorded as anonymous", err,
			iamdomain.ErrInvalidLogin)
	}
	rig.drain()
	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 0 {
		t.Errorf("a refused enrolment left rows %v holding the address", got)
	}
	// THE CONTROL: the same person with a login lands, and the name the
	// directory holds is the one every unbound change is written under.
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000005a2",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Sre", Email: "dana@example.com", Login: "dana.sre",
		OpID: "op-login", Reason: "a hire",
	}); err != nil {
		t.Fatalf("a person with a login was refused: %v", err)
	}
	rig.drain()
	if got := rig.column(`SELECT login FROM iam_people`); len(got) != 1 ||
		got[0] != "dana.sre" {
		t.Errorf("the directory holds logins %v, want [dana.sre]", got)
	}
}

// AND AT A RENAME, where the holder's kind is read INSIDE THE SNAPSHOT.
//
// A rename is a login claim on its own subject — `PATCH /iam/people/{id}`
// releases the old one and claims the new — and it passed through no grammar
// at all, so a person refused `token:ops` at enrolment could rename into it a
// minute later. The kind is read in the decide's own transaction rather than
// taken from a caller, because a caller stating it would be stating what it
// read somewhere else.
func TestARenameHoldsALoginToItsHoldersKind(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const person, machine = "018f3a9c-0000-7000-8000-0000000004a1",
		"018f3a9c-0000-7000-8000-0000000004a2"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-person",
	}); err != nil {
		t.Fatalf("enrol the person: %v", err)
	}
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
		Name: "Release pipeline", Login: "svc:ci", OpID: "op-machine",
	}); err != nil {
		t.Fatalf("enrol the machine: %v", err)
	}
	rig.drain()

	if err := rig.claim(iamdomain.KindLogin, "token:ops", person,
		"op-rename-person"); !errors.Is(err, iamdomain.ErrInvalidLogin) {
		t.Errorf("a person renamed into token:ops was refused with %v, want %v "+
			"— they would make the ops credential act as their seat", err,
			iamdomain.ErrInvalidLogin)
	}
	if err := rig.claim(iamdomain.KindLogin, "dana.sre", machine,
		"op-rename-machine"); !errors.Is(err, iamdomain.ErrInvalidLogin) {
		t.Errorf("a machine renamed into dana.sre was refused with %v, want %v",
			err, iamdomain.ErrInvalidLogin)
	}
	// SOMEBODY THIS NODE DOES NOT HOLD has no kind to judge a login
	// against, and is refused rather than guessed about.
	if err := rig.claim(iamdomain.KindLogin, "nobody.here",
		"018f3a9c-0000-7000-8000-0000000004ff",
		"op-rename-nobody"); !errors.Is(err, iamdomain.ErrNotFound) {
		t.Errorf("a rename of somebody this node does not hold was refused "+
			"with %v, want %v", err, iamdomain.ErrNotFound)
	}
	rig.drain()
	if got := rig.column(`SELECT login FROM iam_people ORDER BY login`); len(got) != 2 ||
		got[0] != "sarah.chen" || got[1] != "svc:ci" {
		t.Errorf("after the refused renames the logins are %v, want "+
			"[sarah.chen svc:ci]", got)
	}

	// THE CONTROL: a rename within the kind's own grammar lands.
	if err := rig.claim(iamdomain.KindLogin, "sarah.c.chen", person,
		"op-rename-ok"); err != nil {
		t.Fatalf("a person could not rename to sarah.c.chen: %v", err)
	}
	rig.drain()
	if got := rig.column(`SELECT login FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != "sarah.c.chen" {
		t.Errorf("the rename left %v, want [sarah.c.chen]", got)
	}
}
