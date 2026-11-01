package code_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WHOLE PATH: a write reaches the broker, the applier and the reader.
//
// The code domain's write path is a publisher and its read path is a set of
// applied rows, and nothing between them is mocked here — a real embedded
// broker, a real store, the shipped domain and the shipped applier. Every
// property the cases built on this assert is a claim about the three of them
// agreeing, which no unit test can reach.

// harnessAt is the store's clock: every authored instant the harness's writes
// carry.
var harnessAt = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

// asker is who every read a case makes asks as, where the case is not about
// who asks: a person, bound to their human seat, whom no chart and no
// visibility narrows (ADR-0058) — so no case's own check of the rows is
// filtered by the grant it is not about.
var asker = code.Principal{Kind: code.PrincipalOperator, OperatorID: "founder-token", Seat: "founder"}

// testRing is the one keyring every node these cases build signs its records
// and opens its peers' under: one fleet, one keyring, as every state log's
// records are signed (ADR-0031). The key is a fixture — what the cases are
// about is what the domain writes, and the signature's own rules are
// certified in internal/statelog.
var testRing = statelog.OneKey("k1", "the code cases' fleet key material")

// testSigner is the code log's signer under [testRing].
func testSigner(t testing.TB) *statelog.Signer {
	t.Helper()
	signer, err := statelog.NewSigner(code.Domain{}.Name(), testRing)
	if err != nil {
		t.Fatalf("build the record signer: %v", err)
	}
	return signer
}

// testVerifier opens what [testSigner] sealed.
func testVerifier(t testing.TB) *statelog.Verifier {
	t.Helper()
	verifier, err := statelog.NewVerifier(code.Domain{}.Name(), testRing)
	if err != nil {
		t.Fatalf("build the record verifier: %v", err)
	}
	return verifier
}

// node is one data node on the code log: its own estate, applier, publisher,
// lifecycle store and reader.
type node struct {
	t   testing.TB
	id  string
	db  store.ReplicatedHandle
	log *js.DomainLog
	// records is the log read back as BODIES, each signed frame opened under
	// [testRing] — what the framework's loop decodes, and what a case that
	// reads a record off the log decodes too: the raw log answers frames.
	records statelog.VerifiedLog
	applier *code.Applier
	pub     *statelog.Publisher
	store   *code.Store
	reader  *code.Reader
	waiter  *testWaiter
	// authority is the code log's read authority the reader reads through:
	// a case builds a reader of its own over it.
	authority *statelog.Reader

	// chart is the roster the reader judges a seat by — set by a case about
	// grants before it reads, and nil otherwise: no chart, as a reader built
	// without one has. The lifecycle store decides by it too: with none, it
	// decides no lifecycle request, as a node with no epoch installed.
	chart code.Roster

	// scratch is the lifecycle policy the store holds a create to — the
	// defaults a company that sets no code.scratch runs under, unless a case
	// sets another before it asks.
	scratch code.LifecyclePolicy

	// behind marks the node confirmed behind the company's configuration:
	// its lifecycle store decides no request while it is set.
	behind atomic.Bool

	// readAt is the instant the node's reader reads at, where a test set
	// one; harnessAt otherwise.
	readAt atomic.Pointer[time.Time]

	// mu SERIALISES the drain: a write's wait drains on the writing
	// goroutine, and two writes waiting at once — two repositories
	// published concurrently — would otherwise apply one record twice.
	mu       sync.Mutex
	consumed uint64

	// storedAt, when set, is the broker instant the drain hands the applier
	// for a record instead of the broker's own — how a case ages rows past
	// a horizon without waiting out a day.
	storedAt func(seq uint64, broker time.Time) time.Time

	// applied, when set, is told the rows each record's apply reported —
	// what the framework holds a record's transaction to.
	applied func(seq uint64, rows int)
}

// harnessRoom is what a case may write to the code log with ordinary records:
// the 64 MiB the harness's ceiling was before a gate reserve shared it. The
// page-budget case — twenty pushes of the longest refs — writes 7.6 MB
// (measured), past the 7 MiB a 64 MiB ceiling leaves beside the reserve.
const harnessRoom = 64 << 20

