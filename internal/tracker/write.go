package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// The write paths, and the one sentence they all follow.
//
//	Take ONE snapshot of your own rows. Decide and form the expectation
//	inside it. Publish. Let the broker arbitrate. Never guess.
//
// Everything below is that sentence with a payload attached. The framework
// owns the loop — the snapshot, the expectation, the rounds, the resolution of
// an ambiguous append — and what a write path contributes is a DECISION
// FUNCTION: given the rows in this transaction, what record should be
// published, or what should the caller be told instead.
//
// # Three outcomes, never a bool
//
// A write is `applied`, `pending` or `unknown`. Applied means the rows are
// here; pending means the record is committed on the log and this node has not
// consumed it yet; unknown means the acknowledgement was lost and the record
// may or may not be there. Collapsing the last two into "failed" is how a
// caller retries a write that landed — and collapsing them into "succeeded" is
// how a caller reports work that was never recorded.

// Writer is the tracker's write authority.
type Writer struct {
	publisher *statelog.Publisher

	// db is the replicated estate, read by the sequences that need to see
	// a subtree BEFORE their first append. It is never the write path's
	// own snapshot — that is the framework's, taken per append — and
	// nothing decided here is paired with an expectation.
	db *store.DB

	// claims is the coordination a walking sequence takes its claim from,
	// and nodeID is who holds it. Both may be nil or empty on a writer
	// that only makes single-append writes; a sequence that needs one
	// refuses by name rather than running without it.
	claims Claims
	nodeID string

	// Actor and ActorKind are who this writer acts as. ON THE WRITER
	// rather than on each call, because a surface acts as exactly one
	// party for its whole life — and a tracker whose author field is
	// chosen per call is not an audit trail.
	Actor     string
	ActorKind AuthorKind

	// metrics is where the counters this package owns are recorded. Nil
	// records nothing, which is what a writer built for a test gets: the
	// instruments are the engine's, and a nil check here is cheaper than
	// a second recorder nobody reads.
	metrics *metrics.Recorder

	// Drain is the applier's measured rows a second on this node, which
	// is the divisor of every projection and every retry hint computed
	// from one. Nil means unmeasured, which the projection reads as its
	// pessimistic floor rather than as infinity.
	Drain func() float64

	// Now is the clock the AUTHORED instants are stamped from. An
	// argument rather than a package call, so a test can pin it and so
	// nothing on the write path reads a clock the applier is forbidden.
	Now func() time.Time
}

// WriterDeps is everything a writer needs that it does not own.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        *store.DB
	Claims    Claims
	NodeID    string
	Metrics   *metrics.Recorder
	Drain     func() float64
	Actor     string
	ActorKind AuthorKind
	Now       func() time.Time
}

// NewWriter builds the tracker's write authority.
func NewWriter(d WriterDeps) (*Writer, error) {
	switch {
	case d.Publisher == nil:
		return nil, fmt.Errorf("tracker: a writer has no publisher")
	case d.Actor == "":
		return nil, fmt.Errorf("tracker: a writer has no actor — every record " +
			"carries who wrote it, and one that does not is a history row " +
			"nobody can attribute")
	case !d.ActorKind.Valid():
		return nil, fmt.Errorf("tracker: %q is not an author kind", d.ActorKind)
	}
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Writer{
		publisher: d.Publisher, db: d.DB, claims: d.Claims, nodeID: d.NodeID,
		metrics: d.Metrics, Actor: d.Actor, ActorKind: d.ActorKind,
		Drain: d.Drain, Now: now,
	}, nil
}

