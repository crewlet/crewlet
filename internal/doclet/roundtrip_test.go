package doclet_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WHOLE PATH: a write reaches the broker, the applier and the reader.
//
// The knowledge base's write path is a publisher and its read path is a set of
// applied rows, and nothing between them is mocked here — a real embedded
// broker, a real store, the shipped domain and the shipped applier. Every
// property this file asserts is one no unit test can reach, because each is a
// claim about the three of them agreeing.

var wednesday = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

type roundTrip struct {
	t       *testing.T
	db      store.ReplicatedHandle
	log     *js.DomainLog
	store   *doclet.Store
	applier *doclet.Applier
	reader  *doclet.Reader
	waiter  *testWaiter

	// signer is the key this node's publisher seals every record under,
	// held here too for the cases that append a record by hand — another
	// build's, a barrier — which the fleet's own writer would have signed
	// just the same, and which every reader refuses unsigned.
	signer *statelog.Signer

	// verifier opens a record's frame, because this harness replays the
	// log exactly as the framework's own loop does and that loop verifies
	// before any domain decodes.
	verifier *statelog.Verifier

	consumed uint64
}

// markLedgerLost moves the operation ledger's watermark to cutoff and deletes
// nothing, as the retention sweep records the cutoff it deleted below.
func (r *roundTrip) markLedgerLost(cutoff time.Time) {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO statelog_ops_lost (ops_table, lost_before) VALUES (?, ?)
			ON CONFLICT (ops_table) DO UPDATE SET
				lost_before = MAX(lost_before, excluded.lost_before)`,
			doclet.Domain{}.OpsTable(), store.EncodeTime(cutoff))
		return err
	}); err != nil {
		r.t.Fatalf("move the operation ledger's watermark: %v", err)
	}
}

// harnessSpaces are the containers every round trip declares: the ones its
// cases write in, and the root and skills containers it reserves.
var harnessSpaces = []string{"ENG", "OPS", "PROD", "SUITE", "HOME", "TS"}

func newRoundTrip(t *testing.T) *roundTrip {
	t.Helper()
	return newRoundTripOn(t, openLog(t), openNodeStore(t, "node.db"), "node-a")
}

// harnessReserved is what every round trip's store is told the company holds
// back and declares — see [newRoundTripOn].
func harnessReserved() doclet.Reserved {
	return doclet.Reserved{Skills: "TS", Root: "HOME", Declared: harnessSpaces}
}

// openLog stands up a broker and the knowledge base's log on it, both closed
// with the test.
func openLog(t *testing.T) *js.DomainLog {
	t.Helper()
	// THE BROKER DECLARES ITS SIZE, for brokertest's reason: left to derive
	// its cap from the runner's free space it takes three quarters of it,
	// and would refuse even this log's gibibyte on a runner with less than
	// a gibibyte and a third free, whatever the case under it does.
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
	spec := doclet.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD THIS HARNESS OVERRIDES, and it is the
	// floor Tier A holds stream.pages_log_max_bytes to: the smallest log any
	// node runs, which is the one the gate reserve takes the largest share
	// of, so every write here is admitted against the tightest ordinary
	// ceiling a production log has. Everything else is the domain's own
	// declaration, its largest record included.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: config.PagesLogMaxBytesFloor,
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

// openNodeStore opens one node's own store, closed with the test.
func openNodeStore(t *testing.T, name string) store.ReplicatedHandle {
	t.Helper()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), name), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}

// newRoundTripOn is the harness's node over a log and a store it is handed:
// the ones [newRoundTrip] opens, or a SECOND node joining the same log under
// its own id and its own store — the shape a race between two nodes needs.
func newRoundTripOn(t *testing.T, log *js.DomainLog, db store.ReplicatedHandle,
	nodeID string) *roundTrip {

	t.Helper()
	return newRoundTripReserving(t, log, db, nodeID, harnessReserved)
}

// newRoundTripReserving is [newRoundTripOn] with the store's own idea of what
// the company reserves and declares — nil for a store built with no seam at
// all, which is what a case about that zero needs.
func newRoundTripReserving(t *testing.T, log *js.DomainLog, db store.ReplicatedHandle,
	nodeID string, reserved func() doclet.Reserved) *roundTrip {

	t.Helper()
	rows, err := doclet.NewRows(db.Reader(), doclet.Domain{}.Stream())
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := doclet.NewFence(db.Reader(), nodeID)
	// The published trim floor is zero on a fleet that has never trimmed,
	// which is the state every new company is in — and the state in which
	// an absent anchor really does mean an unclaimed address. The log's own
	// first sequence is the fence's other bound and its last the check that
	// this node is on this log at all, both read from the stream the way
	// the engine reads them.
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	waiter := &testWaiter{}
	// THIS NODE'S CHECKPOINT IS THE WAITER'S, which is what the harness's
	// own applier advances — the position the end is compared against.
	fence.Committed = waiter.Committed
	// THE LOG'S GATE RESERVE, reading its usage from the stream as the
	// engine's does, so every write here is admitted as a production one is.
	reserve, err := statelog.NewReserve(doclet.Domain{}.Stream(),
		func(ctx context.Context) (statelog.Usage, error) {
			stats, err := log.Stats(ctx)
			return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
		})
	if err != nil {
		t.Fatalf("build the gate reserve: %v", err)
	}
	signer, verifier := testSigner(t, doclet.Domain{}), testVerifier(t, doclet.Domain{})
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: doclet.Domain{}, Spec: doclet.Domain{}.Stream(), Log: log,
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Rows:    rows, Fence: fence, Signer: signer,
		Gates: doclet.NewGates(db.Reader()), Waiter: waiter, Voids: waiter, Identity: waiter,
		NodeID:        nodeID,
		Admission:     reserve,
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	kb, err := doclet.NewStore(doclet.Options{
		Publisher: publisher, DB: db.Reader(),
		Now: func() time.Time { return wednesday },
		// EVERY HARNESS RESERVES TS AND HOME, so both rules are in force
		// on every case rather than only on the one that is about them —
		// a rule installed by its own test alone is one the rest of the
		// suite proves nothing about. And it DECLARES the spaces the cases
		// write in — the two reserved ones among them, as a company's
		// root and skills containers always are — for the same reason:
		// every create here passes the declared-space rule, so a case
		// creating somewhere undeclared fails rather than passing on a
		// store that checked nothing. See [harnessReserved].
		Reserved: reserved,
	})
	if err != nil {
		t.Fatalf("build the store: %v", err)
	}
	// THROUGH THE FRAMEWORK, like production. A harness that handed the
	// reader its own transaction would exercise the SQL and none of the
	// contract the rows are served under — which is the shape that let the
	// level be a label for as long as it was.
	authority, err := statelogtest.LocalReader(doclet.Domain{}, db,
		waiter.Committed())
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	reader, err := doclet.NewReader(doclet.ReaderOptions{Log: authority})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	r := &roundTrip{
		t: t, db: db, log: log, store: kb,
		applier: doclet.NewApplier(nodeID, nil, nil),
		reader:  reader, waiter: waiter,
		signer: signer, verifier: verifier,
	}
	// THIS NODE'S APPLIER RUNS WHILE A WRITE WAITS, as a running node's
	// does — see [testWaiter]. A case about a node that is behind says so
	// with [roundTrip.holdApplier].
	waiter.advance = r.drain
	return r
}

// drain consumes every record the broker holds beyond what this node has
// applied, exactly as the framework's own loop does — one transaction per
// record, carrying the rows, the operation id, the anchor and the checkpoint
// together.
func (r *roundTrip) drain() {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	spec := doclet.Domain{}.Stream()
	for seq := r.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil {
			r.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		// THE FRAME FIRST, exactly as the framework's loop does it: a
		// record is signed on its way to the appender, and what a
		// domain decodes is the BODY. A replay that handed the
		// domain's decoder these bytes would be reading the signature
		// as JSON, which is what this harness did for one commit and
		// what every reader of a signed log has to get right.
		body, verdict := r.verifier.Open(payload)
		if verdict != statelog.Verified {
			r.t.Fatalf("record %d did not verify: %s", seq, verdict)
		}
		env, err := doclet.DecodeEnvelope(body)
		if err != nil {
			r.t.Fatalf("decode record %d: %v", seq, err)
		}
		record := statelog.Record{
			Envelope: statelog.Envelope{
				V: env.V, Kind: string(env.Subject.Kind),
				Subject: statelog.Subject{
					Kind: string(env.Subject.Kind), ID: env.Subject.ID,
				},
				Op: string(env.Op), OpID: env.OpID, Gen: env.Gen,
				Writer: env.Writer, Scope: env.Scope.Resolve(env.Subject),
			},
			Position: statelog.Position{
				Stream: spec.Name, Generation: env.Gen, Seq: seq,
			},
			Payload:  body,
			StoredAt: storedAt,
		}
		if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
			reason, gated, err := r.applier.Gated(r.t.Context(), tx, record)
			if err != nil {
				return err
			}
			if gated {
				r.t.Logf("record %d gated: %s", seq, reason)
				return nil
			}
			if _, err := r.applier.Apply(r.t.Context(), tx, record,
				statelog.ApplyOptions{Now: wednesday, StoredAt: storedAt}); err != nil {
				return err
			}
			// THE WIRE SUBJECT, as the framework's own applier records
			// it: the ledger's subject is what a write resolving an op id
			// compares against, so a harness that wrote the bare one
			// would have every write it resolves refused as a reuse.
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO pages_ops (op_id, subject, position, applied_at)
				VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
				env.OpID, spec.SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed(), store.EncodeTime(storedAt)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO statelog_anchor (stream, subject, anchor)
				VALUES (?,?,?)
				ON CONFLICT (stream, subject) DO UPDATE SET
					anchor = MAX(anchor, excluded.anchor)`,
				record.Position.Stream,
				spec.SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed()); err != nil {
				return err
			}
			_, err = tx.ExecContext(r.t.Context(), `
				INSERT INTO statelog_cursor
					(stream, generation, seq, stream_created_at, updated_at, applied_version)
				VALUES (?,?,?,0,0, 0)
				ON CONFLICT (stream) DO UPDATE SET
					generation = excluded.generation, seq = excluded.seq`,
				record.Position.Stream, int64(record.Position.Generation),
				int64(record.Position.Seq))
			return err
		}); err != nil {
			r.t.Fatalf("apply record %d: %v", seq, err)
		}
		// THE POST-COMMIT HALF, which the framework runs after every
		// committed batch and never inside the transaction.
		r.applier.Committed(r.t.Context())
		r.consumed = seq
		r.waiter.reach(record.Position)
	}
}

