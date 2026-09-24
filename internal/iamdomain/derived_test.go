package iamdomain_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REDEMPTION'S PERSON IS ONE UUID7 PER CREDENTIAL.
//
// Every attempt at one redemption must name the person its first attempt
// claimed the address for, so the derivation is a function of the credential
// and nothing else — and it is a uuid7 at the credential's own instant,
// because the directory pages in id order and that order is creation order.
func TestARedemptionsPersonIsOneUUID7PerCredential(t *testing.T) {
	t.Parallel()
	invitation := uuid.Must(uuid.NewV7())
	first, err := iamdomain.InvitedPersonID(invitation.String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	again, _ := iamdomain.InvitedPersonID(invitation.String())
	if first != again {
		t.Errorf("one invitation derived two people: %s and %s", first, again)
	}
	person := uuid.MustParse(first)
	if person.Version() != 7 || person.Variant() != uuid.RFC4122 {
		t.Errorf("the derived person %s is version %d, variant %v — want a "+
			"uuid7 like every minted person id", person, person.Version(),
			person.Variant())
	}
	if !sameMillisecond(person, invitation) {
		t.Errorf("the derived person %s is not at the invitation's instant %s",
			person, invitation)
	}
	other, _ := iamdomain.InvitedPersonID(uuid.Must(uuid.NewV7()).String())
	if other == first {
		t.Error("two invitations derived one person")
	}

	// AN ID THIS BUILD NEVER MINTS FOR AN INVITATION IS REFUSED rather than
	// derived at some instant nobody chose.
	for _, bad := range []string{"inv-1", uuid.NewString()} {
		if _, err := iamdomain.InvitedPersonID(bad); err == nil {
			t.Errorf("invitation %q derived a person", bad)
		}
	}

	// A BOOTSTRAP IS ONE PERSON PER CODE, at the code's own instant.
	minted := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	founder := iamdomain.BootstrappedPersonID("code-digest", minted)
	if founder != iamdomain.BootstrappedPersonID("code-digest", minted) {
		t.Error("one bootstrap code derived two founders")
	}
	if got := uuid.MustParse(founder); got.Version() != 7 {
		t.Errorf("the founder %s is not a uuid7", got)
	}
	if ms := millisecondOf(uuid.MustParse(founder)); ms != minted.UnixMilli() {
		t.Errorf("the founder's instant is %d, want the code's %d", ms,
			minted.UnixMilli())
	}
}

// A FOUNDING'S PERSON SAYS SO BY ITS SHAPE, AND NOTHING ELSE'S DOES.
//
// The next founding finds an earlier attempt's reservation by this shape once
// the code it took has been swept off the log, and releases it — so every id a
// founding derives must carry it, and no id anything else mints or derives may,
// or a founding would end an administrator's half-finished colleague.
//
// Mutation: drop the mark from the derivation and the founder fails the shape;
// answer the shape for any uuid7 and the minted and invited ids pass it.
func TestAFoundingsPersonSaysSoByItsShape(t *testing.T) {
	t.Parallel()
	minted := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	for i := range 64 {
		founder := iamdomain.BootstrappedPersonID(fmt.Sprintf("code-%d", i), minted)
		if !iamdomain.FounderAttempt(founder) {
			t.Fatalf("the founder %s a code derived does not carry the shape", founder)
		}
		if strings.ToUpper(founder) != founder &&
			iamdomain.FounderAttempt(strings.ToUpper(founder)) {
			t.Errorf("a non-canonical spelling of %s carries the shape", founder)
		}
	}
	for i := range 256 {
		minted := uuid.Must(uuid.NewV7()).String()
		if iamdomain.FounderAttempt(minted) {
			t.Errorf("a minted person id %s (try %d) carries the founder shape",
				minted, i)
		}
		invited, err := iamdomain.InvitedPersonID(uuid.Must(uuid.NewV7()).String())
		if err != nil {
			t.Fatalf("derive: %v", err)
		}
		if iamdomain.FounderAttempt(invited) {
			t.Errorf("an invited person %s carries the founder shape", invited)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", uuid.NewString()} {
		if iamdomain.FounderAttempt(bad) {
			t.Errorf("%q carries the founder shape", bad)
		}
	}
	// AND IT IS STILL ONE PERSON PER CODE: two codes minted in the same
	// millisecond derive two people.
	if iamdomain.BootstrappedPersonID("one", minted) ==
		iamdomain.BootstrappedPersonID("two", minted) {
		t.Error("two codes minted in one millisecond derived one founder")
	}
}

// A STOPPED ENROLMENT FINISHES UNDER ITS OWN PERSON, AND ONLY UNDER IT.
//
// The domain half of a derived person: an enrolment refused after its address
// claim — here, on a login somebody else holds — leaves the address held by
// the person it named, so a retry naming that SAME person takes the address
// it already holds, claims another login and lands. A retry naming a fresh id
// is refused by the reservation, which is what every redemption met when the
// person was minted per request.
func TestAStoppedEnrolmentFinishesUnderItsOwnPerson(t *testing.T) {
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
	person, err := iamdomain.InvitedPersonID(uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	redemption := func(login string) iamdomain.Enrolment {
		return iamdomain.Enrolment{
			PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Dana Sre", Email: "dana@example.com", Login: login,
			OpID: "invite:the-link", Reason: "redeemed an invitation",
		}
	}
	var claimed *iamdomain.ErrClaimed
	if err := rig.enrol(redemption("dana.sre")); !errors.As(err, &claimed) ||
		claimed.Kind != iamdomain.KindLogin {
		t.Fatalf("the first attempt answered %v, want the login refused as held", err)
	}
	// THE CONTROL FIRST: a fresh id for the same address is refused by the
	// stopped attempt's reservation — the failure a minted id produced.
	fresh := redemption("dana.ops")
	fresh.PersonID, fresh.OpID = uuid.Must(uuid.NewV7()).String(), "invite:fresh"
	if err := rig.enrol(fresh); !errors.As(err, &claimed) ||
		claimed.Kind != iamdomain.KindEmail || claimed.Holder != person {
		t.Errorf("a fresh id answered %v, want the address refused as held by "+
			"the stopped attempt's person %s", err, person)
	}
	if err := rig.enrol(redemption("dana.ops")); err != nil {
		t.Fatalf("the retry under the same person was refused: %v", err)
	}
	rig.drain()
	if got := rig.column(`SELECT login FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != "dana.ops" {
		t.Errorf("the redeemed person holds logins %v, want [dana.ops]", got)
	}
}

// A RETRY THAT NAMES ANOTHER LOGIN CLAIMS THAT LOGIN.
//
// A retry of one enrolment runs under the same operation id — that is what
// lets the ledger collapse the appends that already landed — but a redeemer
// may change their login between attempts. The login's claim carried the
// operation id alone, so inside the broker's duplicate window the second
// login's claim was acknowledged as the first's: it never happened, and the
// person was left holding the login they had given up. The login is part of
// its claim's op id now.
//
// THE FIRST ATTEMPT STOPS before its person record, which is what a retry is
// for: here its person record goes unanswered, so it may or may not exist and
// the gesture says unknown.
func TestARetryThatNamesAnotherLoginClaimsIt(t *testing.T) {
	t.Parallel()
	var broker *silentBroker
	rig := newWriteRigWith(t, func(inner statelog.Appender) statelog.Appender {
		broker = &silentBroker{Appender: inner, on: ".person."}
		return broker
	})
	person, err := iamdomain.InvitedPersonID(uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	attempt := func(login string) (statelog.Result, error) {
		var result statelog.Result
		err := rig.draining(func() error {
			var err error
			result, err = rig.writer.Enrol(rig.t.Context(), iamdomain.Enrolment{
				PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
				Name: "Dana Sre", Email: "dana@example.com", Login: login,
				OpID: "invite:the-link", Reason: "redeemed an invitation",
			})
			return err
		})
		return result, err
	}
	broker.silent.Store(true)
	if result, err := attempt("dana.sre"); err != nil ||
		result.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the first attempt answered %+v (%v), want unknown at its "+
			"unanswered person record", result, err)
	}
	broker.silent.Store(false)
	if _, err := attempt("dana.ops"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	rig.drain()
	if got := rig.column(`SELECT login FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != "dana.ops" {
		t.Errorf("after a retry naming dana.ops the person holds %v — the "+
			"second login's claim was collapsed into the first's", got)
	}
}

// AN ENROLMENT CREATES; IT NEVER REWRITES SOMEBODY WHO EXISTS.
//
// Every enrolment's person is derived — from an invitation, a code, or an
// administrator's operation key — so an enrolment reaching a person who is
// already enrolled is a second request under one derivation. The person record
// is arbitrated rather than a create, so nothing at the broker stopped it
// landing over them: a key reused for another address gave the existing person
// a second one, and a second redemption rewrote the person the first created.
// What still lands is the retry of the very enrolment that created them, which
// the operation ledger answers.
//
// Mutation: drop the guard on the claims and the reused key's address lands on
// the existing person; drop the one on the person record and a second
// redemption answers applied.
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
	// THE SAME KEY FOR ANOTHER ADDRESS is another request, refused before
	// its claim gives the existing person a second address.
	reused := create
	reused.Email, reused.Login = "somebody.else@example.com", "somebody.else"
	if err := rig.enrol(reused); !errors.Is(err, iamdomain.ErrOperationReused) {
		t.Errorf("a key reused for another address answered %v, want %v",
			err, iamdomain.ErrOperationReused)
	}
	rig.drain()
	if got := rig.column(`SELECT email_blind FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != blindOf(t, "omar@example.com") {
		t.Errorf("the existing person's address is %v after a reused key", got)
	}
	// AND A PERSON RECORD UNDER ANOTHER OPERATION is refused even where
	// every claim it names is already theirs.
	other := create
	other.OpID = operationKey()
	if err := rig.enrol(other); !errors.Is(err, iamdomain.ErrOperationReused) {
		t.Errorf("an enrolment of an existing person under another operation "+
			"answered %v, want %v", err, iamdomain.ErrOperationReused)
	}
}

// A CREATE'S PERSON AND AN ISSUE'S INVITATION ARE THE OPERATION'S.
//
// Each used to be minted per request, so the retry an unknown answer asks for
// named a second object that the first attempt's claim on the address then
// refused. Derived from the operation key, every attempt of one operation names
// one id — and an invitation's is derived under the company's key, because the
// id is the verifier its link carries and a guessable key must not make it a
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
	// THE INVITED PERSON IS DERIVED FROM IT in turn, which needs the uuid7.
	if _, err := iamdomain.InvitedPersonID(invitation); err != nil {
		t.Errorf("the derived invitation derives no person: %v", err)
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
// the address refused as spoken for by the first — a 409 against its own first
// attempt, and a link to the one that landed shown to nobody. Mutation: mint the
// id per call and the retry is refused as claimed; drop the terms comparison
// and a reused key answers another request's invitation.
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
	if !again.ExpiresAt.Equal(first.ExpiresAt) {
		t.Errorf("the retry answered expiry %s, want the one it was issued "+
			"with %s", again.ExpiresAt, first.ExpiresAt)
	}
	if got := rig.column(`SELECT id FROM iam_invites`); len(got) != 1 {
		t.Errorf("iam_invites holds %v after a retry, want the one invitation", got)
	}
	// THE KEY FOR ANOTHER ADDRESS, OR ON OTHER TERMS, is another request.
	for name, attempt := range map[string]func() error{
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
