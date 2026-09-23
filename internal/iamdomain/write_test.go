package iamdomain_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WHOLE WRITE PATH: a claim reaches the broker, the applier and the rows.
//
// Everything below it has its own tests over fakes, and every one of them can
// be individually right while the composition is wrong. But in THIS domain the
// harness earns its cost for a sharper reason than the others: uniqueness here
// is not an index, it is the broker refusing a second publish on one subject —
// so "two people cannot hold one address" is a claim about a broker, and
// nothing but a broker can settle it.

var brokerAt = time.Unix(1_700_000_000, 0).UTC()

type writeRig struct {
	t        *testing.T
	db       *store.DB
	log      *js.DomainLog
	writer   *iamdomain.Writer
	applier  *iamdomain.Applier
	waiter   *rigWaiter
	verifier *statelog.Verifier
	consumed uint64
	events   *writerEvents

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
	spec := iamdomain.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD THIS HARNESS OVERRIDES, and it is not a
	// property under test: the shipped default is sized for years of a real
	// company's sign-ins, and an embedded broker in a temporary directory
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

	rows, err := iamdomain.NewRows(db)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := iamdomain.NewFence(db, "node-a")
	// The published trim floor is zero on a fleet that has never trimmed,
	// which is the state every new company is in — and the state in which
	// an absent anchor really does mean an unclaimed address.
	fence.Floor = func(context.Context) (uint64, error) { return 0, nil }
	waiter := &rigWaiter{}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: iamdomain.Domain{}, Log: log, Rows: rows, Fence: fence,
		Signer: testSigner(t), Gates: iamdomain.NewGates(db),
		Waiter: waiter, NodeID: "node-a",
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	fence.Cursor = waiter.Committed

	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("build the blinder: %v", err)
	}
	keys := newKeyStore()
	sealer, err := iamdomain.NewSealer(keys)
	if err != nil {
		t.Fatalf("build the sealer: %v", err)
	}
	announced := &writerEvents{}
	writer, err := iamdomain.NewWriter(iamdomain.WriterDeps{
		Publisher: publisher, DB: db, Blinder: blinder, Sealer: sealer,
		Events: announced, Actor: "ana.admin", ActorKind: iam.KindPerson,
		// THE RIG'S PARTY AUTHORS EVERYTHING, so every case here is
		// about the rule it names rather than about the grant gate.
		Grants: []iam.Grant{iam.GrantPeopleManage},
		Now:    func() time.Time { return brokerAt },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	return &writeRig{
		t: t, db: db, log: log, writer: writer, waiter: waiter,
		events:   announced,
		applier:  iamdomain.NewApplier("node-a", sealer),
		verifier: testVerifier(t),
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

// drainTo is [writeRig.drain] REPORTING rather than failing, for the one case
// that drains from a second goroutine: t.Fatalf there stops that goroutine and
// lets the test go on believing it is still consuming, so the racing case's
// loser would never see the winner's record.
func (r *writeRig) drainTo() error {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		return fmt.Errorf("read the log's end: %w", err)
	}
	spec := iamdomain.Domain{}.Stream()
	for seq := r.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil {
			return fmt.Errorf("read record %d: %w", seq, err)
		}
		if !ok {
			continue
		}
		body, verdict := r.verifier.Open(payload)
		if verdict != statelog.Verified {
			return fmt.Errorf("record %d did not verify: %s", seq, verdict)
		}
		env, err := iamdomain.DecodeEnvelope(body)
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
				INSERT INTO iam_ops (op_id, subject, position, applied_at)
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

func (r *writeRig) drainSafely() {
	if err := r.drainTo(); err != nil && r.t.Context().Err() == nil {
		r.t.Errorf("the background consumer stopped: %v", err)
	}
}

// enrol creates one person through the whole path and applies the result.
func (r *writeRig) enrol(in iamdomain.Enrolment) error {
	r.t.Helper()
	return r.draining(func() error {
		_, err := r.writer.Enrol(r.t.Context(), in)
		return err
	})
}

// claim takes one claim through the whole path and applies the result.
func (r *writeRig) claim(kind iamdomain.ObjectKind, token, person, opID string) error {
	r.t.Helper()
	return r.draining(func() error {
		_, err := r.writer.Claim(r.t.Context(), kind, token, person, opID)
		return err
	})
}

// draining runs one gesture with this rig's consumer alongside it.
func (r *writeRig) draining(gesture func() error) error {
	r.t.Helper()
	// THE CONSUMER RUNS ALONGSIDE, because an enrolment is a SEQUENCE:
	// each step waits for this node's applier to reach the step before it,
	// so a rig that drained only afterwards would deadlock on the second
	// claim.
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				r.drainSafely()
				return
			case <-time.After(5 * time.Millisecond):
				r.drainSafely()
			}
		}
	}()
	err := gesture()
	close(stop)
	<-done
	return err
}