// write creates one page and applies it.
func (r *roundTrip) write(actor doclet.Actor, in doclet.NewPage) doclet.Written {
	r.t.Helper()
	if in.Container == "" {
		in.Container = "ENG"
	}
	if in.Title == "" {
		in.Title = "a page"
	}
	got, err := r.store.Create(r.t.Context(), actor, in)
	if err != nil {
		r.t.Fatalf("create %q: %v", in.Title, err)
	}
	r.drain()
	return got
}

// get reads one page back.
func (r *roundTrip) get(ref string) doclet.Detail {
	r.t.Helper()
	detail, err := r.reader.Get(r.t.Context(), ref, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		r.t.Fatalf("get %q: %v", ref, err)
	}
	return detail
}

// Voided voids nothing: this harness never re-anchors, so no generation is
// ever abandoned.
func (w *testWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

// testWaiter is this node's own applier as the publisher and the reader see
// it.
//
// # It applies while a write waits, because a running node's applier does
//
// A write appends its record and then waits — out to the publisher's
// [statelog.Deps.ResolveBudget] — for this node to apply it, which is what
// lets the operation ledger answer `applied`. A node's applier consumes the log
// the whole time, so that wait ends in milliseconds; a harness whose applier
// ran only when a case called [roundTrip.drain] could only let it expire, and
// every write here sat out the full two seconds and answered `pending`. That
// was most of this package's time and none of what it asserts, and it took
// every write down the arm production almost never takes. So the wait runs
// [roundTrip.drain], on the writing goroutine — the publisher holds no
// transaction while it waits, its decision's snapshot already closed — and a
// case's own drain afterwards finds nothing left.
//
// # And a node that is behind says so, at once
//
// [roundTrip.holdApplier] takes the applier out of the wait: a write then
// resolves `pending` and a floored read refuses `behind`, which is a node whose
// applier has not reached the position. The wait answers what an expired budget
// answers, without spending it — nothing can move the position while the
// caller blocks here, so waiting would only arrive at the same answer later.
type testWaiter struct {
	mu sync.Mutex
	at statelog.Position

	// advance is this node's applier, run from inside a wait; nil is a
	// node holding its applier back — see [roundTrip.holdApplier].
	advance func()
}

// holdApplier keeps this node's applier out of every wait for the rest of the
// case: a write resolves `pending` and a read floored past what this node has
// applied refuses `behind`. A drain by hand brings the node level once and
// does NOT put the applier back in the wait, so the next write is `pending`
// again. It is the state of a node whose applier is behind its own log — the
// one a case about a write that has not landed here, or about a read that
// must not be served from before it, needs to hold.
func (r *roundTrip) holdApplier() {
	r.waiter.mu.Lock()
	defer r.waiter.mu.Unlock()
	r.waiter.advance = nil
}

func (w *testWaiter) reach(p statelog.Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.Packed() > w.at.Packed() {
		w.at = p
	}
}

func (w *testWaiter) Committed() statelog.Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

// StreamIdentity is always the live stream: this harness never rebuilds its
// log, so every position the waiter holds is a sequence on it.
func (w *testWaiter) StreamIdentity() error { return nil }
func (w *testWaiter) Truncated() error      { return nil }

func (w *testWaiter) WaitCommitted(ctx context.Context, p statelog.Position) error {
	for {
		if w.Committed().Packed() >= p.Packed() {
			return nil
		}
		w.mu.Lock()
		advance := w.advance
		w.mu.Unlock()
		if advance == nil {
			// HELD: nothing moves the position while the caller blocks
			// here, so answer what the budget's expiry would. THE
			// CALLER'S OWN ERROR FIRST: a context already cancelled is
			// a cancellation, and reporting it as a deadline would send
			// the publisher down the pending path after an explicit
			// cancel.
			if err := ctx.Err(); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		advance()
		if w.Committed().Packed() >= p.Packed() {
			return nil
		}
		// A POSITION PAST THE LOG'S END is not one this wait can reach
		// by applying; it can only be reached by a write that has not
		// happened yet, so the wait is the caller's budget.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (w *testWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {
	return w.WaitCommitted(ctx, p)
}

func author(handle string) doclet.Actor {
	return doclet.Actor{Handle: handle, Kind: doclet.AuthorHuman}
}

func agent(handle string) doclet.Actor {
	return doclet.Actor{Handle: handle, Kind: doclet.AuthorAgent, TurnID: "turn-" + handle}
}

func ptr[T any](v T) *T { return &v }

// testSigner is this suite's record signer.
//
// EVERY RECORD ON EVERY STATE LOG IS SIGNED, so a write authority built
// without one is refused at construction rather than producing records the
// fleet would refuse one at a time, on every node, for ever. The key is a
// fixture: what these cases are about is what the domain writes, and the
// signature's own rules are certified in [statelog].
func testSigner(t *testing.T, d statelog.Domain) *statelog.Signer {
	t.Helper()
	signer, err := statelog.NewSigner(d.Name(), statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

// testVerifier opens what [testSigner] sealed.
func testVerifier(t *testing.T, d statelog.Domain) *statelog.Verifier {
	t.Helper()
	verifier, err := statelog.NewVerifier(d.Name(), statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}
