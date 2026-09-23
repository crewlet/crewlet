package iamdomain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PROVIDER LINK'S INVARIANTS: a subject is pinned by a CLAIM, the claim is
// the only writer of the row a sign-in resolves through, and every gesture that
// makes or ends one says so on the trail.

const linkIssuer = "https://idp.example.com"

// linkRig is a write rig with the three things every link case needs: a way to
// enrol somebody, a subject's blind, and the two gestures.
type linkRig struct {
	*writeRig
	reader  *iamdomain.Reader
	blinder *iamdomain.Blinder
}

func newLinkRig(t *testing.T) *linkRig {
	t.Helper()
	return newLinkRigWith(t, nil)
}

// newLinkRigWith is [newLinkRig] with the publisher's appender wrapped, for a
// case that needs an enrolment to STOP after its claims: a broker that answers
// nothing for the person record leaves the claims landed and the person
// unwritten, which is the sequence a crash between the two leaves.
func newLinkRigWith(t *testing.T,
	wrap func(statelog.Appender) statelog.Appender) *linkRig {

	t.Helper()
	rig := newWriteRigWith(t, wrap)
	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	return &linkRig{writeRig: rig, reader: rig.reader(t), blinder: blinder}
}

func (r *linkRig) subject(sub string) iamdomain.Link {
	r.t.Helper()
	blind, err := r.blinder.Subject(linkIssuer, sub)
	if err != nil {
		r.t.Fatalf("blind a subject: %v", err)
	}
	return iamdomain.Link{Issuer: linkIssuer, Blind: blind}
}

func (r *linkRig) person(login string) string {
	r.t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	if err := r.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: login, Email: login + "@example.com", Login: login,
		OpID: "enrol-" + login, Reason: "a joiner",
	}); err != nil {
		r.t.Fatalf("enrol %s: %v", login, err)
	}
	return id
}

func (r *linkRig) link(person string, link iamdomain.Link, replacing, op string) error {
	r.t.Helper()
	return r.during(func() error {
		_, err := r.writer.Link(r.t.Context(), iamdomain.LinkChange{
			PersonID: person, Link: link, Replacing: replacing,
			OpID: op, Reason: "pinned by an administrator",
		})
		return err
	})
}

func (r *linkRig) unlink(person string, link iamdomain.Link, op string) error {
	r.t.Helper()
	return r.during(func() error {
		_, err := r.writer.Unlink(r.t.Context(), person, link, op, "left the provider")
		return err
	})
}

// resolves is who a provider sign-in with this subject would become.
func (r *linkRig) resolves(link iamdomain.Link) string {
	r.t.Helper()
	held, err := r.reader.PersonBySubjectBlind(r.t.Context(), link.Blind,
		brokerAt.Add(time.Hour))
	if err != nil {
		r.t.Fatalf("resolve a subject: %v", err)
	}
	return held.ID
}

// linkEvents narrows what was announced to the link trail.
func linkEvents(seen []events.Payload) (linked []types.IAMIdentityLinked,
	unlinked []types.IAMIdentityUnlinked) {

	for _, payload := range seen {
		switch row := payload.(type) {
		case types.IAMIdentityLinked:
			linked = append(linked, row)
		case types.IAMIdentityUnlinked:
			unlinked = append(unlinked, row)
		}
	}
	return linked, unlinked
}

// TWO PEOPLE CANNOT BE PINNED TO ONE SUBJECT, and the refusal names who holds
// it.
//
// A subject is the whole of what a provider sign-in proves, so a second
// person pinned to it is somebody who signs in as the first. The claim
// arbitrates on the subject, the decide names the holder, and the first
// person goes on resolving — the second pin changed nothing and announced
// nothing.
func TestTwoPeopleCannotBePinnedToOneSubject(t *testing.T) {
	t.Parallel()
	rig := newLinkRig(t)
	ada, eve := rig.person("ada.first"), rig.person("eve.second")
	subject := rig.subject("shared-sub")

	if err := rig.link(ada, subject, "", "link-ada"); err != nil {
		t.Fatalf("link ada: %v", err)
	}
	rig.events.take()
	err := rig.link(eve, subject, "", "link-eve")
	var claimed *iamdomain.ErrClaimed
	if !errors.As(err, &claimed) || claimed.Kind != iamdomain.KindLink ||
		claimed.Holder != ada {
		t.Fatalf("pinning a held subject answered %v, want ErrClaimed of kind "+
			"link naming %s", err, ada)
	}
	if got := rig.resolves(subject); got != ada {
		t.Errorf("the subject resolves to %q after a refused second pin, "+
			"want %q", got, ada)
	}
	if linked, _ := linkEvents(rig.events.take()); len(linked) != 0 {
		t.Errorf("a refused link announced %+v", linked)
	}
}

