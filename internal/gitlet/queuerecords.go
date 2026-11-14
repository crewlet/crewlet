package gitlet

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A QUEUE'S OWN RECORDS: what a target's merge queue does with the entries an
// enqueue made (queue.go), each a record on the REPOSITORY's own subject that
// its committer decides alone ([Committer.ProposeQueue]) from one snapshot,
// as an enqueue is — the queue's work is the repository's home's (M4's home
// Gitlet node builds what they record), its decisions the committer's:
//
//   - a CANDIDATE ([Candidate]) records a queue's TRAIN: the
//     candidates its home built of the target's live entries onto the
//     target's tip — each a batch of entries merged on the candidate before
//     it, its tip what the queue's checks then run on — and the one pack the
//     commits they built are stored in, so a candidate is ROWS
//     (code_candidates) and a pack row, never a ref (M5-D12). A target holds
//     one train at most: a candidate ABANDONS the target's earlier one, its
//     candidates removed and its entries waiting again.
//   - an EJECT ([Eject]) puts live entries out of their queue — a required
//     check failed on what they were tested in, their merge conflicted, their
//     gate no longer holds, their head moved, a fast-forward that is not one,
//     their repository a pull mirror — naming the checks or the paths, and
//     abandons the train each was in.
//   - a LAND ([Land]) lands entries on their target in ONE record: a train's
//     tested PREFIX — its candidates from the first, up to one whose tip
//     every required check passed on. ITS DECIDE PROVES EVERY ENTRY'S GATE
//     AGAIN, from its own snapshot, with the entry's ENQUEUER as the merger
//     (M5-D8): an enqueue is the gate, and a landing is the record that
//     moves the branch, which the ADR binds. Its apply merges each entry's
//     change first and lands ONE PUSH ROW AN ENTRY, `via queue`, naming its
//     enqueuer — the queue never acts on its own account — so which push
//     landed a commit names each change's own landing.
//   - a GREEN ([Green]) moves a protected target's green row onto a landing
//     record whose required checks passed on its tip — a landing no queue
//     tested first, a person's override merge or push; a BROKEN ([Broken])
//     marks a landing above the green row whose required check failed. Green
//     only ever advances on green: no record moves it on red, and none moves
//     it back.
//   - a QUEUE_SWEEP ([QueueSweep], [Store.QueueSweep]) removes an IDLE
//     repository's ended entries and its landings below their target's green
//     row past [QueueHistory], which its own candidates', ejections' and
//     landings' horizons remove while its queues work (M5-D34).
//   - a CLOSE_SETTLED ([CloseSettled]) settles the closes landed entries owe
//     their changes' work items (code_queue.close_op, which the land sets
//     where the enqueue asked it): its home closes each item AS ITS ENQUEUER
//     on whichever turn first reads the owed close, and records how the
//     turn's closes ended, in one record — a landing answered pending, a
//     home that stopped between the landing and the close, a tracker that
//     could not answer: each leaves the close owed for the next turn on any
//     home, never lost.
//
// # A train and a landing are built at a drop epoch
//
// A candidate's commits are built by the home over its copy of the
// repository, and a landing moves the target onto commits its train built
// so: each proposal carries the drop epoch that copy held
// ([QueueProposal.DropEpoch]), and its decide refuses it `expunging` where an
// expunge has begun since ([MergeRefusalExpunging]), before every rule of its
// own, as a push's and a merge's are — a build before a begin may
// rely on objects the begin left dropping. And what the queue holds in flight
// an expunge keeps, as it keeps a ref's tip: every live entry's head, every
// candidate's tip and every target's green row are among the objects its
// scan walks from (the `expunge_tips` read), so a purge never drops the
// commit a landing moves its target onto.
//
// # Base format
//
// Every op, payload and field here is in the base format, as queue.go's head
// says: the domain landed on main with them, with a land's apply setting
// code_queue.close_op, and with the values they add to closed sets
// (`merged_via` queue on a change's row, `via` queue on a push's and `kind`
// queue on a landing's). None is a gate.