// UpdateTask changes one.
//
// ARBITRATED AGAINST THE TASK'S OWN LAST RECORD, which is what makes two
// writers on one task contend at the broker and two writers on different tasks
// never contend at all.
func (w *Writer) UpdateTask(ctx context.Context, opID, id, project string,
	patch TaskPatch, notify *Notify) (WriteResult, error) {

	switch {
	case id == "":
		return WriteResult{}, fmt.Errorf("tracker: an update names no task")
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: an update on task %s "+
			"names no project — the caller resolved a key to reach this task "+
			"and therefore holds one", id)
	}
	subject := TaskSubject(id)
	// A PROJECT MOVE TOUCHES BOTH CONTAINERS, so it states them: the
	// task's rows leave one project's closure and arrive in another's, and
	// a scope naming only the destination would let a write into the
	// project it left slip past a deferral that covers it.
	scope := ScopeSet{Subject: true, Container: project}
	if patch.Project != nil && *patch.Project != project {
		scope = ScopeSet{Terms: []ScopeTerm{
			{Kind: TermObject, Container: project, ID: id},
			{Kind: TermObject, Container: *patch.Project, ID: id},
		}}
	}
	at := w.Now()

	result, err := w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readTask(ctx, tx, id)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !held {
				return statelog.Decision{}, fmt.Errorf("tracker: task %s is not "+
					"on this node: %w", id, statelog.ErrUnavailable)
			}
			if current.Removed != nil {
				// A TOMBSTONED TASK IS FROZEN — no comment, body, field
				// or relation of it can change — which is what makes a
				// removal an entirely local decision with no walk
				// behind it.
				return statelog.Decision{}, fmt.Errorf("tracker: task %s was "+
					"removed by %s at %s; restore it first",
					id, current.Removed.By, current.Removed.At.Format(time.RFC3339))
			}
			decision, err := w.decide(subject, OpPatch, scope, opID, patch, notify, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	if patch.Body != nil {
		result.Warnings = bodyWarnings(*patch.Body)
	}
	return result, err
}

// MoveTasks repositions tasks in a project's manual order.
//
// # Why the subject is the ORDER and not any of the tasks
//
// The object a drag mutates is the project's ORDER — a total order over its
// tasks that no single task owns and no single task's version can protect. Two
// people dragging two different cards in one project are editing the same
// object, and arbitrating on either card would let both writes land and leave
// the order neither of them intended.
//
// The scope is the project CONTAINER rather than an enumeration, because the
// affected set is the tasks named plus up to a re-spread's worth of
// neighbours: a writer whose set could exceed the term cap states the smallest
// covering term instead, which is what keeps the field bounded by construction
// rather than by a cap a writer can hit and then have to handle.
func (w *Writer) MoveTasks(ctx context.Context, opID, project string,
	placements []Placement) (WriteResult, error) {

	switch {
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a move names no project")
	case len(placements) == 0:
		return WriteResult{}, fmt.Errorf("tracker: a move places no task")
	case len(placements) > MaxBulkTasks+RankRespreadInline:
		// THE CALLER'S OWN MOVES PLUS A RE-SPREAD'S WORTH OF
		// NEIGHBOURS. The scope is the project CONTAINER rather than an
		// enumeration precisely so the placement list is not bounded by
		// the term cap: one drag can legitimately rewrite hundreds of
		// neighbouring keys, and a covering term costs one path.
		return WriteResult{}, fmt.Errorf("tracker: a move carries %d "+
			"placements and one record carries at most %d — %d moves plus "+
			"a re-spread's %d neighbours", len(placements),
			MaxBulkTasks+RankRespreadInline, MaxBulkTasks, RankRespreadInline)
	}
	subject := RankOrderSubject(project)
	scope := ScopeSet{Terms: []ScopeTerm{{Kind: TermContainer, ID: project}}}
	at := w.Now()

	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			for _, placement := range placements {
				if !placement.Rank.Valid() {
					return statelog.Decision{}, fmt.Errorf("tracker: %q is not "+
						"a well-formed rank key", placement.Rank)
				}
			}
			// A RANK MOVE CARRIES NO NOTIFICATION. A reposition is not
			// history: it changes where a card sits and nothing about
			// what the work is, so waking anybody for it would make a
			// board's own drag a source of inbox traffic.
			return w.decide(subject, OpPatch, scope, opID, RankOrder{
				V: DocumentVersion, Project: project, Placements: placements,
			}, nil, at)
		},
	})
}

// WriteDocument publishes a whole-document object.
//
// ONE PATH FOR SEVEN KINDS, because full post-state is one upsert with no
// patch semantics to get wrong — and the kinds that take it are exactly the
// ones small enough for that to be affordable.
func (w *Writer) WriteDocument(ctx context.Context, opID string, subject Subject,
	container string, document any, notify *Notify) (WriteResult, error) {

	if _, _, err := documentTable(subject); err != nil {
		return WriteResult{}, err
	}
	// THE CONTAINER IS TAKEN AND CHECKED RATHER THAN ASSUMED. Most kinds
	// carry their own home in their subject and must not be given a second
	// one; a view and a goal choose theirs. Accepting one where it means
	// nothing would let a caller file a person's record under a project.
	if container != "" && !subject.Kind.HomedInAProject() {
		return WriteResult{}, fmt.Errorf("tracker: a %s names container %q, "+
			"and its own path is derived from its subject — a container here "+
			"would file its deferral where no probe for it looks",
			subject.Kind, container)
	}
	scope := ScopeSet{Subject: true, Container: container}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return w.decide(subject, OpPatch, scope, opID, document, notify, at)
		},
	})
}

