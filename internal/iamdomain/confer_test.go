package iamdomain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// WHAT AN ENROLMENT MAY CONFER, AND ON WHOSE AUTHORITY.
//
// An enrolment is a permission change from nothing to what it carries, and it
// was held to nothing: a party allowed to enrol could enrol a colleague holding
// everything and then sign in as them. [iamdomain.Writer.UpdatePerson] already
// refused conferring what the writer does not hold; these cases hold the
// enrolment and the invitation to the same rule, read through [iam.Expand], and
// hold the two enrolments that are NOT the writer's to authorise — the first
// person and a redemption — to the basis each names.

// nodeWriter is a running node's own writer: the permissions `internal/engine`
// gives it and nothing else, and — because it is built rather than derived
// from a principal — no credential, which is what keeps the sign-in surface's
// records at the base record version and off every token's name.
func nodeWriter(rig *writeRig) *iamdomain.Writer { return rig.node }

// anAdmin is a person holding admin and nothing else written on their row.
func anAdmin(rig *writeRig) *iamdomain.Writer {
	return rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin}))
}

// AN ENROLMENT NEEDS ADMIN, AND CONFERS ONLY NAMES THIS BUILD KNOWS.
//
// Somebody using every app, and no more, enrols nobody — an enrolment decides
// who may do anything at all — and is refused before anything is claimed, so
// the refusal leaves no residue. An administrator may confer any permission,
// admin included, and never a name this build cannot read, which would ride a
// new row for good. Mutation: let MayConfer accept an unknown name and the
// second case lands.
func TestAnEnrolmentConfersOnlyWhatItsWriterHolds(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	enrol := func(w *iamdomain.Writer, login string, grants []iam.Grant) error {
		return rig.draining(func() error {
			_, err := w.Create(rig.t.Context(), iamdomain.Creation{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
				Stage: iam.StageActive, Login: login, Grants: grants,
				OpID: operationKey(), Reason: "a pipeline",
			})
			return err
		})
	}
	user := rig.writer.As(principalNamed("ana.user", iam.KindPerson, iam.Apps))
	if err := enrol(user, "ci:widen", []iam.Grant{iam.GrantWorklet}); !errors.Is(err,
		iamdomain.ErrRefused) {
		t.Errorf("a party using every app enrolled somebody (%v)", err)
	}
	if err := enrol(anAdmin(rig), "ci:future", []iam.Grant{iam.GrantWorklet,
		"swarm"}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("an administrator conferred a name this build does not know (%v)", err)
	}
	// REFUSED BEFORE THE FIRST CLAIM, so the refusal leaves nothing behind:
	// the writer's own permissions need no read, and a claimed login behind a
	// refusal that was knowable up front is residue for nothing.
	rig.drain()
	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 0 {
		t.Errorf("a refused enrolment left rows behind: %v", got)
	}
	// AND THE CONTROL: an administrator confers what admin includes.
	if err := enrol(anAdmin(rig), "ci:within", []iam.Grant{iam.GrantAdmin}); err != nil {
		t.Errorf("an administrator was refused conferring admin: %v", err)
	}
}

// A SERVICE ACCOUNT MAY HOLD ADMIN.
//
// A machine acts only through tokens, and a token may carry admin: what keeps
// it off the gestures that need a person present is the authority table's
// presence on their rows and this domain's refusal of an administrative record
// written through one — never a permission it cannot hold. So a service
// account is enrolled holding admin, and an edit may add admin to one; it used
// to be refused the two grants a token could not carry, which no longer
// exist. Mutation: refuse admin to a machine at the enrolment and the first
// case goes red.
func TestAServiceAccountMayHoldAdmin(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	machine := uuid.Must(uuid.NewV7()).String()
	if err := rig.draining(func() error {
		_, err := rig.writer.Create(rig.t.Context(), iamdomain.Creation{
			PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
			Login: "ci:release", Grants: []iam.Grant{iam.GrantAdmin},
			OpID: operationKey(), Reason: "a deploy pipeline",
		})
		return err
	}); err != nil {
		t.Fatalf("a service account was refused admin: %v", err)
	}
	rig.drain()
	other := uuid.Must(uuid.NewV7()).String()
	if err := rig.draining(func() error {
		_, err := rig.writer.Create(rig.t.Context(), iamdomain.Creation{
			PersonID: other, Kind: iam.KindMachine, Stage: iam.StageActive,
			Login: "ci:files", Grants: []iam.Grant{iam.GrantWorklet},
			OpID: operationKey(), Reason: "a pipeline",
		})
		return err
	}); err != nil {
		t.Fatalf("a service account holding Worklet was refused: %v", err)
	}
	rig.drain()
	if err := rig.draining(func() error {
		_, err := rig.writer.UpdatePerson(rig.t.Context(), iamdomain.PersonUpdate{
			PersonID: other, OpID: operationKey(), Reason: "promoted",
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Grants = append(p.Grants, iam.GrantAdmin)
				return p, nil
			},
		})
		return err
	}); err != nil {
		t.Errorf("an edit adding admin to a service account was refused: %v", err)
	}
}

