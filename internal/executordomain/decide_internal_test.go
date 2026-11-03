package executordomain

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// newCapture is a capture's input naming an object minted at minted.
func newCapture(seat, conversation, turn string, launch, seq, size int64, minted time.Time) NewCapture {
	return NewCapture{
		Handle: seat, Turn: turn, ConversationKey: conversation,
		Harness: "claude-code", HarnessVersion: "2.1.304", SessionID: uuid.NewString(),
		Object: objstore.Object{Key: objstore.KeyAt(minted),
			Hash: objstore.HashOf([]byte(turn)), Size: size},
		ContentKey: "k1", ArchiveFormat: 1, LaunchSeq: launch, CaptureSeq: seq,
		Home: "/home/agent", Workspace: "/work", SeatBudget: budget,
	}
}

// decide runs a capture's decide over e's rows at at.
func (e *estate) decide(n NewCapture, at time.Time) error {
	e.t.Helper()
	var decided error
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		_, decided = decideCapture(e.t.Context(), tx, n, at)
		return nil
	}); err != nil {
		e.t.Fatalf("read the snapshot: %v", err)
	}
	return decided
}

// A CAPTURE OF AN OBJECT PAST objstore.RecordWithin IS REFUSED AT THE DECIDE, by
// the deciding node's clock: the collector may already be reclaiming an object
// minted that long ago, so a row must never name it. At the bound itself the
// capture is still decided.
//
// Mutation: drop the age check from decideCapture and a capture names an
// object the collector deletes from under it.
func TestACaptureOfAnObjectPastRecordWithinIsRefused(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	n := newCapture("ana", "chat:room", newTurn(), 1, 1, 100, monday)
	if err := e.decide(n, monday.Add(objstore.RecordWithin)); err != nil {
		t.Fatalf("a capture decided at the bound was refused: %v", err)
	}
	if err := e.decide(n, monday.Add(objstore.RecordWithin+time.Second)); !errors.Is(err, ErrUploadStale) {
		t.Fatalf("a capture decided past the bound: %v, want ErrUploadStale", err)
	}
}

