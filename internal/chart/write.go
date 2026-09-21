package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WRITE AUTHORITY, and the one rule every path here obeys.
//
//	Take ONE snapshot of your own rows. Decide and form the expectation
//	inside it. Publish. Let the broker arbitrate. Never guess.
//
// [statelog.Publisher] is what enforces it; this file is the chart's decides.
// What each one adds is the domain's own rules — the batch replay, the address
// checks, the content that must be read back out of the row it patches — all of
// which have to happen INSIDE the snapshot, because every one of them is a
// decision about state another writer is changing at the same time.
//
// # Why the actor is on the writer and never on the call
//
// A chart whose author field is chosen by the caller is not an audit trail, and
// a reorganisation is precisely the change a company most needs one of. So a
// surface acts as exactly one party: the identity comes from the surface's own
// IMMUTABLE context — the seat bound into a turn, the credential on a request —
// and a surface serving many parties takes one writer per party through
// [Writer.As]. Neither a model nor a request body can reach it.
//
// A WRITER WITH NO ACTOR IS REFUSED at construction rather than writing an
// empty author column: an unattributed change in this domain is one nobody can
// be asked about, and "" is indistinguishable from a surface that forgot.

// Writer is one party's authority to change the chart.
type Writer struct {
	publisher *statelog.Publisher

	// db is the replicated estate. It is never the write path's own
	// snapshot — that is the framework's, taken per append — and nothing
	// decided against it is paired with an expectation.
	db *store.DB

	// Actor and ActorKind are who this writer acts as, and OperatorID and
	// TurnID the provenance that travels with it. See this file's header
	// for why they are here and not on each call.
	Actor      string
	ActorKind  AuthorKind
	OperatorID string
	TurnID     string

	// Revision is the company configuration revision a write came from,
	// when it came from one. Its ABSENCE is the answer to "which edit did
	// this": nobody's — somebody did it by hand.
	Revision string

	// after is this writer's own high-water mark, handed from step to step
	// by a gesture that writes twice. See [Writer.After].
	after statelog.Position

	// seal turns a secret-tagged literal into a sealed reference, and is
	// nil on a writer with no secret store behind it — which refuses a
	// write that needs one rather than storing the literal.
	seal Sealer

	// Now is the writer's clock, for the AUTHORED instant only. Nothing
	// this clock produces reaches a row: every instant the applier stores
	// is the broker's, which is what makes one node's copy byte-identical
	// to another's.
	Now func() time.Time
}

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        *store.DB

	// Seal is the secret store's sealing seam. Optional: a company with no
	// secret store configured can still author a chart, and a write that
	// needs sealing refuses by name rather than storing a literal.
	Seal Sealer

	Actor      string
	ActorKind  AuthorKind
	OperatorID string
	TurnID     string
	Revision   string
	Now        func() time.Time
}

