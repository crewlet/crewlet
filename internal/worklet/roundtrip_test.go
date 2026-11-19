package worklet_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// aTurnNobodyWoke is the provenance of an agent's writer in a turn nobody
// woke (types.ForTurn(nil)): an agent's writer must state whom it writes for
// (ADR-0109), and every case writing as an agent about something else uses this.
func aTurnNobodyWoke() worklet.Provenance {
	return worklet.Provenance{WrittenFor: types.ForTurn(nil)}
}

// THE ROUND TRIP: a write reaches the broker, the applier consumes it, and the
// reader answers from the rows it wrote. Each layer's fakes can pass while the
// composition is wrong (a subject the dispatch does not recognise, a scope
// resolved two ways, an envelope read back empty); only a real broker and a
// real store on both ends show it.
type roundTrip struct {
	t        *testing.T
	broker   *js.Queue
	node     *store.DB
	db       store.ReplicatedHandle
	log      *js.DomainLog
	writer   *worklet.Writer
	applier  *worklet.Applier
	reader   *worklet.Reader
	waiter   *testWaiter
	metrics  *metrics.Recorder
	consumed uint64

	// reserve is the log's gate reserve, one per log per node as the engine
	// holds it, shared by every publisher the harness builds.
	reserve *statelog.Reserve

	// claims is where the writer takes a walking sequence's claim, so a case
	// can hold one as ANOTHER node. The in-memory twin unless a case moves it
	// ([roundTrip.claimOn]).
	claims coord.Backend

	// publisher is kept so a writer rebuilt over other coordination writes
	// exactly as this one did.
	publisher *statelog.Publisher

	// verifier opens a record's frame: the framework's loop verifies before
	// any domain decodes, and this harness replays as it does.
	verifier *recordseal.Verifier

	// at is the writer's AUTHORED clock; `wednesday` unless a case is about
	// the order of instants somebody typed.
	at time.Time

	// nodeID is this harness's node, for a second writer over the same
	// store ([roundTrip.lossyWriter]).
	nodeID string

	// leads is the writer's chart seam ([worklet.Writer.Leads]): nil unless
	// a case is about whom a fallback reaches ([roundTrip.ledBy]).
	leads worklet.Leads
}