// The queue's bounds.
const (
	// MaxCandidateEntries is the most entries one train holds: a target's
	// deepest queue's candidates, each its largest batch — MaxQueueDepth ×
	// MaxQueueBatch, 64 — so one candidate record carries every candidate a
	// queue tests at once.
	MaxCandidateEntries = MaxQueueDepth * MaxQueueBatch

	// MaxLandEntries is the most entries one land record lands: 64,
	// [MaxPushesPerRecord], since it lands one push row an entry — the
	// committer's group commit's shape — which a train's whole tested
	// prefix, at most [MaxCandidateEntries] (also 64), fits in. MEASURED,
	// a record of all 64, each deleting its source, holds the write lane
	// 33 ms, and one of a single entry 5.7 ms (BenchmarkApplyALandRecord,
	// doc.go's measured section): under the third of
	// statelog.ApplyTxTimeBudget the domain's records are sized to, so a
	// train's whole prefix lands as one.
	MaxLandEntries = MaxPushesPerRecord

	// MaxEjectedPaths is the most paths one ejection names of a conflicted
	// merge: [MaxConflictPaths], the most a change's mergeability names of
	// the same conflict, within [MaxConflictBytes] — the one list a person
	// reads first.
	MaxEjectedPaths = MaxConflictPaths

	// QueueHistory is how long a queue's ended entries and its target's
	// landings below its green row are kept: thirty days, [CheckHistory] —
	// what a change's page and a target's history cite are the checks they
	// were judged by, which are kept that long.
	QueueHistory = CheckHistory

	// QueueSweepGrace is how long past [QueueHistory] a repository's ended
	// entries or landings outlive it before the code-retention duty sweeps
	// them: a day, [CheckSweepGrace], for its reason — a repository whose
	// queues keep working removes its own history with every candidate,
	// ejection and landing, so its rows never outlive the history by the
	// grace and the duty never appends what one of its own records would.
	QueueSweepGrace = CheckSweepGrace

	// QueueSweepRows is the most rows one queue_sweep record removes: every
	// row a record's apply may touch (MaxRecordRows) but the repository's
	// own, which it moves the version of, as a retired-pack sweep's
	// ([RetireSweepRows]). It bounds a sweep of NARROW rows — a landed or
	// dequeued entry, a landing — and [QueueSweepBytes] the ejections among
	// them.
	QueueSweepRows = MaxRecordRows - 1

	// QueueSweepBytes is how many BYTES of ejections one queue_sweep record
	// removes beside QueueSweepRows' count: its ended entries are taken
	// oldest first, each weighed by what its ejection's JSON takes
	// (code_queue.ejected_bytes), until their bytes reach it — always at
	// least one, and at most one past it — and a step whose entries it cut
	// takes no landing, which waits for the next.
	//
	// A ROW COUNT BOUNDS A SWEEP'S TIME ONLY FOR NARROW ROWS, as it does a
	// purge part's ([PurgePartBytes]): an ejection holds its failed checks
	// and up to [MaxEjectedPaths] conflicted paths within [MaxConflictBytes],
	// some 16 KiB at its widest, and deleting it frees its overflow pages.
	// Bounded by QueueSweepRows alone, a step of the widest ejections
	// removed about 64 MiB; bounded here it removes 2 058 of them, and its
	// record holds the write lane 64 ms, against 44 ms for QueueSweepRows
	// narrow entries (BenchmarkApplyAQueueSweep, doc.go's measured section),
	// both under the third of statelog.ApplyTxTimeBudget the domain's
	// deletes are sized to. [PurgePartBytes], because it is the bytes of
	// wide rows a delete takes in that third, measured, and these are the
	// same deletes of overflow pages.
	QueueSweepBytes = PurgePartBytes

	// queueHorizonRows is the most rows one candidate, eject or land record
	// removes of its repository's queue history past [QueueHistory] — ended
	// entries first, then landings below their green row: twice
	// [MaxLandEntries], since a land ends at most that many entries and
	// writes at most that many landings, so a working queue's history keeps
	// pace with what its records add, and the sweep only reaches an idle
	// one.
	queueHorizonRows = 2 * MaxLandEntries

	// queueHorizonBytes is how many bytes of ejections one candidate, eject
	// or land record removes beside queueHorizonRows' count, weighed as
	// [QueueSweepBytes] weighs them: [HorizonBytesPerRecord], MaxRecordBytes,
	// for its reason — an eject's ejections are its record's own, so no
	// record adds more ejection bytes than that, and the history's removal
	// keeps pace with its growth in bytes as in rows, inside the hot records
	// at the cost of one record's bytes of deletes.
	queueHorizonBytes = HorizonBytesPerRecord
)

// The ids a queue's records name, each DERIVED from the record's operation id
// as an enqueue's entry is ([QueueEntryID]), so a record its home asks again
// under its operation is the same train, the same landing and the same pushes,
// which the ledger collapses.

// QueueTrainID is the train a candidate record under opID makes.
func QueueTrainID(opID string) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", "code.queue.train", opID)
}