// harnessLogBytes is the smallest ceiling that leaves ordinary records room
// bytes beside the code log's GATE RESERVE (statelog.ReservationOf): the code
// log claims identity, so the top of its ceiling — the larger of a sixteenth
// of it and seven of its largest appends and a mebibyte, 57 MiB for a log
// whose largest append is 8 MiB — is kept for the records that install or
// lift a gate, and ordinary writes are refused `log_full` short of it. The
// fixed point of ceiling = room + reserve(ceiling), decided by the reserve's
// own arithmetic rather than a figure written here.
func harnessLogBytes(room uint64) uint64 {
	r := statelog.ReservationOf(code.Domain{})
	ceiling := room
	for r.Ordinary(ceiling) < room {
		ceiling = room + r.Bytes(ceiling)
	}
	return ceiling
}

// openLog provisions the code log on an embedded broker and opens it.
func openLog(t testing.TB) *js.DomainLog {
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
	spec := code.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD OVERRIDDEN, and it is not a property
	// under test: an embedded broker in a temporary directory refuses to
	// reserve the shipped one.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: int64(harnessLogBytes(harnessRoom)),
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

// newNode is one node on the code log, its applier running inside every wait.
func newNode(t testing.TB) *node {
	t.Helper()
	return nodeOn(t, openLog(t), "node-a")
}

// newTwoNodes is two nodes on ONE log, each with its own estate and applier —
// the shape a race between two writers, or a read on a node behind another,
// needs.
func newTwoNodes(t testing.TB) (*node, *node) {
	t.Helper()
	log := openLog(t)
	return nodeOn(t, log, "node-a"), nodeOn(t, log, "node-b")
}

// nodeOn builds one node over a log it is handed.
func nodeOn(t testing.TB, log *js.DomainLog, id string) *node {
	t.Helper()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), id+".db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	rows, err := code.NewRows(db.Reader(), code.Domain{}.Stream())
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	waiter := &testWaiter{}
	fence := code.NewFence(db.Reader(), id)
	// A FLEET THAT HAS NEVER TRIMMED, whose log's own first sequence is the
	// fence's other bound and its last the check this node is on it.
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	fence.Committed = waiter.Committed
	reserve, err := statelog.NewReserve(code.Domain{}.Stream(),
		func(ctx context.Context) (statelog.Usage, error) {
			stats, err := log.Stats(ctx)
			return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
		})
	if err != nil {
		t.Fatalf("build the gate reserve: %v", err)
	}
	records := statelog.VerifiedLog{Log: log, Verifier: testVerifier(t)}
	pub, err := statelog.NewPublisher(statelog.Deps{
		Domain: code.Domain{}, Spec: code.Domain{}.Stream(), Log: log,
		Signer: testSigner(t), Records: records,
		Rows: rows, Fence: fence, Gates: code.NewGates(db.Reader()),
		Waiter: waiter, Voids: waiter, Identity: waiter, NodeID: id,
		Admission: reserve, Generation: func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	// THROUGH THE FRAMEWORK'S READ AUTHORITY, like production, over this
	// node's own waiter: a level asked for is a level served.
	authority, err := statelogtest.LocalReaderOver(code.Domain{}, db, waiter)
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	n := &node{
		t: t, id: id, db: db, log: log, records: records, applier: code.NewApplier(nil), pub: pub,
		waiter: waiter, authority: authority, scratch: code.ScratchOf(config.Code{}),
	}
	// THE LIFECYCLE STORE, deciding a tool's request by this node's chart,
	// its scratch policy and whether it is behind — each read per request,
	// as production reads the current epoch's.
	n.store, err = code.NewStore(code.StoreOptions{
		Publisher: pub, DB: db.Reader(), NodeID: id,
		Now: func() time.Time { return harnessAt },
		SeatRules: func() (code.SeatRules, bool) {
			if n.chart == nil {
				return code.SeatRules{}, false
			}
			return code.SeatRules{Roster: n.chart}, true
		},
		Current: func() bool { return !n.behind.Load() },
		Policy:  func() code.LifecyclePolicy { return n.scratch },
	})
	if err != nil {
		t.Fatalf("build the store: %v", err)
	}
	readAt := func() time.Time {
		if at := n.readAt.Load(); at != nil {
			return *at
		}
		return harnessAt
	}
	reader, err := code.NewReader(code.ReaderOptions{Log: authority, Now: readAt,
		Roster: func() code.Roster { return n.chart }})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	n.reader = reader
	waiter.advance = n.drain
	return n
}

// hold keeps this node's applier out of every wait: a read past its position
// refuses `behind`, and a write it publishes resolves `pending`.
func (n *node) hold() {
	n.waiter.mu.Lock()
	defer n.waiter.mu.Unlock()
	n.waiter.advance = nil
}

// release puts this node's applier back in every wait, and brings it level.
func (n *node) release() {
	n.waiter.mu.Lock()
	n.waiter.advance = n.drain
	n.waiter.mu.Unlock()
	n.drain()
}

// drain consumes every record the broker holds beyond what this node has
// applied, exactly as the framework's own loop does — one transaction per
// record, carrying the rows, the operation id, the anchor and the checkpoint
// together.
func (n *node) drain() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	ctx := context.WithoutCancel(n.t.Context())
	last, err := n.log.End(ctx)
	if err != nil {
		n.t.Errorf("read the log's end: %v", err)
		return
	}
	spec := code.Domain{}.Stream()
	for seq := n.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := n.records.At(ctx, seq)
		if err != nil {
			n.t.Errorf("read record %d: %v", seq, err)
			return
		}
		if !ok {
			n.consumed = seq
			continue
		}
		if n.storedAt != nil {
			storedAt = n.storedAt(seq, storedAt)
		}
		env, err := code.Domain{}.Envelope(payload)
		if err != nil {
			n.t.Errorf("decode record %d: %v", seq, err)
			return
		}
		record := statelog.Record{
			Envelope: env,
			Position: statelog.Position{Stream: spec.Name, Generation: env.Gen, Seq: seq},
			Payload:  payload, StoredAt: storedAt,
		}
		wire := spec.SubjectPrefix + "." + code.Subject{
			Kind: code.ObjectKind(env.Subject.Kind), ID: env.Subject.ID,
		}.String()
		if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
			if _, gated, err := n.applier.Gated(ctx, tx, record); err != nil || gated {
				return err
			}
			rows, err := n.applier.Apply(ctx, tx, record, statelog.ApplyOptions{
				Now: harnessAt, StoredAt: storedAt, MaxVariables: n.db.Caps().MaxVariables,
			})
			if err != nil {
				return err
			}
			if n.applied != nil {
				n.applied(seq, rows)
			}
			if env.OpID != "" {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO code_ops (op_id, subject, position, applied_at)
					VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
					env.OpID, wire, record.Position.Packed(), store.EncodeTime(storedAt)); err != nil {
					return err
				}
			}
			if code.ObjectKind(env.Subject.Kind).Arbitrated() {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?,?,?)
					ON CONFLICT (stream, subject) DO UPDATE SET
						anchor = MAX(anchor, excluded.anchor)`,
					record.Position.Stream, wire, record.Position.Packed()); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO statelog_cursor
					(stream, generation, seq, stream_created_at, updated_at, applied_version)
				VALUES (?,?,?,?,0,0)
				ON CONFLICT (stream) DO UPDATE SET
					generation = excluded.generation, seq = excluded.seq`,
				record.Position.Stream, int64(record.Position.Generation),
				int64(record.Position.Seq), store.EncodeTime(harnessAt))
			return err
		}); err != nil {
			n.t.Errorf("apply record %d: %v", seq, err)
			return
		}
		n.applier.Committed(ctx)
		n.consumed = seq
		n.waiter.reach(record.Position)
	}
}