func TestAnInvitationConfersOnlyWhatItsIssuerHolds(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	invite := func(w *iamdomain.Writer, address string, grants []iam.Grant) error {
		return rig.draining(func() error {
			_, err := w.Invite(rig.t.Context(), iamdomain.InviteMint{
				Email:  address,
				Grants: grants, ExpiresAt: brokerAt.Add(168 * time.Hour),
				Seat: rig.vacantSeat(seatOf(address)),
				OpID: operationKey(), Reason: "onboarding",
			})
			return err
		})
	}
	user := rig.writer.As(principalNamed("ana.user", iam.KindPerson, iam.Apps))
	if err := invite(user, "sarah@example.com",
		[]iam.Grant{iam.GrantWorklet}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a party using every app issued an invitation (%v) — an "+
			"invitation decides who may do anything at all", err)
	}
	if err := invite(anAdmin(rig), "kim@example.com",
		[]iam.Grant{"swarm"}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("an invitation conferred a name this build does not know (%v) "+
			"— the redemption would hand out what nobody here can read", err)
	}
	if err := invite(anAdmin(rig), "ravi@example.com",
		[]iam.Grant{iam.GrantAdmin}); err != nil {
		t.Errorf("an invitation conferring what the issuer holds was "+
			"refused: %v", err)
	}
}

// A REDEMPTION CONFERS WHAT THE INVITATION SAID, and the node's own writer —
// which is what processes every redemption — may not stand in for it.
func TestARedemptionConfersWhatTheInvitationSaid(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	offered := []iam.Grant{iam.GrantWorklet, iam.GrantDoclet}
	// THE LINK'S SECRET AND THE SEAT, per invitation: a redemption presents
	// the one and binds the other, and these cases are about what the
	// invitation confers.
	secrets, seats := map[string]string{}, map[string]string{}
	var redeemAs func(person, invitation, address string, grants []iam.Grant) error
	issue := func(address string) string {
		t.Helper()
		var id string
		seat := rig.vacantSeat(seatOf(address))
		if err := rig.draining(func() error {
			issued, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
				Email: address, Grants: offered, Seat: seat,
				ExpiresAt: brokerAt.Add(168 * time.Hour),
				OpID:      operationKey(), Reason: "onboarding",
			})
			id = issued.ID
			secrets[id], seats[id] = issued.Secret, seat
			return err
		}); err != nil {
			t.Fatalf("invite %s: %v", address, err)
		}
		rig.drain()
		return id
	}
	redeem := func(invitation, address string, grants []iam.Grant) error {
		return redeemAs(uuid.Must(uuid.NewV7()).String(), invitation, address,
			grants)
	}
	redeemAs = func(person, invitation, address string, grants []iam.Grant) error {
		return rig.draining(func() error {
			_, err := nodeWriter(rig).Redeem(rig.t.Context(), iamdomain.Redemption{
				PersonID: person, Stage: iam.StageActive,
				Name: "A joiner", Email: address, Login: iam.LoginFromAddress(address),
				Password: aPassword(), Grants: grants, Seat: seats[invitation],
				Invitation: invitation, InvitationSecret: secrets[invitation],
				OpID: "op-redeem-" + person, Reason: "redeemed an invitation",
			})
			return err
		})
	}

	sarah := issue("sarah@example.com")
	if err := redeem(sarah, "sarah@example.com", offered); err != nil {
		t.Fatalf("redeeming exactly what was offered was refused: %v — the "+
			"invitation is the authority", err)
	}

	for _, refused := range []struct {
		name    string
		address string
		grants  []iam.Grant
		other   string
	}{
		{"a permission the invitation did not carry", "a1@example.com",
			[]iam.Grant{iam.GrantWorklet, iam.GrantAdmin}, ""},
		{"an address the invitation was not issued to", "a3@example.com",
			offered, "someone-else@example.com"},
	} {
		invitation := issue(refused.address)
		address := refused.address
		if refused.other != "" {
			address = refused.other
		}
		if err := redeem(invitation, address,
			refused.grants); !errors.Is(err, iamdomain.ErrRefused) {
			t.Errorf("a redemption asking for %s was not refused (%v)",
				refused.name, err)
		}
	}
	// A REFUSAL PUBLISHES NOTHING: the invitation is read in the record's
	// own snapshot, so a redemption it does not cover leaves nothing
	// holding the joiner's address against the corrected retry. Sarah is
	// the only row.
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 1 {
		t.Errorf("refused redemptions left rows behind: %v", rows)
	}

	// A SPENT LINK IS NOBODY'S AUTHORITY, however many times it is shown —
	// including once the address it enrolled is free again. The person it
	// created holds the address, so the directory refuses a second
	// redemption while they are there; what is left to refuse is the link
	// outliving them, which removing them stands in for here.
	spent := issue("spent@example.com")
	redeemer := uuid.Must(uuid.NewV7()).String()
	if err := redeemAs(redeemer, spent, "spent@example.com", offered); err != nil {
		t.Fatalf("the first redemption: %v", err)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.Remove(rig.t.Context(), redeemer, "op-remove",
			"the redeemer left")
		return err
	}); err != nil {
		t.Fatalf("remove the redeemer: %v", err)
	}
	rig.drain()
	if err := redeem(spent, "spent@example.com",
		offered); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("an invitation somebody already redeemed enrolled a second "+
			"person once the first had gone (%v)", err)
	}

	// AN INVITATION OFFERING ADMIN OFFERS EVERY APP WITH IT, read through
	// [iam.Expand]: the redemption may spell the apps out beside admin, as a
	// picker that ticks and locks them does.
	admin := rig.vacantSeat(seatOf("boss@example.com"))
	var bossInvite iamdomain.InviteIssued
	if err := rig.draining(func() error {
		var err error
		bossInvite, err = rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
			Email: "boss@example.com", Grants: []iam.Grant{iam.GrantAdmin},
			Seat: admin, ExpiresAt: brokerAt.Add(168 * time.Hour),
			OpID: operationKey(), Reason: "onboarding",
		})
		return err
	}); err != nil {
		t.Fatalf("invite an administrator: %v", err)
	}
	rig.drain()
	secrets[bossInvite.ID], seats[bossInvite.ID] = bossInvite.Secret, admin
	if err := redeem(bossInvite.ID, "boss@example.com", iam.AllGrants); err != nil {
		t.Errorf("redeeming admin and the apps it includes was refused: %v", err)
	}
}

