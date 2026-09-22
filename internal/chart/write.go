package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
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

	// holders reads who, in the identity directory, is bound to a seat.
	// Nil is a node that does not run that domain, and every seat removal
	// is then refused naming the node — see [Holders].
	holders Holders

	// Actor and ActorKind are who this writer acts as, and OperatorID and
	// TurnID the provenance that travels with it. See this file's header
	// for why they are here and not on each call.
	Actor      string
	ActorKind  AuthorKind
	OperatorID string
	TurnID     string

	// Grants is what this writer's party is entitled to, and it is read by
	// exactly one thing: [Writer.mayAuthor], which refuses a record the
	// party may not publish. See grant.go for why the domain decides that
	// half at all, and why it is not a second opinion about the half
	// internal/authz decides.
	//
	// A WRITER WITH NONE IS A REAL PARTY rather than a misconfiguration —
	// an agent editing its own team's content holds no capability and
	// should not — so the zero value is fail-closed instead of refused:
	// it may author the public half and nothing else.
	Grants []iam.Grant

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
	Grants     []iam.Grant
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
		Grants:     slices.Clone(deps.Grants),
		OperatorID: deps.OperatorID, TurnID: deps.TurnID,
		Revision: deps.Revision, Now: now,
	}, nil
}

// As is this writer acting as somebody else, and it is the ONLY way the actor
// ever changes.
//
// A COPY RATHER THAN AN ARGUMENT, for the reason this file's header gives: a
// writer acts as one party, and a surface serving many takes one writer each.
//
// THE GRANTS TRAVEL WITH THE ACTOR AND ARE NOT OPTIONAL, because they are a
// property of the party rather than of the writer this one was cloned from.
// Carrying the previous party's grants forward is the defect this signature
// exists to make unwritable: a surface resolving an anonymous caller would
// hand them whatever the node itself holds. A party with no capabilities
// passes nil and may author the public half, which is the honest answer for
// an agent editing its own team.
// WithHolders installs the identity directory this writer consults before a
// seat removal, and returns the writer for chaining.
//
// CALLED ONCE AT WIRING TIME, like [Writer.As]'s siblings: the seam is read
// inside a decide, and a writer whose directory moved under a write in flight
// would decide two removals two ways.
//
// NIL IS A NODE THAT DOES NOT RUN THE IDENTITY DOMAIN, and every seat removal
// through it is then REFUSED naming the node rather than allowed — see
// [Holders].
func (w *Writer) WithHolders(h Holders) *Writer {
	w.holders = h
	return w
}

func (w *Writer) As(actor string, kind AuthorKind, grants []iam.Grant) *Writer {
	next := *w
	next.Actor, next.ActorKind = actor, kind
	next.Grants = slices.Clone(grants)
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
			edges, removed, err := batch.Validate(ctx, tx, w.holders)
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
			edges, removed, err := batch.Validate(ctx, tx, w.holders)
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

	// THE DOMAIN'S OWN HALF OF THE AUTHORITY QUESTION, asked here because
	// this is the one funnel every decide in this package reaches. See
	// grant.go for what it decides and what it deliberately does not.
	if err := w.mayAuthor(classOf(payload)); err != nil {
		return statelog.Decision{}, err
	}
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

// WriteRekey moves one key onto one object, keeping the old one resolving.
//
// ON THE KEY'S OWN SUBJECT, create-only at an expectation of zero, for the
// reason a page's create arbitrates on its title: two objects taking one
// address must CONTEND, and two objects' own subjects never would. A rekey
// published on the object's subject would let two renames onto one address
// both succeed, and the chart would then hold two objects answering to it.
//
// THE FORMER KEY IS STATED rather than read, because the apply RETIRES that
// claim and a retirement the record did not state would be a row rewritten on
// one node's authority rather than the log's.
func (w *Writer) WriteRekey(ctx context.Context, opID string, object ObjectRef,
	former string) (WriteResult, error) {

	if opID == "" {
		return WriteResult{}, fmt.Errorf("chart: a rekey needs an operation id")
	}
	key := NormalizeKey(object.ID)
	was := NormalizeKey(former)
	switch {
	case key == "":
		return WriteResult{}, fmt.Errorf("chart: a rekey names no new key")
	case was == "":
		return WriteResult{}, fmt.Errorf("chart: the rekey onto %q retires "+
			"nothing — a claim that moves no reference leaves two addresses "+
			"that both resolve: %w", key, ErrRefused)
	case was == key:
		return WriteResult{}, fmt.Errorf("chart: the rekey onto %q retires "+
			"itself: %w", key, ErrRefused)
	case object.Kind != KindUnit && object.Kind != KindSeat:
		return WriteResult{}, fmt.Errorf("chart: only a unit and a seat hold "+
			"a key, and this rekey names a %s: %w", object.Kind, ErrRefused)
	}
	subject := RekeySubject(key)
	at := w.Now()
	// THE SCOPE NAMES THE OBJECT, not the key: a claim's subject is an
	// address and the apply writes the object's row, so a scope built from
	// the subject would file the record's blast radius under something that
	// has no row at all.
	scope := BatchScope([]ScopeTerm{scopeTermFor(object, "")})

	result, err := w.publish(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		// FIRST-WRITER-WINS ON THE NEW ADDRESS, which is what makes two
		// renames onto one key contend: the second sees the claim and is
		// refused rather than overwriting it.
		Pattern: statelog.PatternCreate,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			// THE OBJECT HAS TO BE THERE, read in this snapshot: a
			// rekey of something that has already been removed would
			// claim an address for an object the apply then cannot
			// find, leaving the claim standing and nothing holding it.
			present, err := objectPresent(ctx, tx, ObjectRef{
				Kind: object.Kind, ID: was})
			if err != nil {
				return statelog.Decision{}, err
			}
			if !present {
				return statelog.Decision{}, fmt.Errorf("chart: nothing in the "+
					"chart answers to %q, so there is no object to move the "+
					"key %q onto: %w", was, key, ErrRefused)
			}
			// AND THE ADDRESS HAS TO BE FREE, which is the half the broker
			// cannot arbitrate for us. A create files under the STRUCTURE's
			// subject and a claim under the ADDRESS's, so the two never
			// contend: the log will order a create of `infra` after a claim
			// on `infra` was decided, quite legally. Unchecked, the apply
			// then reached `UNIQUE constraint failed: chart_units.key` — on
			// every node, identically, on a record none of them can ever get
			// past, which stalls the whole domain rather than losing one
			// rename. ([Applier.rekeyUnit] declines the same case for the
			// same reason; this is where an operator is TOLD.)
			//
			// A RETIRED ADDRESS COUNTS AS HELD. It still resolves
			// ([resolveUnit]), so letting a second object take it would
			// silently re-point every reference written before the first
			// one moved. The object's OWN retired address does not count:
			// renaming back is a claim on something that already answers to
			// this object.
			holder, held, err := addressHolder(ctx, tx, object.Kind, key)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held && holder != was {
				// TWO SENTENCES, because the two cases send a reader to
				// different places: one names an object they can see at
				// that address, the other an object that is NOT there and
				// answers anyway, which is the whole of what a retired
				// address does and the last thing somebody staring at the
				// chart would work out for themselves.
				because := fmt.Sprintf("%q already holds it", holder)
				if holder != key {
					because = fmt.Sprintf("%q still answers to it, having "+
						"been renamed from it", holder)
				}
				return statelog.Decision{}, fmt.Errorf("chart: %q cannot take "+
					"the address %q: %s — rename or remove %s first, or pick "+
					"another address: %w", was, key, because, holder, ErrRefused)
			}
			return w.record(subject, OpRekey, opID, at, scope, RekeyPayload{
				V: DocumentVersion, Key: key, FormerKey: was,
				Object: ObjectRef{Kind: object.Kind, ID: key},
			})
		},
	})
	return WriteResult{Result: result,
		Objects: []ObjectRef{{Kind: object.Kind, ID: key}}}, err
}