// QueueCandidateID is the ordinal'th candidate of train, from 1.
func QueueCandidateID(train string, ordinal int) string {
	at, _ := statelog.OpMintedAt(train)
	return statelog.DeriveOpID(at, "", train, strconv.Itoa(ordinal))
}

// QueueLandID is the landing a land record under opID is: the id its pushes
// are derived from.
func QueueLandID(opID string) string {
	at, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(at, "", "code.queue.land", opID)
}

// LandPushID is the push a landing lands entryID as: one push row an entry, so
// which push landed a commit names each change's own landing.
func LandPushID(landID, entryID string) string {
	at, _ := statelog.OpMintedAt(landID)
	return statelog.DeriveOpID(at, "", landID, entryID)
}

// The ejections' reasons, each an end of a live entry ([QueueEndReason]) beside
// a dequeue's and [EjectHeadMoved], a push's.
const (
	// EjectCheckFailed is a required check that failed on what the entry was
	// tested in.
	EjectCheckFailed QueueEndReason = "check_failed"
	// EjectConflict is the entry's merge onto what was before it in its
	// train conflicting.
	EjectConflict QueueEndReason = "conflict"
	// EjectGateUnmet is the entry's gate no longer holding with its
	// enqueuer as the merger: a blocker raised, an approval withdrawn, a
	// merge authority lost since it was enqueued.
	EjectGateUnmet QueueEndReason = "gate_unmet"
	// EjectNotFastForward is a fast_forward entry whose head does not hold
	// what was before it.
	EjectNotFastForward QueueEndReason = "not_fast_forward"
	// EjectRepoMirrored is the entry's repository a PULL MIRROR
	// ([RepoRef.Mirrored]), which nothing merges into: it moves only by its
	// syncs, so a candidate and a landing on it are refused repo_mirrored,
	// and its home puts every live entry out rather than build, on every
	// turn, a train no landing could land — its enqueuer and its change's
	// authors told why. Its decide holds the reason to the rows: refused
	// stale on a repository that is not a pull mirror ([decideEject]).
	//
	// BASE FORMAT, as this file's head says.
	EjectRepoMirrored QueueEndReason = "repo_mirrored"
)

// EjectReasons is the closed set an eject record may name, in this order.
func EjectReasons() []QueueEndReason {
	return []QueueEndReason{EjectCheckFailed, EjectConflict, EjectGateUnmet, EjectHeadMoved, EjectNotFastForward,
		EjectRepoMirrored}
}

// MergedViaQueue is how a change a land record merged reached its target
// ([MergedVia]): its queue landed it, tested merged on its target first.
//
// BASE FORMAT, as this file's head says.
const MergedViaQueue MergedVia = "queue"

// Candidate records a queue's TRAIN on one target: its candidates,
// built by the repository's home onto the target's tip, and the pack the
// commits they built are stored in — the record of op candidate on the
// repository's subject.
//
// BASE FORMAT, as this file's head says.
type Candidate struct {
	RepoID    string `json:"repo_id"`
	TargetRef string `json:"target_ref"`
	// Train is [QueueTrainID] of the proposal's operation id.
	Train string `json:"train"`
	// BaseOID is the target's tip the train was built onto, which the decide
	// holds the target to.
	BaseOID string `json:"base_oid"`
	// Candidates are the train's candidates in order, each a batch of entries
	// merged onto the one before — the first onto BaseOID.
	Candidates []TrainCandidate `json:"candidates"`
	// Pack is the pack of every object the train's merges wrote, stored
	// before it was proposed; nil only for a train of fast-forwards, which
	// writes none.
	Pack *PackRef `json:"pack,omitempty"`
}

// TrainCandidate is one candidate of a train: its id ([QueueCandidateID]), its
// ordinal from 1, the entries it merges and the commit each became, and its
// tip — the last entry's commit, what the queue's checks run on.
type TrainCandidate struct {
	CandidateID string           `json:"candidate_id"`
	Ordinal     int              `json:"ordinal"`
	Entries     []CandidateEntry `json:"entries"`
	TipOID      string           `json:"tip_oid"`
}

// Eject puts live entries out of their queue: the record of op eject on the
// repository's subject.
//
// BASE FORMAT, as this file's head says.
type Eject struct {
	RepoID  string     `json:"repo_id"`
	Entries []Ejection `json:"entries"`
	// Routing is whom the record concerns, a routing an entry's change in
	// the entries' order — its authors told it left the queue, or that its
	// merge conflicted, and its enqueuer — STAMPED BY THE COMMITTER and
	// kept within the record's room ([keptRoutings]).
	Routing []ChangeRouting `json:"routing,omitempty"`
}