// A LINK AND AN UNLINK EACH SAY SO, naming the person, the provider and who
// did it — and never the subject, which identifies a person at a third party
// and would outlive every crypto-shred on the trail.
func TestALinkAndAnUnlinkAreAnnounced(t *testing.T) {
	t.Parallel()
	rig := newLinkRig(t)
	ada := rig.person("ada.linked")
	subject := rig.subject("ada-sub")
	rig.events.take()

	if err := rig.link(ada, subject, "", "link-ada"); err != nil {
		t.Fatalf("link: %v", err)
	}
	linked, _ := linkEvents(rig.events.take())
	want := types.IAMIdentityLinked{Person: ada, Issuer: linkIssuer,
		Via: types.LinkViaAdmin, By: "ana.admin"}
	if len(linked) != 1 || linked[0] != want {
		t.Fatalf("linked = %+v, want exactly %+v", linked, want)
	}

	if err := rig.unlink(ada, subject, "unlink-ada"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	_, unlinked := linkEvents(rig.events.take())
	wantGone := types.IAMIdentityUnlinked{Person: ada, Issuer: linkIssuer,
		By: "ana.admin", Reason: "left the provider"}
	if len(unlinked) != 1 || unlinked[0] != wantGone {
		t.Fatalf("unlinked = %+v, want exactly %+v", unlinked, wantGone)
	}
	if got := rig.resolves(subject); got != "" {
		t.Errorf("an unlinked subject still resolves to %q", got)
	}

	// AN UNLINK NAMING THE WRONG HOLDER is refused with who does hold it,
	// because a release filed under the wrong person's bucket would take
	// the subject from somebody nobody named.
	eve := rig.person("eve.other")
	if err := rig.link(eve, subject, "", "link-eve"); err != nil {
		t.Fatalf("re-pin the released subject to somebody else: %v — an "+
			"unlinked subject must be free to pin again", err)
	}
	err := rig.unlink(ada, subject, "unlink-wrong")
	var claimed *iamdomain.ErrClaimed
	if !errors.As(err, &claimed) || claimed.Holder != eve {
		t.Fatalf("unlinking from the wrong holder answered %v, want "+
			"ErrClaimed naming %s", err, eve)
	}
	if got := rig.resolves(subject); got != eve {
		t.Errorf("a refused unlink moved the subject: resolves to %q", got)
	}
}

// A PERSON HOLDS ONE LINK, AND A MOVE LEAVES THE OLD SUBJECT FREE.
//
// The directory row reports the link the person holds now (what an
// administrator names as the one they are replacing); the subject moved off
// resolves nobody and can be pinned to somebody else; and a pin that does not
// name the link it replaces is refused rather than switching them.
func TestAPersonHoldsOneLinkAndAMoveFreesTheOld(t *testing.T) {
	t.Parallel()
	rig := newLinkRig(t)
	ada := rig.person("ada.moving")
	old, next, third := rig.subject("old"), rig.subject("new"), rig.subject("third")

	if err := rig.link(ada, old, "", "link-old"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := rig.link(ada, next, old.Blind, "link-new"); err != nil {
		t.Fatalf("move: %v", err)
	}
	row, err := rig.reader.Person(t.Context(), ada)
	if err != nil {
		t.Fatalf("read the person: %v", err)
	}
	if row.Link != next {
		t.Errorf("the directory row reports link %+v, want %+v", row.Link, next)
	}
	if got := rig.resolves(old); got != "" {
		t.Errorf("the subject moved off still resolves to %q", got)
	}
	if got := rig.resolves(next); got != ada {
		t.Errorf("the subject moved to resolves to %q, want %q", got, ada)
	}
	eve := rig.person("eve.after")
	if err := rig.link(eve, old, "", "link-eve-old"); err != nil {
		t.Errorf("the subject a move released could not be pinned again: %v", err)
	}

	// A PIN THAT DID NOT NAME WHAT IT REPLACES is refused, and so is a
	// move naming a link the person no longer holds: a person already
	// linked is never relinked in passing, because the account they have
	// signed in with would stop working and a different one start.
	for name, replacing := range map[string]string{
		"a pin naming nothing": "", "a move naming a stale link": old.Blind,
	} {
		if err := rig.link(ada, third, replacing, "link-third-"+name); !errors.Is(err,
			iamdomain.ErrLinked) {
			t.Errorf("%s answered %v, want ErrLinked", name, err)
		}
	}
	if got := rig.resolves(next); got != ada {
		t.Errorf("a refused relink moved the person off their subject: it "+
			"resolves to %q", got)
	}
	if got := rig.resolves(third); got != "" {
		t.Errorf("a refused relink pinned the new subject to %q", got)
	}
}

// TWO PINS RACING ON ONE PERSON LEAVE THEM WITH ONE LINK.
//
// Each pin arbitrates on its own SUBJECT, so two administrators pinning two
// different subjects to one person never contend at the broker — and each
// decide reads a snapshot the other's record is not in yet, so [ErrLinked]
// passes both. What keeps the person at one link is the APPLY: the later
// record takes the earlier one off them, on every node, in log order. The
// race is made deterministic by publishing both before this node applies
// either.
func TestTwoPinsRacingOnOnePersonLeaveOneLink(t *testing.T) {
	t.Parallel()
	rig := newLinkRig(t)
	ada := rig.person("ada.raced")
	first, second := rig.subject("first"), rig.subject("second")
	for i, pin := range []struct {
		by   string
		link iamdomain.Link
	}{{"ana.admin", first}, {"bo.admin", second}} {
		writer := rig.writer.As(pin.by, iam.KindPerson, iam.AllGrants)
		if _, err := writer.Link(t.Context(), iamdomain.LinkChange{
			PersonID: ada, Link: pin.link, OpID: "race-" + pin.by,
			Reason: "raced",
		}); err != nil {
			t.Fatalf("pin %d: %v — neither decide can see the other's record", i, err)
		}
	}
	rig.drain()
	if got := rig.resolves(second); got != ada {
		t.Errorf("the later pin resolves to %q, want %q", got, ada)
	}
	if got := rig.resolves(first); got != "" {
		t.Errorf("the earlier pin still resolves to %q: one person, two ways "+
			"to become them", got)
	}
	live := rig.column(`SELECT subject_blind FROM iam_credentials
		WHERE person_id = ? AND method = 'oidc' AND revoked_at = 0`, ada)
	if !slices.Equal(live, []string{second.Blind}) {
		t.Errorf("live links = %v, want only the later pin", live)
	}
}

// A CONTENT WRITE NEVER TOUCHES A LINK, AND A DOCUMENT NEVER CARRIES ONE.
//
// The link is its claim's row. A person's credential set is rewritten whole
// by every content record — a password set, a token minted — and a rewrite
// that cleared the table would unlink everybody who ever changed their
// password; a document carrying an oidc entry would be a second writer of a
// fact the broker never arbitrated.
func TestAContentWriteNeverTouchesALink(t *testing.T) {
	t.Parallel()
	rig := newLinkRig(t)
	ada := rig.person("ada.content")
	subject := rig.subject("ada-content")
	if err := rig.link(ada, subject, "", "link-ada"); err != nil {
		t.Fatalf("link: %v", err)
	}

	if err := rig.during(func() error {
		_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
			PersonID: ada, OpID: "set-password", Reason: "a password",
			Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
				return append(held, iamdomain.Credential{
					ID: uuid.NewString(), Method: iamdomain.MethodPassword,
					Verifier: "$argon2id$v=19$m=65536,t=3,p=1$c2FsdA$aGFzaA",
				})
			},
		})
		return err
	}); err != nil {
		t.Fatalf("set a password: %v", err)
	}
	if got := rig.resolves(subject); got != ada {
		t.Fatalf("a password set unlinked the person: the subject resolves "+
			"to %q", got)
	}

	for name, gesture := range map[string]func() error{
		"an enrolment carrying one": func() error {
			_, err := rig.writer.Enrol(t.Context(), iamdomain.Enrolment{
				PersonID: uuid.NewString(), Kind: iam.KindPerson,
				Stage: iam.StageActive, Name: "Rex", Email: "rex@example.com",
				Login: "rex.smuggled", OpID: "enrol-rex", Reason: "smuggled",
				Credentials: []iamdomain.Credential{{
					ID: uuid.NewString(), Method: iamdomain.MethodOIDC,
					SubjectBlind: rig.subject("rex").Blind,
				}},
			})
			return err
		},
		"a credential set adding one": func() error {
			_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
				PersonID: ada, OpID: "set-oidc", Reason: "smuggled",
				Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
					return append(held, iamdomain.Credential{
						ID: uuid.NewString(), Method: iamdomain.MethodOIDC,
						SubjectBlind: rig.subject("second").Blind,
					})
				},
			})
			return err
		},
	} {
		err := rig.during(gesture)
		if !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("%s answered %v, want ErrInvalid — a link is pinned by "+
				"its claim and nothing else", name, err)
		}
	}
	if got := rig.resolves(rig.subject("second")); got != "" {
		t.Errorf("a smuggled link resolves to %q", got)
	}
}