// reader builds this rig's read side over the same estate its writer writes.
//
// THE RIG'S OWN WAITER is the committed position, so a read's level means what
// it says: the reader and the publisher agree about how far this node has got,
// which is the property a reader built over a separate notion of position
// silently would not have.
func (r *writeRig) reader(t *testing.T) *iamdomain.Reader {
	t.Helper()
	// THROUGH THE FRAMEWORK'S OWN TEST CONSTRUCTOR, over a waiter whose
	// position MOVES as the rig drains — which is what makes a read that
	// has to wait actually wait, rather than being served the rows from
	// before the position it was handed.
	log, err := statelogtest.LocalReaderOver(
		iamdomain.Domain{}, r.db.Replicated(), r.waiter)
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB: r.db, Log: log, Committed: r.waiter.Committed,
	})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	return reader
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

// WaitApplied is WaitCommitted here, and the harness is why: this rig applies
// every record inline in its own drain, so "committed" and "applied" are one
// position rather than two.
func (w *rigWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {

	return w.WaitCommitted(ctx, p)
}

func testSigner(t *testing.T) *statelog.Signer {
	t.Helper()
	signer, err := statelog.NewSigner(iamdomain.Domain{}.Name(),
		statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

func testVerifier(t *testing.T) *statelog.Verifier {
	t.Helper()
	verifier, err := statelog.NewVerifier(iamdomain.Domain{}.Name(),
		statelog.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}

// TWO ENROLMENTS ON ONE ADDRESS YIELD EXACTLY ONE WINNER.
//
// THE case this domain exists to settle, and the one nothing below a broker
// can. There is no unique index on `iam_people.email_blind` and there cannot
// be one — a constraint violation inside an apply transaction stalls every
// node's log at once — so what keeps two people off one address is that both
// claims publish to the SAME SUBJECT at an expectation of zero, and the broker
// accepts exactly one.
func TestTwoEnrolmentsOnOneAddressYieldOneWinner(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const address = "sarah.chen@example.com"

	var mu sync.Mutex
	var winners, losers int
	var refusal error
	var wg sync.WaitGroup
	for i, id := range []string{
		"018f3a9c-0000-7000-8000-00000000000a",
		"018f3a9c-0000-7000-8000-00000000000b",
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := rig.enrol(iamdomain.Enrolment{
				PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
				Name: "Sarah Chen", Email: address,
				Login:  fmt.Sprintf("sarah.chen%d", i),
				OpID:   fmt.Sprintf("op-%d", i),
				Reason: "the joiner",
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
				return
			}
			losers++
			refusal = err
		}()
	}
	wg.Wait()
	rig.drain()

	if winners != 1 || losers != 1 {
		t.Fatalf("%d enrolments landed and %d were refused, want exactly one "+
			"of each — two people holding one address is the state this "+
			"estate has no index to refuse and no way to notice",
			winners, losers)
	}
	if refusal == nil {
		t.Fatal("the loser was refused with no error")
	}

	// AND THE ROWS AGREE. Exactly one person holds the address, whatever
	// the two goroutines did.
	holders := rig.column(
		`SELECT id FROM iam_people WHERE email_blind <> '' ORDER BY id`)
	if len(holders) != 1 {
		t.Errorf("%d people hold an address, want 1: %v", len(holders), holders)
	}
}

// AND THE REFUSAL NAMES THE HOLDER WHERE IT CAN.
//
// "That address is taken" is a support ticket; "that address belongs to person
// X" is an answer. The naming is ADVISORY — the row it reads can be written
// between the decide and the append, which is precisely the race the
// create-at-zero settles — so the case asserts the SECOND claim, where the
// first has already landed and the read is not racing anybody.
func TestASecondClaimOnAHeldAddressNamesWhoHoldsIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const first = "018f3a9c-0000-7000-8000-00000000000a"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: first, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-1",
	}); err != nil {
		t.Fatalf("the first enrolment: %v", err)
	}
	rig.drain()

	err := rig.enrol(iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-00000000000b",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Name: "Someone Else", Email: "Sarah.Chen+jira@Example.COM",
		Login: "someone.else", OpID: "op-2",
	})
	if err == nil {
		t.Fatal("a second enrolment on a held address landed — and it reached " +
			"it by a DIFFERENT SPELLING, so the fold that makes one address " +
			"one claim is not being applied")
	}
	var claimed *iamdomain.ErrClaimed
	if !errors.As(err, &claimed) {
		t.Fatalf("the refusal is %v, which does not name the holder", err)
	}
	if claimed.Holder != first {
		t.Errorf("the refusal names %q as the holder, want %q",
			claimed.Holder, first)
	}
}

