package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE COMMITTER: GROUP COMMIT of one repository's pushes, in order, on one
// goroutine, each record carrying every push already waiting when the last
// one finished — WITHOUT LINGER (doc.go's "Group commit"; "What a push costs,
// measured" for why it pays).
//
// A batch is gathered by each push's own bytes and rows, but the rows of the
// live changes its pushes move (revisions.go) only a snapshot can count. So
// the decide CARRIES a push whose changes would take the record past
// MaxRecordRows: it and the pushes behind it lead the next batch in arrival
// order. Only one whose changes would not fit a record of its own is refused
// too_many_changes.

// An ok is only ever reported from a row, read once a batch, never from the
// decision: a record a gate dropped or a round lost to a peer writes no row
// ([Committer.answers]). A batch runs under the committer's own lifetime, not
// a caller's: a caller that hangs up stops waiting, and the batch goes on.
//
// A push is decided only while its answer fits its asker's ONE write attempt,
// whose end a routed proposal carries; past it the router hands the same push
// id to the runner-up, which would contend with this committer on the subject.
// It is judged against the deadline itself, never a wait counted from
// arrival (what the asker spent before the push arrived is its attempt too),
// both while it waits ([Committer.await]) and as its batch forms
// ([Committer.admit]). A push is TAKEN or WITHDRAWN, never both ([queued.state]).

// A batch is published with the highest of its pushes' [Proposal.Seen] as its
// session: the committer may be behind the node that checked a push, and
// decided from below it the push is told a false definite refusal (stale,
// repo_deleted) that no compare-and-set catches, since a decision that moves
// nothing is never put to the broker. Not reached within the publisher's
// budget, it is refused behind — about this node, so the runner-up decides.
// The floor is per batch, never kept by the node: it is somebody else's writes.
//
// Which code-log refusal is about THIS node, answered [ErrNotServing] so the
// runner-up commits the push, and which is the client's (not_applied,
// repo_deleted, unavailable) is [Committer.refused]'s, by the refusal's kind
// and whether it names the batch's own record, never by its position.

// A node that cannot judge a seat's push decides none and says so about
// itself, [ErrNotServing]: confirmed behind the company's configuration
// ([CommitterOptions.Current]) or holding no chart yet
// ([CommitterOptions.SeatRules]). A person's push reads no chart and is
// decided in both; a mixed batch decides the persons' and hands the seats' on.

// Every other proposal — a compaction ([Committer.ProposeCompaction]), an
// expunge's records ([Committer.ProposeExpunge]), a change's lifecycle write
// ([Committer.ProposeChange]), a merge queue's write ([Committer.ProposeQueue])
// and a merge ([Committer.ProposeMerge]) — waits in the same ONE queue under
// the same bounds and deadline rules, and is decided alone, a record of its own
// op under its own operation id (asked again of the runner-up, the ledger
// collapses it), answered from the rows it wrote. One queue and one writer a
// repository is why its proposals land in arrival order and never contend from
// one node, and why no write lands between the snapshot that proved a merge's
// gate and the record that moves its target (ADR-0096).

// The node's bounds (CommitterNodeProposals, CommitterNodeBytes) sit beside
// each queue's (CommitterInbox, CommitterInboxBytes) because a node commits
// for every repository rendezvous ranks it first for: per-repository bounds
// alone let a thousand of the largest pushes hold about a gibibyte of entries
// (EntryRoom each) and as much decoded, and small pushes to a few repositories
// take more answer slots of the estate's proposal lane than it has, the rest
// waiting unseen in the broker's buffer rather than refused.
//
// Past any bound a proposal is refused busy AT ONCE, never blocked. Every such
// refusal, the time bound's too, is a BACKLOG kept for [CommitBusyWindow] by
// repository for code_commit_busy ([Committer.AlarmReading], busy.go), except
// one that ARRIVED with its asker's attempt already spent.

// CommitterOptions configure a committer.
type CommitterOptions struct {
	// Publisher is the code log's write authority.
	Publisher Publisher
	// DB is the replicated estate an applied batch's answers are read from.
	DB store.ReplicatedReader
	// NodeID is the deciding node, which every record names as its actor.
	NodeID string
	// Limits is the deciding node's epoch's code.limits. REQUIRED: a
	// decision input is never defaulted here.
	Limits func() Limits
	// Retention is the deciding node's epoch's code.deleted_retention_days:
	// when a deleted repository's purge may begin, which a push refused
	// repo_deleted is told ([RepoGone]). REQUIRED: a default would name the
	// wrong instant to every company that set its own.
	Retention func() time.Duration
	// SeatRules is the deciding node's epoch's chart and calendar, which a
	// SEAT'S push is judged by (its grant, decide rule 2b; its share of a
	// repository outside its unit; its day's quota), read once a batch that
	// carries one. False before this node installs its first epoch: every
	// seat's push is then answered [ErrNotServing], so the runner-up decides
	// it, and a person's is decided as ever. A nil roster would let a seat's
	// push land unjudged, or RECORD a not_permitted no chart made. REQUIRED.
	SeatRules func() (SeatRules, bool)
	// Current is the engine's posture gate (configplane's Reconciler.Admits):
	// false exactly while this node is CONFIRMED behind the company's
	// configuration (shed or stuck, never lag alone); nil is current. While
	// false, every proposal the chart judges is answered [ErrNotServing] as it
	// is queued and again as its batch forms, so the runner-up decides it;
	// without it, a node stuck before a revision revoking a seat would go on
	// landing that seat's pushes. A person's push reads no chart and is
	// decided as ever.
	Current func() bool
	// Now is the decide's clock — what upload_expired judges a pack's age by,
	// the company day a seat's push is counted on, a record's authored
	// instant, and the instant a landed record's announcement carries
	// (announce.go); nil takes the wall clock in UTC.
	Now func() time.Time
	// Events is this node's event stream, on which the committer's own
	// announcer (announce.go; NewCommitter starts it, Stop ends it) announces
	// `change_merged`, `change_opened`, `code_queue_ejected` and
	// `code_target_broken` for the records it landed; nil starts none and
	// announces none.
	Events EventPublisher
	// AgentIDs resolves a seat's handle to the agent id its day is counted
	// under (ADR-0013), off the node's live epoch — false for a human seat
	// and a handle the chart does not hold. REQUIRED with Events: an opened
	// change announced without its owner's id is counted on no seat's day.
	AgentIDs func(handle string) (string, bool)
	// Metrics records the batch sizes and the busy refusals; nil records
	// nothing.
	Metrics *metrics.Recorder
	// Logger is where a failed batch is reported, and a minute's busy
	// refusals of a repository (code_commit_busy); nil is the package's.
	Logger *slog.Logger
}

// SeatRules is the chart and the calendar a SEAT'S push is decided by, read
// from the DECIDING node's epoch ([CommitterOptions.SeatRules]) — so two nodes
// on different activations may judge one seat's command differently only while
// one's lag is unconfirmed, about a minute at worst (configplane.LagGrace,
// 45 s, from its first read of the activation's pointer, and its next posture
// decision): a node CONFIRMED behind decides no seat's push at all
// ([CommitterOptions.Current], ADR-0088).
type SeatRules struct {
	// Roster is the chart a seat's grant is judged by ([GrantOf]).
	Roster Roster
	// Zone is the company's clock (its timezone, ADR-0018), which the day a
	// seat's pushes are counted on is cut by: nil is UTC.
	Zone *time.Location
	// Epoch is the activation Roster is of (configplane.ActivationStamp, Unix
	// milliseconds): what a [Mergeability] stamps the readiness it judges by
	// Roster with ([Mergeability.ChartEpoch]), so the home re-judges a change
	// whose stored answer an older chart judged. 0 is a chart of no known
	// activation, which no home re-judges anything for.
	Epoch int64
}