// activate raises the fence to the activation at.
func (n *node) activate(at time.Time) code.Activation {
	n.t.Helper()
	a, err := n.store.Activate(n.t.Context(), at)
	if err != nil {
		n.t.Fatalf("activate %s: %v", at, err)
	}
	return a
}

// ensure declares one repository at the activation at, and answers what that
// did.
func (n *node) ensure(name string, settings code.RepoSettings, at time.Time) code.Ensured {
	n.t.Helper()
	got, err := n.store.EnsureRepo(n.t.Context(), n.activate(at), code.Declared{
		Name: name, Settings: settings,
	})
	if err != nil {
		n.t.Fatalf("ensure %q at %s: %v", name, at, err)
	}
	n.drain()
	return got
}

// push publishes one (repo, push) record carrying entries, through this node's
// publisher, and answers the result — a record built here rather than decided
// by a committer, for the cases about the applier and the reader.
func (n *node) push(repo string, entries ...code.PushEntry) statelog.Result {
	n.t.Helper()
	res, err := n.pushErr(repo, entries...)
	if err != nil {
		n.t.Fatalf("push to %s: %v", repo, err)
	}
	return res
}

// pushErr is [node.push], answering its error.
func (n *node) pushErr(repo string, entries ...code.PushEntry) (statelog.Result, error) {
	return n.publish(code.RepoSubject(repo), code.OpPush, code.Scope{code.RepoPath(repo)},
		code.PushBatch{RepoID: repo, Pushes: entries})
}

