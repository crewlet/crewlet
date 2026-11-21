package iamdomain_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WHOLE WRITE PATH: a directory record reaches the broker, the applier and
// the rows. Uniqueness here is no index but two writes on the directory subject
// contending at the broker, the loser deciding again from rows that hold the
// winner, so only a real broker can settle it.

var brokerAt = time.Unix(1_700_000_000, 0).UTC()

type writeRig struct {
	t        *testing.T
	db       store.ReplicatedHandle
	log      *js.DomainLog
	writer   *iamdomain.Writer
	applier  *iamdomain.Applier
	waiter   *rigWaiter
	verifier *recordseal.Verifier
	consumed uint64
	events   *writerEvents

	// directory counts the applier's post-commit moves that said a seat's
	// standing may have moved, and moved keeps every move it handed over
	// since the last [writeRig.takeMoved].
	directory atomic.Int64
	movedMu   sync.Mutex
	moved     []iamdomain.Moved

	// sealer seals and opens somebody's values over [testKeyring], so a case
	// can open what a row carries.
	sealer *iamdomain.Sealer

	// publisher is the rig's one, for a case that needs a second writer
	// over the same log with a different seam.
	publisher *statelog.Publisher

	// seats is the running company's seats, by handle and kind, that the rig's
	// writers check a seat bind against.
	seats *rigSeats

	// node is the rig's NODE writer, built as `internal/engine` builds a node's
	// own (see [nodeWriter]); nodeDeps lets a case build another at its own
	// clock rather than move this one's under a case running beside it.
	node     *iamdomain.Writer
	nodeDeps iamdomain.WriterDeps

	drainMu sync.Mutex
}

// principalNamed is a principal of kind called name (its login, or a seat's
// handle), holding grants, with an id derived from the name so two cases naming
// one party name one principal.
func principalNamed(name string, kind iam.Kind, grants []iam.Grant) iam.Principal {
	p := iam.Principal{
		ID:   uuid.NewSHA1(uuid.NameSpaceOID, []byte("principal:"+name)),
		Kind: kind, Stage: iam.StageActive, Grants: grants,
	}
	if kind == iam.KindSeat {
		p.Seat = name
	} else {
		p.Login = name
	}
	return p
}

func newWriteRig(t *testing.T) *writeRig {
	t.Helper()
	return newWriteRigWith(t, nil)
}

