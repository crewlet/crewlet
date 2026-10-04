package iamdomain_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
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
		Grants: []iam.Grant{iam.GrantStateRead},
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
	// THE GRANTS COME BACK: a reader that dropped them would compose a
	// principal indistinguishable from a person who was given less.
	if len(held.Grants) != 1 || held.Grants[0] != iam.GrantStateRead {
		t.Errorf("grants = %v, want state:read", held.Grants)
	}
}

// AND THE ESTATE KNOWS WHETHER ANYBODY IS IN IT, which is what `/health` tells
// an operator of a fresh install: nobody is enrolled yet, so the next step is
// to invite the first person. A COUNT and never a listing, because it is asked
// by an unauthenticated route and the answer is one bit.
//
// And "nobody" is PROVED against the log's end or not said: every node applies
// the identity log from boot, company or none, so a node that has just joined
// a fleet with people in it holds empty rows for a moment — and its /health
// and its boot log would tell an operator to invite a founder into a company
// that has one. Mutation: drop the coverage check and the behind arm answers
// (false, nil).
func TestTheEstateSaysWhetherAnybodyIsEnrolled(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	held, err := rig.anybody(t)
	if err != nil {
		t.Fatalf("AnyPerson: %v", err)
	}
	if held {
		t.Fatal("a fresh estate reports somebody in it, so its operator is " +
			"never told to invite the first person")
	}

	// ROWS BEHIND THE LOG CANNOT SAY NOBODY: the record they have not
	// applied may be the first person's.
	behind, err := rig.behind(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	held, err = reader.AnyPerson(t.Context(), behind)
	if !errors.Is(err, iamdomain.ErrNotCurrent) ||
		!errors.Is(err, statelog.ErrUnavailable) || held {
		t.Errorf("rows behind the log answered (%v, %v), want the unknown arm "+
			"— their \"nobody\" is a joining node telling its operator to "+
			"invite a founder into a company that has one", held, err)
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

	held, err = rig.anybody(t)
	if err != nil {
		t.Fatalf("AnyPerson: %v", err)
	}
	if !held {
		t.Error("an estate holding a person reports itself empty, so its " +
			"operator goes on being told nobody is in")
	}
}

// A NAMED SESSION THESE ROWS DO NOT HOLD IS NOBODY'S ONLY WHERE THEY HOLD THE
// WHOLE LOG.
//
// A named sign-out answers "ended" for a lineage nobody holds — the session is
// over, or never was. On rows that have not applied the session's start that
// was a session still running reported closed, with nothing written: an
// administrator ending a stolen laptop's session on a node behind the one that
// listed it was told it was done. So the absence is proved against the log's
// end, read first, exactly as [iamdomain.Reader.AnyPerson]'s "nobody" is. The
// controls: a held session is answered whatever the end, live and then over,
// and a lineage nobody opened is nobody's on rows that hold the whole log.
//
// Mutation: drop the proof from the absent arm and the behind case answers
// ("", false, nil).
func TestANamedSessionTheseRowsDoNotHoldIsNobodysOnlyWhereTheyHoldTheLog(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	person := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "enrol-sarah", Reason: "the joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	wall := time.Now().UTC()
	held := rig.openSession(person, wall.Add(24*time.Hour))
	end, err := rig.end(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	behind, err := rig.behind(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}

	for _, at := range []uint64{end, behind} {
		owner, live, err := reader.SessionStanding(t.Context(), held, wall, at)
		if err != nil || owner != person || !live {
			t.Errorf("a held session against end %d answered (%q, %v, %v), "+
				"want its owner, live", at, owner, live, err)
		}
	}
	nobody := uuid.Must(uuid.NewV7()).String()
	if owner, live, err := reader.SessionStanding(t.Context(), nobody, wall,
		end); err != nil || owner != "" || live {
		t.Errorf("a lineage nobody opened, on rows holding the whole log, "+
			"answered (%q, %v, %v), want nobody's", owner, live, err)
	}
	owner, live, err := reader.SessionStanding(t.Context(), nobody, wall, behind)
	if !errors.Is(err, iamdomain.ErrNotCurrent) ||
		!errors.Is(err, statelog.ErrUnavailable) || owner != "" || live {
		t.Errorf("a lineage these rows do not hold, on rows behind the log, "+
			"answered (%q, %v, %v), want the unknown arm — the record they "+
			"have not applied may be that session's start", owner, live, err)
	}

	rig.closeSession(person, held, "logout")
	if owner, live, err := reader.SessionStanding(t.Context(), held, wall,
		end); err != nil || owner != person || live {
		t.Errorf("an ended session answered (%q, %v, %v), want its owner, "+
			"not live", owner, live, err)
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
		end, err := rig.end(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		held, err := reader.HoldsBlinds(t.Context(), end)
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

	// AND "NONE" IS PROVED OR NOT SAID: rows that have not applied the
	// log's end cannot say a blinded row is not among what they are
	// missing, so they answer that they cannot say.
	end, err := rig.behind(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if held, err := reader.HoldsBlinds(t.Context(), end); !errors.Is(err,
		iamdomain.ErrNotCurrent) {
		t.Errorf("rows behind the log answered (%v, %v), want ErrNotCurrent — "+
			"their \"none\" is the answer that mints over a deleted key", held, err)
	}
}

// A WAIT IS FOR THE PACKED POSITION A BEARER STATES, ON THIS DOMAIN'S OWN LOG,
// AND A READER WITH NO APPLIER NEVER REPORTS ARRIVAL.
//
// The request guard waits here for a session's start before it decides a
// write on the rows, so the two halves are the whole contract: the position
// unpacks into the log this reader answers for, generation and all — a wait
// in the wrong number space arrives early or never — and a reader built with
// no runner behind it answers an error rather than nil, because a wait that
// reported arrival would send the guard to decide on rows that are not there.
//
// THE APPLIER HERE HAS COMMITTED NOTHING, which is the state a node is in when
// the first sign-in after a boot is answered: the stream is the domain's own,
// and one read back off an empty cursor would be no stream at all.
func TestAReaderWaitsForTheBearersPositionOnItsOwnLog(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	log, err := statelogtest.LocalReaderOver(
		iamdomain.Domain{}, rig.db.Replicated(), rig.waiter)
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	stream := iamdomain.Domain{}.Stream().Name
	var asked []statelog.Position
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB: rig.db, Log: log,
		Committed: func() statelog.Position { return statelog.Position{} },
		Await: func(_ context.Context, p statelog.Position) error {
			asked = append(asked, p)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	want := statelog.Position{Stream: stream, Generation: 2, Seq: 41}
	if err := reader.AwaitApplied(t.Context(), uint64(want.Packed())); err != nil {
		t.Fatalf("a wait the applier answered came back %v", err)
	}
	if len(asked) != 1 || asked[0] != want {
		t.Errorf("the applier was asked to wait for %v, want %v", asked, want)
	}

	bare, err := iamdomain.NewReader(iamdomain.ReaderOptions{DB: rig.db, Log: log})
	if err != nil {
		t.Fatalf("build a reader with no runner: %v", err)
	}
	if err := bare.AwaitApplied(t.Context(), 1); err == nil {
		t.Error("a reader with no applier reported a position reached")
	}
}