// Committer is this node's group commit of pushes, a goroutine a repository
// with work.
type Committer struct {
	pub       Publisher
	db        store.ReplicatedReader
	nodeID    string
	limits    func() Limits
	retention func() time.Duration
	seatRules func() (SeatRules, bool)
	current   func() bool
	now       func() time.Time
	events    EventPublisher
	agentIDs  func(string) (string, bool)
	metrics   *metrics.Recorder
	log       *slog.Logger

	// idle is CommitterIdle, and a test's shorter value (export_test.go).
	idle time.Duration
	// clock is the queue's clock, which a push's wait is measured by — the
	// wall clock's monotonic reading, never Now (the decide's clock, which a
	// caller may pin), and a test's own (export_test.go).
	clock func() time.Time
	// withdrawAfter, when set, arms a queued push's withdrawal in place of a
	// timer ([Committer.withdrawal]), so the suite reads what it is armed with
	// and fires it itself (export_test.go): the moment it fires is the claim,
	// and waiting on the machine's clock cannot tell a timer armed late from
	// one armed on time. Nil, as in production, is a timer.
	withdrawAfter func(d time.Duration) (fire <-chan time.Time, stop func())
	// nodeBytes is CommitterNodeBytes, and nodeProposals
	// CommitterNodeProposals; each a test's smaller value (export_test.go).
	nodeBytes     int
	nodeProposals int
	// formedInRound, when set, is told how many protection verdicts each
	// decide round formed inside the publisher's snapshot — a test's
	// (export_test.go), which holds a round under the rules the formation
	// before the publish read to forming none there.
	formedInRound func(formed int)
	// queueEntries is MaxQueueEntries, and a test's smaller value
	// (export_test.go).
	queueEntries int

	// busyWindow is every busy refusal of the last CommitBusyWindow, a
	// bucket a minute, which [Committer.AlarmReading] reads (busy.go).
	busyWindow busyWindow

	// life is the batches' context: the committer's own lifetime, never a
	// caller's — THE ONE CONTEXT STORED IN A STRUCT, because a batch is
	// every caller's in it and none of theirs. Ended by Stop once its wait
	// is over.
	life context.Context
	end  context.CancelFunc

	mu    sync.Mutex
	repos map[string]*repoCommitter
	// held is the entryBound of every proposal this node holds, across its
	// repositories, from its queueing until its caller has its answer —
	// queued or in a batch — and holding how many proposals those are. A
	// caller that leaves before its answer releases its push while a batch
	// that took it still runs, which is at most one batch a repository for at
	// most a CommitBudget.
	held     int
	holding  int
	stopped  bool
	stopping chan struct{}
	wg       sync.WaitGroup

	// announcements is the announcer's queue of landed records' events
	// (announce.go), nil for a committer with no event stream; sent to only
	// by the repositories' goroutines, which wg counts, so Stop closes it
	// once wg is done (endAnnouncements) and announcerDone is closed once
	// the announcer has drained it.
	announcements    chan announcement
	announcerDone    chan struct{}
	endAnnouncements func()
}

// repoCommitter is one repository's queue and the goroutine draining it.
type repoCommitter struct {
	repoID string
	inbox  chan *queued
	// queued is the entryBound of everything in inbox; changed only under
	// the committer's mu.
	queued int
}

// queued is one proposal waiting for its batch: a push's, or a compaction's,
// an expunge record's, a change's, a queue's write or a merge, which wait in
// the same queue in the same arrival order and are each decided as a batch of
// their own ([Committer.ProposeCompaction], [Committer.ProposeExpunge],
// [Committer.ProposeChange], [Committer.ProposeQueue],
// [Committer.ProposeMerge]).
type queued struct {
	// p is the push this proposal is; compaction, expunge, change,
	// queueWrite or merge, where one is set, is the compaction, the expunge
	// record, the write of a change, the write of a merge queue or the merge
	// it is instead, and p then its zero value.
	p          Proposal
	compaction *CompactionProposal
	expunge    *ExpungeProposal
	remote     *remoteWrite
	change     *ChangeProposal
	queueWrite *QueueProposal
	merge      *MergeProposal
	// bytes is entryBound(p), or a compaction's, an expunge record's, a
	// change's, a queue's write's or a merge's encoded payload; rows is
	// rowBound(p), compactDecidedRows of a compaction, mergeRecordRows of a
	// merge, or decidedRows of an expunge record, a change or a queue's
	// write.
	bytes int
	rows  int
	// arrived is when the proposal was queued, by the committer's clock;
	// deadline is its caller's — the end of its asker's attempt, zero for a
	// caller with none; gone is its caller's context ending — nil for a
	// caller whose context never ends.
	arrived  time.Time
	deadline time.Time
	gone     <-chan struct{}
	// state is who holds the push: the queue, a batch that took it, or
	// nobody — withdrawn undecided. It leaves pushWaiting by compare-and-swap
	// alone, so the repository's goroutine taking it and its caller
	// withdrawing it never both win.
	state atomic.Int32
	// answer receives at most one value; capacity 1, so the goroutine never
	// blocks on a caller that hung up.
	answer chan answered
}

// A queued push's states ([queued.state]).
const (
	pushWaiting int32 = iota
	pushTaken
	pushWithdrawn
)

// repoID is the repository q's queue is.
func (q *queued) repoID() string {
	switch {
	case q.compaction != nil:
		return q.compaction.Compaction.RepoID
	case q.expunge != nil:
		return q.expunge.RepoID()
	case q.remote != nil:
		return q.remote.repoID()
	case q.change != nil:
		return q.change.RepoID()
	case q.queueWrite != nil:
		return q.queueWrite.RepoID()
	case q.merge != nil:
		return q.merge.RepoID()
	}
	return q.p.RepoID
}

// The committer's work, as the busy count's `kind` attribute names it.
const (
	kindPush       = "push"
	kindCompaction = "compaction"
	kindExpunge    = "expunge"
	kindImport     = "import"
	kindMirrorSync = "mirror_sync"
	kindChange     = "change"
	kindQueue      = "merge_queue"
	kindMerge      = "merge"
)

// kind is what q is, as the busy count names it.
func (q *queued) kind() string {
	switch {
	case q.compaction != nil:
		return kindCompaction
	case q.expunge != nil:
		return kindExpunge
	case q.remote != nil:
		return q.remote.kind()
	case q.change != nil:
		return kindChange
	case q.queueWrite != nil:
		return kindQueue
	case q.merge != nil:
		return kindMerge
	}
	return kindPush
}

// alone reports a proposal decided as a batch of its own — a compaction's, an
// expunge record's, a change's, a queue's write or a merge, each a record of
// its own op — never gathered with pushes.
func (q *queued) alone() bool {
	return q.compaction != nil || q.expunge != nil || q.remote != nil || q.change != nil || q.queueWrite != nil ||
		q.merge != nil
}

