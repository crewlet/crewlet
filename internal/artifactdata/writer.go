package artifactdata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE DATASTORE WRITE PATH: one record per write, published to the store's
// subject, its verdict READ BACK from the row its apply wrote.
//
// # What the writer decides, and what it leaves to the applier
//
// A write is SEQUENCED: the writer forms no expectation and decides nothing
// from a snapshot about the write's effect. It refuses before anything is
// appended only what no later row can change (a name outside the grammar, a
// batch past its bounds, a person value naming no seat the running chart
// holds, this node's write budget spent) and what is permanent (a store its
// artifact's purge removed). Everything else (schema, rules, guards, caps,
// limits) is the applier's, in log order, so every data node reaches one verdict.
//
// A schema change is ARBITRATED: decided in a snapshot of the schema it
// replaces, refused early when the snapshot already refuses it, and judged
// again by the applier.
//
// # The authority is stamped, never decided here
//
// Who may write is the authority table's, decided at the API and carried in
// [Call.Authority]: the caller's effective role on the artifact, their actor
// kind and the limits in force. This package evaluates collection and field
// rules against that stamp; it never evaluates the role ladder against an
// action (one implementation of a shared rule, ADR-0008).

// Call is one write gesture's caller context, the artifacts log's own: a
// purge reaches this store as one of its dependents under the artifact purge's
// call.
type Call = artifactsdomain.Call

// WriteResult is what a write's record did.
type WriteResult struct {
	// Verdict is the applier's, read from this node's rows; EMPTY when the
	// record has not applied here yet (Result is then pending, carrying the
	// position to read at).
	Verdict artifacts.DataVerdict `json:"verdict,omitempty"`

	// Seq is the record's position, packed: the history row it wrote.
	Seq int64 `json:"seq,omitempty"`

	Result statelog.Result `json:"result"`

	// Conflict is the current state of the documents a conflicting guard
	// named, the ones the writer may read: everything a merge needs.
	Conflict []Doc `json:"conflict,omitempty"`

	// Reason is the applier's sentence for a refusal.
	Reason string `json:"reason,omitempty"`
}

// Metrics is the recorder the writer reports through.
type Metrics interface {
	Observe(name string, d time.Duration, attrs metrics.Attrs)
	Add(name string, n uint64, attrs metrics.Attrs)
	Set(name string, v float64, attrs metrics.Attrs)
}

// Objectives is where the writer's service objective events go: the node's
// objective meter ([objectives.Meter]), which judges each against its bound.
type Objectives interface {
	Record(n objectives.Name, elapsed time.Duration, answered bool)
}

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	// Publisher is the domain's write authority, and DB the replicated
	// estate its decisions and its verdict reads read.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// SeatExists reports whether a seat handle is a seat of the running
	// organisation: what a person column's value is held to before the
	// write is appended. REQUIRED.
	SeatExists func(ctx context.Context, handle string) (bool, error)

	// Now is the clock authored instants and this node's write budget are
	// read from; nil is the wall clock.
	Now func() time.Time

	// Metrics records each write's outcome and the company's footprint;
	// nil records nothing.
	Metrics Metrics

	// Objectives counts every write and undo into the datastore objectives
	// ([objectives.DataRoundTrip], [objectives.DataResolved]); nil counts
	// none.
	Objectives Objectives
}

// Writer is the datastore write path.
type Writer struct {
	publisher  *statelog.Publisher
	db         store.ReplicatedReader
	seatExists func(ctx context.Context, handle string) (bool, error)
	now        func() time.Time
	metrics    Metrics
	judged     Objectives
	limits     *costlimit.Limiter
}

// NewWriter builds the write path.
func NewWriter(deps WriterDeps) (*Writer, error) {
	switch {
	case deps.Publisher == nil:
		return nil, errors.New("artifactdata: a write authority is required: every write is " +
			"a record on the datastore log")
	case deps.DB.IsZero():
		return nil, errors.New("artifactdata: the replicated estate is required: a write's " +
			"verdict is read back from this node's rows")
	case deps.SeatExists == nil:
		return nil, errors.New("artifactdata: a seat resolver is required: a person value " +
			"names a seat of the running organisation")
	}
	w := &Writer{publisher: deps.Publisher, db: deps.DB, seatExists: deps.SeatExists,
		now: deps.Now, metrics: deps.Metrics, judged: deps.Objectives}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	w.limits = newLimiter(w.now)
	return w, nil
}