// Ejection is one entry an eject puts out: why ([EjectReasons]), and what of
// it a person reads first — the required checks that failed on what it was
// tested in, or the paths its merge conflicted on.
type Ejection struct {
	EntryID string         `json:"entry_id"`
	Reason  QueueEndReason `json:"reason"`
	Checks  []string       `json:"checks,omitempty"`
	Paths   []string       `json:"paths,omitempty"`
}

// Land lands a queue's entries on their target in one record: the record of op
// land on the repository's subject.
//
// BASE FORMAT, as this file's head says.
type Land struct {
	RepoID string `json:"repo_id"`
	// LandID is [QueueLandID] of the proposal's operation id: what each
	// entry's push id is derived from ([LandPushID]).
	LandID    string `json:"land_id"`
	TargetRef string `json:"target_ref"`
	// BaseOID is the target's tip the landing was built onto, which the
	// decide holds the target to; TipOID what it moves to — the last entry's
	// commit.
	BaseOID string `json:"base_oid"`
	TipOID  string `json:"tip_oid"`
	// Train is the train the landing lands a tested prefix of, whose pack
	// holds every commit it lands.
	Train string `json:"train"`
	// Entries are what lands, in the order the target moves through them,
	// the first onto BaseOID.
	Entries []LandEntry `json:"entries"`
	// Node is the Gitlet node that built the landing and ReceivedAt when it was
	// asked to — each push row's node and received_at.
	Node       string    `json:"node"`
	ReceivedAt time.Time `json:"received_at"`
	// Routing is whom the record concerns, a routing an entry's change in
	// the entries' order — its authors told it merged, its enqueuer, who
	// merged it, not — STAMPED BY THE DECIDE and kept within the record's
	// room ([keptRoutings]).
	Routing []ChangeRouting `json:"routing,omitempty"`
}

// LandEntry is one entry a landing lands: the entry, its change, and the
// commit the target moves to with it.
type LandEntry struct {
	EntryID  string `json:"entry_id"`
	ChangeID string `json:"change_id"`
	Commit   string `json:"commit"`
	// DeleteSource is STAMPED BY THE DECIDE: the entry asked its source
	// deleted and a push deleting it as its enqueuer would be taken — so the
	// apply deletes the change's source, read from its row, with no rule
	// matched; the source is kept otherwise, as a merge asked keeping it.
	DeleteSource bool `json:"delete_source,omitempty"`
}

// Green moves a protected target's green row onto the tip of a landing record
// whose required checks passed on it: the record of op green on the
// repository's subject.
//
// BASE FORMAT, as this file's head says.
type Green struct {
	RepoID    string `json:"repo_id"`
	TargetRef string `json:"target_ref"`
	// OID is the landing record's tip — its last landing's commit — and
	// Through the record's position on the code log.
	OID     string            `json:"oid"`
	Through statelog.Position `json:"through"`
}