func newRoundTrip(t *testing.T) *roundTrip {
	t.Helper()
	r := newRoundTripWithoutProject(t)
	// THE PROJECT FIRST: a create mints its key from the project's counter,
	// which this node must have applied.
	if _, err := r.writer.WriteDocument(t.Context(), "op-project",
		worklet.ProjectSubject("ENG"), "", worklet.Project{
			V: 1, Key: "ENG", Name: "Engineering",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, worklet.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	r.drain()
	return r
}

// newRoundTripWithoutProject is the harness with NO project seeded, a company
// as it boots, for the chart apply's cases that need the before.
func newRoundTripWithoutProject(t *testing.T) *roundTrip {
	t.Helper()
	// THE BROKER DECLARES ITS SIZE (brokertest.StoreMaxBytes): derived from
	// the runner's free disk, it would refuse this log's gibibyte on a
	// runner short of space.
	q, err := js.Open(t.Context(), js.Config{StoreDir: brokertest.TempDir(t),
		StoreMaxBytes: brokertest.StoreMaxBytes})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	spec := worklet.Domain{}.Stream()
	// ONLY THE CEILING IS OVERRIDDEN, to stream.tracker_log_max_bytes's
	// floor: the smallest log, where the gate reserve takes the largest
	// share, so every write meets the tightest production ceiling.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: config.TrackerLogMaxBytesFloor,
		MaxMessageBytes: spec.MaxAppendBytes(), Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return newRoundTripOn(t, q, log, dbNode, db, "node-a")
}

// newRoundTripOn is the harness's node over a broker, log and store it is
// handed: [newRoundTripWithoutProject]'s, or a second node (one that adopted a
// snapshot, say) joining the same log under its own id.
func newRoundTripOn(t *testing.T, q *js.Queue, log *js.DomainLog, node *store.DB,
	db store.ReplicatedHandle, nodeID string) *roundTrip {

	t.Helper()
	rows, err := worklet.NewRows(db.Reader(), worklet.Domain{}.Stream())
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	// THE HARNESS BEFORE THE WRITER: the writer's clock reads r.at.
	r := &roundTrip{t: t, broker: q, node: node, db: db, log: log, at: wednesday,
		claims: memory.New(), nodeID: nodeID}

	fence := worklet.NewFence(db.Reader(), nodeID)
	// A never-trimmed fleet's floor is zero, where an absent anchor really
	// means an empty subject. The log's first and last sequences are read
	// from the stream as the engine reads them.
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	waiter := &testWaiter{}
	// THIS NODE'S CHECKPOINT IS THE WAITER'S, which the harness's applier
	// advances.
	fence.Committed = waiter.Committed
	// A REAL RECORDER: the session wait reaches no result, so its HISTOGRAM
	// is a case's only witness that a gesture waited for its own append.
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("build a metrics recorder: %v", err)
	}
	r.metrics = recorder
	// THE GATE RESERVE reads usage from the stream as the engine's does.
	reserve, err := statelog.NewReserve(worklet.Domain{}.Stream(),
		func(ctx context.Context) (statelog.Usage, error) {
			stats, err := log.Stats(ctx)
			return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
		})
	if err != nil {
		t.Fatalf("build the gate reserve: %v", err)
	}
	r.reserve = reserve
	// THE VERIFIER BEFORE THE PUBLISHER, which reads the log's records (a
	// lost acknowledgement's, a retry's) through it.
	r.verifier = testVerifier(t, worklet.Domain{})
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: worklet.Domain{}, Spec: worklet.Domain{}.Stream(), Log: log,
		Records: statelog.VerifiedLog{Log: log, Verifier: r.verifier},
		Signer:  testSigner(t, worklet.Domain{}), Rows: rows, Fence: fence,
		Gates: worklet.NewGates(db.Reader()), Waiter: waiter, Voids: waiter, Identity: waiter, NodeID: nodeID,
		Metrics:       recorder,
		Admission:     reserve,
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	r.publisher = publisher
	// A REAL CLAIM BACKEND: the WALKING sequences (the cross-project move,
	// the trash's subtree walk) refuse without one. In-memory suffices; the
	// claim only excludes a SECOND node.
	writer := r.writerClaiming(r.claims)
	// THE READ AUTHORITY IS LOCAL: this harness drives the applier itself,
	// and the barrier belongs to cases with a quorum to commit against.
	logReader, err := statelogtest.LocalReader(worklet.Domain{}, db,
		statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := worklet.NewReader(db.Reader(), logReader, nil)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	r.writer, r.applier = writer, worklet.NewApplier(nil)
	r.reader, r.waiter = reader, waiter
	return r
}

// writerClaiming is this harness's writer taking its walk claims from claims.
func (r *roundTrip) writerClaiming(claims coord.Backend) *worklet.Writer {
	r.t.Helper()
	writer, err := worklet.NewWriter(worklet.WriterDeps{
		Publisher: r.publisher, DB: r.db.Reader(), NodeID: r.nodeID,
		Claims: claims, Leads: r.leads,
		Actor: "ana", ActorKind: worklet.AuthorHuman,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		r.t.Fatalf("build the writer: %v", err)
	}
	return writer
}

// claimOn moves this node's coordination, and the writer's walk claims, to
// claims. A case about what the fleet's store will HONOUR needs that store:
// the twin keeps any deadline a claim asks for.
func (r *roundTrip) claimOn(claims coord.Backend) {
	r.t.Helper()
	r.claims = claims
	r.writer = r.writerClaiming(claims)
}

// ledBy rebuilds this node's writer over the same publisher and coordination
// with a chart to resolve leads through, as the engine's writer holds one.
func (r *roundTrip) ledBy(leads worklet.Leads) {
	r.t.Helper()
	r.leads = leads
	r.writer = r.writerClaiming(r.claims)
}

// holdTheAppliersPin holds the estate's pinned writer for the test's life, as
// a running node's applier does, so housekeeping that wants the pin cannot
// pass here where it would fail on a live node.
func holdTheAppliersPin(t *testing.T, r *roundTrip) {
	t.Helper()
	w, err := r.db.Writer(t.Context())
	if err != nil {
		t.Fatalf("pin the applier's writer: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
}

// applyWhileWriting makes this node's applier run from inside a write's own
// wait, as in production.
//
// Other cases drive the applier BY HAND, deliberately: a create's `pending`
// outcome is reachable only while nothing has consumed it. But a GESTURE
// writing one subject twice waits for its own earlier append
// ([worklet.Writer.After]), which with nothing consuming during the call can
// only expire, so a case about a whole sequence turns this on.
func (r *roundTrip) applyWhileWriting() {
	r.waiter.mu.Lock()
	defer r.waiter.mu.Unlock()
	r.waiter.advance = r.drain
}

// lagBehindOwnWrites is a node whose applier is behind its own appends: it
// runs in the session wait [worklet.Writer.After] asks for, never to resolve
// an outcome, so every append reports `pending` and a gesture's later append
// sees an earlier one only by waiting for it.
func (r *roundTrip) lagBehindOwnWrites() {
	r.waiter.mu.Lock()
	defer r.waiter.mu.Unlock()
	r.waiter.advance = r.drain
	r.waiter.lagOutcomes = true
}

// applyOnlyOnDrain undoes [roundTrip.applyWhileWriting], so a case can put a
// whole sequence on the log and read before any of it applies.
func (r *roundTrip) applyOnlyOnDrain() {
	r.waiter.mu.Lock()
	defer r.waiter.mu.Unlock()
	r.waiter.advance = nil
	r.waiter.lagOutcomes = false
}

// drain applies every record beyond this node's checkpoint as the
// framework's loop does: one transaction per record, rows and checkpoint
// together.
func (r *roundTrip) drain() {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	r.apply(r.consumed+1, last)
}

// redeliver re-applies one consumed record: a redelivery, a reprocess after
// an upgrade or a snapshot adopter's replay.
func (r *roundTrip) redeliver(seq uint64) {
	r.t.Helper()
	consumed := r.consumed
	r.apply(seq, seq)
	r.consumed = consumed
}

// open unwraps one record's signature frame and hands back its body: as in
// the framework's loop, a domain decodes the BODY, never the signed bytes the
// log holds.
func (r *roundTrip) open(seq uint64, framed []byte) []byte {
	r.t.Helper()
	body, verdict := r.verifier.Open(framed)
	if verdict != recordseal.Verified {
		r.t.Fatalf("record %d did not verify: %s", seq, verdict)
	}
	return body
}

func (r *roundTrip) apply(from, last uint64) {
	r.t.Helper()
	for seq := from; seq <= last; seq++ {
		subject, payload, storedAt, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil {
			r.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		env, err := worklet.DecodeEnvelope(r.open(seq, payload))
		if err != nil {
			r.t.Fatalf("decode record %d: %v", seq, err)
		}
		_ = subject
		// AS THE FRAMEWORK'S LOOP HANDS IT ([statelog.AppliedInstant]).
		storedAt = statelog.AppliedInstant(storedAt)
		record := statelog.Record{
			Envelope: statelog.Envelope{
				V: env.V, Kind: string(env.Subject.Kind),
				Subject: statelog.Subject{
					Kind: string(env.Subject.Kind), ID: env.Subject.ID,
				},
				Op: string(env.Op), OpID: env.OpID, Gen: env.Gen,
				Writer: env.Writer,
			},
			Position: statelog.Position{
				Stream: worklet.Domain{}.Stream().Name, Generation: env.Gen, Seq: seq,
			},
			Payload:  r.open(seq, payload),
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
			// ROWS, OP ID AND ANCHOR IN ONE TRANSACTION, as the
			// framework commits them: rows without the anchor would
			// pair an old decision with a new expectation.
			if _, err := r.applier.Apply(r.t.Context(), tx, record,
				statelog.ApplyOptions{Now: wednesday, StoredAt: storedAt,
					MaxVariables: r.db.Caps().MaxVariables}); err != nil {
				return err
			}
			// THE WIRE SUBJECT, as the framework records it: a write
			// resolving an op id compares against it, and the bare one
			// would be refused as a reuse.
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO tracker_ops (op_id, subject, position, applied_at)
				VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
				env.OpID,
				worklet.Domain{}.Stream().SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed(), store.EncodeTime(storedAt)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO statelog_anchor (stream, subject, anchor)
				VALUES (?,?,?)
				ON CONFLICT (stream, subject) DO UPDATE SET
					anchor = MAX(anchor, excluded.anchor)`,
				record.Position.Stream,
				worklet.Domain{}.Stream().SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed()); err != nil {
				return err
			}
			// AND THE CHECKPOINT, in the same transaction: without it
			// every read answers position zero and no lag assertion
			// means anything.
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
		r.consumed = seq
		r.waiter.reach(record.Position)
	}
}

// scopeOfLastRecord is the resolved scope of the newest record on the log,
// read FROM THE WIRE: a deferral is filed under what the RECORD carries, and
// the writer's struct would pass for a scope that never reached the broker.
func (r *roundTrip) scopeOfLastRecord() statelog.ScopeSet {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v", last, err)
	}
	env, err := worklet.DecodeEnvelope(r.open(last, payload))
	if err != nil {
		r.t.Fatalf("decode record %d: %v", last, err)
	}
	return env.Scope.Resolve(env.Subject)
}

// askErr is [roundTrip.ask] for a read expected to REFUSE. A parse error is
// fatal, so the case tests the reader's gate: a refusal that moved into
// [ParseQuery] fails here rather than quietly passing.
func (r *roundTrip) askErr(kv map[string]any) error {
	r.t.Helper()
	q, err := worklet.ParseQuery(worklet.MapParams(kv), wednesday, berlin)
	if err != nil {
		r.t.Fatalf("ParseQuery: %v", err)
	}
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	_, err = r.reader.Tasks(r.t.Context(), q, wednesday)
	return err
}

func (r *roundTrip) ask(kv map[string]any) worklet.Answer {
	r.t.Helper()
	q, err := worklet.ParseQuery(worklet.MapParams(kv), wednesday, berlin)
	if err != nil {
		r.t.Fatalf("ParseQuery: %v", err)
	}
	// A HARNESS IS A SURFACE TOO: it resolves an absent level to its own
	// default, as the API and the seat tools do.
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Tasks(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Tasks: %v", err)
	}
	return answer
}

// Voided voids nothing: this harness applies by hand and never re-anchors.
func (w *testWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

// testWaiter is this node's own applier as the publisher sees it.
type testWaiter struct {
	mu sync.Mutex
	at statelog.Position

	// advance is the applier, run from inside a wait
	// ([roundTrip.applyWhileWriting]).
	advance func()

	// lagOutcomes keeps the applier out of the wait that RESOLVES an
	// outcome ([roundTrip.lagBehindOwnWrites]).
	lagOutcomes bool
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

// Reached is Committed: this rig applies and moves its position in one step.
func (w *testWaiter) Reached() statelog.Position { return w.Committed() }

// StreamIdentity is always the live stream: this harness never rebuilds its
// log.
func (w *testWaiter) StreamIdentity() error { return nil }
func (w *testWaiter) Truncated() error      { return nil }

func (w *testWaiter) WaitReached(ctx context.Context, p statelog.Position) error {
	for {
		if w.Committed().Packed() >= p.Packed() {
			return nil
		}
		w.mu.Lock()
		advance := w.advance
		w.mu.Unlock()
		if advance == nil {
			// NOTHING CAN EVER ADVANCE THIS ([roundTrip.applyWhileWriting]),
			// so answer now what the whole resolve budget would: the write
			// is pending, and the case drains it afterwards. Waiting it out
			// dominated this package's run time. DeadlineExceeded, so the
			// publisher takes the timeout path it would after the wait.
			//
			// THE CALLER'S OWN ERROR FIRST: a cancelled context is a
			// cancellation, and DeadlineExceeded would hand the publisher
			// the wrong reason.
			if err := ctx.Err(); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		advance()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (w *testWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {
	w.mu.Lock()
	lag := w.lagOutcomes
	w.mu.Unlock()
	if lag && w.Committed().Packed() < p.Packed() {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	return w.WaitReached(ctx, p)
}

// A WRITE REACHES THE BROKER, THE APPLIER, AND THE READER.
func TestATaskWrittenIsATaskRead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	task := newTask("t-1")
	task.Title = "wire the applier"
	result, err := r.writer.CreateTask(t.Context(), "op-create", task, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// PENDING, not applied: the record is on the log and not yet consumed,
	// and "applied" would send a caller to read a row that is not there.
	if result.Outcome != statelog.OutcomePending {
		t.Fatalf("the create's outcome is %q before the applier ran; it is on "+
			"the log and not yet in the rows", result.Outcome)
	}
	if result.Position.Seq == 0 {
		t.Fatal("a committed write reported no position")
	}

	r.drain()
	answer := r.ask(map[string]any{"container": "project:ENG"})
	if len(answer.Rows) != 1 || answer.Rows[0].Title != "wire the applier" {
		t.Fatalf("the reader answers %+v after the applier consumed the "+
			"create", answer.Rows)
	}
	if answer.Rows[0].Version != uint64(result.Position.Packed()) {
		t.Errorf("the row's version is %d and the record landed at %d — the "+
			"version IS the composed position, so a caller comparing the two "+
			"must find them equal",
			answer.Rows[0].Version, result.Position.Packed())
	}

	// AND A SECOND WRITE ARBITRATES AGAINST THE FIRST.
	if _, err := r.writer.UpdateTask(t.Context(), "op-patch", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Title: ptr("wired")}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()
	answer = r.ask(map[string]any{"container": "project:ENG"})
	if len(answer.Rows) != 1 || answer.Rows[0].Title != "wired" {
		t.Fatalf("the patch did not reach the rows: %+v", answer.Rows)
	}
}

// A SNAPSHOT CARRIES THE CHECKPOINT ITS ROWS ARE AT, and nothing later.
//
// The expectation-zero fence compares it against the trim floor, a claim about
// the rows the decision read, so it comes from the snapshot's own transaction:
// an unapplied record on the log must not move it, and applying it must.
func TestASnapshotCarriesTheCheckpointItsRowsAreAt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	rows, err := worklet.NewRows(r.db.Reader(), worklet.Domain{}.Stream())
	if err != nil {
		t.Fatalf("NewRows: %v", err)
	}
	checkpoint := func() statelog.Position {
		t.Helper()
		snap, err := rows.Snapshot(t.Context(), statelog.Subject{
			Kind: string(worklet.KindTask), ID: "t-1",
		}, statelog.ScopeSet{Paths: []string{"task:t-1"}}, "op-probe",
			func(*sql.Tx, statelog.OpEntry) error { return nil },
			func(*sql.Tx, statelog.Position) (statelog.Decision, error) {
				return statelog.Decision{Payload: []byte("{}")}, nil
			})
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		return snap.Checkpoint
	}
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: worklet.Domain{}.Stream().Name, Seq: seq}
	}

	if got, want := checkpoint(), at(r.consumed); got != want {
		t.Fatalf("a snapshot over rows applied through %d carries %s, want %s",
			r.consumed, got, want)
	}
	applied := r.consumed
	if _, err := r.writer.CreateTask(t.Context(), "op-create", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got, want := checkpoint(), at(applied); got != want {
		t.Fatalf("a record on the log this node has not applied moved the "+
			"snapshot's checkpoint to %s, want %s — the rows have not changed", got, want)
	}
	r.drain()
	if r.consumed == applied {
		t.Fatal("the create never reached the applier, so this case shows nothing")
	}
	if got, want := checkpoint(), at(r.consumed); got != want {
		t.Fatalf("after applying through %d a snapshot carries %s, want %s",
			r.consumed, got, want)
	}
}

// A CREATE WHOSE TASK IS ALREADY FILED IS ANSWERED FROM THE GUARDING ROW.
//
// The task's id derives from the create's operation, so a create meeting a
// task under it is that create made before (a rebased re-run the ledger does
// not hold). It answers with that task and its key, minting and publishing
// nothing. Decided from THIS NODE'S ROW, not the broker: a trim removes both
// the record and the broker's claim on the subject.
func TestACreateWhoseTaskIsFiledIsAnsweredFromTheGuardingRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	first, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	end := r.logEnd(t)
	again, err := r.writer.CreateTask(t.Context(), "op-2", newTask("t-1"), nil)
	if err != nil || again.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the create made again = (%s, %v), want applied", again.Outcome, err)
	}
	if again.Key != first.Key || again.Key == "" {
		t.Errorf("the create made again answered key %q, want the task's own %q",
			again.Key, first.Key)
	}
	if now := r.logEnd(t); now != end {
		t.Errorf("the create made again published %d record(s) — a number "+
			"minted, or a second task", now-end)
	}
}

// AND WHERE THE TASK IS FILED BETWEEN THE MINT AND THE TASK'S APPEND, the task
// step answers the same way: the number this attempt took is a gap.
func TestATaskFiledAfterItsMintIsAnsweredFromItsRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	first, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	got, err := r.writer.FileTaskAt(t.Context(), "op-2", newTask("t-1"), 99)
	if err != nil || got.Outcome != statelog.OutcomeApplied || got.Key != first.Key {
		t.Fatalf("the task step over a filed task = (%s key %q, %v), want applied "+
			"under %q", got.Outcome, got.Key, err, first.Key)
	}
}

// A RANK MOVE ARBITRATES ON THE ORDER, NOT ON THE TASKS: two people dragging
// two cards in one project edit the same object, and arbitrating per card would
// land both and leave an order neither intended.
func TestARankMoveArbitratesOnTheOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		// DRAINED BETWEEN THE TWO: both mint from one counter, and an
		// undrained second create is refused `behind`
		// (TestASecondWriteBehindIsRefusedRatherThanWaitingForEver).
		r.drain()
	}

	moved, err := r.writer.MoveTasks(t.Context(), "op-move", "ENG",
		[]worklet.Placement{{Task: "t-2", Rank: "a1"}})
	if err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()

	// THE ORDER'S ROW CARRIES THE VERSION and the moved task
	// `scoped_through`, keeping the task's broker expectation on its own
	// subject's last message.
	var version, scoped int64
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT version FROM tracker_rank_orders WHERE project_key = 'ENG'`).
			Scan(&version); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT scoped_through FROM tracker_tasks WHERE id = 't-2'`).Scan(&scoped)
	}); err != nil {
		t.Fatalf("read what the move wrote: %v", err)
	}
	if version != moved.Position.Packed() {
		t.Errorf("the order's version is %d and the record landed at %d",
			version, moved.Position.Packed())
	}
	if scoped != moved.Position.Packed() {
		t.Errorf("the moved task's scoped_through is %d, want the move's own "+
			"position %d — stamping its `version` instead would make its next "+
			"write form an expectation the broker refuses for ever",
			scoped, moved.Position.Packed())
	}
}

// A RANK MOVE SURVIVES THE MOVED TASK'S NEXT EDIT: a task's record merges from
// its stored DOCUMENT, so a move writing only the rank column would be undone
// by the next edit, and a detail read would show the old rank.
func TestARankMoveSurvivesTheMovedTasksNextEdit(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	created := oneTask(t, r, "t-2").Rank
	moved, err := worklet.KeyBetween(oneTask(t, r, "t-1").Rank, created)
	if err != nil || moved == created {
		t.Fatalf("a key between the two cards: %q, %v", moved, err)
	}
	if _, err := r.writer.MoveTasks(t.Context(), "op-move", "ENG",
		[]worklet.Placement{{Task: "t-2", Rank: moved}}); err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()
	if held := oneTask(t, r, "t-2"); held.Rank != moved {
		t.Errorf("the moved task's document says rank %q, want %q", held.Rank, moved)
	}

	title := "edited after the move"
	if _, err := r.writer.UpdateTask(t.Context(), "op-edit", "t-2", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()
	if got := r.strings(`SELECT rank FROM tracker_tasks WHERE id = 't-2'`); len(got) != 1 ||
		got[0] != string(moved) {
		t.Errorf("the moved task's rank is %v after its next edit, want %q — the "+
			"edit put back the rank the move replaced", got, moved)
	}
	if held := oneTask(t, r, "t-2"); held.Rank != moved || held.Title != title {
		t.Errorf("after the edit the document says rank %q and title %q, want %q "+
			"and %q", held.Rank, held.Title, moved, title)
	}
}

// A PLACEMENT FOR A TASK IN ANOTHER PROJECT IS NOT WRITTEN: a placement is a
// position in the ONE project the record's scope names, and writing elsewhere
// is outside that scope, which a deferral on the other project would not hold
// back.
func TestAPlacementForATaskInAnotherProjectIsNotWritten(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-t-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})
	before := oneTask(t, r, "t-1").Rank
	elsewhere, err := worklet.KeyBetween(before, "")
	if err != nil || elsewhere == before {
		t.Fatalf("a key other than the task's own: %q, %v", elsewhere, err)
	}
	if _, err := r.writer.MoveTasks(t.Context(), "op-move", "OPS",
		[]worklet.Placement{{Task: "t-1", Rank: elsewhere}}); err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()
	if got := r.strings(`SELECT rank FROM tracker_tasks WHERE id = 't-1'`); len(got) != 1 ||
		got[0] != string(before) {
		t.Errorf("a task in ENG took rank %v from OPS's order, want its own %q", got, before)
	}
	if held := oneTask(t, r, "t-1"); held.Rank != before {
		t.Errorf("a task in ENG's document took rank %q from OPS's order", held.Rank)
	}
}

// A WRITE AGAINST A SUBJECT THIS NODE IS BEHIND ON IS REFUSED, NOT WAITED OUT.
//
// Re-deciding after a rejected append needs the subject's true last position,
// and a lagging applier is unbounded: on the caller's context alone a request
// with no deadline would wait for ever. Two back-to-back creates mint from one
// counter, so the second must be refused under the write path's budget,
// naming the position a caller can retry against.
func TestASecondWriteBehindIsRefusedRatherThanWaitingForEver(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// NO DRAIN: the first create's counter record is on the log, unapplied.
	started := time.Now()
	_, err := r.writer.CreateTask(t.Context(), "op-2", newTask("t-2"), nil)
	if err == nil {
		t.Fatal("a create against a counter this node has not caught up on " +
			"was accepted, so it decided from a state below its own peer's write")
	}
	var unavailable *statelog.Unavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("the refusal is %v, which is not the typed one a caller reads "+
			"a retry position out of", err)
	}
	if unavailable.Reason != statelog.ReasonBehind {
		t.Errorf("the refusal's reason is %q and the caller's remedy depends "+
			"on it being %q", unavailable.Reason, statelog.ReasonBehind)
	}
	if unavailable.Position.Seq == 0 {
		t.Error("the refusal names no position, so the caller has nothing to " +
			"wait for and nothing to retry against")
	}
	if waited := time.Since(started); waited > 30*time.Second {
		t.Fatalf("the write waited %s before refusing — the wait is bounded by "+
			"the write path's own budget, and an unbounded one blocks the "+
			"caller for as long as this node stays behind", waited)
	}

	// AND IT SUCCEEDS ONCE THE APPLIER CATCHES UP: a retry, not a failure.
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-2", newTask("t-2"), nil); err != nil {
		t.Fatalf("the same create after the drain: %v", err)
	}
}

// testSigner is this suite's record signer: a write authority without one is
// refused at construction. The key is a fixture; the signature's rules are
// certified in [statelog].
func testSigner(t *testing.T, d statelog.Domain) *recordseal.Signer {
	t.Helper()
	signer, err := recordseal.NewSigner(d.Name(), recordseal.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

// testVerifier opens what [testSigner] sealed.
func testVerifier(t *testing.T, d statelog.Domain) *recordseal.Verifier {
	t.Helper()
	verifier, err := recordseal.NewVerifier(d.Name(), recordseal.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}
