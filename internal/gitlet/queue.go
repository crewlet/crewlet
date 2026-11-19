package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/words"
)

// A branch whose merge gate requires a check is a QUEUE TARGET
// ([GateRules.QueueTarget]): its changes land through its merge queue, each
// tested merged on the branch first. Every queue write is a record on the
// repository's own subject, decided by its committer ([Committer.ProposeQueue])
// beside the change's writes, so a change is never both queued and closed: a
// close, draft, retarget or landing dequeues its live entry in its own apply,
// and a push moving its head ejects it (head_moved), lest the queue test a head
// nobody enqueued. An enqueue is judged by [JudgeGate] from ONE snapshot, its
// asker the merger (M5-D8), on all but the merge itself, which the candidate
// proves; its landing judges again from its own, a race left deliberate. Every
// op, payload and field is base format ([RecordVersion]) but
// [Enqueue.WorkingFor] and [ChangeRouting.WorkingFor], version 2 and no gate.

// MergeMethod is how a change merges into its target: a merge commit, one
// squashed commit, or a fast-forward of the target to the change's head.
type MergeMethod string

const (
	// MergeMethodMerge merges the change's head with a merge commit.
	MergeMethodMerge MergeMethod = "merge"
	// MergeMethodSquash lands the change as one commit.
	MergeMethodSquash MergeMethod = "squash"
	// MergeMethodFastForward moves the target to the change's head, which
	// must hold the target's tip.
	MergeMethodFastForward MergeMethod = "fast_forward"
)

// MergeMethods is the closed set, the default first.
func MergeMethods() []MergeMethod {
	return []MergeMethod{MergeMethodMerge, MergeMethodSquash, MergeMethodFastForward}
}

// Valid reports a method this build knows.
func (m MergeMethod) Valid() bool { return slices.Contains(MergeMethods(), m) }

// QueueState is where a queue entry stands (code_queue.state).
type QueueState string

const (
	// QueueWaiting is an entry waiting for a candidate to take it.
	QueueWaiting QueueState = "waiting"
	// QueueTesting is an entry a candidate is testing.
	QueueTesting QueueState = "testing"
	// QueueLanded is an entry that landed on its target.
	QueueLanded QueueState = "landed"
	// QueueEjected is an entry the queue put out: a check failed, a merge
	// conflicted, its gate no longer met, its head moved.
	QueueEjected QueueState = "ejected"
	// QueueDequeued is an entry somebody, or its change's own end, took out.
	QueueDequeued QueueState = "dequeued"
)

// QueueStates is the closed set, the live ones first.
func QueueStates() []QueueState {
	return []QueueState{QueueWaiting, QueueTesting, QueueLanded, QueueEjected, QueueDequeued}
}

// Valid reports a state this build knows.
func (s QueueState) Valid() bool { return slices.Contains(QueueStates(), s) }

// Live reports an entry still in its queue: waiting or being tested.
func (s QueueState) Live() bool { return s == QueueWaiting || s == QueueTesting }

// QueueEndReason is why an entry left its queue other than by landing
// (code_queue.end_reason). A named string, so a reason a later build records
// is a value.
type QueueEndReason string

const (
	// EndAsked is a principal's dequeue.
	EndAsked QueueEndReason = "asked"
	// EndClosed is its change closed.
	EndClosed QueueEndReason = "closed"
	// EndDrafted is its change made a draft again.
	EndDrafted QueueEndReason = "drafted"
	// EndRetargeted is its change retargeted onto another branch.
	EndRetargeted QueueEndReason = "retargeted"
	// EndMerged is its change merged other than through its queue — a push
	// that set its target to its head, or moved the target past it.
	EndMerged QueueEndReason = "merged"
	// EjectHeadMoved is its change's head moved by a push since it was
	// enqueued: the queue would test a head nobody enqueued.
	EjectHeadMoved QueueEndReason = "head_moved"
)

// MaxQueueEntries is the most WAITING entries one target's queue holds.
// JUDGMENT: at depth 8, batch 1 and ten-minute checks a queue lands about
// 1 152 changes a day, so more than a day of its fastest capacity waiting is
// broken, not busy; the bound keeps the queue one keyset page and every
// place one seek of code_queue_live_idx.
const MaxQueueEntries = 1024

// MergeRateWindow is the span a queue's merge rate is measured over.
// JUDGMENT (M5-D28): about six ten-minute rounds, so one slow round moves the
// mean little and a burst of enqueues is still seen while it lasts. The home
// counts a target's enqueues within it ([QueueDueQuery.EnqueuedSince])
// against its capacity over it (config.LandingCapacity, at its trains' own
// round), raising code_merge_rate_over_capacity.
const MergeRateWindow = time.Hour

// MergeRateScan is the most enqueues of one target a due-queue read counts
// within [MergeRateWindow]. A queue at both bounds (depth 8, batch 8) lands
// 3 840 changes an hour at one-minute rounds, so for rounds of a minute or
// longer a count stopped here still exceeds the capacity it is compared with;
// only seconds-long rounds have a capacity past it, and there the comparison
// errs quiet. It bounds the read too: one seek of code_queue_enqueued_idx,
// stopped here, never touching another target's entries.
const MergeRateScan = 4096

// QueueEntryID is the entry an enqueue under opID makes, derived from it so an
// enqueue asked again is the same entry, which the ledger collapses.
func QueueEntryID(opID string) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", "code.queue", opID)
}

