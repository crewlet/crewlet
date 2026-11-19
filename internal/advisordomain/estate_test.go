package advisordomain_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE APPLY HARNESS: one data node's estate, records run through the shipped
// gate and applier at instants the case chooses, as the framework's loop runs
// them, with no broker in between. Writers are the write harness's business.

// estate is one node's replicated estate and how far it has applied.
type estate struct {
	t       *testing.T
	db      store.ReplicatedHandle
	applier *advisordomain.Applier
	seq     uint64
}

// newEstate is a fresh estate at the migrated image.
func newEstate(t *testing.T) *estate {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &estate{t: t, db: db, applier: advisordomain.NewApplier()}
}

// position is the place of record seq on the log.
func position(seq uint64) statelog.Position {
	return statelog.Position{Stream: advisordomain.Domain{}.Stream().Name, Generation: 1, Seq: seq}
}

// apply runs one record at the log's next position, stored at `at`.
func (e *estate) apply(body []byte, at time.Time) {
	e.t.Helper()
	e.applyAt(e.seq+1, body, at)
}

// applyAt runs one record at position seq, stored at `at`: a later seq than
// any before is the log moving on, an earlier one a redelivery.
func (e *estate) applyAt(seq uint64, body []byte, at time.Time) {
	e.t.Helper()
	env, err := advisordomain.Domain{}.Envelope(body)
	if err != nil {
		e.t.Fatalf("envelope: %v", err)
	}
	e.seq = max(e.seq, seq)
	rec := statelog.Record{Envelope: env, Position: position(seq), Payload: body, StoredAt: at}
	if err := e.db.Tx(e.t.Context(), func(tx *sql.Tx) error {
		if _, gated, err := e.applier.Gated(e.t.Context(), tx, rec); err != nil || gated {
			return err
		}
		_, err := e.applier.Apply(e.t.Context(), tx, rec, statelog.ApplyOptions{
			Now: at, StoredAt: at, MaxVariables: 999})
		return err
	}); err != nil {
		e.t.Fatalf("apply record %d: %v", seq, err)
	}
}

// reader is this estate's read side, at what it has applied.
func (e *estate) reader() *advisordomain.Reader {
	e.t.Helper()
	authority, err := statelogtest.LocalReader(advisordomain.Domain{}, e.db, position(e.seq))
	if err != nil {
		e.t.Fatalf("build the read authority: %v", err)
	}
	r, err := advisordomain.NewReader(advisordomain.ReaderOptions{DB: e.db.Reader(), Log: authority})
	if err != nil {
		e.t.Fatalf("build the reader: %v", err)
	}
	return r
}

// count is how many rows a statement counts here.
func (e *estate) count(statement string, args ...any) int {
	e.t.Helper()
	var c int
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(e.t.Context(), statement, args...).Scan(&c)
	}); err != nil {
		e.t.Fatalf("count: %v", err)
	}
	return c
}

// record is one record on key as the engine's duty writes it, under opID.
func record(t *testing.T, key string, op advisordomain.OpKind, opID string, payload any) []byte {
	t.Helper()
	return recordBy(t, advisordomain.FindingSubject(key), op, opID, payload,
		iam.Actor{Name: "engine", Kind: iam.ActorSystem})
}

// recordBy is one record on subject by actor, under opID.
func recordBy(t *testing.T, subject advisordomain.Subject, op advisordomain.OpKind, opID string,
	payload any, actor iam.Actor) []byte {

	t.Helper()
	scope := sentinel()
	if op == advisordomain.OpExpire {
		scope = domain()
	}
	body, err := rawRecordBy(subject, "node-a", opID, op, scope, payload, actor)
	if err != nil {
		t.Fatalf("encode a %s record: %v", op, err)
	}
	return body
}