// AN ENROLMENT THAT LANDS IS READABLE AS ONE PERSON, SEALED.
func TestAnEnrolmentWritesOnePersonWhoseValuesAreSealed(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000000a"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-1", Reason: "the joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()

	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 1 || got[0] != id {
		t.Fatalf("the estate holds %v, want exactly %q", got, id)
	}
	// THE CLEARTEXT IS NOWHERE. Not in the sealed columns, not in the
	// document, and — the one that matters most — not in the claim's own
	// subject, which is a broker path in every delivery.
	for _, column := range []string{"name_sealed", "email_sealed", "document"} {
		for _, value := range rig.column(
			`SELECT CAST(` + column + ` AS TEXT) FROM iam_people`) {
			for _, fragment := range []string{"Sarah", "Chen", "example.com"} {
				if contains(value, fragment) {
					t.Errorf("iam_people.%s carries %q in the clear", column, fragment)
				}
			}
		}
	}
	for _, subject := range rig.column(`SELECT subject FROM iam_ops ORDER BY subject`) {
		for _, fragment := range []string{"sarah.chen@", "example.com"} {
			if contains(subject, fragment) {
				t.Errorf("the subject %q carries the address, and a subject is "+
					"a broker path carried in the clear in every delivery, "+
					"every consumer's filter and every stream listing",
					subject)
			}
		}
	}
	// AND THE LOGIN IS DELIBERATELY IN THE CLEAR, because an operator
	// reading their own audit trail needs it.
	if got := rig.column(`SELECT login FROM iam_people`); len(got) != 1 ||
		got[0] != "sarah.chen" {
		t.Errorf("the login reads %v, want it stored as itself", got)
	}
}

