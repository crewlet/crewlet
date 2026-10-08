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
	if err := rig.enrol(iamdomain.Creation{
		PersonID: "018f3a9c-0000-7000-8000-0000000007a1",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Elsewhere", Email: "dana@elsewhere.example.com",
		Login: "dana.sre", Seat: rig.vacantSeat("dana-elsewhere"),
		LinkExpiresAt: firstLinkExpiry, OpID: "op-first", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol the login's holder: %v", err)
	}
	rig.drain()
	seat := rig.vacantSeat("dana-lead")
	issued, err := inviteFor(t, rig, "dana@example.com", seat)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	end, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	redeem := func(login string) error {
		return rig.draining(func() error {
			_, err := nodeWriter(rig).Redeem(t.Context(), iamdomain.Redemption{
				PersonID: uuid.Must(uuid.NewV7()).String(),
				Stage:    iam.StageActive, Name: "Dana Sre", Email: "dana@example.com",
				Login: login, Password: aPassword(),
				Grants: []iam.Grant{iam.GrantStateRead}, Seat: seat,
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
// answers — WITH THE FIRST PASSWORD LINK ITS FIRST ATTEMPT ISSUED, since the
// link is derived from the person: the same id, the same secret and the expiry
// the record stored, not the one the retry asked for. A service account's
// create answers no link, first time or again.
//
// Mutation: drop createsNobodyTwice from the enrolment's decide, and the
// second request lands over the existing person; mint the link per call and
// the retry answers a link whose verifier the estate never stored.
func TestAnEnrolmentNeverRewritesSomebodyWhoExists(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	key := operationKey()
	person, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	create := iamdomain.Creation{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Omar Ops", Email: "omar@example.com", Login: "omar.ops",
		Seat: rig.vacantSeat("ops-lead"), LinkExpiresAt: firstLinkExpiry,
		OpID: key, Reason: "created by jane.doe",
	}
	first, err := rig.create(create)
	if err != nil {
		t.Fatalf("the create: %v", err)
	}
	if first.Link == nil || first.Link.Credential == "" || first.Link.Secret == "" ||
		!first.Link.ExpiresAt.Equal(firstLinkExpiry) || first.LinkClosed {
		t.Fatalf("the create answered link %+v (closed %v), want its first "+
			"password link", first.Link, first.LinkClosed)
	}
	rig.drain()

	// THE RETRY OF THAT VERY CREATE — the one an unknown answer asks for —
	// is answered by the ledger, with the same link and the expiry it was
	// stored with, although the retry names a later one.
	retry := create
	retry.LinkExpiresAt = firstLinkExpiry.Add(time.Hour)
	again, err := rig.create(retry)
	if err != nil {
		t.Errorf("the create's own retry was refused: %v", err)
	}
	if !again.Collapsed || again.Link == nil ||
		again.Link.Credential != first.Link.Credential ||
		again.Link.Secret != first.Link.Secret ||
		!again.Link.ExpiresAt.Equal(first.Link.ExpiresAt) {
		t.Errorf("the create's own retry answered link %+v (collapsed %v), "+
			"want the first attempt's %+v", again.Link, again.Collapsed, first.Link)
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

	// A SERVICE ACCOUNT'S CREATE ISSUES NO LINK, nor does its retry.
	machineKey := operationKey()
	machine, err := iamdomain.CreatedPersonID(machineKey)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	account := iamdomain.Creation{
		PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "svc:release", OpID: machineKey, Reason: "the pipeline",
	}
	for _, attempt := range []string{"the create", "its retry"} {
		created, err := rig.create(account)
		if err != nil || created.Link != nil || created.LinkClosed {
			t.Errorf("a service account's %s answered link %+v (closed %v, %v), "+
				"want none", attempt, created.Link, created.LinkClosed, err)
		}
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
		if _, err := blinder.PasswordLinkID(bad); !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("PasswordLinkID(%q) answered %v, want %v", bad, err,
				iamdomain.ErrInvalid)
		}
	}
}

// A CREATED PERSON'S FIRST PASSWORD LINK IS THEIRS, DERIVED UNDER THE COMPANY'S
// KEY.
//
// The person is derived from the create's operation key, and the link from the
// person, so the retry the ledger answers without running the decide hands
// back the link its first attempt issued. Each half is held to what that
// rests on: one person derives one link id — a uuid7 at the person's instant,
// as every credential id here is — and one id one secret; another person,
// another link; another company's key, another link, so neither half is
// computable by somebody holding the operation key; and each in a MAC domain
// of its own, so no link id or secret is a value the same key derives for an
// invitation or an address. The secret carries no `.`, the separator its
// `<id>.<secret>` link splits on.
//
// Mutation: derive the link id or its secret in the invitation's domain and
// the domain separation fails; derive them from the key alone and the
// company's-key case does.
func TestAFirstPasswordLinkIsDerivedFromItsPerson(t *testing.T) {
	t.Parallel()
	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("NewBlinder: %v", err)
	}
	key := operationKey()
	person, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatalf("derive a person: %v", err)
	}
	link, err := blinder.PasswordLinkID(person)
	if err != nil {
		t.Fatalf("PasswordLinkID: %v", err)
	}
	secret, err := blinder.PasswordLinkSecret(link)
	if err != nil {
		t.Fatalf("PasswordLinkSecret: %v", err)
	}
	if again, _ := blinder.PasswordLinkID(person); again != link {
		t.Errorf("one person derived two links: %s and %s", link, again)
	}
	if again, _ := blinder.PasswordLinkSecret(link); again != secret {
		t.Error("one link derived two secrets")
	}
	parsed := uuid.MustParse(link)
	if parsed.Version() != 7 || !sameMillisecond(parsed, uuid.MustParse(person)) {
		t.Errorf("the link %s is not a uuid7 at its person's instant %s", link,
			person)
	}
	if link == person {
		t.Error("the link id is the person's id")
	}
	other, _ := iamdomain.CreatedPersonID(operationKey())
	if theirs, _ := blinder.PasswordLinkID(other); theirs == link {
		t.Error("two people derived one link")
	}
	if strings.Contains(secret, ".") || len(secret) < 43 {
		t.Errorf("the secret %q carries the link's separator or is shorter "+
			"than 32 bytes of base64", secret)
	}

	// UNDER THE COMPANY'S KEY.
	elsewhere, err := iamdomain.NewBlinder([]byte(strings.Repeat("z",
		iamdomain.MinBlindKeyBytes)))
	if err != nil {
		t.Fatalf("NewBlinder: %v", err)
	}
	if theirs, _ := elsewhere.PasswordLinkID(person); theirs == link {
		t.Error("the link id does not depend on the company's key")
	}
	if theirs, _ := elsewhere.PasswordLinkSecret(link); theirs == secret {
		t.Error("the link's secret does not depend on the company's key, so " +
			"anybody holding its id — which every row holds — can open it")
	}

	// IN DOMAINS OF THEIR OWN: the same input under the invitation's and the
	// address's derivations gives none of these values.
	invitationID, _ := blinder.InvitationID(person)
	invitationSecret, _ := blinder.InvitationSecret(link)
	blind, _ := blinder.Email(person)
	for name, value := range map[string]string{
		"an invitation id": invitationID, "an invitation secret": invitationSecret,
		"a blind": blind,
	} {
		if value == link || value == secret {
			t.Errorf("a first password link shares a MAC domain with %s", name)
		}
	}

	if _, err := blinder.PasswordLinkSecret(""); err == nil {
		t.Error("a secret was derived for no link")
	}
	var none *iamdomain.Blinder
	if _, err := none.PasswordLinkID(person); !errors.Is(err, iamdomain.ErrNoBlindKey) {
		t.Errorf("a node with no company key derived a link id (%v)", err)
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
	seat := rig.vacantSeat("platform-lead")
	issue := func(key, address string, grants []iam.Grant) (iamdomain.InviteIssued, error) {
		var issued iamdomain.InviteIssued
		err := rig.draining(func() error {
			var err error
			issued, err = rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
				Email: address, Grants: grants, Seat: seat,
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
	rig.seatOnly("data-lead")
	for name, attempt := range map[string]func() error{
		"a seat the first did not bind": func() error {
			return rig.draining(func() error {
				_, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
					Email: "priya@example.com", Grants: []iam.Grant{iam.GrantStateRead},
					Seat: "data-lead", ExpiresAt: brokerAt.Add(168 * time.Hour),
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