// operation is the id one write is published under: derived from the call's
// key, the verb, the store and what the write says, or freshly minted where
// the call carries no key. The same key with another body is another
// operation, never the first answered back from the ledger.
func (w *Writer) operation(c Call, verb, object string, content any) (string, error) {
	key := strings.TrimSpace(c.OpKey)
	if key == "" {
		return statelog.NewOpID(time.Now(), verb), nil
	}
	if err := statelog.CheckCallerOpID(key); err != nil {
		return "", fmt.Errorf("%w: the operation key: %w", ErrInvalid, err)
	}
	said, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("artifactdata: digest what a %s says: %w", verb, err)
	}
	sum := sha256.Sum256(said)
	at, _ := statelog.OpMintedAt(key)
	return statelog.DeriveOpID(at, verb, key, object, hex.EncodeToString(sum[:])), nil
}

// validCall refuses a caller the records could not name.
func validCall(c Call) error {
	a := c.Authority
	switch {
	case strings.TrimSpace(a.Actor.Name) == "":
		return fmt.Errorf("%w: the call names no actor: every record names who wrote it", ErrInvalid)
	case !a.Actor.Kind.Valid():
		return fmt.Errorf("%w: actor kind %q is not one of %v", ErrInvalid, a.Actor.Kind, iam.ActorKinds)
	case a.Role != "" && !a.Role.Valid():
		return fmt.Errorf("%w: role %q is not one of %v", ErrInvalid, a.Role, artifacts.Roles)
	case a.Origin != "" && !a.Origin.Valid():
		return fmt.Errorf("%w: origin %q is not one of %v", ErrInvalid, a.Origin, artifacts.WriteOrigins)
	}
	return nil
}

// stampedCall refuses a write whose authority the applier would refuse on
// every data node: no role, or no datastore limits.
func stampedCall(c Call) error {
	if err := validCall(c); err != nil {
		return err
	}
	if !c.Authority.Role.Valid() {
		return fmt.Errorf("%w: the call's authority carries no role on the artifact", ErrInvalid)
	}
	if c.Authority.Limits.DataBytes <= 0 || c.Authority.Limits.DataStoreBytes <= 0 {
		return fmt.Errorf("%w: the call's authority carries no datastore limits: the API stamps "+
			"the limits in force (config.Artifacts.Stamp) into every write", ErrInvalid)
	}
	return nil
}

// checkBatch refuses a batch no store can hold: past the op or byte bound, or
// naming something outside the grammar.
func checkBatch(ops []artifacts.DataOp) error {
	if len(ops) == 0 || len(ops) > artifacts.MaxBatchOps {
		return fmt.Errorf("%w: a write carries 1..%d ops and this one carries %d", ErrInvalid,
			artifacts.MaxBatchOps, len(ops))
	}
	raw, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("%w: the ops do not encode: %w", ErrInvalid, err)
	}
	if int64(len(raw)) > artifacts.MaxBatchBytes {
		return fmt.Errorf("%w: the ops are %d bytes and a write carries at most %d", ErrInvalid,
			len(raw), artifacts.MaxBatchBytes)
	}
	for i, op := range ops {
		if err := checkOpShape(op); err != nil {
			return fmt.Errorf("op %d: %w", i, err)
		}
	}
	return nil
}

// decision encodes one record inside the snapshot's own transaction, stamped
// with the framework's writer and generation.
func (w *Writer) decision(stamp statelog.Stamp, c Call, subject Subject, op OpKind,
	opID string, payload any) (statelog.Decision, error) {

	if err := subject.writable(); err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("artifactdata: encode the %s payload for %s: %w",
			op, subject, err)
	}
	actor := c.Authority.Actor
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: w.now(),
			Gen: stamp.Gen, Writer: stamp.Writer, Scope: ScopeSet{Subject: true},
		},
		Mutation: body, Actor: actor.Name, ActorKind: actor.Kind,
		OperatorID: actor.OperatorID, TurnID: c.TurnID,
	})
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{Payload: encoded}, nil
}