// Broken marks a landing above its target's green row broken, the named
// required checks having failed on its commit: the record of op broken on the
// repository's subject.
//
// BASE FORMAT, as this file's head says.
type Broken struct {
	RepoID    string `json:"repo_id"`
	TargetRef string `json:"target_ref"`
	// Position and Ordinal are the landing's key on its target: the record
	// that landed it and its place there; OID its commit.
	Position statelog.Position `json:"position"`
	Ordinal  int               `json:"ordinal"`
	OID      string            `json:"oid"`
	// Checks are the required checks that failed on OID.
	Checks []string `json:"checks"`
	// Routing is whom the record concerns ([ChangeRouting]) — the authors
	// of the change the landing merged, its enqueuer and the effective lead
	// of the repository's owner — STAMPED BY THE COMMITTER; nil for a
	// landing no change made.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// QueueSweep removes repository RepoID's ended queue entries and its landings
// below their target's green row from before Through, the duty holder's clock
// less [QueueHistory]: the record of op queue_sweep on the repository's
// subject ([Store.QueueSweep]).
//
// BASE FORMAT, as this file's head says.
type QueueSweep struct {
	RepoID  string    `json:"repo_id"`
	Through time.Time `json:"through"`
}

// CloseSettled settles the closes landed entries owe their changes' work
// items (M5-D27, code_queue.close_op): the record of op close_settled on the
// repository's subject, which the repository's home asks once each close as
// its entry's enqueuer has ENDED — the item closed, or definitively not —
// never while the tracker's answer is still unknown, which leaves that close
// owed for the next turn on whichever home holds the repository.
//
// EVERY CLOSE A TURN ENDED, IN ONE RECORD: a close is one tracker write of
// its enqueuer's, but how it ended is the home's to record, and a landing of
// [MaxLandEntries] changes asking their items closed would otherwise cost the
// code log as many appends, one after another inside one queue turn, each a
// decide and an acknowledgement of the committer's. A not_closed close still
// names its change in a notice of its own: the record carries a routing a
// change ([CloseSettled.Routing]), as an eject's and a land's do. An entry
// another settle already cleared is SKIPPED by the decide, never a refusal
// of the rest; a record none of whose entries owes a close any more is
// refused stale.
//
// BASE FORMAT, as this file's head says.
type CloseSettled struct {
	RepoID string `json:"repo_id"`
	// Settled is how each close ended, an entry once, at most
	// [MaxOwedCloses] — as the committer records it, only the entries its
	// snapshot found still owing their close.
	Settled []SettledClose `json:"settled"`
	// Routing is whom the record's not_closed closes tell — for each, the
	// change's authors and its enqueuer, under `merged`, the detail beside it
	// ([ChangeRouting.NotClosed]) — a routing a change in Settled's order,
	// STAMPED BY THE COMMITTER and kept within the record's room
	// ([keptRoutings]); none for a closed one, which tells nobody.
	Routing []ChangeRouting `json:"routing,omitempty"`
}

// SettledClose is how one landed entry's owed close ended.
type SettledClose struct {
	EntryID string       `json:"entry_id"`
	Outcome CloseOutcome `json:"outcome"`
	// Detail is why a close that ended not_closed was not — the engine's
	// own sentence (a vendor's item it does not write, an enqueuer the chart
	// no longer lets update it, the tracker's refusal) — at most
	// [MaxCloseDetail] bytes; "" for a closed one.
	Detail string `json:"detail,omitempty"`
}

// CloseOutcome is how a landed entry's owed close ended — a closed set, in
// [CloseOutcomes]' order.
type CloseOutcome string

// The outcomes.
const (
	// CloseClosed is the item closed as the enqueuer, or found finished
	// already.
	CloseClosed CloseOutcome = "closed"
	// CloseNotClosed is a close that will not be made however often it is
	// asked: the item is a vendor's, the enqueuer may no longer update it,
	// or the tracker refused it.
	CloseNotClosed CloseOutcome = "not_closed"
)

// CloseOutcomes is the two, in order.
func CloseOutcomes() []CloseOutcome { return []CloseOutcome{CloseClosed, CloseNotClosed} }

// Valid reports one of [CloseOutcomes].
func (o CloseOutcome) Valid() bool { return slices.Contains(CloseOutcomes(), o) }

// MaxCloseDetail is the most bytes a not_closed settle's detail holds:
// [MaxNoticeExcerpt], since the detail is what its notice keeps of it — one
// sentence a person reads in their Inbox, never a log.
const MaxCloseDetail = MaxNoticeExcerpt

// validate refuses a settle no home asks: no close or more than
// [MaxOwedCloses], an entry that is no bare UUIDv7 or is named twice, an
// outcome none of [CloseOutcomes], a closed one carrying a detail or a
// not_closed one none, a detail past [MaxCloseDetail], a routing its asker
// set.
func (c CloseSettled) validate() error {
	if err := validRepoID("close_settled.repo_id", c.RepoID); err != nil {
		return err
	}
	if len(c.Routing) > 0 {
		return stampedRouting("close_settled", &c.Routing[0])
	}
	switch n := len(c.Settled); {
	case n == 0:
		return badField("close_settled.settled", "a settle settles at least one close")
	case n > MaxOwedCloses:
		return badField("close_settled.settled", "%d closes, past the %d one record settles (MaxOwedCloses)",
			n, MaxOwedCloses)
	}
	named := map[string]bool{}
	for i, s := range c.Settled {
		field := fmt.Sprintf("close_settled.settled[%d]", i)
		if err := bareUUIDv7(field+".entry_id", s.EntryID); err != nil {
			return err
		}
		if named[s.EntryID] {
			return badField(field+".entry_id", "%s is named twice", s.EntryID)
		}
		named[s.EntryID] = true
		switch {
		case !s.Outcome.Valid():
			return badField(field+".outcome", "%q is not one of %s", s.Outcome, words.Or(CloseOutcomes()))
		case s.Outcome == CloseClosed && s.Detail != "":
			return badField(field+".detail", "a closed item is settled with no reason it was not")
		case s.Outcome == CloseNotClosed && s.Detail == "":
			return badField(field+".detail", "a close that was not made says why")
		case len(s.Detail) > MaxCloseDetail:
			return badField(field+".detail", "%d bytes, past the %d a settle's sentence holds "+
				"(MaxCloseDetail)", len(s.Detail), MaxCloseDetail)
		}
	}
	return nil
}

// entries is the entries c settles, in its order.
func (c CloseSettled) entries() []string {
	ids := make([]string, len(c.Settled))
	for i, s := range c.Settled {
		ids[i] = s.EntryID
	}
	return ids
}

// validate refuses a candidate record no home builds ([ErrInvalidProposal],
// naming the field): its train not [QueueTrainID] of opID; no candidate or
// more than [MaxQueueDepth]; a candidate out of order, misnamed, empty or past
// [MaxQueueBatch]; an entry that is no bare UUIDv7 or named twice; a commit or
// a tip that is no whole object id, a tip that is not its candidate's last
// commit; a pack whose row is no bundle's, past [MaxWritePackBytes] or not
// named for the train.
func (c Candidate) validate(opID string) error {
	if err := validRepoID("candidate.repo_id", c.RepoID); err != nil {
		return err
	}
	if why := branchProblem(c.TargetRef); why != "" {
		return badField("candidate.target_ref", "%s", why)
	}
	if want := QueueTrainID(opID); c.Train != want {
		return badField("candidate.train", "%q, and the train a candidate under %s makes is %s", c.Train,
			opID, want)
	}
	if !wholeOID(c.BaseOID) {
		return badField("candidate.base_oid", "%q is not a whole object id", c.BaseOID)
	}
	switch n := len(c.Candidates); {
	case n == 0:
		return badField("candidate.candidates", "a train holds at least one candidate")
	case n > MaxQueueDepth:
		return badField("candidate.candidates", "%d candidates, past the %d a queue tests at once "+
			"(MaxQueueDepth)", n, MaxQueueDepth)
	}
	named := map[string]bool{}
	for i, cand := range c.Candidates {
		field := fmt.Sprintf("candidate.candidates[%d]", i)
		switch {
		case cand.Ordinal != i+1:
			return badField(field+".ordinal", "%d, and a train's candidates are numbered from 1 in order",
				cand.Ordinal)
		case cand.CandidateID != QueueCandidateID(c.Train, cand.Ordinal):
			return badField(field+".candidate_id", "%q is not the id of candidate %d of train %s", cand.CandidateID,
				cand.Ordinal, c.Train)
		case len(cand.Entries) == 0:
			return badField(field+".entries", "a candidate merges at least one entry")
		case len(cand.Entries) > MaxQueueBatch:
			return badField(field+".entries", "%d entries, past the %d one candidate takes (MaxQueueBatch)",
				len(cand.Entries), MaxQueueBatch)
		}
		for j, e := range cand.Entries {
			entry := fmt.Sprintf("%s.entries[%d]", field, j)
			if err := bareUUIDv7(entry+".entry_id", e.EntryID); err != nil {
				return err
			}
			if named[e.EntryID] {
				return badField(entry+".entry_id", "%s is named twice in the train", e.EntryID)
			}
			named[e.EntryID] = true
			if !wholeOID(e.Commit) {
				return badField(entry+".commit", "%q is not a whole object id", e.Commit)
			}
		}
		if last := cand.Entries[len(cand.Entries)-1].Commit; cand.TipOID != last {
			return badField(field+".tip_oid", "%q, and a candidate's tip is its last entry's commit %s",
				cand.TipOID, last)
		}
	}
	return checkQueuePack("candidate.pack", c.Pack, c.Train)
}

// validate refuses an eject record no home forms: no entry or more than
// [MaxLandEntries]; an entry that is no bare UUIDv7 or named twice; a reason
// not one of [EjectReasons]; checks named but for a failed check — at most
// MaxRequiredChecks, each a check's name — and paths but for a conflict, at
// most [MaxEjectedPaths] within [MaxConflictBytes], each one a record carries.
func (e Eject) validate() error {
	if err := validRepoID("eject.repo_id", e.RepoID); err != nil {
		return err
	}
	if len(e.Routing) > 0 {
		return stampedRouting("eject", &e.Routing[0])
	}
	switch n := len(e.Entries); {
	case n == 0:
		return badField("eject.entries", "an eject puts at least one entry out")
	case n > MaxLandEntries:
		return badField("eject.entries", "%d entries, past the %d one record ejects (MaxLandEntries)", n,
			MaxLandEntries)
	}
	named := map[string]bool{}
	for i, x := range e.Entries {
		field := fmt.Sprintf("eject.entries[%d]", i)
		if err := bareUUIDv7(field+".entry_id", x.EntryID); err != nil {
			return err
		}
		if named[x.EntryID] {
			return badField(field+".entry_id", "%s is named twice", x.EntryID)
		}
		named[x.EntryID] = true
		switch {
		case !slices.Contains(EjectReasons(), x.Reason):
			return badField(field+".reason", "%q is not one of %s", x.Reason, words.Or(EjectReasons()))
		case len(x.Checks) > 0 && x.Reason != EjectCheckFailed:
			return badField(field+".checks", "only a failed check's ejection names checks")
		case len(x.Paths) > 0 && x.Reason != EjectConflict:
			return badField(field+".paths", "only a conflict's ejection names paths")
		case len(x.Checks) > MaxRequiredChecks:
			return badField(field+".checks", "%d checks, past the %d a gate requires (MaxRequiredChecks)",
				len(x.Checks), MaxRequiredChecks)
		case len(x.Paths) > MaxEjectedPaths:
			return badField(field+".paths", "%d paths, past the %d an ejection names (MaxEjectedPaths)",
				len(x.Paths), MaxEjectedPaths)
		case ConflictsCost(x.Paths) > MaxConflictBytes:
			return badField(field+".paths", "the list is %d bytes of JSON, past the %d an ejection names "+
				"(MaxConflictBytes)", ConflictsCost(x.Paths), MaxConflictBytes)
		}
		for j, name := range x.Checks {
			if !config.ValidCodeCheckName(name) {
				return badField(fmt.Sprintf("%s.checks[%d]", field, j), "%q is not a check name", name)
			}
		}
		for j, p := range x.Paths {
			if !ConflictPathCarried(p) {
				return badField(fmt.Sprintf("%s.paths[%d]", field, j), "%q is no path a record carries", p)
			}
		}
	}
	return nil
}

// validate refuses a land record no home builds: its id not [QueueLandID] of
// opID; a target that is no branch; a base or a tip that is no whole object
// id, a tip that is not its last entry's commit; a train that is no bare
// UUIDv7; no entry or more than [MaxLandEntries]; an entry or a change that
// is no bare UUIDv7 or named twice, a commit that is no whole object id or the
// base; no node, or no instant it was asked at; and a field the decide stamps
// set by its asker.
func (l Land) validate(opID string) error {
	if err := validRepoID("land.repo_id", l.RepoID); err != nil {
		return err
	}
	if len(l.Routing) > 0 {
		return stampedRouting("land", &l.Routing[0])
	}
	if want := QueueLandID(opID); l.LandID != want {
		return badField("land.land_id", "%q, and the landing a land under %s is is %s", l.LandID, opID, want)
	}
	if why := branchProblem(l.TargetRef); why != "" {
		return badField("land.target_ref", "%s", why)
	}
	for _, oid := range []struct{ field, value string }{{"land.base_oid", l.BaseOID}, {"land.tip_oid", l.TipOID}} {
		if !wholeOID(oid.value) {
			return badField(oid.field, "%q is not a whole object id", oid.value)
		}
	}
	if err := bareUUIDv7("land.train", l.Train); err != nil {
		return err
	}
	switch n := len(l.Entries); {
	case n == 0:
		return badField("land.entries", "a landing lands at least one entry")
	case n > MaxLandEntries:
		return badField("land.entries", "%d entries, past the %d one record lands (MaxLandEntries)", n,
			MaxLandEntries)
	}
	entries, changes := map[string]bool{}, map[string]bool{}
	for i, e := range l.Entries {
		field := fmt.Sprintf("land.entries[%d]", i)
		if err := bareUUIDv7(field+".entry_id", e.EntryID); err != nil {
			return err
		}
		if err := bareUUIDv7(field+".change_id", e.ChangeID); err != nil {
			return err
		}
		switch {
		case entries[e.EntryID]:
			return badField(field+".entry_id", "%s is named twice", e.EntryID)
		case changes[e.ChangeID]:
			return badField(field+".change_id", "%s is named twice", e.ChangeID)
		case !wholeOID(e.Commit):
			return badField(field+".commit", "%q is not a whole object id", e.Commit)
		case e.Commit == l.BaseOID:
			return badField(field+".commit", "the commit is the landing's base %s: a landing moves its target",
				short(l.BaseOID))
		case e.DeleteSource:
			return badField(field+".delete_source", "the committer stamps whether a landing deletes a source "+
				"from the entry it reads, and an asker sends none")
		}
		entries[e.EntryID], changes[e.ChangeID] = true, true
	}
	if last := l.Entries[len(l.Entries)-1].Commit; l.TipOID != last {
		return badField("land.tip_oid", "%q, and a landing's tip is its last entry's commit %s", l.TipOID, last)
	}
	switch {
	case !config.ValidNodeID(l.Node):
		return badField("land.node", "%q is not a node id config admits", l.Node)
	case l.ReceivedAt.IsZero():
		return badField("land.received_at", "a landing names when its builder was asked for it")
	}
	return nil
}

// validate refuses a green record: a target that is no branch, an object id
// that is no whole one, a position that is none on the code log.
func (g Green) validate() error {
	if err := validRepoID("green.repo_id", g.RepoID); err != nil {
		return err
	}
	if why := branchProblem(g.TargetRef); why != "" {
		return badField("green.target_ref", "%s", why)
	}
	if !wholeOID(g.OID) {
		return badField("green.oid", "%q is not a whole object id", g.OID)
	}
	return onCodeLog("green.through", g.Through)
}

// validate refuses a broken record: a target that is no branch, a landing key
// that is none, an object id that is no whole one, no check or more than a gate
// requires, a name that is no check's.
func (b Broken) validate() error {
	if err := validRepoID("broken.repo_id", b.RepoID); err != nil {
		return err
	}
	if err := stampedRouting("broken", b.Routing); err != nil {
		return err
	}
	if why := branchProblem(b.TargetRef); why != "" {
		return badField("broken.target_ref", "%s", why)
	}
	if err := onCodeLog("broken.position", b.Position); err != nil {
		return err
	}
	switch {
	case b.Ordinal < 1:
		return badField("broken.ordinal", "%d; a landing's place in its record is from 1", b.Ordinal)
	case !wholeOID(b.OID):
		return badField("broken.oid", "%q is not a whole object id", b.OID)
	case len(b.Checks) == 0:
		return badField("broken.checks", "a landing is broken by a required check that failed on it")
	case len(b.Checks) > MaxRequiredChecks:
		return badField("broken.checks", "%d checks, past the %d a gate requires (MaxRequiredChecks)",
			len(b.Checks), MaxRequiredChecks)
	}
	for i, name := range b.Checks {
		if !config.ValidCodeCheckName(name) {
			return badField(fmt.Sprintf("broken.checks[%d]", i), "%q is not a check name", name)
		}
		if slices.Contains(b.Checks[:i], name) {
			return badField(fmt.Sprintf("broken.checks[%d]", i), "%q is named twice", name)
		}
	}
	return nil
}

// wholeOID reports a whole object id other than the zero one.
func wholeOID(oid string) bool { return gitref.ValidOID(oid) && oid != ZeroOID }

// onCodeLog refuses a position that is none on the code log.
func onCodeLog(field string, p statelog.Position) error {
	switch {
	case p.IsZero():
		return badField(field, "names no position on the code log")
	case p.Stream != topics.CodeLogStream:
		return badField(field, "%s is a position on another log than the code log (%s)", p, topics.CodeLogStream)
	case p.Valid() != nil:
		return fmt.Errorf("%w: %s: %w", ErrInvalidProposal, field, p.Valid())
	}
	return nil
}

// checkQueuePack refuses a queue record's pack whose row is no bundle's
// ([PackRef.Check]), past what an engine-built pack may store
// ([MaxWritePackBytes]) — the home's build's own bound, a merge's — or not
// named id, the record's own derived id.
func checkQueuePack(field string, pack *PackRef, id string) error {
	if pack == nil {
		return nil
	}
	if err := pack.Check(); err != nil {
		return fmt.Errorf("%w: %s.%w", ErrInvalidProposal, field, err)
	}
	switch {
	case pack.PackBytes() > MaxWritePackBytes:
		return badField(field, "%d bytes of pack, past the %d an engine-built pack may store "+
			"(MaxWritePackBytes)", pack.PackBytes(), MaxWritePackBytes)
	case pack.PackID != id:
		return badField(field+".pack_id", "%q, and the record's pack is named %s", pack.PackID, id)
	}
	return nil
}
