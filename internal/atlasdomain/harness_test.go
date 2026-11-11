package atlasdomain_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/iam"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WRITE HARNESS: a real broker, the atlas log provisioned on it, and data
// nodes that each publish through the domain's own writer, signed, and apply
// every record the log holds, verified, through the shipped applier.

// writerClock is the instant every writer here authors its records at.
var writerClock = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// recordKey is the material every record here is signed under.
var recordKey = recordseal.OneKey("k1", "test-material")

// openAtlasLog is the atlas log on a broker of its own.
func openAtlasLog(t *testing.T) *js.DomainLog {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir(),
		StoreMaxBytes: brokertest.StoreMaxBytes})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	spec := atlasdomain.Domain{}.Stream()
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: atlasdomain.AtlasLogMaxBytes / 4,
		MaxMessageBytes: spec.MaxAppendBytes(), Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	return log
}

// writeNode is one data node: its own estate, a writer over the shared log and
// the applier that brings its estate up to the log.
type writeNode struct {
	t        *testing.T
	db       store.ReplicatedHandle
	log      *js.DomainLog
	writer   *atlasdomain.Writer
	applier  *atlasdomain.Applier
	waiter   *nodeWaiter
	verifier *recordseal.Verifier

	mu       sync.Mutex
	consumed uint64
}

// newWriteNode is a data node named nodeID over log.
func newWriteNode(t *testing.T, log *js.DomainLog, nodeID string) *writeNode {
	t.Helper()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), nodeID+".db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	d := atlasdomain.Domain{}
	spec := d.Stream()
	rows, err := atlasdomain.NewRows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := atlasdomain.NewFence(db.Reader(), nodeID)
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	waiter := &nodeWaiter{}
	fence.Committed = waiter.Reached
	reserve, err := statelog.NewReserve(spec, func(ctx context.Context) (statelog.Usage, error) {
		stats, err := log.Stats(ctx)
		return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
	})
	if err != nil {
		t.Fatalf("build the gate reserve: %v", err)
	}
	signer, err := recordseal.NewSigner(d.Name(), recordKey)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	verifier, err := recordseal.NewVerifier(d.Name(), recordKey)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: d, Spec: spec, Log: log,
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Rows:    rows, Fence: fence, Signer: signer,
		Gates: atlasdomain.NewGates(db.Reader()), Waiter: waiter, Voids: waiter,
		Identity: waiter, NodeID: nodeID, Admission: reserve,
		Generation: func() uint32 { return 0 }, ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	writer, err := atlasdomain.NewWriter(atlasdomain.WriterDeps{
		Publisher: publisher, DB: db.Reader(),
		Now: func() time.Time { return writerClock },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	n := &writeNode{t: t, db: db, log: log, writer: writer, waiter: waiter,
		verifier: verifier, applier: atlasdomain.NewApplier()}
	waiter.advance = n.drain
	return n
}

// drain applies every record the log holds past this node's own, each one
// verified first, as the framework's loop does.
func (n *writeNode) drain() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	last, err := n.log.End(n.t.Context())
	if err != nil {
		n.t.Fatalf("read the log's end: %v", err)
	}
	for seq := n.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := n.log.At(n.t.Context(), seq)
		if err != nil {
			n.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		body, verdict := n.verifier.Open(payload)
		if verdict != recordseal.Verified {
			n.t.Fatalf("record %d did not verify: %s", seq, verdict)
		}
		n.applyBody(seq, body, storedAt)
	}
}

// redeliver applies the log's records from the first again, as a node whose
// checkpoint did not commit with its rows would: every write is guarded.
func (n *writeNode) redeliver() {
	n.t.Helper()
	n.mu.Lock()
	n.consumed = 0
	n.mu.Unlock()
	n.drain()
}

// applyBody applies one verified record and records its operation, its anchor
// and the checkpoint, in one transaction.
func (n *writeNode) applyBody(seq uint64, body []byte, storedAt time.Time) {
	n.t.Helper()
	spec := atlasdomain.Domain{}.Stream()
	envelope, err := atlasdomain.Domain{}.Envelope(body)
	if err != nil {
		n.t.Fatalf("decode record %d: %v", seq, err)
	}
	record := statelog.Record{
		Envelope: envelope,
		Position: statelog.Position{Stream: spec.Name, Generation: envelope.Gen, Seq: seq},
		Payload:  body, StoredAt: storedAt,
	}
	wire := spec.SubjectPrefix + "." + envelope.Subject.String()
	if err := n.db.Tx(n.t.Context(), func(tx *sql.Tx) error {
		_, gated, err := n.applier.Gated(n.t.Context(), tx, record)
		if err != nil {
			return err
		}
		if !gated {
			if _, err := n.applier.Apply(n.t.Context(), tx, record, statelog.ApplyOptions{
				Now: writerClock, StoredAt: storedAt, MaxVariables: 999,
			}); err != nil {
				return err
			}
			if envelope.OpID != "" {
				if _, err := tx.ExecContext(n.t.Context(), `
					INSERT INTO atlas_ops (op_id, subject, position, applied_at)
					VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
					envelope.OpID, wire, record.Position.Packed(), store.EncodeTime(storedAt)); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(n.t.Context(), `
			INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?,?,?)
			ON CONFLICT (stream, subject) DO UPDATE SET anchor = MAX(anchor, excluded.anchor)`,
			record.Position.Stream, wire, record.Position.Packed()); err != nil {
			return err
		}
		_, err = tx.ExecContext(n.t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at, applied_version)
			VALUES (?,?,?,0,0,0)
			ON CONFLICT (stream) DO UPDATE SET generation = excluded.generation, seq = excluded.seq`,
			record.Position.Stream, int64(record.Position.Generation), int64(seq))
		return err
	}); err != nil {
		n.t.Fatalf("apply record %d: %v", seq, err)
	}
	n.consumed = seq
	n.waiter.reach(record.Position)
}

// reader is this node's read side, served at the position it has applied.
func (n *writeNode) reader() *atlasdomain.Reader {
	n.t.Helper()
	authority, err := statelogtest.LocalReader(atlasdomain.Domain{}, n.db, n.waiter.Reached())
	if err != nil {
		n.t.Fatalf("build the read authority: %v", err)
	}
	r, err := atlasdomain.NewReader(atlasdomain.ReaderOptions{DB: n.db.Reader(), Log: authority})
	if err != nil {
		n.t.Fatalf("build the reader: %v", err)
	}
	return r
}

// count is how many rows a statement counts on this node.
func (n *writeNode) count(statement string, args ...any) int {
	n.t.Helper()
	var c int
	if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), statement, args...).Scan(&c)
	}); err != nil {
		n.t.Fatalf("count: %v", err)
	}
	return c
}