// published is a store write's publish answer in this domain's terms: a write
// the deletion gate dropped is a purged store ([ErrGone]), as the decide says
// when it is the one that finds the marker.
//
// THE GATE FINDS IT WHEN THE PURGE LANDED AHEAD OF THE RECORD, after the decide
// passed: the framework states that as one of its conditions, which the
// datastore API answered `503` "try again" and a tool "try again shortly",
// about a store nothing will ever write again.
func published(id artifacts.ID, err error) error {
	var refused *statelog.Unavailable
	if errors.As(err, &refused) && refused.Reason == statelog.ReasonDeleted {
		return fmt.Errorf("%w: %s", ErrGone, id)
	}
	return err
}

// request is one write's framework request on a store's subject.
func request(id artifacts.ID, opID string, pattern statelog.Pattern,
	decide func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error)) statelog.Request {

	subject := StoreSubject(id)
	return statelog.Request{
		Subject: subject.framework(), Scope: ScopeSet{Subject: true}.Resolve(subject),
		OpID: opID, Pattern: pattern, Decide: decide,
	}
}

// Write publishes one batch of ops on an artifact's store and answers its
// verdict.
func (w *Writer) Write(ctx context.Context, c Call, id artifacts.ID, in artifacts.DataWrite) (WriteResult, error) {
	started := time.Now()
	res, err := w.write(ctx, c, id, in)
	w.record(started, res, err)
	return res, err
}

// write is [Writer.Write] before it is recorded.
func (w *Writer) write(ctx context.Context, c Call, id artifacts.ID, in artifacts.DataWrite) (WriteResult, error) {
	if err := stampedCall(c); err != nil {
		return WriteResult{}, err
	}
	if err := checkBatch(in.Ops); err != nil {
		return WriteResult{}, err
	}
	if len(in.ClientID) > artifacts.MaxDocIDBytes {
		return WriteResult{}, fmt.Errorf("%w: a client id is at most %d bytes", ErrInvalid,
			artifacts.MaxDocIDBytes)
	}
	cost, err := w.cost(ctx, id, in.Ops)
	if err != nil {
		return WriteResult{}, err
	}
	if limited := takeWrite(w.limits, id.String(), c.Authority.Actor.Name, cost); limited != nil {
		return WriteResult{}, limited
	}
	opID, err := w.operation(c, string(OpWrite), id.String(), in)
	if err != nil {
		return WriteResult{}, err
	}
	payload := WritePayload{V: baseRecordVersion, Write: in, Authority: c.Authority}
	res, err := w.publisher.Publish(ctx, request(id, opID, statelog.PatternSequenced,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if err := w.early(ctx, tx, id, in.Ops); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			return w.decision(stamp, c, StoreSubject(id), OpWrite, opID, payload)
		}))
	if err != nil {
		return WriteResult{Result: res}, published(id, err)
	}
	return w.verdict(ctx, c, id, opID, res)
}