// judgedByChart reports a proposal whose decide reads the chart — a seat's
// push, a seat's write of a change ([PrincipalKind.readsChart]), every enqueue
// and a seat's dequeue ([QueueProposal.JudgesChart]), and every merge
// ([MergeProposal.JudgesChart]) — which a node behind the company's
// configuration, or one with no chart yet, never decides. A compaction and an
// expunge record judge no principal by any chart.
func (q *queued) judgedByChart() bool {
	switch {
	case q.change != nil:
		return q.change.JudgesChart()
	case q.queueWrite != nil:
		return q.queueWrite.JudgesChart()
	case q.merge != nil:
		return q.merge.JudgesChart()
	case q.alone():
		return false
	}
	// A SEAT'S PUSH, whose grant the chart says, and a push that asks for a
	// change: its options' text is held to the chart for whom it mentions
	// (concerns.go), as an open's mentions are.
	return q.p.Principal.Kind.readsChart() || q.p.Change != nil
}

// behindFor is why this node may not decide q now, or "": a proposal whose
// decide reads the chart, while the node is confirmed behind the company's
// configuration ([CommitterOptions.Current]).
func (c *Committer) behindFor(q *queued) string {
	if !q.judgedByChart() || c.current == nil || c.current() {
		return ""
	}
	return fmt.Sprintf("this node is behind the company's configuration and decides no %s from the chart the "+
		"fleet replaced; the repository's next committer decides it", q.judged())
}

// judged names q as one of the writes the chart judges, for a node that may
// judge none: a seat's push or write of a change, an enqueue or a merge —
// whoever asks ([QueueProposal.JudgesChart], [MergeProposal.JudgesChart]) — or
// a seat's dequeue.
func (q *queued) judged() string {
	switch {
	case q.queueWrite != nil && q.queueWrite.Enqueue != nil:
		return "enqueue"
	case q.queueWrite != nil:
		return "seat's dequeue"
	case q.merge != nil:
		return "merge"
	}
	return "seat's " + q.kind()
}

// again is what q's asker does after a refusal that decided nothing: a person
// runs git push again, or asks for a purge again, a compactor's or a
// maintainer's next pass plans the repository again from its rows, and a
// change's asker sends its write again.
func (q *queued) again() string {
	switch {
	case q.compaction != nil:
		return "the next pass plans again"
	case q.expunge != nil:
		return q.expunge.again()
	case q.remote != nil:
		return q.remote.again()
	case q.change != nil, q.queueWrite != nil, q.merge != nil:
		return changeAgain
	}
	return "run git push again"
}

// take hands q to the batch about to start, reporting false for a push its
// caller withdrew. A push already taken is the batch's still: a carry that
// did not fit the last record, leading the next.
func (q *queued) take() bool {
	return q.state.CompareAndSwap(pushWaiting, pushTaken) || q.state.Load() == pushTaken
}

// withdraw takes q out of the queue undecided, reporting false for a push a
// batch has already taken — which that batch answers.
func (q *queued) withdraw() bool { return q.state.CompareAndSwap(pushWaiting, pushWithdrawn) }

// late is why q may not start a batch at now, or "" while it may: no more of
// its asker's attempt left than a batch and its answer may take — or, for a
// caller with no deadline, a whole CommitBudget already waited.
func (q *queued) late(now time.Time) string {
	if !q.deadline.IsZero() {
		if left := q.deadline.Sub(now); left <= CommitBudget+ReplyMargin {
			return q.shortOfTime(left)
		}
		return ""
	}
	waited := now.Sub(q.arrived)
	if waited < CommitBudget {
		return ""
	}
	return fmt.Sprintf("the %s waited %s behind this repository's earlier pushes on this node, "+
		"and a %s waits at most %s before its own record is written; %s", q.kind(),
		waited.Round(time.Millisecond), q.kind(), CommitBudget, q.again())
}

// shortOfTime is the refusal of a proposal whose asker stops waiting in left —
// no more than a batch and its answer may take, which is the only time it is
// formed.
func (q *queued) shortOfTime(left time.Duration) string {
	left = min(max(left, 0), CommitBudget+ReplyMargin)
	return fmt.Sprintf("the %s's asker stops waiting in %s, and a batch started now may take %s "+
		"and its answer %s more; %s", q.kind(), left.Round(time.Millisecond), CommitBudget,
		ReplyMargin, q.again())
}

// answered is one proposal's answer: res a push's, compact a compaction's,
// expunge an expunge record's, change a change's, queue a queue's write's,
// merge a merge's — or carried, a push its batch decided nothing of, which
// leads the next batch and is answered there.
type answered struct {
	res     ProposeResult
	compact CompactResult
	expunge ExpungeResult
	remote  SyncResult
	change  ChangeResult
	queue   QueueResult
	merge   MergeResult
	err     error
	carried bool
}

// NewCommitter builds a committer, refusing what it cannot work without, and
// starts its announcer when it announces on an event stream (announce.go),
// which [Committer.Stop] ends.
func NewCommitter(o CommitterOptions) (*Committer, error) {
	return newCommitter(o, CommitterAnnouncements)
}

