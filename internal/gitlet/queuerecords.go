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

// A queue's own records: what a target's merge queue does with the entries an
// enqueue made (queue.go), each on the repository's subject, decided by its
// committer alone from one snapshot ([Committer.ProposeQueue]) and built by its
// home Gitlet node. A [Candidate] records a target's one TRAIN as rows and a
// pack, never a ref (M5-D12), abandoning its earlier train; an [Eject] puts
// entries out and abandons their train; a [Land] lands a train's tested prefix
// in ONE record, its decide proving each entry's gate again with the entry's
// ENQUEUER as merger, since the landing moves the branch (M5-D8), and its apply
// writes one push row an entry, `via queue`, naming that enqueuer: the queue
// never acts on its own account. A [Green] advances a protected target's green
// row onto a landing no queue tested whose checks passed (never on red, never
// back), and a [Broken] marks one above it that failed.

// A train and a landing are built at a drop epoch: each proposal carries its
// home copy's ([QueueProposal.DropEpoch]) and its decide refuses it
// [MergeRefusalExpunging] where an expunge has begun since, before its own
// rules, as a push's and a merge's do, since a build before a begin may rely on
// objects the begin left dropping. An expunge walks from every live entry's
// head, candidate's tip and target's green row (`expunge_tips`), so a purge
// never drops the commit a landing moves its target onto.

// Base format, as queue.go's head says: every op, payload and field here, a
// land's apply setting code_queue.close_op, and the values they add to closed
// sets (queue as `merged_via`, a push's `via` and a landing's `kind`). None is
// a gate. An ejection's `tested_in` alone is version 7 ([TestedInVersion]).

// The queue's bounds.
const (
	// MaxCandidateEntries is the most entries one train holds, MaxQueueDepth ×
	// MaxQueueBatch (64), so one candidate record carries every candidate a
	// queue tests at once.
	MaxCandidateEntries = MaxQueueDepth * MaxQueueBatch

	// MaxLandEntries is the most entries one land record lands:
	// [MaxPushesPerRecord] (64), since it lands one push row an entry, and a
	// train's whole tested prefix ([MaxCandidateEntries], also 64) fits. A
	// record of all 64, each deleting its source, holds the write lane 33 ms
	// (BenchmarkApplyALandRecord, doc.go's measured section), under the third
	// of statelog.ApplyTxTimeBudget the domain's records are sized to.
	MaxLandEntries = MaxPushesPerRecord

	// MaxEjectedPaths is the most paths one ejection names of a conflicted
	// merge: [MaxConflictPaths], as a change's mergeability names of the same
	// conflict, within [MaxConflictBytes].
	MaxEjectedPaths = MaxConflictPaths

	// QueueHistory is how long a queue's ended entries and its target's
	// landings below its green row are kept: [CheckHistory], thirty days, as
	// long as the checks they were judged by, which they cite.
	QueueHistory = CheckHistory

	// QueueSweepGrace is how long past [QueueHistory] a repository's queue
	// history outlives it before the code-retention duty sweeps it: a day,
	// [CheckSweepGrace], for its reason. A working queue's own records remove
	// its history, so the duty never appends what one of them would.
	QueueSweepGrace = CheckSweepGrace

	// QueueSweepRows is the most rows one queue_sweep record removes:
	// MaxRecordRows but the repository's own, whose version it moves, as
	// [RetireSweepRows]. It bounds NARROW rows (an ended entry, a landing);
	// [QueueSweepBytes] bounds the ejections among them.
	QueueSweepRows = MaxRecordRows - 1

	// QueueSweepBytes is how many BYTES of ejections one queue_sweep record
	// removes beside QueueSweepRows' count: ended entries oldest first, each
	// weighed by its ejection's JSON (code_queue.ejected_bytes), until they
	// reach it (at least one, at most one past it); a step whose entries it cut
	// takes no landing. A row count bounds a delete's time only for narrow
	// rows: an ejection is up to some 16 KiB of overflow pages, so rows alone
	// let a step remove some 64 MiB. Bounded here, a step of the widest holds
	// the write lane 64 ms, against 44 ms for QueueSweepRows narrow entries
	// (BenchmarkApplyAQueueSweep, doc.go's measured section), both under the
	// third of statelog.ApplyTxTimeBudget: [PurgePartBytes], the measured bytes
	// of the same overflow-page deletes.
	QueueSweepBytes = PurgePartBytes

	// queueHorizonRows is the most rows one candidate, eject or land record
	// removes of its repository's queue history past [QueueHistory], ended
	// entries, then landings: twice [MaxLandEntries], since a land ends that many
	// entries and writes that many landings at most, so a working queue's
	// history keeps pace and the sweep only reaches an idle one.
	queueHorizonRows = 2 * MaxLandEntries

	// queueHorizonBytes is how many bytes of ejections one candidate, eject or
	// land record removes beside queueHorizonRows' count, weighed as
	// [QueueSweepBytes] weighs them: [HorizonBytesPerRecord], since no record
	// adds more ejection bytes than its own, so removal keeps pace in bytes.
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
	// ([RepoRef.Mirrored]), which moves only by its syncs: a candidate and a
	// landing on it are refused repo_mirrored, so its home ejects every live
	// entry rather than build, every turn, a train no landing could land. The
	// decide refuses it stale on a repository that is not one ([decideEject]).
	EjectRepoMirrored QueueEndReason = "repo_mirrored"
)