// cost is what a batch weighs against this node's write budgets ([Cost]),
// from the field-index entries each document it names holds in this node's
// copy: one read, one indexed count per collection the batch names.
func (w *Writer) cost(ctx context.Context, id artifacts.ID, ops []artifacts.DataOp) (int, error) {
	held := map[docRef]int{}
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		s, present, err := readStore(ctx, tx, id.String())
		if err != nil || !present {
			return err
		}
		byColl := map[string][]string{}
		for _, op := range ops {
			if !slices.Contains(byColl[op.Collection], op.Doc) {
				byColl[op.Collection] = append(byColl[op.Collection], op.Doc)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(byColl)) {
			var no int64
			err := tx.QueryRowContext(ctx, `
				SELECT collection_no FROM artifact_data_collections WHERE store_no = ? AND name = ?`,
				s.no, name).Scan(&no)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("artifactdata: read collection %s of store %s: %w", name, id, err)
			}
			docs := byColl[name]
			args := []any{s.no, no}
			for _, d := range docs {
				args = append(args, d)
			}
			rows, err := tx.QueryContext(ctx, `
				SELECT doc_id, COUNT(*) FROM artifact_data_fields
				WHERE store_no = ? AND collection_no = ? AND doc_id IN (?`+
				strings.Repeat(`, ?`, len(docs)-1)+`) GROUP BY doc_id`, args...)
			if err != nil {
				return fmt.Errorf("artifactdata: count the index entries of %s: %w", name, err)
			}
			for rows.Next() {
				var (
					doc string
					n   int
				)
				if err := rows.Scan(&doc, &n); err != nil {
					_ = rows.Close()
					return fmt.Errorf("artifactdata: count the index entries of %s: %w", name, err)
				}
				held[docRef{Collection: name, Doc: doc}] = n
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return fmt.Errorf("artifactdata: count the index entries of %s: %w", name, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return Cost(ops, func(collection, doc string) int {
		return held[docRef{Collection: collection, Doc: doc}]
	}), nil
}

// early is what a write is refused on before it is appended, read in the
// snapshot's own transaction: a store a purge removed, and a person value
// naming no seat of the running organisation.
func (w *Writer) early(ctx context.Context, tx *sql.Tx, id artifacts.ID, ops []artifacts.DataOp) error {
	if _, marked, err := deletionMarker(ctx, tx, id.String()); err != nil {
		return err
	} else if marked {
		return fmt.Errorf("%w: %s", ErrGone, id)
	}
	s, present, err := readStore(ctx, tx, id.String())
	if err != nil || !present {
		return err
	}
	v := &txView{ctx: ctx, tx: tx, store: s, present: present}
	seen := map[string]bool{}
	for i, op := range ops {
		c, err := v.collection(op.Collection)
		if err != nil {
			return err
		}
		if c == nil || c.schemaless() {
			continue
		}
		for _, h := range personValues(c, op) {
			if seen[h] {
				continue
			}
			seen[h] = true
			held, err := w.seatExists(ctx, h)
			if err != nil {
				return fmt.Errorf("artifactdata: read whether seat %q exists: %w", h, err)
			}
			if !held {
				return fmt.Errorf("%w: op %d: %q is not a seat of the organisation", ErrInvalid, i, h)
			}
		}
	}
	return nil
}

// personValues is every seat handle an op writes into a person column.
func personValues(c *collState, op artifacts.DataOp) []string {
	var out []string
	add := func(field string, raw json.RawMessage) {
		col, ok := c.columns[field]
		if !ok || col.Type != artifacts.ColumnPerson {
			return
		}
		v, err := canon.Parse(raw, 1)
		if err == nil && v.Kind == canon.KindString {
			out = append(out, v.String)
		}
	}
	for field, raw := range op.Fields {
		add(field, raw)
	}
	if op.Field != "" {
		add(op.Field, op.Value)
	}
	return out
}

// verdict is what the apply of operation opID did on store id, read from this
// node's own rows.
func (w *Writer) verdict(ctx context.Context, c Call, id artifacts.ID, opID string,
	res statelog.Result) (WriteResult, error) {

	out := WriteResult{Result: res}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
		return out, nil
	}
	var conflicts []docRef
	found, gone := false, false
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var changes string
		err := tx.QueryRowContext(ctx, `
			SELECT w.seq, w.verdict, w.reason, w.changes FROM artifact_data_writes w
			JOIN artifact_data_stores s ON s.store_no = w.store_no
			WHERE w.op_id = ? AND s.artifact_id = ?`, opID, id.String()).Scan(
			&out.Seq, &out.Verdict, &out.Reason, &changes)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, marked, err := deletionMarker(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			gone = marked
			return err
		case err != nil:
			return err
		}
		found = true
		if out.Verdict != artifacts.DataConflict {
			return nil
		}
		var chs []Change
		if err := json.Unmarshal([]byte(changes), &chs); err != nil {
			return fmt.Errorf("artifactdata: decode a conflict's documents: %w", err)
		}
		for _, ch := range chs {
			if ch.Op == ChangeConflict {
				conflicts = append(conflicts, docRef{ch.Collection, ch.Doc})
			}
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("artifactdata: read what operation %s did on %s: %w", opID, id, err)
	}
	switch {
	case gone:
		out.Verdict, out.Reason = artifacts.DataGone, "the datastore was purged"
		return out, nil
	case !found:
		return out, fmt.Errorf("artifactdata: operation %s applied on %s and wrote no history "+
			"row: a ledger contract violation", opID, id)
	}
	if len(conflicts) > 0 {
		out.Conflict, err = w.current(ctx, c, id, conflicts)
	}
	return out, err
}

// current is the documents a conflict named, as the caller may read them.
func (w *Writer) current(ctx context.Context, c Call, id artifacts.ID, refs []docRef) ([]Doc, error) {
	rd := Reading{Record: c.Authority.Actor.Name, Role: c.Authority.Role}
	var out []Doc
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		s, present, err := readStore(ctx, tx, id.String())
		if err != nil || !present {
			return err
		}
		v := &txView{ctx: ctx, tx: tx, store: s, present: present}
		for _, ref := range refs {
			coll, err := v.collection(ref.Collection)
			if err != nil {
				return err
			}
			if coll == nil || !coll.stored {
				continue
			}
			docs, err := scanDocs(ctx, tx, coll, `
				SELECT doc_id, body, created_by, created_by_kind, created_at, updated_by,
				updated_by_kind, updated_at, version FROM artifact_data_docs
				WHERE store_no = ? AND collection_no = ? AND doc_id = ?`, s.no, coll.no, ref.Doc)
			if err != nil {
				return err
			}
			for _, d := range docs {
				if Readable(coll.spec.Read, rd, d.CreatedBy.Name) {
					out = append(out, d)
				}
			}
		}
		return nil
	})
	return out, err
}

