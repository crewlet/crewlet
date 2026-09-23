package iamdomain_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/google/uuid"
)

// THE READER IS WHAT VALIDATES A BEARER, asserted at compile time.
//
// [session.Directory] is consumer-defined in the package that validates — one
// method, six facts — and this is the only implementation of it that reads a
// real estate. A signature drift between the two would otherwise surface as a
// wiring failure at boot on somebody's deployment rather than as a build
// failure here, and what a node with no directory does is answer 503 to every
// request carrying a cookie.
var _ session.Directory = (*iamdomain.Reader)(nil)

// A READER REFUSES A MISSING HALF BY NAME.
//
// The read authority is the load-bearing one: without the framework's own
// reader every read level is a LABEL rather than a guarantee, so a node behind
// the log would report `session` while serving whatever it happened to hold —
// and an identity answer that silently degraded is one nobody can tell from a
// correct refusal.
func TestAReaderRefusesAMissingHalfByName(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	for name, opts := range map[string]iamdomain.ReaderOptions{
		"no estate at all":  {},
		"no read authority": {DB: rig.db},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reader, err := iamdomain.NewReader(opts)
			if err == nil {
				t.Fatal("a reader was built with a missing half")
			}
			if reader != nil {
				t.Error("a refused reader was returned anyway")
			}
		})
	}
}

// A LOGIN NOBODY HOLDS IS THE ZERO VALUE AND NO ERROR — never a sentinel.
//
// THIS IS THE ENUMERATION RULE, and it is a shape decision rather than a
// convenience one. A caller that could tell "no such login" from "wrong
// password" has a roster: the two arms have to be indistinguishable in what
// they return, how long they take and what they log. internal/iam/credential
// owns the timing half; this is the shape half, and the caller runs a
// fixed-cost decoy against the zero value rather than branching on it.
//
// An error stays the unknown arm, here as everywhere in this estate.
func TestAnAbsentLoginIsNobodyRatherThanAnError(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	seen, err := reader.PersonByLogin(t.Context(), "nobody.at.all")
	if err != nil {
		t.Fatalf("a login nobody holds was an error: %v — a caller cannot "+
			"tell that from the store being unreachable, and the two send "+
			"an operator to opposite places", err)
	}
	if seen.ID != "" {
		t.Errorf("a login nobody holds resolved to %q", seen.ID)
	}

	// THE CONTROL: a login somebody DOES hold resolves, or the assertion
	// above would pass on a reader that answered nobody for everything.
	id := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-1", Reason: "the joiner",
		Grants:    []iam.Grant{iam.GrantStateRead},
		Colleague: iam.ColleagueWrite,
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()

	held, err := reader.PersonByLogin(t.Context(), "sarah.chen")
	if err != nil {
		t.Fatalf("PersonByLogin: %v", err)
	}
	if held.ID != id {
		t.Fatalf("resolved to %q, want %q", held.ID, id)
	}
	if held.Stage != iam.StageActive {
		t.Errorf("stage = %q, want active", held.Stage)
	}
	// BOTH HATS COME BACK. A grant is authority over the deployment and a
	// colleague level is reach into the company's own work, and a reader
	// that dropped either would compose a principal that is half right —
	// which is indistinguishable from a person who was given less.
	if !held.Grants[0].Valid() || held.Grants[0] != iam.GrantStateRead {
		t.Errorf("grants = %v, want state:read", held.Grants)
	}
	if held.Colleague != iam.ColleagueWrite {
		t.Errorf("colleague = %q, want write: the second hat was dropped, so "+
			"somebody who may file work reaches none of it", held.Colleague)
	}
}

