package tracker_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE ROUND TRIP: a write reaches the broker, the applier consumes it, and the
// reader answers from the rows it wrote.
//
// Every layer below has its own tests over fakes, and every one of them can be
// individually right while the composition is wrong: a subject the publisher
// builds and the applier's dispatch does not recognise, a scope the writer
// resolves one way and the probe another, an envelope the framework fills and
// the domain reads back empty. None of those is visible without a real broker
// and a real store on both ends.
type roundTrip struct {
	t        *testing.T
	db       *store.DB
	log      *js.DomainLog
	writer   *tracker.Writer
	applier  *tracker.Applier
	reader   *tracker.Reader
	waiter   *testWaiter
	consumed uint64
}

func newRoundTrip(t *testing.T) *roundTrip {
	t.Helper()
	r := newRoundTripWithoutProject(t)
	// THE PROJECT FIRST, because a create is a SEQUENCE: it takes a key
	// from that project's counter before it writes a task, and a project
	// this node has not applied is one whose counter it cannot mint from.
	// Seeding it here rather than in each case is what keeps the cases
	// about what they are named for.
	if _, err := r.writer.WriteDocument(t.Context(), "op-project",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, nil); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	r.drain()
	return r
}

// newRoundTripWithoutProject is the same harness with NO project seeded,
// which is the state a company is actually in the moment it boots. The chart
// apply is what leaves it, and its own cases need to see the before.
func newRoundTripWithoutProject(t *testing.T) *roundTrip {
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
	spec := tracker.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD THIS HARNESS OVERRIDES, and it is not a
	// property under test: the shipped default is sized for five years of a
	// real company's growth, and an embedded broker in a temporary
	// directory refuses to reserve it. Everything the domain declares
	// besides the ceiling is the shipped value.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: 16 << 20,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})

	rows, err := tracker.NewRows(db)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := tracker.NewFence(db, "node-a")
	// The published trim floor is zero on a fleet that has never trimmed,
	// which is the state every new company is in — and the state in which
	// an absent anchor really does mean an empty subject.
	fence.Floor = func(context.Context) (uint64, error) { return 0, nil }
	waiter := &testWaiter{}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: tracker.Domain{}, Log: log, Rows: rows, Fence: fence,
		Gates: tracker.NewGates(db), Waiter: waiter, NodeID: "node-a",
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	writer, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: publisher, DB: db, NodeID: "node-a",
		Actor: "ana", ActorKind: tracker.AuthorHuman,
		Now: func() time.Time { return wednesday },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	// THE READ AUTHORITY IS LOCAL HERE, because this harness drives the
	// applier itself rather than running a framework loop: what it is
	// about is one record's journey from writer to row, and the barrier
	// belongs to the cases that have a quorum to commit against.
	logReader, err := statelogtest.LocalReader(tracker.Domain{}, db.Replicated(),
		statelog.Position{Stream: tracker.Domain{}.Stream().Name, Generation: 1})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(db, logReader)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	r := &roundTrip{
		t: t, db: db, log: log, writer: writer,
		applier: tracker.NewApplier("node-a"),
		reader:  reader, waiter: waiter,
	}
	return r
}

// drain consumes every record the broker holds beyond what this node has
// applied, exactly as the framework's own loop does — one transaction per
// record, carrying the rows and the checkpoint together.
func (r *roundTrip) drain() {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	r.apply(r.consumed+1, last)
}

