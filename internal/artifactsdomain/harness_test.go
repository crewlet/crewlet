package artifactsdomain_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WHOLE PATH: a write reaches a real embedded broker, the shipped applier
// and the shipped reader, with nothing between them mocked. Every property the
// round-trip cases assert is a claim about the three agreeing.

// monday is every harness's clock: the authored instants, and the instant every
// upload's key is minted at, so the freshness rule passes for every upload a
// case did not age on purpose.
var monday = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// node is one data node of the harness: its own store, publisher, writer,
// reader and applier over a log it may share with another node.
type node struct {
	t         *testing.T
	id        string
	db        store.ReplicatedHandle
	log       *js.DomainLog
	writer    *artifactsdomain.Writer
	publisher *statelog.Publisher
	reader    *artifactsdomain.Reader
	applier   *artifactsdomain.Applier
	waiter    *testWaiter
	verifier  *statelog.Verifier

	mu       sync.Mutex
	consumed uint64
	moved    []artifactsdomain.Moved
}

// seatKinds is the chart every harness resolves a transfer's new owner
// against: seats named agent-* are agents, everything else a person's.
func seatKinds(_ context.Context, handle string) (iam.ActorKind, error) {
	if len(handle) > 6 && handle[:6] == "agent-" {
		return iam.ActorAgent, nil
	}
	return iam.ActorHuman, nil
}

// harnessCeiling is the byte ceiling every harness provisions its log at.
//
// A QUARTER OF TIER A'S FLOOR, and tighter is the honest direction: at this
// ceiling the gate reserve still takes its full fleet term out of the top of
// the log, so every write here is admitted against an ordinary ceiling smaller
// than any production log has. What the broker reserves for it is accounting
// against that broker's own declared limit ([brokertest.StoreMaxBytes]) and
// claims nothing on disk, so the figure is chosen for the gate and not for the
// machine running the suite.
const harnessCeiling = config.ArtifactsLogMaxBytesFloor / 4

// openLog stands up a broker and the artifacts log on it, at [harnessCeiling].
func openLog(t *testing.T) *js.DomainLog {
	t.Helper()
	// THE BROKER DECLARES ITS SIZE, for brokertest's reason: left to derive
	// its cap from the runner's free space it takes three quarters of it,
	// and would refuse even this log's quarter gibibyte on a runner with
	// less than a third of one free, whatever the case under it does.
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
	spec := artifactsdomain.Domain{}.Stream()
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

// openStore opens one node's replicated estate, closed with the test.
func openStore(t *testing.T, name string) store.ReplicatedHandle {
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

// newNode is a node over a log and a store it is handed: a second node joining
// a log under its own id and store is the shape a race and a convergence need.
func newNode(t *testing.T, log *js.DomainLog, db store.ReplicatedHandle, nodeID string) *node {
	t.Helper()
	d := artifactsdomain.Domain{}
	spec := d.Stream()
	rows, err := artifactsdomain.NewRows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := artifactsdomain.NewFence(db.Reader(), nodeID)
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
		Gates: artifactsdomain.NewGates(db.Reader()), Waiter: waiter, Voids: waiter,
		Identity: waiter, NodeID: nodeID, Admission: reserve,
		Generation: func() uint32 { return 0 }, ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	writer, err := artifactsdomain.NewWriter(artifactsdomain.WriterDeps{
		Publisher: publisher, DB: db.Reader(),
		Now: func() time.Time { return monday }, SeatKind: seatKinds,
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	authority, err := statelogtest.LocalReader(d, db, waiter.Committed())
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	reader, err := artifactsdomain.NewReader(artifactsdomain.ReaderOptions{DB: db.Reader(), Log: authority})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	n := &node{t: t, id: nodeID, db: db, log: log, writer: writer, publisher: publisher,
		reader: reader, waiter: waiter, verifier: verifier}
	n.applier = artifactsdomain.NewApplier(nodeID, func(moved []artifactsdomain.Moved) {
		n.moved = append(n.moved, moved...)
	})
	waiter.advance = n.drain
	return n
}

// drain consumes every record the broker holds beyond what this node has
// applied, exactly as the framework's loop does: one transaction per record
// carrying the rows, the operation id, the anchor and the checkpoint together.
func (n *node) drain() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	last, err := n.log.End(n.t.Context())
	if err != nil {
		n.t.Fatalf("read the log's end: %v", err)
	}
	spec := artifactsdomain.Domain{}.Stream()
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
		n.applyBody(spec, seq, body, storedAt)
	}
}

// applyBody applies one verified record body at seq, with the framework's own
// bookkeeping in the same transaction.
func (n *node) applyBody(spec statelog.StreamSpec, seq uint64, body []byte, storedAt time.Time) {
	n.t.Helper()
	envelope, err := artifactsdomain.Domain{}.Envelope(body)
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
					INSERT INTO artifacts_ops (op_id, subject, position, applied_at)
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

// head reads one artifact back at the session level.
func (n *node) head(id artifacts.ID) artifactsdomain.Head {
	n.t.Helper()
	h, _, err := n.reader.Artifact(n.t.Context(), id, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		n.t.Fatalf("read artifact %s: %v", id, err)
	}
	return h
}

// testWaiter is this node's applier as the publisher sees it: it applies while
// a write waits, as a running node's does.
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

func (w *testWaiter) StreamIdentity() error                         { return nil }
func (w *testWaiter) Truncated() error                              { return nil }
func (w *testWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

func (w *testWaiter) WaitCommitted(ctx context.Context, p statelog.Position) error {
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
	return w.WaitCommitted(ctx, p)
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

// person is a call by a person, with the given role on the artifact.
func person(handle string, role artifacts.Role) artifactsdomain.Call {
	return artifactsdomain.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: handle, Kind: iam.ActorHuman, OperatorID: "session:" + handle},
		Role:  role, Limits: limits(), Origin: artifacts.OriginDashboard,
	}}
}

// agent is a call by an agent seat, with the given role on the artifact.
func agent(handle string, role artifacts.Role) artifactsdomain.Call {
	return artifactsdomain.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: handle, Kind: iam.ActorAgent, OperatorID: handle},
		Role:  role, Limits: limits(), Origin: artifacts.OriginTool,
	}, TurnID: "turn-" + handle}
}