// AND THE ESTATE KNOWS WHETHER ANYBODY IS IN IT, which is the bootstrap
// decision: the one-time code that creates the first person stops working the
// moment it is not. A COUNT and never a listing, because it is asked by an
// unauthenticated route and the answer is one bit.
func TestTheEstateSaysWhetherAnybodyIsEnrolled(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	held, err := reader.AnyPerson(t.Context())
	if err != nil {
		t.Fatalf("AnyPerson: %v", err)
	}
	if held {
		t.Fatal("a fresh estate reports somebody in it, so the one-time " +
			"bootstrap code would never be offered")
	}

	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: uuid.New().String(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Name: "Sarah Chen",
		Email: "sarah.chen@example.com", Login: "sarah.chen",
		OpID: "op-1", Reason: "the first person",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()

	held, err = reader.AnyPerson(t.Context())
	if err != nil {
		t.Fatalf("AnyPerson: %v", err)
	}
	if !held {
		t.Error("an estate holding a person reports itself empty, so the " +
			"one-time bootstrap code would go on creating operators")
	}
}

// THE SESSION READ IS ONE SNAPSHOT, and it answers all six facts at a
// position this node states.
//
// Reading the session and the person separately produces a verdict that never
// existed at any instant: a revocation landing between the two is a bearer
// judged against a live session and a bumped epoch, or the reverse. It is also
// the only shape in which "the replicated estate is not open" is one answer
// rather than three.
func TestOneResolveAnswersTheSessionAndThePersonTogether(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	id := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-1", Reason: "the joiner",
		Grants: []iam.Grant{iam.GrantStateRead},
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()

	seen, err := reader.Resolve(t.Context(), "no-such-lineage", id)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// THE PERSON IS FOUND AND THE SESSION IS NOT, which is the pair the
	// single read exists to report honestly: an absent session row is a
	// FINDING that session.Row turns into `gone` or `behind` against the
	// bearer's own start position, and it is not this reader's to decide.
	if !seen.Person.Found {
		t.Error("the person is not found, so every bearer they hold is refused")
	}
	if seen.Session.Found {
		t.Error("a lineage nobody opened was found")
	}
	if seen.Person.Login != "sarah.chen" {
		t.Errorf("login = %q", seen.Person.Login)
	}
	// AND A PERSON NOBODY HAS REVOKED IS EPOCH ZERO, which is a real value
	// rather than a missing row: every bearer they hold carries zero too,
	// so reading the absence as anything else would end every session in
	// the company on its first request.
	if seen.Person.Epoch != 0 {
		t.Errorf("epoch = %d on a person nobody has revoked", seen.Person.Epoch)
	}
	if seen.Generation != 0 {
		t.Errorf("generation = %d on a fleet that has never invalidated",
			seen.Generation)
	}
}

// A PROVIDER'S SUBJECT RESOLVES THROUGH A LIVE LINK, AND NEVER THROUGH THE
// ADDRESS COLUMN.
//
// The callback used to look the subject's blind up as an ADDRESS blind. The two
// are different classes inside one MAC by construction, so that matched nobody
// and every provider sign-in was refused, while the blinder, the reader and the
// callback each passed their own suite. And the subject is the WHOLE of what a
// provider sign-in proves, so a withdrawn link must resolve nobody, a subject a
// person was moved OFF must resolve nobody, and a subject two people hold must
// resolve neither of them. Resolving either one would sign somebody in as
// somebody else with nothing further checked.
func TestAProviderSubjectResolvesThroughALiveLinkOnly(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	subject := func(sub string) string {
		t.Helper()
		blind, err := blinder.Subject("https://idp.example.com", sub)
		if err != nil {
			t.Fatalf("blind a subject: %v", err)
		}
		return blind
	}
	now := brokerAt.Add(time.Hour)
	enrol := func(login string) string {
		t.Helper()
		id := uuid.New().String()
		if err := rig.enrol(iamdomain.Enrolment{
			PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: login, Email: login + "@example.com", Login: login,
			OpID: "enrol-" + login, Reason: "a provider link",
		}); err != nil {
			t.Fatalf("enrol %s: %v", login, err)
		}
		return id
	}
	link := func(person, sub, replacing string) {
		t.Helper()
		if err := rig.during(func() error {
			_, err := rig.writer.Link(t.Context(), iamdomain.LinkChange{
				PersonID: person, Replacing: replacing,
				Link: iamdomain.Link{Issuer: "https://idp.example.com",
					Blind: subject(sub)},
				OpID: "link-" + person + "-" + sub, Reason: "pinned",
			})
			return err
		}); err != nil {
			t.Fatalf("link %s to %s: %v", person, sub, err)
		}
	}

	linked := enrol("ada.linked")
	link(linked, "ada", "")
	withdrawn := enrol("rex.withdrawn")
	link(withdrawn, "rex", "")
	if err := rig.during(func() error {
		_, err := rig.writer.Unlink(t.Context(), withdrawn,
			iamdomain.Link{Issuer: "https://idp.example.com",
				Blind: subject("rex")}, "unlink-rex", "left")
		return err
	}); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	moved := enrol("mo.moved")
	link(moved, "mo-old", "")
	link(moved, "mo-new", subject("mo-old"))
	first := enrol("dan.first")
	link(first, "dan", "")
	second := enrol("dan.second")
	rig.drain()
	// A RESTORE is the only thing that puts one subject on two people: the
	// rows it copies back never passed through the broker, so the second
	// holder is written straight into the estate rather than published.
	if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO iam_credentials
				(id, person_id, method, verifier, subject_blind, expires_at,
				 revoked_at, bucket, created_at, version, document)
			VALUES (?, ?, 'oidc', x'', ?, 0, 0, 0, 0, 1, x'')`,
			uuid.New().String(), second, subject("dan"))
		return err
	}); err != nil {
		t.Fatalf("restore a duplicate link: %v", err)
	}

	// THE CONTROL: the address column never matches a subject, so the
	// lookup that shipped refused a person this estate does link.
	if held, err := reader.PersonByEmailBlind(t.Context(), subject("ada")); err != nil ||
		held.ID != "" {
		t.Fatalf("the address column matched a subject (%q, %v): the two "+
			"blinds are separate classes and must never meet", held.ID, err)
	}

	for name, want := range map[string]struct {
		sub, id, login string
	}{
		"a linked subject":       {"ada", linked, "ada.linked"},
		"the subject moved TO":   {"mo-new", moved, "mo.moved"},
		"a withdrawn link":       {"rex", "", ""},
		"the subject moved OFF":  {"mo-old", "", ""},
		"a subject nobody links": {"nobody", "", ""},
	} {
		held, err := reader.PersonBySubjectBlind(t.Context(), subject(want.sub), now)
		if err != nil {
			t.Errorf("%s: %v, want no error", name, err)
		}
		if held.ID != want.id || held.Login != want.login {
			t.Errorf("%s resolved to %q (%q), want %q — a subject resolving "+
				"to anybody but its one live holder signs somebody in as "+
				"somebody else", name, held.ID, held.Login, want.id)
		}
	}

	held, err := reader.PersonBySubjectBlind(t.Context(), subject("dan"), now)
	if !errors.Is(err, iamdomain.ErrSubjectAmbiguous) {
		t.Fatalf("a subject two people hold answered %v, want "+
			"ErrSubjectAmbiguous", err)
	}
	if held.ID != "" {
		t.Errorf("an ambiguous subject resolved to %q", held.ID)
	}
	for _, id := range []string{first, second} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the refusal does not name holder %s, so an operator "+
				"cannot find the link to remove: %v", id, err)
		}
	}
}

// THE ESTATE SAYS WHETHER IT EVER USED THE BLIND KEY, which is what decides
// whether a missing key may be minted or was deleted.
//
// A machine enrolled with no address derives no blind, so an estate holding
// only machines is one a fresh key is simply the first key for — and refusing
// there would strand a company whose first identities were service accounts.
// One address is enough to make a fresh key an orphaning.
func TestTheEstateSaysWhetherItEverUsedTheBlindKey(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	holds := func() bool {
		t.Helper()
		held, err := reader.HoldsBlinds(t.Context())
		if err != nil {
			t.Fatalf("HoldsBlinds: %v", err)
		}
		return held
	}
	if holds() {
		t.Fatal("a fresh estate reports a blinded row")
	}
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: uuid.New().String(), Kind: iam.KindMachine,
		Stage: iam.StageActive, Name: "Release pipeline", Login: "ci:release",
		OpID: "enrol-machine", Reason: "a service account",
	}); err != nil {
		t.Fatalf("enrol a machine: %v", err)
	}
	rig.drain()
	if holds() {
		t.Error("a machine with no address counts as a blind, so a company " +
			"whose first identity was a service account could never mint a key")
	}
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: uuid.New().String(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Name: "Sarah Chen",
		Email: "sarah.chen@example.com", Login: "sarah.chen",
		OpID: "enrol-person", Reason: "the joiner",
	}); err != nil {
		t.Fatalf("enrol a person: %v", err)
	}
	rig.drain()
	if !holds() {
		t.Error("an estate holding an address reports none, so a deleted key " +
			"would be minted over and every address orphaned")
	}
}

// A RESERVATION IS READ AS ONE, AND NEVER AS A PERSON THAT WILL NOT DECODE.
//
// An enrolment is a sequence — the claims, then the person — and one that
// stops after a claim leaves a row with no kind, no stage and an empty
// document. Every reader used to decode that document as a person and fail, so
// the row turned into the UNKNOWN answer wherever it was touched: a Tier A
// token whose machine enrolment stopped after its login claim answered 503 on
// every guarded route, the directory listing failed whole, and the bootstrap
// counted it as somebody and closed for good. It is nobody, and every read
// here says so in its own shape.
func TestAReservationIsReadAsOneAndNotAsAnUndecodablePerson(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	// THE CLAIM ALONE, which is exactly what an enrolment that stopped
	// after its address leaves behind.
	id := uuid.New().String()
	const blind = "reserved-blind"
	if err := rig.claim(iamdomain.KindEmail, blind, id, "op-reserve"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	rig.drain()

	seen, err := reader.PersonByEmailBlind(t.Context(), blind)
	if err != nil {
		t.Fatalf("a reservation was read as an error: %v — a token bound "+
			"through it answers 503 on every guarded route", err)
	}
	if seen.ID != id || !seen.Reserved {
		t.Errorf("the reservation read as %+v, want person %s marked reserved",
			seen, id)
	}
	if seen.Kind != "" || seen.Stage.MayAct() || len(seen.Credentials) != 0 {
		t.Errorf("a reservation carries a kind %q, stage %q or credentials "+
			"%v — it is nobody and may do nothing", seen.Kind, seen.Stage,
			seen.Credentials)
	}

	anybody, err := reader.AnyPerson(t.Context())
	if err != nil {
		t.Fatalf("AnyPerson: %v", err)
	}
	if anybody {
		t.Error("a reservation counts as somebody enrolled, so a founder " +
			"whose bootstrap stopped after its address claim is refused the " +
			"retry that would finish it")
	}

	page, err := reader.People(t.Context(), iamdomain.PeopleQuery{})
	if err != nil {
		t.Fatalf("the directory failed over a reservation: %v", err)
	}
	if len(page.People) != 1 || !page.People[0].Reserved ||
		page.People[0].ID != id {
		t.Errorf("the directory lists %+v, want the one reservation, marked",
			page.People)
	}
	row, err := reader.Person(t.Context(), id)
	if err != nil || !row.Reserved {
		t.Errorf("Person(%s) = %+v, %v; want the reservation, marked", id, row, err)
	}

	// AND A BEARER NAMING IT FINDS NOBODY — not a person at no stage. The
	// two answers are opposite on a node that has applied an enrolment's
	// claims and not yet its person: absent lets the bearer's own start
	// position say "behind", where "found, may not act" would sign
	// somebody out on the one node that is merely late.
	identity, err := reader.Resolve(t.Context(), "no-such-lineage", id)
	if err != nil {
		t.Fatalf("Resolve over a reservation: %v", err)
	}
	if identity.Person.Found {
		t.Errorf("a reservation resolved as a person: %+v", identity.Person)
	}

	// AND A WRITE THAT FORMS THE NEXT DOCUMENT FROM IT IS REFUSED AS THE
	// ABSENCE IT IS, which a caller answers as "name somebody this node
	// holds" — not as a document that failed to open, which reads as a
	// newer peer's row and sends an operator looking for an upgrade.
	_, err = rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
		PersonID: id, OpID: "op-update-reservation",
		Apply: func(p iamdomain.Person) (iamdomain.Person, error) { return p, nil },
	})
	if !errors.Is(err, iamdomain.ErrNotFound) {
		t.Errorf("updating a reservation answered %v, want %v", err,
			iamdomain.ErrNotFound)
	}
}
