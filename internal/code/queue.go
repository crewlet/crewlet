package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A PROTECTED BRANCH'S MERGE QUEUE: a branch whose merge gate requires a check
// is a QUEUE TARGET ([GateRules.QueueTarget]) — its merges land through its
// queue, each change tested merged on the branch before it lands — and a
// change joins it by an ENQUEUE ([Enqueue]): a record on the
// REPOSITORY's own subject, the one its pushes and its changes' lifecycles are
// on, decided by its committer alone ([Committer.ProposeQueue]) as a change's
// write is. So a change's entry, its merge and its close are decided by one
// writer and arbitrated together: a change is never both queued and closed.
//
// # The enqueue is the gate
//
// An enqueue is decided by the change's merge gate ([JudgeGate]) from ONE
// snapshot, its principal the merger (M5-D8): every precondition met but the
// change's own merge into its target, which the queue's candidate proves
// against the branch it is built on, as a merge's build does. A refusal names
// every precondition not met and whom it waits on (gate_unmet). Beside the
// gate: a live, ready change into a queue target, at the head and the target
// the asker formed it against (stale, head_moved), not queued already
// (already_queued), behind fewer than [MaxQueueEntries] waiting entries of its
// target (queue_full). Its landing proves the gate again from its own
// snapshot; between the two, the verdict race is the deliberate one.
//
// # An entry leaves its queue with its change
//
// A DEQUEUE ([Dequeue]) ends an entry its enqueuer, the change's owner, a
// principal that maintains the repository or a person asks to end. And every
// record that leaves a change no longer what was enqueued ends its live entry
// in its own apply — a close, a draft, a retarget and a landing of the change
// dequeue it, and a push that moves its head EJECTS it, head_moved: the queue
// would otherwise test a head nobody enqueued. No record waits for another:
// the change's row and its entry are the same subject's rows.
//
// # Base format, as a stated condition
//
// The ops, the payloads and every field here are in the base format: no build
// that carries the code domain has run without them — none of the domain is on
// main, so no image and no release holds it — and while that is so, every
// build that meets an enqueue or a dequeue reads it. Once a build without them
// has run, each op takes {Name: "MutationRecord.Op <op>", Since: 2, Path:
// []string{"op"}, Equals: "<op>"} in versionedFields — enqueue, dequeue — with
// RecordVersion at 2, its line under "What each version added", a Carrying
// record in the statelogtest candidate and the ops excepted from the stamp
// test's base-format loop: an older build would otherwise FAIL on such a
// record at DecodeMutation's default arm rather than retain it. Neither is a
// gate.

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
// (code_queue.end_reason): a principal's dequeue, the end of its change as
// enqueued — closed, made a draft, retargeted, merged otherwise — or an
// ejection. A named string, so a reason a later build records is a value.
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

// MaxQueueEntries is the most WAITING entries one target's queue holds:
// 1 024. JUDGMENT: a queue at depth 8 and batch 1 with ten-minute
// checks lands about 1 152 changes a day, so a queue holding more than a
// day of its fastest capacity waiting is broken, not busy — one read of a
// keyset page, and its every entry's place counted in one seek of
// code_queue_live_idx.
const MaxQueueEntries = 1024

// MergeRateWindow is the span a queue's merge rate is measured over:
// an HOUR. JUDGMENT (M5-D28): about six rounds at ten-minute checks — enough
// rounds that one slow round moves the mean little, and short enough that a
// burst of enqueues is seen while it lasts. The repository's home counts a
// target's enqueues within it ([QueueDueQuery.EnqueuedSince]) and prices its
// queue's capacity over it (config.LandingCapacity, at the round its own
// trains took), which is what raises code_merge_rate_over_capacity.
const MergeRateWindow = time.Hour

// MergeRateScan is the most enqueues of one target a `queue_due` read counts
// within [MergeRateWindow]: 4 096. A queue at both bounds — depth 8, batch 8
// — lands 3 840 changes an hour at one-minute rounds, so for every round of a
// minute or longer a count stopped at the bound is still above the capacity
// it is compared with, and says what the whole count would; only a queue
// whose rounds take seconds has a capacity past it, and there the comparison
// errs quiet. It bounds what the read counts, not what it walks: the live
// seek walks the target's live entries, at most [MaxQueueEntries] waiting
// and the trains testing, and the ended seek the repository's entries ended
// within the window, which its queues' landings and dequeues bound.
const MergeRateScan = 4096