// record reports one write's outcome and how long it took: into the
// instruments, and as one event of each datastore objective.
func (w *Writer) record(started time.Time, res WriteResult, err error) {
	elapsed := time.Since(started)
	if w.judged != nil {
		answered := decided(res, err)
		w.judged.Record(objectives.DataRoundTrip, elapsed, answered)
		w.judged.Record(objectives.DataResolved, elapsed, answered)
	}
	if w.metrics == nil {
		return
	}
	outcome := string(res.Verdict)
	switch {
	case err != nil:
		outcome = "refused_early"
		var limited *RateLimited
		if errors.As(err, &limited) {
			outcome = "rate_limited"
		}
	case outcome == "":
		outcome = string(res.Result.Outcome)
	}
	attrs := metrics.Attrs{"verdict": outcome}
	w.metrics.Observe(metrics.ArtifactsDataWriteDuration, elapsed, attrs)
	w.metrics.Add(metrics.ArtifactsDataWrites, 1, attrs)
}

// decided reports whether a write or an undo answered its caller with a
// verdict: the applier's, read back from this node's rows, or a refusal before
// anything was appended (input the domain refuses, a caller's own standing, a
// store or a write that does not exist, a purged store, a write the history
// cannot undo, this node's write budget). A write pending on this node, or one
// whose outcome nobody could establish, delivered no verdict, which is the
// failure the datastore objectives count.
func decided(res WriteResult, err error) bool {
	if err == nil {
		return res.Verdict != ""
	}
	var limited *RateLimited
	return errors.As(err, &limited) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) ||
		errors.Is(err, ErrForbidden) || errors.Is(err, ErrGone) || errors.Is(err, ErrNotUndoable)
}

// Undo publishes the inverse of one earlier write, each document conditioned on
// still holding the version that write left it at, and answers its verdict. A
// person undoes their own writes; an editor undoes anybody's.
func (w *Writer) Undo(ctx context.Context, c Call, id artifacts.ID, seq int64) (WriteResult, error) {
	started := time.Now()
	res, err := w.undo(ctx, c, id, seq)
	w.record(started, res, err)
	return res, err
}

func (w *Writer) undo(ctx context.Context, c Call, id artifacts.ID, seq int64) (WriteResult, error) {
	if err := stampedCall(c); err != nil {
		return WriteResult{}, err
	}
	if seq <= 0 {
		return WriteResult{}, fmt.Errorf("%w: an undo names the write it undoes", ErrInvalid)
	}
	ops, err := w.inverse(ctx, c, id, seq)
	if err != nil {
		return WriteResult{}, err
	}
	// THE INVERSE IS A WRITE, held to every write's bounds before it is
	// appended: the apply marks a row undoable only when its inverse fits,
	// and this keeps an undo from ever reaching the log as a record the log
	// refuses.
	if err := checkBatch(ops); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return WriteResult{}, oversizeUndo(seq, err)
	}
	cost, err := w.cost(ctx, id, ops)
	if err != nil {
		return WriteResult{}, err
	}
	if limited := takeWrite(w.limits, id.String(), c.Authority.Actor.Name, cost); limited != nil {
		return WriteResult{}, limited
	}
	opID, err := w.operation(c, string(OpUndo), id.String(), seq)
	if err != nil {
		return WriteResult{}, err
	}
	payload := UndoPayload{V: baseRecordVersion, Undoes: seq, Ops: ops, Authority: c.Authority}
	res, err := w.publisher.Publish(ctx, request(id, opID, statelog.PatternSequenced,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, marked, err := deletionMarker(ctx, tx, id.String()); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			} else if marked {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrGone, id)
			}
			return w.decision(stamp, c, StoreSubject(id), OpUndo, opID, payload)
		}))
	if err != nil {
		return WriteResult{Result: res}, published(id, err)
	}
	return w.verdict(ctx, c, id, opID, res)
}

