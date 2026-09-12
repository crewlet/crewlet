package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE TWO GATES, and they are in one file because they are ONE CATEGORY: a
// gate is a rule under which a record the broker ACCEPTED produces rows on no
// node. There are exactly two, and a third must not be added casually.
//
//  1. THE DELETION GATE. A commit about a task a purge destroyed applies
//     nowhere, for ever, whatever its position — otherwise a redelivery months
//     later resurrects rows an operator deliberately removed.
//  2. THE EVICTION GATE. A commit written by a node the fleet evicted, at a
//     position above the eviction's own, is dropped everywhere. It depends on
//     nothing but the log's order, which is what makes it the fence that holds
//     when coordination cannot be reached at all.
//
// # Two things that look like gates and are not
//
// A DEFERRAL is not one: a record this build cannot decode is RETAINED at its
// position and applied by a build that can, which is the opposite of dropped.
// A BARRIER is not one either: it writes no row anywhere by design, and a rule
// that dropped it would be a rule about a record that has nothing to drop.
//
// # `applied` is not permanent, and that is stated rather than discovered
//
// "Whatever its position" cuts both ways. A deletion marker committed ABOVE a
// position P retroactively drops a record that already applied at P and was
// truthfully reported `applied` at the time. Resolving through P does not
// close that and must not try: it is a LATER DESTRUCTION of a record that
// genuinely applied, not a false answer at the moment the answer was given.
// The purge report is where a person sees it, and the reject counter on the
// marker is how many records the gate has dropped since.
//
// The order-independence is what makes a late reprocess sound at all:
// retention-and-reprocess rests on a gate that gives the same answer whatever
// order the records arrive in.

// The two seams that answer "should this node be writing at all", and the one
// thing they have in common: both are asked BEFORE an append, and both are
// wrong in a way that is silent.

// Fence refuses a write this node must not make.
//
// # Why it asks THIS NODE'S OWN ROWS and not coordination
//
// An eviction is permitted only while the target's presence lease has LAPSED —
// at least forty-five seconds of coordination silence — so by construction the
// evicted node is the node whose coordination path is not answering. A fence
// that asked coordination would be asking the one source the situation has
// already broken.
//
// So it asks this node's own APPLIED EVICTION ROWS, which its own applier
// wrote from its own log and which are the only source still fresh when
// everything else is wedged. Being wrong here is not a refused write: it is a
// stream of records every node drops while this one collects acknowledgements
// for them.
//
// There WAS a cache in front of that read — "the coordination-derived answer,
// refreshed on the same loop that reads the tombstones", declared as an
// optimisation that "can be stale, because the durable row below is what makes
// staleness safe". Nothing ever assigned it, in this package or any other, and
// the safety argument does not survive being written out: the lookup returned
// the cached answer in BOTH directions, so a stale `false` is precisely an
// evicted node going on publishing — the one failure this type exists to
// prevent, bought to save an indexed single-row read. It was removed rather
// than wired, and [pages.Fence] never had one, so the two fences now answer the
// same question the same way.
type Fence struct {
	db     *store.DB
	nodeID string

	// Floor is the published trim floor, and Cursor this node's own
	// committed position. Both are needed by the one check that costs a
	// round trip.
	Floor  func(ctx context.Context) (uint64, error)
	Cursor func() statelog.Position
}