// keyOf is a deterministic object key for name, minted at monday.
func keyOf(name string) objstore.Key {
	return keyAt(name, monday)
}

// keyAt is a deterministic object key for name, minted at t.
func keyAt(name string, t time.Time) objstore.Key {
	var k objstore.Key
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(t.UnixMilli()))
	copy(k[:6], stamp[2:])
	sum := sha256.Sum256([]byte(name))
	copy(k[6:], sum[:10])
	k[6] = k[6]&0x0f | 0x70
	k[8] = k[8]&0x3f | 0x80
	return k
}

// upload is one uploaded file: its entry and its object.
func upload(t *testing.T, path, content string) artifacts.Entry {
	t.Helper()
	return entryOf(t, path, content, keyOf(path+"\x00"+content))
}

// entryOf is one file's entry naming key.
func entryOf(t *testing.T, path, content string, key objstore.Key) artifacts.Entry {
	t.Helper()
	e, err := manifest.NewEntry(path, "", objstore.Object{
		Key: key, Hash: objstore.HashOf([]byte(content)), Size: int64(len(content)),
	}, false)
	if err != nil {
		t.Fatalf("entry %s: %v", path, err)
	}
	return e
}

// carried is an entry carried from this artifact's own objects.
func carried(e artifacts.Entry) artifacts.Entry {
	e.Carried = true
	return e
}

// page is a publish of an HTML page made of files, entry first, every
// non-carried file uploaded by it.
func page(files ...artifacts.Entry) artifactsdomain.PublishInput {
	in := artifactsdomain.PublishInput{
		Manifest: artifacts.Manifest{Entry: files[0].Path, Files: files},
		Contract: "1.0.0",
	}
	for _, f := range files {
		if !f.Carried {
			in.Uploaded = append(in.Uploaded, f.Object.Key)
		}
	}
	in.Manifest = manifest.Canonical(in.Manifest)
	return in
}

// create makes an HTML artifact owned by owner, its first version one page.
func (n *node) create(c artifactsdomain.Call, title string, files ...artifacts.Entry) artifactsdomain.Published {
	n.t.Helper()
	got, err := n.writer.Create(n.t.Context(), c, artifactsdomain.CreateInput{
		Kind: artifacts.KindHTML, Title: title, Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, First: page(files...),
	})
	if err != nil {
		n.t.Fatalf("create %q: %v", title, err)
	}
	return got
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
			line := ""
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				line += fmt.Sprintf("%s=%v;", cols[i], v)
			}
			lines = append(lines, line)
		}
		slices.Sort(lines)
		for _, l := range lines {
			out += l + "\n"
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read table %s: %v", name, err)
	}
	return out
}

// seedAssets writes count asset rows naming size bytes each straight into this
// node's copy of artifact id, the state a decide meets on an asset store at its
// caps — rows no test could otherwise reach without a thousand writes.
func (n *node) seedAssets(id artifacts.ID, count int, size int64) {
	n.t.Helper()
	if err := n.db.Tx(n.t.Context(), func(tx *sql.Tx) error {
		for i := range count {
			if _, err := tx.ExecContext(n.t.Context(), `
				INSERT INTO artifact_assets (artifact_id, asset_id, name, content_type,
					object, size, added_by, added_by_kind, added_at, version)
				VALUES (?,?,?,?,?,?,?,?,?,?)`, id.String(), uuid.NewString(),
				fmt.Sprintf("seeded-%d.png", i), "image/png",
				fmt.Sprintf("seeded-%s-%d", id, i), size, "jane", "human", 0, 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		n.t.Fatalf("seed %d assets on %s: %v", count, id, err)
	}
}
