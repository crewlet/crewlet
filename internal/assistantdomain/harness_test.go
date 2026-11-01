package assistantdomain_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WHOLE PATH: a write reaches a real embedded broker, the shipped applier
// and the shipped reader, with nothing between them mocked. Every property the
// cases here assert is a claim about the three agreeing.

// monday is every harness's clock.
var monday = time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)

// harnessCeiling is the byte ceiling every harness provisions its log at: a
// quarter of the declared ceiling, because the broker reserves a stream's
// whole ceiling on disk the moment it creates it.
const harnessCeiling = assistantdomain.AssistantLogMaxBytes / 4

// testKeyring is the fleet keyring every harness seals under.
func testKeyring(t *testing.T, ids ...string) secrets.Cipher {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"k1"}
	}
	ring := secrets.Keyring{ActiveID: ids[0], Keys: map[string][]byte{}}
	for i, id := range ids {
		key := make([]byte, 32)
		for j := range key {
			key[j] = byte(i*31 + j + 1)
		}
		ring.Keys[id] = key
	}
	cipher, err := secrets.NewCipher(ring)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}

// newSealer is a sealer over the harness keyring.
func newSealer(t *testing.T) *assistantdomain.Sealer {
	t.Helper()
	s, err := assistantdomain.NewSealer(testKeyring(t))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	return s
}

// node is one data node of the harness: its own store, publisher, writer,
// reader and applier over a log it may share with another node.
type node struct {
	t         *testing.T
	id        string
	db        store.ReplicatedHandle
	log       *js.DomainLog
	writer    *assistantdomain.Writer
	publisher *statelog.Publisher
	reader    *assistantdomain.Reader
	logs      *syncBuffer
	counts    *counter
	applier   *assistantdomain.Applier
	waiter    *testWaiter
	verifier  *statelog.Verifier

	mu       sync.Mutex
	consumed uint64
}

