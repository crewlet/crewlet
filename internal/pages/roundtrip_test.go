package pages_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pages"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
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
	db      *store.DB
	log     *js.DomainLog
	store   *pages.Store
	applier *pages.Applier
	reader  *pages.Reader
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

	// storeOptions and readerOptions are what the store and the reader were
	// built from, kept so a case about the person seam can rebuild both
	// over it ([roundTrip.knowing]) on the same publisher and rows.
	storeOptions  pages.Options
	readerOptions pages.ReaderOptions

	consumed uint64
}

func newRoundTrip(t *testing.T) *roundTrip {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	spec := pages.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD THIS HARNESS OVERRIDES, and it is the
	// floor Tier A holds stream.pages_log_max_bytes to: the smallest log any
	// node runs, which is the one the gate reserve takes the largest share
	// of, so every write here is admitted against the tightest ordinary
	// ceiling a production log has — and a reservation an embedded broker
	// in a temporary directory can grant, where a default sized for years
	// of a company's growth may not be. Everything else is the domain's
	// own declaration, its largest record included.
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
	return newRoundTripOn(t, log, openNodeStore(t, "node.db"), "node-a")
}

// openNodeStore opens one node's own store, closed with the test.
func openNodeStore(t *testing.T, name string) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), name),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}

// newRoundTripOn is the harness's node over a log and a store it is handed:
// the ones [newRoundTrip] opens, or a SECOND node joining the same log under
// its own id and its own store — the shape a race between two nodes needs.
func newRoundTripOn(t *testing.T, log *js.DomainLog, db *store.DB,
	nodeID string) *roundTrip {

	t.Helper()
	rows, err := pages.NewRows(db)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := pages.NewFence(db, nodeID)
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
	reserve, err := statelog.NewReserve(pages.Domain{}.Stream(),
		func(ctx context.Context) (statelog.Usage, error) {
			stats, err := log.Stats(ctx)
			return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
		})
	if err != nil {
		t.Fatalf("build the gate reserve: %v", err)
	}
	signer := testSigner(t, pages.Domain{})
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: pages.Domain{}, Log: log, Rows: rows, Fence: fence,
		Signer: signer,
		Gates:  pages.NewGates(db), Waiter: waiter, Identity: waiter, NodeID: nodeID,
		Admission:     reserve,
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	storeOptions := pages.Options{
		Publisher: publisher, DB: db,
		Now: func() time.Time { return wednesday },
		// EVERY HARNESS RESERVES TS AND HOME, so both rules are in force
		// on every case rather than only on the one that is about them —
		// a rule installed by its own test alone is one the rest of the
		// suite proves nothing about.
		Reserved: func() pages.Reserved {
			return pages.Reserved{Skills: "TS", Root: "HOME"}
		},
	}
	kb, err := pages.NewStore(storeOptions)
	if err != nil {
		t.Fatalf("build the store: %v", err)
	}
	// THROUGH THE FRAMEWORK, like production. A harness that handed the
	// reader its own transaction would exercise the SQL and none of the
	// contract the rows are served under — which is the shape that let the
	// level be a label for as long as it was.
	authority, err := statelogtest.LocalReader(pages.Domain{}, db.Replicated(),
		waiter.Committed())
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	readerOptions := pages.ReaderOptions{
		DB: db, Log: authority, Committed: waiter.Committed,
	}
	reader, err := pages.NewReader(readerOptions)
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	return &roundTrip{
		t: t, db: db, log: log, store: kb,
		applier: pages.NewApplier(nodeID, nil, nil),
		reader:  reader, waiter: waiter,
		signer: signer, verifier: testVerifier(t, pages.Domain{}),
		storeOptions: storeOptions, readerOptions: readerOptions,
	}
}

// knowing rebuilds this node's store and reader over a person seam, as the
// engine builds both — on the same publisher and the same rows, so what was
// written before is still there to be read.
func (r *roundTrip) knowing(ids pages.Identities) {
	r.t.Helper()
	opts, readOpts := r.storeOptions, r.readerOptions
	opts.Identities, readOpts.Identities = ids, ids
	kb, err := pages.NewStore(opts)
	if err != nil {
		r.t.Fatalf("rebuild the store: %v", err)
	}
	reader, err := pages.NewReader(readOpts)
	if err != nil {
		r.t.Fatalf("rebuild the reader: %v", err)
	}
	r.store, r.reader = kb, reader
}

// appendSigned puts one hand-built record on the log, SEALED under this
// fleet's key exactly as the publisher seals what it appends — a record some
// writer of this fleet wrote, only not through this build's decide. Unsigned it
// would be a record no node applies, which is a different case from the one a
// caller of this means.
func (r *roundTrip) appendSigned(subject, msgID string, body []byte) {
	r.t.Helper()
	if _, _, err := r.log.Append(r.t.Context(), subject, msgID, nil,
		r.signer.Seal(body)); err != nil {
		r.t.Fatalf("append a record on %s: %v", subject, err)
	}
}

// standingLog is this harness's log as a standing read is handed it: the
// broker's per-subject probe, and each record's BODY through
// [statelog.StandingOf], because what the broker holds is the signed frame and
// a standing decoded off the frame is no standing at all.
func (r *roundTrip) standingLog() statelog.StandingLog {
	return statelog.StandingOf(r.log, r.verifier)
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
	spec := pages.Domain{}.Stream()
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
		env, err := pages.DecodeEnvelope(body)
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
		if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
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
					(stream, generation, seq, stream_created_at, updated_at)
				VALUES (?,?,?,0,0)
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
func (r *roundTrip) write(actor pages.Actor, in pages.NewPage) pages.Written {
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
func (r *roundTrip) get(ref string) pages.Detail {
	r.t.Helper()
	detail, err := r.reader.Get(r.t.Context(), ref, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		r.t.Fatalf("get %q: %v", ref, err)
	}
	return detail
}

// testWaiter is this node's own applier as the publisher sees it.
type testWaiter struct {
	mu sync.Mutex
	at statelog.Position

	// advance, when set, is this node's applier run from inside a write's
	// own wait — see [roundTrip.applyWhileWriting].
	advance func()
}

// applyWhileWriting makes this node's applier run from inside a write's own
// wait, which is what it does in production and what this harness otherwise
// cannot express: a write that lost the broker's arbitration to a peer's
// record waits for this node to apply that record before it decides again,
// and in a harness where nothing consumes the log during a call that wait can
// only expire.
func (r *roundTrip) applyWhileWriting() {
	r.waiter.mu.Lock()
	defer r.waiter.mu.Unlock()
	r.waiter.advance = r.drain
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
		if advance != nil {
			advance()
		}
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

func author(handle string) pages.Actor {
	return pages.Actor{Handle: handle, Kind: pages.AuthorHuman}
}

func agent(handle string) pages.Actor {
	return pages.Actor{Handle: handle, Kind: pages.AuthorAgent, TurnID: "turn-" + handle}
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