// newCommitter is [NewCommitter] with an announcer holding at most
// announcements unpublished records, a test's smaller value
// (export_test.go).
func newCommitter(o CommitterOptions, announcements int) (*Committer, error) {
	switch {
	case o.Publisher == nil:
		return nil, errors.New("gitlet: a committer needs the code log's write authority — " +
			"every push is a record on that log")
	case o.DB.IsZero():
		return nil, errors.New("gitlet: a committer needs the replicated estate a batch's " +
			"answers are read from — an ok is only ever reported from a push's row")
	case !config.ValidNodeID(o.NodeID):
		return nil, fmt.Errorf("gitlet: a committer needs the deciding node's id, which every "+
			"record names as its actor; %q is not one config admits", o.NodeID)
	case o.Limits == nil:
		return nil, errors.New("gitlet: a committer needs the epoch's code.limits — what a " +
			"push's pack is judged against is a decision input, never a default")
	case o.Retention == nil:
		return nil, errors.New("gitlet: a committer needs the epoch's code.deleted_retention_days — " +
			"a push to a deleted repository is told when its purge may begin, which a default " +
			"would misstate for every company that set its own")
	case o.SeatRules == nil:
		return nil, errors.New("gitlet: a committer needs the epoch's chart and calendar (SeatRules) — " +
			"a seat's push is judged by its grant on the chart and counted on the company's day, " +
			"never by a default")
	case o.Events != nil && o.AgentIDs == nil:
		return nil, errors.New("gitlet: a committer that announces on an event stream needs AgentIDs — an " +
			"opened change is counted on its owner seat's day by the seat's agent id, which only the chart says")
	}
	c := &Committer{
		pub: o.Publisher, db: o.DB, nodeID: o.NodeID, limits: o.Limits, retention: o.Retention,
		seatRules: o.SeatRules, current: o.Current, now: o.Now, events: o.Events, agentIDs: o.AgentIDs,
		metrics: o.Metrics, log: o.Logger,
		idle: CommitterIdle, nodeBytes: CommitterNodeBytes, nodeProposals: CommitterNodeProposals,
		queueEntries: MaxQueueEntries, clock: time.Now,
		repos: map[string]*repoCommitter{}, stopping: make(chan struct{}),
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	if c.log == nil {
		c.log = logging.Get("gitlet")
	}
	c.life, c.end = context.WithCancel(context.Background())
	c.endAnnouncements = func() {}
	if c.events != nil {
		c.announcements = make(chan announcement, announcements)
		c.announcerDone = make(chan struct{})
		c.endAnnouncements = sync.OnceFunc(func() { close(c.announcements) })
		go c.announcer(c.life)
	}
	return c, nil
}

// Propose decides one push in the next batch of its repository and answers
// what became of it. It answers busy, undecided: at once for a full queue or a
// node at [CommitterNodeProposals] or [CommitterNodeBytes]; at once or while
// it waits once its caller's deadline leaves less than [CommitBudget] and
// [ReplyMargin]; and once a push with no deadline has waited a CommitBudget.
// A stopping committer, or one that cannot judge a seat's push, answers
// [ErrNotServing]; a proposal that is not one, [ErrInvalidProposal].
// ctx bounds only this caller's wait: a push whose caller has gone before its
// batch starts is never decided, and the batch it joined goes on without it.
func (c *Committer) Propose(ctx context.Context, p Proposal) (ProposeResult, error) {
	bytes, err := p.measure()
	if err != nil {
		return ProposeResult{}, err
	}
	deadline, _ := ctx.Deadline()
	a := c.queue(ctx, &queued{p: p, bytes: bytes, rows: rowBound(p), arrived: c.clock(),
		deadline: deadline, gone: ctx.Done(), answer: make(chan answered, 1)})
	return a.res, a.err
}

// queue puts q on its repository's queue and waits for its answer — the one
// path every proposal's caller takes, a push's and a compaction's alike.
func (c *Committer) queue(ctx context.Context, q *queued) answered {
	if why := c.behindFor(q); why != "" {
		// A REFUSAL ABOUT THIS NODE, before the push takes a place in the
		// queue: the router hands it to the repository's runner-up.
		return answered{err: fmt.Errorf("%w: %s", ErrNotServing, why)}
	}
	if refusal := q.late(q.arrived); refusal != "" {
		// TOO LATE BEFORE IT IS QUEUED, so it takes no place in the queue:
		// filed busyLate, the one time-bound refusal that is no backlog of
		// this committer's (busy.go).
		return c.busy(q, busyLate, refusal)
	}
	//nolint:contextcheck // The batch is the committer's, not this caller's: it runs under the committer's own lifetime so a caller that hangs up never cancels the batch other callers are in.
	if refused, full := c.enqueue(q); full != "" {
		return c.busy(q, full, refused)
	} else if refused != "" {
		return answered{err: fmt.Errorf("%w: %s", ErrNotServing, refused)}
	}
	// HELD UNTIL THIS CALLER HAS ITS ANSWER, whichever way it leaves — the
	// one place every queued proposal's caller passes through.
	defer c.release(q)
	return c.await(ctx, q)
}

// withdrawal arms a queued push's withdrawal to fire after d: the suite's
// withdrawAfter where a case set it, and otherwise a timer, stopped by its
// stop. A NIL HOOK IS PRODUCTION'S TIMER, so no way of building a Committer
// leaves a queued push nothing to withdraw it by.
func (c *Committer) withdrawal(d time.Duration) (<-chan time.Time, func()) {
	if c.withdrawAfter != nil {
		return c.withdrawAfter(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// await is a queued push's wait for its answer, on its caller's goroutine,
// which watches the deadline WHILE THE PUSH WAITS: the repository's goroutine
// sees its queue only between batches, so a push behind long batches would
// reach the head after its asker had read the silence as a dead committer.
//
// AN ANSWER ALREADY GIVEN WINS over the caller's context ending: Stop answers
// every queued push [ErrNotServing], then the engine withdraws the estate's
// serving registration, cancelling every answer in flight, so both are ready
// and a select would pick at random — turning the refusal that sends the push
// to the runner-up into a final `context canceled`, or into no answer at all.
func (c *Committer) await(ctx context.Context, q *queued) answered {
	var expire <-chan time.Time
	if !q.deadline.IsZero() {
		fire, stop := c.withdrawal(q.deadline.Sub(q.arrived) - CommitBudget - ReplyMargin)
		defer stop()
		expire = fire
	}
	for {
		select {
		case a := <-q.answer:
			return a
		case <-expire:
			expire = nil
			if q.withdraw() {
				// THE QUEUE'S TIME BOUND: queued with time to spare, it
				// WAITED out its asker here, a backlog (busy.go).
				return c.busy(q, busyWaited, q.shortOfTime(q.deadline.Sub(c.clock())))
			}
			// A BATCH TOOK IT FIRST, with a whole budget and the margin
			// still in hand: its answer comes inside the attempt.
		case <-ctx.Done():
			select {
			case a := <-q.answer:
				return a
			default:
			}
			q.withdraw()
			return answered{err: ctx.Err()}
		}
	}
}

// busy counts q refused busy for reason and answers it so, undecided: a push
// with every command it carries refused, a compaction, an expunge record or a
// change's write refused whole.
func (c *Committer) busy(q *queued, reason busyReason, refusal string) answered {
	c.countBusy(q.repoID(), q.kind(), reason, 1)
	switch {
	case q.compaction != nil:
		return answered{compact: CompactResult{Refusal: RefusalBusy, Detail: refusal}}
	case q.expunge != nil:
		return answered{expunge: ExpungeResult{Refusal: RefusalBusy, Detail: refusal}}
	case q.remote != nil:
		return answered{remote: SyncResult{Refusal: RefusalBusy, Detail: refusal}}
	case q.change != nil:
		return answered{change: ChangeResult{ChangeID: q.change.ChangeID(), Refusal: ChangeRefusalBusy,
			Detail: refusal}}
	case q.queueWrite != nil:
		return answered{queue: QueueResult{EntryID: q.queueWrite.EntryID(), Refusal: MergeRefusalBusy,
			Detail: refusal}}
	case q.merge != nil:
		return answered{merge: MergeResult{MergeID: q.merge.Merge.MergeID, Refusal: MergeRefusalBusy,
			Detail: refusal}}
	}
	return answered{res: ProposeResult{Answer: refuseAll(q.p, RefusalBusy, refusal)}}
}

// countBusy counts n proposals of kind to repoID refused busy for reason,
// under the instrument's `kind` and `reason` so a backlog reads apart from a
// deadline or a lost race, and files them in the window code_commit_busy is
// read from, logging any minute that filing closed (busy.go).
func (c *Committer) countBusy(repoID, kind string, reason busyReason, n int) {
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCommitBusy, uint64(n), metrics.Attrs{"kind": kind, "reason": string(reason)})
	}
	c.sayBusy(c.busyWindow.file(c.clock(), repoID, reason, n))
}

// enqueue puts q on its repository's queue, starting the repository's
// goroutine when it has none, and counts it held until [Committer.release].
// It answers why it did not: a sentence, and the bound a full one met —
// [busyQueue] for the repository's count or bytes, [busyNode] for the node's
// count or bytes — or none for a committer that is stopping.
//
// THE SEND IS UNDER mu, which is what makes the idle exit race-free: a
// goroutine leaves the map only under mu and only with an empty queue, so a
// proposal either reaches a goroutine that will drain it or starts a new one.
func (c *Committer) enqueue(q *queued) (string, busyReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return "the committer is stopping", ""
	}
	// BEFORE A GOROUTINE IS STARTED for a repository this node may hold
	// nothing of.
	switch {
	case c.holding >= c.nodeProposals:
		return fmt.Sprintf("%d proposals are held across this node's repositories, the most it holds "+
			"(CommitterNodeProposals)", c.holding), busyNode
	case c.held+q.bytes > c.nodeBytes:
		return fmt.Sprintf("%d bytes of pushes are held across this node's repositories, and %d "+
			"more would pass the %d it holds", c.held, q.bytes, c.nodeBytes), busyNode
	}
	rc := c.repos[q.repoID()]
	if rc == nil {
		rc = &repoCommitter{repoID: q.repoID(), inbox: make(chan *queued, CommitterInbox)}
		c.repos[rc.repoID] = rc
		c.wg.Add(1)
		go c.run(rc)
	}
	if rc.queued+q.bytes > CommitterInboxBytes {
		return fmt.Sprintf("%d bytes of pushes are queued for this repository on this node, "+
			"and %d more would pass the %d it holds", rc.queued, q.bytes, CommitterInboxBytes), busyQueue
	}
	select {
	case rc.inbox <- q:
		rc.queued += q.bytes
		c.held += q.bytes
		c.holding++
		return "", ""
	default:
		return fmt.Sprintf("%d pushes are queued for this repository on this node, the most "+
			"it holds", len(rc.inbox)), busyQueue
	}
}

// Close refuses every new proposal [ErrNotServing] and answers every queued
// one the same, leaving the batches in flight to run — the first half of
// [Committer.Stop], which a teardown stopping several writers under one bound
// takes for all of them before it drains any (internal/engine's
// stopTogether). Safe to call more than once.
func (c *Committer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		c.stopped = true
		close(c.stopping)
	}
}