// ONLY A PERSON IS LINKED: a machine has no provider sign-in, and a subject
// pinned to one would be a way to act as a service account from a browser.
func TestOnlyAPersonIsLinked(t *testing.T) {
	t.Parallel()
	rig := newLinkRig(t)
	machine := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "ci:deploy", OpID: "enrol-ci", Reason: "a pipeline",
	}); err != nil {
		t.Fatalf("enrol a machine: %v", err)
	}
	if err := rig.link(machine, rig.subject("ci"), "", "link-ci"); !errors.Is(err,
		iamdomain.ErrInvalid) {
		t.Errorf("linking a machine answered %v, want ErrInvalid", err)
	}
	link := rig.subject("ci-enrol")
	err := rig.enrol(iamdomain.Enrolment{
		PersonID: uuid.NewString(), Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "ci:other", OpID: "enrol-ci-other", Reason: "a pipeline",
		Link: &link,
	})
	if !errors.Is(err, iamdomain.ErrInvalid) {
		t.Errorf("enrolling a machine through a provider answered %v, want "+
			"ErrInvalid", err)
	}
	if got := rig.resolves(rig.subject("ci")); got != "" {
		t.Errorf("a machine's refused link resolves to %q", got)
	}
}

// AN ENROLMENT THROUGH THE PROVIDER PINS ITS SUBJECT TO THE PERSON IT CREATES,
// says so once the person exists, and a stopped one is a named reservation the
// same redemption finishes.
//
// The redemption is the node's own writer standing on an invitation; the
// first attempt's person record goes unanswered by the broker, so the claims
// land and the person is never written — which is exactly the sequence that
// stopped. (Asking for more than the invitation offered no longer stops it
// there: the basis is checked before the first claim.) The claim report names the reservation holding the link (the
// subject signs in as nobody until it is repaired), and the retry of the same
// redemption at the same derived id completes it.
func TestAnEnrolmentThroughTheProviderPinsItsSubject(t *testing.T) {
	t.Parallel()
	var broker *silentBroker
	rig := newLinkRigWith(t, func(inner statelog.Appender) statelog.Appender {
		broker = &silentBroker{Appender: inner, on: ".person."}
		return broker
	})
	invitation := uuid.Must(uuid.NewV7()).String()
	offered := []iam.Grant{iam.GrantStateRead}
	if err := rig.draining(func() error {
		_, err := rig.writer.Invite(t.Context(), iamdomain.InviteMint{
			ID: invitation, Email: "joiner@example.com", Grants: offered,
			Colleague: iam.ColleagueRead,
			ExpiresAt: brokerAt.Add(168 * time.Hour),
			OpID:      "invite-joiner", Reason: "onboarding",
		})
		return err
	}); err != nil {
		t.Fatalf("invite: %v", err)
	}
	rig.drain()
	rig.events.take()

	person, err := iamdomain.InvitedPersonID(invitation)
	if err != nil {
		t.Fatalf("derive the invited person: %v", err)
	}
	link := rig.subject("joiner")
	redeem := func(grants []iam.Grant, through iamdomain.Link) error {
		return rig.draining(func() error {
			_, err := nodeWriter(rig.writeRig).Enrol(t.Context(), iamdomain.Enrolment{
				PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
				Name: "A joiner", Email: "joiner@example.com",
				Login: "joiner.one", Grants: grants,
				Colleague: iam.ColleagueRead, Invitation: invitation,
				Link: &through, OpID: "redeem-" + person,
				Reason: "redeemed through the identity provider",
			})
			return err
		})
	}

	broker.silent.Store(true)
	if err := redeem(offered, link); err != nil {
		t.Fatalf("a redemption whose person record went unanswered refused: %v",
			err)
	}
	broker.silent.Store(false)
	// A RESERVATION, and read as one: the subject is held and nobody holds
	// it yet, so a sign-in through it has no kind, no stage and nothing it
	// may do ([iamdomain.Sighting.Reserved]).
	held, err := rig.reader.PersonBySubjectBlind(t.Context(), link.Blind,
		brokerAt.Add(time.Hour))
	if err != nil || held.ID != person || !held.Reserved || held.Stage != "" {
		t.Fatalf("a stopped enrolment's subject resolved to %+v (%v), want the "+
			"reservation reported as one — a reservation must never be "+
			"signed in as", held, err)
	}
	if linked, _ := linkEvents(rig.events.take()); len(linked) != 0 {
		t.Errorf("a stopped enrolment announced a link: %+v", linked)
	}
	stopped, err := rig.reader.Claims(t.Context(),
		time.Now().Add(iamdomain.OrphanGrace+time.Minute))
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if len(stopped.Orphans) != 1 || stopped.Orphans[0].Person != person ||
		!slices.Contains(stopped.Orphans[0].Holds, iamdomain.KindLink) {
		t.Fatalf("orphans = %+v, want the reservation naming the link it "+
			"holds", stopped.Orphans)
	}

	// A RETRY THROUGH A DIFFERENT PROVIDER ACCOUNT is refused rather than
	// switched: the stopped attempt pinned its subject to this person,
	// and a second account arriving for them is the relink ErrLinked
	// exists to refuse.
	other := rig.subject("somebody-else")
	if err := redeem(offered, other); !errors.Is(err, iamdomain.ErrLinked) {
		t.Fatalf("a retry through another provider account answered %v, "+
			"want ErrLinked", err)
	}
	if err := redeem(offered, link); err != nil {
		t.Fatalf("the retried redemption: %v", err)
	}
	if got := rig.resolves(link); got != person {
		t.Errorf("the subject resolves to %q, want the person the "+
			"redemption created (%s)", got, person)
	}
	linked, _ := linkEvents(rig.events.take())
	want := types.IAMIdentityLinked{Person: person, Issuer: linkIssuer,
		Via: types.LinkViaInvite, By: "node-a"}
	if len(linked) != 1 || linked[0] != want {
		t.Errorf("linked = %+v, want exactly %+v", linked, want)
	}
	finished, err := rig.reader.Claims(t.Context(),
		time.Now().Add(iamdomain.OrphanGrace+time.Minute))
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if len(finished.Orphans) != 0 {
		t.Errorf("a finished redemption is still reported: %+v", finished.Orphans)
	}
}

