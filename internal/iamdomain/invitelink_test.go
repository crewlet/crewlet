package iamdomain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// inviteFor issues one invitation through the rig's writer, answering what the
// issue handed back — the id AND the secret its link carries.
func inviteFor(t *testing.T, rig *writeRig, address, seat string) (
	iamdomain.InviteIssued, error) {

	t.Helper()
	var issued iamdomain.InviteIssued
	err := rig.draining(func() error {
		var err error
		issued, err = rig.writer.Invite(t.Context(), iamdomain.InviteMint{
			Email: address, Grants: []iam.Grant{iam.GrantStateRead},
			Seat: seat, ExpiresAt: brokerAt.Add(168 * time.Hour),
			OpID: operationKey(), Reason: "onboarding",
		})
		return err
	})
	if err == nil {
		rig.drain()
	}
	return issued, err
}

// redeemAs enrols the person an invitation creates through the node's own
// writer, as the sign-in surface does, presenting secret and binding seat —
// a fresh person per attempt, as the surface mints one, carrying the password
// they chose.
func redeemAs(t *testing.T, rig *writeRig, issued iamdomain.InviteIssued,
	address, secret, seat string) (statelog.Result, error) {

	t.Helper()
	person := uuid.Must(uuid.NewV7()).String()
	var result statelog.Result
	err := rig.draining(func() error {
		var err error
		result, err = nodeWriter(rig).Redeem(t.Context(), iamdomain.Redemption{
			PersonID: person, Stage: iam.StageActive,
			Name: "A joiner", Email: address, Login: iam.LoginFromAddress(address),
			Password:   aPassword(),
			Grants:     []iam.Grant{iam.GrantStateRead},
			Invitation: issued.ID, InvitationSecret: secret, Seat: seat,
			OpID: "invite:" + issued.ID, Reason: "redeemed an invitation",
		})
		return err
	})
	return result, err
}

// AN INVITATION KEEPS ITS LINK'S SECRET ONLY AS A VERIFIER.
//
// The id is the row's primary key, in the record that issued it, in every
// snapshot and backup and in the path of every request that reads it — so when
// the link was the id alone, each of those held a working invitation. The link
// now carries a secret beside the id, and what the estate keeps of it is its
// SHA-256: the document says the verifier, no row anywhere says the secret,
// and the one question a reader can ask of the row is whether a presented
// secret is the one.
//
// Mutation: store the secret itself as the verifier and the document case
// fails; compare without the hash, or admit an empty verifier, and Admits
// answers the wrong way.
func TestAnInvitationKeepsItsLinksSecretOnlyAsAVerifier(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	issued, err := inviteFor(t, rig, "priya@example.com",
		rig.vacantSeat("platform-lead"))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if issued.Secret == "" {
		t.Fatal("the issue handed back no secret: the link would carry the id alone")
	}
	documents := rig.column(`SELECT document FROM iam_invites WHERE id = ?`, issued.ID)
	if len(documents) != 1 {
		t.Fatalf("the invitation's row: %v", documents)
	}
	stored, err := iamdomain.DecodeInvitation([]byte(documents[0]))
	if err != nil {
		t.Fatalf("decode the stored invitation: %v", err)
	}
	if stored.Verifier != iamdomain.InvitationVerifier(issued.Secret) {
		t.Errorf("the stored verifier is %q, want the SHA-256 of the link's "+
			"secret", stored.Verifier)
	}
	for _, table := range []string{"iam_invites", "iam_history"} {
		for _, row := range rig.column(`SELECT CAST(document AS TEXT) FROM ` +
			table + ` WHERE document IS NOT NULL`) {
			if strings.Contains(row, issued.Secret) {
				t.Errorf("%s holds the link's secret in the clear: %s", table, row)
			}
		}
	}

	row, err := rig.invitationRow(t, issued.ID)
	if err != nil {
		t.Fatalf("read the invitation: %v", err)
	}
	if !row.Admits(issued.Secret) {
		t.Error("the row refused the secret its own link carries")
	}
	for _, wrong := range []string{"", issued.ID, issued.Secret + "x",
		iamdomain.InvitationVerifier(issued.Secret)} {
		if row.Admits(wrong) {
			t.Errorf("the row admitted %q, which is not the link's secret", wrong)
		}
	}
	// AND AN INVITATION WITH NO VERIFIER ADMITS NOBODY — one issued before
	// links carried a secret, which admitting on its id would reopen.
	if (iamdomain.InvitationRow{ID: issued.ID}).Admits(issued.Secret) {
		t.Error("an invitation with no verifier admitted a secret")
	}
}