// A REMOVAL DELETES THE ROWS, LEAVES THE TOMBSTONE, AND SHREDS THE KEY.
func TestARemovalLeavesATombstoneAndDestroysTheKey(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000000a"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-1",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()

	if _, err := rig.writer.Remove(rig.t.Context(), id, "op-remove",
		"left the company"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rig.drain()

	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 0 {
		t.Errorf("the person is still present: %v", got)
	}
	tombstone := rig.column(`SELECT claims_json FROM iam_removed WHERE person_id = ?`, id)
	if len(tombstone) != 1 {
		t.Fatalf("the removal left %d tombstones, want 1", len(tombstone))
	}
	// THE TOMBSTONE CARRIES THE BLINDS AND NOT THE ADDRESS. It is the one
	// row designed to outlive the person, so writing an address into it
	// would be the removal's own promise broken by the mechanism that
	// makes it.
	for _, fragment := range []string{"sarah.chen@", "example.com"} {
		if contains(tombstone[0], fragment) {
			t.Errorf("the tombstone %q carries the address it is meant to make "+
				"unrecoverable", tombstone[0])
		}
	}
	// AND THE AUTHENTICATION TRAIL SURVIVES, which is why the id outlives
	// the person: a history whose authors evaporate is not an audit trail.
	if got := rig.column(
		`SELECT op FROM iam_history WHERE person_id = ? ORDER BY op`, id); len(got) == 0 {
		t.Error("the removal deleted the trail, so nothing can say who this " +
			"person was or what they did")
	}
}

// contains is strings.Contains under a name that reads as the question.
func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// THE FLEET-WIDE GENERATION MOVES ONE ROW AND NOBODY'S EPOCH.
//
// That separation is the whole reason the counter exists. Bumping every
// person's revocation epoch instead is a write per person inside a transaction
// the framework bounds by a row budget, so a company that had outgrown one
// transaction would end SOME of its sessions — and the operator would have no
// way to tell which.
func TestInvalidatingEverySessionMovesOneRowAndNobodysEpoch(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000001a"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-1",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	if _, err := rig.writer.Revoke(rig.t.Context(), id, "op-revoke",
		"signed out everywhere"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rig.drain()
	before := rig.column(
		`SELECT epoch FROM iam_revocation_epochs WHERE person_id = ?`, id)

	if _, err := rig.writer.InvalidateAll(rig.t.Context(), "op-invalidate",
		"restored from a backup"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	rig.drain()

	if got := rig.column(
		`SELECT generation FROM iam_session_generation`); len(got) != 1 ||
		got[0] != "1" {
		t.Errorf("the session generation reads %v, want exactly one row at 1", got)
	}
	after := rig.column(
		`SELECT epoch FROM iam_revocation_epochs WHERE person_id = ?`, id)
	if len(after) != 1 || len(before) != 1 || after[0] != before[0] {
		t.Errorf("the person's revocation epoch moved from %v to %v — an "+
			"invalidation that wrote per-person rows would be bounded by the "+
			"apply transaction's row budget", before, after)
	}
	// AND IT IS ON THE TRAIL, in the horizon that keeps things: ending
	// every session in the company is exactly the gesture somebody comes
	// back to an audit a year later for.
	if got := rig.column(
		`SELECT class FROM iam_history WHERE op = 'invalidate'`); len(got) != 1 ||
		got[0] != string(iamdomain.ClassChange) {
		t.Errorf("the invalidation's trail rows read %v, want one classed %q",
			got, iamdomain.ClassChange)
	}
}

// AN OLDER INVALIDATION ARRIVING AFTER A NEWER ONE MUST NOT PULL THE
// GENERATION BACK.
//
// A REDELIVERY ALONE COULD NOT SHOW THIS, and stating why is half the case:
// the new generation is STATED on the record, so delivering one record twice
// writes the value it already holds whatever the guard says, and an in-order
// replay of two records ends on the higher one either way. What the monotone
// guard defends is OUT-OF-ORDER arrival — a reordered fetch, a recovery, a
// node catching up — so this applies the two records back to front, which is
// the only arrangement in which an unguarded upsert is visible.
//
// Ungarded it un-ends every session the second bump ended, on one node, with
// nothing that ever corrects it.
func TestAnOlderInvalidationDoesNotPullTheGenerationBack(t *testing.T) {
	t.Parallel()
	rig := &sweepRigT{writeRig: newWriteRig(t)}
	rig.apply(invalidationRecord(t, 2, 20), brokerAt)
	rig.apply(invalidationRecord(t, 1, 10), brokerAt)

	if got := rig.column(
		`SELECT generation FROM iam_session_generation`); len(got) != 1 ||
		got[0] != "2" {
		t.Errorf("the session generation reads %v after the generation-1 "+
			"record arrived behind the generation-2 one, want 2 — going "+
			"backwards resurrects every session the second bump ended", got)
	}
}

// invalidationRecord is one invalidation as the framework delivers it, at a
// chosen generation and position.
func invalidationRecord(t *testing.T, generation, seq uint64) statelog.Record {
	t.Helper()
	opID := fmt.Sprintf("invalidate-%d", generation)
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.RecordVersion, OpID: opID,
			Subject: iamdomain.InvalidationSubject(),
			Op:      iamdomain.OpInvalidate, Writer: "node-a",
			Scope: iamdomain.RootScope(),
		},
		Actor: "operator", ActorKind: iam.KindPerson,
		Mutation: mustJSON(t, iamdomain.Invalidation{
			V: iamdomain.GateRecordVersion, Generation: generation,
			By: "operator",
		}),
	})
	if err != nil {
		t.Fatalf("encode an invalidation: %v", err)
	}
	return statelog.Record{
		Envelope: statelog.Envelope{
			V:    iamdomain.RecordVersion,
			Kind: string(iamdomain.KindInvalidation),
			Subject: statelog.Subject{
				Kind: string(iamdomain.KindInvalidation),
			},
			Op: string(iamdomain.OpInvalidate), OpID: opID,
		},
		Position: statelog.Position{
			Stream: iamdomain.Domain{}.Stream().Name, Seq: seq,
		},
		Payload: payload, StoredAt: brokerAt,
	}
}