// A REMOVAL ENDS THE LINK, and the subject is free for whoever the company
// pins it to next.
func TestARemovalEndsTheLink(t *testing.T) {
	t.Parallel()
	rig := newLinkRig(t)
	ada := rig.person("ada.leaving")
	subject := rig.subject("ada-leaving")
	if err := rig.link(ada, subject, "", "link-ada"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), ada, "remove-ada", "left")
		return err
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := rig.resolves(subject); got != "" {
		t.Fatalf("a removed person's subject resolves to %q", got)
	}
	eve := rig.person("eve.next")
	if err := rig.link(eve, subject, "", "link-eve"); err != nil {
		t.Errorf("a removed person's subject could not be pinned again: %v", err)
	}
}

// A DUPLICATE LINK A RESTORE LEFT IS REPORTED with both holders, like every
// other duplicate claim — it is the one a provider sign-in refuses for both.
func TestADuplicateLinkIsReported(t *testing.T) {
	t.Parallel()
	rig := newLinkRig(t)
	ada, eve := rig.person("ada.dup"), rig.person("eve.dup")
	subject := rig.subject("dup")
	if err := rig.link(ada, subject, "", "link-ada"); err != nil {
		t.Fatalf("link: %v", err)
	}
	clean, err := rig.reader.Claims(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if len(clean.Duplicates) != 0 {
		t.Fatalf("one live link reported as a duplicate: %+v", clean.Duplicates)
	}
	if _, err := rig.db.Replicated().SQL().ExecContext(t.Context(), `
		INSERT INTO iam_credentials
			(id, person_id, method, verifier, subject_blind, expires_at,
			 revoked_at, bucket, created_at, version, document)
		VALUES (?, ?, 'oidc', x'', ?, 0, 0, 0, 0, 1, x'')`,
		uuid.NewString(), eve, subject.Blind); err != nil {
		t.Fatalf("restore a duplicate: %v", err)
	}
	report, err := rig.reader.Claims(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	holders := []string{ada, eve}
	slices.Sort(holders)
	if len(report.Duplicates) != 1 || report.Duplicates[0].Kind != iamdomain.KindLink ||
		!slices.Equal(report.Duplicates[0].People, holders) {
		t.Fatalf("duplicates = %+v, want the link held by %v", report.Duplicates,
			holders)
	}
}

// A REDEMPTION FINISHED BY PASSWORD ANNOUNCES THE LINK ITS FIRST ATTEMPT
// PINNED.
//
// An attempt through the provider that stopped after its link claim left the
// subject pinned to the reservation, and the same redemption finished with a
// password creates the same person — holding that link. What is announced is
// read from the snapshot the person lands in rather than from the call that
// finished it, or the person could sign in through a provider account with no
// row on the trail saying they were linked to it.
func TestARedemptionFinishedByPasswordAnnouncesTheLinkItHolds(t *testing.T) {
	t.Parallel()
	var broker *silentBroker
	rig := newLinkRigWith(t, func(inner statelog.Appender) statelog.Appender {
		broker = &silentBroker{Appender: inner, on: ".person."}
		return broker
	})
	invitation := uuid.Must(uuid.NewV7()).String()
	offered := []iam.Grant{iam.GrantStateRead}
	if err := rig.draining(func() error {
		_, err := rig.writer.Invite(t.Context(), iamdomain.InviteMint{
			ID: invitation, Email: "later@example.com", Grants: offered,
			Colleague: iam.ColleagueRead,
			ExpiresAt: brokerAt.Add(168 * time.Hour),
			OpID:      "invite-later", Reason: "onboarding",
		})
		return err
	}); err != nil {
		t.Fatalf("invite: %v", err)
	}
	rig.drain()
	person, err := iamdomain.InvitedPersonID(invitation)
	if err != nil {
		t.Fatalf("derive the invited person: %v", err)
	}
	link := rig.subject("later")
	redeem := func(grants []iam.Grant, through *iamdomain.Link) error {
		return rig.draining(func() error {
			_, err := nodeWriter(rig.writeRig).Enrol(t.Context(), iamdomain.Enrolment{
				PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
				Name: "Later", Email: "later@example.com", Login: "later.one",
				Grants: grants, Colleague: iam.ColleagueRead,
				Invitation: invitation, Link: through, OpID: "redeem-" + person,
				Reason: "redeemed",
			})
			return err
		})
	}
	broker.silent.Store(true)
	if err := redeem(offered, &link); err != nil {
		t.Fatalf("a redemption whose person record went unanswered refused: %v",
			err)
	}
	broker.silent.Store(false)
	rig.events.take()

	if err := redeem(offered, nil); err != nil {
		t.Fatalf("the redemption finished by password: %v", err)
	}
	if got := rig.resolves(link); got != person {
		t.Fatalf("the subject resolves to %q, want the person the "+
			"redemption created", got)
	}
	linked, _ := linkEvents(rig.events.take())
	want := types.IAMIdentityLinked{Person: person, Issuer: linkIssuer,
		Via: types.LinkViaInvite, By: "node-a"}
	if len(linked) != 1 || linked[0] != want {
		t.Errorf("linked = %+v, want exactly %+v — the person holds a link "+
			"nothing announced", linked, want)
	}
}