// inverse is the ops that undo write seq, read from its history row.
func (w *Writer) inverse(ctx context.Context, c Call, id artifacts.ID, seq int64) ([]artifacts.DataOp, error) {
	var ops []artifacts.DataOp
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		s, present, err := readStore(ctx, tx, id.String())
		if err != nil {
			return err
		}
		if !present {
			// THE ARTIFACT'S ID FOR THE LOG, right after the sentinel,
			// where [Sentence] cuts it: a person reads the sentence alone,
			// about the artifact they are looking at. "No such document"
			// would be the wrong noun, since no store exists at all.
			return fmt.Errorf("%w: %s: this artifact's data has no write %d to undo",
				ErrNotFound, id, seq)
		}
		var (
			op, verdict, actor, changes string
			undoable                    int
		)
		err = tx.QueryRowContext(ctx, `
			SELECT op, verdict, actor, changes, undoable FROM artifact_data_writes
			WHERE store_no = ? AND seq = ?`, s.no, seq).Scan(&op, &verdict, &actor, &changes, &undoable)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: write %d is not in the store's history (it keeps %s)",
				ErrNotUndoable, seq, artifacts.DataHistoryHotWindow)
		case err != nil:
			return fmt.Errorf("artifactdata: read write %d of %s: %w", seq, id, err)
		}
		switch {
		case verdict != string(artifacts.DataApplied):
			return fmt.Errorf("%w: write %d was refused (%s) and changed nothing", ErrNotUndoable, seq, verdict)
		case OpKind(op) == OpConfigure:
			return fmt.Errorf("%w: write %d is a schema change; change the schema back", ErrNotUndoable, seq)
		case actor != c.Authority.Actor.Name && !c.Authority.Role.Includes(artifacts.RoleEditor):
			return fmt.Errorf("%w: write %d is %s's, and only its author or an editor undoes it",
				ErrForbidden, seq, actor)
		}
		var chs []Change
		if err := json.Unmarshal([]byte(changes), &chs); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return fmt.Errorf("artifactdata: decode write %d of %s: %w", seq, id, err)
		}
		if undoable == 0 {
			return notUndoable(seq, chs)
		}
		ops, err = inverseOps(chs)
		return err
	})
	return ops, err
}

// notUndoable is why the apply marked write seq not undoable: a value it
// changed is kept only as its hash, or its inverse is larger than one write
// carries ([undoFits]).
func notUndoable(seq int64, chs []Change) error {
	for _, ch := range chs {
		for _, f := range ch.Fields {
			if f.BeforeHash != "" {
				return fmt.Errorf("%w: write %d changed %s, a value past the history's %d-byte "+
					"bound that it keeps only as its hash", ErrNotUndoable, seq, f.Field,
					artifacts.MaxHistoryValueBytes)
			}
		}
	}
	ops, err := inverseOps(chs)
	if err != nil {
		return err
	}
	if err := checkBatch(ops); err != nil {
		return oversizeUndo(seq, err)
	}
	return fmt.Errorf("%w: the apply marked write %d not undoable", ErrNotUndoable, seq)
}

// oversizeUndo refuses an undo whose inverse is larger than one write carries.
// The bound's own error is quoted, not wrapped: the class is ErrNotUndoable,
// never the ErrInvalid a caller's own oversized write is.
func oversizeUndo(seq int64, bound error) error {
	return fmt.Errorf("%w: undoing write %d restores more than one write carries (%v); "+
		"restore the rows in smaller writes", ErrNotUndoable, seq,
		strings.TrimPrefix(bound.Error(), ErrInvalid.Error()+": "))
}