// publish appends one record a case builds by hand — a record no writer of
// this build forms, or one formed from a decision taken earlier than its
// append — through this node's publisher, and drains it.
func (n *node) publish(subject code.Subject, op code.OpKind, scope code.Scope,
	payload any) (statelog.Result, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Result{}, err
	}
	opID := statelog.NewOpID(time.Now(), "code."+string(op))
	res, err := n.pub.Publish(n.t.Context(), statelog.Request{
		Subject: statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		Scope:   scope.Set(), OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			encoded, err := code.Encode(code.MutationRecord{
				RecordEnvelope: code.RecordEnvelope{
					OpID: opID, Subject: subject, Op: op, CreatedAt: harnessAt,
					Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
				},
				Mutation: body, Actor: "harness", ActorKind: code.ActorSystem,
			})
			return statelog.Decision{Payload: encoded}, err
		},
	})
	// A HELD NODE STAYS BEHIND its own write, which is what holding it is for.
	if err == nil && n.waiter.running() {
		n.drain()
	}
	return res, err
}

// entry is one push of commands with a pack named for id.
func entry(id string, cmds ...code.Command) code.PushEntry {
	return code.PushEntry{
		PushID: id,
		Principal: code.Principal{
			Kind: code.PrincipalOperator, OperatorID: "op-harness", Seat: "harness",
		},
		Via:  code.ViaGit,
		Node: "node-a", Commands: cmds, Pack: suitePack(id), ReceivedAt: harnessAt,
	}
}

// set is a command moving ref from old to its new oid.
func set(ref, old, new string) code.Command {
	return code.Command{Ref: ref, Old: old, New: new, Result: code.ResultOK}
}

// count is how many rows of a query this node holds.
func (n *node) count(query string, args ...any) int {
	n.t.Helper()
	var c int
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), query, args...).Scan(&c)
	}); err != nil {
		n.t.Fatalf("count %q: %v", query, err)
	}
	return c
}

// exec runs a statement in a writer transaction of this node's estate — a
// case's hand-built row, never the applier's.
func (n *node) exec(query string, args ...any) {
	n.t.Helper()
	if err := n.db.Tx(n.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(n.t.Context(), query, args...)
		return err
	}); err != nil {
		n.t.Fatalf("exec %q: %v", query, err)
	}
}

// testWaiter is this node's applier as the publisher and the reader see it.
//
// It APPLIES WHILE A WRITE WAITS, because a running node's applier does: a
// write waits for this node to apply its record, and a harness whose applier
// ran only when a case drained would let every write's budget expire. A HELD
// node ([node.hold]) answers at once what an expired budget would.
type testWaiter struct {
	mu      sync.Mutex
	at      statelog.Position
	advance func()
}

// running reports an applier in the wait — a node nobody holds.
func (w *testWaiter) running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.advance != nil
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
	if w.at.Stream == "" {
		return statelog.Position{Stream: code.Domain{}.Stream().Name}
	}
	return w.at
}

// Reached is Committed: this rig moves its position when it applies a record,
// in one step, so it reaches nothing its commit has not.
func (w *testWaiter) Reached() statelog.Position { return w.Committed() }

// StreamIdentity and Truncated: this harness never rebuilds its log.
func (w *testWaiter) StreamIdentity() error { return nil }
func (w *testWaiter) Truncated() error      { return nil }

// Voided voids nothing: no generation is ever abandoned here.
func (w *testWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

func (w *testWaiter) WaitReached(ctx context.Context, p statelog.Position) error {
	for {
		if w.Committed().Packed() >= p.Packed() {
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

func (w *testWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet, p statelog.Position) error {
	return w.WaitReached(ctx, p)
}