// redeliver re-applies one record the applier has already consumed, which is
// what a redelivery, a reprocess after an upgrade and a snapshot adopter's
// replay all look like from here.
func (r *roundTrip) redeliver(seq uint64) {
	r.t.Helper()
	consumed := r.consumed
	r.apply(seq, seq)
	r.consumed = consumed
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
		env, err := tracker.DecodeEnvelope(payload)
		if err != nil {
			r.t.Fatalf("decode record %d: %v", seq, err)
		}
		_ = subject
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
				Stream: tracker.Domain{}.Stream().Name, Generation: env.Gen, Seq: seq,
			},
			Payload:  payload,
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
			// THE ROWS, THE OPERATION ID AND THE ANCHOR IN ONE
			// TRANSACTION, exactly as the framework commits them:
			// a snapshot that saw the rows without the anchor
			// would pair an old decision with a new expectation.
			if _, err := r.applier.Apply(r.t.Context(), tx, record,
				statelog.ApplyOptions{Now: wednesday, StoredAt: storedAt}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO tracker_ops (op_id, subject, position, applied_at)
				VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
				env.OpID, env.Subject.String(), record.Position.Packed(),
				store.EncodeTime(storedAt)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO statelog_anchor (stream, subject, anchor)
				VALUES (?,?,?)
				ON CONFLICT (stream, subject) DO UPDATE SET
					anchor = MAX(anchor, excluded.anchor)`,
				record.Position.Stream,
				tracker.Domain{}.Stream().SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed()); err != nil {
				return err
			}
			// AND THE CHECKPOINT, in the same transaction, which is the
			// contract this harness claims to be exercising. Without it
			// every read answers position zero — so a case asserting how
			// far behind an answer may be would be asserting against a
			// number the harness never wrote.
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
		r.consumed = seq
		r.waiter.reach(record.Position)
	}
}

// scopeOfLastRecord is the resolved scope of the newest record on the log.
//
// FROM THE WIRE, never from the writer's own value: what a deferral is filed
// under is what the RECORD carries, and a case reading the writer's struct
// would pass for a scope that never reached the broker.
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
	env, err := tracker.DecodeEnvelope(payload)
	if err != nil {
		r.t.Fatalf("decode record %d: %v", last, err)
	}
	return env.Scope.Resolve(env.Subject)
}

func (r *roundTrip) ask(kv map[string]any) tracker.Answer {
	r.t.Helper()
	q, err := tracker.ParseQuery(tracker.MapParams(kv), wednesday, berlin)
	if err != nil {
		r.t.Fatalf("ParseQuery: %v", err)
	}
	// A HARNESS IS A SURFACE TOO, and an absent level resolves to the
	// surface's own default rather than to a fourth state — so this one
	// names its choice exactly as the API and the seat tools do.
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Tasks(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Tasks: %v", err)
	}
	return answer
}

// testWaiter is this node's own applier as the publisher sees it.
type testWaiter struct {
	mu sync.Mutex
	at statelog.Position
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

func (w *testWaiter) WaitCommitted(ctx context.Context, p statelog.Position) error {
	for {
		if w.Committed().Packed() >= p.Packed() {
			return nil
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
	// PENDING, not applied: the record is committed on the log and this
	// node has not consumed it yet. Reporting it as applied is how a
	// caller reads a row that is not there.
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
	if _, err := r.writer.UpdateTask(t.Context(), "op-patch", "t-1", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Title: ptr("wired")}, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()
	answer = r.ask(map[string]any{"container": "project:ENG"})
	if len(answer.Rows) != 1 || answer.Rows[0].Title != "wired" {
		t.Fatalf("the patch did not reach the rows: %+v", answer.Rows)
	}
}

// A CREATE FOR A NAME ALREADY TAKEN IS REFUSED FROM THE GUARDING ROW.
//
// Established from THIS NODE'S OWN ROW rather than from the broker, because a
// task below the trim floor has no record left on the log to prove it existed
// — and the broker's own claim on that subject is exactly what a trim removes.
func TestASecondCreateIsRefusedFromTheGuardingRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	_, err := r.writer.CreateTask(t.Context(), "op-2", newTask("t-1"), nil)
	if err == nil {
		t.Fatal("a second create for the same task was accepted")
	}
	if !errors.Is(err, statelog.ErrExists) {
		t.Fatalf("the refusal is %v, not the one a caller handles", err)
	}
}

// A RANK MOVE ARBITRATES ON THE ORDER, NOT ON THE TASKS.
//
// Two people dragging two different cards in one project are editing the same
// object, and arbitrating on either card would let both writes land and leave
// the order neither of them intended.
func TestARankMoveArbitratesOnTheOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		// DRAINED BETWEEN THE TWO, because both mint from the same
		// counter and the second cannot decide against a number this
		// node has not applied. A create that did not drain is refused
		// `behind` naming that counter, which is the case
		// TestASecondWriteBehindIsRefusedRatherThanWaitingForEver
		// covers.
		r.drain()
	}

	moved, err := r.writer.MoveTasks(t.Context(), "op-move", "ENG",
		[]tracker.Placement{{Task: "t-2", Rank: "a1"}})
	if err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()

	// THE ORDER'S OWN ROW CARRIES THE VERSION and the moved task carries
	// `scoped_through` — which is what keeps the task's own broker
	// expectation matching its subject's last message rather than a
	// record published on another.
	var version, scoped int64
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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

// A WRITE AGAINST A SUBJECT THIS NODE IS BEHIND ON IS REFUSED, NOT WAITED OUT.
//
// # The failure this exists to catch
//
// A rejected append means a peer wrote in this generation and this node has not
// applied it, so re-deciding needs the subject's true last position. The wait
// for it used to run on the caller's own context with no budget of its own —
// and the state producing it is an applier that has not caught up, which is
// unbounded by construction. A request with no deadline waited FOR EVER, and
// one with a deadline got a cancellation where it needed the reason.
//
// Two creates back to back mint from one counter, so the second is exactly that
// case. It must come back refused, under a budget, naming the position it was
// waiting for — because that number is what turns "a colleague is editing this"
// into something a caller can retry against.
func TestASecondWriteBehindIsRefusedRatherThanWaitingForEver(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// NO DRAIN: this node holds the first create's counter record on the
	// log and has not applied it.
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

	// AND IT SUCCEEDS ONCE THE APPLIER CATCHES UP, which is what makes the
	// refusal a retry rather than a failure.
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-2", newTask("t-2"), nil); err != nil {
		t.Fatalf("the same create after the drain: %v", err)
	}
}