// NewFence builds the write fence for one node.
func NewFence(db *store.DB, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction.
//
// ON EVERY APPEND, and therefore answered from what this node already knows:
// the durable row its own applier wrote, which is one indexed single-row read
// of a table this node owns. A coordination round trip here would put a
// NETWORK call on the hot path of every write in the company, and it would be
// a call to the estate an eviction has already established is silent.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	var from, readmitted sql.NullInt64
	err := f.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM tracker_evictions
			WHERE node_id = ? AND log_stream = ?`,
			f.nodeID, trackerStream).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// UNKNOWN IS NOT "NOT EVICTED". A store this node cannot read is
		// a node that cannot establish it may write, and publishing
		// anyway is how an evicted node collects acknowledgements for
		// records every peer drops.
		return false, fmt.Errorf("tracker: read this node's own eviction row: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 <= from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node.
//
// # Why this one pays for a fresh answer
//
// An expectation of zero says "this subject holds nothing". It is formed when
// the anchor is absent, and an anchor is absent for two very different
// reasons: the subject was never written, or it was written and the record has
// been TRIMMED away beneath this node. In the second case publishing at zero
// overwrites a mutation nothing can recover.
//
// What makes the first case safe is the floor: if the published trim floor is
// at or below this node's own cursor, then nothing was trimmed that this node
// has not already consumed, so an absent anchor really does mean an empty
// subject. That reading has to be FRESH — a cached floor is a floor that moved
// — and A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is a deliberate
// departure from the fail-open rule the delivery claim uses: failing open
// there is a duplicate delivery and is recoverable, failing open here is a
// lost update and is not.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	evicted, err := f.Evicted(ctx)
	if err != nil {
		return err
	}
	if evicted {
		return fmt.Errorf("tracker: this node is evicted, so a write at an "+
			"expectation of zero would be dropped by every peer: %w",
			statelog.ErrConflict)
	}
	if f.Floor == nil {
		return fmt.Errorf("tracker: no published trim floor is readable, so " +
			"this node cannot establish that an absent anchor means an empty " +
			"subject rather than a record trimmed beneath it")
	}
	floor, err := f.Floor(ctx)
	if err != nil {
		return fmt.Errorf("tracker: read the published trim floor: %w — a floor "+
			"that cannot be read is not a floor that is low, and publishing at "+
			"zero on the guess is a lost update nothing recovers", err)
	}
	if floor > cursor.Seq {
		return fmt.Errorf("tracker: the trim floor is at %d and this node has "+
			"consumed through %d, so an absent anchor may be a record trimmed "+
			"beneath it rather than a subject that was never written: %w",
			floor, cursor.Seq, statelog.ErrUnavailable)
	}
	return nil
}

// Gates answers whether a durable record produced rows on NO node.
//
// Without it the resolution rule reads "the operation ledger is empty, so
// somebody else won": the writer re-decides, republishes, is dropped by the
// same gate again, and burns its whole round budget to a conflict a model
// reads as a colleague editing the same object.
type Gates struct {
	db *store.DB
}

// NewGates builds the gate reader for one node.
func NewGates(db *store.DB) *Gates { return &Gates{db: db} }

// GatedAt reports the gate that dropped a record at a position.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject, writer, opID string,
	p statelog.Position) (statelog.Reason, bool, error) {

	var reason statelog.Reason
	var gated bool
	err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// THE DELETION GATE FIRST, because it is permanent where an
		// eviction can be reversed: a caller told "evicted" retries after
		// a readmission, and a caller told "deleted" never should.
		if ObjectKind(subj.Kind) == KindTask {
			var author sql.NullString
			err := tx.QueryRowContext(ctx,
				`SELECT purge_record_id FROM tracker_deletions WHERE task_id = ?`,
				subj.ID).Scan(&author)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("tracker: read the deletion gate: %w", err)
			case author.Valid && author.String == opID:
				// THE RECORD THAT WROTE THE MARKER IS NOT GATED BY IT,
				// by its own id. Without this a purge whose
				// acknowledgement was lost resolves as "applied
				// nowhere" — and its caller is told the destruction it
				// asked for did not happen, when it did.
			default:
				reason, gated = statelog.ReasonDeleted, true
				return nil
			}
		}
		if writer == "" {
			return nil
		}
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM tracker_evictions
			WHERE node_id = ? AND log_stream = ?`,
			writer, p.Stream).Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("tracker: read the eviction gate: %w", err)
		}
		at := p.Packed()
		if from.Valid && at > from.Int64 &&
			(!readmitted.Valid || at < readmitted.Int64) {
			reason, gated = statelog.ReasonEvicted, true
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, gated, nil
}