// QueueEntryID is the entry an enqueue under opID makes — derived from the
// operation id, so an enqueue asked again under it is the same entry, which
// the ledger collapses.
func QueueEntryID(opID string) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", "code.queue", opID)
}

// Enqueue puts a change in its target's queue, at the head and on the target
// its asker was shown ([QueueProposal]): the record of op enqueue on the
// repository's subject.
//
// BASE FORMAT, under the condition this file's head states; once a build
// without it has run it takes {Name: "MutationRecord.Op enqueue", Since: 2,
// Path: []string{"op"}, Equals: "enqueue"}.
type Enqueue struct {
	RepoID string `json:"repo_id"`
	// EntryID is [QueueEntryID] of the proposal's operation id.
	EntryID  string `json:"entry_id"`
	ChangeID string `json:"change_id"`
	// TargetRef and HeadOID are the change's target and head as its asker
	// was shown them, which the decide holds the change to.
	TargetRef string `json:"target_ref"`
	HeadOID   string `json:"head_oid"`
	// Method is how it lands; DeleteSource whether its landing deletes its
	// source branch, and CloseTask whether it closes its work item — each as
	// its asker resolved it.
	Method       MergeMethod `json:"method"`
	DeleteSource bool        `json:"delete_source,omitempty"`
	CloseTask    bool        `json:"close_task,omitempty"`
	// Principal is who enqueued it — the proposal's asker, STAMPED by the
	// committer: the merger the gate judged, and whom the queue lands it as.
	Principal Principal `json:"principal"`
}