// dump is every reproducible table on this node, row by row, in key order.
func (n *writeNode) dump() map[string][]string {
	n.t.Helper()
	out := map[string][]string{}
	for _, table := range atlasdomain.ReproducibleTables {
		if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(n.t.Context(), `SELECT * FROM `+table+` ORDER BY 1, 2`)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			for rows.Next() {
				values := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					return err
				}
				out[table] = append(out[table], fmt.Sprintf("%v", values))
			}
			return rows.Err()
		}); err != nil {
			n.t.Fatalf("dump %s: %v", table, err)
		}
	}
	return out
}

// nodeWaiter is a node's applier as its publisher sees it: a wait drains the
// log once, which applies every record already appended.
type nodeWaiter struct {
	mu      sync.Mutex
	at      statelog.Position
	advance func()
}

func (w *nodeWaiter) reach(p statelog.Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.Packed() > w.at.Packed() {
		w.at = p
	}
}

func (w *nodeWaiter) Reached() statelog.Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

func (w *nodeWaiter) StreamIdentity() error                         { return nil }
func (w *nodeWaiter) Truncated() error                              { return nil }
func (w *nodeWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

// WaitReached drains the log once: a publisher waits only on a record it has
// already appended, so one drain reaches it or nothing will.
func (w *nodeWaiter) WaitReached(ctx context.Context, p statelog.Position) error {
	if w.Reached().Packed() >= p.Packed() {
		return nil
	}
	w.mu.Lock()
	advance := w.advance
	w.mu.Unlock()
	if advance != nil {
		advance()
	}
	if w.Reached().Packed() >= p.Packed() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("harness: the log holds no record at the position waited on")
}

func (w *nodeWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet, p statelog.Position) error {
	return w.WaitReached(ctx, p)
}

// engineCall is a write as the engine makes one, under a fresh operation.
func engineCall() atlasdomain.Call {
	return atlasdomain.Call{By: iam.Actor{Name: "engine", Kind: iam.ActorSystem},
		OpID: statelog.NewOpID(time.Now(), "atlas")}
}

// personCall is a write a person at their seat makes, under a fresh operation.
func personCall(handle string) atlasdomain.Call {
	return atlasdomain.Call{By: iam.Actor{Name: handle, Kind: iam.ActorHuman, OperatorID: handle + "@acme.example"},
		OpID: statelog.NewOpID(time.Now(), "atlas")}
}

// adminCall is an administrator's purge, under a fresh operation.
func adminCall() atlasdomain.Call { return personCall("admin") }

// must is a claim the vocabulary builds, or the test stops.
func must(t *testing.T) func(atlas.Claim, error) atlas.Claim {
	return func(c atlas.Claim, err error) atlas.Claim {
		t.Helper()
		if err != nil {
			t.Fatalf("build a claim: %v", err)
		}
		return c
	}
}

// asPerson is the source the writer replaces with its caller: any person will do.
var asPerson = atlas.PersonSource("whoever")

// record writes one claim as handle on n, at no version, and stops the test on
// a refusal.
func (n *writeNode) record(handle string, c atlas.Claim) statelog.Result {
	n.t.Helper()
	res, err := n.writer.Record(n.t.Context(), personCall(handle), c, atlasdomain.NoIfMatch)
	if err != nil {
		n.t.Fatalf("record %s's claim on %s: %v", handle, c.Subject, err)
	}
	return res
}

// live is every live claim on n, read at what n has applied.
func (n *writeNode) live() []atlas.Claim {
	n.t.Helper()
	got, err := n.reader().Live(n.t.Context(), statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		n.t.Fatalf("read the live claims: %v", err)
	}
	return got.Claims
}

// view is the atlas n's live claims fold to.
func (n *writeNode) view() atlas.View {
	n.t.Helper()
	return atlas.Fold(n.live(), time.Now().UTC())
}
