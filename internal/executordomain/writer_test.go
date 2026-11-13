package executordomain_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WRITE PATH WHOLE: a write reaches a real embedded broker and the shipped
// applier, decided by the shipped writer over a node's own rows, with nothing
// between them mocked — and two nodes over one log where a case needs a node
// that has not applied what the other wrote.

// writerClock is every writer's clock: what a capture's object age is judged on.
var writerClock = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// openExecutorLog stands up a broker and the executor log on it.
func openExecutorLog(t *testing.T) *js.DomainLog {
	t.Helper()
	// THE BROKER DECLARES ITS SIZE, for brokertest's reason: left to derive
	// its cap from the runner's free space it takes three quarters of it.
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
	spec := executordomain.Domain{}.Stream()
	// A QUARTER OF THE DECLARED CEILING: the broker reserves a stream's whole
	// ceiling on disk the moment it creates it.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: executordomain.ExecutorLogMaxBytes / 4,
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

// openNodeStore opens one node's replicated estate, closed with the test.
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

// writeNode is one data node: its own store, publisher and writer over a log
// it may share, applying only when its own write waits on it — so a node that
// has not written since another did has not applied what that one wrote.
type writeNode struct {
	t        *testing.T
	db       store.ReplicatedHandle
	log      *js.DomainLog
	writer   *executordomain.Writer
	applier  *executordomain.Applier
	waiter   *nodeWaiter
	verifier *recordseal.Verifier

	mu       sync.Mutex
	consumed uint64
}

// newWriteNode is a node named nodeID over log and a store of its own.
func newWriteNode(t *testing.T, log *js.DomainLog, nodeID string) *writeNode {
	t.Helper()
	db := openNodeStore(t, nodeID+".db")
	d := executordomain.Domain{}
	spec := d.Stream()
	rows, err := executordomain.NewRows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := executordomain.NewFence(db.Reader(), nodeID)
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
	signer, err := recordseal.NewSigner(d.Name(), recordseal.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	verifier, err := recordseal.NewVerifier(d.Name(), recordseal.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: d, Spec: spec, Log: log,
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Rows:    rows, Fence: fence, Signer: signer,
		Gates: executordomain.NewGates(db.Reader()), Waiter: waiter, Voids: waiter,
		Identity: waiter, NodeID: nodeID, Admission: reserve,
		Generation: func() uint32 { return 0 }, ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	writer, err := executordomain.NewWriter(executordomain.WriterDeps{
		Publisher: publisher, DB: db.Reader(), Now: func() time.Time { return writerClock },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	n := &writeNode{t: t, db: db, log: log, writer: writer, waiter: waiter,
		verifier: verifier, applier: executordomain.NewApplier()}
	waiter.advance = n.drain
	return n
}

// drain applies every record the broker holds beyond what this node has
// applied, with the framework's own bookkeeping, as its loop does.
func (n *writeNode) drain() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	last, err := n.log.End(n.t.Context())
	if err != nil {
		n.t.Fatalf("read the log's end: %v", err)
	}
	spec := executordomain.Domain{}.Stream()
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
		n.applyBody(spec, seq, body, storedAt)
	}
}

// applyBody applies one verified record body at seq.
func (n *writeNode) applyBody(spec statelog.StreamSpec, seq uint64, body []byte, storedAt time.Time) {
	n.t.Helper()
	envelope, err := executordomain.Domain{}.Envelope(body)
	if err != nil {
		n.t.Fatalf("decode record %d: %v", seq, err)
	}
	record := statelog.Record{
		Envelope: envelope,
		Position: statelog.Position{Stream: spec.Name, Generation: envelope.Gen, Seq: seq},
		Payload:  body, StoredAt: storedAt,
	}
	wireSubject := spec.SubjectPrefix + "." + envelope.Subject.String()
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
					INSERT INTO executor_ops (op_id, subject, position, applied_at)
					VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
					envelope.OpID, wireSubject, record.Position.Packed(),
					store.EncodeTime(storedAt)); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(n.t.Context(), `
			INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?,?,?)
			ON CONFLICT (stream, subject) DO UPDATE SET anchor = MAX(anchor, excluded.anchor)`,
			record.Position.Stream, wireSubject, record.Position.Packed()); err != nil {
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
	n.applier.Committed(n.t.Context())
	n.consumed = seq
	n.waiter.reach(record.Position)
}

// row is one turn's object and whether it ended, on this node, and false where
// it holds no row.
func (n *writeNode) row(handle, turn string) (object string, ended, found bool) {
	n.t.Helper()
	var (
		key    sql.NullString
		endSeq sql.NullInt64
	)
	err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `
			SELECT object, end_seq FROM executor_sessions
			WHERE agent_handle = ? AND turn_id = ?`, handle, turn).Scan(&key, &endSeq)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, false
	case err != nil:
		n.t.Fatalf("read %s's turn %s: %v", handle, turn, err)
	}
	return key.String, endSeq.Valid, true
}

// reader is the domain's reader over this node's rows, served at the position
// this node has applied through.
func (n *writeNode) reader() *executordomain.Reader {
	n.t.Helper()
	authority, err := statelogtest.LocalReader(executordomain.Domain{}, n.db, n.waiter.Reached())
	if err != nil {
		n.t.Fatalf("build the read authority: %v", err)
	}
	r, err := executordomain.NewReader(executordomain.ReaderOptions{DB: n.db.Reader(), Log: authority})
	if err != nil {
		n.t.Fatalf("build the reader: %v", err)
	}
	return r
}

// session is one turn's row as this node's reader answers it.
func (n *writeNode) session(handle, turn string) executordomain.Session {
	n.t.Helper()
	read, err := n.reader().Session(n.t.Context(), handle, turn,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		n.t.Fatalf("read %s's turn %s: %v", handle, turn, err)
	}
	return read.Session
}

// nodeWaiter is a node's applier as its publisher sees it: it applies while a
// write waits, as a running node's does.
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

func (w *nodeWaiter) WaitReached(ctx context.Context, p statelog.Position) error {
	for {
		if w.Reached().Packed() >= p.Packed() {
			return nil
		}
		w.mu.Lock()
		advance := w.advance
		w.mu.Unlock()
		if advance == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		advance()
		if w.Reached().Packed() >= p.Packed() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (w *nodeWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet, p statelog.Position) error {
	return w.WaitReached(ctx, p)
}

// engineCall is a write as the engine makes one, under a fresh operation.
func engineCall() executordomain.Call {
	return executordomain.Call{By: iam.Actor{Name: "executor", Kind: iam.ActorSystem},
		OpID: statelog.NewOpID(time.Now(), "executor")}
}

// newTurnID is a run id as the engine mints one.
func newTurnID() string { return statelog.NewOpID(time.Now(), "") }

// newCaptureOf is a capture of turn in conversation, of size bytes, under a
// seat budget of budget, naming an object minted at the writers' clock.
func newCaptureOf(seat, conversation, turn string, capture, size, budget int64) executordomain.NewCapture {
	return executordomain.NewCapture{
		Handle: seat, Turn: turn, ConversationKey: conversation,
		Harness: "claude-code", HarnessVersion: "2.1.304", SessionID: "session-" + turn,
		Object: objstore.Object{Key: objstore.KeyAt(writerClock),
			Hash: objstore.HashOf([]byte(turn)), Size: size},
		ContentKey: "k1", ArchiveFormat: 1, LaunchSeq: 1, CaptureSeq: capture,
		Home: "/home/agent", Workspace: "/work", SeatBudget: budget,
	}
}

// A CAPTURE THE APPLY REFUSES ANSWERS UNRECORDED: decided on a node that had
// not applied another conversation's capture of the same seat, which took the
// seat's budget, it passes its decide and lands after that capture, where the
// apply finds no room — and the writer reads the refusal back off the row
// rather than answering the capture recorded because the record applied.
//
// Mutation: answer Recorded from the outcome alone and the refused capture is
// reported as the session the turn's row names.
func TestACaptureTheApplyRefusesAnswersUnrecorded(t *testing.T) {
	t.Parallel()
	log := openExecutorLog(t)
	a, b := newWriteNode(t, log, "node-a"), newWriteNode(t, log, "node-b")
	const budget = 34 << 20
	room, room2 := newTurnID(), newTurnID()
	// B's conversation has a record B has applied, so its next capture there
	// is decided against an anchor B holds.
	first, err := b.writer.Capture(t.Context(), engineCall(),
		newCaptureOf("ana", "chat:b", room, 1, 2<<20, budget))
	if err != nil || !first.Recorded {
		t.Fatalf("the premise: B's first capture answered (%+v, %v)", first, err)
	}
	// A FILLS THE SEAT'S BUDGET from another conversation, to the byte;
	// B has not applied it.
	full, err := a.writer.Capture(t.Context(), engineCall(),
		newCaptureOf("ana", "chat:a", newTurnID(), 1, 32<<20, budget))
	if err != nil || !full.Recorded {
		t.Fatalf("the premise: A's capture answered (%+v, %v)", full, err)
	}
	late, err := b.writer.Capture(t.Context(), engineCall(),
		newCaptureOf("ana", "chat:b", room2, 1, 2<<20, budget))
	if err != nil {
		t.Fatalf("B's capture decided before A's landed: %v", err)
	}
	if late.Outcome != statelog.OutcomeApplied || late.Recorded {
		t.Errorf("B's capture answered (%s, recorded %v), want applied and not recorded: "+
			"the seat's budget was taken before it landed", late.Outcome, late.Recorded)
	}
	if _, _, found := b.row("ana", room2); found {
		t.Error("the capture the budget could not hold left a row")
	}
}

// A SEAT'S REMOVAL IS PUBLISHED WHATEVER THIS NODE HOLDS: decided on a node that
// has not applied the seat's sessions, it is still a record, and it removes in
// log order on every node what any record before it wrote.
//
// Mutation: decide the removal from this node's count again, publishing
// nothing where it holds no row, and the seat's sessions outlive its removal.
func TestASeatsRemovalIsPublishedWhateverThisNodeHolds(t *testing.T) {
	t.Parallel()
	log := openExecutorLog(t)
	a, b := newWriteNode(t, log, "node-a"), newWriteNode(t, log, "node-b")
	turn := newTurnID()
	if _, err := a.writer.Capture(t.Context(), engineCall(),
		newCaptureOf("ana", "chat:a", turn, 1, 1<<20, 64<<20)); err != nil {
		t.Fatal(err)
	}
	res, err := b.writer.DeleteSeat(t.Context(), engineCall(), "ana")
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Position.Seq == 0 {
		t.Fatalf("the removal on a node holding none of the seat's rows answered (%+v, %v), "+
			"want a record applied", res, err)
	}
	if _, _, found := b.row("ana", turn); found {
		t.Error("the seat's session survived its removal on the node that removed it")
	}
	a.drain()
	if _, _, found := a.row("ana", turn); found {
		t.Error("the seat's session survived its removal on the node that captured it")
	}
}

// A RETENTION DELETE IS DECIDED AGAINST THE ROW'S OWN LAST INSTANT: a turn
// touched after the cutoff is refused not due, and one before it goes.
//
// Mutation: drop the decide's expiredIn and the live conversation's turn is
// deleted under it.
func TestARetentionDeleteIsDecidedAgainstTheRowsLastInstant(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "node-a")
	turn := newTurnID()
	if _, err := n.writer.Capture(t.Context(), engineCall(),
		newCaptureOf("ana", "chat:a", turn, 1, 1<<20, 64<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.DeleteExpired(t.Context(), engineCall(), "ana", "chat:a", turn,
		time.Now().Add(-time.Hour)); !errors.Is(err, executordomain.ErrNotDue) {
		t.Errorf("a delete whose cutoff is before the turn's last capture: %v, want ErrNotDue", err)
	}
	if _, err := n.writer.DeleteExpired(t.Context(), engineCall(), "ana", "chat:a", turn,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("a delete past the turn's last capture: %v", err)
	}
	if _, _, found := n.row("ana", turn); found {
		t.Error("the turn past its cutoff kept its row")
	}
}

// A LOST TURN IS ENDED LOST, AND ONLY IF NOTHING TOUCHED IT SINCE: a turn that
// never ended and was last captured before the instant asked is ended with the
// engine's account of a lost end; one captured since is refused not due, and
// one already ended is refused ended.
//
// Mutation: drop the last-instant check from EndLost and a turn captured a
// moment ago is ended under its running executor.
func TestALostTurnIsEndedLostOnlyIfNothingTouchedItSince(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "node-a")
	turn := newTurnID()
	if _, err := n.writer.Capture(t.Context(), engineCall(),
		newCaptureOf("ana", "chat:a", turn, 1, 1<<20, 64<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.EndLost(t.Context(), engineCall(), "ana", "chat:a", turn,
		time.Now().Add(-time.Hour)); !errors.Is(err, executordomain.ErrNotDue) {
		t.Errorf("a lost end of a turn captured since: %v, want ErrNotDue", err)
	}
	if _, err := n.writer.EndLost(t.Context(), engineCall(), "ana", "chat:a", turn,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("a lost end of a turn untouched since: %v", err)
	}
	if object, ended, found := n.row("ana", turn); !found || !ended || object == "" {
		t.Errorf("the lost turn's row is (object %q, ended %v, found %v), want it ended "+
			"with its session kept", object, ended, found)
	}
	if _, err := n.writer.EndLost(t.Context(), engineCall(), "ana", "chat:a", turn,
		time.Now().Add(time.Hour)); !errors.Is(err, executordomain.ErrTurnEnded) {
		t.Errorf("a second lost end: %v, want ErrTurnEnded", err)
	}
}

// A SEAT'S REMOVAL IS FORGOTTEN ONLY PAST ITS CUTOFF: the retention of a
// removal made after the cutoff it was listed under is refused not due.
//
// Mutation: drop the decide's instant check from ExpireRemoval and a removal
// made a moment ago is forgotten while a capture decided before it may still
// land.
func TestASeatsRemovalIsForgottenOnlyPastItsCutoff(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "node-a")
	if _, err := n.writer.DeleteSeat(t.Context(), engineCall(), "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.ExpireRemoval(t.Context(), engineCall(), "ana",
		time.Now().Add(-time.Hour)); !errors.Is(err, executordomain.ErrNotDue) {
		t.Errorf("the retention of a removal made after its cutoff: %v, want ErrNotDue", err)
	}
	if _, err := n.writer.ExpireRemoval(t.Context(), engineCall(), "ana",
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("the retention of a removal past its cutoff: %v", err)
	}
	if _, err := n.writer.ExpireRemoval(t.Context(), engineCall(), "ana",
		time.Now().Add(time.Hour)); !errors.Is(err, executordomain.ErrNotDue) {
		t.Errorf("the retention of a removal already forgotten: %v, want ErrNotDue", err)
	}
}

// A CAPTURE'S CONTEXT AND CUT-OFF REACH THE ROW: the whole input of its
// session's last request and whether its harness was cut off, which only the
// run's record held before and a launch continuing the session after its turn
// ended reads off the row — the newest capture's, through the insert of a
// turn's first capture and the update of a later one; and a context below
// nothing is refused naming the field.
//
// Mutation: leave context_tokens and interrupted off updateCaptureStatement
// and the later capture's account never reaches the row; drop them from the
// payload Writer.Capture builds and neither capture's does.
func TestACapturesContextAndCutOffReachTheRow(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "node-a")
	turn := newTurnID()
	cut := newCaptureOf("ana", "chat:a", turn, 1, 1<<20, 64<<20)
	cut.ContextTokens, cut.Interrupted = 61_000, true
	if res, err := n.writer.Capture(t.Context(), engineCall(), cut); err != nil || !res.Recorded {
		t.Fatalf("the first capture answered (%+v, %v)", res, err)
	}
	if s := n.session("ana", turn); s.ContextTokens != 61_000 || !s.Interrupted {
		t.Errorf("the row says %d tokens, cut off %v, want the first capture's 61000 and cut off",
			s.ContextTokens, s.Interrupted)
	}
	clean := newCaptureOf("ana", "chat:a", turn, 2, 1<<20, 64<<20)
	clean.ContextTokens = 72_000
	if res, err := n.writer.Capture(t.Context(), engineCall(), clean); err != nil || !res.Recorded {
		t.Fatalf("the later capture answered (%+v, %v)", res, err)
	}
	if s := n.session("ana", turn); s.ContextTokens != 72_000 || s.Interrupted {
		t.Errorf("the row says %d tokens, cut off %v, want the newest capture's 72000, whole",
			s.ContextTokens, s.Interrupted)
	}
	below := newCaptureOf("ana", "chat:a", turn, 3, 1<<20, 64<<20)
	below.ContextTokens = -1
	if _, err := n.writer.Capture(t.Context(), engineCall(), below); !errors.Is(err, executordomain.ErrInvalid) {
		t.Errorf("a capture of -1 tokens: %v, want ErrInvalid", err)
	}
}

// A BEGIN SAYS WHETHER THE LOG HOLDS ITS BASE, read back off the row the apply
// wrote: a turn's first read of a head that still stands holds it; the same
// begin asked again under another operation publishes nothing, the first read
// standing, and still says it holds; and a begin whose base another turn's end
// absorbed before it landed records its read, holds nothing, and says why.
//
// Mutation: answer Held from the outcome alone and the begin whose base had
// gone is reported holding it.
func TestABeginSaysWhetherTheLogHoldsItsBase(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "node-a")
	ctx := t.Context()

	base, holder := newTurnID(), newTurnID()
	n.recorded(newCaptureOf("ana", "chat:a", base, 1, 1<<20, 64<<20))
	n.completed("ana", base, "chat:a")
	begin := beginFrom(n.headOf("ana", "chat:a"), "ana", holder, "chat:a")
	first, err := n.writer.Begin(ctx, engineCall(), begin)
	if err != nil || first.Outcome != statelog.OutcomeApplied || first.Position.Seq == 0 || !first.Held {
		t.Fatalf("the begin answered (%+v, %v), want it published and holding its base", first, err)
	}
	again, err := n.writer.Begin(ctx, engineCall(), begin)
	if err != nil || again.Position.Seq != 0 || !again.Held {
		t.Errorf("the begin asked again answered (%+v, %v), want nothing published and the base held",
			again, err)
	}

	moved, sibling, late := newTurnID(), newTurnID(), newTurnID()
	n.recorded(newCaptureOf("ana", "chat:b", moved, 1, 1<<20, 64<<20))
	n.completed("ana", moved, "chat:b")
	read := n.headOf("ana", "chat:b")
	// ANOTHER TURN THAT READ THE SAME HEAD ends first, absorbing it.
	c := newCaptureOf("ana", "chat:b", sibling, 1, 1<<20, 64<<20)
	c.ReadThrough, c.Absorbs = read.Position, read.Absorbs()
	n.recorded(c)
	n.completed("ana", sibling, "chat:b")
	gone, err := n.writer.Begin(ctx, engineCall(), beginFrom(read, "ana", late, "chat:b"))
	if err != nil || gone.Outcome != statelog.OutcomeApplied || gone.Held ||
		!strings.Contains(gone.Why, "gone") {
		t.Errorf("the begin whose base was absorbed first answered (%+v, %v), want it applied, "+
			"holding nothing and saying the base is gone", gone, err)
	}
	if s := n.session("ana", late); s.BaseTurn != "" || s.ReadThrough.Packed() != read.Position.Packed() {
		t.Errorf("the late begin's row names base %q read at %s, want no base and its read at %s",
			s.BaseTurn, s.ReadThrough, read.Position)
	}
}

// A BEGIN THAT HOLDS NOTHING SAYS WHY, read back off the row the apply wrote: a
// turn whose own capture landed before its begin holds no base — its launches
// continue that capture's session — though the begin records its read and
// names the base; and a begin whose base the seat's budget released between
// the read and the record holds nothing and names none.
//
// Mutation: drop beginHolds' landed-capture arm and the late begin, whose row
// names its base, is reported holding it; let a base whose row names no
// session stand (drop baseStands' session arm) and the begin landing after the
// budget released its base names it, and is reported holding it.
func TestABeginThatHoldsNothingSaysWhy(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "node-a")
	ctx := t.Context()

	// A LATE BEGIN: the turn's own first capture, which read nothing, landed
	// first.
	base, late := newTurnID(), newTurnID()
	n.recorded(newCaptureOf("ana", "chat:a", base, 1, 1<<20, 64<<20))
	n.completed("ana", base, "chat:a")
	h := n.headOf("ana", "chat:a")
	n.recorded(newCaptureOf("ana", "chat:a", late, 1, 1<<20, 64<<20))
	got, err := n.writer.Begin(ctx, engineCall(), beginFrom(h, "ana", late, "chat:a"))
	if err != nil || got.Outcome != statelog.OutcomeApplied || got.Held ||
		!strings.Contains(got.Why, "landed a capture") {
		t.Errorf("the begin landing after its turn's first capture answered (%+v, %v), want it "+
			"applied, holding nothing and saying the turn landed a capture first", got, err)
	}
	if s := n.session("ana", late); s.BaseTurn != base || s.ReadThrough.Packed() != h.Position.Packed() {
		t.Errorf("the late begin's row names base %q read at %s, want the base %s read at %s",
			s.BaseTurn, s.ReadThrough, base, h.Position)
	}

	// A BASE THE BUDGET RELEASED between the read and the begin, on a seat of
	// its own so its budget counts this case's sessions alone.
	released, holder := newTurnID(), newTurnID()
	n.recorded(newCaptureOf("ben", "chat:b", released, 1, 1<<20, 64<<20))
	n.completed("ben", released, "chat:b")
	read := n.headOf("ben", "chat:b")
	// ROOM FOR ANOTHER CONVERSATION'S CAPTURE IS THE UNHELD BASE'S: a
	// mebibyte beside a mebibyte under a budget of one and a half.
	n.recorded(newCaptureOf("ben", "chat:c", newTurnID(), 1, 1<<20, 3<<19))
	if object, _, _ := n.row("ben", released); object != "" {
		t.Fatalf("the premise: the budget kept the base's session %s", object)
	}
	gone, err := n.writer.Begin(ctx, engineCall(), beginFrom(read, "ben", holder, "chat:b"))
	if err != nil || gone.Outcome != statelog.OutcomeApplied || gone.Held ||
		!strings.Contains(gone.Why, "no longer names") {
		t.Errorf("the begin whose base the budget released first answered (%+v, %v), want it "+
			"applied, holding nothing and saying the base's row no longer names its session", gone, err)
	}
	if s := n.session("ben", holder); s.BaseTurn != "" || s.ReadThrough.Packed() != read.Position.Packed() {
		t.Errorf("the begin's row names base %q read at %s, want no base and its read at %s",
			s.BaseTurn, s.ReadThrough, read.Position)
	}
}

// A HOLDER WHOSE END WAS LOST IS ENDED LOST, AND ITS HOLD LIFTS. A turn whose
// begin holds a base and which never captured, once the retention duty finds
// no run of it a day past its begin, is listed as a turn whose end may be lost
// and ended `lost` through EndLost's own decide — refused not due before its
// begin's instant — and from then on holds nothing: the base, which the holder
// never continued and so never absorbed, stays its conversation's head, and
// the retention's listing, which left it out while it was held, lists it. The
// lost end's account names no capture, the turn having taken none.
//
// Mutation: refuse in EndLost's decide a turn whose row landed no capture and
// the lost holder is never ended, holding its base until the retention; write
// a begun row with no instant of its own and EndLost never finds it not due;
// word the lost end as a day past the turn's last capture and the account of a
// turn that captured nothing names one.
func TestAHolderWhoseEndWasLostIsEndedLostAndItsHoldLifts(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "node-a")
	ctx := t.Context()
	base, holder := newTurnID(), newTurnID()
	n.recorded(newCaptureOf("ana", "chat:a", base, 1, 1<<20, 64<<20))
	n.completed("ana", base, "chat:a")
	begun, err := n.writer.Begin(ctx, engineCall(),
		beginFrom(n.headOf("ana", "chat:a"), "ana", holder, "chat:a"))
	if err != nil || !begun.Held {
		t.Fatalf("the premise: the begin answered (%+v, %v), want its base held", begun, err)
	}
	later := time.Now().Add(time.Hour)
	listed := func(turn string) bool {
		t.Helper()
		expired, listErr := n.reader().Expired(ctx, later, 10)
		if listErr != nil {
			t.Fatalf("list what is past the retention: %v", listErr)
		}
		return slices.ContainsFunc(expired, func(e executordomain.Expiry) bool { return e.Turn == turn })
	}
	if listed(base) {
		t.Fatal("the premise: the retention lists the base its holder holds")
	}
	lost, err := n.reader().Unended(ctx, later, 10)
	if err != nil || !slices.ContainsFunc(lost, func(e executordomain.Expiry) bool { return e.Turn == holder }) {
		t.Fatalf("the turns whose end may be lost are (%+v, %v), want the holder among them", lost, err)
	}
	if _, err := n.writer.EndLost(ctx, engineCall(), "ana", "chat:a", holder,
		time.Now().Add(-time.Hour)); !errors.Is(err, executordomain.ErrNotDue) {
		t.Errorf("a lost end of the holder before its begin's instant: %v, want ErrNotDue", err)
	}
	if _, err := n.writer.EndLost(ctx, engineCall(), "ana", "chat:a", holder, later); err != nil {
		t.Fatalf("a lost end of the holder past its begin: %v", err)
	}
	s := n.session("ana", holder)
	if !s.Ended() || s.Ending.Outcome != executorkeep.OutcomeLost || s.BaseTurn != base ||
		strings.Contains(s.Ending.Failure, "capture") {
		t.Errorf("the holder's row is %+v, want it ended lost, naming the base it was handed and "+
			"no capture it never took", s)
	}
	if !listed(base) {
		t.Error("the retention still leaves out the base once its holder ended")
	}
	if h := n.headOf("ana", "chat:a"); h.Session.Turn != base {
		t.Errorf("the conversation's head is %s once its holder ended, want the base", h.Session.Turn)
	}
}