// THE FIRST PERSON IS INVITED UNDER A TIER A TOKEN, on an estate with nobody
// in it, and carries what the token could confer.
//
// There is no exemption for them. A company with nobody in it still holds the
// deployment's own token, which is a party like any other: its invitation is
// held to its grants, and the node's own writer redeems it on the invitation's
// authority exactly as it redeems every later one. Nothing on the way may ask
// for somebody to be enrolled already — that would be a company nobody could
// ever enter.
//
// Mutation: hold the invitation to an enrolled issuer and the first issue is
// refused.
func TestTheFirstPersonIsInvitedUnderATierAToken(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	if anybody, err := rig.anybody(t); err != nil || anybody {
		t.Fatalf("the rig's estate is not empty (%v, %v)", anybody, err)
	}
	// THE FOUNDER'S SEAT, which the company declares before anybody can be
	// invited onto it: every person holds one.
	founder := rig.vacantSeat("founder")
	token := rig.writer.As(principalNamed("token:ops", iam.KindMachine, iam.AllGrants))
	var issued iamdomain.InviteIssued
	if err := rig.draining(func() error {
		var err error
		issued, err = token.Invite(rig.t.Context(), iamdomain.InviteMint{
			Email: "founder@example.com", Grants: iam.AllGrants,
			Seat: founder, ExpiresAt: brokerAt.Add(168 * time.Hour),
			OpID: operationKey(), Reason: "the first person",
		})
		return err
	}); err != nil {
		t.Fatalf("a Tier A token could not invite the first person into an "+
			"empty estate: %v", err)
	}
	rig.drain()
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.draining(func() error {
		_, err := nodeWriter(rig).Redeem(rig.t.Context(), iamdomain.Redemption{
			PersonID: person, Stage: iam.StageActive,
			Name: "The founder", Email: "founder@example.com",
			Login: "the.founder", Password: aPassword(),
			Grants: iam.AllGrants, Seat: founder,
			Invitation: issued.ID, InvitationSecret: issued.Secret,
			OpID: "op-redeem-" + person, Reason: "redeemed the first invitation",
		})
		return err
	}); err != nil {
		t.Fatalf("the first person was refused the permissions their "+
			"invitation carried: %v — the invitation is the authority", err)
	}
	rig.drain()
	held, err := rig.reader(t).PersonByLogin(rig.t.Context(), "the.founder")
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Grants) != len(iam.AllGrants) {
		t.Errorf("the first person carries %v, want every permission the "+
			"token conferred", held.Grants)
	}
	if anybody, err := rig.anybody(t); err != nil || !anybody {
		t.Errorf("the estate still reads as empty once the first person is "+
			"in (%v, %v)", anybody, err)
	}
}

// operationKey is a fresh operation key, which a create's id is derived from:
// a uuid7, as every one the surfaces mint is.
func operationKey() string { return uuid.Must(uuid.NewV7()).String() }

// blindOf is the keyed blind this rig's writer derives for an address, which is
// what the directory compares it by.
func blindOf(t *testing.T, address string) string {
	t.Helper()
	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("NewBlinder: %v", err)
	}
	blind, err := blinder.Email(address)
	if err != nil {
		t.Fatalf("blind %s: %v", address, err)
	}
	return blind
}
