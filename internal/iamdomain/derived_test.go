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

// A REDEMPTION REFUSED FOR ITS LOGIN PUBLISHES NOTHING, SO A RETRY WITH ANOTHER
// LOGIN LANDS AND SPENDS THE LINK.
//
// A redemption is one record: the person, their address, their login and the
// spent link land together or not at all. So a redeemer told their login was
// taken has left nothing behind — no address held for a person nobody
// finished, no link half spent — and the attempt that names another login is
// a fresh person that lands whole.
//
// Mutation: publish the person before deciding the login, and the refused
// attempt's address refuses the retry.
func TestARedemptionRefusedForItsLoginPublishesNothing(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000007a1",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Elsewhere", Email: "dana@elsewhere.example.com",
		Login: "dana.sre", OpID: "op-first", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol the login's holder: %v", err)
	}
	rig.drain()
	issued, err := inviteFor(t, rig, "dana@example.com", "")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	end, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	redeem := func(login string) error {
		return rig.draining(func() error {
			_, err := nodeWriter(rig).Enrol(t.Context(), iamdomain.Enrolment{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindPerson,
				Stage: iam.StageActive, Name: "Dana Sre", Email: "dana@example.com",
				Login: login, Grants: []iam.Grant{iam.GrantStateRead},
				Invitation: issued.ID, InvitationSecret: issued.Secret,
				OpID: "invite:" + issued.ID, Reason: "redeemed an invitation",
			})
			return err
		})
	}
	var taken *iamdomain.ErrTaken
	if err := redeem("dana.sre"); !errors.As(err, &taken) ||
		taken.Field != iamdomain.UniqueLogin {
		t.Fatalf("the first attempt answered %v, want the login refused as held", err)
	}
	if after, err := rig.log.End(t.Context()); err != nil || after != end {
		t.Errorf("a refused redemption moved the log from %d to %d (%v)", end,
			after, err)
	}
	if err := redeem("dana.ops"); err != nil {
		t.Fatalf("the retry with another login was refused: %v", err)
	}
	rig.drain()
	held := rig.column(`SELECT id FROM iam_people WHERE login = 'dana.ops'`)
	if len(held) != 1 {
		t.Fatalf("dana.ops is held by %v, want one person", held)
	}
	if got := rig.column(`SELECT person_id FROM iam_invites WHERE id = ?
		AND redeemed_at <> 0`, issued.ID); len(got) != 1 || got[0] != held[0] {
		t.Errorf("the link is spent by %v, want the person the retry created %s",
			got, held[0])
	}
}