// Stop refuses every new proposal [ErrNotServing], answers every queued one
// the same — a node that decided nothing, so the router asks the runner-up —
// and waits for the batches in flight, which are FINISHED rather than
// abandoned so their callers get their outcome, and then for the announcer to
// publish what they and the batches before them landed. If ctx ends first,
// the batches are cancelled and answered from what their publish returned,
// and every announcement still held fails its publish at once and is logged
// unpublished. Safe to call more than once.
func (c *Committer) Stop(ctx context.Context) {
	c.Close()
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		// NO REPOSITORY'S GOROUTINE IS LEFT TO ANNOUNCE, and Close started
		// none since: the announcer's queue has no sender, so it is closed
		// and drained.
		c.endAnnouncements()
		if c.announcerDone != nil {
			<-c.announcerDone
		}
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	c.end()
	<-done
}

// run drains one repository's queue: a batch at a time, each the first
// proposal and everything queued behind it that fits, until the committer
// stops or the queue has been empty for the idle time.
func (c *Committer) run(rc *repoCommitter) {
	defer c.wg.Done()
	idle := time.NewTimer(c.idle)
	defer idle.Stop()
	// carried leads the next batch, in the order it arrived: the pushes the
	// last batch's decide carried, then the first proposal its gather met
	// that did not fit it — every one taken from the queue already.
	var carried []*queued
	for {
		select {
		case <-c.stopping:
			c.refuseQueued(rc, carried)
			return
		default:
		}
		var first *queued
		if len(carried) > 0 {
			first, carried = carried[0], carried[1:]
		} else {
			select {
			case first = <-rc.inbox:
				c.dequeued(rc, first)
			case <-c.stopping:
				c.refuseQueued(rc, nil)
				return
			case <-idle.C:
				c.mu.Lock()
				if len(rc.inbox) == 0 {
					delete(c.repos, rc.repoID)
					c.mu.Unlock()
					return
				}
				c.mu.Unlock()
				idle.Reset(c.idle)
				continue
			}
		}
		if !c.admit(first) {
			continue
		}
		switch {
		case first.compaction != nil:
			c.compact(rc.repoID, first)
		case first.expunge != nil:
			c.expunge(rc.repoID, first)
		case first.remote != nil:
			c.remote(rc.repoID, first)
		case first.change != nil:
			c.change(rc.repoID, first)
		case first.queueWrite != nil:
			c.queueWrite(rc.repoID, first)
		case first.merge != nil:
			c.mergeWrite(rc.repoID, first)
		default:
			var batch []*queued
			batch, carried = c.gather(rc, first, carried)
			carried = append(c.commit(rc.repoID, batch), carried...)
		}
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(c.idle)
	}
}

// gather is first — a push — and every proposal already waiting behind it that
// fits the record, the carried before the queued, WITHOUT LINGER. One
// [Committer.admit] turns away is answered and left out; the first that does
// not fit is carried to lead the next batch, and admitted again there, the
// batch it would start in. It answers the batch and what is carried still.
//
// A proposal [queued.alone] is never gathered: a record carries one op, and
// each is a record of its own. Met behind pushes, it ends the batch and is
// carried, so the repository's proposals still land in arrival order.
func (c *Committer) gather(rc *repoCommitter, first *queued, carried []*queued) ([]*queued, []*queued) {
	batch := []*queued{first}
	bytes, rows := recordOverheadBytes+first.bytes, first.rows
	for len(batch) < MaxPushesPerRecord {
		var q *queued
		if len(carried) > 0 {
			q, carried = carried[0], carried[1:]
		} else {
			select {
			case q = <-rc.inbox:
				c.dequeued(rc, q)
			default:
				return batch, nil
			}
		}
		if !c.admit(q) {
			continue
		}
		if q.alone() || bytes+q.bytes > MaxRecordBytes ||
			recordRows(rows+q.rows) > MaxRecordRows {
			return batch, append([]*queued{q}, carried...)
		}
		batch = append(batch, q)
		bytes += q.bytes
		rows += q.rows
	}
	return batch, carried
}

// admit reports whether q may be decided in the batch about to start, and
// answers it when not — judged as the batch forms, when deciding it would
// begin. It TAKES a push first ([queued.take]), so its caller can no longer
// withdraw it. A WITHDRAWN push was answered by its caller. One whose caller
// has GONE is answered to nobody: its asker may already have handed the push
// id to the runner-up, and deciding it here would contend with that node. A
// LATE one ([queued.late]) is answered busy, a backlog ([busyWaited]). A
// chart-judged one is judged again against the node's posture, which may have
// turned behind since it queued, and answered [ErrNotServing]. None of these
// appends anything.
func (c *Committer) admit(q *queued) bool {
	if !q.take() {
		return false
	}
	select {
	case <-q.gone:
		return false
	default:
	}
	if why := c.behindFor(q); why != "" {
		q.answer <- answered{err: fmt.Errorf("%w: %s", ErrNotServing, why)}
		return false
	}
	if refusal := q.late(c.clock()); refusal != "" {
		q.answer <- c.busy(q, busyWaited, refusal)
		return false
	}
	return true
}

// release takes q and its bytes off what the node holds, once its caller has
// left [Committer.Propose].
func (c *Committer) release(q *queued) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held -= q.bytes
	c.holding--
}

// dequeued takes q's bytes off rc's queue.
func (c *Committer) dequeued(rc *repoCommitter, q *queued) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rc.queued -= q.bytes
}

// Queued is how many proposals repoID's queue on this node holds now: sent and
// not yet taken off by its goroutine, so a batch in flight holds none and a
// withdrawn proposal counts until the goroutine meets it.
//
// THE ONE READING OF A QUEUE a case waits on before letting a held batch go
// (the committer's suite, internal/gitlet/gitlettest's F19): the next batch
// takes only what is queued when it starts, so one let go early would split
// its followers, and a wait of any length would only make that unlikely.
func (c *Committer) Queued(repoID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rc := c.repos[repoID]; rc != nil {
		return len(rc.inbox)
	}
	return 0
}