// WriteImport publishes one config revision's whole authored structure.
//
// ITS OWN VERB rather than a large batch, because what an operator reads in
// the log is the difference between "somebody moved a seat" and "a config
// revision rewrote the chart" — and reconstructing that from the SIZE of a
// payload is not reading, it is guessing. It is also what the import ledger is
// keyed on, so re-activating an unchanged revision (the credential-rotation
// gesture, and therefore routine) is a no-op every node reaches the same way.
//
// IT IS QUIET. A revision that touches forty seats would otherwise wake forty
// seats to tell each of them their goal was reworded.
func (w *Writer) WriteImport(ctx context.Context, opID, revision string,
	edges []Edge) (WriteResult, error) {

	if opID == "" {
		return WriteResult{}, fmt.Errorf("chart: an import needs an operation id")
	}
	if revision == "" {
		return WriteResult{}, fmt.Errorf("chart: an import names no revision, "+
			"and the ledger that makes a re-import a no-op is keyed on one: %w",
			ErrRefused)
	}
	subject := TreeSubject()
	at := w.Now()
	terms := make([]ScopeTerm, 0, len(edges))
	objects := make([]ObjectRef, 0, len(edges))
	for _, edge := range edges {
		terms = append(terms, scopeTermFor(edge.Object, edge.Parent))
		objects = append(objects, edge.Object)
	}
	scope := BatchScope(terms)

	result, err := w.publish(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			// THE LEDGER IS CHECKED AT THE APPLY, not here, and that is
			// deliberate: every node has to reach the same no-op, and a
			// decision taken on this node's own rows would have a node
			// that has not applied the previous import publish a second
			// one. The apply is where every node sees the same ledger.
			return w.record(subject, OpImport, opID, at, scope, ImportPayload{
				V: DocumentVersion, Revision: revision, Edges: edges,
			})
		},
	})
	return WriteResult{Result: result, Objects: objects}, err
}

// scopeTermFor is one object's term in this domain's alphabet.
//
// ONE FUNCTION because four writers build them and a seat's term takes a unit
// where a unit's does not — a rule written four times is a record filed under
// a path no probe for it ever looks at.
func scopeTermFor(object ObjectRef, parent string) ScopeTerm {
	if object.Kind == KindSeat {
		return ScopeTerm{Kind: TermSeat, Unit: NormalizeKey(parent),
			ID: NormalizeKey(object.ID)}
	}
	return ScopeTerm{Kind: TermUnit, ID: NormalizeKey(object.ID)}
}

// objectPresent reports whether the chart holds this object, in this
// transaction.
func objectPresent(ctx context.Context, tx *sql.Tx, ref ObjectRef) (bool, error) {
	var table, column string
	switch ref.Kind {
	case KindUnit:
		table, column = "chart_units", "key"
	case KindSeat:
		table, column = "chart_seats", "handle"
	default:
		return false, nil
	}
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM `+table+` WHERE `+column+` = ?`, ref.ID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("chart: read %s %s: %w", ref.Kind, ref.ID, err)
	}
	return true, nil
}
