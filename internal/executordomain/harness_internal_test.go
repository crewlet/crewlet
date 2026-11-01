package executordomain

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE APPLY HARNESS: records applied through the shipped applier into a
// migrated replicated estate, at positions this harness hands out and broker
// instants a second apart, exactly as the framework's loop would apply them.
// The cases read the rows back with the same scan the reader uses.

// monday is the harness's clock.
var monday = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// estate is one node's replicated estate and the next position on its log.
type estate struct {
	t       *testing.T
	db      store.ReplicatedHandle
	applier *Applier
	seq     uint64
}

// newEstate opens a migrated estate, closed with the test.
func newEstate(t *testing.T) *estate {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &estate{t: t, db: db, applier: NewApplier()}
}

// stamped is the broker instant of the record at seq.
func stamped(seq uint64) time.Time { return monday.Add(time.Duration(seq) * time.Second) }

// apply applies one record at the next position and answers that position and
// how many rows it wrote.
func (e *estate) apply(subject Subject, op OpKind, scope ScopeSet, payload any) (statelog.Position, int) {
	e.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatalf("encode the %s payload: %v", op, err)
	}
	e.seq++
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID: statelog.NewOpID(monday, "harness"), Subject: subject, Op: op,
			CreatedAt: monday, Gen: 1, Writer: "node-a", Scope: scope,
		},
		Mutation: body, Actor: "executor", ActorKind: iam.ActorSystem,
	})
	if err != nil {
		e.t.Fatalf("encode the %s record: %v", op, err)
	}
	env, err := Domain{}.Envelope(encoded)
	if err != nil {
		e.t.Fatalf("envelope the %s record: %v", op, err)
	}
	position := statelog.Position{Stream: Domain{}.Stream().Name, Generation: 1, Seq: e.seq}
	record := statelog.Record{Envelope: env, Position: position, Payload: encoded,
		StoredAt: stamped(e.seq)}
	var written int
	if err := e.db.Tx(e.t.Context(), func(tx *sql.Tx) error {
		if _, gated, err := e.applier.Gated(e.t.Context(), tx, record); err != nil || gated {
			return err
		}
		var err error
		written, err = e.applier.Apply(e.t.Context(), tx, record, statelog.ApplyOptions{
			Now: monday, StoredAt: record.StoredAt, MaxVariables: 999,
		})
		return err
	}); err != nil {
		e.t.Fatalf("apply the %s record at %s: %v", op, position, err)
	}
	return position, written
}

// capture applies c's capture.
func (e *estate) capture(c Capture) (statelog.Position, int) {
	e.t.Helper()
	return e.apply(ConversationSubject(c.Handle, c.ConversationKey, c.Turn), OpCapture,
		seatScope(c.Handle), c)
}

// end applies how a turn ended.
func (e *estate) end(p End) (statelog.Position, int) {
	e.t.Helper()
	return e.apply(ConversationSubject(p.Handle, p.ConversationKey, p.Turn), OpEnd,
		ScopeSet{Subject: true}, p)
}

// removeSeat applies a removal of handle, and answers its position.
func (e *estate) removeSeat(handle string) statelog.Position {
	e.t.Helper()
	at, _ := e.apply(SeatSubject(handle), OpSeatDelete, ScopeSet{Subject: true},
		SeatDelete{V: 1, Handle: handle})
	return at
}

// at is the position of the last record e applied.
func (e *estate) at() statelog.Position {
	return statelog.Position{Stream: Domain{}.Stream().Name, Generation: 1, Seq: e.seq}
}

// reader is the domain's reader over e's rows, served at the position of the
// last record applied: the shipped reader through the framework's read
// authority, as a node caught up to that position serves it.
func (e *estate) reader() *Reader {
	e.t.Helper()
	authority, err := statelogtest.LocalReader(Domain{}, e.db, e.at())
	if err != nil {
		e.t.Fatalf("build the read authority: %v", err)
	}
	r, err := NewReader(ReaderOptions{DB: e.db.Reader(), Log: authority})
	if err != nil {
		e.t.Fatalf("build the reader: %v", err)
	}
	return r
}

// head is a conversation's head as a launch reads it, here at `stale`.
func (e *estate) head(handle, conversationKey string) Head {
	e.t.Helper()
	h, err := e.reader().Head(e.t.Context(), handle, conversationKey,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		e.t.Fatalf("read %s's head of %s: %v", handle, conversationKey, err)
	}
	return h
}

// launching is a capture of turn that began from h: its read's position and
// what the read handed out.
func launching(h Head, c Capture) Capture {
	c.ReadThrough, c.Absorbs = h.Position.Packed(), h.Absorbs()
	return c
}

// session is one turn's row, and false where there is none.
func (e *estate) session(handle, turn string) (Session, bool) {
	e.t.Helper()
	var (
		out   *Session
		found bool
	)
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		var err error
		out, err = oneSession(e.t.Context(), tx, `SELECT `+sessionColumns+`
			FROM executor_sessions WHERE agent_handle = ? AND turn_id = ?`, handle, turn)
		return err
	}); err != nil {
		e.t.Fatalf("read %s's turn %s: %v", handle, turn, err)
	}
	if out != nil {
		found = true
		return *out, found
	}
	return Session{}, found
}

// tombed reports a turn's tombstone and its reason.
func (e *estate) tombed(handle, turn string) (string, bool) {
	e.t.Helper()
	var reason string
	err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(e.t.Context(), `SELECT reason FROM executor_session_tombs
			WHERE agent_handle = ? AND turn_id = ?`, handle, turn).Scan(&reason)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false
	case err != nil:
		e.t.Fatalf("read %s's tombstone of %s: %v", handle, turn, err)
	}
	return reason, true
}

// newTurn is a fresh turn id, a run id as the engine mints one.
func newTurn() string { return statelog.NewOpID(monday, "") }

// keyAt is an object key minted at at.
func keyAt(at time.Time) objstore.Key { return objstore.KeyAt(at) }

// capturing is a capture of turn of seat, in conversation, of size bytes at
// ordinals (launch, seq), under a seat budget of budget.
func capturing(seat, conversation, turn string, launch, seq, size, budget int64) Capture {
	return Capture{
		V: 1, Handle: seat, Turn: turn, ConversationKey: conversation,
		Harness: "claude-code", HarnessVersion: "2.1.304", SessionID: uuid.NewString(),
		Object: keyAt(monday).String(), ObjectHash: string(objstore.HashOf([]byte(turn))),
		ObjectSize: size, ContentKey: "k1", ArchiveFormat: 1,
		LaunchSeq: launch, CaptureSeq: seq, Home: "/home/agent", Workspace: "/work",
		SeatBudget: budget,
	}
}

// ending is a turn's end in conversation.
func ending(seat, conversation, turn, work string, e Ending) End {
	return End{V: 1, Handle: seat, Turn: turn, ConversationKey: conversation,
		WorkKey: work, AskExcerpt: "fix the build", Ending: e}
}