// AN ENROLMENT CREATES; IT NEVER REWRITES SOMEBODY WHO EXISTS.
//
// An administrator's create derives its person from its operation key, so an
// enrolment reaching a person who is already enrolled is a second request under
// one key: a surface binds the operation it publishes to the request, so the
// same key with another body arrives under another operation id naming the
// same derived person — and landing it would rewrite them. What still lands is
// the retry of the very create that made them, which the operation ledger
// answers.
//
// Mutation: drop createsNobodyTwice from Enrol's decide, and the second
// request lands over the existing person.
func TestAnEnrolmentNeverRewritesSomebodyWhoExists(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	key := operationKey()
	person, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	create := iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Omar Ops", Email: "omar@example.com", Login: "omar.ops",
		OpID: key, Reason: "created through /iam/people",
	}
	if err := rig.enrol(create); err != nil {
		t.Fatalf("the create: %v", err)
	}
	rig.drain()

	// THE RETRY OF THAT VERY CREATE — the one an unknown answer asks for —
	// is answered by the ledger.
	if err := rig.enrol(create); err != nil {
		t.Errorf("the create's own retry was refused: %v", err)
	}
	// THE SAME KEY FOR ANOTHER ADDRESS is another request — published under
	// another operation, as a surface binds it — refused before it gives the
	// existing person a second address.
	reused := create
	reused.Email, reused.Login = "somebody.else@example.com", "somebody.else"
	reused.OpID = statelog.StepOpID(key, "people-create", "another-request")
	if err := rig.enrol(reused); !errors.Is(err, iamdomain.ErrOperationReused) {
		t.Errorf("a key reused for another address answered %v, want %v",
			err, iamdomain.ErrOperationReused)
	}
	rig.drain()
	if got := rig.column(`SELECT email_blind FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != blindOf(t, "omar@example.com") {
		t.Errorf("the existing person's address is %v after a reused key", got)
	}
	// AND A CREATE UNDER ANOTHER OPERATION is refused even where every
	// value it names is already theirs.
	other := create
	other.OpID = operationKey()
	if err := rig.enrol(other); !errors.Is(err, iamdomain.ErrOperationReused) {
		t.Errorf("an enrolment of an existing person under another operation "+
			"answered %v, want %v", err, iamdomain.ErrOperationReused)
	}
}

// A CREATE'S PERSON AND AN ISSUE'S INVITATION ARE THE OPERATION'S.
//
// Each used to be minted per request, so the retry an unknown answer asks for —
// or the ledger's answer to it — named a second object nobody created. Derived
// from the operation key, every attempt of one operation names one id — and an
// invitation's is derived under the company's key, because the id is what its
// link's secret is derived from and a guessable key must not make it a
// guessable link.
func TestACreatesIdentityIsItsOperationKeys(t *testing.T) {
	t.Parallel()
	key := operationKey()
	person, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatalf("derive a person: %v", err)
	}
	if again, _ := iamdomain.CreatedPersonID(key); again != person {
		t.Errorf("one key derived two people: %s and %s", person, again)
	}
	if other, _ := iamdomain.CreatedPersonID(operationKey()); other == person {
		t.Error("two keys derived one person")
	}
	parsed := uuid.MustParse(person)
	if parsed.Version() != 7 || !sameMillisecond(parsed, uuid.MustParse(key)) {
		t.Errorf("the created person %s is not a uuid7 at its key's instant %s",
			person, key)
	}

	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("NewBlinder: %v", err)
	}
	invitation, err := blinder.InvitationID(key)
	if err != nil {
		t.Fatalf("derive an invitation: %v", err)
	}
	if again, _ := blinder.InvitationID(key); again != invitation {
		t.Errorf("one key derived two invitations: %s and %s", invitation, again)
	}
	if !sameMillisecond(uuid.MustParse(invitation), uuid.MustParse(key)) {
		t.Errorf("the invitation %s is not at its key's instant", invitation)
	}
	// A UUID7 IN ITS OWN RIGHT, because a redemption's operation id carries
	// the invitation's instant.
	if parsed := uuid.MustParse(invitation); parsed.Version() != 7 {
		t.Errorf("the derived invitation %s is not a uuid7", invitation)
	}
	// UNDER THE COMPANY'S KEY: another company's key derives another id from
	// the same operation key, so the link is not computable from the key.
	elsewhere, err := iamdomain.NewBlinder([]byte(strings.Repeat("z",
		iamdomain.MinBlindKeyBytes)))
	if err != nil {
		t.Fatalf("NewBlinder: %v", err)
	}
	if theirs, _ := elsewhere.InvitationID(key); theirs == invitation {
		t.Error("the invitation id does not depend on the company's key, so " +
			"anybody holding the operation key can compute the link")
	}
	if blind, _ := blinder.Email(key); blind == invitation {
		t.Error("an invitation id shares a MAC domain with a blind")
	}

	// A KEY THAT IS NOT A UUID7 derives nothing: its instant is the id's.
	for _, bad := range []string{"retry-7", uuid.NewString(), ""} {
		if _, err := iamdomain.CreatedPersonID(bad); !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("CreatedPersonID(%q) answered %v, want %v", bad, err,
				iamdomain.ErrInvalid)
		}
		if _, err := blinder.InvitationID(bad); !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("InvitationID(%q) answered %v, want %v", bad, err,
				iamdomain.ErrInvalid)
		}
	}
}

// AN ISSUE RETRIED UNDER ITS KEY ANSWERS THE INVITATION IT ISSUED.
//
// The retry an unknown answer asks for used to name a second invitation, which
// the address refused as held by the first — a 409 against its own first
// attempt, and a link to the one that landed shown to nobody. Mutation: mint the
// id per call and the retry is refused as taken; drop the terms comparison and
// a reused key answers another request's invitation.
func TestAnIssueRetriedUnderItsKeyAnswersTheInvitationItIssued(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	key := operationKey()
	issue := func(key, address string, grants []iam.Grant) (iamdomain.InviteIssued, error) {
		var issued iamdomain.InviteIssued
		err := rig.draining(func() error {
			var err error
			issued, err = rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
				Email: address, Grants: grants,
				ExpiresAt: brokerAt.Add(168 * time.Hour),
				OpID:      key, Reason: "onboarding",
			})
			return err
		})
		return issued, err
	}
	first, err := issue(key, "priya@example.com", []iam.Grant{iam.GrantStateRead})
	if err != nil {
		t.Fatalf("the issue: %v", err)
	}
	rig.drain()
	again, err := issue(key, "priya@example.com", []iam.Grant{iam.GrantStateRead})
	if err != nil || again.ID != first.ID {
		t.Fatalf("the retry answered %+v (%v), want the first attempt's "+
			"invitation %s", again, err, first.ID)
	}
	// AND THE SAME LINK: the secret is derived from the id, so the retry
	// shows the link whose verifier the first attempt stored — a secret
	// minted per call would hand back a link that opens nothing.
	if first.Secret == "" || again.Secret != first.Secret {
		t.Errorf("the retry answered secret %q, want the first attempt's %q",
			again.Secret, first.Secret)
	}
	if !again.ExpiresAt.Equal(first.ExpiresAt) {
		t.Errorf("the retry answered expiry %s, want the one it was issued "+
			"with %s", again.ExpiresAt, first.ExpiresAt)
	}
	if got := rig.column(`SELECT id FROM iam_invites`); len(got) != 1 {
		t.Errorf("iam_invites holds %v after a retry, want the one invitation", got)
	}
	// THE KEY FOR ANOTHER ADDRESS, OR ON OTHER TERMS, is another request —
	// a seat the first attempt did not bind included.
	rig.seatOnly("platform-lead")
	for name, attempt := range map[string]func() error{
		"a seat the first did not bind": func() error {
			return rig.draining(func() error {
				_, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
					Email: "priya@example.com", Grants: []iam.Grant{iam.GrantStateRead},
					Seat: "platform-lead", ExpiresAt: brokerAt.Add(168 * time.Hour),
					OpID: key, Reason: "onboarding",
				})
				return err
			})
		},
		"another address": func() error {
			_, err := issue(key, "someone.else@example.com",
				[]iam.Grant{iam.GrantStateRead})
			return err
		},
		"other grants": func() error {
			_, err := issue(key, "priya@example.com",
				[]iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite})
			return err
		},
	} {
		if err := attempt(); !errors.Is(err, iamdomain.ErrOperationReused) {
			t.Errorf("%s under the same key answered %v, want %v", name, err,
				iamdomain.ErrOperationReused)
		}
	}
}

// sameMillisecond reports whether two uuid7s carry one instant.
func sameMillisecond(a, b uuid.UUID) bool { return millisecondOf(a) == millisecondOf(b) }

// millisecondOf is the instant in a uuid7's first 48 bits.
func millisecondOf(id uuid.UUID) int64 {
	var ms int64
	for _, b := range id[:6] {
		ms = ms<<8 | int64(b)
	}
	return ms
}