// newWriteRigWith is [newWriteRig] with the publisher's appender wrapped — how
// a case puts a broker that answers nothing in front of one subject.
func newWriteRigWith(t *testing.T,
	wrap func(statelog.Appender) statelog.Appender) *writeRig {

	t.Helper()
	// THE BROKER DECLARES ITS SIZE (brokertest): left to derive it, it takes
	// three quarters of the runner's free disk and could refuse even this log's
	// 64 MiB.
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
	spec := iamdomain.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD OVERRIDDEN: Tier A's floor for
	// stream.iam_log_max_bytes, the smallest log any node runs and the one the
	// gate reserve takes the largest share of, so every write is admitted
	// against the tightest production ceiling.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: config.IamLogMaxBytesFloor,
		MaxMessageBytes: spec.MaxAppendBytes(), Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	db := openNodeEstate(t, "the store")

	rows, err := iamdomain.NewRows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := iamdomain.NewFence(db.Reader(), "node-a")
	// A trim floor of zero is a fleet that never trimmed, every new company's
	// state, where an absent anchor really is an unclaimed address. The log's
	// first sequence (the fence's other bound) and last (proof this node is on
	// this log) are read from the stream as the engine reads them.
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	// AT THE STREAM'S ORIGIN, as the framework's applier starts: a waiter at
	// the zero position names no stream, so a gesture's second step fails
	// comparing against it, which a case asserting only "not refused" reads as
	// success.
	waiter := &rigWaiter{at: statelog.Position{
		Stream: iamdomain.Domain{}.Stream().Name}}
	fence.Committed = waiter.Committed
	var appender statelog.Appender = log
	if wrap != nil {
		appender = wrap(log)
	}
	// THE LOG'S GATE RESERVE, reading usage from the stream as the engine does:
	// this log claims identity, and the framework refuses a publisher over one
	// without it.
	reserve, err := statelog.NewReserve(spec,
		func(ctx context.Context) (statelog.Usage, error) {
			stats, err := log.Stats(ctx)
			return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
		})
	if err != nil {
		t.Fatalf("build the gate reserve: %v", err)
	}
	verifier := testVerifier(t)
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: iamdomain.Domain{}, Spec: spec, Log: appender, Rows: rows,
		Fence: fence, Signer: testSigner(t),
		// THE SAME LOG READ BY POSITION through this node's verifier, as the
		// engine hands it: a resolution asks a record it did not put there who
		// wrote it.
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Gates:   iamdomain.NewGates(db.Reader()),
		Waiter:  waiter, Voids: waiter, Identity: waiter, Admission: reserve,
		NodeID:     "node-a",
		Generation: func() uint32 { return 0 },
		// THE PRODUCTION BUDGET, not a shorter one: this rig's polling applier
		// re-reads every bucketed row around each record and, under -race with
		// the suite in parallel, falls seconds behind, so a shorter budget
		// refuses a write `behind` for no case's reason.
		ResolveBudget: statelog.DefaultResolveBudget,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}

	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatalf("build the blinder: %v", err)
	}
	sealer, err := iamdomain.NewSealer(testCipher(t, testKeyring))
	if err != nil {
		t.Fatalf("build the sealer: %v", err)
	}
	announced := &writerEvents{}
	seats := &rigSeats{}
	writer, err := iamdomain.NewWriter(iamdomain.WriterDeps{
		Publisher: publisher, DB: db.Reader(), Blinds: blinder, Sealer: sealer,
		Seats:  seats,
		Events: announced, Actor: "ana.admin", ActorKind: iam.KindPerson,
		// THE RIG'S PARTY HOLDS EVERY PERMISSION, since an enrolment confers
		// only what its writer holds, so each case is about its own rule; a
		// case about the permission gate narrows it with [iamdomain.Writer.As].
		Grants: iam.AllGrants,
		Now:    func() time.Time { return brokerAt },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	// THE NODE'S OWN WRITER, from NewWriter as the engine builds it, never
	// derived with As: a party derived from a principal carries its credential,
	// and the node acts on nobody's.
	nodeDeps := iamdomain.WriterDeps{
		Publisher: publisher, DB: db.Reader(), Blinds: blinder, Sealer: sealer,
		Seats:  seats,
		Events: announced, Actor: "node-a", ActorKind: iamdomain.NodeKind,
		// AS THE ENGINE GIVES IT: admin, and nothing a row could add.
		Grants: []iam.Grant{iam.GrantAdmin},
		Now:    func() time.Time { return brokerAt },
	}
	node, err := iamdomain.NewWriter(nodeDeps)
	if err != nil {
		t.Fatalf("build the node's writer: %v", err)
	}
	rig := &writeRig{
		t: t, db: db, log: log, writer: writer, waiter: waiter,
		events: announced, seats: seats,
		verifier: verifier,
		sealer:   sealer, publisher: publisher, node: node, nodeDeps: nodeDeps,
	}
	// WHAT EACH COMMITTED BATCH MOVED IS KEPT, so a case can say which records
	// told this node its seats' standing may have moved, and whose credentials.
	rig.applier = iamdomain.NewApplier("node-a", func(m iamdomain.Moved) {
		if m.Seats {
			rig.directory.Add(1)
		}
		rig.movedMu.Lock()
		defer rig.movedMu.Unlock()
		rig.moved = append(rig.moved, m)
	})
	return rig
}

// takeMoved is every move the applier handed over since the last call, folded
// into one, and forgets them.
func (r *writeRig) takeMoved() iamdomain.Moved {
	r.movedMu.Lock()
	defer r.movedMu.Unlock()
	var out iamdomain.Moved
	for _, m := range r.moved {
		out.Seats = out.Seats || m.Seats
		out.Everyone = out.Everyone || m.Everyone
		out.People = append(out.People, m.People...)
		out.Logins = append(out.Logins, m.Logins...)
		out.Sessions = append(out.Sessions, m.Sessions...)
		out.Removed = append(out.Removed, m.Removed...)
	}
	r.moved = nil
	return out
}

// drain consumes every record the broker holds beyond what this node has
// applied, exactly as the framework's own loop does.
func (r *writeRig) drain() {
	r.t.Helper()
	if err := r.drainTo(); err != nil {
		r.t.Fatal(err)
	}
}

// drainTo is [writeRig.drain] reporting rather than failing, for the case that
// drains from a second goroutine: t.Fatalf there stops only that goroutine, and
// the racing loser would never see the winner's record.
func (r *writeRig) drainTo() error {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	consumed, err := applyLog(r.t, r.log, r.verifier, r.db, r.applier,
		r.consumed, brokerAt, r.waiter.reach)
	r.consumed = consumed
	return err
}

// applyLog applies every record past `from` into one node's estate, as the
// framework's loop does, and reports how far it got. A function, not a rig
// method, because a determinism case applies the SAME log into a second node's
// OWN store; `now` is that node's batch clock.
func applyLog(t *testing.T, log *js.DomainLog, verifier *recordseal.Verifier,
	db store.ReplicatedHandle, applier *iamdomain.Applier, from uint64, now time.Time,
	reached func(statelog.Position)) (uint64, error) {

	last, err := log.End(t.Context())
	if err != nil {
		return from, fmt.Errorf("read the log's end: %w", err)
	}
	spec := iamdomain.Domain{}.Stream()
	consumed := from
	for seq := from + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := log.At(t.Context(), seq)
		if err != nil {
			return consumed, fmt.Errorf("read record %d: %w", seq, err)
		}
		if !ok {
			continue
		}
		body, verdict := verifier.Open(payload)
		if verdict != recordseal.Verified {
			return consumed, fmt.Errorf("record %d did not verify: %s", seq, verdict)
		}
		env, err := iamdomain.DecodeEnvelope(body)
		if err != nil {
			return consumed, fmt.Errorf("decode record %d: %w", seq, err)
		}
		// AS THE FRAMEWORK'S LOOP HANDS IT ([statelog.AppliedInstant]).
		storedAt = statelog.AppliedInstant(storedAt)
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
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			reason, gated, err := applier.Gated(t.Context(), tx, record)
			if err != nil {
				return err
			}
			if gated {
				t.Logf("record %d gated: %s", seq, reason)
				return nil
			}
			before, err := bucketedRows(t.Context(), tx)
			if err != nil {
				return err
			}
			if _, err := applier.Apply(t.Context(), tx, record,
				statelog.ApplyOptions{Now: now, StoredAt: storedAt}); err != nil {
				return err
			}
			after, err := bucketedRows(t.Context(), tx)
			if err != nil {
				return err
			}
			if err := withinScope(env, before, after); err != nil {
				return fmt.Errorf("record %d: %w", seq, err)
			}
			// THE LEDGER ROW THE FRAMEWORK'S RUNNER WRITES, column for column:
			// the subject under the log's prefix (the publisher compares a held
			// operation's against it before answering a retry) and the record's
			// broker instant beside this node's.
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO iam_ops
					(op_id, subject, position, applied_at, stored_at)
				VALUES (?,?,?,?,?) ON CONFLICT (op_id) DO NOTHING`,
				env.OpID, spec.SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed(), store.EncodeTime(now),
				store.EncodeTime(storedAt)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO statelog_anchor (stream, subject, anchor)
				VALUES (?,?,?)
				ON CONFLICT (stream, subject) DO UPDATE SET
					anchor = MAX(anchor, excluded.anchor)`,
				record.Position.Stream,
				spec.SubjectPrefix+"."+env.Subject.String(),
				record.Position.Packed()); err != nil {
				return err
			}
			_, err = tx.ExecContext(t.Context(), `
				INSERT INTO statelog_cursor
					(stream, generation, seq, stream_created_at, updated_at)
				VALUES (?,?,?,0,0)
				ON CONFLICT (stream) DO UPDATE SET
					generation = excluded.generation, seq = excluded.seq`,
				record.Position.Stream, int64(record.Position.Generation),
				int64(record.Position.Seq))
			return err
		}); err != nil {
			return consumed, fmt.Errorf("apply record %d: %w", seq, err)
		}
		applier.Committed(t.Context())
		consumed = seq
		if reached != nil {
			reached(record.Position)
		}
	}
	return consumed, nil
}

// EVERY ROW AN APPLY WRITES IS IN A BUCKET ITS RECORD DECLARED.
//
// A record's scope is every bucket its apply may write, and what a node holding
// back an EARLIER record in one of them holds it back by. An apply writing
// outside it runs ahead of that record, whose reprocessing overwrites the later
// work on that node alone (a removal's erasure undone, a spent invitation
// written back unspent), so its rows differ from every peer's for good. So
// [withinScope] holds EVERY record every case here applies to it.

// bucketedRows is every row of every identity table that carries a bucket, as
// the row's whole content keyed to its bucket.
func bucketedRows(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	tables, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name LIKE 'iam\_%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			_ = tables.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err := tables.Close(); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, table := range names {
		if err := bucketedIn(ctx, tx, table, out); err != nil {
			return nil, fmt.Errorf("read %s: %w", table, err)
		}
	}
	return out, nil
}