// recorded captures c through n's writer, and fails the test unless the turn's
// row was read back naming it.
func (n *writeNode) recorded(c executordomain.NewCapture) {
	n.t.Helper()
	if res, err := n.writer.Capture(n.t.Context(), engineCall(), c); err != nil || !res.Recorded {
		n.t.Fatalf("capture %s answered (%+v, %v)", c.Turn, res, err)
	}
}

// completed ends handle's turn in conversation, completed, through n's writer.
func (n *writeNode) completed(handle, turn, conversation string) {
	n.t.Helper()
	if _, err := n.writer.End(n.t.Context(), engineCall(), executordomain.NewEnd{Handle: handle,
		Turn: turn, ConversationKey: conversation,
		Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}); err != nil {
		n.t.Fatalf("end %s: %v", turn, err)
	}
}

// headOf is handle's head of conversation as this node reads it, failing the
// test where the conversation has none.
func (n *writeNode) headOf(handle, conversation string) executordomain.Head {
	n.t.Helper()
	h, err := n.reader().Head(n.t.Context(), handle, conversation,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || h.Session == nil {
		n.t.Fatalf("read %s's head of %s: (%+v, %v)", handle, conversation, h.Session, err)
	}
	return h
}

// beginFrom is the begin of handle's turn whose first launch read h: the read's
// position, what it handed out, and its head as the base.
func beginFrom(h executordomain.Head, handle, turn, conversation string) executordomain.NewBegin {
	return executordomain.NewBegin{Handle: handle, Turn: turn, ConversationKey: conversation,
		ReadThrough: h.Position, Absorbs: h.Absorbs(), BaseTurn: h.Session.Turn,
		BaseObject: h.Session.Object}
}

// AN END KEEPS ITS WORDS REDACTED BEFORE THEY ARE CUT. What a turn was asked is
// kept to its first kibibyte and an ending's accounts to theirs, and a
// credential that bound runs across is a marker before the cut — never the
// head of a token no later redaction recognises, which every reader of the row
// was shown through its redaction, a seat's model among them.
//
// Mutation: keep the ask's first kibibyte as it was asked, and its row holds
// the token's head.
func TestAnEndKeepsItsWordsRedactedBeforeTheyAreCut(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "n1")
	const token = "ghp_Q7wErTy9UiOp3AsDfGh5JkLzXcVbNm1PqRsT"
	across := strings.Repeat("a", executordomain.AskExcerptMaxBytes-16) + token + " and the rest"
	turn := newTurnID()
	if _, err := n.writer.End(t.Context(), engineCall(), executordomain.NewEnd{Handle: "ana", Turn: turn,
		ConversationKey: "chat:a", Ask: across, Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
			Failure: strings.Repeat("f", executorkeep.FailureMaxBytes-16) + token}}); err != nil {
		t.Fatalf("end: %v", err)
	}
	row := n.session("ana", turn)
	if strings.Contains(row.AskExcerpt, "ghp_") || len(row.AskExcerpt) > executordomain.AskExcerptMaxBytes ||
		!strings.HasSuffix(row.AskExcerpt, "…") {
		t.Errorf("the row keeps the ask as %q, want it cut before the token its bound ran across",
			row.AskExcerpt[max(len(row.AskExcerpt)-40, 0):])
	}
	if f := row.Ending.Failure; strings.Contains(f, "ghp_") {
		t.Errorf("the row keeps the failure as %q, want no head of the token", f[max(len(f)-40, 0):])
	}
}
