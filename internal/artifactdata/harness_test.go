package artifactdata_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WHOLE PATH: a write reaches a real embedded broker, the shipped applier
// and the shipped reader, with nothing between them mocked.

// monday is every harness's clock.
var monday = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// node is one data node of the harness.
type node struct {
	t         *testing.T
	id        string
	db        store.ReplicatedHandle
	log       *js.DomainLog
	writer    *artifactdata.Writer
	publisher *statelog.Publisher
	reader    *artifactdata.Reader
	applier   *artifactdata.Applier
	waiter    *testWaiter
	verifier  *statelog.Verifier
	judged    *judged

	mu       sync.Mutex
	consumed uint64
	moved    []artifactdata.Moved

	// storedAt overrides the broker's instant for every record this node
	// applies, for a case about broker time (the history's hot window).
	storedAt func(seq uint64, broker time.Time) time.Time

	// clock is the writer's: a second further on at every read, so this
	// node's write budget refills between a case's writes, unless frozen.
	clock *harnessClock
}

// harnessClock is a writer's clock that moves a step (a second unless a case
// sets one) each read.
type harnessClock struct {
	mu     sync.Mutex
	at     time.Time
	step   time.Duration
	frozen bool
}

func (c *harnessClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.frozen {
		step := c.step
		if step == 0 {
			step = time.Second
		}
		c.at = c.at.Add(step)
	}
	return c.at
}

// stride makes each read move the clock by d, so a case writing whole
// batches back to back stays inside its write budget.
func (c *harnessClock) stride(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = d
}

// freeze stops the clock, so a case can spend a write budget at one instant.
func (c *harnessClock) freeze() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = true
}

// seats is the organisation every harness resolves a person value against.
var seats = map[string]bool{"jane": true, "bob": true, "dana": true, "sam": true, "agent-iris": true}

func seatExists(_ context.Context, handle string) (bool, error) { return seats[handle], nil }

// harnessCeiling is the byte ceiling every harness provisions its log at: a
// quarter of Tier A's floor, for the artifacts harness's reason.
const harnessCeiling = config.ArtifactDataLogMaxBytesFloor / 4

// openLog stands up a broker and the datastore log on it.
func openLog(t *testing.T) *js.DomainLog {
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
	spec := artifactdata.Domain{}.Stream()
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: harnessCeiling,
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

// openStore opens one node's replicated estate.
func openStore(t testing.TB, name string) store.ReplicatedHandle {
	t.Helper()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), name), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}

// newHarness is one node over a log of its own.
func newHarness(t *testing.T) *node {
	t.Helper()
	return newNode(t, openLog(t), openStore(t, "node.db"), "node-a")
}