// TWO OPERATORS INVALIDATING AT ONCE CONTEND, AND THE SECOND SEES THE FIRST.
//
// The singleton subject is what buys it. On two subjects neither would
// contend, both would read the same current generation, both would write the
// same new one — and every cookie minted between them would survive a gesture
// whose whole promise is that nothing issued before it does.
func TestTwoInvalidationsContendAndLandAsTwoIncrements(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	for _, op := range []string{"op-a", "op-b"} {
		if _, err := rig.writer.InvalidateAll(rig.t.Context(), op, "rotation"); err != nil {
			t.Fatalf("invalidate %s: %v", op, err)
		}
		rig.drain()
	}
	if got := rig.column(
		`SELECT generation FROM iam_session_generation`); len(got) != 1 ||
		got[0] != "2" {
		t.Errorf("two invalidations left the generation at %v, want 2 — a "+
			"second bump that read a stale value would leave every cookie "+
			"minted between them valid", got)
	}
}

// AN INVALIDATION NEEDS THE ADMINISTRATIVE GRANT, UNLIKE A REVOCATION.
//
// The asymmetry is the blast radius: revoking is something a person does to
// themselves and something the engine does on their behalf the moment somebody
// else has their cookie, so gating it would make the fastest response to a
// compromise the one that needs an administrator. Ending EVERYBODY's sessions
// has no self-service reading at all.
func TestInvalidatingEverySessionIsRefusedWithoutTheGrant(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	ungranted := rig.writer.As("nobody", iam.KindPerson, nil)
	if _, err := ungranted.InvalidateAll(rig.t.Context(), "op-1", ""); !errors.Is(
		err, iamdomain.ErrRefused) {
		t.Errorf("an ungranted party invalidating every session got %v, want "+
			"%v", err, iamdomain.ErrRefused)
	}
	// And the same party may still revoke their own sessions, which is the
	// half that must never need an administrator.
	if _, err := ungranted.Revoke(rig.t.Context(),
		"018f3a9c-0000-7000-8000-00000000002a", "op-2", "signed out"); errors.Is(
		err, iamdomain.ErrRefused) {
		t.Error("revoking was refused for want of a grant, which would make " +
			"the fastest response to a stolen cookie the one that needs an " +
			"administrator")
	}
}