// EjectReasons is the closed set an eject record may name, in this order.
func EjectReasons() []QueueEndReason {
	return []QueueEndReason{EjectCheckFailed, EjectConflict, EjectGateUnmet, EjectHeadMoved, EjectNotFastForward,
		EjectRepoMirrored}
}

// MergedViaQueue is how a change a land record merged reached its target
// ([MergedVia]): its queue landed it, tested merged on its target first.
const MergedViaQueue MergedVia = "queue"

// Candidate records a queue's TRAIN on one target: its candidates, built by
// the repository's home onto the target's tip, and the pack their commits are
// stored in. A target holds one train: a candidate abandons the earlier one,
// its entries waiting again. The record of op candidate.
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

// Eject puts live entries out of their queue and abandons the train each was
// in: the record of op eject.
type Eject struct {
	RepoID  string     `json:"repo_id"`
	Entries []Ejection `json:"entries"`
	// Routing is whom the record concerns (its change's authors and its
	// enqueuer), one an entry in order, STAMPED BY THE COMMITTER and kept
	// within the record's room ([keptRoutings]).
	Routing []ChangeRouting `json:"routing,omitempty"`
}

// Ejection is one entry an eject puts out: why ([EjectReasons]) and the
// required checks that failed on it or the paths its merge conflicted on. A
// bisected culprit's ejection also names the train it failed on.
type Ejection struct {
	EntryID string         `json:"entry_id"`
	Reason  QueueEndReason `json:"reason"`
	Checks  []string       `json:"checks,omitempty"`
	Paths   []string       `json:"paths,omitempty"`
	// TestedIn is the train a bisected culprit's checks failed on, stamped by
	// its home since the train's rows go with the eject; nil otherwise and
	// before [TestedInVersion].
	TestedIn *EjectionTrain `json:"tested_in,omitempty"`
}

// EjectionTrain is the train an ejection's checks failed on: its candidate,
// the commit it merged onto (the target's tip, for a train's first) and the
// commit the checks failed on, its merge up to the entry. VERSION 7
// ([TestedInVersion]).
type EjectionTrain struct {
	CandidateID string `json:"candidate_id"`
	BaseOID     string `json:"base_oid"`
	CommitOID   string `json:"commit_oid"`
}

// Land lands a queue's entries on their target in one record: the record of op
// land. Its decide proves each entry's gate again ([Committer.ProposeQueue]).
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
	// Routing is whom the record concerns, one an entry's change in order:
	// its authors, not its enqueuer, who merged it. STAMPED BY THE DECIDE and
	// kept within the record's room ([keptRoutings]).
	Routing []ChangeRouting `json:"routing,omitempty"`
}

// LandEntry is one entry a landing lands: the entry, its change, and the
// commit the target moves to with it.
type LandEntry struct {
	EntryID  string `json:"entry_id"`
	ChangeID string `json:"change_id"`
	Commit   string `json:"commit"`
	// DeleteSource is STAMPED BY THE DECIDE when the entry asked its source
	// deleted and its enqueuer's push deleting it would be taken, so the apply
	// deletes the source with no rule matched; otherwise the source is kept.
	DeleteSource bool `json:"delete_source,omitempty"`
}