// bucketedIn adds one table's rows to out, when the table carries a bucket.
func bucketedIn(ctx context.Context, tx *sql.Tx, table string, out map[string]int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	at := slices.Index(columns, "bucket")
	if at < 0 {
		return nil
	}
	for rows.Next() {
		cells := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		var key strings.Builder
		key.WriteString(table)
		for _, cell := range cells {
			fmt.Fprintf(&key, "\x00%T:%v", cell, cell)
		}
		bucket, ok := cells[at].(int64)
		if !ok {
			return fmt.Errorf("a bucket of %T", cells[at])
		}
		out[key.String()] = bucket
	}
	return rows.Err()
}

// withinScope refuses an apply that changed a row in a bucket its record did not
// declare.
func withinScope(env iamdomain.RecordEnvelope, before, after map[string]int64) error {
	if env.Scope.Root || env.Subject.Kind.RootScoped() {
		return nil
	}
	check := func(rows, other map[string]int64, what string) error {
		for key, bucket := range rows {
			if _, same := other[key]; same {
				continue
			}
			if !slices.Contains(env.Scope.Buckets, iamdomain.Bucket(bucket)) {
				table, _, _ := strings.Cut(key, "\x00")
				return fmt.Errorf("the %s on %s %s a row of %s in bucket %d, and "+
					"its scope declares %v — a node holding back an earlier "+
					"record in that bucket applies this one ahead of it", env.Op,
					env.Subject, what, table, bucket, env.Scope.Buckets)
			}
		}
		return nil
	}
	if err := check(before, after, "changed or removed"); err != nil {
		return err
	}
	return check(after, before, "wrote")
}

func (r *writeRig) drainSafely() {
	if err := r.drainTo(); err != nil && r.t.Context().Err() == nil {
		r.t.Errorf("the background consumer stopped: %v", err)
	}
}

// enrol creates one person or service account through the whole path — an
// administrator's create — and applies the result.
func (r *writeRig) enrol(in iamdomain.Creation) error {
	r.t.Helper()
	_, err := r.create(in)
	return err
}

// create is [writeRig.enrol] answering what the create answered: its outcome,
// and a person's first password link.
func (r *writeRig) create(in iamdomain.Creation) (iamdomain.Created, error) {
	r.t.Helper()
	var created iamdomain.Created
	err := r.draining(func() error {
		var err error
		created, err = r.writer.Create(r.t.Context(), in)
		return err
	})
	return created, err
}

// firstLinkExpiry is when a person the rig creates can no longer spend their
// first password link, by the rig writer's clock.
var firstLinkExpiry = brokerAt.Add(credential.EnrolmentLinkLifetime)

// vacantSeat puts one HUMAN seat in the running company and answers its handle:
// the seat a person's create, an invitation or a move names. Every person holds
// one (ADR-0032), so every case that creates a person asks for one here.
func (r *writeRig) vacantSeat(handle string) string {
	r.t.Helper()
	r.seatOnly(handle)
	return handle
}

// seatOf is a seat handle for whoever an address is for, from its local part
// alone: a handle is in the clear on every record and row that binds it, and
// one spelled from the whole address would put the address there with it.
func seatOf(address string) string {
	local, _, _ := strings.Cut(address, "@")
	return strings.ReplaceAll(local, ".", "-") + "-desk"
}

// aPassword is the one credential a redemption carries: a password, as the
// verifier the sign-in surface derives — its bytes are opaque to this domain.
func aPassword() iamdomain.Credential {
	return iamdomain.Credential{
		V: iamdomain.DocumentVersion, ID: uuid.Must(uuid.NewV7()).String(),
		Method: iamdomain.MethodPassword, Verifier: "argon",
	}
}

// bind binds one person to a seat through the whole path and applies the
// result.
func (r *writeRig) bind(seat, person, opID string) error {
	r.t.Helper()
	return r.draining(func() error {
		_, err := r.writer.SetIdentity(r.t.Context(), iamdomain.IdentityEdit{
			PersonID: person, Seat: &seat, OpID: opID, Reason: "a bind"})
		return err
	})
}

// rename moves one person to another login through the whole path and applies
// the result.
func (r *writeRig) rename(person, login, opID string) error {
	r.t.Helper()
	return r.draining(func() error {
		_, err := r.writer.SetIdentity(r.t.Context(), iamdomain.IdentityEdit{
			PersonID: person, Login: &login, OpID: opID, Reason: "a rename"})
		return err
	})
}

// draining runs one gesture with this rig's consumer alongside it.
func (r *writeRig) draining(gesture func() error) error {
	r.t.Helper()
	// THE CONSUMER RUNS ALONGSIDE: every write waits for this node's applier to
	// reach its record, so draining only afterwards deadlocks.
	return r.during(gesture)
}