// RecordTurn adds a turn's spend to a task.
//
// THE ONE ADDITIVE WRITE: it carries no expectation and races nobody, because
// a turn records something that already happened. Its idempotency is its own
// row's insert rather than an arbitration.
func (w *Writer) RecordTurn(ctx context.Context, opID, taskID, project string,
	payload any) (WriteResult, error) {

	if project == "" {
		return WriteResult{}, fmt.Errorf("tracker: a turn on task %s names "+
			"no project — a turn's path is its task's, so one without a project "+
			"files under a container the task is not in", taskID)
	}
	subject := TurnSubject(taskID)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternAdditive,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return w.decide(subject, OpTurn, scope, opID, payload, nil, at)
		},
	})
}

// decide builds the record a write path publishes.
//
// ONE BUILDER for every path, so the envelope's eight reserved keys are filled
// in one place: a path that filled seven of them would publish a record the
// deferral index could not file, and the failure would only show up on a
// rolling upgrade.
func (w *Writer) decide(subject Subject, op OpKind, scope ScopeSet, opID string,
	payload any, notify *Notify, at time.Time) (statelog.Decision, error) {

	if err := notify.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	if scope.Subject && scope.Container == "" && subject.Kind.RequiresAProject() {
		return statelog.Decision{}, fmt.Errorf("tracker: a %s record for %s "+
			"states no container, and there is no %s outside a project — an "+
			"empty one resolves to the workspace and files its deferral where "+
			"no project-scoped probe looks", op, subject, subject.Kind)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("tracker: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	record := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Scope: scope,
		},
		Mutation:  body,
		Actor:     w.Actor,
		ActorKind: w.ActorKind,
		Notify:    notify,
	}
	encoded, err := record.Encode()
	if err != nil {
		return statelog.Decision{}, err
	}
	if len(encoded) > MaxCommitBytes {
		// REFUSED NAMING THE SIZE, never cut to fit: a record silently
		// trimmed is a row that cannot be rebuilt from it, which is the
		// one property the whole record format exists to have.
		return statelog.Decision{}, fmt.Errorf("tracker: the %s record for %s "+
			"is %d bytes and the design maximum is %d — a record is refused "+
			"rather than trimmed, because a trimmed one cannot rebuild its row",
			op, subject, len(encoded), MaxCommitBytes)
	}
	return statelog.Decision{
		Payload:  encoded,
		Envelope: envelopeOf(record, scope, subject),
	}, nil
}

// envelopeOf is the framework's own view of a record.
//
// IT RESOLVES THE SAME SCOPE THE RECORD CARRIES, through the same one-argument
// call every other side uses. The publisher probes the deferral index with the
// request's scope and the applier files a deferral under the envelope's, so a
// second spelling here is a record filed where its own writer never looks.
func envelopeOf(record MutationRecord, scope ScopeSet, subject Subject) statelog.Envelope {
	return statelog.Envelope{
		V:       record.V,
		Kind:    string(subject.Kind),
		Subject: wire(subject),
		Op:      string(record.Op),
		OpID:    record.OpID,
		Scope:   scope.Resolve(subject),
	}
}

// wire is the framework's subject for one of this domain's.
func wire(s Subject) statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// published is [Writer.publish] with the domain's own result around it, for
// the paths whose only extra fact is the body warning — which is every
// single-append path.
func (w *Writer) published(ctx context.Context, req statelog.Request) (WriteResult, error) {
	result, err := w.publish(ctx, req)
	return WriteResult{Result: result}, err
}

// publish runs one request and translates the framework's refusals into the
// caller's own vocabulary.
//
// THE THREE OUTCOMES TRAVEL UNCHANGED. What this adds is the ONE thing the
// framework cannot know: which of its refusals a caller can act on, and what
// to do about it — a refused create is a name already taken, a refused write
// on a removed task is a restore, and an unknown outcome is a resolution
// rather than a retry.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	result, err := w.publisher.Publish(ctx, req)
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, statelog.ErrExists):
		return result, fmt.Errorf("tracker: %s already exists: %w",
			req.Subject.ID, err)
	}
	return result, err
}