// AdoptedAt is when this node's adoption of a donated snapshot completed.
//
// The operation ledger is this node's own and is SCRUBBED from every donated
// snapshot, so an op id minted before this instant cannot be answered for here
// at all — and reading its absence as "somebody else won" would re-decide
// against a row that moved because of this very write.
func (g *Gates) AdoptedAt(ctx context.Context) (time.Time, bool, error) {
	// THE FRAMEWORK'S OWN READER, not a second query against its table.
	// This one had drifted into looking for a row keyed `id = 'current'`
	// on a table keyed on `started_at` — which fails on every call with a
	// missing column rather than reporting no adoption, and the caller
	// then treats a working node as one that cannot answer for anything.
	//
	// It is still THE NODE'S OWN ESTATE that is read: an adoption record
	// is a fact about this machine's history, and a donated snapshot must
	// not carry the recipient's own.
	return statelog.AdoptedAt(ctx, g.db)
}

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER.
//
// # Why this one number never moves
//
// A record whose version this build does not know is normally RETAINED and
// applied later by a build that does. That is exactly wrong for a gate: a node
// that deferred an eviction would leave its own gate table empty and go on
// applying every record the evicted node appends, and there is no inverse that
// repairs it. So an un-decodable gate record STOPS that build's applier
// instead — which only works if a gate record is decodable by every build
// there will ever be, and that is what pinning the version at one buys.
//
// The consequence is deliberate: a gate record's SHAPE can only ever grow by
// addition, never by reshaping, for the life of the deployment.
const GateRecordVersion = 1

// PurgeResult is what an operator is told after a purge, in THREE SIBLING
// GROUPS.
//
// # And there is no duration in it
//
// A purge destroys rows on every node that applies the record. What it cannot
// do is reach a node that is not applying — one that is offline, or evicted,
// or holding a store file nobody has replayed — and a copy on such a disk ends
// in exactly three ways, NONE OF THEM A CLOCK: the node returns and replays,
// it adopts a snapshot (whose install replaces the store file), or the disk is
// replaced or destroyed.
//
// The tempting sentence — "gone from every copy within seven days" — reads the
// log's minimum trim age as a retention ceiling. It is a FLOOR on trimming,
// which is a LOWER bound on how long the log keeps a record, and it says
// nothing whatever about any node's own store file. So the `stale` group below
// is a REPORTING word: the fleet has passed the window the operator's own
// retention configured, and that triggers nothing, evicts nobody, and makes no
// claim about whether that node has been trimmed past.
type PurgeResult struct {
	// Position is where the purge record landed.
	Position statelog.Position

	// Applied are the nodes that have consumed the record and destroyed
	// their rows.
	Applied []string

	// Pending are counted nodes that have not reached it yet. They will:
	// the applier is contiguous, so a node below this position applies it
	// on the way past.
	Pending []string

	// Stale are counted nodes the fleet has not heard from within the
	// window its own retention configures. A REPORTING GROUP: it triggers
	// nothing and says nothing about whether that node has been trimmed
	// past — only that nobody can currently say when it will apply.
	Stale []string
}

// purgeWake is what the one irreversible operation announces, and to whom.
//
// # Why it exists at all
//
// Because it did not, and the absence was invisible from both ends. Every
// purge published with a nil notification, so nobody was ever told that a
// task and every comment, revision, history row and turn record on it had
// been destroyed — while [Candidates] carried a `ChangePurged` branch and
// [ReasonPurged] sat in the reason list, neither of which any record could
// ever reach. Dead code on one side, silence on the other, and the two looked
// like each other's explanation.
//
// # The project lead, and only the project lead
//
// A purge has no assignee to tell — the row is gone, and the snapshot
// deliberately carries no watchers, no collaborators and no reporter, because
// a notification that named them would be a copy of the content the purge
// exists to destroy, retained on the log for its whole retention window. What
// survives is that it HAPPENED, to which key, by whom: the lead's own
// accountability for their project, which is exactly why the reason is
// `purged` rather than `watcher`.
//
// NIL WHEN THERE IS NO LEAD, on [sprintWake]'s rule: a company with nobody to
// tell is told nothing, and the record still names itself `purged` because the
// kind is the writer's and not the notification's.
func purgeWake(task Task, reason, actor string, leads Leads) *Notify {
	if leads == nil {
		return nil
	}
	lead := leads.ProjectLead(task.Project)
	if lead == "" {
		return nil
	}
	return &Notify{
		Kind: ChangePurged,
		Snapshot: Snapshot{
			Key: task.Key, Project: task.Project, ProjectLead: lead,
		},
		Excerpt: purgeExcerpt(task, reason, actor),
	}
}