// refuseQueued answers what is carried and everything still queued
// [ErrNotServing], and forgets the repository. Nothing can be queued after it:
// a proposal is sent under mu only while the committer is not stopped, and
// this runs only once Stop has said it is.
func (c *Committer) refuseQueued(rc *repoCommitter, carried []*queued) {
	refuse := func(q *queued) {
		q.answer <- answered{err: fmt.Errorf("%w: the committer stopped before deciding the %s",
			ErrNotServing, q.kind())}
	}
	for _, q := range carried {
		refuse(q)
	}
	for {
		select {
		case q := <-rc.inbox:
			refuse(q)
		default:
			c.mu.Lock()
			delete(c.repos, rc.repoID)
			c.mu.Unlock()
			return
		}
	}
}

// round is one round's decision, which the answers are formed from.
type round struct {
	entries []PushEntry
	answers []decided
	// ejected is the changes whose live queue entry the round's record ejects
	// by moving their head ([batchMoves]).
	ejected []string
}

// commit publishes one batch as ONE record and answers every proposal in it —
// but the pushes its decide CARRIED, decided by none of its rounds, which it
// answers the caller instead, in the order they arrived, to lead the next
// batch (decide.go). Only a publish that answered carries one: a batch whose
// publish failed answers every proposal in it the failure, the carried among
// them, since what failed may be a round's record that held them.
func (c *Committer) commit(repoID string, batch []*queued) []*queued {
	// THE EPOCH'S CHART AND CALENDAR, READ ONCE A BATCH that carries a seat's
	// push — every round of the batch judges by the same — and on a node
	// that holds none yet, every such push handed on, decided by nobody,
	// while the batch's persons' pushes are decided as ever.
	var seats SeatRules
	if slices.ContainsFunc(batch, (*queued).judgedByChart) {
		var held bool
		if seats, held = c.seatRules(); !held {
			batch = slices.DeleteFunc(batch, func(q *queued) bool {
				if !q.judgedByChart() {
					return false
				}
				q.answer <- answered{err: fmt.Errorf("%w: this node holds no chart yet to judge a seat's "+
					"push or a change's mentions by; the repository's next committer decides it", ErrNotServing)}
				return true
			})
			if len(batch) == 0 {
				return nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(c.life, CommitBudget)
	defer cancel()
	proposals := make([]Proposal, len(batch))
	// THE BATCH IS DECIDED AT OR PAST WHAT EVERY PUSH IN IT WAS CHECKED
	// AGAINST ([Proposal.Seen]; the file's head says why). Validate held
	// every Seen to the code log, so they compare.
	var seen statelog.Position
	for i, q := range batch {
		proposals[i] = q.p
		if q.p.Seen.Packed() > seen.Packed() {
			seen = q.p.Seen
		}
	}
	// THE OPERATION ID IS MINTED ONCE A BATCH, stable across its rounds, by
	// the wall clock: the ledger judges what an id's mint instant vouches for
	// against its own sweeps, never against the decide's clock.
	opID := statelog.NewOpID(time.Now(), "code_push")
	limits, retention := c.limits(), c.retention()
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	var last round
	// THE BATCH'S PROTECTION VERDICTS, FORMED BEFORE THE PUBLISH — CPU alone,
	// holding no connection of the reader pool — and shared by its rounds
	// while the rules each round's snapshot reads are the ones they were
	// formed under (decide.go).
	var verdicts verdicts
	verdicts.formBefore(ctx, c.db, repoID, proposals)
	res, err := c.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated, Session: seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET AT THE TOP OF EVERY ROUND: only the last round says
			// what the publish decided.
			last = round{}
			now := c.now()
			formed := verdicts.formed
			entries, answers, moves, version, err := decideBatch(ctx, tx, repoID, proposals, limits,
				retention, seats, now, &verdicts)
			if c.formedInRound != nil {
				c.formedInRound(verdicts.formed - formed)
			}
			if err != nil {
				return statelog.Decision{}, err
			}
			last = round{entries: entries, answers: answers, ejected: moves.ejected}
			if len(entries) == 0 {
				return statelog.Decision{Version: version}, nil
			}
			decision, err := recordFor(stamp, subject, OpPush, scope, opID,
				Actor{ID: c.nodeID, Kind: ActorSystem}, PushBatch{RepoID: repoID, Pushes: entries}, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := withinRecord(decision, entries, moves.rows); err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = version
			return decision, nil
		},
	})
	answers := c.answers(ctx, repoID, proposals, last, res, err, opID)
	var carried []*queued
	for i, q := range batch {
		if answers[i].carried {
			carried = append(carried, q)
			continue
		}
		q.answer <- answers[i]
	}
	// WHAT THE BATCH LANDED IS HANDED TO THE ANNOUNCER, never published
	// here, where the repository's next batch would wait on it (announce.go).
	c.announceOpenings(repoID, pushOpenings(answers))
	// A QUEUED CHANGE WHOSE HEAD THE RECORD MOVED left its queue in the
	// record's own apply, which no queue record names: announced once, from
	// this node, once the record applied here — never for a record the
	// ledger collapsed this batch onto, whose own batch announced it.
	if err == nil && res.Outcome == statelog.OutcomeApplied && !res.Position.IsZero() && !res.Collapsed &&
		len(last.ejected) > 0 {
		c.announcePushEjections(repoID, last.ejected, res.Position)
	}
	return carried
}

// pushOpenings is every change a batch's pushes opened, each once: a push
// answered applied FROM THIS BATCH'S OWN ROW whose change says it opened —
// never one the batch found landed before it (Collapsed), whose own batch
// announced it, and never a pending or unknown push's, which may yet apply
// nowhere.
func pushOpenings(answers []answered) []opening {
	var out []opening
	for _, a := range answers {
		r := a.res
		if a.carried || a.err != nil || r.Outcome != statelog.OutcomeApplied || r.Answer.Collapsed ||
			r.Answer.Change == nil || !r.Answer.Change.Opened {
			continue
		}
		if !slices.ContainsFunc(out, func(o opening) bool { return o.changeID == r.Answer.Change.ChangeID }) {
			out = append(out, opening{changeID: r.Answer.Change.ChangeID, byPush: true})
		}
	}
	return out
}

// withinRecord refuses a decision past a record's bytes or rows, which the
// batching and the decide's admission of each entry keep from ever forming —
// the check makes the invariant the record's own rather than the batcher's.
// Rows are counted as the decide admitted them: each entry's [entryRows] and
// the notices its routings make the apply write ([noticeRows]), and
// changeRows, what its apply writes about the live changes the entries move
// ([changeBook]).
func withinRecord(d statelog.Decision, entries []PushEntry, changeRows int) error {
	rows := changeRows
	for _, e := range entries {
		rows += entryRows(e) + noticeRows(e.Routing)
	}
	switch {
	case len(d.Payload) > MaxRecordBytes:
		return fmt.Errorf("%w: %d bytes, past MaxRecordBytes (%d)", ErrRecordCap, len(d.Payload),
			MaxRecordBytes)
	case recordRows(rows) > MaxRecordRows:
		return fmt.Errorf("%w: up to %d rows, past MaxRecordRows (%d)", ErrRecordCap,
			recordRows(rows), MaxRecordRows)
	}
	return nil
}

// answers is every proposal's answer from the publish's result, the push rows
// a record applied and the last round's decision.
//
// The last round to decide is not always the one whose record landed: after a
// lost acknowledgement another round decides again, perhaps differently (the
// clock, a peer's write), and the publisher may find the first in its ledger
// and answer it, collapsed. So once a record applied, a proposal whose push
// has a row is answered FROM THAT ROW, whichever round wrote it, and the
// decision answers only one with none.
func (c *Committer) answers(ctx context.Context, repoID string, batch []Proposal, last round,
	res statelog.Result, err error, opID string) []answered {

	out := make([]answered, len(batch))
	if err != nil {
		c.refused(ctx, repoID, batch, out, err, opID)
		return out
	}
	if len(last.answers) != len(batch) {
		// A PUBLISH THAT ANSWERED WITHOUT DECIDING breaks the publisher's
		// contract; every proposal is told so rather than answered from
		// nothing.
		failed := fmt.Errorf("gitlet: the publish of operation %s answered %s without deciding "+
			"the batch", opID, res.Outcome)
		for i := range out {
			out[i].err = failed
		}
		return out
	}
	var (
		rows map[string]PushRow
		// numbers is the number of every change a proposal's answer may
		// name — the one its push would have opened, and the live one the
		// decision says it named — that the repository holds.
		numbers map[string]int64
	)
	if res.Outcome == statelog.OutcomeApplied && !res.Position.IsZero() {
		ids := make([]string, len(batch))
		for i, p := range batch {
			ids[i] = p.PushID
		}
		readErr := c.db.Read(ctx, func(tx *sql.Tx) error {
			var err error
			if rows, err = pushRowsIn(ctx, tx, ids); err != nil {
				return err
			}
			numbers, err = changeNumbersIn(ctx, tx, repoID, answeredChanges(batch, last.answers, false))
			return err
		})
		if readErr != nil {
			failed := fmt.Errorf("gitlet: the record of operation %s applied at %s and its push rows "+
				"could not be read: %w", opID, res.Position, readErr)
			c.log.WarnContext(ctx, "code_commit_failed", "kind", kindPush, "repo", repoID, "op_id", opID,
				"error", failed)
			for i := range out {
				out[i].err = failed
			}
			return out
		}
	} else if ids := answeredChanges(batch, last.answers, true); len(ids) > 0 {
		// PUSHES THAT LANDED BEFORE THE BATCH and asked for a change, whose
		// change no round of it decided: the one each opened, if it did,
		// read by its id. Its read failing leaves them answered with no
		// word of their change, which is all it costs.
		if readErr := c.db.Read(ctx, func(tx *sql.Tx) error {
			var err error
			numbers, err = changeNumbersIn(ctx, tx, repoID, ids)
			return err
		}); readErr != nil {
			c.log.WarnContext(ctx, "code_change_unanswered", "repo", repoID, "op_id", opID, "error", readErr)
		}
	}
	c.observeBatch(last, res, rows)
	for i, p := range batch {
		d := last.answers[i]
		if d.dupOf >= 0 {
			out[i] = out[d.dupOf]
			continue
		}
		out[i] = c.answer(repoID, p, d, res, rows, numbers, opID)
	}
	return out
}

// answeredChanges is the id of every change the answers of batch may name: for
// each proposal that asked for a change, the one its push opens
// ([PushChangeID]) and the live one its decision says it named — or, collapsed
// alone, only those of the proposals that landed before the batch, the one
// thing their answer can say of a change no round of it decided.
func answeredChanges(batch []Proposal, answers []decided, collapsed bool) []string {
	var ids []string
	for i, p := range batch {
		d := answers[i]
		if p.Change == nil || d.dupOf >= 0 || (collapsed && d.collapsed == nil) {
			continue
		}
		ids = append(ids, PushChangeID(p.PushID))
		if d.change != nil && d.change.ChangeID != "" && !d.change.Opened {
			ids = append(ids, d.change.ChangeID)
		}
	}
	return ids
}

// changeNumbersIn is the number of each change ids names that is a change of
// repoID, by its id — one statement over the changes' key, driven from the
// list.
func changeNumbersIn(ctx context.Context, tx *sql.Tx, repoID string, ids []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, changeNumbersSQL, repoID, list)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the numbers of repository %s's changes: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var number int64
		if err := rows.Scan(&id, &number); err != nil {
			return nil, fmt.Errorf("gitlet: read the numbers of repository %s's changes: %w", repoID, err)
		}
		out[id] = number
	}
	return out, rows.Err()
}