// inverseOps is the ops that restore what changes changed, each guarded on the
// version the change left its document at.
func inverseOps(chs []Change) ([]artifacts.DataOp, error) {
	var ops []artifacts.DataOp
	for _, ch := range chs {
		after := ch.After
		guard := &after
		switch ch.Op {
		case ChangeCreate:
			ops = append(ops, artifacts.DataOp{Op: artifacts.DataDelete, Collection: ch.Collection,
				Doc: ch.Doc, IfVersion: guard})
		case ChangeDelete:
			fields := map[string]json.RawMessage{}
			for _, f := range ch.Fields {
				if f.BeforeHash != "" {
					return nil, fmt.Errorf("%w: %s holds a value past the history's bound", ErrNotUndoable, f.Field)
				}
				if len(f.Before) > 0 {
					fields[f.Field] = f.Before
				}
			}
			ops = append(ops, artifacts.DataOp{Op: artifacts.DataSet, Collection: ch.Collection,
				Doc: ch.Doc, IfVersion: guard, Fields: fields})
		case ChangeUpdate:
			fields := map[string]json.RawMessage{}
			for _, f := range ch.Fields {
				switch {
				case f.BeforeHash != "":
					return nil, fmt.Errorf("%w: %s holds a value past the history's bound", ErrNotUndoable, f.Field)
				case len(f.Before) > 0:
					fields[f.Field] = f.Before
				default:
					fields[f.Field] = json.RawMessage("null")
				}
			}
			if len(fields) == 0 {
				continue
			}
			ops = append(ops, artifacts.DataOp{Op: artifacts.DataMerge, Collection: ch.Collection,
				Doc: ch.Doc, IfVersion: guard, Fields: fields})
		default:
			return nil, fmt.Errorf("%w: a %s change is not undone by a write", ErrNotUndoable, ch.Op)
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("%w: the write changed nothing", ErrNotUndoable)
	}
	return ops, nil
}

// Configure publishes a schema change: every collection the store keeps after
// it, each compiled from its preset. It is decided in a snapshot of the schema
// it replaces and refused early whenever that snapshot refuses it (a change an
// agent may not make, one rewriting more than one record may), then judged
// again by the applier.
func (w *Writer) Configure(ctx context.Context, c Call, id artifacts.ID,
	collections []artifacts.Collection) (WriteResult, error) {

	if err := stampedCall(c); err != nil {
		return WriteResult{}, err
	}
	compiled := make([]artifacts.Collection, 0, len(collections))
	for _, coll := range collections {
		out, err := Compile(coll)
		if err != nil {
			return WriteResult{}, err
		}
		if err := ValidateCollection(out); err != nil {
			return WriteResult{}, err
		}
		compiled = append(compiled, out)
	}
	opID, err := w.operation(c, string(OpConfigure), id.String(), compiled)
	if err != nil {
		return WriteResult{}, err
	}
	payload := ConfigurePayload{V: baseRecordVersion, Collections: compiled, Authority: c.Authority}
	res, err := w.publisher.Publish(ctx, request(id, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, marked, err := deletionMarker(ctx, tx, id.String()); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			} else if marked {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrGone, id)
			}
			s, present, err := readStore(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			p, err := planConfigure(ctx, tx, s, present, c.Authority, compiled)
			if err != nil {
				return statelog.Decision{}, err
			}
			if p.refusal != nil {
				return statelog.Decision{}, configRefusal(p.refusal)
			}
			return w.decision(stamp, c, StoreSubject(id), OpConfigure, opID, payload)
		}))
	if err != nil {
		return WriteResult{Result: res}, configured(id, err)
	}
	return w.verdict(ctx, c, id, opID, res)
}

// configured is a schema change's publish answer in this domain's terms: one
// that lost its race on the store too many times is [ErrConflict], and any
// other is a store write's ([published]).
//
// THE FRAMEWORK'S SENTINEL AND NOT ITS ERROR, whose text names the subject the
// race was lost on, and not the store's id either: a caller that printed this
// error would show a person a broker path and an id they never chose. The
// operation the answer carries is what a log line finds the store by.
func configured(id artifacts.ID, err error) error {
	if errors.Is(err, statelog.ErrConflict) {
		return fmt.Errorf("%w: %w", ErrConflict, statelog.ErrConflict)
	}
	return published(id, err)
}

// configRefusal is a schema change's decide-time refusal in the caller's terms.
func configRefusal(r *refusal) error {
	switch {
	case r.class != nil:
		return fmt.Errorf("%w: %s", r.class, r.reason)
	case r.verdict == artifacts.DataForbidden:
		return fmt.Errorf("%w: %s", ErrForbidden, r.reason)
	}
	return fmt.Errorf("%w: %s", ErrInvalid, r.reason)
}

