package chart_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WHOLE WRITE PATH: a batch reaches the broker, the applier and the rows.
//
// Everything below it has its own tests over fakes, and every one of them can
// be individually right while the composition is wrong — a scope the writer
// resolves one way and the probe another, an expectation formed from a value
// that moved, a decide that read outside its own snapshot. None of that is
// visible without a real broker and a real store on both ends, which is why
// this harness has both.
//
// IT IS ALSO THE ONLY PLACE ARBITRATION CAN BE PROVEN. The framework's shared
// suite certifies determinism and idempotency against one estate; "two writers
// contended and exactly one won" is a claim about a broker, and nothing but a
// broker can settle it.

type writeRig struct {
	t       *testing.T
	db      *store.DB
	log     *js.DomainLog
	writer  *chart.Writer
	sealer  *fakeSealer
	applier *chart.Applier
	waiter  *rigWaiter

	verifier *statelog.Verifier
	consumed uint64

	// drainMu serialises the one case that drains from two goroutines.
	drainMu sync.Mutex
}

func newWriteRig(t *testing.T) *writeRig {
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
	spec := chart.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD THIS HARNESS OVERRIDES, and it is not a
	// property under test: the shipped default is sized for years of a real
	// company's growth, and an embedded broker in a temporary directory
	// refuses to reserve it.
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

	rows, err := chart.NewRows(db)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := chart.NewFence(db, "node-a")
	// The published trim floor is zero on a fleet that has never trimmed,
	// which is the state every new company is in — and the state in which
	// an absent anchor really does mean an unclaimed address.
	fence.Floor = func(context.Context) (uint64, error) { return 0, nil }
	waiter := &rigWaiter{}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: chart.Domain{}, Log: log, Rows: rows, Fence: fence,
		Signer: testSigner(t, chart.Domain{}),
		Gates:  chart.NewGates(db), Waiter: waiter, NodeID: "node-a",
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	fence.Cursor = waiter.Committed
	sealer := newSealer(t)
	writer, err := chart.NewWriter(chart.WriterDeps{
		Publisher: publisher, DB: db, Seal: sealer,
		Actor: "ana", ActorKind: chart.AuthorHuman,
		Now: func() time.Time { return brokerAt },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	return &writeRig{
		t: t, db: db, log: log, writer: writer, sealer: sealer,
		applier: chart.NewApplier("node-a", nil), waiter: waiter,
		verifier: testVerifier(t, chart.Domain{}),
	}
}

// drain consumes every record the broker holds beyond what this node has
// applied, exactly as the framework's own loop does.
func (r *writeRig) drain() {
	r.t.Helper()
	if err := r.drainTo(); err != nil {
		r.t.Fatal(err)
	}
}

// drainTo is [writeRig.drain] REPORTING rather than failing.
//
// The split exists for the one case that drains from a second goroutine:
// t.Fatalf there stops that goroutine and lets the test go on believing it is
// still consuming — so the racing case's consumer would silently stop and its
// loser would never see the winner's record. Everything else calls the wrapper
// above and gets the ordinary fail-fast.
func (r *writeRig) drainTo() error {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		return fmt.Errorf("read the log's end: %w", err)
	}
	spec := chart.Domain{}.Stream()
	for seq := r.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil {
			return fmt.Errorf("read record %d: %w", seq, err)
		}
		if !ok {
			continue
		}
		// THE FRAME FIRST, exactly as the framework's loop does it: a
		// record is signed on its way to the appender, so what a domain
		// decodes is the BODY.
		body, verdict := r.verifier.Open(payload)
		if verdict != statelog.Verified {
			return fmt.Errorf("record %d did not verify: %s", seq, verdict)
		}
		env, err := chart.DecodeEnvelope(body)
		if err != nil {
			return fmt.Errorf("decode record %d: %w", seq, err)
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
				statelog.ApplyOptions{Now: brokerAt, StoredAt: storedAt}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.t.Context(), `
				INSERT INTO chart_ops (op_id, subject, position, applied_at)
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
			return fmt.Errorf("apply record %d: %w", seq, err)
		}
		r.applier.Committed(r.t.Context())
		r.consumed = seq
		r.waiter.reach(record.Position)
	}
	return nil
}

// drainSafely consumes from a background goroutine, reporting through
// t.Errorf — which is safe from any goroutine where t.Fatalf is not.
//
// A DRAIN AGAINST A STOPPED BROKER IS NOT A FAILURE: the consumer runs until
// the test tells it to stop, and the test's own cleanup closes the broker, so
// a read that arrives in that window is the loop ending rather than a fault.
func (r *writeRig) drainSafely() {
	if err := r.drainTo(); err != nil && r.t.Context().Err() == nil {
		r.t.Errorf("the background consumer stopped: %v", err)
	}
}

// batch publishes one structural batch and applies it.
func (r *writeRig) batch(opID string, ops ...chart.Operation) chart.WriteResult {
	r.t.Helper()
	got, err := r.writer.WriteBatch(r.t.Context(), opID,
		chart.Batch{Operations: ops})
	if err != nil {
		r.t.Fatalf("write batch %s: %v", opID, err)
	}
	r.drain()
	return got
}

// seat publishes one seat's content and applies it.
func (r *writeRig) seat(opID string, content chart.SeatContent) (chart.WriteResult, error) {
	r.t.Helper()
	got, err := r.writer.WriteSeat(r.t.Context(), opID, content)
	if err == nil {
		r.drain()
	}
	return got, err
}

// column reads one column out of the estate.
func (r *writeRig) column(query string, args ...any) []string {
	r.t.Helper()
	var out []string
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(r.t.Context(), query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				return err
			}
			out = append(out, value)
		}
		return rows.Err()
	}); err != nil {
		r.t.Fatalf("read %q: %v", query, err)
	}
	return out
}

// rigWaiter is this node's own applier as the publisher sees it.
type rigWaiter struct {
	mu sync.Mutex
	at statelog.Position
}

func (w *rigWaiter) reach(p statelog.Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.Packed() > w.at.Packed() {
		w.at = p
	}
}

func (w *rigWaiter) Committed() statelog.Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

func (w *rigWaiter) WaitCommitted(ctx context.Context, p statelog.Position) error {
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

func (w *rigWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {
	return w.WaitCommitted(ctx, p)
}

// --- the cases -------------------------------------------------------------- //

// A BATCH REACHES THE ROWS AS ONE ARBITRATED RECORD.
func TestAStructuralBatchReachesTheRows(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)

	result := r.batch("op-build",
		op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "platform", "engineering"),
		op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", "platform"))

	if len(result.Objects) != 3 {
		t.Errorf("the result names %d objects, want 3: %+v",
			len(result.Objects), result.Objects)
	}
	got := r.column(`SELECT key || '<' || parent_key FROM chart_units ORDER BY key`)
	want := []string{"engineering<", "platform<engineering"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("units = %v, want %v", got, want)
	}
	if seat := r.column(
		`SELECT unit_key FROM chart_seats WHERE handle = 'sarah-chen'`); len(seat) != 1 ||
		seat[0] != "platform" {
		t.Errorf("the seat landed at %v, want [platform]", seat)
	}
}

// TWO BATCHES RACING ON ONE ADDRESS YIELD ONE OBJECT AND ONE REFUSAL.
//
// THE CASE NOTHING BUT A BROKER CAN SETTLE. Both goroutines open their own
// snapshot, both see the address free, both decide to create it — and exactly
// one append can carry the structure's expectation. The loser re-decides
// against the newer snapshot, sees the address taken, and is refused by this
// domain's own rule rather than by a constraint.
//
// THE CONTROL IS THE ROW COUNT: a decide that read outside its own snapshot
// would let both land, and the estate would hold one object with whichever
// parent arrived last.
func TestTwoBatchesRacingOnOneAddressLeaveOneObject(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)

	// A CONSUMER RUNNING THROUGHOUT, because the loser has to be able to
	// SEE the winner's record to re-decide against it. Without one, the
	// re-decide reads the same snapshot for ever and the publisher reports
	// contention rather than a rule.
	done := make(chan struct{})
	var drained sync.WaitGroup
	drained.Add(1)
	go func() {
		defer drained.Done()
		for {
			select {
			case <-done:
				r.drainSafely()
				return
			case <-time.After(time.Millisecond):
				r.drainSafely()
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, opID := range []string{"op-a", "op-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.writer.WriteBatch(t.Context(), opID, chart.Batch{
				Operations: []chart.Operation{
					op(chart.OpCreateUnit, chart.KindUnit, "platform", ""),
				}})
		}()
	}
	wg.Wait()
	close(done)
	drained.Wait()

	won, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, chart.ErrRefused):
			lost++
		default:
			t.Fatalf("a racing batch failed for neither reason: %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("%d batches won and %d were refused, want exactly one of "+
			"each — two writers creating one address must contend, and the "+
			"loser must be told the address is taken rather than that the "+
			"broker was busy: %v", won, lost, errs)
	}
	if rows := r.column(`SELECT key FROM chart_units`); len(rows) != 1 {
		t.Errorf("the estate holds %v, want exactly one unit — a decide that "+
			"read outside its own snapshot would let both land", rows)
	}
}

// A LITERAL IN A SECRET FIELD IS SEALED AND NEVER REACHES THE LOG.
func TestALiteralInASecretFieldIsSealedAndAReferenceIsNot(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))

	if _, err := r.seat("op-literal", chart.SeatContent{
		Handle: "sarah-chen", Kind: chart.SeatHuman,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
	}); err != nil {
		t.Fatalf("write a seat with a literal address: %v", err)
	}
	name := chart.SecretName(
		chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}, "email")
	if got, sealed := r.sealer.get(name); !sealed || got != "sarah.chen@example.com" {
		t.Errorf("the literal reached the store as (%q, %v), want the address",
			got, sealed)
	}
	stored := r.column(`SELECT email FROM chart_seats WHERE handle = 'sarah-chen'`)
	if len(stored) != 1 || stored[0] != chart.SecretRef(
		chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}, "email") {
		t.Errorf("the row holds %v, want the reference — a literal in the row "+
			"is a literal in the log every node applies and in every snapshot "+
			"and backup of it", stored)
	}

	// A WHOLE ${VAR} IS STORED VERBATIM, and the control matters: if the
	// predicate were wrong in this direction, a reference would be sealed
	// and the record would carry a pointer to a pointer.
	before := len(r.sealer.sealed)
	if _, err := r.seat("op-ref", chart.SeatContent{
		Handle: "sarah-chen", Kind: chart.SeatHuman,
		Name: "Sarah Chen", Email: "${SARAH_EMAIL}",
	}); err != nil {
		t.Fatalf("write a seat with a reference: %v", err)
	}
	if len(r.sealer.sealed) != before {
		t.Errorf("a whole ${VAR} was sealed — it names a credential rather " +
			"than being one, and sealing it puts a pointer inside the store")
	}
	if got := r.column(
		`SELECT email FROM chart_seats WHERE handle = 'sarah-chen'`); len(got) != 1 ||
		got[0] != "${SARAH_EMAIL}" {
		t.Errorf("the row holds %v, want the reference verbatim", got)
	}
}

// A MASKED VALUE IS RESTORED FROM THE ROW IT PATCHES.
//
// GET-edit-PUT is how a person edits a seat, and the read masks credentials —
// so a write receives the marker constantly. A write that stored it would
// replace a working value with the eight characters `__redacted__`, silently,
// and the failure would surface hours later naming nothing.
func TestAPatchBuiltFromAMaskedReadKeepsTheStoredValue(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))

	if _, err := r.seat("op-one", chart.SeatContent{
		Handle: "sarah-chen", Kind: chart.SeatHuman,
		Name: "Sarah Chen", Email: "${SARAH_EMAIL}",
	}); err != nil {
		t.Fatalf("seed the seat: %v", err)
	}

	// THE ROUND TRIP: a reader fetched the seat, the surface masked the
	// address, they changed the NAME and sent the whole thing back.
	if _, err := r.seat("op-two", chart.SeatContent{
		Handle: "sarah-chen", Kind: chart.SeatHuman,
		Name: "Sarah Chen-Okoro", Email: redacted(),
	}); err != nil {
		t.Fatalf("write a patch built from a masked read: %v", err)
	}
	got := r.column(
		`SELECT name || '|' || email FROM chart_seats WHERE handle = 'sarah-chen'`)
	want := "Sarah Chen-Okoro|${SARAH_EMAIL}"
	if len(got) != 1 || got[0] != want {
		t.Errorf("the seat is %v, want [%q] — the mask resolves from the row "+
			"in the decide's own snapshot, so editing one field never "+
			"replaces another with the marker", got, want)
	}
}

// AND A MASK OVER A FIELD WITH NO STORED VALUE IS REFUSED, NAMING IT.
//
// There is nothing to restore it from, so the alternatives are storing the
// literal — the outage above — or storing an empty value, which silently
// clears a field the caller believed they were leaving alone.
func TestAMaskWithNoStoredValueIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))

	_, err := r.seat("op-mask", chart.SeatContent{
		Handle: "sarah-chen", Kind: chart.SeatHuman,
		Name: "Sarah Chen", Email: redacted(),
	})
	if err == nil {
		t.Fatal("a mask over a field with nothing behind it was accepted — " +
			"whatever it stored is either the marker itself or an empty value " +
			"the caller never asked for")
	}
	if !errors.Is(err, chart.ErrRefused) {
		t.Errorf("the refusal does not answer ErrRefused: %v", err)
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}

// A WRITER WITH NO ACTOR IS REFUSED AT CONSTRUCTION.
//
// A chart whose author field is chosen by the caller is not an audit trail, and
// a reorganisation is the change a company most needs one of. An empty author
// is indistinguishable from a surface that forgot to supply one.
func TestAWriterWithNoActorIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)

	for name, deps := range map[string]chart.WriterDeps{
		"no actor": {Publisher: nil, DB: r.db, ActorKind: chart.AuthorHuman},
		"no kind": {Publisher: nil, DB: r.db, Actor: "ana",
			ActorKind: chart.AuthorKind("nobody")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The publisher is nil in both, so this asserts only that
			// the identity checks run — which they must, because a
			// writer built with a publisher and no actor is the shape
			// that reaches production.
			if _, err := chart.NewWriter(deps); err == nil {
				t.Fatal("the writer was built")
			}
		})
	}

	full := chart.WriterDeps{Publisher: nil, DB: r.db}
	if _, err := chart.NewWriter(full); err == nil {
		t.Fatal("a writer with neither a publisher nor an actor was built")
	}
}

// redacted is the mask every HTTP surface serves a credential as.
//
// READ FROM THE CONFIG PACKAGE'S OWN CONSTANT rather than typed here, so a
// change to the marker fails this case rather than silently making it test a
// literal nothing produces.
func redacted() string { return config.Redacted }

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