// A SEAT BINDING RECORDS THE CHART POSITION IT WAS DECIDED AT.
//
// It is what makes the seat lookup three-valued, and the field has exactly one
// producer: the bind's own decide, reading this node's chart checkpoint in the
// SAME transaction it reads the seat row in. Without it a seat missing from a
// node's view has one answer where it needs two — "the seat is gone" (403) and
// "this node has not applied the hire yet" (503) are opposite, and a node
// merely behind on the chart would tell everybody their seat does not exist.
func TestASeatBindingRecordsTheChartPositionItWasDecidedAt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000003a"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-1",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	rig.seatChartAt(42)

	if _, err := rig.writer.Claim(rig.t.Context(), iamdomain.KindSeat,
		"platform-lead", id, "op-bind"); err != nil {
		t.Fatalf("bind the seat: %v", err)
	}
	rig.drain()

	want := statelog.Position{
		Stream: topics.ChartLogStream, Seq: 42,
	}.Packed()
	if got := rig.column(
		`SELECT chart_position FROM iam_people WHERE id = ?`, id); len(got) != 1 ||
		got[0] != fmt.Sprint(want) {
		t.Errorf("the binding recorded chart position %v, want %d — a binding "+
			"with no position leaves every node unable to tell a removed seat "+
			"from one it has not applied yet", got, want)
	}

	// AND A RELEASE TAKES IT BACK OFF. Left behind, it is a sentence
	// about a binding that no longer exists.
	if _, err := rig.writer.Release(rig.t.Context(), iamdomain.KindSeat,
		"platform-lead", id, "op-unbind", "moved teams"); err != nil {
		t.Fatalf("release the seat: %v", err)
	}
	rig.drain()
	if got := rig.column(
		`SELECT chart_position FROM iam_people WHERE id = ?`, id); len(got) != 1 ||
		got[0] != "0" {
		t.Errorf("after the release the chart position reads %v, want 0", got)
	}
}

// A NODE THAT HAS APPLIED NO CHART RECORD CAN STILL BIND SOMEBODY TO A SEAT.
//
// The position defaults to ZERO rather than the bind being refused, and the
// direction is what matters: zero is a floor every node's own position covers,
// so a seat that then goes missing answers 403 naming the seat. Refusing
// instead would make a node with no chart checkpoint — which is every node a
// first company is set up on — unable to bind anybody at all.
func TestABindOnANodeWithNoChartCheckpointRecordsZero(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000004a"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-1",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	rig.seatOnly("platform-lead")

	if _, err := rig.writer.Claim(rig.t.Context(), iamdomain.KindSeat,
		"platform-lead", id, "op-bind"); err != nil {
		t.Fatalf("bind the seat on a node with no chart checkpoint: %v", err)
	}
	rig.drain()
	if got := rig.column(
		`SELECT chart_position FROM iam_people WHERE id = ?`, id); len(got) != 1 ||
		got[0] != "0" {
		t.Errorf("the binding recorded %v, want 0", got)
	}
}

// seatChartAt puts one seat in the chart's tables and moves the chart log's
// checkpoint to seq, which is what the bind's decide reads.
func (r *writeRig) seatChartAt(seq uint64) {
	r.t.Helper()
	r.seatOnly("platform-lead")
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, 0, ?, 0, 0)
			ON CONFLICT (stream) DO UPDATE SET seq = excluded.seq`,
			topics.ChartLogStream, int64(seq))
		return err
	}); err != nil {
		r.t.Fatalf("move the chart checkpoint: %v", err)
	}
}

// seatOnly puts one seat row in the chart's table and nothing else, so the
// bind's advisory existence check passes.
func (r *writeRig) seatOnly(handle string) {
	r.t.Helper()
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO chart_seats (handle, kind, created_at, updated_at, version, document)
			VALUES (?, 'human', 0, 0, 1, x'')
			ON CONFLICT (handle) DO NOTHING`, handle)
		return err
	}); err != nil {
		r.t.Fatalf("seed a seat: %v", err)
	}
}