// newNode is a node over a log and a store it is handed.
func newNode(t *testing.T, log *js.DomainLog, db store.ReplicatedHandle, nodeID string) *node {
	t.Helper()
	d := artifactdata.Domain{}
	spec := d.Stream()
	rows, err := artifactdata.NewRows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := artifactdata.NewFence(db.Reader(), nodeID)
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	waiter := &testWaiter{}
	fence.Committed = waiter.Committed
	reserve, err := statelog.NewReserve(spec, func(ctx context.Context) (statelog.Usage, error) {
		stats, err := log.Stats(ctx)
		return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
	})
	if err != nil {
		t.Fatalf("build the gate reserve: %v", err)
	}
	signer, err := statelog.NewSigner(d.Name(), statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	verifier, err := statelog.NewVerifier(d.Name(), statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: d, Spec: spec, Log: log,
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Rows:    rows, Fence: fence, Signer: signer,
		Gates: artifactdata.NewGates(db.Reader()), Waiter: waiter, Voids: waiter,
		Identity: waiter, NodeID: nodeID, Admission: reserve,
		Generation: func() uint32 { return 0 }, ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	clock := &harnessClock{at: monday}
	judge := &judged{}
	writer, err := artifactdata.NewWriter(artifactdata.WriterDeps{
		Publisher: publisher, DB: db.Reader(), SeatExists: seatExists, Now: clock.now,
		Objectives: judge,
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	authority, err := statelogtest.LocalReader(d, db, waiter.Committed())
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	reader, err := artifactdata.NewReader(artifactdata.ReaderOptions{DB: db.Reader(), Log: authority})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	n := &node{t: t, id: nodeID, db: db, log: log, writer: writer, publisher: publisher,
		reader: reader, waiter: waiter, verifier: verifier, judged: judge, clock: clock}
	n.applier = artifactdata.NewApplier(nodeID, func(moved []artifactdata.Moved) {
		n.moved = append(n.moved, moved...)
	})
	waiter.advance = n.drain
	return n
}

// drain consumes every record the broker holds beyond what this node has
// applied, one transaction per record.
func (n *node) drain() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	last, err := n.log.End(n.t.Context())
	if err != nil {
		n.t.Fatalf("read the log's end: %v", err)
	}
	spec := artifactdata.Domain{}.Stream()
	for seq := n.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := n.log.At(n.t.Context(), seq)
		if err != nil {
			n.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		body, verdict := n.verifier.Open(payload)
		if verdict != statelog.Verified {
			n.t.Fatalf("record %d did not verify: %s", seq, verdict)
		}
		if n.storedAt != nil {
			storedAt = n.storedAt(seq, storedAt)
		}
		n.applyBody(spec, seq, body, storedAt)
	}
}

// applyBody applies one verified record body at seq, with the framework's own
// bookkeeping in the same transaction.
func (n *node) applyBody(spec statelog.StreamSpec, seq uint64, body []byte, storedAt time.Time) {
	n.t.Helper()
	envelope, err := artifactdata.Domain{}.Envelope(body)
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
				Now: monday, StoredAt: storedAt, MaxVariables: 999,
			}); err != nil {
				return err
			}
			if envelope.OpID != "" {
				if _, err := tx.ExecContext(n.t.Context(), `
					INSERT INTO artifactdata_ops (op_id, subject, position, applied_at)
					VALUES (?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
					envelope.OpID, wire, record.Position.Packed(),
					store.EncodeTime(storedAt)); err != nil {
					return err
				}
			}
		}
		if envelope.Kind != statelog.BarrierKind {
			if _, err := tx.ExecContext(n.t.Context(), `
				INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?,?,?)
				ON CONFLICT (stream, subject) DO UPDATE SET anchor = MAX(anchor, excluded.anchor)`,
				record.Position.Stream, wire, record.Position.Packed()); err != nil {
				return err
			}
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

// holdApplier keeps this node's applier out of every wait: a write resolves
// `pending`.
func (n *node) holdApplier() {
	n.waiter.mu.Lock()
	defer n.waiter.mu.Unlock()
	n.waiter.advance = nil
}

// testWaiter is this node's applier as the publisher sees it.
type testWaiter struct {
	mu      sync.Mutex
	at      statelog.Position
	advance func()
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

// Reached is Committed: this rig moves its position when it applies a record,
// in one step, so it reaches nothing its commit has not.
func (w *testWaiter) Reached() statelog.Position { return w.Committed() }

func (w *testWaiter) StreamIdentity() error                         { return nil }
func (w *testWaiter) Truncated() error                              { return nil }
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

// ---- fixtures ----------------------------------------------------------- //

// limits are the plan quotas every harness call is stamped with.
func limits() artifacts.Limits {
	return artifacts.Limits{
		CompanyBytes: artifacts.DefaultCompanyBytes, Artifacts: artifacts.DefaultArtifacts,
		DataBytes: artifacts.DefaultDataBytes, DataStoreBytes: artifacts.DefaultDataStoreBytes,
		PendingBytes: artifacts.DefaultPendingBytes,
	}
}

// person is a call by a person with a role on the artifact.
func person(handle string, role artifacts.Role) artifactdata.Call {
	return artifactdata.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: handle, Kind: iam.ActorHuman, OperatorID: "session:" + handle},
		Role:  role, Limits: limits(), Origin: artifacts.OriginDashboard,
	}}
}

// agent is a call by an agent seat with a role on the artifact.
func agent(handle string, role artifacts.Role) artifactdata.Call {
	return artifactdata.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: handle, Kind: iam.ActorAgent, OperatorID: handle},
		Role:  role, Limits: limits(), Origin: artifacts.OriginTool,
	}, TurnID: "turn-" + handle}
}

// sheet is the artifact every case writes, unless it names another.
var sheet = artifacts.IDFor("the review sheet")

// raw is a JSON literal.
func raw(s string) json.RawMessage { return json.RawMessage(s) }

// set is a set op.
func set(coll, doc string, fields map[string]string) artifacts.DataOp {
	op := artifacts.DataOp{Op: artifacts.DataSet, Collection: coll, Doc: doc,
		Fields: map[string]json.RawMessage{}}
	for k, v := range fields {
		op.Fields[k] = raw(v)
	}
	return op
}

// merge is a merge op.
func merge(coll, doc string, fields map[string]string) artifacts.DataOp {
	op := set(coll, doc, fields)
	op.Op = artifacts.DataMerge
	return op
}

// guarded is op conditioned on version.
func guarded(op artifacts.DataOp, version int64) artifacts.DataOp {
	op.IfVersion = &version
	return op
}

// field is an increment, append, add or remove op.
func field(kind artifacts.DataOpKind, coll, doc, name, value string) artifacts.DataOp {
	return artifacts.DataOp{Op: kind, Collection: coll, Doc: doc, Field: name, Value: raw(value)}
}

// write publishes ops as c and answers the result, failing the case on an
// error before the append.
func (n *node) write(c artifactdata.Call, ops ...artifacts.DataOp) artifactdata.WriteResult {
	n.t.Helper()
	res, err := n.writer.Write(n.t.Context(), c, sheet, artifacts.DataWrite{Ops: ops})
	if err != nil {
		n.t.Fatalf("write %v: %v", ops, err)
	}
	return res
}

// applied publishes ops as c and fails the case unless they apply.
func (n *node) applied(c artifactdata.Call, ops ...artifacts.DataOp) artifactdata.WriteResult {
	n.t.Helper()
	res := n.write(c, ops...)
	if res.Verdict != artifacts.DataApplied {
		n.t.Fatalf("write %v: verdict %s (%s), want applied", ops, res.Verdict, res.Reason)
	}
	return res
}

// refused publishes ops as c and fails the case unless the applier refused
// them with want.
func (n *node) refused(c artifactdata.Call, want artifacts.DataVerdict, ops ...artifacts.DataOp) artifactdata.WriteResult {
	n.t.Helper()
	res := n.write(c, ops...)
	if res.Verdict != want {
		n.t.Fatalf("write %v: verdict %s (%s), want %s", ops, res.Verdict, res.Reason, want)
	}
	return res
}

// configure sets the sheet's schema as c, failing the case unless it applies.
func (n *node) configure(c artifactdata.Call, collections ...artifacts.Collection) artifactdata.WriteResult {
	n.t.Helper()
	res, err := n.writer.Configure(n.t.Context(), c, sheet, collections)
	if err != nil {
		n.t.Fatalf("configure: %v", err)
	}
	if res.Verdict != artifacts.DataApplied {
		n.t.Fatalf("configure: verdict %s (%s), want applied", res.Verdict, res.Reason)
	}
	return res
}

// reading is a reader's standing.
func reading(record string, role artifacts.Role) artifactdata.Reading {
	return artifactdata.Reading{Record: record, Role: role,
		Fresh: statelog.Freshness{Level: statelog.ReadSession}}
}

// doc reads one document as an editor, failing the case where it is absent.
func (n *node) doc(coll, id string) artifactdata.Doc {
	n.t.Helper()
	d, err := n.reader.Doc(n.t.Context(), sheet, reading("owner", artifacts.RoleOwner), coll, id)
	if err != nil {
		n.t.Fatalf("read %s/%s: %v", coll, id, err)
	}
	return d
}

// fields is a document's fields as a map of their canonical JSON.
func fields(t *testing.T, d artifactdata.Doc) map[string]string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(d.Fields, &m); err != nil {
		t.Fatalf("decode %s/%s: %v", d.Collection, d.ID, err)
	}
	out := map[string]string{}
	for k, v := range m {
		out[k] = string(v)
	}
	return out
}

// table renders one table's rows in key order, for a comparison across nodes.
func (n *node) table(name string) string {
	n.t.Helper()
	var out string
	if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT * FROM `+name)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		var lines []string
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			var line strings.Builder
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					v = fmt.Sprintf("%x", b)
				}
				fmt.Fprintf(&line, "%s=%v;", cols[i], v)
			}
			lines = append(lines, line.String())
		}
		slices.Sort(lines)
		out = strings.Join(lines, "\n")
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read table %s: %v", name, err)
	}
	return out
}

// count is a query's one integer.
func (n *node) count(q string, args ...any) int64 {
	n.t.Helper()
	var v int64
	if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), q, args...).Scan(&v)
	}); err != nil {
		n.t.Fatalf("%s: %v", q, err)
	}
	return v
}

// replicatedTables is every table the domain claims identity over.
func replicatedTables() []string {
	var out []string
	for name, class := range (artifactdata.Domain{}).Tables() {
		if class == statelog.Replicated {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