// MoveTask drops one task between two neighbours, re-spreading inline when the
// gap has run out of room.
//
// # The gesture, and the one place a rank key can grow without bound
//
// A drag mints a key strictly between the two neighbours it landed between.
// Repeatedly dropping at the same spot subdivides the same gap, and the key
// grows one symbol per halving — so a board somebody keeps re-ordering at one
// point reaches [RankRenormaliseAt] in a few hundred drags. That is the
// designed rate, not a fault: what makes it harmless is that the mint is
// REPLACED by a re-spread rather than allowed to keep growing.
//
// A re-spread rewrites a window of neighbours to evenly spaced short keys and
// carries them in the SAME record as the drag, so the order is never observed
// half-spread. The window is derived from the gap rather than fixed —
// [RespreadWindow] widens until the keys it would produce are short — and it
// stops at [RankRespreadInline]. Past that the drag STILL SUCCEEDS with its
// long key, and the applier flags the project for the duty's paced walk on
// every node: a drag refused because a board is crowded is a person told their
// own board is broken.
func (w *Writer) MoveTask(ctx context.Context, opID, project, taskID string,
	after, before Rank) (WriteResult, error) {

	switch {
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a move names no project")
	case taskID == "":
		return WriteResult{}, fmt.Errorf("tracker: a move names no task")
	}
	placements, err := w.placeBetween(ctx, project, taskID, after, before)
	if err != nil {
		return WriteResult{}, err
	}
	return w.MoveTasks(ctx, opID, project, placements)
}

// placeBetween mints the drag's own key and, when it is too long, the window
// of neighbours that shortens it.
func (w *Writer) placeBetween(ctx context.Context, project, taskID string,
	after, before Rank) ([]Placement, error) {

	key, err := KeyBetween(after, before)
	if err != nil {
		return nil, fmt.Errorf("tracker: mint a key between %q and %q: %w",
			after, before, err)
	}
	if len(key) <= RankRenormaliseAt {
		return []Placement{{Task: taskID, Rank: key}}, nil
	}
	window, err := RespreadWindow(after, before)
	if err != nil {
		return nil, err
	}
	if w.db == nil {
		// NO STORE, NO RE-SPREAD, AND THE DRAG STILL LANDS. The applier
		// flags the project from the key's own length, so the repair is
		// scheduled by the record rather than by whoever wrote it.
		return []Placement{{Task: taskID, Rank: key}}, nil
	}

	var neighbours []Placement
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, rank FROM tracker_tasks
			WHERE project_key = ? AND rank > ? AND rank < ?
			ORDER BY rank, id LIMIT ?`,
			project, string(after), string(before), window)
		if err != nil {
			return fmt.Errorf("tracker: read the re-spread window in %s: %w",
				project, err)
		}
		defer rows.Close()
		for rows.Next() {
			var p Placement
			var rank string
			if err := rows.Scan(&p.Task, &rank); err != nil {
				return fmt.Errorf("tracker: read a re-spread neighbour: %w", err)
			}
			p.Rank = Rank(rank)
			neighbours = append(neighbours, p)
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}

	// THE MOVED TASK TAKES ITS PLACE IN THE WINDOW rather than being
	// appended to it: the whole point of the re-spread is that the record
	// states one consistent order, and a drag written beside a window it
	// is not part of would land between two keys the same record has just
	// moved.
	fresh, err := KeysBetween(after, before, len(neighbours)+1)
	if err != nil {
		return nil, fmt.Errorf("tracker: re-spread %d neighbours between %q "+
			"and %q: %w", len(neighbours), after, before, err)
	}
	placements := make([]Placement, 0, len(fresh))
	placements = append(placements, Placement{Task: taskID, Rank: fresh[0]})
	for i, neighbour := range neighbours {
		placements = append(placements, Placement{
			Task: neighbour.Task, Rank: fresh[i+1],
		})
	}
	return placements, nil
}

// count records one of this package's own counters.
//
// NIL RECORDS NOTHING, deliberately: the instruments belong to the engine,
// which builds one recorder for the whole process, and a writer in a test has
// no reason to carry a second one whose numbers nobody reads.
func (w *Writer) count(name string, attrs metrics.Attrs) {
	if w.metrics != nil {
		w.metrics.Add(name, 1, attrs)
	}
}
