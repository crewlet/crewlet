package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The gates share a file because they are one category: a rule under which a
// record the broker ACCEPTED produces rows on no node. There are exactly two,
// and a third must not be added casually.
//  1. Deletion: a commit about a purged task applies nowhere, whatever its
//     position, or a late redelivery resurrects rows an operator removed.
//  2. Eviction: an evicted node's commit above its eviction's position is
//     dropped everywhere; it needs only the log's order, so it holds when
//     coordination is unreachable.
// Not gates: a deferral (retained, applied later) and a barrier (writes no
// row). `applied` is not permanent: a marker above P drops a record already
// applied at P (the purge report and marker's reject counter show it). Answers
// independent of arrival order are what make retention-and-reprocess sound.

// Fence refuses a write this node must not make; it is asked before an append.
//
// It asks this node's own applied eviction rows, not coordination: an eviction
// is permitted only once the target's presence lease lapsed (45 s of
// coordination silence), so the evicted node is exactly the one whose
// coordination path is broken. Wrong here is a stream of records every node
// drops while this one collects acknowledgements for them.
//
// No cache fronts the read: a stale `false` is an evicted node going on
// publishing, the failure this type exists to prevent. [doclet.Fence] answers
// the same way.
type Fence struct {
	db     store.ReplicatedReader
	nodeID string

	// Floor is the fleet's published trim floor at a generation and Ends
	// the log's two ends, read live: [Fence.ClearForZero] takes the higher
	// of the floor and the first surviving sequence, and the last says this
	// node is on this log at all.
	//
	// The floor is compared against the caller's cursor, the checkpoint the
	// write's own snapshot read ([statelog.Snap.Checkpoint]), and read at
	// that cursor's generation. The floor theorem says a trimmed record is
	// in the rows the decision was made from, so the applier's live
	// position, which moves forward, would clear a decision that never saw
	// a record; and a floor at another generation names another sequence
	// space.
	Floor func(ctx context.Context, generation uint32) (uint64, error)
	Ends  func(ctx context.Context) (statelog.LogEnds, error)

	// Committed is this node's applier's live checkpoint, which the end is
	// compared against instead of the cursor: where this node's appends land
	// against its applier is a property of the node, not of one decision.
	// See [statelog.ZeroFence].
	Committed func() statelog.Position
}