// during runs one gesture with this node's applier consuming alongside it,
// which any write that waits for its own apply needs — and drains what is left
// once it returns.
func (r *writeRig) during(gesture func() error) error {
	r.t.Helper()
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

// reader builds this rig's read side over its writer's estate, positioned by
// the rig's own waiter, which moves as the rig drains: a read reports the
// position its rows were derived through, and the reader and the publisher
// agree how far this node has got.
func (r *writeRig) reader(t *testing.T) *iamdomain.Reader {
	t.Helper()
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB: r.db.Reader(), Reached: r.waiter.Reached,
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
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
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

// Reached is Committed: this rig moves its position when it applies a record,
// in one step, so it reaches nothing its commit has not.
func (w *rigWaiter) Reached() statelog.Position { return w.Committed() }

// StreamIdentity and Truncated are the stream identity the publisher asks
// before every append: the rig's log is the one its rows came from, always.
func (w *rigWaiter) StreamIdentity() error { return nil }
func (w *rigWaiter) Truncated() error      { return nil }

// Voided voids nothing: the rig applies by hand and never re-anchors, so no
// generation is ever abandoned or overtaken under it.
func (w *rigWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

func (w *rigWaiter) WaitReached(ctx context.Context, p statelog.Position) error {
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

// WaitApplied is WaitReached: this rig applies every record inline in its
// drain, so committed and applied are one position.
func (w *rigWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {

	return w.WaitReached(ctx, p)
}

// testKeyring is the fleet keyring this rig's node holds: the one key every
// value it seals is sealed under.
var testKeyring = secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{
	"k1": []byte(strings.Repeat("1", 32)),
}}

// testCipher is a keyring as a node holds it.
func testCipher(t *testing.T, ring secrets.Keyring) secrets.Cipher {
	t.Helper()
	cipher, err := secrets.NewCipher(ring)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}

// end is the identity log's last sequence as the broker holds it, which is what
// a node that has drained the log is proved against.
func (r *writeRig) end(ctx context.Context) (uint64, error) { return r.log.End(ctx) }

// resetRow asks [iamdomain.Reader.ResetByID] as the engine does: against the
// log's end, read first.
func (r *writeRig) resetRow(t *testing.T, id string) (iamdomain.ResetRow, error) {
	t.Helper()
	end, err := r.end(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	return r.reader(t).ResetByID(t.Context(), id, end)
}

// invitationRow is [writeRig.resetRow] for [iamdomain.Reader.InvitationByID].
func (r *writeRig) invitationRow(t *testing.T, id string) (iamdomain.InvitationRow, error) {
	t.Helper()
	end, err := r.end(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	return r.reader(t).InvitationByID(t.Context(), id, end)
}

// anybody asks [iamdomain.Reader.AnyPerson] as the engine does: against the
// log's end, read first.
func (r *writeRig) anybody(t *testing.T) (bool, error) {
	t.Helper()
	end, err := r.end(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	return r.reader(t).AnyPerson(t.Context(), end)
}

// behind is the log's end as a node that has not applied its tail sees it, as
// every node does for a moment after boot.
func (r *writeRig) behind(ctx context.Context) (uint64, error) {
	end, err := r.log.End(ctx)
	return end + 3, err
}

// openNodeEstate opens one node's store with its replicated estate beside it,
// closed with the test, and answers the estate's handle; what names the node in
// a failure.
func openNodeEstate(t *testing.T, what string) store.ReplicatedHandle {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close %s: %v", what, err)
		}
	})
	return db
}

func testSigner(t *testing.T) *recordseal.Signer {
	t.Helper()
	signer, err := recordseal.NewSigner(iamdomain.Domain{}.Name(),
		recordseal.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

func testVerifier(t *testing.T) *recordseal.Verifier {
	t.Helper()
	verifier, err := recordseal.NewVerifier(iamdomain.Domain{}.Name(),
		recordseal.OneKey("k1", "test-material"))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}

// TWO ENROLMENTS ON ONE ADDRESS YIELD EXACTLY ONE WINNER.
//
// There is no unique index on `iam_people.email_blind` and cannot be: a
// constraint violation inside an apply stalls every node's log. Both enrolments
// publish on the DIRECTORY subject at the anchor their snapshot read; the
// broker accepts one, and the other decides again from rows holding the winner
// and is refused naming them.
//
// Mutation: drop the address check from the enrolment's decide, and both land.
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
		// A SEAT EACH, so the one value the two contend for is the address
		// and the loser is refused for it rather than for a seat.
		seat := rig.vacantSeat(fmt.Sprintf("joiner-%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := rig.enrol(iamdomain.Creation{
				PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
				Name: "Sarah Chen", Email: address,
				Login: fmt.Sprintf("sarah.chen%d", i), Seat: seat,
				LinkExpiresAt: firstLinkExpiry,
				OpID:          fmt.Sprintf("op-%d", i),
				Reason:        "the joiner",
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
	var taken *iamdomain.ErrTaken
	if !errors.As(refusal, &taken) || taken.Field != iamdomain.UniqueEmail {
		t.Fatalf("the loser was refused with %v, want the address taken", refusal)
	}

	// AND THE ROWS AGREE: exactly one person holds the address.
	holders := rig.column(
		`SELECT id FROM iam_people WHERE email_blind <> '' ORDER BY id`)
	if len(holders) != 1 {
		t.Errorf("%d people hold an address, want 1: %v", len(holders), holders)
	}
}

// AND TAKING A HELD ADDRESS NAMES WHO HOLDS IT: the directory's own decide
// refuses, in the snapshot the broker arbitrates, so the person it names is the
// holder.
func TestTakingAHeldAddressNamesWhoHoldsIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const first = "018f3a9c-0000-7000-8000-00000000000a"
	if err := rig.enrol(iamdomain.Creation{
		PersonID: first, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", Seat: rig.vacantSeat("sarah-chen"),
		LinkExpiresAt: firstLinkExpiry, OpID: "op-1",
	}); err != nil {
		t.Fatalf("the first enrolment: %v", err)
	}
	rig.drain()

	err := rig.enrol(iamdomain.Creation{
		PersonID: "018f3a9c-0000-7000-8000-00000000000b",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Name: "Someone Else", Email: "Sarah.Chen+jira@Example.COM",
		Login: "someone.else", Seat: rig.vacantSeat("someone-else"),
		LinkExpiresAt: firstLinkExpiry, OpID: "op-2",
	})
	if err == nil {
		t.Fatal("a second enrolment on a held address landed — and it reached " +
			"it by a DIFFERENT SPELLING, so the fold that makes one address " +
			"one blind is not being applied")
	}
	var taken *iamdomain.ErrTaken
	if !errors.As(err, &taken) || taken.Field != iamdomain.UniqueEmail {
		t.Fatalf("the refusal is %v, which does not name the holder", err)
	}
	if taken.Person != first {
		t.Errorf("the refusal names %q as the holder, want %q",
			taken.Person, first)
	}
}

// AN ENROLMENT THAT LANDS IS READABLE AS ONE PERSON, SEALED.
func TestAnEnrolmentWritesOnePersonWhoseValuesAreSealed(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000000a"
	if err := rig.enrol(iamdomain.Creation{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", Seat: rig.vacantSeat("sarah-chen"),
		LinkExpiresAt: firstLinkExpiry, OpID: "op-1", Reason: "the joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()

	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 1 || got[0] != id {
		t.Fatalf("the estate holds %v, want exactly %q", got, id)
	}
	// THE CLEARTEXT IS NOWHERE: not in the sealed columns, the document, or any
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

// A REMOVAL DELETES THE ROWS AND LEAVES THE TOMBSTONE. What it erases from
// the rows that outlive the person is [TestARemovalLeavesNoValueOfTheirsThatOpens]'s.
func TestARemovalLeavesATombstone(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000000a"
	if err := rig.enrol(iamdomain.Creation{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", Seat: rig.vacantSeat("sarah-chen"),
		LinkExpiresAt: firstLinkExpiry, OpID: "op-1",
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
	// THE TOMBSTONE CARRIES THE BLINDS, NOT THE ADDRESS: it is the one row
	// built to outlive the person.
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
// Bumping every person's revocation epoch instead is a write per person in a
// transaction the framework bounds by a row budget, so a company past one
// transaction would end SOME of its sessions, with no way to tell which.
func TestInvalidatingEverySessionMovesOneRowAndNobodysEpoch(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000001a"
	if err := rig.enrol(iamdomain.Creation{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", Seat: rig.vacantSeat("sarah-chen"),
		LinkExpiresAt: firstLinkExpiry, OpID: "op-1",
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
	// AND IT IS ON THE TRAIL, in the horizon that keeps things: ending every
	// session is a gesture an audit comes back to.
	if got := rig.column(
		`SELECT class FROM iam_history WHERE op = 'invalidate'`); len(got) != 1 ||
		got[0] != string(iamdomain.ClassChange) {
		t.Errorf("the invalidation's trail rows read %v, want one classed %q",
			got, iamdomain.ClassChange)
	}
}

// AN OLDER INVALIDATION ARRIVING AFTER A NEWER ONE MUST NOT PULL THE GENERATION
// BACK.
//
// The generation is STATED on the record, so a redelivery or an in-order replay
// ends on the higher one whatever the guard does. The monotone guard defends
// OUT-OF-ORDER arrival (a reordered fetch, a recovery, a node catching up), so
// this applies the two back to front, the only order an unguarded upsert shows
// in. Unguarded, it un-ends every session the second bump ended, on one node,
// for good.
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
// The singleton subject buys it: on two subjects both would read the same
// generation and write the same next one, and every cookie minted between them
// would survive.
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
// Revoking is self-service, and what the engine does the moment somebody else
// has a person's cookie: gating it would make the fastest response to a
// compromise need an administrator. Ending EVERYBODY's sessions has no
// self-service reading.
func TestInvalidatingEverySessionIsRefusedWithoutTheGrant(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	ungranted := rig.writer.As(principalNamed("nobody", iam.KindPerson, nil))
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

// AN INVALIDATION IS ADMIN'S, AND A PERSON'S.
//
// It ends every person's authority at once and is the last step of a restore.
// A party using every app is refused it, so is an administrator acting through
// a machine token — refused as a token, before admin is weighed — and nothing
// is published either way; the administrator at a keyboard, the control,
// lands. Mutation: drop the admin check and the app user bumps the generation.
func TestInvalidatingEverySessionIsAdmins(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	user := rig.writer.As(principalNamed("app.user", iam.KindPerson, iam.Apps))
	if _, err := user.InvalidateAll(rig.t.Context(), operationKey(), ""); !errors.Is(
		err, iamdomain.ErrRefused) || errors.Is(err, iamdomain.ErrTokenRefused) {
		t.Errorf("a party using every app invalidating every session got %v, "+
			"want a plain refusal", err)
	}
	token := throughMachineToken(rig.writer.As(principalNamed("ana.admin",
		iam.KindPerson, []iam.Grant{iam.GrantAdmin})))
	if _, err := token.InvalidateAll(rig.t.Context(), operationKey(), ""); !errors.Is(
		err, iamdomain.ErrTokenRefused) {
		t.Errorf("admin through a machine token invalidating every session got "+
			"%v, want %v", err, iamdomain.ErrTokenRefused)
	}
	admin := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin}))
	if err := rig.during(func() error {
		_, err := admin.InvalidateAll(rig.t.Context(), operationKey(), "a restore")
		return err
	}); err != nil {
		t.Errorf("an administrator was refused: %v", err)
	}
}

// throughMachineToken is w's party acting through a machine token — the
// credential the request guard names beside a token's owner.
func throughMachineToken(w *iamdomain.Writer) *iamdomain.Writer {
	clone := *w
	clone.OperatorID = iam.MachineTokenName(uuid.Must(uuid.NewV7()).String())
	return &clone
}

// seatOnly puts one HUMAN seat in the running company and nothing else, so
// the bind's advisory existence check passes.
func (r *writeRig) seatOnly(handle string) {
	r.t.Helper()
	r.seats.put(handle, session.SeatKindHuman)
}

// agentSeat puts one AGENT seat in the running company.
func (r *writeRig) agentSeat(handle string) {
	r.t.Helper()
	r.seats.put(handle, "agent")
}

// dropSeat takes a seat out of the running company, as an applied revision
// that no longer holds it does.
func (r *writeRig) dropSeat(handle string) {
	r.t.Helper()
	r.seats.drop(handle)
}

// rigSeats is the running company a rig's writers check a seat bind against —
// [iamdomain.SeatLookup] over a map, safe for the cases that bind from two
// goroutines.
type rigSeats struct {
	mu    sync.Mutex
	kinds map[string]string
}

func (s *rigSeats) put(handle, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kinds == nil {
		s.kinds = map[string]string{}
	}
	s.kinds[handle] = kind
}

func (s *rigSeats) drop(handle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.kinds, handle)
}

func (s *rigSeats) Seat(_ context.Context, handle string) (session.Seat, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind, held := s.kinds[handle]
	if !held {
		return session.Seat{}, false, nil
	}
	return session.Seat{Handle: handle, Kind: kind}, true, nil
}

// AN ADMINISTRATIVE RECORD IS NEVER WRITTEN THROUGH A MACHINE TOKEN.
//
// A token may carry admin, so that no token writes the directory rests on this
// check and the authority table's presence rows, not on a permission. Over
// every record [administrativeGestures] lists, a party holding admin acting
// through a machine token is refused [iamdomain.ErrTokenRefused] (wrapping
// [iamdomain.ErrRefused]) before anything is read or published, even when this
// build cannot parse the id past its class. The CONTROL: a Tier A token's own
// login (`token:<id>`), which IS its principal, is no machine token and is
// admitted. Mutation: check admin before the token and every case is admitted.
func TestAnAdministrativeRecordIsNeverWrittenThroughAMachineToken(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	admin := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin}))
	gestures := administrativeGestures(rig.t.Context(), uuid.Must(uuid.NewV7()).String())
	for _, through := range []string{
		iam.MachineTokenName(uuid.Must(uuid.NewV7()).String()),
		iam.MachineTokenPrefix + "x",
	} {
		token := *admin
		token.OperatorID = through
		for name, gesture := range gestures {
			err := gesture(&token)
			if !errors.Is(err, iamdomain.ErrTokenRefused) ||
				!errors.Is(err, iamdomain.ErrRefused) {
				t.Errorf("%s through %s: %v, want %v wrapping %v", name, through,
					err, iamdomain.ErrTokenRefused, iamdomain.ErrRefused)
			}
		}
	}
	rig.drain()
	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 0 {
		t.Errorf("a refused gesture left rows behind: %v", got)
	}
	// THE CONTROL: the deployment's own token is its principal, not a
	// machine token acting as somebody, and it enrols.
	tierA := *admin
	tierA.OperatorID = iam.TokenLogin("ops")
	if err := rig.draining(func() error { return gestures["create"](&tierA) }); err != nil {
		t.Errorf("a Tier A token's own login was refused as a machine token: %v", err)
	}
}

// administrativeGestures is every record [iamdomain.Writer.mayAdminister]
// guards, each a call a party may try; those about an existing person name
// person, whom a refused gesture never reaches.
func administrativeGestures(ctx context.Context,
	person string) map[string]func(w *iamdomain.Writer) error {

	return map[string]func(w *iamdomain.Writer) error{
		"create": func(w *iamdomain.Writer) error {
			_, err := w.Create(ctx, iamdomain.Creation{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
				Stage: iam.StageActive, Login: "svc:token-made", OpID: operationKey(),
				Reason: "a hire", Grants: []iam.Grant{iam.GrantWorklet},
			})
			return err
		},
		"redeem": func(w *iamdomain.Writer) error {
			_, err := w.Redeem(ctx, iamdomain.Redemption{
				PersonID: uuid.Must(uuid.NewV7()).String(), Stage: iam.StageActive,
				Email: "x@example.com", Login: "x.joiner", Password: aPassword(),
				Invitation: uuid.Must(uuid.NewV7()).String(), InvitationSecret: "s",
				Seat: "nowhere", OpID: operationKey(), Reason: "redeemed",
			})
			return err
		},
		"invite": func(w *iamdomain.Writer) error {
			_, err := w.Invite(ctx, iamdomain.InviteMint{
				Email: "y@example.com", Grants: []iam.Grant{iam.GrantWorklet},
				Seat: "nowhere", ExpiresAt: brokerAt.Add(time.Hour),
				OpID: operationKey(), Reason: "onboarding",
			})
			return err
		},
		"cancel an invitation": func(w *iamdomain.Writer) error {
			_, err := w.CancelInvitation(ctx, uuid.Must(uuid.NewV7()).String(),
				operationKey(), "withdrawn")
			return err
		},
		"update": func(w *iamdomain.Writer) error {
			_, err := w.UpdatePerson(ctx, iamdomain.PersonUpdate{
				PersonID: person, OpID: operationKey(), Reason: "an edit",
				Apply: func(p iamdomain.Person) (iamdomain.Person, error) { return p, nil },
			})
			return err
		},
		"set an identity": func(w *iamdomain.Writer) error {
			login := "jane.renamed"
			_, err := w.SetIdentity(ctx, iamdomain.IdentityEdit{
				PersonID: person, Login: &login, OpID: operationKey(),
				Reason: "a rename",
			})
			return err
		},
		"set a stage": func(w *iamdomain.Writer) error {
			_, err := w.SetStage(ctx, person, iam.StageSuspended, operationKey(),
				"suspended")
			return err
		},
		"remove": func(w *iamdomain.Writer) error {
			_, err := w.Remove(ctx, person, operationKey(), "left")
			return err
		},
		"invalidate every session": func(w *iamdomain.Writer) error {
			_, err := w.InvalidateAll(ctx, operationKey(), "a restore")
			return err
		},
	}
}

// THE ASSISTANT WRITES AN ADMINISTRATIVE RECORD ONLY WHERE NO CEREMONY IS IN
// IT.
//
// An administrator's Dashboard Assistant holds admin and writes through an exec
// grant carrying a request the administrator approved (ADR-0051). The sign-in
// ceremonies are kept from it for the machine token's reason, whatever authority
// reached it: an enrolment or redemption, an invitation and ending every session
// are refused [iamdomain.ErrAssistRefused], wrapping [iamdomain.ErrRefused] and
// never read as a token, as is an id of the assistant's class this build cannot
// parse. Every other administrative record is the administrator's own.
// Mutation: drop the lock and the ceremonies are admitted; widen it to every
// record and the edits are refused.
func TestTheAssistantWritesNoSignInCeremony(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	keyboard := principalNamed("ana.admin", iam.KindPerson, []iam.Grant{iam.GrantAdmin})
	gestures := administrativeGestures(rig.t.Context(), uuid.Must(uuid.NewV7()).String())
	ceremonies := []string{"create", "redeem", "invite", "invalidate every session"}
	composed := keyboard
	composed.Via = iam.AssistName(uuid.Must(uuid.NewV7()))
	composed.AssistScope = iam.AssistScopeExec
	malformed := *rig.writer.As(keyboard)
	malformed.OperatorID = iam.AssistPrefix + "x"
	for through, w := range map[string]*iamdomain.Writer{
		"an assistant grant":             rig.writer.As(composed),
		"an id of the assistant's class": &malformed,
	} {
		for name, gesture := range gestures {
			err := gesture(w)
			refused := errors.Is(err, iamdomain.ErrAssistRefused)
			switch ceremony := slices.Contains(ceremonies, name); {
			case ceremony && (!refused || !errors.Is(err, iamdomain.ErrRefused) ||
				errors.Is(err, iamdomain.ErrTokenRefused)):
				t.Errorf("%s through %s: %v, want %v wrapping %v", name, through,
					err, iamdomain.ErrAssistRefused, iamdomain.ErrRefused)
			case !ceremony && refused:
				t.Errorf("%s through %s was refused as the assistant (%v): an approved "+
					"edit is the administrator's own", name, through, err)
			}
		}
	}
	rig.drain()
	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 0 {
		t.Errorf("a refused gesture left rows behind: %v", got)
	}
	if err := rig.draining(func() error {
		return gestures["create"](rig.writer.As(keyboard))
	}); err != nil {
		t.Errorf("the administrator at their keyboard was refused: %v", err)
	}
}

// AN ADMINISTRATIVE RECORD NEEDS ADMIN.
//
// A party using every app is still somebody using the company rather than
// running it: an enrolment, refused plainly — not as a token — before anything
// is claimed. The control, an administrator, lands. Mutation: admit any
// permission at mayAdminister and the app user enrols somebody.
func TestAnAdministrativeRecordNeedsAdmin(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	enrol := func(w *iamdomain.Writer, login string) error {
		return rig.draining(func() error {
			_, err := w.Create(rig.t.Context(), iamdomain.Creation{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
				Stage: iam.StageActive, Login: login, OpID: operationKey(),
				Reason: "a hire",
			})
			return err
		})
	}
	user := rig.writer.As(principalNamed("automation", iam.KindMachine, iam.Apps))
	if err := enrol(user, "svc:apps"); !errors.Is(err, iamdomain.ErrRefused) ||
		errors.Is(err, iamdomain.ErrTokenRefused) {
		t.Errorf("a party using every app enrolled somebody (%v)", err)
	}
	admin := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin}))
	if err := enrol(admin, "svc:admin"); err != nil {
		t.Errorf("an administrator could not enrol somebody: %v", err)
	}
}

// AN UNKNOWN PERMISSION MAY BE REMOVED, AND KEPT, BUT NEVER ADDED.
//
// A name this build does not know arrives in a row from a newer peer. Keeping
// it adds nothing and narrowing never escalates; adding one is refused even to
// an administrator, since nothing here can say what it opens on a peer that
// reads it. Comparisons run through [iam.Expand], so an app spelled out beside
// admin adds nothing. Held on [iamdomain.Writer.MayConfer], which every
// enrolment, invitation, edit and mint asks, and through an edit's record,
// which publishes nothing on refusal. Mutation: judge additions against the raw
// row, not its expansion, and the admin-implied app is refused.
func TestAnUnknownGrantMayBeRemovedButNotAdded(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	admin := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin}))
	const future = iam.Grant("swarm")
	for _, c := range []struct {
		name          string
		before, after []iam.Grant
		refused       bool
	}{
		{"kept", []iam.Grant{iam.GrantWorklet, future},
			[]iam.Grant{iam.GrantWorklet, future, iam.GrantDoclet}, false},
		{"removed", []iam.Grant{iam.GrantWorklet, future},
			[]iam.Grant{iam.GrantWorklet}, false},
		{"added", []iam.Grant{iam.GrantWorklet},
			[]iam.Grant{iam.GrantWorklet, future}, true},
		{"an app admin already includes", []iam.Grant{iam.GrantAdmin},
			[]iam.Grant{iam.GrantAdmin, iam.GrantWorklet}, false},
	} {
		err := admin.MayConfer(c.before, c.after)
		if c.refused != errors.Is(err, iamdomain.ErrRefused) {
			t.Errorf("%s: MayConfer(%v, %v) = %v, want refused=%v", c.name,
				c.before, c.after, err, c.refused)
		}
	}
	// AND A PARTY WITHOUT ADMIN keeps what is there and adds only what it
	// holds — the expansion of the row is no licence of its own.
	user := rig.writer.As(principalNamed("app.user", iam.KindPerson,
		[]iam.Grant{iam.GrantWorklet}))
	if err := user.MayConfer([]iam.Grant{iam.GrantAdmin},
		[]iam.Grant{iam.GrantAdmin, iam.GrantDoclet}); err != nil {
		t.Errorf("spelling out an app the row holds through admin was "+
			"refused: %v", err)
	}
	if err := user.MayConfer(nil, []iam.Grant{iam.GrantDoclet}); !errors.Is(err,
		iamdomain.ErrRefused) {
		t.Errorf("a Worklet user conferred Doclet (%v)", err)
	}

	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Okafor", Email: "dana@example.com", Login: "dana.sre",
		Seat:   rig.vacantSeat("dana-okafor"),
		Grants: []iam.Grant{iam.GrantWorklet},
		OpID:   operationKey(), Reason: "a hire", LinkExpiresAt: firstLinkExpiry,
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	before := rig.column(`SELECT document FROM iam_people WHERE id = ?`, person)
	if err := rig.draining(func() error {
		_, err := admin.UpdatePerson(rig.t.Context(), iamdomain.PersonUpdate{
			PersonID: person, OpID: operationKey(), Reason: "a future grant",
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Grants = append(p.Grants, future)
				return p, nil
			},
		})
		return err
	}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("an edit added a name this build does not know (%v)", err)
	}
	rig.drain()
	if after := rig.column(`SELECT document FROM iam_people WHERE id = ?`,
		person); !slices.Equal(before, after) {
		t.Errorf("a refused edit changed the row: %v, was %v", after, before)
	}
}

// AN INVITATION IS A DIRECTORY RECORD, so two administrators inviting one
// person produce ONE invitation and one refusal naming it; on the invitation's
// own fresh id both would land, two links each creating the same person.
//
// Mutation: drop the open-invitation check from Invite's decide, and both land.
func TestTwoInvitationsToOneAddressYieldOneWinner(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	// A SEAT EACH, so the one value the two contend for is the address.
	invite := func(op, seat string) error {
		_, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
			Email:     "sarah@example.com",
			Grants:    []iam.Grant{iam.GrantWorklet},
			Seat:      rig.vacantSeat(seat),
			ExpiresAt: brokerAt.Add(168 * time.Hour),
			OpID:      op, Reason: "onboarding",
		})
		return err
	}
	if err := invite(operationKey(), "platform-lead"); err != nil {
		t.Fatalf("the first invitation: %v", err)
	}
	rig.drain()
	err := invite(operationKey(), "data-lead")
	if err == nil {
		t.Fatal("a second invitation to the same address was accepted, so " +
			"the company holds two links that each create one person")
	}
	var taken *iamdomain.ErrTaken
	if !errors.As(err, &taken) || taken.Invitation == "" ||
		taken.Field != iamdomain.UniqueEmail {
		t.Errorf("the refusal is %v, want one naming the invitation that "+
			"already holds the address", err)
	}
	if got := rig.column(`SELECT id FROM iam_invites`); len(got) != 1 {
		t.Errorf("iam_invites holds %v, want exactly one row", got)
	}
}