// Dequeue takes a live entry out of its queue at a principal's ask: the record
// of op dequeue on the repository's subject.
//
// BASE FORMAT, under the condition this file's head states; once a build
// without it has run it takes {Name: "MutationRecord.Op dequeue", Since: 2,
// Path: []string{"op"}, Equals: "dequeue"}.
type Dequeue struct {
	RepoID  string `json:"repo_id"`
	EntryID string `json:"entry_id"`
	// Principal is who dequeued it — the proposal's asker, STAMPED by the
	// committer.
	Principal Principal `json:"principal"`
	// Routing is whom the record concerns ([ChangeRouting]), STAMPED BY THE
	// COMMITTER: the change's authors and its enqueuer, told it left the
	// queue — all but who asked. Under the condition concerns.go states, it
	// takes {Name: "Dequeue.Routing", Since: 2, Op: "dequeue", Path:
	// []string{"mutation", "routing"}} once a build without it has run.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// QueueProposal is one write of a target's queue as its asker hands it to the
// repository's committer ([Committer.ProposeQueue]): EXACTLY ONE of an
// enqueue, a dequeue and one of the queue's own records — a candidate, an
// eject, a land, a green and a broken (queuerecords.go) — decided alone as a
// record of its own, beside who asks and the floor they read at. Its OpID is
// the record's operation id — an enqueue's entry id, a candidate's train and a
// landing's id are derived from it — and its Seen the publish's session floor.
type QueueProposal struct {
	OpID string `json:"op_id"`
	// Asker is who asks an enqueue or a dequeue: a person, or a seat — whose
	// merge authority, like every approval an enqueue's gate counts, is
	// judged from the chart of the deciding node's epoch
	// ([QueueProposal.JudgesChart]). A token bound to nobody merges nothing.
	// The queue's own records name NOBODY: they are the engine's — its home
	// builds them — and a landing lands each entry as its enqueuer, read from
	// the entry's row, never as whoever asked.
	Asker Principal `json:"asker"`
	// Seen is the code log's position the asker read the change at — zero
	// for one that read none. The committer DECIDES AT OR PAST IT, or refuses
	// `behind` about itself, so the runner-up decides.
	Seen statelog.Position `json:"seen,omitzero"`
	// DropEpoch is, for a candidate or a landing, the repository's drop
	// epoch as the copy its home built it from held it (internal/code/cache's
	// Synced.DropEpoch), as a push's is ([Proposal.DropEpoch]): the committer
	// refuses it `expunging` where the repository's epoch is another
	// ([MergeRefusalExpunging]) — an expunge's begin since has left dropping
	// objects that copy held live, and a train's commits built over them,
	// which its landing lands, would move the target onto an object the
	// expunge's commit retires. Zero, the epoch of a repository never
	// expunged, on every other record: an enqueue, a dequeue, an eject, a
	// green and a broken move no ref onto anything a build made. NEVER
	// RECORDED, as Seen is not, so no record's format moves for it.
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// Ceiling is a seat's DEQUEUE's requester ceiling (M3-decisions U40 (a)):
	// whom the turn that asks it works for, judged by the committer beside
	// the seat's own standing, from the same chart and snapshot — the seat's
	// own work spared (an entry it enqueued, its own change's, or one into a
	// branch of its own namespace or of a repository it owns), a colleague
	// held to what it may take out itself, nobody refused the rest
	// ([decideDequeue]). An enqueue carries none: the git node that forms it
	// has judged its merge's ceiling already (internal/code/host's merge.go).
	//
	// A proposal is no record — the record is what the committer stamps from
	// it — so this field takes no versionedFields row; it crosses the
	// estate's wire to whichever data node decides it under the code domain's
	// stated condition — no build carrying the domain has run without it,
	// none of the domain being on main — so every node asked it reads it.
	Ceiling   *WriteCeiling `json:"ceiling,omitempty"`
	Enqueue   *Enqueue      `json:"enqueue,omitempty"`
	Dequeue   *Dequeue      `json:"dequeue,omitempty"`
	Candidate *Candidate    `json:"candidate,omitempty"`
	Eject     *Eject        `json:"eject,omitempty"`
	Land      *Land         `json:"land,omitempty"`
	Green     *Green        `json:"green,omitempty"`
	Broken    *Broken       `json:"broken,omitempty"`
}

// engines reports a proposal of one of the queue's OWN records, which its home
// asks and which names no asker.
func (p QueueProposal) engines() bool {
	return p.Candidate != nil || p.Eject != nil || p.Land != nil || p.Green != nil || p.Broken != nil
}

// writes is how many of the records a proposal may be p carries: one, or it
// is no proposal.
func (p QueueProposal) writes() int {
	n := 0
	for _, set := range []bool{p.Enqueue != nil, p.Dequeue != nil, p.Candidate != nil, p.Eject != nil,
		p.Land != nil, p.Green != nil, p.Broken != nil} {
		if set {
			n++
		}
	}
	return n
}

// QueueResult is what became of one queue proposal.
type QueueResult struct {
	// Outcome is applied, pending or unknown for a proposal whose record was
	// appended, and empty for one refused. Applied is answered from the rows
	// the record wrote: an enqueue's or a dequeue's entry, a candidate's,
	// an eject's and a land's entries, a green's green row and a broken's
	// landing.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// EntryID is the entry an enqueue or a dequeue is about.
	EntryID string `json:"entry_id,omitempty"`
	// State is the entry's state once the record applied; Place its place
	// in its target's queue while it is live, from 1, and 0 otherwise — or
	// where a later record moved the entry since.
	State QueueState `json:"state,omitempty"`
	Place int        `json:"place,omitempty"`
	// Position is where the record landed — for one asked again under its
	// own operation id, the record the ledger found — and, for a not_applied
	// or repo_deleted refusal the log made after the append, where that
	// record lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
	// Refusal is why it was refused with no record, Detail its one sentence,
	// and Unmet every precondition of the change's merge gate an enqueue
	// refused gate_unmet did not meet, each naming whom it waits on.
	Refusal MergeRefusal   `json:"refusal,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Unmet   []Precondition `json:"unmet,omitempty"`
	// Refused is every entry of a landing its decide would not land, each
	// with why — not_queued, head_moved, gate_unmet with every precondition
	// unmet — beside the landing's own refusal naming the first: what its
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

// MergeRefusal is why a repository's committer decided nothing of a write
// that moves a change towards its merge — the merge itself ([MergeProposal])
// or an enqueue or a dequeue of its target's queue ([QueueProposal]) — a code
// its asker reads as "<code>: <sentence>". A named string, so a code this
// build does not know is a value rather than a panic. NONE IS EVER RECORDED: a
// refused write appends nothing.
type MergeRefusal string

// The codes, in the order the decides' rules reach them — the ones every
// write's decide shares, then a merge's, an enqueue's and a dequeue's own —
// then the committer's own.
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
	// landing on a PULL MIRROR ([RepoRef.Mirrored]): it moves only by its
	// syncs from its source, so nothing merges into it ([MirroredDetail]). A
	// dequeue and an eject still land — neither moves a ref — so a
	// repository that became a mirror with entries queued sheds them, as a
	// close sheds its live changes.
	MergeRefusalRepoMirrored MergeRefusal = "repo_mirrored"
	// MergeRefusalExpunging is a merge, a candidate or a landing built
	// against the repository at another drop epoch than the repository's
	// ([MergeProposal.DropEpoch], [QueueProposal.DropEpoch]): an expunge
	// began on it since ([ExpungeBegin]), leaving dropping what the copy its
	// build read held live, so the commit it moves a ref onto may reach an
	// object the expunge's commit retires. Nothing was decided, and its
	// builder builds it again against the repository as the begin left it
	// ([ExpungingBuiltDetail]) — as a push checked before a begin is refused
	// ([RefusalExpunging]).
	MergeRefusalExpunging MergeRefusal = "expunging"
	// MergeRefusalChangeNotFound is a change the repository does not hold.
	MergeRefusalChangeNotFound MergeRefusal = "change_not_found"
	// MergeRefusalChangeNotOpen is a change closed or merged.
	MergeRefusalChangeNotOpen MergeRefusal = "change_not_open"
	// MergeRefusalChangeDraft is a draft, which merges nowhere.
	MergeRefusalChangeDraft MergeRefusal = "change_draft"
	// MergeRefusalStale is a write formed against a target the change no
	// longer has: the asker reads the change again.
	MergeRefusalStale MergeRefusal = "stale"
	// MergeRefusalHeadMoved is a write formed against a head the change has
	// moved from.
	MergeRefusalHeadMoved MergeRefusal = "head_moved"
	// MergeRefusalTargetMoved is a merge built onto a tip its target has
	// moved from since: what it built merges a branch that is no longer
	// there, and its builder builds it again onto the tip the target holds.
	MergeRefusalTargetMoved MergeRefusal = "target_moved"
	// MergeRefusalTargetMoving is a merge its BUILDER answered once its
	// target moved under the first build and under every one of
	// [MergeRecomputes] rebuilds after it: the branch moves faster than a
	// merge is built onto it, and its queue, or a quieter moment, lands it.
	// Never a decide's: the git node that built the merge answers it.
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
	// MergeRefusalSourceRefused is a merge asked to delete its source
	// branch where a push of that deletion would be refused — a protection
	// that keeps the branch, or, for a source outside the seats' namespace,
	// the merger's grant: the sentence names the push's refusal, and the
	// merge lands keeping its source.
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
	// whoever its turn works for may not make (its git node judges the
	// write's ceiling, internal/code/host's merge.go).
	MergeRefusalNotPermitted MergeRefusal = "not_permitted"
	// MergeRefusalNotGreen is a landing whose tip, or a green row
	// whose landing, some required check of its target has not passed on —
	// by its latest attempt checked for the target: nothing moves on red,
	// nor on what is still running.
	MergeRefusalNotGreen MergeRefusal = "not_green"
	// MergeRefusalNotRed is a broken landing none of whose named checks
	// failed on its commit by its latest attempt.
	MergeRefusalNotRed MergeRefusal = "not_red"
	// MergeRefusalNoLanding is a green or a broken naming no landing above
	// its target's green row: one green already, or none at all.
	MergeRefusalNoLanding MergeRefusal = "no_landing"
	// MergeRefusalRecordFull is a landing whose rows — its entries' own and
	// the live changes their pushes move — would take its record past
	// MaxRecordRows: its home lands fewer entries at once.
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
// naming the field), before it is queued: anything but exactly one of the
// records a queue proposal may be; an operation id this engine does not
// accept; an enqueue's or a dequeue's asker that is no principal, or names a
// run, and an asker on one of the queue's own records, which name nobody; a
// Seen that is no position on the code log; a repository id that is no UUID
// in its canonical spelling; a drop epoch below zero, or set on a record
// other than a candidate or a landing; an entry id that is no bare UUIDv7 — and an
// enqueue's that is not [QueueEntryID] of its operation id; a change id that
// is no bare UUIDv7; a target that is no branch a change may name; a head that
// is no whole object id; a method this build does not know; a principal the
// asker set, which the committer stamps; each of the queue's own records'
// rules (queuerecords.go); and a record past what one proposal may carry
// ([EntryRoom]).
func (p QueueProposal) Validate() error {
	_, err := p.measure()
	return err
}

// measure validates p and answers its queued byte charge: its record's
// payload as the committer stamps it — every entry of a landing deleting its
// source, the widest its decide makes it — encoded, held under [EntryRoom].
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
	// AND THE ROOM A ROUTING KEEPS on a record of one change, which its
	// committer stamps; a record of several keeps what fits of what is left
	// ([keptRoutings]).
	bytes := len(encoded)
	if p.Dequeue != nil || p.Broken != nil {
		bytes += MaxRoutingBytes
	}
	if bytes > EntryRoom {
		return 0, fmt.Errorf("%w: %s: %d bytes, past the %d one proposal may take of its record",
			ErrInvalidProposal, op, bytes, EntryRoom)
	}
	return bytes, nil
}

// validFields is every rule of [QueueProposal.Validate] but the record's
// bytes.
func (p QueueProposal) validFields() error {
	if p.writes() != 1 {
		return fmt.Errorf("%w: a queue proposal is exactly one of an enqueue, a dequeue, a candidate, an "+
			"eject, a land, a green and a broken", ErrInvalidProposal)
	}
	if err := statelog.CheckCallerOpID(p.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	if p.engines() {
		// THE QUEUE'S OWN RECORDS NAME NOBODY: its home asks them, and a
		// landing lands each entry as its enqueuer.
		if p.Asker != (Principal{}) {
			return badField("asker", "the queue's own records are the engine's and name no asker: a landing "+
				"lands each entry as its enqueuer")
		}
	} else {
		switch why := p.Asker.invalid(); {
		case why != "":
			return fmt.Errorf("%w: asker: %s", ErrInvalidProposal, why)
		case !p.Asker.Run.IsZero():
			return badField("asker.run", "a queue's write names its asker alone, never a run")
		}
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
	}
	if p.Ceiling != nil {
		switch {
		case p.Dequeue == nil:
			return badField("ceiling", "only a seat's dequeue carries its requester's ceiling: an enqueue's "+
				"is judged by the git node that forms it")
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
		return badField("enqueue.method", "%q is not one of %v", e.Method, MergeMethods())
	case e.Principal != (Principal{}):
		return badField("enqueue.principal", "the committer stamps who enqueued it from the asker, and an asker "+
			"sends none")
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
	}
	return "", nil
}

// stamped is the op and the payload of p's record as the committer forms it
// before its decide: an enqueue's and a dequeue's asker as its principal. A
// landing's stamps are its decide's, from its snapshot.
func (p QueueProposal) stamped() (OpKind, any) {
	op, payload := p.record()
	switch r := payload.(type) {
	case Enqueue:
		r.Principal = p.Asker
		return op, r
	case Dequeue:
		r.Principal = p.Asker
		return op, r
	}
	return op, payload
}

// RepoID is the repository p is about — the ONE answer the router keys p by,
// the committer queues it under and the estate's seam raises the
// repository's floor for.
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
// which a node holding none, or one confirmed behind the company's
// configuration, never decides: EVERY ENQUEUE AND EVERY LANDING, whoever asks
// — the merge gate each judges counts an approval by who may approve the
// change, an owner's by the lead chain above it and a seat's merge authority
// by its grant, each the chart's ([JudgeGate]), as a mergeability's readiness
// is ([ConversationWrite.JudgesChart]) — and a SEAT'S dequeue, whose grant
// says whether it may take an entry out. A person's dequeue consults none, nor
// do the queue's other records, which read rows alone. ONE ANSWER, which the
// committer queues and decides by and the estate declares the operation's
// grant by, so the two never disagree about which writes a node behind the
// company's configuration may decide.
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
	}
	return queueDecidedRows
}

// ProposeQueue decides one write of a target's queue as a batch of its own in
// the repository's queue — the push's queue and bounds ([Committer.Propose]),
// as a change's write's are ([Committer.ProposeChange]): a full queue or a node
// holding its bytes is busy at once, a write whose caller's deadline leaves too
// little for a batch and its answer is busy undecided, a stopping committer
// answers [ErrNotServing] — as does one that cannot judge a write the chart
// judges ([QueueProposal.JudgesChart]), behind the company's configuration or
// holding no chart yet — and a proposal
// that is not one answers [ErrInvalidProposal]. ctx bounds only this caller's
// wait.
//
// ITS OPERATION ID IS THE PROPOSAL'S OWN, so a write its asker hands the
// repository's runner-up after this node went silent is the same write
// there, collapsed by the ledger onto the record that landed — an enqueue onto
// the entry it made. ITS SESSION IS ITS SEEN. Applied is answered FROM THE
// ENTRY'S ROW the record wrote, never from the decision.
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
	case errors.Is(err, ErrNotServing):
		outcome = "handed_on"
	case err != nil:
		outcome = "failed"
	case res.Refusal != "":
		outcome = string(res.Refusal)
	}
	c.metrics.Add(metrics.CodeQueueProposals, 1, metrics.Attrs{"op": string(op), "outcome": outcome})
}

// queueDecision is one round's decision of a queue proposal: its refusal, its
// sentence and the preconditions a gate_unmet names where it is refused; the
// record's payload, stamped, where it forms one; and the version the
// repository's row was read at either way.
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

// decideQueue is one round's decision of p over the rows tx holds — see this
// file's head. Pure over (the rows, p, seats, limits, retention, capacity):
// seats is the deciding epoch's chart, which a seat's write is judged by;
// limits its code.limits, whose merge_by floor the gate holds a merger to;
// retention its code.deleted_retention_days, which a repo_deleted refusal
// names; capacity the waiting entries a target's queue holds
// (MaxQueueEntries); now the decide's clock, which a pack's upload is judged
// by.
func decideQueue(ctx context.Context, tx *sql.Tx, p QueueProposal, seats SeatRules, limits Limits,
	retention time.Duration, capacity int, now time.Time) (queueDecision, error) {

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
		// AN ENQUEUE, A CANDIDATE AND A LANDING ARE A MERGE'S STEPS, which a
		// pull mirror never takes: it moves only by its syncs. A DEQUEUE and
		// an EJECT move no ref, so they still land, and a repository that
		// became a mirror sheds its queue; a GREEN and a BROKEN record what a
		// landing's tip checks said of a landing made before it was one.
		return d.refuse(MergeRefusalRepoMirrored, MirroredDetail(repo.name, repo.managedBy))
	case (p.Candidate != nil || p.Land != nil) && p.DropEpoch != repo.dropEpoch:
		// THE DROP EPOCH, before every rule of the record's own, as a
		// push's is: a train built against a copy at another epoch, or a
		// landing of one read at another, may move the target onto an object
		// an expunge begun since left dropping, and its home builds it again.
		what, builder := "train", "its home"
		if p.Land != nil {
			what = "landing"
		}
		return d.refuse(MergeRefusalExpunging, ExpungingBuiltDetail(repo.name, what, builder, p.DropEpoch,
			repo.dropEpoch))
	}
	if p.JudgesChart() && seats.Roster == nil {
		// THE CALLER'S BUG: the committer hands a write the chart judges on
		// to the repository's next committer before deciding it with no
		// chart.
		return queueDecision{}, fmt.Errorf("code: a queue write the chart judges on %s reached the decide with "+
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
		return decideGreen(ctx, tx, *p.Green, repo, d)
	case p.Broken != nil:
		return decideBroken(ctx, tx, *p.Broken, repo, seats, d)
	}
	return decideEnqueue(ctx, tx, p, repo, seats, limits, capacity, d)
}

// decideEnqueue is an enqueue's rules over the rows tx holds — see this file's
// head, in their order.
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
	// THE GATE, every precondition but the change's merge into its target,
	// which the queue's candidate proves on the branch it is built on.
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
	// record goes on to its routing below: a person's dequeue tells the
	// change's authors and its enqueuer as a seat's does.
	if kind != PartyPerson {
		why, err := whyNoDequeue(ctx, tx, p, entry, repo, seats)
		if err != nil {
			return queueDecision{}, err
		}
		if why != "" {
			return d.refuse(MergeRefusalNotPermitted, why)
		}
	}
	// WHOM IT CONCERNS: the change's authors and its enqueuer, told it left
	// the queue — all but who asked.
	_, payload := p.stamped()
	row, found, err := changeRowIn(ctx, tx, entry.ChangeID)
	if err != nil {
		return queueDecision{}, err
	}
	if found {
		out := payload.(Dequeue)
		if out.Routing, err = routingFor(ctx, tx, concerning{repoName: repo.name, change: row, actor: handle,
			actorKind: kind, authors: true, dequeued: true, enqueuer: entry.EnqueuedBy.reviewer()}); err != nil {
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
	// mine is whether party, a principal of standing grant, may take the
	// entry out: its enqueuer, its change's owner, or one that maintains the
	// repository.
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
	// THE SEAT'S DEQUEUE, HELD TO WHOEVER ITS TURN WORKS FOR (Ceiling): its
	// own work — an entry it enqueued, its own change's, or one into a branch
	// of its own namespace or of a repository it owns — steered by anybody,
	// as every write's is; a colleague held to what it may take out itself;
	// nobody steering nothing else.
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
	// AN EJECTION IS ANNOUNCED once its own record applied here, never for a
	// retry the ledger collapsed onto it — once its answer is handed back, so
	// its asker waits on none of it.
	if p.Eject != nil && answerErr == nil && out.Outcome == statelog.OutcomeApplied && !res.Collapsed {
		c.announceEjected(*p.Eject, out.Position)
	}
	// A BROKEN LANDING IS ANNOUNCED the same way, once its own record applied
	// here: the one event a red tip's bisect leaves (ADR-0068).
	if p.Broken != nil && answerErr == nil && out.Outcome == statelog.OutcomeApplied && !res.Collapsed {
		c.announceBroken(*p.Broken, out.Position)
	}
}

// scope is the scope p's record names: WHAT ITS DECIDE READS (M5-D33), stated
// before it runs, so a node holding a deferred record of any of it decides
// none. An enqueue's gate reads the change's conversation and facts and the
// repository's checks beside the repository; a LANDING proves each entry's
// gate, so it names each entry's change beside them — and, past
// [MaxScopePaths], the whole domain, which covers them all; a green and a
// broken read the repository's checks; a dequeue, a candidate and an eject
// read the repository's rows alone.
func (p QueueProposal) scope() []string {
	repoID := p.RepoID()
	switch {
	case p.Enqueue != nil:
		return []string{RepoPath(repoID), ChecksPath(repoID), ChangePath(p.Enqueue.ChangeID)}
	case p.Land != nil:
		scope := []string{RepoPath(repoID), ChecksPath(repoID)}
		if len(scope)+len(p.Land.Entries) > MaxScopePaths {
			return []string{DomainPath()}
		}
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
		return QueueResult{}, fmt.Errorf("code: the publish of operation %s answered applied with no record, "+
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
		failed := fmt.Errorf("code: the record of operation %s applied at %s and its entry's row could not be "+
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
		c.log.ErrorContext(ctx, "code_queue_invalid", "repo", p.RepoID(), "op_id", p.OpID, "error", err)
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
	return QueueResult{}, fmt.Errorf("code: queue operation %s: %w", p.OpID, err)
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
}

// scanEntry reads one row [entryColumns] spells.
func scanEntry(scan func(...any) error) (queueEntry, error) {
	var (
		e                           queueEntry
		method, kind, state, reason string
	)
	err := scan(&e.RepoID, &e.EntryID, &e.TargetRef, &e.ChangeID, &e.Seq, &e.HeadOID, &method, &e.DeleteSource,
		&e.CloseTask, &e.EnqueuedBy.Handle, &kind, &e.EnqueuedOperator, &state, &e.Train,
		&e.CandidateID, &e.CommitOID, &e.EnqueuedAt, &e.EndedAt, &e.EndedPosition, &reason)
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
		return queueEntry{}, false, fmt.Errorf("code: read entry %s of %s's queue: %w", entryID, repoID, err)
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
		return nil, fmt.Errorf("code: read %d changes' queue entries: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		e, err := scanEntry(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("code: read %d changes' queue entries: %w", len(ids), err)
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
		return 0, fmt.Errorf("code: count %s's waiting entries on %s: %w", target, repoID, err)
	}
	return n, nil
}

// placeIn is a live entry's place in its target's queue, from 1: the live
// entries before it, counted over code_queue_live_idx's range up to it — at
// most MaxQueueEntries waiting, and the candidates' entries before them.
func placeIn(ctx context.Context, tx *sql.Tx, e queueEntry) (int, error) {
	var before int
	if err := tx.QueryRowContext(ctx, placeSQL, e.RepoID, e.TargetRef, e.Seq).Scan(&before); err != nil {
		return 0, fmt.Errorf("code: read entry %s's place in %s's queue: %w", e.EntryID, e.TargetRef, err)
	}
	return before + 1, nil
}

// changeOwnerIn is changeID's owner, by handle and kind.
func changeOwnerIn(ctx context.Context, tx *sql.Tx, changeID string) (string, PartyKind, error) {
	row, found, err := changeRowIn(ctx, tx, changeID)
	if err != nil || !found {
		return "", "", err
	}
	return row.owner, row.ownerKind, nil
}

// The queue's reads, each registered in plan_test.go: one entry by its key; the
// live entries of a list of changes, driven from the list, one seek of
// code_queue_change_idx a change; a target's waiting entries counted, and the
// live ones before an entry counted, each over code_queue_live_idx with its
// partial predicate repeated verbatim — the pinned planner takes a partial
// index only for a statement that implies its predicate in its own words.
const (
	// entryColumns is every column of an entry [scanEntry] reads, in its
	// order.
	entryColumns = `repo_id, entry_id, target_ref, change_id, seq, head_oid, method, delete_source, close_task,
		enqueued_by, enqueued_kind, enqueued_operator, state, train, candidate_id, commit_oid, enqueued_at,
		ended_at, ended_position, end_reason`
	// qualifiedEntryColumns is entryColumns read beside json_each, which has
	// columns of its own: the same columns, in the same order.
	qualifiedEntryColumns = `q.repo_id, q.entry_id, q.target_ref, q.change_id, q.seq, q.head_oid, q.method,
		q.delete_source, q.close_task, q.enqueued_by, q.enqueued_kind, q.enqueued_operator, q.state, q.train,
		q.candidate_id, q.commit_oid, q.enqueued_at, q.ended_at, q.ended_position, q.end_reason`
	entrySQL       = `SELECT ` + entryColumns + ` FROM code_queue WHERE repo_id = ? AND entry_id = ?`
	liveEntriesSQL = `SELECT ` + qualifiedEntryColumns + ` FROM json_each(?) AS j
		CROSS JOIN code_queue AS q INDEXED BY code_queue_change_idx
		WHERE q.change_id = CAST(j.value AS TEXT) AND q.state IN ('waiting', 'testing')`
	waitingSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND target_ref = ? AND state IN ('waiting', 'testing') AND state = 'waiting' LIMIT ?)`
	placeSQL = `SELECT COUNT(*) FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ? AND target_ref = ? AND state IN ('waiting', 'testing') AND seq < ?`
)