// purgeExcerpt is the line the lead reads.
//
// IT NAMES THE KEY AND NOT THE TITLE. A purge destroys the content; an
// excerpt quoting it would keep a copy on the log for the whole retention
// window, which is the one thing this operation is for. The key is an
// identifier the person already has in whatever ticket asked for the purge.
//
// The REASON is the operator's own sentence and is kept, cut to fit: it is
// why they did it, and a record of an irreversible act with no reason on it is
// the shape nobody can audit afterwards.
func purgeExcerpt(task Task, reason, actor string) string {
	out := task.Key + " was purged"
	if actor != "" {
		out += " by " + actor
	}
	out += " — this cannot be undone"
	if reason = strings.TrimSpace(reason); reason != "" {
		out += ": " + reason
	}
	return textcut.Within(out, MaxExcerpt)
}

// PurgeTask destroys a task and every row it produced.
//
// THE ONE SEQUENCE NO DUTY EVER COMPLETES, because nothing may destroy data
// without the confirmation present at that moment. A purge interrupted is a
// purge that did not happen, and re-running it is the operator's own gesture
// rather than a repair somebody's cron performs on their behalf.
func (w *Writer) PurgeTask(ctx context.Context, opID, id, project, reason string) (WriteResult, error) {
	switch {
	case id == "":
		return WriteResult{}, fmt.Errorf("tracker: a purge names no task")
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a purge on task %s names no "+
			"project", id)
	case w.ActorKind != AuthorHuman && w.ActorKind != AuthorOperator:
		// A PERSON OR A TOKEN, never an agent and never the engine: this
		// is the one operation with no inverse, and the confirmation
		// that licenses it is a person's.
		return WriteResult{}, fmt.Errorf("tracker: a purge is an operator "+
			"gesture and this writer acts as %q — nothing else may destroy "+
			"a task, because nothing else can be asked to confirm it",
			w.ActorKind)
	}
	subject := TaskSubject(id)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readTask(ctx, tx, id)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, fmt.Errorf("tracker: task %s is "+
					"not on this node: %w", id, statelog.ErrUnavailable)
			}
			decision, err := w.decide(subject, OpPurge, ChangePurged, scope, opID, struct {
				V      int    `json:"v"`
				Reason string `json:"reason,omitempty"`
			}{V: GateRecordVersion, Reason: reason}, purgeWake(current, reason, w.Actor, w.Leads), at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
}

// EvictNode installs the gate that drops a node's records.
//
// # Why the record carries the position and not a time
//
// The gate is "records this node wrote ABOVE this position", and the position
// is the log's own — so every node reaches the same verdict about every record
// with no clock, no coordination read and no agreement beyond the order they
// all already have. That is what makes it the fence that holds when
// coordination cannot be reached at all, which is the only state in which an
// eviction is permitted: the fleet refuses one while the target's presence
// lease is live, so an evicted node has already been silent for at least
// three coordination round trips.
func (w *Writer) EvictNode(ctx context.Context, opID, nodeID string) (WriteResult, error) {
	return w.gateNode(ctx, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted
// and evicted again reads correctly rather than as one long absence.
func (w *Writer) ReadmitNode(ctx context.Context, opID, nodeID string) (WriteResult, error) {
	return w.gateNode(ctx, opID, nodeID, true)
}

func (w *Writer) gateNode(ctx context.Context, opID, nodeID string, readmit bool) (WriteResult, error) {
	if nodeID == "" {
		return WriteResult{}, fmt.Errorf("tracker: an eviction names no node")
	}
	subject := EvictionSubject(nodeID)
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return w.decide(subject, OpEviction, "", scope, opID, Eviction{
				V: GateRecordVersion, NodeID: nodeID,
				EvictedBy: w.Actor, EvictedAt: at, Readmitted: readmit,
			}, nil, at)
		},
	})
}