// A CAPTURE THE ROWS CANNOT TAKE IS REFUSED BEFORE ANYTHING IS PUBLISHED: one
// not newer than the row's ordinals, one of an ended turn, and one the seat's
// budget cannot hold even with every ended session released — each with the
// sentinel its caller acts on.
//
// Mutation: drop any one arm of decideCapture and its capture is published to
// be dropped by every applier, its object left for the collector.
func TestACaptureTheRowsCannotTakeIsRefusedAtTheDecide(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run, ended := newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", run, 1, 2, 100, budget))
	if err := e.decide(newCapture("ana", "chat:room", run, 1, 2, 100, monday),
		monday); !errors.Is(err, ErrStaleCapture) {
		t.Errorf("a capture at the row's own ordinals: %v, want ErrStaleCapture", err)
	}
	e.capture(capturing("ana", "chat:room", ended, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", ended, "", Ending{Outcome: OutcomeCompleted}))
	if err := e.decide(newCapture("ana", "chat:room", ended, 2, 1, 100, monday),
		monday); !errors.Is(err, ErrTurnEnded) {
		t.Errorf("a capture of an ended turn: %v, want ErrTurnEnded", err)
	}
	tight := newCapture("ana", "chat:other", newTurn(), 1, 1, 150, monday)
	tight.SeatBudget = 200
	if err := e.decide(tight, monday); !errors.Is(err, ErrOverBudget) {
		t.Errorf("a capture the seat's live turns leave no room for: %v, want ErrOverBudget", err)
	}
}

// A CAPTURE NAMING ANOTHER CONVERSATION THAN ITS TURN'S IS REFUSED, so a turn's
// records are never arbitrated on two subjects.
func TestACaptureNamingAnotherConversationIsRefused(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
	if err := e.decide(newCapture("ana", "chat:elsewhere", run, 1, 2, 100, monday),
		monday); !errors.Is(err, ErrInvalid) {
		t.Errorf("a capture naming another conversation: %v, want ErrInvalid", err)
	}
}

// EVERY RECORD IS PUBLISHED UNDER AN OPERATION DERIVED PER EXECUTION: two
// captures of one launch are two operations, one capture retried is one, and a
// re-run of the same unit of work (another turn id) is another, which is what
// keeps a re-run's capture from being answered as the first run's.
//
// Mutation: derive the capture's operation from the work key and the two runs
// share one id.
func TestEveryRecordIsPublishedUnderAnOperationDerivedPerExecution(t *testing.T) {
	t.Parallel()
	first, rerun := newTurn(), newTurn()
	at := monday
	one := CaptureOpID(at, "ana", first, "launch-1", 1)
	if CaptureOpID(at, "ana", first, "launch-1", 1) != one {
		t.Error("a retried capture derived another operation")
	}
	for name, other := range map[string]string{
		"the launch's next capture": CaptureOpID(at, "ana", first, "launch-1", 2),
		"the next launch":           CaptureOpID(at, "ana", first, "launch-2", 1),
		"a re-run of the work":      CaptureOpID(at, "ana", rerun, "launch-1", 1),
		"the turn's end":            EndOpID(at, "ana", first),
		"the turn's begin":          BeginOpID(at, "ana", first),
	} {
		if other == one {
			t.Errorf("%s shares the capture's operation %s", name, one)
		}
	}
	if BeginOpID(at, "ana", first) == EndOpID(at, "ana", first) {
		t.Error("a turn's begin and its end share one operation")
	}
	if BeginOpID(at, "ana", first) != BeginOpID(at, "ana", first) {
		t.Error("a retried begin derived another operation")
	}
	for _, id := range []string{one, EndOpID(at, "ana", first), BeginOpID(at, "ana", first),
		SeatDeleteOpID(at, "ana")} {
		if err := statelog.CheckCallerOpID(id); err != nil {
			t.Errorf("derived operation %q is not one a call accepts: %v", id, err)
		}
	}
}

// A TURN IS KEYED BY ITS ONE SPELLING: a run id the engine mints is a UUID in
// canonical lowercase form, and every other spelling uuid.Parse accepts — the
// braced, the uppercase, the URN and the undashed — is refused rather than
// keyed on, since each would be a second row, a second conversation subject and
// a second associated data for one run, and the retention's check of the run's
// liveness would miss it.
//
// Mutation: check the turn with uuid.Parse alone and every spelling here is
// accepted.
func TestATurnIsKeyedByItsOneSpelling(t *testing.T) {
	t.Parallel()
	turn := newTurn()
	if err := checkTurn("ana", turn, "", ""); err != nil {
		t.Fatalf("a canonical run id was refused: %v", err)
	}
	for name, spelling := range map[string]string{
		"braced":    "{" + turn + "}",
		"uppercase": strings.ToUpper(turn),
		"a URN":     "urn:uuid:" + turn,
		"undashed":  strings.ReplaceAll(turn, "-", ""),
	} {
		if err := checkTurn("ana", spelling, "", ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("the %s spelling %q: %v, want ErrInvalid", name, spelling, err)
		}
	}
}

// A RECORD ABOUT A TURN NAMES THE TURN'S ONE UNIT OF WORK: a capture or an end
// naming another work key than the one the turn's row already carries is
// refused at the decide, as one naming another conversation is, while one
// naming none contradicts nothing.
//
// Mutation: drop the work-key arm from decideCapture or decideEnd and a turn's
// records disagree about which attempt at which work it is.
func TestARecordNamingAnotherUnitOfWorkIsRefused(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := newTurn()
	keyed := capturing("ana", "task:ENG-7", run, 1, 1, 100, budget)
	keyed.WorkKey = "work-1"
	e.capture(keyed)

	other := newCapture("ana", "task:ENG-7", run, 1, 2, 100, monday)
	other.WorkKey = "work-2"
	if err := e.decide(other, monday); !errors.Is(err, ErrInvalid) {
		t.Errorf("a capture naming another unit of work: %v, want ErrInvalid", err)
	}
	if err := e.decide(newCapture("ana", "task:ENG-7", run, 1, 2, 100, monday), monday); err != nil {
		t.Errorf("a capture naming no unit of work was refused: %v", err)
	}
	var decided error
	if err := e.db.Read(t.Context(), func(tx *sql.Tx) error {
		_, decided = decideEnd(t.Context(), tx, NewEnd{Handle: "ana", Turn: run,
			ConversationKey: "task:ENG-7", WorkKey: "work-2"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(decided, ErrInvalid) {
		t.Errorf("an end naming another unit of work: %v, want ErrInvalid", decided)
	}
}

// A CALL NAMES ITS AUTHOR AND AN OPERATION A RETRY CAN REPRODUCE, and a turn id
// that is not a run id is refused rather than keyed on.
func TestACallAndATurnAreRefusedWhenNoRecordCouldNameThem(t *testing.T) {
	t.Parallel()
	good := Call{By: iam.Actor{Name: "executor", Kind: iam.ActorSystem},
		OpID: statelog.NewOpID(monday, "x")}
	if err := good.validate(); err != nil {
		t.Fatalf("a well-formed call was refused: %v", err)
	}
	if err := (Call{By: good.By, OpID: "not an op id"}).validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a call with no operation id: %v, want ErrInvalid", err)
	}
	if err := checkTurn("ana", "turn-7", "", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("a turn id that is no run id: %v, want ErrInvalid", err)
	}
	if err := checkTurn("Ana.B", newTurn(), "", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("a handle no seat holds: %v, want ErrInvalid", err)
	}
	long := strings.Repeat("k", ConversationKeyMaxBytes+1)
	if err := checkTurn("ana", newTurn(), long, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("a conversation key past its bound: %v, want ErrInvalid", err)
	}
}

// decideBeginOf runs a begin's decide over e's rows: whether the turn's first
// read stands already, and what the decide refused.
func (e *estate) decideBeginOf(n NewBegin) (bool, error) {
	e.t.Helper()
	var (
		begun   bool
		decided error
	)
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		_, begun, decided = decideBegin(e.t.Context(), tx, n)
		return nil
	}); err != nil {
		e.t.Fatalf("read the snapshot: %v", err)
	}
	return begun, decided
}

// A BEGIN THE ROWS CANNOT TAKE IS REFUSED BEFORE ANYTHING IS PUBLISHED, and one
// the turn's first read already stands for publishes nothing: an ended turn's
// begin is refused ended, one naming another conversation or unit of work than
// the turn's row is invalid, a turn whose row holds a read is begun already,
// and a turn with no row is decided.
//
// Mutation: drop any arm of decideBegin and its begin is published to be
// dropped by every applier — or, the first read's, published at all.
func TestABeginTheRowsCannotTakeIsRefusedAtTheDecide(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	ended, keyed, begun := newTurn(), newTurn(), newTurn()
	e.capture(capturing("ana", "chat:room", ended, 1, 1, 100, budget))
	e.end(ending("ana", "chat:room", ended, "", Ending{Outcome: OutcomeCompleted}))
	work := capturing("ana", "chat:room", keyed, 1, 1, 100, budget)
	work.WorkKey = "work-1"
	e.capture(work)
	h := e.head("ana", "chat:room")
	e.begin(beginning(h, "ana", "chat:room", begun, ""))
	of := func(turn, conversation, work string) NewBegin {
		return NewBegin{Handle: "ana", Turn: turn, ConversationKey: conversation, WorkKey: work,
			ReadThrough: h.Position, Absorbs: h.Absorbs(), BaseTurn: h.Session.Turn,
			BaseObject: h.Session.Object}
	}
	for name, tc := range map[string]struct {
		begin NewBegin
		want  error
		begun bool
	}{
		"an ended turn":                      {of(ended, "chat:room", ""), ErrTurnEnded, false},
		"another conversation":               {of(keyed, "chat:elsewhere", ""), ErrInvalid, false},
		"another unit of work":               {of(keyed, "chat:room", "work-2"), ErrInvalid, false},
		"a turn whose first read stands":     {of(begun, "chat:room", ""), nil, true},
		"a turn with no row yet":             {of(newTurn(), "chat:room", ""), nil, false},
		"a captured turn's own unit of work": {of(keyed, "chat:room", "work-1"), nil, false},
	} {
		got, err := e.decideBeginOf(tc.begin)
		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) || got != tc.begun {
			t.Errorf("%s: decided begun %v (%v), want begun %v (%v)", name, got, err, tc.begun, tc.want)
		}
	}
}

// A BEGIN NO READ HANDS OUT IS REFUSED BY ITS WRITER, naming the field, before
// any snapshot is read: no conversation, no read, a read of another log, the
// turn as its own base, a base the read did not hand out, a base's session with
// no base turn, and a base turn with no session.
//
// Mutation: drop any arm of NewBegin.validate and its begin is published to be
// dropped by every applier.
func TestABeginNoReadHandsOutIsRefusedByItsWriter(t *testing.T) {
	t.Parallel()
	turn, base := newTurn(), newTurn()
	at := statelog.Position{Stream: Domain{}.Stream().Name, Generation: 1, Seq: 9}
	good := NewBegin{Handle: "ana", Turn: turn, ConversationKey: "chat:room", ReadThrough: at,
		Absorbs: []string{base}, BaseTurn: base, BaseObject: objstore.Object{Key: objstore.KeyAt(monday),
			Hash: objstore.HashOf([]byte(base)), Size: 100}}
	if err := good.validate(); err != nil {
		t.Fatalf("a begin as a read hands one out was refused: %v", err)
	}
	for name, spoil := range map[string]func(*NewBegin){
		"no conversation":              func(n *NewBegin) { n.ConversationKey = "" },
		"no read":                      func(n *NewBegin) { n.ReadThrough = statelog.Position{} },
		"a read of another log":        func(n *NewBegin) { n.ReadThrough.Stream = "CREWLET_MUTATION_LOG" },
		"the turn as its own base":     func(n *NewBegin) { n.BaseTurn = n.Turn },
		"a base the read did not hand": func(n *NewBegin) { n.Absorbs = nil },
		"a session with no base turn":  func(n *NewBegin) { n.BaseTurn = "" },
		"a base turn with no session":  func(n *NewBegin) { n.BaseObject = objstore.Object{} },
	} {
		n := good
		n.Absorbs = slices.Clone(good.Absorbs)
		spoil(&n)
		if err := n.validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("a begin with %s: %v, want ErrInvalid", name, err)
		}
	}
}