// NewWriter builds one party's authority.
func NewWriter(deps WriterDeps) (*Writer, error) {
	if deps.Publisher == nil {
		return nil, fmt.Errorf("chart: a writer needs a publisher — without " +
			"one there is nothing to arbitrate a change against, and a write " +
			"that decided locally would be a chart only this node holds")
	}
	if deps.DB == nil {
		return nil, fmt.Errorf("chart: a writer needs the replicated estate, " +
			"which is where every rule it enforces is read from")
	}
	if deps.Actor == "" {
		return nil, fmt.Errorf("chart: a writer needs an actor — a " +
			"reorganisation is the change a company most needs an audit of, " +
			"and an empty author is indistinguishable from a surface that " +
			"forgot to supply one")
	}
	if !deps.ActorKind.Valid() {
		return nil, fmt.Errorf("chart: %q is not an author kind (want one of "+
			"%v) — the kind is a column precisely so a person, an agent and "+
			"an operator token are never one name three rows apart",
			deps.ActorKind, AuthorKinds())
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Writer{
		publisher: deps.Publisher, db: deps.DB, seal: deps.Seal,
		Actor: deps.Actor, ActorKind: deps.ActorKind,
		OperatorID: deps.OperatorID, TurnID: deps.TurnID,
		Revision: deps.Revision, Now: now,
	}, nil
}

// As is this writer acting as somebody else, and it is the ONLY way the actor
// ever changes.
//
// A COPY RATHER THAN AN ARGUMENT, for the reason this file's header gives: a
// writer acts as one party, and a surface serving many takes one writer each.
func (w *Writer) As(actor string, kind AuthorKind) *Writer {
	next := *w
	next.Actor, next.ActorKind = actor, kind
	return &next
}

// After is this writer's next write waiting for an earlier one of its own.
//
// A GESTURE HANDS THE POSITION FROM STEP TO STEP rather than the writer
// remembering it, because a writer is shared by every sequence on this node: a
// mark it accumulated would make one gesture's step wait for an unrelated
// gesture's record, and a mark scoped to the writer would carry a stale
// position into every later write the surface made.
func (w *Writer) After(at statelog.Position) *Writer {
	next := *w
	next.after = at
	return &next
}

// WriteResult is what a chart write reports back.
type WriteResult struct {
	statelog.Result

	// Objects are what the write touched, in the order the record states
	// them — which is what a surface renders and what a caller polls on.
	Objects []ObjectRef
}

// WriteBatch publishes one structural change.
//
// THE WHOLE BATCH IS ONE RECORD on the structure's one subject, so exactly one
// batch at a time can change the shape of the company — which is what makes a
// cycle impossible rather than merely detectable. See [Batch] for why that
// serialisation is bought deliberately.
func (w *Writer) WriteBatch(ctx context.Context, opID string, batch Batch) (
	WriteResult, error) {

	if opID == "" {
		return WriteResult{}, fmt.Errorf("chart: a batch needs an operation " +
			"id — it is what makes a retry after a lost acknowledgement land " +
			"once rather than twice")
	}
	if len(batch.Operations) == 0 {
		return WriteResult{}, fmt.Errorf("chart: the batch holds no " +
			"operations, so there is nothing to arbitrate")
	}
	subject := TreeSubject()
	at := w.Now()
	var objects []ObjectRef
	// THE SCOPE IS STATED BEFORE THE DECIDE, from what the operations
	// NAME: the framework's deferral probe runs first, and a scope derived
	// from what the replay concludes would be formed after the question it
	// answers. See [Batch.Scope] for why a superset is the safe direction.
	declared := batch.Scope()

	result, err := w.publish(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    declared.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			// EVERY RULE INSIDE THE SNAPSHOT. The replay reads the
			// chart's whole structure out of this transaction and
			// checks each operation against the state the ones before
			// it produced — which is the only reading under which a
			// batch that creates a unit and then fills it is valid,
			// and the only one under which two moves that jointly
			// close a cycle are refused.
			edges, removed, err := batch.Validate(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			objects = objectsOf(edges, removed)
			// A REMOVAL IS ITS OWN RECORD and cannot ride with a
			// placement, because [Domain.InstallsGate] is answered
			// from the ENVELOPE: a record that installed a gate for
			// some of its objects and not others would make that
			// question one about a payload a stopped node cannot
			// read. A batch that does both publishes the placement
			// here and the removal through [Writer.WriteRemoval],
			// which the caller sequences with [Writer.After].
			if len(removed) > 0 && len(edges) > 0 {
				return statelog.Decision{}, fmt.Errorf("chart: the batch both "+
					"places and removes objects, and the two are different "+
					"records: a removal installs a gate, which has to be "+
					"answerable from the envelope alone. Publish the "+
					"placements first and the removals after them: %w",
					ErrRefused)
			}
			if len(removed) > 0 {
				return w.decideRemoval(subject, opID, at, removed, batch.Reason)
			}
			if len(edges) == 0 {
				// A BATCH THAT CHANGES NOTHING SUCCEEDS. An empty
				// decision is a legitimate outcome rather than an
				// error: a caller re-submitting a chart that already
				// holds should be told it landed.
				return statelog.Decision{}, nil
			}
			return w.decidePlacement(subject, opID, at, edges)
		},
	})
	return WriteResult{Result: result, Objects: objects}, err
}

// WriteRemoval takes objects out of the chart.
//
// ITS OWN CALL rather than an operation inside a placement batch, because a
// removal INSTALLS A GATE and that has to be answerable from the envelope
// alone — see the refusal in [Writer.WriteBatch]. The batch's own rules still
// apply: a unit that still holds anything is refused, because an orphaned
// subtree is reachable from nothing and removable by nothing.
func (w *Writer) WriteRemoval(ctx context.Context, opID string, batch Batch) (
	WriteResult, error) {

	if opID == "" {
		return WriteResult{}, fmt.Errorf("chart: a removal needs an operation id")
	}
	subject := TreeSubject()
	at := w.Now()
	var objects []ObjectRef
	declared := batch.Scope()

	result, err := w.publish(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    declared.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			edges, removed, err := batch.Validate(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			if len(edges) > 0 {
				return statelog.Decision{}, fmt.Errorf("chart: a removal "+
					"batch states %d placement(s), and the two are different "+
					"records: %w", len(edges), ErrRefused)
			}
			if len(removed) == 0 {
				return statelog.Decision{}, nil
			}
			objects = removed
			return w.decideRemoval(subject, opID, at, removed, batch.Reason)
		},
	})
	return WriteResult{Result: result, Objects: objects}, err
}