// changeNumbersSQL is each change of a repository its list names, by id: one
// seek of the changes' key an element, driven from the list.
const changeNumbersSQL = `SELECT c.id, c.number FROM json_each(?2) AS j CROSS JOIN code_changes AS c
	WHERE c.id = CAST(j.value AS TEXT) AND c.repo_id = ?1`

// landedChange is what p's change came to, from the rows a record applied
// (numbers): the change its push opened, where the repository holds one of its
// id — the one fact the rows hold, whichever round of the batch decided the
// record that landed — and otherwise what the decision says, the live change
// it named with that change's number from its row, or why it opened none. Nil
// where p asked for no change; where the decision says it opened one the rows
// do not hold, a decision of another round than the record's; and where there
// is no decision to say it — a push that landed in an earlier record, whose
// answer of its change went with that record's.
func landedChange(p Proposal, decision *ChangeAnswer, numbers map[string]int64) *ChangeAnswer {
	if p.Change == nil {
		return nil
	}
	id := PushChangeID(p.PushID)
	if n, ok := numbers[id]; ok {
		return &ChangeAnswer{ChangeID: id, Number: n, Opened: true}
	}
	switch {
	case decision == nil, decision.Opened:
		return nil
	case decision.Refusal != "":
		refused := *decision
		return &refused
	}
	n, ok := numbers[decision.ChangeID]
	if !ok {
		return nil
	}
	return &ChangeAnswer{ChangeID: decision.ChangeID, Number: n}
}

// observeBatch records how many pushes the batch's record carried: the rows
// the applied record wrote — the record that landed, whichever round decided
// it — and otherwise the entries of the decision that was published.
func (c *Committer) observeBatch(last round, res statelog.Result, rows map[string]PushRow) {
	if c.metrics == nil {
		return
	}
	pushes := len(last.entries)
	if rows != nil {
		pushes = 0
		for _, row := range rows {
			if row.Position == res.Position {
				pushes++
			}
		}
	}
	if pushes > 0 {
		c.metrics.ObserveValue(metrics.CodeCommitBatchSize, float64(pushes), nil)
	}
}