// AND TWO INVITATIONS ONTO ONE SEAT YIELD ONE WINNER: the invitation on its way
// to a seat holds it, so a second is refused naming the first, never two links
// whose second redemption fails at the last step.
//
// Mutation: drop the seat's open-invitation check from Invite's decide, and
// both land.
func TestTwoInvitationsToOneSeatYieldOneWinner(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	invite := func(address string) (iamdomain.InviteIssued, error) {
		var issued iamdomain.InviteIssued
		err := rig.during(func() error {
			var err error
			issued, err = rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
				Email: address, Grants: []iam.Grant{iam.GrantWorklet},
				Seat: seat, ExpiresAt: brokerAt.Add(168 * time.Hour),
				OpID: operationKey(), Reason: "onboarding",
			})
			return err
		})
		return issued, err
	}
	first, err := invite("sarah@example.com")
	if err != nil {
		t.Fatalf("the first invitation: %v", err)
	}
	_, err = invite("dana@example.com")
	var taken *iamdomain.ErrTaken
	if !errors.As(err, &taken) || taken.Field != iamdomain.UniqueSeat ||
		taken.Invitation != first.ID || taken.Value != seat {
		t.Fatalf("a second invitation onto %s answered %v, want it refused "+
			"naming invitation %s as holding the seat", seat, err, first.ID)
	}
	if got := rig.column(`SELECT id FROM iam_invites`); len(got) != 1 {
		t.Errorf("iam_invites holds %v, want exactly one row", got)
	}
}