// openLog stands up a broker and the assistant log on it.
func openLog(t *testing.T) *js.DomainLog {
	t.Helper()
	// THE BROKER DECLARES ITS SIZE, for brokertest's reason: left to derive
	// its cap from the runner's free space it takes three quarters of it, a
	// fact about the machine rather than about the case.
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
	spec := assistantdomain.Domain{}.Stream()
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
// a log under its own id and store is the shape a race needs.
func newNode(t *testing.T, log *js.DomainLog, db store.ReplicatedHandle, nodeID string) *node {
	t.Helper()
	d := assistantdomain.Domain{}
	spec := d.Stream()
	rows, err := assistantdomain.NewRows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := assistantdomain.NewFence(db.Reader(), nodeID)
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
		Gates: assistantdomain.NewGates(db.Reader()), Waiter: waiter, Voids: waiter,
		Identity: waiter, NodeID: nodeID, Admission: reserve,
		Generation: func() uint32 { return 0 }, ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	sealer := newSealer(t)
	writer, err := assistantdomain.NewWriter(assistantdomain.WriterDeps{
		Publisher: publisher, DB: db.Reader(), Sealer: sealer,
		Now: func() time.Time { return monday },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	authority, err := statelogtest.LocalReader(d, db, waiter.Reached())
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	logs, counts := &syncBuffer{}, &counter{}
	reader, err := assistantdomain.NewReader(assistantdomain.ReaderOptions{
		DB: db.Reader(), Log: authority, Sealer: sealer,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)), Metrics: counts,
	})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	n := &node{t: t, id: nodeID, db: db, log: log, writer: writer, publisher: publisher,
		reader: reader, logs: logs, counts: counts, waiter: waiter, verifier: verifier,
		applier: assistantdomain.NewApplier()}
	waiter.advance = n.drain
	return n
}

// drain consumes every record the broker holds beyond what this node has
// applied, exactly as the framework's loop does.
func (n *node) drain() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	last, err := n.log.End(n.t.Context())
	if err != nil {
		n.t.Fatalf("read the log's end: %v", err)
	}
	spec := assistantdomain.Domain{}.Stream()
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
	envelope, err := assistantdomain.Domain{}.Envelope(body)
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
				Now: monday, StoredAt: storedAt, MaxVariables: 999,
			}); err != nil {
				return err
			}
			if envelope.OpID != "" {
				if _, err := tx.ExecContext(n.t.Context(), `
					INSERT INTO assistant_ops (op_id, subject, position, applied_at)
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

// Committed is the checkpoint the fence and the reader are wired to: this
// harness commits every record it reaches, so it is [testWaiter.Reached].
func (w *testWaiter) Committed() statelog.Position { return w.Reached() }

func (w *testWaiter) Reached() statelog.Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

func (w *testWaiter) StreamIdentity() error                         { return nil }
func (w *testWaiter) Truncated() error                              { return nil }
func (w *testWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

func (w *testWaiter) WaitReached(ctx context.Context, p statelog.Position) error {
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

func (w *testWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet, p statelog.Position) error {
	return w.WaitReached(ctx, p)
}

// ---- fixtures ----------------------------------------------------------- //

// op mints a fresh operation id.
func op(name string) string { return statelog.NewOpID(time.Now(), name) }

// person is a call as the person who owns the conversation, under a fresh
// operation.
func person(owner uuid.UUID, name string) assistantdomain.Call {
	return assistantdomain.Call{
		Owner: owner,
		By:    iam.Actor{Name: name, Kind: iam.ActorHuman, OperatorID: "session:" + name},
		OpID:  op("assistant"),
	}
}

// fresh is c under a new operation id.
func fresh(c assistantdomain.Call) assistantdomain.Call {
	c.OpID = op("assistant")
	return c
}

// conversation creates one conversation for c's owner and answers its id.
func (n *node) conversation(c assistantdomain.Call, title string) uuid.UUID {
	n.t.Helper()
	id := uuid.New()
	if _, err := n.writer.CreateConversation(n.t.Context(), fresh(c), id, title); err != nil {
		n.t.Fatalf("create conversation %q: %v", title, err)
	}
	return id
}

// openTurn opens a turn in conversation and answers its id.
func (n *node) openTurn(c assistantdomain.Call, conversation uuid.UUID) uuid.UUID {
	n.t.Helper()
	turn := uuid.New()
	if _, _, err := n.writer.OpenTurn(n.t.Context(), fresh(c), assistantdomain.NewTurn{
		Conversation: conversation, Turn: turn, Node: n.id, Model: "claude",
		Page: &wire.PageChip{Title: "Worklet › Tasks", Screen: "worklet-tasks"},
	}); err != nil {
		n.t.Fatalf("open a turn: %v", err)
	}
	return turn
}

// say records one message of turn.
func (n *node) say(c assistantdomain.Call, turn uuid.UUID, idx int, role wire.MessageRole, text string) uuid.UUID {
	n.t.Helper()
	id := assistantdomain.MessageID(turn, idx)
	if _, err := n.writer.RecordMessage(n.t.Context(), fresh(c), turn,
		assistantdomain.NewMessage{ID: id, Role: role, Text: text}); err != nil {
		n.t.Fatalf("record message %d: %v", idx, err)
	}
	return id
}

// proposalCard is a valid proposal card for proposal.
func proposalCard(proposal uuid.UUID) wire.Card {
	return wire.Card{Type: wire.CardProposal, Proposal: &wire.ProposalCard{
		ProposalID: proposal, Scope: wire.ScopeServer, Tool: "create_task",
		Title: "Create task", Changes: []wire.Change{{Kind: wire.ChangeFact, Field: "project", Label: "Project", To: "ENG"}},
		Digest:    "sha256:" + hex64("d"),
		ExpiresAt: monday.Add(15 * time.Minute), State: wire.ProposalProposed,
		ApproveLabel: "Approve and create",
	}}
}

// hex64 is 64 hex digits of c.
func hex64(c string) string {
	out := ""
	for range 64 {
		out += c
	}
	return out
}

// propose records a proposal of turn and answers its id.
func (n *node) propose(c assistantdomain.Call, turn uuid.UUID) uuid.UUID {
	n.t.Helper()
	id := uuid.New()
	if _, err := n.writer.CreateProposal(n.t.Context(), fresh(c), assistantdomain.NewProposal{
		Turn: turn, Proposal: id, Tool: "create_task", Act: "work.write",
		Scope: wire.ScopeServer, Method: "POST", Path: "/operator/act/create_task",
		Body:          []byte(`{"title":"Payroll review"}`),
		RequestDigest: hex64("a"), Digest: "sha256:" + hex64("d"),
		OpKey: op("approve"), Card: proposalCard(id),
		ExpiresAt: monday.Add(15 * time.Minute),
	}); err != nil {
		n.t.Fatalf("create a proposal: %v", err)
	}
	return id
}

// table renders one table's rows sorted, for a comparison across nodes.
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

// count is how many rows of table name hold column = value.
func (n *node) count(table, column, value string) int {
	n.t.Helper()
	var c int
	if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(),
			`SELECT COUNT(*) FROM `+table+` WHERE `+column+` = ?`, value).Scan(&c)
	}); err != nil {
		n.t.Fatalf("count %s: %v", table, err)
	}
	return c
}

// staticCipher is the harness keyring for a caller with no *testing.T: the
// suite's write hook.
func staticCipher() secrets.Cipher {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": key}})
	if err != nil {
		panic(err)
	}
	return cipher
}

// rows is how many rows table holds.
func (n *node) rows(table string) int {
	n.t.Helper()
	var c int
	if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&c)
	}); err != nil {
		n.t.Fatalf("count %s: %v", table, err)
	}
	return c
}

// syncBuffer is a log sink a suite reads back while the reader writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// counter records what a reader counts, by name and attributes.
type counter struct {
	mu   sync.Mutex
	adds []counted
}

type counted struct {
	name  string
	n     uint64
	attrs metrics.Attrs
}

func (c *counter) Add(name string, n uint64, attrs metrics.Attrs) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adds = append(c.adds, counted{name: name, n: n, attrs: attrs})
}

func (c *counter) read() []counted {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]counted(nil), c.adds...)
}