// answer is one proposal's answer from its row, its decision and the
// publish's result — and its change's, from the numbers of the changes the
// repository holds that its answer may name (numbers, [landedChange]).
func (c *Committer) answer(repoID string, p Proposal, d decided, res statelog.Result,
	rows map[string]PushRow, numbers map[string]int64, opID string) answered {

	if d.err != nil {
		return answered{err: d.err}
	}
	if row, ok := rows[p.PushID]; ok {
		if row.RepoID != repoID {
			return answered{err: refusedf("%w: push_id: %s is a push to repository %s, not %s",
				ErrInvalidProposal, p.PushID, row.RepoID, repoID)}
		}
		// THE RECORD'S OWN ROW, or one an earlier record applied: the push
		// is answered from it either way, and collapsed when it is not this
		// record's — whose change no round of this batch decided either.
		ours := row.Position == res.Position
		decision := d.change
		if !ours {
			decision = nil
		}
		answer := ProposeResult{
			Outcome: statelog.OutcomeApplied, Position: row.Position,
			Answer: PushAnswer{
				PushID: p.PushID, Commands: withDetails(row.Commands, d.commands), Collapsed: !ours,
				Change: landedChange(p, decision, numbers),
			},
		}
		if ours {
			answer.OpID = opID
		}
		return answered{res: answer}
	}
	if d.collapsed != nil {
		// LANDED BEFORE THE BATCH, read in the decide's own snapshot — the
		// answer when no record applied and so no rows were read again.
		return answered{res: ProposeResult{
			Outcome: statelog.OutcomeApplied, Position: d.collapsed.Position,
			Answer: PushAnswer{PushID: p.PushID, Commands: d.collapsed.Commands, Collapsed: true,
				Change: landedChange(p, nil, numbers)},
		}}
	}
	if d.carried {
		// DECIDED BY NO ROUND, and in no record: it leads the next batch,
		// which answers it ([Committer.commit]).
		return answered{carried: true}
	}
	if !d.inRecord {
		// ANSWERED WITH NO RECORD: every command noop or ng and no attempt
		// ([Attempted]), or the whole push refused.
		return answered{res: ProposeResult{Answer: PushAnswer{
			PushID: p.PushID, Commands: d.commands, Refused: d.refused, Detail: d.detail,
		}}}
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		return answered{err: fmt.Errorf("gitlet: push %s was in the record of operation %s "+
			"applied at %s and has no row — an ok is only ever reported from a row",
			p.PushID, opID, res.Position)}
	case statelog.OutcomePending:
		return answered{res: ProposeResult{
			Outcome: statelog.OutcomePending, Position: res.Position, OpID: opID,
			Answer: PushAnswer{PushID: p.PushID, Commands: d.commands, Change: d.change},
		}}
	}
	return answered{res: ProposeResult{
		Outcome: statelog.OutcomeUnknown, OpID: opID,
		Answer: PushAnswer{PushID: p.PushID, Commands: d.commands, Change: d.change},
	}}
}

// withDetails is a row's commands with the decision's Detail on each refused
// one the decision refused for the SAME reason, matched by ref — the row
// stores no sentence, and the client is owed the one the decide formed, but
// only the one about the refusal the row holds: the decision may be another
// round's.
func withDetails(stored, decided []Command) []Command {
	type said struct {
		reason Refusal
		detail string
	}
	details := make(map[string]said, len(decided))
	for _, c := range decided {
		if c.Result == ResultNG && c.Detail != "" {
			details[c.Ref] = said{c.Reason, c.Detail}
		}
	}
	out := make([]Command, len(stored))
	for i, c := range stored {
		if s, ok := details[c.Ref]; ok && c.Result == ResultNG && c.Reason == s.reason {
			c.Detail = s.detail
		}
		out[i] = c
	}
	return out
}

// nodeRefusals are the code log's refusals about THIS NODE, which another
// data node can serve: answered [ErrNotServing] — whatever position the
// refusal names, which for each of them is something other than a record of
// this push — so the router asks the runner-up under the same push id. An
// eviction is here as a refusal before the append and as one found at another
// operation's record; one that dropped the batch's own record is not_applied,
// decided before this set is asked.
var nodeRefusals = map[statelog.Reason]bool{
	statelog.ReasonEvicted: true, statelog.ReasonEvictionUnknown: true, statelog.ReasonDeferred: true,
	statelog.ReasonBehind: true, statelog.ReasonBelowFloor: true, statelog.ReasonFloorUnknown: true,
	statelog.ReasonUsageUnknown: true, statelog.ReasonWrongStream: true, statelog.ReasonLogTruncated: true,
	statelog.ReasonSkew: true,
}

// refused answers every proposal of a batch whose publish failed.
func (c *Committer) refused(ctx context.Context, repoID string, batch []Proposal, out []answered,
	err error, opID string) {

	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, statelog.ErrConflict):
		c.countBusy(repoID, kindPush, busyContended, len(batch))
		for i, p := range batch {
			out[i].res.Answer = refuseAll(p, RefusalBusy, "the repository kept changing for every "+
				"round the write may take; run git push again")
		}
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		// A FACT ABOUT THE REPOSITORY, the same for every node: its
		// deletion marker drops every record about it. The position is
		// named only where the marker dropped the BATCH'S OWN record; one
		// found at another operation's record names where the gate was
		// judged, which is no record of the push.
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		detail := c.purgedDetail(ctx, repoID, opID)
		for i, p := range batch {
			out[i].res = ProposeResult{Position: at, OpID: opID,
				Answer: refuseAll(p, RefusalRepoDeleted, detail)}
		}
	case errors.As(err, &refusal) && refusal.Landed():
		// THE BATCH'S RECORD LANDED AND APPLIES NOWHERE: a gate drops it on
		// every node. Judged by the refusal's kind and whose record it names
		// (statelog.Unavailable.Landed), never by a position alone: a refusal
		// before the append names one too, and so does an eviction found at
		// ANOTHER operation's record — both pushes the runner-up can commit.
		detail := fmt.Sprintf("the push applies nowhere: its record at %s was durable and "+
			"the code log's %s gate drops it on every node; run git push again",
			refusal.Position, refusal.Reason)
		for i, p := range batch {
			out[i].res = ProposeResult{Position: refusal.Position, OpID: opID,
				Answer: refuseAll(p, RefusalNotApplied, detail)}
		}
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		for i := range out {
			out[i].err = fmt.Errorf("%w: %w", ErrNotServing, refusal)
		}
	case errors.As(err, &refusal):
		detail := "the code log refused the write: " + strings.TrimPrefix(refusal.Error(), "statelog: ")
		for i, p := range batch {
			out[i].res.Answer = refuseAll(p, RefusalUnavailable, detail)
		}
	default:
		failed := fmt.Errorf("gitlet: the batch of operation %s: %w", opID, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", kindPush, "repo", repoID, "op_id", opID,
			"error", err)
		for i := range out {
			out[i].err = failed
		}
	}
}

// purgedDetail is the sentence of a batch the repository's deletion marker
// refused: [RepoGone]'s purged state NAMED BY THE MARKER, which holds the name
// for ever, read over its primary key as the decide reads it (deletionIn), so
// one state is one sentence whichever path refused it; named by the id, a
// client would be told of a repository it never typed. The id names it only
// where the read cannot (no marker row since the estate was replaced, or a
// failed read), which leaves the refusal what it is: the marker answered it.
func (c *Committer) purgedDetail(ctx context.Context, repoID, opID string) string {
	return purgedDetail(ctx, c.db, c.log, repoID, opID)
}

// purgedDetail is [Committer.purgedDetail] over db, logging an unread marker to
// log — the ONE reading of a purged repository's refusal, which the committer
// and the change writer ([ChangeWriter]) both answer with, since one deletion
// marker drops every record about the repository, on its own subject and on its
// changes' alike.
func purgedDetail(ctx context.Context, db store.ReplicatedReader, log *slog.Logger, repoID, opID string) string {
	gone := RepoGone{ID: repoID, Purged: true}
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		name, _, err := deletionIn(ctx, tx, repoID)
		gone.Name = name
		return err
	}); err != nil {
		log.WarnContext(ctx, "code_deletion_marker_unread", "repo", repoID, "op_id", opID,
			"error", err)
	}
	return gone.Detail()
}