// decidePlacement forms the structural record.
func (w *Writer) decidePlacement(subject Subject, opID string, at time.Time,
	edges []Edge) (statelog.Decision, error) {

	terms := make([]ScopeTerm, 0, len(edges))
	for _, edge := range edges {
		switch edge.Object.Kind {
		case KindUnit:
			terms = append(terms, ScopeTerm{Kind: TermUnit, ID: edge.Object.ID})
		case KindSeat:
			terms = append(terms, ScopeTerm{
				Kind: TermSeat, Unit: edge.Parent, ID: edge.Object.ID})
		}
	}
	scope := BatchScope(terms)
	return w.record(subject, OpPlace, opID, at, scope,
		PlacementPayload{V: DocumentVersion, Edges: edges})
}

// decideRemoval forms the gate-installing record.
func (w *Writer) decideRemoval(subject Subject, opID string, at time.Time,
	removed []ObjectRef, reason string) (statelog.Decision, error) {

	terms := make([]ScopeTerm, 0, len(removed))
	for _, ref := range removed {
		switch ref.Kind {
		case KindUnit:
			terms = append(terms, ScopeTerm{Kind: TermUnit, ID: ref.ID})
		case KindSeat:
			// A REMOVED SEAT'S TERM IS FILED UNDER THE ORG ROOT, not
			// under the unit it was in: the record is the one that
			// takes it out, so a term naming its old unit would claim
			// the removal makes that whole unit stale. It does not —
			// what it makes stale is the seat.
			terms = append(terms, ScopeTerm{Kind: TermSeat, ID: ref.ID})
		}
	}
	scope := BatchScope(terms)
	// PINNED AT [GateRecordVersion] FOR EVER. This is the one record here a
	// node must not be able to defer, so its shape can never be one a node
	// might not know.
	return w.record(subject, OpRemove, opID, at, scope,
		RemovePayload{V: GateRecordVersion, Objects: removed, Reason: reason})
}

// record encodes one record with this writer's own provenance.
//
// EVERY RECORD GOES THROUGH HERE, which is what makes the actor, the kind, the
// operator id, the turn and the revision one set rather than five fields each
// write remembers to fill. A path that built a record itself would be the one
// that forgot the author column.
func (w *Writer) record(subject Subject, op OpKind, opID string, at time.Time,
	scope ScopeSet, payload any) (statelog.Decision, error) {

	body, err := marshal(payload)
	if err != nil {
		return statelog.Decision{}, err
	}
	rec := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Scope: scope,
		},
		Mutation:   body,
		Actor:      w.Actor,
		ActorKind:  w.ActorKind,
		OperatorID: w.OperatorID,
		TurnID:     w.TurnID,
		Revision:   w.Revision,
	}
	if op == OpRemove {
		rec.V = GateRecordVersion
	}
	encoded, err := Encode(rec)
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{
		Payload: encoded,
		Envelope: statelog.Envelope{
			V: rec.V, Kind: string(subject.Kind),
			Subject: statelog.Subject{
				Kind: string(subject.Kind), ID: subject.ID,
			},
			Op: string(op), OpID: opID, Scope: scope.Resolve(subject),
		},
	}, nil
}

// publish runs one request and translates the framework's refusals into this
// domain's own vocabulary.
//
// THE THREE OUTCOMES TRAVEL UNCHANGED. What this adds is the one thing the
// framework cannot know: which of its refusals a caller can act on. A refused
// create is an address already taken, and an unknown outcome is a resolution
// rather than a retry.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (
	statelog.Result, error) {

	req.Session = w.after
	result, err := w.publisher.Publish(ctx, req)
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, statelog.ErrExists):
		return result, fmt.Errorf("chart: %s already exists: %w",
			req.Subject.ID, err)
	}
	return result, err
}

// objectsOf is every object a decision touched, placements first.
func objectsOf(edges []Edge, removed []ObjectRef) []ObjectRef {
	out := make([]ObjectRef, 0, len(edges)+len(removed))
	for _, edge := range edges {
		out = append(out, edge.Object)
	}
	return append(out, removed...)
}

// wire is a subject as the framework addresses it.
func wire(s Subject) statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}