// Green moves a protected target's green row onto the tip of a landing record
// whose required checks passed on it, a landing no queue tested first (a
// person's override merge or push): the record of op green.
type Green struct {
	RepoID    string `json:"repo_id"`
	TargetRef string `json:"target_ref"`
	// OID is the landing record's tip — its last landing's commit — and
	// Through the record's position on the code log.
	OID     string            `json:"oid"`
	Through statelog.Position `json:"through"`
}

// Broken marks a landing above its target's green row broken, the named
// required checks having failed on its commit: the record of op broken.
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
	// Routing is whom the record concerns ([ChangeRouting]): the landed
	// change's authors, its enqueuer and the repository owner's effective
	// lead. STAMPED BY THE COMMITTER; nil for a landing no change made.
	Routing *ChangeRouting `json:"routing,omitempty"`
}

// QueueSweep removes an IDLE repository's ended queue entries and its landings
// below their target's green row from before Through, the duty holder's clock
// less [QueueHistory] (M5-D34; a working queue's records remove them): the
// record of op queue_sweep ([Store.QueueSweep]).
type QueueSweep struct {
	RepoID  string    `json:"repo_id"`
	Through time.Time `json:"through"`
}

// CloseSettled settles the closes landed entries owe their changes' work
// items (M5-D27, code_queue.close_op, set by the land): the record of op
// close_settled. The home asks it once each close, made as the entry's
// enqueuer, has ENDED (closed, or definitively not); one whose tracker answer
// is unknown, or whose home stopped first, stays owed to the next turn on any
// home, never lost. Every close a turn ended goes in ONE record, so a landing
// of [MaxLandEntries] changes costs one append, not one each in the turn; a
// not_closed close still gets its own notice ([CloseSettled.Routing]). The
// decide SKIPS an entry another settle cleared, never refusing the rest, and
// refuses stale a record none of whose entries still owes a close.
type CloseSettled struct {
	RepoID string `json:"repo_id"`
	// Settled is how each close ended, an entry once, at most
	// [MaxOwedCloses]; the committer records only the entries its snapshot
	// found still owing their close.
	Settled []SettledClose `json:"settled"`
	// Routing is whom the not_closed closes tell (the change's authors and
	// its enqueuer, under `merged`, with [ChangeRouting.NotClosed]), one a
	// change in Settled's order, STAMPED BY THE COMMITTER and kept within the
	// record's room ([keptRoutings]); a closed one tells nobody.
	Routing []ChangeRouting `json:"routing,omitempty"`
}

// SettledClose is how one landed entry's owed close ended.
type SettledClose struct {
	EntryID string       `json:"entry_id"`
	Outcome CloseOutcome `json:"outcome"`
	// Detail is the engine's own sentence why a not_closed close was not, at
	// most [MaxCloseDetail] bytes; "" for a closed one.
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
// [MaxNoticeExcerpt], what its notice keeps: one sentence, never a log.
const MaxCloseDetail = MaxNoticeExcerpt

// validate refuses a settle no home asks, a routing its asker set among them.
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
// naming the field).
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

// validate refuses an eject record no home forms, a routing its asker set
// among them.
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
		if err := x.TestedIn.validate(field+".tested_in", x); err != nil {
			return err
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

// validate refuses a train x names that no home stamps. A nil train is valid.
func (t *EjectionTrain) validate(field string, x Ejection) error {
	switch {
	case t == nil:
		return nil
	case x.Reason != EjectCheckFailed:
		return badField(field, "only a failed check's ejection names the train it failed on")
	case len(x.Checks) == 0:
		return badField(field, "an ejection names the checks that failed on its train")
	}
	if err := bareUUIDv7(field+".candidate_id", t.CandidateID); err != nil {
		return err
	}
	switch {
	case !wholeOID(t.BaseOID):
		return badField(field+".base_oid", "%q is no whole object id", t.BaseOID)
	case !wholeOID(t.CommitOID):
		return badField(field+".commit_oid", "%q is no whole object id", t.CommitOID)
	case t.CommitOID == t.BaseOID:
		return badField(field+".commit_oid", "a merge onto %s is not %s itself", short(t.BaseOID),
			short(t.CommitOID))
	}
	return nil
}

// validate refuses a land record no home builds, a field the decide stamps set
// by its asker among them.
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

// validate refuses a green record no home asks.
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

// validate refuses a broken record no home asks, a routing its asker set
// among them.
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
// ([PackRef.Check]), past [MaxWritePackBytes] (the home's build's own bound,
// as a merge's) or not named id, the record's own derived id.
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