// NewFence builds the write fence for one node.
func NewFence(db store.ReplicatedReader, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction.
//
// Asked on every append, so it reads the durable row this node's own applier
// wrote (one indexed single-row read), never a network call on every write's
// hot path to the coordination an eviction has already found silent.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	var from, readmitted sql.NullInt64
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM tracker_evictions
			WHERE node_id = ? AND log_stream = ?`,
			f.nodeID, trackerStream).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// UNKNOWN IS NOT "NOT EVICTED": publishing anyway is how an
		// evicted node collects acknowledgements for records every peer
		// drops.
		return false, fmt.Errorf("worklet: read this node's own eviction row: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 <= from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node.
//
// An anchor is absent because the subject was never written, or its record was
// TRIMMED beneath this node, where zero overwrites a mutation nothing recovers.
// So the read is fresh, and unknown refuses: fail-open here is a lost update,
// not the delivery claim's duplicate.
// It takes the higher of the published floor (written before the purge it
// licenses) and the stream's first sequence (covering a floor published at
// another generation, which reads as zero), as the floor theorem in [statelog]
// does. The check is [statelog.ZeroFence]; this supplies its four reads.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// Gates answers whether a durable record produced rows on NO node.
//
// Without it the resolution rule reads an empty operation ledger as "somebody
// else won": the writer republishes, is dropped by the same gate again, and
// burns its round budget to a conflict a model reads as a colleague's edit.
type Gates struct {
	db store.ReplicatedReader
}

// NewGates builds the gate reader for one node.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at a position applies nowhere, and the
// gate that answers for it, by the rule [statelog.Gates] states (the
// knowledge base's reader keeps it too; statelogtest.RunGates certifies both).
//
// The body is not read: every record about an object is published on its
// subject (or its tree's, which the marker covers), so the subject names what
// the deletion gate is keyed on.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject, writer, opID string,
	p statelog.Position, _ []byte) (statelog.Reason, bool, error) {

	var reason statelog.Reason
	var gated bool
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		// THE DELETION GATE FIRST, per [statelog.Gates]: the marker holds
		// the task for every writer for ever, while an eviction is one
		// writer's and a readmission ends it, so `deleted` is the answer
		// that stays true. The applier's opposite order decides only which
		// gate a drop is COUNTED under. The subjects are [markedTask]'s,
		// the function the applier asks too.
		if taskID, ok := markedTask(subj); ok {
			var author sql.NullString
			err := tx.QueryRowContext(ctx,
				`SELECT purge_record_id FROM tracker_deletions WHERE task_id = ?`,
				taskID).Scan(&author)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("worklet: read the deletion gate: %w", err)
			case author.Valid && author.String == opID:
				// THE RECORD THAT WROTE THE MARKER IS NOT GATED BY IT,
				// by its own id, or a purge whose acknowledgement was
				// lost resolves as "applied nowhere", when it did.
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
			return fmt.Errorf("worklet: read the eviction gate: %w", err)
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

// markedTask is the task whose deletion marker gates a record on subj, and
// false for a subject no task's marker covers.
//
// It covers the task's own subject and its TURNS' ([TurnSubject]): a turn
// records its spend when it ENDS, often after its task's purge, and ungated it
// would reach an apply with no row to add to, a malformed record that stops
// the log on every node. [Applier.Gated] drops and [Gates.GatedAt] reports by
// this one function, which [statelog.Gates] holds to one answer. It reads the
// subject alone because the reader is handed nothing else.
func markedTask(subj statelog.Subject) (string, bool) {
	if ObjectKind(subj.Kind).GatedByPurge() {
		return subj.ID, true
	}
	return "", false
}

// GateRecordVersion is the SHAPE version every gate-installing record's
// payload carries, FOR EVER.
//
// A node that deferred a gate would go on applying every record it gates, with
// no inverse to repair it, so an un-decodable gate record STOPS the applier
// until the node is upgraded, and pinning the shape (it only grows by addition)
// means no shape change can cost a stop. A change to what a gate's APPLY does
// is written instead at a RECORD version above every older build (a
// [versionedFields] row carrying [statelog.VersionedField.Gate]), where the
// framework halts that build. It keeps its kind and op: [Domain.InstallsGate] reads the envelope, and one an
// older build does not know is no gate to it, deferred or retried for ever.
const GateRecordVersion = 1

// purgeMutation is a purge's payload.
type purgeMutation struct {
	V      int    `json:"v"`
	Reason string `json:"reason,omitempty"`
}

// PurgeResult is what an operator is told after a purge, in THREE SIBLING
// GROUPS, and with no duration.
//
// A copy on a node that is not applying ends only when the node replays,
// adopts a snapshot, or loses its disk: no clock. The log's minimum trim age
// is a lower bound on how long the log keeps a record, not a ceiling on any
// node's store file, so "gone from every copy within seven days" would be false.
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
	// window its own retention configures. Reporting only: it triggers
	// nothing and says nothing about whether that node was trimmed past.
	Stale []string
}

// purgeWake is what a purge announces, and to whom: the project lead alone,
// for their accountability for the project, hence reason `purged`. The
// snapshot carries no watchers, collaborators or reporter: naming them would
// keep a copy of the destroyed content on the log for its retention window.
//
// Nil when there is no lead; the record still names itself `purged`, because
// the kind is the writer's and not the notification's.
func purgeWake(task Task, collision bool, reason, actor string, leads Leads) *Notify {
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
		Excerpt: purgeExcerpt(task, collision, reason, actor),
	}
}

// maxPurgeKey is the longest task key a purge's notice is measured against: a
// project key of at most ten characters (the config's container-key grammar,
// `[A-Z][A-Z0-9]{1,9}`), a dash, and a sequence number of up to nineteen
// digits — the most an int64 has.
const maxPurgeKey = 10 + 1 + 19

// purgeExcerpt is the line the lead reads.
//
// It names the key, never the title: quoting the content would keep a copy on
// the log, which a purge exists to prevent. Where another task claimed the key
// first it adds the id ([ItemNamed]), since the key then opens a task that was
// not purged. The operator's reason is kept WHOLE; one too long for the line is
// refused by [Writer.PurgeTask] before anything is destroyed.
func purgeExcerpt(task Task, collision bool, reason, actor string) string {
	out := ItemNamed(task.ID, task.Key, collision) + " was purged"
	if actor != "" {
		out += " by " + actor
	}
	out += " — this cannot be undone"
	if reason = strings.TrimSpace(reason); reason != "" {
		out += ": " + reason
	}
	return out
}

// PurgeTask destroys a task and every row it produced.
//
// No duty ever completes it: destroying data needs the confirmation present at
// that moment, so an interrupted purge did not happen, and re-running it is
// the operator's own gesture.
//
// A task already purged is refused with [ErrNoTask], naming the purge: a retry
// of the purge that did it is answered by its own operation id with that
// purge's outcome, and any other purge of it has nothing left to destroy.
func (w *Writer) PurgeTask(ctx context.Context, opID, id, project, reason string) (WriteResult, error) {
	switch {
	case id == "":
		return WriteResult{}, fmt.Errorf("worklet: a purge names no task")
	case project == "":
		return WriteResult{}, fmt.Errorf("worklet: a purge on task %s names no "+
			"project", id)
	case w.ActorKind != AuthorHuman && w.ActorKind != AuthorOperator:
		// A PERSON OR A TOKEN: the one operation with no inverse is
		// licensed by a person's confirmation.
		return WriteResult{}, fmt.Errorf("worklet: a purge is an operator "+
			"gesture and this writer acts as %q — nothing else may destroy "+
			"a task, because nothing else can be asked to confirm it",
			w.ActorKind)
	}
	// THE REASON FITS ITS ONE-LINE NOTICE WHOLE OR THE PURGE IS REFUSED,
	// before anything is decided. Judged against the LONGEST line this
	// purge could write (the longest key, collided so [ItemNamed] adds the
	// id), so the answer cannot depend on the task's key or collision,
	// which only the decide reads.
	worst := Task{ID: id, Key: strings.Repeat("K", maxPurgeKey)}
	if line := purgeExcerpt(worst, true, reason, w.Actor); len(line) > MaxExcerpt {
		return WriteResult{}, fmt.Errorf("%w: a purge's reason is carried whole on the "+
			"one line its project's lead is told, and that line holds %d bytes; this "+
			"reason makes it %d — say it in fewer words", ErrInvalid, MaxExcerpt, len(line))
	}
	subject := TaskSubject(id)
	// The scope names EVERY TASK ITS APPLY WRITES (dependents, blockers
	// listing it, relating and referencing tasks, its subtree:
	// [purgeReach]) and EVERY KEY IT HANDS ON ([keyTerm]), or a record
	// deferred under any of them is overtaken by the purge on the node
	// that deferred it.
	scope, err := w.scopeForPurge(ctx, id, project)
	if err != nil {
		return WriteResult{}, err
	}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readTask(ctx, tx, id)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				// A TASK ALREADY PURGED IS FINAL, never "not on this
				// node", which the caller retries and every node holding
				// the marker would repeat. A retry of the SAME purge is
				// answered by its ledger row before reaching here.
				return statelog.Decision{}, missingTask(ctx, tx, id,
					"there is nothing left to purge: its deletion marker is "+
						"the account of it that survives")
			}
			// THE PROJECT THE CALLER NAMED IS A PRECONDITION — see
			// [stillIn]: the purge's scope names it, and under the wrong
			// one a deferral on the real project would not hold it back.
			if moved := stillIn(current, project); moved != nil {
				return statelog.Decision{}, moved
			}
			// THE CLAIMED SCOPE STILL COVERS THE ROWS DECIDED ON: a
			// dependent, relation or child arriving since the scope was
			// read is one the apply will write.
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			reach, err := readPurgeReach(ctx, tx, id)
			if err != nil {
				return statelog.Decision{}, err
			}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := scope.coversReach(reach); err != nil {
				return statelog.Decision{}, err
			}
			// AND THE KEYS IT HANDS ON: a move since the scope was read
			// can leave the task under a key the request never probed.
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := scope.coversKeys(keysOf(current)); err != nil {
				return statelog.Decision{}, err
			}
			collision, err := keyHeldByAnother(ctx, tx, current.Key, current.ID)
			if err != nil {
				return statelog.Decision{}, fmt.Errorf("worklet: read whether "+
					"key %s opens task %s: %w", current.Key, current.ID, err)
			}
			decision, err := w.decide(ctx, tx, stamp, subject, OpPurge, ChangePurged, scope, opID,
				purgeMutation{V: GateRecordVersion, Reason: reason},
				purgeWake(current, collision, reason, w.Actor, w.Leads), at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
}

// EvictNode installs the gate that drops a node's records on this log.
//
// The gate is "records this node wrote ABOVE this position", the log's own,
// so every node reaches the same verdict with no clock or coordination read.
//
// Whether the eviction is permitted ([statelog.PermitEviction]) is judged once
// for the whole fleet gesture by the engine's node gate, the one production
// caller of this and [Writer.ReadmitNode], before any log is written: judged
// per log, one gesture could leave a node evicted on one log and counted on
// another. EvictedBy is this writer's actor, so the caller hands it a writer
// acting as the operator who ran the gesture — see [Writer.As].
func (w *Writer) EvictNode(ctx context.Context, opID, nodeID string) (WriteResult, error) {
	return w.gateNode(ctx, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted
// and evicted again reads correctly rather than as one long absence.
//
// Unjudged here for [Writer.EvictNode]'s reason: a node below a trim floor is
// refused a readmission by the engine's node gate, which reads the positions
// register, the published floors and every identity-claiming log once, before
// any log is written — see [statelog.PermitReadmission].
func (w *Writer) ReadmitNode(ctx context.Context, opID, nodeID string) (WriteResult, error) {
	return w.gateNode(ctx, opID, nodeID, true)
}

// gateNode publishes one eviction record.
//
// A RETRY IS ANSWERED BY THE FRAMEWORK from this node's ledger, before this
// decision runs ([statelog.Snap.Held]); what this write adds is the one thing
// the ledger cannot say — whether the record its operation landed is still in
// force, judged from the node's own row in the same snapshot
// ([statelog.GateStanding]).
func (w *Writer) gateNode(ctx context.Context, opID, nodeID string, readmit bool) (WriteResult, error) {
	if nodeID == "" {
		return WriteResult{}, fmt.Errorf("worklet: an eviction names no node")
	}
	subject := EvictionSubject(nodeID)
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		Pattern:  statelog.PatternArbitrated,
		NodeGate: true,
		Standing: func(tx *sql.Tx, held statelog.Position) error {
			row, found, err := standingIn(ctx, tx, nodeID)
			if err != nil {
				return err
			}
			return statelog.GateStanding(opID, nodeID, readmit, held, row, found)
		},
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return w.decide(ctx, tx, stamp, subject, OpEviction, "", scope, opID, Eviction{
				V: GateRecordVersion, NodeID: nodeID,
				EvictedBy: w.Actor, EvictedAt: at, Readmitted: readmit,
			}, nil, at)
		},
	})
}

// standingIn is nodeID's eviction row on this log, read in the transaction the
// caller holds.
func standingIn(ctx context.Context, tx *sql.Tx, nodeID string) (statelog.EvictionRow, bool, error) {
	var from int64
	var readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM tracker_evictions
		WHERE node_id = ? AND log_stream = ?`,
		nodeID, trackerStream).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("worklet: read %s's "+
			"eviction row: %w", nodeID, err)
	}
	return statelog.EvictionRow{
		NodeID: nodeID, From: uint64(from), Readmitted: uint64(readmitted.Int64),
		// THE COMPARISON [Fence.Evicted] MAKES, so a retry and the node's
		// own fence can never disagree about whether it is back.
		Back: readmitted.Valid && readmitted.Int64 > from,
	}, true, nil
}
