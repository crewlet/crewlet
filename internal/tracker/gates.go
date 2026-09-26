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
// NETWORK call on the hot path of every write in the company, and one that
// fails whenever coordination does, which the row this node applied does not.
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
// — and a read that answers unknown refuses. [statelog.VerifyZero] is the
// check, and every refusal it makes names its reason.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.VerifyZero(ctx, f.Evicted, f.Floor, cursor)
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

// GateRecordVersion is the version the eviction record carries, FOR EVER — on
// its envelope ([recordVersionOf]) and in its payload ([Eviction.V]).
//
// # Why this one number never moves
//
// A record whose version this build does not know is normally RETAINED and
// applied later by a build that does. That is exactly wrong for a gate: a node
// that deferred an eviction would leave its own gate table empty and go on
// applying every record the evicted node appends, and there is no inverse that
// repairs it. So an un-decodable gate record STOPS that build's applier
// instead. For the eviction that stop is the outcome worth never paying: an
// eviction is ordinarily written about a node that has already gone silent
// ([Writer.EvictNode]), and a stop would halt more appliers at the same moment.
// Pinned at one, every build decodes it and none stops on it — so its SHAPE can
// only ever grow by addition, never by reshaping, for the life of the
// deployment, and a semantic change takes a new record kind.
//
// # The purge is a gate too, and it is not at one
//
// A purge at [PurgeRecordVersion] applies by a different rule from one at
// [RecordVersion], and is written at its own version for that reason: at one,
// a build that reads one would apply it by the first version's rule. A new
// kind is not open to it the way it is to the eviction — a purge's subject is
// the task it destroys, which is what arbitrates it against every other write
// to that task. So a build that reads below it stops at a purge it cannot
// read, and that stop is the price.
//
// # The generation record is NOT at this version
//
// A reanchor's record ([Generation]) installs no gate ([ObjectKind.InstallsGate]
// is the eviction alone), so nothing here binds it: its envelope is at
// [RecordVersion] and its payload states [DocumentVersion], as every other
// object's does.
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
// task, its comments and its revisions had been destroyed — while
// [Candidates] carried a `ChangePurged` branch and
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
// NIL WHEN THERE IS NO LEAD: a company with nobody to
// tell is told nothing, and the record still names itself `purged` because the
// kind is the writer's and not the notification's. The purge's row in the feed
// carries the same line either way — with no notification to bring it, the
// applier writes it onto the row ([Applier.purgeLine]).
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
// The REASON is the operator's own sentence and is carried WHOLE: it is why
// they did it, and a record of an irreversible act with no reason on it is the
// shape nobody can audit afterwards. So nothing here cuts it — [Writer.PurgeTask]
// refuses a reason longer than [purgeReasonRoom] before anything is published,
// and [Notify.Validate] refuses an excerpt past [MaxExcerpt] behind it.
func purgeExcerpt(task Task, reason, actor string) string {
	out := task.Key + " was purged"
	if actor != "" {
		out += " by " + actor
	}
	out += " — this cannot be undone"
	if reason = strings.TrimSpace(reason); reason != "" {
		out += purgeReasonSeparator + reason
	}
	return out
}

// purgeReasonSeparator is what joins the reason to the rest of the line.
const purgeReasonSeparator = ": "

// purgeReasonRoom is how many bytes of reason fit [purgeExcerpt] whole.
//
// THE LINE IS THE BOUND, not a constant of its own, because the reason shares
// [MaxExcerpt] with the key and the actor it is written beside — the same
// reason fits one task and not another with a longer key. The refusal names
// this number so the operator knows how much to shorten by.
//
// THE REASON IS REFUSED RATHER THAN CUT because this line is where it is read,
// and it is read in two places that must agree: the lead's notification
// carries it as an excerpt, which [Notify.Validate] bounds at [MaxExcerpt], and
// the purge's row in the activity feed carries the same line — the
// notification's, or with no lead to notify, the one the applier writes onto
// the row ([Applier.purgeLine]). A reason cut to fit the notification would
// leave the lead reading a different sentence from the one the feed shows.
//
// Applied whether or not the project has a lead to tell, so that what a purge
// accepts does not change when somebody is appointed lead.
func purgeReasonRoom(task Task, actor string) int {
	return MaxExcerpt - len(purgeExcerpt(task, "", actor)) - len(purgeReasonSeparator)
}

// ErrPurgeReasonTooLong is a purge refused because its reason does not fit
// whole on the line it travels on — see [purgeReasonRoom].
//
// A TYPED ERROR carrying the room, because this is the CALLER's to fix and the
// caller has to be able to tell it from a purge that failed: a surface that
// cannot classify it answers an operator's over-long sentence as a server
// fault. And the room is the number the fix needs, since it differs from one
// task to the next with the length of the key.
type ErrPurgeReasonTooLong struct {
	// Key is the task the purge named.
	Key string

	// Bytes is how long the stated reason is, surrounding space trimmed.
	Bytes int

	// Room is how many bytes of reason fit this task's line.
	Room int
}

func (e *ErrPurgeReasonTooLong) Error() string {
	return fmt.Sprintf("tracker: the reason for purging %s is %d bytes and at "+
		"most %d fit — it travels whole on the one line a purge leaves, and "+
		"nothing is purged until it fits; shorten it", e.Key, e.Bytes, e.Room)
}

// PurgeTask destroys a task: every row it owns, every row through which
// another task refers to it, and the CONTENT of its history and inbox rows —
// who, when and what kind of change stay. [Applier.purgeTask] is the list.
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
			// INSIDE THE DECIDE, because the room depends on the task's
			// key, which is read here — see [purgeReasonRoom].
			if stated, room := len(strings.TrimSpace(reason)),
				purgeReasonRoom(current, w.Actor); stated > room {
				return statelog.Decision{}, &ErrPurgeReasonTooLong{
					Key: current.Key, Bytes: stated, Room: room,
				}
			}
			// THE PAYLOAD STATES THE RECORD'S OWN VERSION, the one
			// [Writer.decide] stamps on the envelope, so the two never
			// disagree about which rule the purge was written for.
			decision, err := w.decide(subject, OpPurge, ChangePurged, scope, opID, struct {
				V      int    `json:"v"`
				Reason string `json:"reason,omitempty"`
			}{V: recordVersionOf(subject, OpPurge, nil), Reason: reason},
				purgeWake(current, reason, w.Actor, w.Leads), at)
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
// coordination cannot be reached at all, which is the state an eviction is
// for: the fleet refuses one while the target's presence lease is live unless
// an operator forces it, so an evicted node has ordinarily stopped reaching
// coordination already.
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