// AN INVITATION NEEDS AN EXPIRY: one read as `never` is a superuser claim live
// in somebody's mailbox for the life of the company. It names a seat, so the
// expiry is all that is missing, and the refusal is asserted on its sentinel,
// since any refusal at all, the seat's included, would otherwise pass.
func TestAnInvitationWithNoExpiryIsRefused(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	_, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
		Email: "sarah@example.com", Seat: rig.vacantSeat("platform-lead"),
		OpID: operationKey(),
	})
	if !errors.Is(err, iamdomain.ErrInvalid) ||
		errors.Is(err, iamdomain.ErrSeatRequired) ||
		!strings.Contains(err.Error(), "expiry") {
		t.Errorf("an invitation with no expiry answered %v, want %v naming "+
			"the expiry", err, iamdomain.ErrInvalid)
	}
	if got := rig.column(`SELECT id FROM iam_invites`); len(got) != 0 {
		t.Errorf("the refused issue left invitations %v", got)
	}
}

// A MACHINE IDENTITY ENROLS WITH NO ADDRESS, AND A PERSON MAY NOT.
//
// `svc:ci` has no login page and no mailbox (it proves itself with a token), so
// requiring an address would mean inventing one; but a row neither addressable
// nor named is a credential holder nobody can list, revoke or audit. A PERSON's
// address IS their interactive login key, so one without it could never sign
// in.
func TestAMachineEnrolsWithNoAddressAndAPersonMayNot(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	// WITH NO SEAT EITHER, a service account's to leave out: it acts as itself.
	if err := rig.enrol(iamdomain.Creation{
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
	// AND IT IS STILL SEALED: a machine's name is a person's words ("Release
	// pipeline, raised by Dana"), kept and erased as anybody's are.
	if got := rig.column(
		`SELECT length(name_sealed) FROM iam_people WHERE login = 'svc:ci'`); //
	len(got) != 1 || got[0] == "0" {
		t.Errorf("the machine's name_sealed is %v, want ciphertext", got)
	}

	// THE REFUSALS ARE ASSERTED ON THE SENTINEL, never on "an error happened":
	// in a rig not draining, an enrolment reaching the publisher fails for
	// unrelated reasons.
	//
	// A PERSON ALSO MISSING THEIR SEAT is told about the address first, the
	// more specific omission.
	if _, err := rig.writer.Create(rig.t.Context(), iamdomain.Creation{
		PersonID: "018f3a9c-0000-7000-8000-0000000000f2",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Login: "dana.sre", OpID: "op-person", Reason: "a hire",
	}); !errors.Is(err, iamdomain.ErrNotFindable) {
		t.Errorf("a person with no address was refused with %v, want %v — "+
			"nothing they hold is an interactive login key, so they can "+
			"never sign in", err, iamdomain.ErrNotFindable)
	}
	// AN ADDRESS OF ONLY WHITESPACE IS NO ADDRESS, judged on what it folds to,
	// for a person, a service account and an invitation alike: judged raw it
	// passes the empty check and fails at the blind unclassified, a 500 for a
	// value the caller typed.
	blank := []struct {
		name  string
		write func() error
	}{
		{"a person", func() error {
			_, err := rig.writer.Create(rig.t.Context(), iamdomain.Creation{
				PersonID: "018f3a9c-0000-7000-8000-0000000000f4",
				Kind:     iam.KindPerson, Stage: iam.StageActive, Email: "   ",
				Login: "dana.sre", Seat: rig.vacantSeat("sre-desk"),
				LinkExpiresAt: firstLinkExpiry, OpID: "op-blank-person",
				Reason: "a hire",
			})
			return err
		}},
		{"a service account", func() error {
			_, err := rig.writer.Create(rig.t.Context(), iamdomain.Creation{
				PersonID: "018f3a9c-0000-7000-8000-0000000000f5",
				Kind:     iam.KindMachine, Stage: iam.StageActive, Email: " \t",
				Login: "svc:blank", OpID: "op-blank-machine", Reason: "a pipeline",
			})
			return err
		}},
		{"an invitation", func() error {
			_, err := rig.writer.Invite(rig.t.Context(), iamdomain.InviteMint{
				Email: "  ", Seat: rig.vacantSeat("sre-desk"),
				ExpiresAt: brokerAt.Add(time.Hour), OpID: operationKey(),
				Reason: "onboarding",
			})
			return err
		}},
	}
	for _, tc := range blank {
		if err := tc.write(); !errors.Is(err, iamdomain.ErrNotFindable) {
			t.Errorf("%s with a blank address was refused with %v, want %v",
				tc.name, err, iamdomain.ErrNotFindable)
		}
	}
	// AND A MACHINE WITH NEITHER IS REFUSED TOO, which is the control:
	// the rule is "findable", not "no address needed".
	if _, err := rig.writer.Create(rig.t.Context(), iamdomain.Creation{
		PersonID: "018f3a9c-0000-7000-8000-0000000000f3",
		Kind:     iam.KindMachine, Stage: iam.StageActive,
		OpID: "op-nameless", Reason: "a pipeline",
	}); !errors.Is(err, iamdomain.ErrNotFindable) {
		t.Errorf("a machine with neither an address nor a login was refused "+
			"with %v, want %v — it is a credential holder nobody can list "+
			"or revoke", err, iamdomain.ErrNotFindable)
	}
}

// AN ENROLMENT OUT OF BOUNDS PUBLISHES NOTHING.
//
// Asserted on the SENTINEL, which a surface answers 400, and on the untouched
// ESTATE, so the corrected retry takes the address and the login with nothing
// of the refused one in its way.
func TestAnEnrolmentOutOfBoundsPublishesNothing(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("sarah-chen")
	for i, tc := range []struct {
		name   string
		mutate func(*iamdomain.Creation)
	}{
		{"a reason past the cap", func(e *iamdomain.Creation) {
			e.Reason = strings.Repeat("x", iamdomain.MaxReason+1)
		}},
		{"a name past the cap", func(e *iamdomain.Creation) {
			e.Name = strings.Repeat("x", iamdomain.MaxName+1)
		}},
		{"a first password link already aged out", func(e *iamdomain.Creation) {
			e.LinkExpiresAt = brokerAt
		}},
		{"a first password link that never ages out", func(e *iamdomain.Creation) {
			e.LinkExpiresAt = time.Time{}
		}},
	} {
		in := iamdomain.Creation{
			PersonID: fmt.Sprintf("018f3a9c-0000-7000-8000-0000000004a%d", i),
			Kind:     iam.KindPerson, Stage: iam.StageActive,
			Name: "Sarah Chen", Email: "sarah.chen@example.com",
			Login: "sarah.chen", Seat: seat, LinkExpiresAt: firstLinkExpiry,
			OpID: fmt.Sprintf("op-bounds-%d", i), Reason: "a hire",
		}
		tc.mutate(&in)
		if err := rig.enrol(in); !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("%s: refused with %v, want %v", tc.name, err,
				iamdomain.ErrInvalid)
		}
	}
	rig.drain()
	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 0 {
		t.Errorf("a refused enrolment left rows %v — the corrected retry would "+
			"be refused by its own first attempt", got)
	}

	// THE CONTROL: the same enrolment inside its bounds lands, and takes
	// the address and the login the refused ones did not.
	if err := rig.enrol(iamdomain.Creation{
		PersonID: "018f3a9c-0000-7000-8000-0000000004b1",
		Kind:     iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", Seat: seat, LinkExpiresAt: firstLinkExpiry,
		OpID: "op-bounds-ok", Reason: "a hire",
	}); err != nil {
		t.Fatalf("the corrected enrolment was refused: %v", err)
	}
}