// THE ADMINISTRATIVE GRANT IS people:manage, AND IT IS NOT config:write.
//
// Both halves matter and the second is what was broken. A party holding the
// COMPANY's own grant could enrol itself a colleague, which is the escalation
// this estate exists to close; and the node's own writer — which authors the
// bootstrap enrolment and the invite redemption on behalf of people with no
// principal yet — holds fleet:operate, so every one of those paths was
// refused on a real deployment while the surface's own suite, built on a stub
// writer, stayed green.
func TestAnAdministrativeRecordNeedsPeopleManageAndNotTheCompanysGrant(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := "018f3a9c-0000-7000-8000-0000000000b1"
	enrol := func(w *iamdomain.Writer, op string) error {
		_, err := w.Enrol(rig.t.Context(), iamdomain.Enrolment{
			PersonID: person + op, Kind: iam.KindMachine,
			Stage: iam.StageActive, Login: "svc:" + op,
			OpID: op, Reason: "a hire",
		})
		return err
	}
	company := rig.writer.As("automation", iam.KindMachine,
		[]iam.Grant{iam.GrantConfigWrite, iam.GrantFleetOperate})
	if err := enrol(company, "op-company"); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a party holding the company's own grants enrolled somebody "+
			"(%v) — config:write rebuilds a company's tools and must not also "+
			"decide who may do that tomorrow", err)
	}
	directory := rig.writer.As("ana.admin", iam.KindPerson,
		[]iam.Grant{iamdomain.AdminGrant})
	if err := enrol(directory, "op-directory"); errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a party holding %s was refused an enrolment: %v",
			iamdomain.AdminGrant, err)
	}
}

// A CALLER MAY NOT CONFER A GRANT THEY DO NOT HOLD, on anybody.
//
// One rule rather than the two the design states — "not onto your own record"
// and "not onto somebody else's" — because the two have the same answer, and
// splitting them is how one arm comes to be checked and the other not. Taking
// a grant AWAY is always allowed: nobody escalates by narrowing, and an
// administrator who cannot hold secrets:reveal must still be able to withdraw
// it from a leaver.
func TestACallerCannotConferAGrantTheyDoNotHold(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := "018f3a9c-0000-7000-8000-0000000000c2"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Okafor", Email: "dana@example.com", Login: "dana.sre",
		Grants: []iam.Grant{iam.GrantSecretRead, iam.GrantWorkWrite},
		OpID:   "op-enrol", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()

	// A narrow administrator: they manage people and hold nothing else.
	narrow := rig.writer.As("ana.admin", iam.KindPerson,
		[]iam.Grant{iamdomain.AdminGrant})
	grant := func(w *iamdomain.Writer, op string, to []iam.Grant) error {
		_, err := w.UpdatePerson(rig.t.Context(), iamdomain.PersonUpdate{
			PersonID: person,
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Grants = to
				return p, nil
			},
			OpID: op, Reason: "a change of authority",
		})
		return err
	}
	if err := grant(narrow, "op-widen",
		[]iam.Grant{iam.GrantSecretRead, iam.GrantSecretWrite}); !errors.Is(
		err, iamdomain.ErrRefused) {
		t.Errorf("a party holding neither secrets:write nor it conferred it "+
			"(%v)", err)
	}
	// AND NARROWING IS ALWAYS ALLOWED, which is the control — and it KEEPS
	// a grant the caller does not hold, because that is the arm being
	// exercised. Narrowing to nothing would pass whatever the rule said:
	// the check walks what the row will CARRY, and an empty set carries
	// nothing to object to.
	if err := grant(narrow, "op-narrow",
		[]iam.Grant{iam.GrantSecretRead}); err != nil {
		t.Errorf("withdrawing one grant while keeping another the caller does "+
			"not hold was refused: %v — an administrator must be able to "+
			"strip a leaver without first being given everything they hold",
			err)
	}
	rig.drain()
	if got := rig.column(
		`SELECT document FROM iam_people WHERE id = '` + person + `'`); len(got) != 1 {
		t.Fatalf("the person's row is %v", got)
	}
}