// Enqueue puts a change in its target's queue at the head and target its
// asker was shown ([QueueProposal]): op enqueue on the repository's subject,
// base format but for [Enqueue.WorkingFor].
type Enqueue struct {
	RepoID string `json:"repo_id"`
	// EntryID is [QueueEntryID] of the proposal's operation id.
	EntryID  string `json:"entry_id"`
	ChangeID string `json:"change_id"`
	// TargetRef and HeadOID are the target and head the asker was shown,
	// which the decide holds the change to.
	TargetRef string `json:"target_ref"`
	HeadOID   string `json:"head_oid"`
	// Method is how it lands; DeleteSource and CloseTask whether its landing
	// deletes its source branch and closes its work item, as its asker
	// resolved them.
	Method       MergeMethod `json:"method"`
	DeleteSource bool        `json:"delete_source,omitempty"`
	CloseTask    bool        `json:"close_task,omitempty"`
	// Principal is the asker, STAMPED by the committer: the merger the gate
	// judged, and whom the queue lands it as.
	Principal Principal `json:"principal"`
	// WorkingFor is whom an agent's enqueue was made for (ADR-0109): the
	// carrier of [QueueProposal.WrittenFor], STAMPED BY THE COMMITTER beside
	// Principal, since an enqueue has no routing; nil for a seat's own work
	// and a person's. The apply keeps it on code_queue.working_for, from which
	// the entry's landing fills its push row ([Land.pushes]), the queue's
	// later records about the entry read it back, and the home's close of the
	// landed change's item is made for them ([QueueWorkEntry.WorkingFor]).
	//
	// VERSION 2, as every payload's working_for is ([versionedFields]).
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`
}

// Dequeue takes a live entry out of its queue at a principal's ask: op
// dequeue on the repository's subject, base format.
type Dequeue struct {
	RepoID  string `json:"repo_id"`
	EntryID string `json:"entry_id"`
	// Principal is the asker, STAMPED by the committer.
	Principal Principal `json:"principal"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER: the change's authors and its enqueuer, told it left the
	// queue — all but who asked.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// QueueProposal is one write of a target's queue as its asker hands it to the
// repository's committer ([Committer.ProposeQueue]): EXACTLY ONE of an
// enqueue, a dequeue or one of the queue's own records (queuerecords.go),
// each decided alone. OpID is the record's operation id, from which an
// enqueue's entry id, a candidate's train and a landing's id derive; Seen is
// the publish's session floor.
type QueueProposal struct {
	OpID string `json:"op_id"`
	// Asker is who asks an enqueue or a dequeue: a person, or a seat whose
	// merge authority, like every approval the gate counts, is judged from the
	// deciding node's chart ([QueueProposal.JudgesChart]). A token bound to
	// nobody merges nothing. The queue's own records are the engine's and name
	// NOBODY: a landing lands each entry as its enqueuer, from the entry's row.
	Asker Principal `json:"asker"`
	// Seen is the code log position the asker read the change at, zero for
	// none. The committer DECIDES AT OR PAST IT, or refuses `behind` about
	// itself, so the runner-up decides.
	Seen statelog.Position `json:"seen,omitzero"`
	// DropEpoch is, for a candidate or a landing, the repository's drop epoch
	// as the copy its home built from held it (internal/gitlet/cache's
	// Synced.DropEpoch), as a push's is ([Proposal.DropEpoch]). The committer
	// refuses another epoch `expunging` ([MergeRefusalExpunging]): an expunge
	// begun since left dropping objects the train may have built over, which
	// its landing would move the target onto. Zero on every other record,
	// which moves no ref onto anything a build made. NEVER RECORDED, as Seen
	// is not, so no record's format moves for it.
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// Ceiling is a seat's DEQUEUE's requester ceiling (M3-decisions U40 (a)):
	// whom its turn works for, judged by the committer beside the seat's own
	// standing from the same chart and snapshot ([whyNoDequeue]). An enqueue
	// carries none: the Gitlet node that forms it judged its merge's ceiling
	// (internal/gitlet/host's merge.go).
	//
	// A proposal is no record, so no versionedFields row; it crosses the
	// estate's wire, and every node reads it ([RecordVersion]).
	Ceiling *WriteCeiling `json:"ceiling,omitempty"`
	// WrittenFor is whom a SEAT'S enqueue or dequeue is made for (ADR-0109),
	// as [ChangeProposal.WrittenFor] is: whoever its turn works for, from the
	// call's bound ceiling (an enqueue's handed on by the Gitlet node forming
	// it), or stated nobody; a person's and the queue's own records carry
	// nobody. Held to the asker by [writtenForInvalid], a dequeue's to its
	// ceiling by [writtenForUnder]; an enqueue's merge by the node forming it.
	//
	// A proposal is no record, so no versionedFields row; its record carries it
	// ([Enqueue.WorkingFor], [ChangeRouting.WorkingFor]). A build that does not
	// know it, its carrier's kind or a named app refuses it naming the field,
	// so the walk asks one that does ([unknownToThisBuild]); an older build's
	// arrives stated nobody ([QueueProposal.UnmarshalJSONFrom]).
	WrittenFor types.WrittenFor `json:"written_for,omitzero"`

	Enqueue      *Enqueue      `json:"enqueue,omitempty"`
	Dequeue      *Dequeue      `json:"dequeue,omitempty"`
	Candidate    *Candidate    `json:"candidate,omitempty"`
	Eject        *Eject        `json:"eject,omitempty"`
	Land         *Land         `json:"land,omitempty"`
	Green        *Green        `json:"green,omitempty"`
	Broken       *Broken       `json:"broken,omitempty"`
	CloseSettled *CloseSettled `json:"close_settled,omitempty"`
}

// engines reports a proposal of one of the queue's OWN records, which its home
// asks and which names no asker.
func (p QueueProposal) engines() bool {
	return p.Candidate != nil || p.Eject != nil || p.Land != nil || p.Green != nil || p.Broken != nil ||
		p.CloseSettled != nil
}

// writes is how many records p carries: exactly one, or it is no proposal.
func (p QueueProposal) writes() int {
	n := 0
	for _, set := range []bool{p.Enqueue != nil, p.Dequeue != nil, p.Candidate != nil, p.Eject != nil,
		p.Land != nil, p.Green != nil, p.Broken != nil,
		p.CloseSettled != nil} {
		if set {
			n++
		}
	}
	return n
}

// QueueResult is what became of one queue proposal.
type QueueResult struct {
	// Outcome is applied, pending or unknown for an appended record, empty
	// for a refusal. Applied is answered from the rows the record wrote.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// EntryID is the entry an enqueue or a dequeue is about.
	EntryID string `json:"entry_id,omitempty"`
	// State is the entry's state once the record applied, Place its place in
	// its target's queue from 1 while live and 0 otherwise — or as a later
	// record left them.
	State QueueState `json:"state,omitempty"`
	Place int        `json:"place,omitempty"`
	// Position is where the record landed (for a retry under its operation
	// id, the record the ledger found), or for a not_applied or repo_deleted
	// refusal made after the append, where that record lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
	// Refusal is why it was refused with no record, Detail its sentence, and
	// Unmet every gate precondition a gate_unmet enqueue did not meet, each
	// naming whom it waits on.
	Refusal MergeRefusal   `json:"refusal,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Unmet   []Precondition `json:"unmet,omitempty"`
	// Refused is every entry of a landing its decide would not land, each
	// with why, beside the landing's own refusal naming the first: what its
	// home ejects before it builds the landing again.
	Refused []RefusedEntry `json:"refused,omitempty"`
}

// RefusedEntry is one entry of a landing its decide would not land: why, its
// one sentence, and every precondition of its change's merge gate it no longer
// meets with its enqueuer as the merger.
type RefusedEntry struct {
	EntryID string         `json:"entry_id"`
	Refusal MergeRefusal   `json:"refusal"`
	Detail  string         `json:"detail"`
	Unmet   []Precondition `json:"unmet,omitempty"`
}

// MergeRefusal is why a repository's committer decided nothing of a merge
// ([MergeProposal]) or a queue write ([QueueProposal]); its asker reads
// "<code>: <sentence>". A named string, so an unknown code is a value rather
// than a panic. NONE IS EVER RECORDED: a refused write appends nothing.
type MergeRefusal string

// The codes, in the order the decides reach them: every write's, a merge's,
// an enqueue's and a dequeue's own, then the committer's.
const (
	// MergeRefusalRepoDeleted is a write to a repository that is deleted,
	// being purged, purged or not there at all.
	MergeRefusalRepoDeleted MergeRefusal = "repo_deleted"
	// MergeRefusalRepoArchived is a write to an archived, read-only
	// repository.
	MergeRefusalRepoArchived MergeRefusal = "repo_archived"
	// MergeRefusalRepoImporting is a write to a repository being imported
	// from another host ([RepoStatusImporting]), which takes no write until
	// its import is done ([ImportingDetail]).
	MergeRefusalRepoImporting MergeRefusal = "repo_importing"
	// MergeRefusalRepoMirrored is a merge, an enqueue, a candidate or a
	// landing on a PULL MIRROR ([RepoRef.Mirrored]), which moves only by its
	// syncs ([MirroredDetail]). A dequeue and an eject move no ref and still
	// land, so its home sheds every entry still queued ([EjectRepoMirrored]).
	MergeRefusalRepoMirrored MergeRefusal = "repo_mirrored"
	// MergeRefusalExpunging is a merge, a candidate or a landing built at
	// another drop epoch than the repository's ([MergeProposal.DropEpoch],
	// [QueueProposal.DropEpoch]): an expunge begun since ([ExpungeBegin]) may
	// retire an object the commit it moves a ref onto reaches. Nothing was
	// decided; its builder builds it again ([ExpungingBuiltDetail]), as a
	// push checked before a begin is refused ([RefusalExpunging]).
	MergeRefusalExpunging MergeRefusal = "expunging"
	// MergeRefusalChangeNotFound is a change the repository does not hold.
	MergeRefusalChangeNotFound MergeRefusal = "change_not_found"
	// MergeRefusalChangeNotOpen is a change closed or merged.
	MergeRefusalChangeNotOpen MergeRefusal = "change_not_open"
	// MergeRefusalChangeDraft is a draft, which merges nowhere.
	MergeRefusalChangeDraft MergeRefusal = "change_draft"
	// MergeRefusalStale is a write formed against a target the change no
	// longer has, or an eject naming its repository a pull mirror it no
	// longer is: the asker reads the change, or the queue, again.
	MergeRefusalStale MergeRefusal = "stale"
	// MergeRefusalHeadMoved is a write formed against a head the change has
	// moved from.
	MergeRefusalHeadMoved MergeRefusal = "head_moved"
	// MergeRefusalTargetMoved is a merge built onto a tip its target has
	// moved from since: what it built merges a branch that is no longer
	// there, and its builder builds it again onto the tip the target holds.
	MergeRefusalTargetMoved MergeRefusal = "target_moved"
	// MergeRefusalTargetMoving is a merge whose target moved under its first
	// build and every one of [MergeRecomputes] rebuilds, or every rebuild its
	// [MergeBuildDeadline] and write attempt had room for: the branch moves
	// faster than a merge is built onto it, so its queue or a quieter moment
	// lands it. Never a decide's: the Gitlet node that built it answers it.
	MergeRefusalTargetMoving MergeRefusal = "target_moving"
	// MergeRefusalNotPerson is a merge past its target's gate asked by
	// anybody but a person: only a person overrides a merge gate.
	MergeRefusalNotPerson MergeRefusal = "not_person"
	// MergeRefusalQueueTarget is a merge into a target whose merges land
	// through its queue — its gate requires a check — asked directly rather
	// than enqueued: only a person's override merges into one directly.
	MergeRefusalQueueTarget MergeRefusal = "queue_target"
	// MergeRefusalNoQueue is an enqueue onto a target that has no queue: its
	// gate requires no check, so a change merges into it directly.
	MergeRefusalNoQueue MergeRefusal = "no_queue"
	// MergeRefusalAlreadyQueued is a change its target's queue holds already.
	MergeRefusalAlreadyQueued MergeRefusal = "already_queued"
	// MergeRefusalQueueFull is a target whose queue holds MaxQueueEntries
	// waiting entries.
	MergeRefusalQueueFull MergeRefusal = "queue_full"
	// MergeRefusalGateUnmet is a change that does not meet its target's
	// merge gate: the result names every precondition unmet
	// ([QueueResult.Unmet], [MergeResult.Unmet]).
	MergeRefusalGateUnmet MergeRefusal = "gate_unmet"
	// MergeRefusalSourceRefused is a merge asked to delete its source branch
	// where a push of that deletion would be refused (a protection, or outside
	// the seats' namespace the merger's grant): the sentence names the push's
	// refusal, and the merge lands keeping its source.
	MergeRefusalSourceRefused MergeRefusal = "source_refused"
	// MergeRefusalUploadExpired is a merge whose pack was uploaded longer
	// ago than a record may still name it (objstore.RecordWithin), as a
	// push's is ([RefusalUploadExpired]): its builder builds it again.
	MergeRefusalUploadExpired MergeRefusal = "upload_expired"
	// MergeRefusalRepoSize is a merge whose pack would take the repository
	// past code.limits.repo_max_bytes, as a push's would ([RefusalRepoSize]).
	MergeRefusalRepoSize MergeRefusal = "repo_size"
	// MergeRefusalCompactionBehind is a merge carrying a pack into a
	// repository holding MaxPacksPerRepo live packs, as a push carrying one
	// is ([RefusalCompactionBehind]): nothing was decided, and it lands when
	// asked again once a compaction has merged the packs.
	MergeRefusalCompactionBehind MergeRefusal = "compaction_behind"
	// MergeRefusalNotQueued is a dequeue of an entry no queue holds live.
	MergeRefusalNotQueued MergeRefusal = "not_queued"
	// MergeRefusalNotPermitted is a dequeue by a principal that is neither
	// the entry's enqueuer, its change's owner, a principal that maintains
	// the repository, nor a person — or a seat's merge, or enqueue, that
	// whoever its turn works for may not make (its Gitlet node judges the
	// write's ceiling, internal/gitlet/host's merge.go).
	MergeRefusalNotPermitted MergeRefusal = "not_permitted"
	// MergeRefusalNotGreen is a landing whose tip, or a green row whose
	// landing, has a required check of its target not passed by its latest
	// attempt: nothing moves on red, nor on what is still running.
	MergeRefusalNotGreen MergeRefusal = "not_green"
	// MergeRefusalNotRed is a broken landing none of whose named checks
	// failed on its commit by its latest attempt.
	MergeRefusalNotRed MergeRefusal = "not_red"
	// MergeRefusalNoLanding is a green or a broken naming no landing above
	// its target's green row: one green already, or none at all.
	MergeRefusalNoLanding MergeRefusal = "no_landing"
	// MergeRefusalRecordFull is a landing whose rows — its entries' own and
	// the live changes their pushes move — would take its record past
	// MaxRecordRows: its home lands fewer entries at once. Never a green's,
	// which writes the same few rows whatever lies beneath it.
	MergeRefusalRecordFull MergeRefusal = "record_full"
	// MergeRefusalBusy is the committer's queue full, or its round lost to a
	// repository that kept changing.
	MergeRefusalBusy MergeRefusal = "busy"
	// MergeRefusalNotApplied is a record that landed and a gate drops on
	// every node.
	MergeRefusalNotApplied MergeRefusal = "not_applied"
	// MergeRefusalUnavailable is a refusal of the code log's for every node.
	MergeRefusalUnavailable MergeRefusal = "unavailable"
)

// MergeRefusals is the closed set, in the order the decides reach them.
func MergeRefusals() []MergeRefusal {
	return []MergeRefusal{MergeRefusalRepoDeleted, MergeRefusalRepoArchived, MergeRefusalRepoImporting,
		MergeRefusalRepoMirrored, MergeRefusalExpunging, MergeRefusalChangeNotFound,
		MergeRefusalChangeNotOpen, MergeRefusalChangeDraft, MergeRefusalStale, MergeRefusalHeadMoved,
		MergeRefusalTargetMoved, MergeRefusalTargetMoving, MergeRefusalNotPerson, MergeRefusalQueueTarget, MergeRefusalNoQueue,
		MergeRefusalAlreadyQueued, MergeRefusalQueueFull, MergeRefusalGateUnmet,
		MergeRefusalSourceRefused, MergeRefusalUploadExpired, MergeRefusalRepoSize,
		MergeRefusalCompactionBehind, MergeRefusalNotQueued, MergeRefusalNotPermitted, MergeRefusalNotGreen,
		MergeRefusalNotRed, MergeRefusalNoLanding, MergeRefusalRecordFull, MergeRefusalBusy,
		MergeRefusalNotApplied, MergeRefusalUnavailable}
}

// Valid reports a code this build knows.
func (r MergeRefusal) Valid() bool { return slices.Contains(MergeRefusals(), r) }

// Validate refuses a proposal no committer could decide ([ErrInvalidProposal],
// naming the field) before it is queued: not exactly one record; a malformed
// operation id, Seen, repository id, entry id (an enqueue's not [QueueEntryID]
// of its operation id), change id, target, head or method; a drop epoch below
// zero or on other than a candidate or a landing; an enqueue's or dequeue's
// asker that is no principal or names a run, or any asker on the queue's own
// records; a written-for its asker may not state or a dequeue's ceiling does
// not bound ([writtenForUnder]: the chart's half is the decide's); a principal
// the asker set, which the committer stamps; the queue's own records' rules
// (queuerecords.go); a record past [EntryRoom]. First, a written-for value this
// build does not know is answered as its decoder answers it on the wire
// ([unknownToThisBuild]), never as invalid: a build that knows it judges it.
func (p QueueProposal) Validate() error {
	_, err := p.measure()
	return err
}

// measure validates p and answers its queued byte charge: its record's payload
// as stamped and at its widest (every landing entry deleting its source),
// encoded and held under [EntryRoom].
func (p QueueProposal) measure() (int, error) {
	if err := p.validFields(); err != nil {
		return 0, err
	}
	op, payload := p.stamped()
	if l, ok := payload.(Land); ok {
		l.Entries = slices.Clone(l.Entries)
		for i := range l.Entries {
			l.Entries[i].DeleteSource = true
		}
		payload = l
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: encode it: %w", ErrInvalidProposal, op, err)
	}
	// Plus the routing the committer stamps on a one-change record; one of
	// several changes keeps what fits ([keptRoutings]).
	bytes := len(encoded)
	if p.Dequeue != nil || p.Broken != nil {
		bytes += MaxRoutingBytes
	}
	if bytes > EntryRoom {
		return 0, refusedf("%w: %s: %d bytes, past the %d one proposal may take of its record",
			ErrInvalidProposal, op, bytes, EntryRoom)
	}
	return bytes, nil
}

// validFields is every rule of [QueueProposal.Validate] but the record's
// bytes.
func (p QueueProposal) validFields() error {
	// A value this build does not know, before any rule (writtenfor.go's head).
	if err := unknownToThisBuild(writtenForAt, p.WrittenFor); err != nil {
		return err
	}
	if p.writes() != 1 {
		return fmt.Errorf("%w: a queue proposal is exactly one of an enqueue, a dequeue, a candidate, an "+
			"eject, a land, a green, a broken and a close_settled", ErrInvalidProposal)
	}
	if err := statelog.CheckCallerOpID(p.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	if p.engines() {
		if p.Asker != (Principal{}) {
			return badField("asker", "the queue's own records are the engine's and name no asker: a landing "+
				"lands each entry as its enqueuer")
		}
	} else {
		switch why := p.Asker.invalid(); {
		case why != "":
			return refusedf("%w: asker: %s", ErrInvalidProposal, why)
		case !p.Asker.Run.IsZero():
			return badField("asker.run", "a queue's write names its asker alone, never a run")
		}
	}
	// A dequeue's carrier is held to its ceiling too, the one queue write a
	// ceiling rides on.
	carried := writtenForInvalid(p.Asker, p.WrittenFor)
	if p.Dequeue != nil {
		carried = writtenForUnder(p.Asker, p.WrittenFor, p.Ceiling, nil)
	}
	if carried != nil {
		return refusedCarrier(ErrInvalidProposal, carried)
	}
	if !p.Seen.IsZero() {
		switch {
		case p.Seen.Stream != topics.CodeLogStream:
			return badField("seen", "%s is a position on another log than the code log (%s)", p.Seen,
				topics.CodeLogStream)
		case p.Seen.Valid() != nil:
			return fmt.Errorf("%w: seen: %w", ErrInvalidProposal, p.Seen.Valid())
		}
	}
	switch {
	case p.DropEpoch < 0:
		return badField("drop_epoch", "%d is no drop epoch: it counts the expunges begun on the repository, "+
			"from 0", p.DropEpoch)
	case p.DropEpoch != 0 && p.Candidate == nil && p.Land == nil:
		return badField("drop_epoch", "only a candidate or a landing carries the drop epoch its build read: "+
			"no other queue record moves a ref onto anything a build made")
	}
	if err := validRepoID("repo_id", p.RepoID()); err != nil {
		return err
	}
	switch {
	case p.Candidate != nil:
		return p.Candidate.validate(p.OpID)
	case p.Eject != nil:
		return p.Eject.validate()
	case p.Land != nil:
		return p.Land.validate(p.OpID)
	case p.Green != nil:
		return p.Green.validate()
	case p.Broken != nil:
		return p.Broken.validate()
	case p.CloseSettled != nil:
		return p.CloseSettled.validate()
	}
	if p.Ceiling != nil {
		switch {
		case p.Dequeue == nil:
			return badField("ceiling", "only a seat's dequeue carries its requester's ceiling: an enqueue's "+
				"is judged by the Gitlet node that forms it")
		case p.Ceiling.invalid(p.Asker) != "":
			return fmt.Errorf("%w: %s", ErrInvalidProposal, p.Ceiling.invalid(p.Asker))
		}
	}
	if d := p.Dequeue; d != nil {
		if err := stampedRouting("dequeue", d.Routing); err != nil {
			return err
		}
		if err := bareUUIDv7("dequeue.entry_id", d.EntryID); err != nil {
			return err
		}
		if d.Principal != (Principal{}) {
			return badField("dequeue.principal", "the committer stamps who dequeued it from the asker, and an "+
				"asker sends none")
		}
		return nil
	}
	e := p.Enqueue
	if want := QueueEntryID(p.OpID); e.EntryID != want {
		return badField("enqueue.entry_id", "%q, and the entry an enqueue under %s makes is %s", e.EntryID,
			p.OpID, want)
	}
	if err := bareUUIDv7("enqueue.change_id", e.ChangeID); err != nil {
		return err
	}
	if why := branchProblem(e.TargetRef); why != "" {
		return badField("enqueue.target_ref", "%s", why)
	}
	switch {
	case !gitref.ValidOID(e.HeadOID) || e.HeadOID == ZeroOID:
		return badField("enqueue.head_oid", "%q is not a whole object id", e.HeadOID)
	case !e.Method.Valid():
		return badField("enqueue.method", "%q is not one of %s", e.Method, words.Or(MergeMethods()))
	case e.Principal != (Principal{}):
		return badField("enqueue.principal", "the committer stamps who enqueued it from the asker, and an asker "+
			"sends none")
	case e.WorkingFor != nil:
		return badField("enqueue.working_for", "the committer stamps whom an enqueue was made for from the "+
			"proposal's written_for, and an asker sends none")
	}
	return nil
}

// record is the op and the payload p carries, as its asker sent it.
func (p QueueProposal) record() (OpKind, any) {
	switch {
	case p.Enqueue != nil:
		return OpEnqueue, *p.Enqueue
	case p.Dequeue != nil:
		return OpDequeue, *p.Dequeue
	case p.Candidate != nil:
		return OpCandidate, *p.Candidate
	case p.Eject != nil:
		return OpEject, *p.Eject
	case p.Land != nil:
		return OpLand, *p.Land
	case p.Green != nil:
		return OpGreen, *p.Green
	case p.Broken != nil:
		return OpBroken, *p.Broken
	case p.CloseSettled != nil:
		return OpCloseSettled, *p.CloseSettled
	}
	return "", nil
}

// stamped is p's op and payload as the committer forms them before its
// decide: the asker as an enqueue's or dequeue's principal, and an enqueue's
// carrier. A landing's stamps are its decide's, from its snapshot.
func (p QueueProposal) stamped() (OpKind, any) {
	op, payload := p.record()
	switch r := payload.(type) {
	case Enqueue:
		r.Principal, r.WorkingFor = p.Asker, p.WrittenFor.Carrier()
		return op, r
	case Dequeue:
		r.Principal = p.Asker
		return op, r
	}
	return op, payload
}

// RepoID is the repository p is about: the ONE answer the router keys p by,
// the committer queues it under and the estate's seam raises the floor of.
func (p QueueProposal) RepoID() string {
	switch {
	case p.Enqueue != nil:
		return p.Enqueue.RepoID
	case p.Dequeue != nil:
		return p.Dequeue.RepoID
	case p.Candidate != nil:
		return p.Candidate.RepoID
	case p.Eject != nil:
		return p.Eject.RepoID
	case p.Land != nil:
		return p.Land.RepoID
	case p.Green != nil:
		return p.Green.RepoID
	case p.Broken != nil:
		return p.Broken.RepoID
	case p.CloseSettled != nil:
		return p.CloseSettled.RepoID
	}
	return ""
}

// EntryID is the entry an enqueue or a dequeue is about, "" for the queue's
// own records, which name several.
func (p QueueProposal) EntryID() string {
	switch {
	case p.Enqueue != nil:
		return p.Enqueue.EntryID
	case p.Dequeue != nil:
		return p.Dequeue.EntryID
	}
	return ""
}

// JudgesChart reports a proposal whose decide reads the deciding node's chart,
// which a node holding none, or confirmed behind the company's configuration,
// never decides: every enqueue, landing and broken, whatever asks (the merge
// gate counts approvals, owners' lead chains and seats' merge grants from the
// chart, [JudgeGate]), and a SEAT'S dequeue, whose grant says whether it may
// take an entry out. ONE ANSWER, which the committer decides by and the estate
// declares the operation's grant by, so the two never disagree.
func (p QueueProposal) JudgesChart() bool {
	return p.Enqueue != nil || p.Land != nil || p.Broken != nil || (p.Dequeue != nil && p.Asker.Kind.readsChart())
}

// queueDecidedRows is the most rows an enqueue or a dequeue makes its apply
// touch: the entry's row and the repository's — and a dequeue's of an entry
// a train is testing, the train it abandons ([trainRows]).
const queueDecidedRows = 2 + trainRows + routingNoticeRows

// trainRows is the most rows abandoning one train touches: its candidates
// removed and its other entries waiting again — [MaxQueueDepth] and
// [MaxCandidateEntries] at most.
const trainRows = MaxQueueDepth + MaxCandidateEntries

// rows is the most rows p's record makes its apply touch, as the committer
// charges its queued item: an enqueue's or a dequeue's [queueDecidedRows], and
// each of the queue's own records its own bound.
func (p QueueProposal) rows() int {
	switch {
	case p.Candidate != nil:
		return candidateDecidedRows
	case p.Eject != nil:
		return ejectDecidedRows(len(p.Eject.Entries))
	case p.Land != nil:
		return landRecordRows(len(p.Land.Entries))
	case p.Green != nil:
		return greenDecidedRows
	case p.Broken != nil:
		return brokenDecidedRows
	case p.CloseSettled != nil:
		return closeSettledDecidedRows(len(p.CloseSettled.Settled))
	}
	return queueDecidedRows
}

// ProposeQueue decides one write of a target's queue as a batch of its own in
// the repository's queue, with a push's bounds ([Committer.Propose]) as a
// change's write ([Committer.ProposeChange]): busy for a full queue or bytes,
// busy undecided for a deadline too short for a batch and its answer;
// [ErrNotServing] from a stopping committer, or one that cannot judge a
// chart-judged write ([QueueProposal.JudgesChart]); [ErrInvalidProposal] for
// no proposal. ctx bounds only this caller's wait.
//
// ITS OPERATION ID IS THE PROPOSAL'S OWN, so a retry at the runner-up after
// this node went silent collapses in the ledger onto the record that landed.
// ITS SESSION IS ITS SEEN. Applied is answered FROM THE ENTRY'S ROW the record
// wrote, never from the decision.
func (c *Committer) ProposeQueue(ctx context.Context, p QueueProposal) (QueueResult, error) {
	bytes, err := p.measure()
	if err != nil {
		c.countQueue(p, QueueResult{}, err)
		return QueueResult{}, err
	}
	deadline, _ := ctx.Deadline()
	a := c.queue(ctx, &queued{queueWrite: &p, bytes: bytes, rows: p.rows(), arrived: c.clock(),
		deadline: deadline, gone: ctx.Done(), answer: make(chan answered, 1)})
	c.countQueue(p, a.queue, a.err)
	return a.queue, a.err
}

// countQueue counts one proposal's answer under its op and what became of it,
// as [changeOutcome] names a change proposal's.
func (c *Committer) countQueue(p QueueProposal, res QueueResult, err error) {
	if c.metrics == nil {
		return
	}
	op, _ := p.record()
	outcome := string(res.Outcome)
	switch {
	case handedOn(err):
		outcome = "handed_on"
	case err != nil:
		outcome = "failed"
	case res.Refusal != "":
		outcome = string(res.Refusal)
	}
	c.metrics.Add(metrics.CodeQueueProposals, 1, metrics.Attrs{"op": string(op), "outcome": outcome})
}

// queueDecision is one round's decision of a queue proposal: its refusal with
// its sentence and unmet preconditions, or its stamped payload; and either way
// the version the repository's row was read at.
type queueDecision struct {
	refusal MergeRefusal
	detail  string
	unmet   []Precondition
	refused []RefusedEntry
	payload any
	version int64
}

// refuse is d refused under code with detail.
func (d queueDecision) refuse(code MergeRefusal, detail string) (queueDecision, error) {
	d.refusal, d.detail = code, detail
	return d, nil
}

// decideQueue is one round's decision of p over the rows tx holds (this
// file's head), pure over them and its arguments: seats is the deciding
// epoch's chart; limits its code.limits (the gate's merge_by floor);
// retention its code.deleted_retention_days, named by a repo_deleted refusal;
// capacity the most waiting entries a target's queue holds (MaxQueueEntries);
// now the clock a pack's upload is judged by.
func decideQueue(ctx context.Context, tx *sql.Tx, p QueueProposal, seats SeatRules, limits Limits,
	retention time.Duration, capacity int, now time.Time) (queueDecision, error) {

	// A DEQUEUE'S CARRIER ON THE DECIDING CHART, before anything is read
	// ([writtenForUnder]): one its ceiling does not bound is its sender's bug,
	// one only this chart refuses a chart that moved under the turn
	// ([ChartRefusal]).
	if p.Dequeue != nil {
		if err := writtenForUnder(p.Asker, p.WrittenFor, p.Ceiling, seats.Roster); err != nil {
			return queueDecision{}, refusedCarrier(ErrInvalidProposal, err)
		}
	}
	repoID := p.RepoID()
	repo, found, err := pushRepoIn(ctx, tx, repoID)
	if err != nil {
		return queueDecision{}, err
	}
	d := queueDecision{version: repo.version}
	switch {
	case !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging:
		detail, err := goneDetail(ctx, tx, repoID, repo, found, retention)
		if err != nil {
			return queueDecision{}, err
		}
		return d.refuse(MergeRefusalRepoDeleted, detail)
	case repo.status == RepoStatusArchived:
		return d.refuse(MergeRefusalRepoArchived, fmt.Sprintf("repository %s is archived and read-only "+
			"(code.repos archived: true): nothing merges into it", repo.name))
	case repo.status == RepoStatusImporting:
		return d.refuse(MergeRefusalRepoImporting, ImportingDetail(repo.name))
	case repo.mirrored && (p.Enqueue != nil || p.Candidate != nil || p.Land != nil):
		// An enqueue, a candidate and a landing are a merge's steps, which a
		// pull mirror never takes. A dequeue and an eject move no ref and still
		// land ([EjectRepoMirrored] sheds its queue); a green and a broken
		// record checks of a landing made before it was a mirror.
		return d.refuse(MergeRefusalRepoMirrored, MirroredDetail(repo.name, repo.managedBy))
	case (p.Candidate != nil || p.Land != nil) && p.DropEpoch != repo.dropEpoch:
		// THE DROP EPOCH before the record's own rules, as a push's: a train or
		// landing read at another epoch may move the target onto an object an
		// expunge begun since left dropping, so its home builds it again.
		what, builder := "train", "its home"
		if p.Land != nil {
			what = "landing"
		}
		return d.refuse(MergeRefusalExpunging, ExpungingBuiltDetail(repo.name, what, builder, p.DropEpoch,
			repo.dropEpoch))
	}
	if p.JudgesChart() && seats.Roster == nil {
		// THE CALLER'S BUG: the committer hands such a write on rather than
		// decide it with no chart.
		return queueDecision{}, fmt.Errorf("gitlet: a queue write the chart judges on %s reached the decide with "+
			"no chart to judge it by — the committer hands such a write to the repository's next committer",
			repoID)
	}
	switch {
	case p.Dequeue != nil:
		return decideDequeue(ctx, tx, p, repo, seats, d)
	case p.Candidate != nil:
		return decideCandidate(ctx, tx, *p.Candidate, repo, limits, now, d)
	case p.Eject != nil:
		return decideEject(ctx, tx, *p.Eject, repo, d)
	case p.Land != nil:
		return decideLand(ctx, tx, *p.Land, repo, seats, limits, d)
	case p.Green != nil:
		return decideGreen(ctx, tx, *p.Green, d)
	case p.Broken != nil:
		return decideBroken(ctx, tx, *p.Broken, repo, seats, d)
	case p.CloseSettled != nil:
		return decideCloseSettled(ctx, tx, *p.CloseSettled, repo, d)
	}
	return decideEnqueue(ctx, tx, p, repo, seats, limits, capacity, d)
}

// decideEnqueue is an enqueue's rules over the rows tx holds (this file's
// head), in the order of their refusals.
func decideEnqueue(ctx context.Context, tx *sql.Tx, p QueueProposal, repo pushRepo, seats SeatRules,
	limits Limits, capacity int, d queueDecision) (queueDecision, error) {

	e := p.Enqueue
	f, found, err := LoadGateFacts(ctx, tx, e.RepoID, e.ChangeID)
	if err != nil {
		return queueDecision{}, err
	}
	if !found {
		return d.refuse(MergeRefusalChangeNotFound, fmt.Sprintf("repository %s holds no change %s", repo.name,
			e.ChangeID))
	}
	c := f.Rows.Change
	named := ChangeRef(repo.name, c.Number)
	switch {
	case c.State == ChangeStateClosed || c.State == ChangeStateMerged:
		return d.refuse(MergeRefusalChangeNotOpen, fmt.Sprintf("%s is %s, and only a live change merges", named,
			c.State))
	case c.State == ChangeStateDraft:
		return d.refuse(MergeRefusalChangeDraft, fmt.Sprintf("%s is a draft: its owner, a principal that "+
			"maintains the repository or a person marks it ready for review first", named))
	case c.TargetRef != e.TargetRef:
		return d.refuse(MergeRefusalStale, fmt.Sprintf("%s merges into %s, and this enqueue was formed against "+
			"%s: read it again", named, c.TargetRef, e.TargetRef))
	case c.Head != e.HeadOID:
		return d.refuse(MergeRefusalHeadMoved, fmt.Sprintf("%s's head is %s, and this enqueue was formed against "+
			"%s: read it again", named, short(c.Head), short(e.HeadOID)))
	}
	f.Roster, f.Floor, f.Merger = seats.Roster, limits.MergeByFloor, p.Asker
	gate, err := f.Gate()
	if err != nil {
		return queueDecision{}, err
	}
	if !gate.QueueTarget() {
		return d.refuse(MergeRefusalNoQueue, fmt.Sprintf("%s has no merge queue — no rule matching it requires "+
			"a check — so %s merges into it directly", c.TargetRef, named))
	}
	live, err := liveEntryOf(ctx, tx, e.ChangeID)
	if err != nil {
		return queueDecision{}, err
	}
	if live != nil {
		return d.refuse(MergeRefusalAlreadyQueued, fmt.Sprintf("%s is %s in %s's queue already (entry %s)", named,
			live.State, live.TargetRef, live.EntryID))
	}
	waiting, err := waitingIn(ctx, tx, e.RepoID, e.TargetRef, capacity)
	if err != nil {
		return queueDecision{}, err
	}
	if waiting >= capacity {
		return d.refuse(MergeRefusalQueueFull, fmt.Sprintf("%s's queue holds %d waiting changes, the most one "+
			"holds (MaxQueueEntries): a queue that far behind lands nothing sooner for one more", c.TargetRef,
			waiting))
	}
	// THE GATE, but the merge into its target, which the candidate proves.
	r := JudgeGate(f, gate)
	for _, pre := range r.Preconditions {
		if pre.State != PreconditionMet && pre.Kind != PreconditionMergeable {
			d.unmet = append(d.unmet, pre)
		}
	}
	if len(d.unmet) > 0 {
		return d.refuse(MergeRefusalGateUnmet, fmt.Sprintf("%s does not meet %s's merge gate: %s", named,
			c.TargetRef, d.unmet[0].Detail))
	}
	_, d.payload = p.stamped()
	return d, nil
}

// decideDequeue is a dequeue's rules over the rows tx holds: a live entry of
// this repository's (not_queued), asked by its enqueuer, its change's owner, a
// principal that maintains the repository or a person (not_permitted).
func decideDequeue(ctx context.Context, tx *sql.Tx, p QueueProposal, repo pushRepo, seats SeatRules,
	d queueDecision) (queueDecision, error) {

	q := p.Dequeue
	entry, found, err := entryIn(ctx, tx, q.RepoID, q.EntryID)
	if err != nil {
		return queueDecision{}, err
	}
	switch {
	case !found:
		return d.refuse(MergeRefusalNotQueued, fmt.Sprintf("repository %s's queues hold no entry %s", repo.name,
			q.EntryID))
	case !entry.State.Live():
		return d.refuse(MergeRefusalNotQueued, fmt.Sprintf("entry %s of %s's queue is %s, and only a live entry "+
			"leaves it", q.EntryID, entry.TargetRef, entry.State))
	}
	handle, kind := p.Asker.party()
	// A person takes any entry out; anybody else is judged. Either way the
	// dequeue is routed below, as a seat's is.
	if kind != PartyPerson {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		why, err := whyNoDequeue(ctx, tx, p, entry, repo, seats)
		if err != nil {
			return queueDecision{}, err
		}
		if why != "" {
			return d.refuse(MergeRefusalNotPermitted, why)
		}
	}
	// WHOM IT CONCERNS: the change's authors and its enqueuer but the asker,
	// beside whom the dequeue was made for.
	_, payload := p.stamped()
	row, found, err := changeRowIn(ctx, tx, entry.ChangeID)
	if err != nil {
		return queueDecision{}, err
	}
	if found {
		out := payload.(Dequeue)
		if out.Routing, err = routingFor(ctx, tx, concerning{repoName: repo.name, change: row, actor: handle,
			actorKind: kind, workingFor: p.WrittenFor.Carrier(), authors: true, dequeued: true,
			enqueuer: entry.EnqueuedBy.reviewer()}); err != nil {
			return queueDecision{}, err
		}
		payload = out
	}
	d.payload = payload
	return d, nil
}

// whyNoDequeue is why a principal that is not a person may not take entry out
// of its queue, empty when it may: it is the entry's enqueuer, its change's
// owner or a principal that maintains the repository — and, for a seat's
// dequeue made for somebody else (p.Ceiling), that somebody may too.
func whyNoDequeue(ctx context.Context, tx *sql.Tx, p QueueProposal, entry queueEntry, repo pushRepo,
	seats SeatRules) (string, error) {

	q := p.Dequeue
	handle, kind := p.Asker.party()
	ownerHandle, ownerKind, err := changeOwnerIn(ctx, tx, entry.ChangeID)
	if err != nil {
		return "", err
	}
	owner := Party{Handle: ownerHandle, Kind: ownerKind}
	// mine reports whether party, holding grant, may take the entry out: its
	// enqueuer, its change's owner, or one that maintains the repository.
	mine := func(party Party, grant Grant) bool {
		return party.Kind != "" && (party == entry.EnqueuedBy || party == owner) || grant.Access >= AccessMaintain
	}
	asker := Party{Handle: handle, Kind: kind}
	grant := GrantOf(p.Asker, repo.grantRef(q.RepoID), seats.Roster)
	if !mine(asker, grant) {
		return fmt.Sprintf("an entry leaves its queue at its enqueuer's ask, its change's owner's, a "+
			"principal's that maintains the repository or a person's, and this asker holds %s on %s",
			grant.Access, repo.name), nil
	}
	// A SEAT'S DEQUEUE IS HELD TO WHOEVER ITS TURN WORKS FOR (Ceiling): its
	// own work (an entry it enqueued, its own change's, or one into its own
	// namespace or a repository it owns) for anybody, as every write's; a
	// colleague to what it may take out itself; nobody to nothing else.
	c := p.Ceiling
	if c == nil || asker == entry.EnqueuedBy || asker == owner || grant.OwnWork(Command{Ref: entry.TargetRef}) {
		return "", nil
	}
	if c.Kind != WriteCeilingSeat {
		return "this work is for whoever started its turn, who may change nothing of this company's code; " +
			"an entry this seat did not enqueue, of a change it does not own, is not its own work", nil
	}
	colleague := Principal{Kind: PrincipalAgent, Seat: c.Seat}
	if cg := GrantOf(colleague, repo.grantRef(q.RepoID), seats.Roster); !mine(Party{Handle: c.Seat,
		Kind: PartyAgent}, cg) {

		return fmt.Sprintf("this work is for seat %s, whose ask started its turn, and an entry leaves its "+
			"queue at its enqueuer's ask, its change's owner's or a principal's that maintains the "+
			"repository: that seat is none of them, holding %s on %s", c.Seat, cg.Access, repo.name), nil
	}
	return "", nil
}

// queueWrite publishes one write of a target's queue and answers it — the
// write's batch of its own, under the committer's own lifetime and one
// CommitBudget, as a change's write is ([Committer.change]).
func (c *Committer) queueWrite(repoID string, q *queued) {
	p := *q.queueWrite
	var seats SeatRules
	if q.judgedByChart() {
		var held bool
		if seats, held = c.seatRules(); !held {
			q.answer <- answered{err: fmt.Errorf("%w: this node holds no chart yet to judge the queue write by; "+
				"the repository's next committer decides it", ErrNotServing)}
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.life, CommitBudget)
	defer cancel()
	op, _ := p.record()
	retention, limits, capacity := c.retention(), c.limits(), c.queueEntries
	subject := RepoSubject(repoID)
	scope := p.scope()
	var (
		last    queueDecision
		decided bool
	)
	res, err := c.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: p.OpID, Pattern: statelog.PatternArbitrated, Session: p.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			last, decided = queueDecision{}, false
			d, err := decideQueue(ctx, tx, p, seats, limits, retention, capacity, c.now())
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.payload == nil {
				return statelog.Decision{Version: d.version}, nil
			}
			decision, err := recordFor(stamp, subject, op, scope, p.OpID, Actor{ID: c.nodeID, Kind: ActorSystem},
				d.payload, c.now())
			if err != nil {
				return statelog.Decision{}, err
			}
			if len(decision.Payload) > MaxRecordBytes {
				return statelog.Decision{}, fmt.Errorf("%w: %d bytes, past MaxRecordBytes (%d)", ErrRecordCap,
					len(decision.Payload), MaxRecordBytes)
			}
			decision.Version = d.version
			return decision, nil
		},
	})
	out, answerErr := c.queueAnswer(ctx, p, last, decided, res, err)
	q.answer <- answered{queue: out, err: answerErr}
	// AN EJECTION IS ANNOUNCED only when its own record applied here, never
	// for a collapsed retry, and after its answer, so neither its asker nor
	// the repository's next write waits on it (announce.go).
	if p.Eject != nil && answerErr == nil && out.Outcome == statelog.OutcomeApplied && !res.Collapsed {
		c.announceEjected(*p.Eject, out.Position)
	}
	// A BROKEN LANDING IS ANNOUNCED the same way: the one event a red tip's
	// bisect leaves (ADR-0098).
	if p.Broken != nil && answerErr == nil && out.Outcome == statelog.OutcomeApplied && !res.Collapsed {
		c.announceBroken(*p.Broken, out.Position)
	}
}

// scope is the scope p's record names: WHAT ITS DECIDE READS (M5-D33), stated
// before it runs, so a node holding a deferred record of any of it decides
// none. An enqueue's gate reads the repository, its checks and the change; a
// LANDING each entry's change too, EVERY ONE BY NAME, never the whole domain
// ([MaxScopePaths] is sized past [MaxLandEntries]); a green and a broken the
// repository and its checks; the rest the repository's rows alone.
func (p QueueProposal) scope() []string {
	repoID := p.RepoID()
	switch {
	case p.Enqueue != nil:
		return []string{RepoPath(repoID), ChecksPath(repoID), ChangePath(p.Enqueue.ChangeID)}
	case p.Land != nil:
		scope := []string{RepoPath(repoID), ChecksPath(repoID)}
		for _, e := range p.Land.Entries {
			scope = append(scope, ChangePath(e.ChangeID))
		}
		return scope
	case p.Green != nil, p.Broken != nil:
		return []string{RepoPath(repoID), ChecksPath(repoID)}
	}
	return []string{RepoPath(repoID)}
}

// queueAnswer is a queue proposal's answer from the publish's result and the
// last round's decision.
func (c *Committer) queueAnswer(ctx context.Context, p QueueProposal, last queueDecision, decided bool,
	res statelog.Result, err error) (QueueResult, error) {

	if err != nil {
		return c.refusedQueue(ctx, p, err)
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() {
			return c.appliedQueue(ctx, p, res.Position)
		}
		if decided && last.refusal != "" {
			return QueueResult{EntryID: p.EntryID(), Refusal: last.refusal, Detail: last.detail,
				Unmet: last.unmet, Refused: last.refused}, nil
		}
		return QueueResult{}, fmt.Errorf("gitlet: the publish of operation %s answered applied with no record, "+
			"and its decide refused nothing", p.OpID)
	case statelog.OutcomePending:
		return QueueResult{Outcome: statelog.OutcomePending, EntryID: p.EntryID(), Position: res.Position}, nil
	}
	return QueueResult{Outcome: statelog.OutcomeUnknown, EntryID: p.EntryID()}, nil
}

// appliedQueue answers a write whose record applied at at, FROM THE ENTRY'S
// ROW: its state, and its place where it is live and its row's last write is
// this record. An entry with no row is a record that applied and wrote what
// this node does not hold: logged at ERROR and answered unknown.
func (c *Committer) appliedQueue(ctx context.Context, p QueueProposal, at statelog.Position) (QueueResult,
	error) {

	if p.engines() {
		return c.appliedQueueRecord(ctx, p, at)
	}
	var (
		entry queueEntry
		found bool
		place int
	)
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		if entry, found, err = entryIn(ctx, tx, p.RepoID(), p.EntryID()); err != nil || !found {
			return err
		}
		if entry.State.Live() {
			place, err = placeIn(ctx, tx, entry)
		}
		return err
	}); err != nil {
		failed := fmt.Errorf("gitlet: the record of operation %s applied at %s and its entry's row could not be "+
			"read: %w", p.OpID, at, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", kindQueue, "repo", p.RepoID(), "op_id", p.OpID,
			"error", failed)
		return QueueResult{}, failed
	}
	if !found {
		c.log.ErrorContext(ctx, "code_queue_unanswerable", "repo", p.RepoID(), "entry", p.EntryID(),
			"op_id", p.OpID, "position", at.String())
		return QueueResult{Outcome: statelog.OutcomeUnknown, EntryID: p.EntryID(), Detail: fmt.Sprintf("the "+
			"write's record applied at %s and the entry it names is not in the rows this node holds", at)}, nil
	}
	return QueueResult{Outcome: statelog.OutcomeApplied, EntryID: entry.EntryID, State: entry.State,
		Place: place, Position: at}, nil
}

// refusedQueue answers a queue proposal whose publish failed — a change's
// mapping ([Committer.refusedChange]) in a queue's words.
func (c *Committer) refusedQueue(ctx context.Context, p QueueProposal, err error) (QueueResult, error) {
	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal):
		c.log.Log(ctx, invalidLevel(err), "code_queue_invalid", "repo", p.RepoID(), "op_id", p.OpID, "error", err)
		return QueueResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		c.countBusy(p.RepoID(), kindQueue, busyContended, 1)
		return QueueResult{EntryID: p.EntryID(), Refusal: MergeRefusalBusy,
			Detail: "the repository kept changing for every round the write may take; " + changeAgain}, nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		return QueueResult{EntryID: p.EntryID(), Refusal: MergeRefusalRepoDeleted, Position: at,
			Detail: c.purgedDetail(ctx, p.RepoID(), p.OpID)}, nil
	case errors.As(err, &refusal) && refusal.Landed():
		return QueueResult{EntryID: p.EntryID(), Refusal: MergeRefusalNotApplied, Position: refusal.Position,
			Detail: fmt.Sprintf("the write applies nowhere: its record at %s was durable and the code log's %s "+
				"gate drops it on every node; %s", refusal.Position, refusal.Reason, changeAgain)}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return QueueResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return QueueResult{EntryID: p.EntryID(), Refusal: MergeRefusalUnavailable,
			Detail: "the code log refused the write: " + strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
	}
	c.log.WarnContext(ctx, "code_commit_failed", "kind", kindQueue, "repo", p.RepoID(), "op_id", p.OpID,
		"error", err)
	return QueueResult{}, fmt.Errorf("gitlet: queue operation %s: %w", p.OpID, err)
}

// queueEntry is one code_queue row as a decide, an answer and a read take it.
type queueEntry struct {
	RepoID, EntryID, TargetRef, ChangeID string
	Seq                                  int64
	HeadOID                              string
	Method                               MergeMethod
	DeleteSource, CloseTask              bool
	EnqueuedBy                           Party
	EnqueuedOperator                     string
	State                                QueueState
	Train, CandidateID, CommitOID        string
	EnqueuedAt, EndedAt                  int64
	EndedPosition                        int64
	EndReason                            QueueEndReason
	// WorkingFor is whom the entry's enqueue was made for
	// (code_queue.working_for, [carrierColumn]), read whole and written back
	// as read. The entry's landing fills its push row from it in its own
	// apply ([Land.pushes]), so every node lands the same rows, and the
	// queue's records about the entry carry it on their routings
	// ([storedCarrier]). Only the home's queue work answers it
	// ([QueueWorkEntry.WorkingFor]); the queue's listings leave it out.
	WorkingFor sql.NullString
}

// scanEntry reads one row [entryColumns] spells.
func scanEntry(scan func(...any) error) (queueEntry, error) {
	var (
		e                           queueEntry
		method, kind, state, reason string
	)
	err := scan(&e.RepoID, &e.EntryID, &e.TargetRef, &e.ChangeID, &e.Seq, &e.HeadOID, &method, &e.DeleteSource,
		&e.CloseTask, &e.EnqueuedBy.Handle, &kind, &e.EnqueuedOperator, &state, &e.Train,
		&e.CandidateID, &e.CommitOID, &e.EnqueuedAt, &e.EndedAt, &e.EndedPosition, &reason, &e.WorkingFor)
	e.Method, e.EnqueuedBy.Kind, e.State, e.EndReason = MergeMethod(method), PartyKind(kind), QueueState(state),
		QueueEndReason(reason)
	return e, err
}

// entryIn reads repository repoID's entry entryID by its key, and false where
// the repository's queues hold no such entry.
func entryIn(ctx context.Context, tx *sql.Tx, repoID, entryID string) (queueEntry, bool, error) {
	e, err := scanEntry(tx.QueryRowContext(ctx, entrySQL, repoID, entryID).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return queueEntry{}, false, nil
	case err != nil:
		return queueEntry{}, false, fmt.Errorf("gitlet: read entry %s of %s's queue: %w", entryID, repoID, err)
	}
	return e, true, nil
}

// liveEntryOf is changeID's LIVE entry, nil where its queue holds none.
func liveEntryOf(ctx context.Context, tx *sql.Tx, changeID string) (*queueEntry, error) {
	live, err := liveEntriesOf(ctx, tx, []string{changeID})
	if err != nil {
		return nil, err
	}
	if e, ok := live[changeID]; ok {
		return &e, nil
	}
	return nil, nil
}

// liveEntriesOf is the LIVE entry of each change of ids that a queue holds, by
// change — one statement over the list, a seek of code_queue_change_idx a
// change. A change holds one live entry at most, since an enqueue of a queued
// change is refused.
func liveEntriesOf(ctx context.Context, tx *sql.Tx, ids []string) (map[string]queueEntry, error) {
	out := map[string]queueEntry{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, liveEntriesSQL, list)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d changes' queue entries: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		e, err := scanEntry(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gitlet: read %d changes' queue entries: %w", len(ids), err)
		}
		out[e.ChangeID] = e
	}
	return out, rows.Err()
}

// waitingIn counts target's WAITING entries on repoID, at most capacity — one
// seek of code_queue_live_idx, its partial predicate spelled as the index
// declares it.
func waitingIn(ctx context.Context, tx *sql.Tx, repoID, target string, capacity int) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, waitingSQL, repoID, target, capacity).Scan(&n); err != nil {
		return 0, fmt.Errorf("gitlet: count %s's waiting entries on %s: %w", target, repoID, err)
	}
	return n, nil
}

// placeIn is a live entry's place in its target's queue, from 1: the live
// entries before it, counted over code_queue_live_idx's range up to it — at
// most MaxQueueEntries waiting, and the candidates' entries before them.
func placeIn(ctx context.Context, tx *sql.Tx, e queueEntry) (int, error) {
	var before int
	if err := tx.QueryRowContext(ctx, placeSQL, e.RepoID, e.TargetRef, e.Seq).Scan(&before); err != nil {
		return 0, fmt.Errorf("gitlet: read entry %s's place in %s's queue: %w", e.EntryID, e.TargetRef, err)
	}
	return before + 1, nil
}

// placesIn is each live entry's place in its target's queue, from 1, by entry
// id: ONE WALK of code_queue_live_idx per (repository, target), up to the
// latest of entries there, each place its rank in the walk. A walk reads at
// most the target's live entries however many of entries are queued there,
// where [placeIn] per entry would read the entries before each one again.
func placesIn(ctx context.Context, tx *sql.Tx, entries []queueEntry) (map[string]int, error) {
	places := make(map[string]int, len(entries))
	latest := map[[2]string]int64{}
	var targets [][2]string
	for _, e := range entries {
		key := [2]string{e.RepoID, e.TargetRef}
		held, seen := latest[key]
		if !seen {
			targets = append(targets, key)
		}
		latest[key] = max(held, e.Seq)
	}
	for _, key := range targets {
		seqs, err := liveSeqsIn(ctx, tx, key[0], key[1], latest[key])
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.RepoID != key[0] || e.TargetRef != key[1] {
				continue
			}
			at, found := slices.BinarySearch(seqs, e.Seq)
			if !found {
				return nil, fmt.Errorf("gitlet: entry %s is live in %s's queue and not among the live entries "+
					"the same snapshot holds there", e.EntryID, e.TargetRef)
			}
			places[e.EntryID] = at + 1
		}
	}
	return places, nil
}

// liveSeqsIn is the seq of every live entry of target's queue on repoID up to
// through, in queue order: one walk of code_queue_live_idx's range, which
// holds them in that order.
func liveSeqsIn(ctx context.Context, tx *sql.Tx, repoID, target string, through int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, liveSeqsSQL, repoID, target, through)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's live queue entries on %s: %w", target, repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's live queue entries on %s: %w", target, repoID, err)
		}
		seqs = append(seqs, seq)
	}
	return seqs, rows.Err()
}

// changeOwnerIn is changeID's owner, by handle and kind.
func changeOwnerIn(ctx context.Context, tx *sql.Tx, changeID string) (string, PartyKind, error) {
	row, found, err := changeRowIn(ctx, tx, changeID)
	if err != nil || !found {
		return "", "", err
	}
	return row.owner, row.ownerKind, nil
}

// The queue's reads, each registered in plan_test.go: one entry by its key; a
// list of changes' live entries, driven from the list, one seek of
// code_queue_change_idx a change; and over code_queue_live_idx a target's
// waiting count, the live entries before one, and its live seqs in queue
// order, each repeating the index's partial predicate verbatim, since the
// pinned planner takes a partial index only for a statement implying it in
// its own words.
const (
	// entryColumns is every column of an entry [scanEntry] reads, in its
	// order.
	entryColumns = `repo_id, entry_id, target_ref, change_id, seq, head_oid, method, delete_source, close_task,
		enqueued_by, enqueued_kind, enqueued_operator, state, train, candidate_id, commit_oid, enqueued_at,
		ended_at, ended_position, end_reason, working_for`
	// qualifiedEntryColumns is entryColumns read beside json_each, which has
	// columns of its own: the same columns, in the same order.
	qualifiedEntryColumns = `q.repo_id, q.entry_id, q.target_ref, q.change_id, q.seq, q.head_oid, q.method,
		q.delete_source, q.close_task, q.enqueued_by, q.enqueued_kind, q.enqueued_operator, q.state, q.train,
		q.candidate_id, q.commit_oid, q.enqueued_at, q.ended_at, q.ended_position, q.end_reason, q.working_for`
	entrySQL = `SELECT ` + entryColumns + ` FROM code_queue INDEXED BY ` + queueKey +
		` WHERE repo_id = ? AND entry_id = ?`
	liveEntriesSQL = `SELECT ` + qualifiedEntryColumns + ` FROM json_each(?) AS j
		CROSS JOIN code_queue AS q INDEXED BY code_queue_change_idx
		WHERE q.change_id = CAST(j.value AS TEXT) AND q.state IN ('waiting', 'testing')`
	waitingSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND target_ref = ? AND state IN ('waiting', 'testing') AND state = 'waiting' LIMIT ?)`
	placeSQL = `SELECT COUNT(*) FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND target_ref = ? AND state IN ('waiting', 'testing') AND seq < ?`
	liveSeqsSQL = `SELECT seq FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND target_ref = ? AND state IN ('waiting', 'testing') AND seq <= ? ORDER BY seq`
)