// PurgeArtifactState removes an artifact's datastore for good, as the first
// step of the artifact's purge ([artifactsdomain.Dependent]): one record under
// the step's own operation id, so a retry of the purge collapses the step that
// landed. The deletion marker is written whether or not the store holds rows,
// so a write about it that arrives later produces no rows on any node.
func (w *Writer) PurgeArtifactState(ctx context.Context, c Call, opID string, id artifacts.ID) error {
	if err := validCall(c); err != nil {
		return err
	}
	if strings.TrimSpace(opID) == "" {
		return fmt.Errorf("%w: a purge step names its operation id", ErrInvalid)
	}
	res, err := w.publisher.Publish(ctx, request(id, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, marked, err := deletionMarker(ctx, tx, id.String()); err != nil {
				return statelog.Decision{}, err
			} else if marked {
				// PURGED ALREADY, by this step or another: nothing to
				// publish, and the artifact's purge goes on.
				return statelog.Decision{}, nil
			}
			return w.decision(stamp, c, StoreSubject(id), OpPurge, opID, PurgePayload{V: GateRecordVersion})
		}))
	if err != nil {
		return fmt.Errorf("artifactdata: purge the datastore of %s: %w", id, err)
	}
	if res.Outcome == statelog.OutcomeUnknown {
		return fmt.Errorf("artifactdata: the purge of %s's datastore has an unknown outcome; retry "+
			"the artifact's purge under the same key", id)
	}
	return nil
}

// EvictNode publishes a node's eviction from this log.
func (w *Writer) EvictNode(ctx context.Context, by iam.Actor, opID, node string) (statelog.Result, error) {
	return w.gateNode(ctx, by, opID, node, false)
}

// ReadmitNode is the eviction's inverse commit.
func (w *Writer) ReadmitNode(ctx context.Context, by iam.Actor, opID, node string) (statelog.Result, error) {
	return w.gateNode(ctx, by, opID, node, true)
}

// gateNode publishes one eviction record on the node's own subject.
func (w *Writer) gateNode(ctx context.Context, by iam.Actor, opID, node string, readmit bool) (statelog.Result, error) {
	switch {
	case node == "":
		return statelog.Result{}, fmt.Errorf("%w: an eviction names no node", ErrInvalid)
	case opID == "":
		return statelog.Result{}, fmt.Errorf("%w: the eviction of %s has no operation id", ErrInvalid, node)
	}
	c := Call{Authority: artifacts.Authority{Actor: by}}
	if err := validCall(c); err != nil {
		return statelog.Result{}, err
	}
	subject := EvictionSubject(node)
	at := w.now()
	return w.publisher.Publish(ctx, statelog.Request{
		Subject: subject.framework(), Scope: ScopeSet{Subject: true}.Resolve(subject), OpID: opID,
		Pattern: statelog.PatternArbitrated, NodeGate: true,
		Standing: func(tx *sql.Tx, held statelog.Position) error {
			row, found, err := standingIn(ctx, tx, node)
			if err != nil {
				return err
			}
			return statelog.GateStanding(opID, node, readmit, held, row, found)
		},
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return w.decision(stamp, c, subject, OpEviction, opID, Eviction{
				V: GateRecordVersion, NodeID: node, EvictedBy: by.Name, EvictedAt: at,
				Readmitted: readmit,
			})
		},
	})
}

// standingIn is node's eviction row on this log.
func standingIn(ctx context.Context, tx *sql.Tx, node string) (statelog.EvictionRow, bool, error) {
	var from int64
	var readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM artifactdata_evictions
		WHERE node_id = ?`, node).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("artifactdata: read %s's eviction row: %w", node, err)
	}
	return statelog.EvictionRow{
		NodeID: node, From: uint64(from), Readmitted: uint64(readmitted.Int64),
		Back: readmitted.Valid && readmitted.Int64 > from,
	}, true, nil
}

// FootprintGauge sets the company's datastore footprint on the recorder, from
// this node's rows: what the engine samples after each committed batch.
func (w *Writer) FootprintGauge(ctx context.Context) error {
	if w.metrics == nil {
		return nil
	}
	var fp int64
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		fp, err = companyFootprint(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	w.metrics.Set(metrics.ArtifactsDataFootprintBytes, float64(fp), nil)
	return nil
}