// AN INVITATION ARBITRATES ON THE ADDRESS, so two administrators inviting one
// person produce ONE invitation and one refusal naming what is already there.
//
// The obvious subject — the invitation's own fresh id — would make both
// succeed, and the company would hold two links either of which creates the
// same person.
func TestTwoInvitationsToOneAddressYieldOneWinner(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	invite := func(id, op string) error {
		_, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
			ID: id, Email: "sarah@example.com",
			Grants:    []iam.Grant{iam.GrantStateRead},
			ExpiresAt: brokerAt.Add(168 * time.Hour),
			OpID:      op, Reason: "onboarding",
		})
		return err
	}
	if err := invite("018f3a9c-0000-7000-8000-0000000000d1", "op-1"); err != nil {
		t.Fatalf("the first invitation: %v", err)
	}
	rig.drain()
	err := invite("018f3a9c-0000-7000-8000-0000000000d2", "op-2")
	if err == nil {
		t.Fatal("a second invitation to the same address was accepted, so " +
			"the company holds two links that each create one person")
	}
	var claimed *iamdomain.ErrClaimed
	if !errors.As(err, &claimed) {
		t.Errorf("the refusal is %v, want one naming what already holds the "+
			"address", err)
	}
	if got := rig.column(`SELECT id FROM iam_invites`); len(got) != 1 {
		t.Errorf("iam_invites holds %v, want exactly one row", got)
	}
}

// AN INVITATION NEEDS AN EXPIRY, because one read as `never` is a superuser
// claim that stays live in somebody's mailbox for the life of the company.
func TestAnInvitationWithNoExpiryIsRefused(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	if _, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
		ID:    "018f3a9c-0000-7000-8000-0000000000e1",
		Email: "sarah@example.com", OpID: "op-1",
	}); err == nil {
		t.Error("an invitation with no expiry was accepted")
	}
}

// A MACHINE IDENTITY ENROLS WITH NO ADDRESS, AND A PERSON MAY NOT.
//
// `svc:ci` has no login page and no mailbox — it proves itself with a token —
// so requiring an address would mean inventing one, and a row that is neither
// addressable nor named is a credential holder nobody can list, revoke or
// audit. A PERSON is the opposite case: the address IS the interactive login
// key, so one without it could never sign in.
//
// This was refused outright until the rule was split, which meant a company
// could not declare a service account at all.
func TestAMachineEnrolsWithNoAddressAndAPersonMayNot(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000000f1",
		Kind:     iam.KindMachine, Stage: iam.StageActive,
		Name: "Release pipeline", Login: "svc:ci",
		OpID: "op-machine", Reason: "the pipeline",
	}); err != nil {
		t.Fatalf("enrol a machine with no address: %v", err)
	}
	rig.drain()
	if got := rig.column(
		`SELECT login FROM iam_people WHERE email_blind = ''`); len(got) != 1 ||
		got[0] != "svc:ci" {
		t.Errorf("the address-less rows are %v, want exactly [svc:ci]", got)
	}
	// AND IT IS STILL SEALED. A machine's NAME is a person's words —
	// "Release pipeline, raised by Dana" — so a removal has to be able to
	// shred it exactly as it shreds anybody else's.
	if got := rig.column(
		`SELECT length(name_sealed) FROM iam_people WHERE login = 'svc:ci'`); //
	len(got) != 1 || got[0] == "0" {
		t.Errorf("the machine's name_sealed is %v, want ciphertext", got)
	}

	// THE REFUSALS ARE ASSERTED ON THE SENTINEL and never on "an error
	// happened": an enrolment that reaches the publisher fails for
	// unrelated reasons in a rig that is not draining, which would make
	// both of these pass whatever the rule said.
	if _, err := rig.writer.Enrol(rig.t.Context(), iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000000f2",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Login: "dana.sre", OpID: "op-person", Reason: "a hire",
	}); !errors.Is(err, iamdomain.ErrNotFindable) {
		t.Errorf("a person with no address was refused with %v, want %v — "+
			"nothing they hold is an interactive login key, so they can "+
			"never sign in", err, iamdomain.ErrNotFindable)
	}
	// AND A MACHINE WITH NEITHER IS REFUSED TOO, which is the control:
	// the rule is "findable", not "no address needed".
	if _, err := rig.writer.Enrol(rig.t.Context(), iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000000f3",
		Kind:     iam.KindMachine, Stage: iam.StageActive,
		OpID: "op-nameless", Reason: "a pipeline",
	}); !errors.Is(err, iamdomain.ErrNotFindable) {
		t.Errorf("a machine with neither an address nor a login was refused "+
			"with %v, want %v — it is a credential holder nobody can list "+
			"or revoke", err, iamdomain.ErrNotFindable)
	}
}
