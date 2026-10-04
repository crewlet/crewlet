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
// a fresh person per attempt, as the surface mints one.
func redeemAs(t *testing.T, rig *writeRig, issued iamdomain.InviteIssued,
	address, secret, seat string) (statelog.Result, error) {

	t.Helper()
	person := uuid.Must(uuid.NewV7()).String()
	var result statelog.Result
	err := rig.draining(func() error {
		var err error
		result, err = nodeWriter(rig).Enrol(t.Context(), iamdomain.Enrolment{
			PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "A joiner", Email: address, Login: iam.LoginFromAddress(address),
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
// It is issued at [iamdomain.ConditionRecordVersion], which is the only thing
// that stops a node running an older build — one that knows neither the
// verifier nor the seat — from applying it during a rolling upgrade and then
// redeeming it by its id alone.
//
// Mutation: store the secret itself as the verifier and the document case
// fails; compare without the hash, or admit an empty verifier, and Admits
// answers the wrong way; drop the verifier's row from the field table
// (`versionedFields`) and the version case fails.
func TestAnInvitationKeepsItsLinksSecretOnlyAsAVerifier(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	issued, err := inviteFor(t, rig, "priya@example.com", "")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if issued.Secret == "" {
		t.Fatal("the issue handed back no secret: the link would carry the id alone")
	}
	// AT THE VERSION THAT STATES A CONDITION, so a node running an older
	// build DEFERS the record rather than applying an invitation it would
	// redeem on its id alone, with no secret and no seat.
	if env := rig.lastEnvelope(); env.Op != iamdomain.OpInvite ||
		env.V != iamdomain.ConditionRecordVersion {
		t.Errorf("the invitation was written as op %s at version %d, want %s "+
			"at %d — an older build would apply it and redeem it on its id",
			env.Op, env.V, iamdomain.OpInvite, iamdomain.ConditionRecordVersion)
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

	row, err := rig.reader(t).InvitationByID(t.Context(), issued.ID)
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
	issued, err := inviteFor(t, rig, "sam@example.com", "")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	for _, wrong := range []string{"", "not-the-secret", issued.ID} {
		if _, err := redeemAs(t, rig, issued, "sam@example.com", wrong,
			""); !errors.Is(err, iamdomain.ErrRefused) {
			t.Errorf("a redemption presenting %q was not refused (%v)", wrong, err)
		}
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("refused redemptions left rows behind: %v", rows)
	}
	result, err := redeemAs(t, rig, issued, "sam@example.com", issued.Secret, "")
	if err != nil || result.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the redemption presenting the link's secret answered %+v (%v)",
			result, err)
	}
}

// AN INVITATION BINDS ONLY A HUMAN SEAT NOBODY HOLDS, decided when it is issued.
//
// Each refusal is one the person the link is sent to could do nothing about: a
// seat nobody created, an agent's seat — a person bound to one is refused on
// every request — and a seat a colleague is already bound to. What it records
// is the seat's handle, which is its identity.
//
// Mutation: drop the kind check and the agent seat is issued; drop the holder
// check and the held seat is.
func TestAnInvitationBindsOnlyAHumanSeatNobodyHolds(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	rig.seatOnly("platform-lead")
	rig.seatOnly("held-seat")
	rig.agentSeat("release-bot")
	const colleague = "018f3a9c-0000-7000-8000-0000000000c1"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: colleague, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Colleague", Email: "colleague@example.com", Login: "col.league",
		OpID: "op-colleague",
	}); err != nil {
		t.Fatalf("enrol the colleague: %v", err)
	}
	if err := rig.bind("held-seat", colleague, "op-colleague-seat"); err != nil {
		t.Fatalf("bind the colleague: %v", err)
	}

	for _, refused := range []struct {
		seat string
		want error
	}{
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
	if rows := rig.column(`SELECT id FROM iam_invites`); len(rows) != 0 {
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
// did not offer and is refused, publishing nothing.
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
	row, err := rig.reader(t).InvitationByID(t.Context(), issued.ID)
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

// A SEAT BOUND SINCE THE ISSUE REFUSES THE REDEMPTION, before anything is
// written.
//
// The chart and the directory move between an issue and a redemption — a
// week, by default — and a seat an administrator bound to somebody else in
// that time is a link that no longer works. It is refused naming the seat as
// taken, whose remedy is a new invitation, and the refusal publishes nothing:
// the invited address is not held against the next invitation to the same
// person.
//
// Mutation: drop the seat check from Enrol's decide and the redemption binds
// a second person to the colleague's seat.
func TestASeatBoundSinceTheIssueRefusesTheRedemptionBeforeAnything(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	rig.seatOnly("platform-lead")
	issued, err := inviteFor(t, rig, "lead@example.com", "platform-lead")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	const colleague = "018f3a9c-0000-7000-8000-0000000000c2"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: colleague, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Colleague", Email: "colleague@example.com", Login: "col.league",
		OpID: "op-colleague",
	}); err != nil {
		t.Fatalf("enrol the colleague: %v", err)
	}
	if err := rig.bind("platform-lead", colleague, "op-colleague-seat"); err != nil {
		t.Fatalf("bind the colleague: %v", err)
	}

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