// A REDEMPTION PRESENTS THE LINK'S SECRET, and the record that creates the
// person checks it in its own snapshot.
//
// The route checks it too, but a record can be published by more than a route,
// and naming an invitation's id — which every snapshot holds — is not holding
// its link. A wrong or absent secret is refused in the record's own snapshot,
// so it leaves nothing behind; the right one lands.
//
// Mutation: drop the secret from the person record's basis and the wrong
// secret enrols the person.
func TestARedemptionPresentsItsLinksSecret(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	issued, err := inviteFor(t, rig, "sam@example.com", seat)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	for _, wrong := range []string{"", "not-the-secret", issued.ID} {
		if _, err := redeemAs(t, rig, issued, "sam@example.com", wrong,
			seat); !errors.Is(err, iamdomain.ErrRefused) {
			t.Errorf("a redemption presenting %q was not refused (%v)", wrong, err)
		}
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("refused redemptions left rows behind: %v", rows)
	}
	result, err := redeemAs(t, rig, issued, "sam@example.com", issued.Secret, seat)
	if err != nil || result.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the redemption presenting the link's secret answered %+v (%v)",
			result, err)
	}
}

// A REDEMPTION CARRIES ONE CREDENTIAL, AND IT IS A PASSWORD.
//
// The invitee presents a link and a password they chose, and nothing they
// presented could vouch for any other credential: a token carried here would
// skip the mint's cut to what its owner holds, a second factor would be one
// nobody enrolled, and a reset link one nobody was shown. The type holds a
// redemption to one credential; its validation holds that one to the
// password's method, and refuses the rest before anything is published.
//
// Mutation: drop the method from the redemption's validation and each row
// lands a person holding that credential instead.
func TestARedemptionCarriesOnlyThePasswordItsInviteeChose(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	issued, err := inviteFor(t, rig, "sam@example.com", seat)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	before, err := rig.end(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []iamdomain.CredentialMethod{iamdomain.MethodToken,
		iamdomain.MethodReset, iamdomain.MethodTOTP} {
		carried := aPassword()
		carried.Method = method
		err := rig.during(func() error {
			_, err := nodeWriter(rig).Redeem(t.Context(), iamdomain.Redemption{
				PersonID: uuid.Must(uuid.NewV7()).String(), Stage: iam.StageActive,
				Name: "Sam", Email: "sam@example.com", Login: "sam.example",
				Password: carried, Grants: []iam.Grant{iam.GrantStateRead},
				Invitation: issued.ID, InvitationSecret: issued.Secret, Seat: seat,
				OpID: "invite:" + issued.ID, Reason: "redeemed an invitation",
			})
			return err
		})
		if !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("a redemption carrying a %s credential answered %v, want %v",
				method, err, iamdomain.ErrInvalid)
		}
	}
	if after, _ := rig.end(t.Context()); after != before {
		t.Errorf("the refused redemptions published %d records", after-before)
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("refused redemptions left %v", rows)
	}
	// THE CONTROL: the same redemption carrying a password lands.
	if _, err := redeemAs(t, rig, issued, "sam@example.com", issued.Secret,
		seat); err != nil {
		t.Errorf("the redemption carrying a password was refused: %v", err)
	}
}

// AN INVITATION BINDS ONLY A HUMAN SEAT NOBODY HOLDS, decided when it is issued
// — and it binds one: the person it creates holds a seat from the moment they
// exist (ADR-0026).
//
// Each refusal is one the person the link is sent to could do nothing about: no
// seat at all, a seat nobody created, an agent's seat — a person bound to one
// is refused on every request — a seat a colleague is already bound to, and a
// seat another open invitation is on its way to. What it records is the seat's
// handle, which is its identity.
//
// Mutation: drop the seat requirement and the seatless invitation is issued;
// drop the kind check and the agent seat is; drop the holder check and the
// held seat is; drop the open-invitation check and the invited seat is.
func TestAnInvitationBindsOnlyAHumanSeatNobodyHolds(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	rig.seatOnly("platform-lead")
	rig.agentSeat("release-bot")
	const colleague = "018f3a9c-0000-7000-8000-0000000000c1"
	if err := rig.enrol(iamdomain.Creation{
		PersonID: colleague, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Colleague", Email: "colleague@example.com", Login: "col.league",
		Seat: rig.vacantSeat("held-seat"), LinkExpiresAt: firstLinkExpiry,
		OpID: "op-colleague",
	}); err != nil {
		t.Fatalf("enrol the colleague: %v", err)
	}
	invited, err := inviteFor(t, rig, "invited@example.com",
		rig.vacantSeat("invited-seat"))
	if err != nil {
		t.Fatalf("invite onto invited-seat: %v", err)
	}

	for _, refused := range []struct {
		seat string
		want error
	}{
		{"", iamdomain.ErrSeatRequired},
		{"nobody-made-this", iamdomain.ErrInvalid},
		{"release-bot", iamdomain.ErrInvalid},
	} {
		if _, err := inviteFor(t, rig, refused.seat+"@example.com",
			refused.seat); !errors.Is(err, refused.want) {
			t.Errorf("an invitation binding %s was not refused with %v (%v)",
				refused.seat, refused.want, err)
		}
	}
	var taken *iamdomain.ErrTaken
	if _, err := inviteFor(t, rig, "second@example.com",
		"held-seat"); !errors.As(err, &taken) || taken.Person != colleague {
		t.Errorf("an invitation binding a seat the colleague holds was not "+
			"refused naming them (%v)", err)
	}
	taken = nil
	if _, err := inviteFor(t, rig, "third@example.com",
		"invited-seat"); !errors.As(err, &taken) ||
		taken.Field != iamdomain.UniqueSeat || taken.Invitation != invited.ID {
		t.Errorf("an invitation onto a seat another open invitation holds was "+
			"not refused naming that invitation (%v)", err)
	}
	if rows := rig.column(`SELECT id FROM iam_invites`); len(rows) != 1 ||
		rows[0] != invited.ID {
		t.Errorf("refused issues left invitations behind: %v", rows)
	}

	issued, err := inviteFor(t, rig, "lead@example.com", "platform-lead")
	if err != nil {
		t.Fatalf("an invitation binding a human seat nobody holds was refused: %v", err)
	}
	documents := rig.column(`SELECT document FROM iam_invites WHERE id = ?`, issued.ID)
	stored, err := iamdomain.DecodeInvitation([]byte(documents[0]))
	if err != nil {
		t.Fatalf("decode the stored invitation: %v", err)
	}
	if stored.Seat != "platform-lead" {
		t.Errorf("the invitation recorded seat %q, want platform-lead", stored.Seat)
	}
}

