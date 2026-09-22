package iamdomain_test

import (
	"testing"

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
