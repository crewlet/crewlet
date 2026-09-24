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
func TestARetryThatNamesAnotherLoginClaimsIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person, err := iamdomain.InvitedPersonID(uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	attempt := func(login string) error {
		return rig.enrol(iamdomain.Enrolment{
			PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Dana Sre", Email: "dana@example.com", Login: login,
			OpID: "invite:the-link", Reason: "redeemed an invitation",
		})
	}
	if err := attempt("dana.sre"); err != nil {
		t.Fatalf("the first attempt: %v", err)
	}
	if err := attempt("dana.ops"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	rig.drain()
	if got := rig.column(`SELECT login FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != "dana.ops" {
		t.Errorf("after a retry naming dana.ops the person holds %v — the "+
			"second login's claim was collapsed into the first's", got)
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