// A REDEMPTION BINDS THE SEAT ITS INVITATION NAMED, and nothing else.
//
// A redemption naming another seat, or none, asks for something the invitation
// did not offer and is refused as the link's refusal ([iamdomain.ErrRefused]),
// publishing nothing.
//
// Mutation: drop the seat comparison from the person record's basis and the
// redemption naming no seat spends the link without the binding.
func TestARedemptionBindsTheSeatItsInvitationNamed(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	rig.seatOnly("platform-lead")
	rig.seatOnly("other-seat")
	issued, err := inviteFor(t, rig, "lead@example.com", "platform-lead")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	row, err := rig.invitationRow(t, issued.ID)
	if err != nil {
		t.Fatalf("read the invitation: %v", err)
	}
	if row.Seat != "platform-lead" {
		t.Errorf("the row names seat %q, want platform-lead", row.Seat)
	}

	for _, other := range []string{"", "other-seat"} {
		if _, err := redeemAs(t, rig, issued, "lead@example.com", issued.Secret,
			other); !errors.Is(err, iamdomain.ErrRefused) {
			t.Errorf("a redemption binding %q, where the invitation binds "+
				"platform-lead, was not refused (%v)", other, err)
		}
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("refused redemptions left rows behind: %v", rows)
	}

	result, err := redeemAs(t, rig, issued, "lead@example.com", issued.Secret,
		row.Seat)
	if err != nil || result.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the redemption answered %+v (%v)", result, err)
	}
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE login = ?`,
		iam.LoginFromAddress("lead@example.com")); len(got) != 1 ||
		got[0] != "platform-lead" {
		t.Errorf("the person holds seat %v, want platform-lead", got)
	}
}

// A SEAT A PERSON HOLDS REFUSES THE REDEMPTION, before anything is written.
//
// The invitation holds its seat from its issue, so a bind or a create onto it
// is refused naming the invitation — the first defence, asserted first. The
// redemption's own decide is the second, for what the first cannot see: a
// person on the seat a retained record, a restore or a build before
// invitations held their seats left behind. That residue is planted, since the
// writer no longer produces it, and the redemption is refused naming the
// seat's holder — whose remedy is a new invitation — publishing nothing: the
// invited address is not held against the next invitation to the same person.
//
// Mutation: drop the seat check from the enrolment's decide and the redemption
// binds a second person to the colleague's seat.
func TestASeatBoundSinceTheIssueRefusesTheRedemptionBeforeAnything(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	rig.seatOnly("platform-lead")
	issued, err := inviteFor(t, rig, "lead@example.com", "platform-lead")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	const colleague = "018f3a9c-0000-7000-8000-0000000000c2"
	if err := rig.enrol(iamdomain.Creation{
		PersonID: colleague, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Colleague", Email: "colleague@example.com", Login: "col.league",
		Seat: rig.vacantSeat("colleague-desk"), LinkExpiresAt: firstLinkExpiry,
		OpID: "op-colleague",
	}); err != nil {
		t.Fatalf("enrol the colleague: %v", err)
	}
	var held *iamdomain.ErrTaken
	if err := rig.bind("platform-lead", colleague,
		"op-colleague-seat"); !errors.As(err, &held) ||
		held.Invitation != issued.ID {
		t.Fatalf("moving the colleague onto the invited seat answered %v, want "+
			"it refused naming invitation %s", err, issued.ID)
	}
	plant(t, rig, `UPDATE iam_people SET seat_id = 'platform-lead' WHERE id = ?`,
		colleague)

	var taken *iamdomain.ErrTaken
	if _, err := redeemAs(t, rig, issued, "lead@example.com", issued.Secret,
		"platform-lead"); !errors.As(err, &taken) ||
		taken.Field != iamdomain.UniqueSeat || taken.Person != colleague {
		t.Fatalf("a redemption binding a seat a colleague took since the issue "+
			"was not refused naming the seat's holder (%v)", err)
	}
	if rows := rig.column(`SELECT id FROM iam_people WHERE id <> ?`,
		colleague); len(rows) != 0 {
		t.Errorf("the refused redemption left a row behind: %v", rows)
	}
}
