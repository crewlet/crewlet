package code

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

// THE COMMITTER: GROUP COMMIT of one repository's pushes.
//
// Every push to a repository is a record on ONE subject, so two pushes
// committed one record each contend at the broker and pay an append, a
// compare-and-set round and an apply apiece — and the apply holds the
// replicated estate's one write lane on every data node. So a committer
// takes a repository's pushes IN ORDER on one goroutine, and each record
// carries every push that was waiting when the last one finished: the first
// push alone, and then — WITHOUT LINGER, only what is already queued — up to
// MaxPushesPerRecord pushes, MaxRecordBytes of entries and MaxRecordRows of
// writes. An idle repository commits a record a push and waits for nobody;
// a busy one amortises the append over everything that arrived during the
// last one, which is when amortising pays (doc.go's measured section: a
// group commit of sixty-four small pushes holds the lane 0.16–0.18 ms a push
// against 3.4–4.8 ms for one alone).
//
// # A push whose changes do not fit leads the next record
//
// A batch is gathered by what each push may write of its own — its bytes and
// its rows, bounded before any record exists — but a push record also writes
// the rows of the live changes its pushes move (revisions.go), which only a
// snapshot can count. So the decide counts them as it admits each push, and
// one whose changes' rows would take the record past MaxRecordRows beside the
// pushes before it is CARRIED rather than refused: nothing of it or of the
// pushes behind it is decided, and they lead the next batch in the order they
// arrived, where its rows are the record's first. Only a push whose changes'
// rows would not fit even a record of its own is refused, too_many_changes.
//
// # An ok is only ever reported from a row
//
// A batch's record is decided inside the publisher's snapshot and is applied
// on this node before the publish answers applied. Its answers are then READ
// FROM THE PUSH ROWS — one read for the batch — never from the decision: a
// record a gate dropped, or a decision that lost a round to a peer, writes
// no row, and reporting the decision would tell a client ok for a ref that
// never moved. A push in the record with no row is an error, not an ok; and a
// push WITH a row is answered from it whichever round of the batch decided
// the record that wrote it, since the last round to decide is not always the
// one that landed ([Committer.answers]).
//
// # The batch belongs to nobody who asked
//
// A batch is every caller's in it and none of theirs, so it runs under the
// committer's own lifetime — a caller that hangs up stops waiting, and the
// batch the other callers are in goes on (the push may land: its answer is
// then the row, which the client's next push collapses onto).
//
// # Answered inside the one attempt its asker gives it
//
// The estate router gives the node it asks ONE write attempt and carries its
// end to the serving node as the proposal's deadline; past it the asker hands
// the same push id to the runner-up — whose committer then contends with this
// one on the repository's subject. So a push is decided here only while its
// answer fits that attempt: a batch takes at most a CommitBudget and its
// answer ReplyMargin more, and a push starts a batch only while both are left
// before its deadline. It is judged against the DEADLINE ITSELF, never a wait
// counted from when it arrived, because what the asker spent before the push
// reached this queue — the request's trip, a wait for an answerer, the
// serving node's wait for the asker's floor — is the asker's attempt too, and
// a rule counted from arrival answered up to that much after the asker had
// stopped waiting. And it is judged WHILE THE PUSH WAITS, not only as a batch
// forms: the caller's own goroutine withdraws its push the moment too little
// is left, so a push behind however many slow batches is answered busy inside
// the attempt rather than left for an asker that has given up. A push is
// either TAKEN into a batch or WITHDRAWN from the queue, never both
// ([queued.state]): one taken is answered within its batch's budget, which it
// had in hand when it was taken; one withdrawn is decided by nobody and
// appended by nobody. A caller with NO deadline — the in-process path, where
// nobody asks a runner-up while it runs — may wait in the queue for less than
// one CommitBudget. One whose caller has already gone is dropped as its batch
// would start, because its asker may have handed it on and nobody here is
// waiting for it; a push already in a batch keeps the rule above: the batch
// goes on without its caller.
//
// # Decided at or past what its pushes were checked against
//
// A push's commands were formed against the state its receiving node read — a
// linearizable sync before receive-pack ran — and the committer is usually
// another node, which may be behind it: one that has just become the
// repository's committer, or one a few records short of a repository an
// activation has just created. Decided from below it, the push would be told
// a definite refusal that is false — a command whose old value the client was
// shown refused stale, the repository refused deleted — and told it again
// after the client fetched, until this node caught up; and a decision that
// moves nothing is never put to the broker, so no compare-and-set would catch
// it. So a batch is published with the highest of its pushes' [Proposal.Seen]
// positions as its session: decided from a snapshot that holds them, or —
// when this node cannot reach them within the publisher's budget — refused
// behind, which is a refusal about this node, so the runner-up decides it. A
// per-batch floor, never one this node keeps: it is somebody else's writes.
//
// # A refusal before the append moves the push to another node
//
// Refusals about THIS node made before anything was appended — evicted,
// behind, a record it cannot decode, a floor it is below — are answered
// [ErrNotServing]: the router asks the runner-up with the same push id, which
// can commit it. So is an eviction found at ANOTHER operation's record: the
// batch's answer was lost, the newest record on the repository's subject was
// somebody else's, and this node's eviction is below it — a refusal about this
// node, whose own append may never have landed and applies nowhere if it did,
// so the push id has a row on no node and the runner-up commits it. A record
// of THIS BATCH that landed and a gate drops on every node is not_applied,
// naming where it landed; a repository that carries a deletion marker is
// repo_deleted, naming the batch's record only where the marker dropped one;
// anything else the code log refuses for every node alike is unavailable, and
// the client runs git push again. Which is which is the refusal's KIND and
// whether the record it names is the batch's own (statelog.Unavailable.Landed),
// never whether it names a position: behind, deferred and skew all name one,
// and so does an eviction found at another operation's record, and none of
// them is a record of the push.
//
// # A seat's push is decided only by a node that can judge it
//
// A seat's push is judged from the chart and the calendar of the deciding
// node's epoch ([SeatRules]): its grant, the protections that take a person's
// push alone, its share of a repository outside its unit and its company
// day's quota (decide.go). So a node that cannot judge one decides none, and
// says so about ITSELF — [ErrNotServing], under which the router asks the
// repository's runner-up with the same push id — in two states: confirmed
// behind the company's configuration ([CommitterOptions.Current]: checked as
// the push is queued and again as its batch forms, since the revision it
// failed to apply may be the one that revoked the seat), and holding no chart
// yet ([CommitterOptions.SeatRules], read once a batch). A person's push reads
// no chart and is decided in both, and a batch of both kinds decides the
// persons' and hands the seats' on. Never a seat's push judged by a nil chart:
// let through, it would land unjudged; refused not_permitted, it would record
// a refusal no chart made.
//
// # A compaction waits in the same queue, and is decided alone
//
// A repository's COMPACTION — proposed by the git node that built one
// ([Committer.ProposeCompaction]) — waits in the repository's ONE queue beside
// its pushes, in the order they arrived, held to its bounds and the node's like
// any proposal and to the same deadline rules, and is decided as a batch of its
// own: one record of its own op, whose operation id is the compaction's own
// id, so a compaction asked again of the runner-up after a committer went
// silent is the same compaction, collapsed by the ledger. Its applied answer
// is read from the rows too — the outputs' rows the record wrote — never from
// the decision. A committer runs ONE writer a repository, so a compaction and
// the pushes queued around it can never contend on the repository's subject
// from the same node. An EXPUNGE'S records — a person's request, and the
// begin and the commit its maintainer builds ([Committer.ProposeExpunge]) —
// wait and are decided the same way, each alone under its own operation id;
// and so does every write of a CHANGE'S LIFECYCLE ([Committer.ProposeChange]):
// an open, an edit, a draft's state, a retarget, a close and a reopen — so a
// change's lifecycle and the pushes that move its branches are decided in the
// order they arrived, by one writer, and contend on one subject. A seat's
// write of a change is judged from the chart as a seat's push is, and a node
// that cannot judge one decides none.
//
// # Bounded in count, in bytes and in time
//
// A repository's queue holds at most CommitterInbox proposals and
// CommitterInboxBytes of entries; past either a push is refused busy AT
// ONCE, never blocked — sixteen full records waiting behind one in flight is
// a repository taking pushes faster than one node commits them. And the node
// holds at most CommitterNodeBytes of proposals across every repository, from
// each one's queueing to its caller's answer: a node commits for every
// repository rendezvous ranks it first for, and per-repository bounds alone
// let a thousand of the largest pushes from as many repositories hold about a
// gibibyte of entries (EntryRoom each), and as much again decoded. A proposal
// waits in the queue only while its asker's attempt still holds a batch and
// its answer — one asked with no deadline, a whole CommitBudget — and is
// refused busy past it, withdrawn as it waits ([Committer.await]) or refused
// as its batch forms ([Committer.admit]).
//
// A refusal at any of these bounds is a BACKLOG, the one busy that says this
// committer turns proposals away — the time bound's too, which a committer
// slower than its pushes meets before its count (busy.go) — so the committer
// keeps those refusals for [CommitBusyWindow] by repository and the engine
// reads them into the node's alarm reading ([Committer.AlarmReading]):
// code_commit_busy stands on the data node whose committer refused them. Only
// a proposal that ARRIVED with its asker's attempt already spent, refused
// before it takes a place in the queue, is no backlog of this committer's.

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
	// repo_deleted for it is told ([RepoGone]). REQUIRED, like Limits: a
	// default here would name the wrong instant to every company that set
	// its own.
	Retention func() time.Duration
	// SeatRules is the deciding node's epoch's chart and calendar, which a
	// SEAT'S push is judged by — its grant (decide rule 2b), its share of a
	// repository outside its unit and its day's quota — read once a batch
	// that carries one. False where this node has installed no epoch yet
	// (its boot, before the first): every seat's push of the batch is then
	// answered [ErrNotServing], a refusal about THIS node, so the router
	// hands it to the repository's runner-up under the same push id, and a
	// person's is decided as ever — never a seat's push let through by a nil
	// roster, nor refused not_permitted by one, which would RECORD a refusal
	// no chart made. REQUIRED: a seat's push is never decided by a default.
	SeatRules func() (SeatRules, bool)
	// Current is whether this node serves the company's configuration: the
	// engine's posture gate (configplane's Reconciler.Admits), false exactly
	// while the node is CONFIRMED behind that configuration — shed or stuck,
	// never lag alone — and nil is current. While it answers false, every
	// proposal carrying a seat's push is answered [ErrNotServing] — as it is
	// queued and again as its batch forms, decided by nobody — so the router
	// hands it to the repository's runner-up under the same push id, as for
	// a stopping committer. A person's push is decided as ever, since no rule
	// of a person's reads the chart; like every data operation on such a
	// node it is asked of this one only after every current data node.
	// Without the gate, a node that failed to apply a revision revoking a seat
	// went on landing that seat's pushes on every repository it commits for,
	// for as long as it stayed stuck.
	Current func() bool
	// Now is the decide's clock — what upload_expired judges a pack's age by,
	// the company day a seat's push is counted on, and a record's authored
	// instant; nil takes the wall clock in UTC.
	Now func() time.Time
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
// ([CommitterOptions.Current], ADR-0058).
type SeatRules struct {
	// Roster is the chart a seat's grant is judged by ([GrantOf]).
	Roster Roster
	// Zone is the company's clock (its timezone, ADR-0018), which the day a
	// seat's pushes are counted on is cut by: nil is UTC.
	Zone *time.Location
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
	metrics   *metrics.Recorder
	log       *slog.Logger

	// idle is CommitterIdle, and a test's shorter value (export_test.go).
	idle time.Duration
	// clock is the queue's clock, which a push's wait is measured by — the
	// wall clock's monotonic reading, never Now (the decide's clock, which a
	// caller may pin), and a test's own (export_test.go).
	clock func() time.Time
	// nodeBytes is CommitterNodeBytes, and a test's smaller value
	// (export_test.go).
	nodeBytes int
	// formedInRound, when set, is told how many protection verdicts each
	// decide round formed inside the publisher's snapshot — a test's
	// (export_test.go), which holds a round under the rules the formation
	// before the publish read to forming none there.
	formedInRound func(formed int)

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
	// queued or in a batch. A caller that leaves before its answer releases
	// its push while a batch that took it still runs, which is at most one
	// batch a repository for at most a CommitBudget.
	held     int
	stopped  bool
	stopping chan struct{}
	wg       sync.WaitGroup
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
// an expunge record's or a change's, which wait in the same queue in the same
// arrival order and are each decided as a batch of their own
// ([Committer.ProposeCompaction], [Committer.ProposeExpunge],
// [Committer.ProposeChange]).
type queued struct {
	// p is the push this proposal is; compaction, expunge or change, where
	// one is set, is the compaction, the expunge record or the write of a
	// change it is instead, and p then its zero value.
	p          Proposal
	compaction *CompactionProposal
	expunge    *ExpungeProposal
	change     *ChangeProposal
	// bytes is entryBound(p), or a compaction's, an expunge record's or a
	// change's encoded payload; rows is rowBound(p), compactDecidedRows of a
	// compaction or decidedRows of an expunge record or a change.
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
	case q.change != nil:
		return q.change.RepoID()
	}
	return q.p.RepoID
}

// The committer's work, as the busy count's `kind` attribute names it.
const (
	kindPush       = "push"
	kindCompaction = "compaction"
	kindExpunge    = "expunge"
	kindChange     = "change"
)

// kind is what q is, as the busy count names it.
func (q *queued) kind() string {
	switch {
	case q.compaction != nil:
		return kindCompaction
	case q.expunge != nil:
		return kindExpunge
	case q.change != nil:
		return kindChange
	}
	return kindPush
}

// alone reports a proposal decided as a batch of its own — a compaction's, an
// expunge record's or a change's, each a record of its own op — never
// gathered with pushes.
func (q *queued) alone() bool { return q.compaction != nil || q.expunge != nil || q.change != nil }

// judgedByChart reports a proposal whose decide reads the chart — a seat's
// push, and a seat's write of a change ([PrincipalKind.readsChart]) — which a
// node behind the company's configuration, or one with no chart yet, never
// decides. A compaction and an expunge record judge no principal by any
// chart.
func (q *queued) judgedByChart() bool {
	switch {
	case q.change != nil:
		return q.change.judgesChart()
	case q.alone():
		return false
	}
	return q.p.Principal.Kind.readsChart()
}

// behindFor is why this node may not decide q now, or "": a proposal whose
// decide reads the chart, while the node is confirmed behind the company's
// configuration ([CommitterOptions.Current]).
func (c *Committer) behindFor(q *queued) string {
	if !q.judgedByChart() || c.current == nil || c.current() {
		return ""
	}
	return fmt.Sprintf("this node is behind the company's configuration and decides no seat's %s from the "+
		"chart the fleet replaced; the repository's next committer decides it", q.kind())
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
	case q.change != nil:
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
// expunge an expunge record's, change a change's — or carried, a push its
// batch decided nothing of, which leads the next batch and is answered there.
type answered struct {
	res     ProposeResult
	compact CompactResult
	expunge ExpungeResult
	change  ChangeResult
	err     error
	carried bool
}

// NewCommitter builds a committer, refusing what it cannot work without.
func NewCommitter(o CommitterOptions) (*Committer, error) {
	switch {
	case o.Publisher == nil:
		return nil, errors.New("code: a committer needs the code log's write authority — " +
			"every push is a record on that log")
	case o.DB.IsZero():
		return nil, errors.New("code: a committer needs the replicated estate a batch's " +
			"answers are read from — an ok is only ever reported from a push's row")
	case !config.ValidNodeID(o.NodeID):
		return nil, fmt.Errorf("code: a committer needs the deciding node's id, which every "+
			"record names as its actor; %q is not one config admits", o.NodeID)
	case o.Limits == nil:
		return nil, errors.New("code: a committer needs the epoch's code.limits — what a " +
			"push's pack is judged against is a decision input, never a default")
	case o.Retention == nil:
		return nil, errors.New("code: a committer needs the epoch's code.deleted_retention_days — " +
			"a push to a deleted repository is told when its purge may begin, which a default " +
			"would misstate for every company that set its own")
	case o.SeatRules == nil:
		return nil, errors.New("code: a committer needs the epoch's chart and calendar (SeatRules) — " +
			"a seat's push is judged by its grant on the chart and counted on the company's day, " +
			"never by a default")
	}
	c := &Committer{
		pub: o.Publisher, db: o.DB, nodeID: o.NodeID, limits: o.Limits, retention: o.Retention,
		seatRules: o.SeatRules, current: o.Current, now: o.Now, metrics: o.Metrics, log: o.Logger,
		idle: CommitterIdle, nodeBytes: CommitterNodeBytes, clock: time.Now,
		repos: map[string]*repoCommitter{}, stopping: make(chan struct{}),
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	if c.log == nil {
		c.log = logging.Get("code")
	}
	c.life, c.end = context.WithCancel(context.Background())
	return c, nil
}

// Propose decides one push in the next batch of its repository and answers
// what became of it. A full queue, or a node holding its bytes
// ([CommitterNodeBytes]), answers busy at once; a push whose caller's
// DEADLINE leaves too little for a batch and its answer ([CommitBudget] and
// [ReplyMargin]) answers busy undecided — at once, or the moment that is so
// while it waits — and one whose caller has no deadline answers busy once it
// has waited a whole CommitBudget; a stopping committer answers
// [ErrNotServing], and so does one that cannot judge a seat's push — behind
// the company's configuration, or holding no chart yet; a proposal that is
// not one answers [ErrInvalidProposal].
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
		// TOO LATE BEFORE IT IS QUEUED: answered without taking the place
		// in the queue a proposal that can still be decided needs. Filed
		// LATE, the one refusal at the queue's time bound that no backlog
		// of this committer's made: its asker's attempt was spent before it
		// arrived (busy.go).
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

// await is a queued push's wait for its answer, on its caller's goroutine.
//
// THE DEADLINE IS WATCHED WHILE THE PUSH WAITS, here: the repository's
// goroutine sees its queue only between batches, so a push queued behind
// batches that ran long reached the head after its asker had stopped waiting
// — silence, which the asker reads as a dead committer and answers by asking
// the runner-up.
//
// AN ANSWER ALREADY GIVEN WINS over the caller's context ending. A stopping
// committer hands every queued push back [ErrNotServing] before its Stop
// returns, and the engine withdraws the estate's serving registration next,
// which CANCELS the context of every answer in flight — so the answer and the
// cancellation are both ready by the time a caller's goroutine is scheduled,
// and a select picks between ready cases at random. Lost to the coin, the
// refusal that sends the push to the repository's runner-up with the same
// push id became `context canceled` — a final failure for a push nothing
// decided — or, sent as the serving node's broker went down, no answer at
// all, which its asker waited out for a whole write attempt.
func (c *Committer) await(ctx context.Context, q *queued) answered {
	var expire <-chan time.Time
	if !q.deadline.IsZero() {
		timer := time.NewTimer(q.deadline.Sub(q.arrived) - CommitBudget - ReplyMargin)
		defer timer.Stop()
		expire = timer.C
	}
	for {
		select {
		case a := <-q.answer:
			return a
		case <-expire:
			expire = nil
			if q.withdraw() {
				// THE QUEUE'S TIME BOUND, met behind this repository's
				// earlier proposals: it queued with time to spare, so
				// it WAITED out its asker here, a backlog of this
				// committer's (busy.go).
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
	case q.change != nil:
		return answered{change: ChangeResult{ChangeID: q.change.ChangeID(), Refusal: ChangeRefusalBusy,
			Detail: refusal}}
	}
	return answered{res: ProposeResult{Answer: refuseAll(q.p, RefusalBusy, refusal)}}
}

// countBusy counts n proposals of kind to repoID refused busy for reason —
// under the `kind` and the `reason` the instrument names, so a repository's
// pushes refused for a backlog read apart from a compaction that met one, and a
// backlog apart from a deadline or a lost race — and files them in the window
// code_commit_busy is read from, writing the lines of any minute that filing
// closed (busy.go).
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
// bytes — or none for a committer that is stopping.
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
	if c.held+q.bytes > c.nodeBytes {
		// BEFORE A GOROUTINE IS STARTED for a repository this node may
		// hold nothing of.
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
		return "", ""
	default:
		return fmt.Sprintf("%d pushes are queued for this repository on this node, the most "+
			"it holds", len(rc.inbox)), busyQueue
	}
}

// Stop refuses every new proposal [ErrNotServing], answers every queued one
// the same — a node that decided nothing, so the router asks the runner-up —
// and waits for the batches in flight, which are FINISHED rather than
// abandoned so their callers get their outcome. If ctx ends first, the
// batches are cancelled and answered from what their publish returned. Safe
// to call more than once.
func (c *Committer) Stop(ctx context.Context) {
	c.mu.Lock()
	if !c.stopped {
		c.stopped = true
		close(c.stopping)
	}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
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
		case first.change != nil:
			c.change(rc.repoID, first)
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
// fits the record, the carried before the queued — WITHOUT LINGER: a batch is
// what was waiting, never what might come. A proposal [Committer.admit] turns
// away is answered and left out; the first that does not fit is carried to
// lead the next batch, and admitted again then, since that batch is the one it
// would start in. It answers the batch and what is carried still, in order.
//
// A COMPACTION, AN EXPUNGE RECORD OR A CHANGE'S WRITE IS NEVER GATHERED: a
// record carries one op, and each is a record of its own (OpCompact,
// OpExpungeRequest, OpExpungeBegin, OpExpungeCommit, and the ops ChangeOps
// lists). Met behind pushes, it ends the batch and is carried to be decided
// alone, after them and before anything queued behind it, so a repository's
// proposals still land in the order they arrived.
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
// answers it when it may not — judged as the batch forms, because that is
// when deciding it would begin. A push it judges is TAKEN first, so its
// caller can no longer withdraw it and nobody else answers it
// ([queued.take]).
//
// A push its caller WITHDREW was answered by its caller. A CALLER THAT HAS
// GONE is answered to nobody: its asker abandoned the attempt, and may
// already have handed the same push id to the runner-up, so deciding it here
// would contend with that node on the repository's subject for a push nobody
// here is waiting for. A push that is LATE ([queued.late]) is answered busy:
// a batch started now could answer it only after its asker stopped waiting —
// and since every push queued with time to spare, it WAITED that time out
// behind this repository's earlier batches, a backlog of this committer's
// ([busyWaited]). None is decided, and none appends anything; the client runs
// git push again.
//
// A SEAT'S PUSH IS JUDGED AGAIN HERE against the node's posture: queued while
// the node served the company's configuration, it may meet its batch after
// the node was confirmed behind it, and is then answered [ErrNotServing] —
// decided by nobody, so the runner-up decides it under the same push id.
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

// release takes q's bytes off what the node holds, once its caller has left
// [Committer.Propose].
func (c *Committer) release(q *queued) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held -= q.bytes
}

// dequeued takes q's bytes off rc's queue.
func (c *Committer) dequeued(rc *repoCommitter, q *queued) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rc.queued -= q.bytes
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
					"push by; the repository's next committer decides it", ErrNotServing)}
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
	// AGAINST ([Proposal.Seen]): the publisher waits for this node's applier
	// to reach the highest of them before it takes the snapshot, and a node
	// that cannot within its budget refuses `behind` — a refusal about this
	// node, so every push is answered ErrNotServing and the router asks the
	// runner-up. Validate held every Seen to the code log, so they compare.
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
	opID := statelog.NewOpID(time.Now(), "code.push")
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
			entries, answers, changeRows, version, err := decideBatch(ctx, tx, repoID, proposals, limits,
				retention, seats, now, &verdicts)
			if c.formedInRound != nil {
				c.formedInRound(verdicts.formed - formed)
			}
			if err != nil {
				return statelog.Decision{}, err
			}
			last = round{entries: entries, answers: answers}
			if len(entries) == 0 {
				return statelog.Decision{Version: version}, nil
			}
			decision, err := recordFor(stamp, subject, OpPush, scope, opID,
				Actor{ID: c.nodeID, Kind: ActorSystem}, PushBatch{RepoID: repoID, Pushes: entries}, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := withinRecord(decision, entries, changeRows); err != nil {
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
	return carried
}

// withinRecord refuses a decision past a record's bytes or rows, which the
// batching and the decide's admission of each entry keep from ever forming —
// the check makes the invariant the record's own rather than the batcher's.
// Rows are counted as the decide admitted them: each entry's [entryRows], and
// changeRows, what its apply writes about the live changes the entries move
// ([changeBook]).
func withinRecord(d statelog.Decision, entries []PushEntry, changeRows int) error {
	rows := changeRows
	for _, e := range entries {
		rows += entryRows(e)
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
// # A row is always the answer
//
// The last round to run the decide is not always the round whose record
// landed: a round whose acknowledgement was lost is followed by another that
// decides again — at a later instant, perhaps from rows that do not hold the
// first round's record yet — and the publisher may then find the first one in
// its ledger and answer it, collapsed, without deciding again. Two decisions
// of one batch differ through the clock (an upload past its bound) and
// through whatever a peer wrote between them, so once a record applied, every
// proposal whose push has a row is answered FROM THAT ROW, whichever round
// wrote it, and the decision answers only a proposal with none.
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
		failed := fmt.Errorf("code: the publish of operation %s answered %s without deciding "+
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
			failed := fmt.Errorf("code: the record of operation %s applied at %s and its push rows "+
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
		return nil, fmt.Errorf("code: read the numbers of the changes a batch's answers name: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var number int64
		if err := rows.Scan(&id, &number); err != nil {
			return nil, fmt.Errorf("code: read the numbers of the changes a batch's answers name: %w", err)
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
			return answered{err: fmt.Errorf("%w: push_id: %s is a push to repository %s, not %s",
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
		// ANSWERED WITH NO RECORD: every command noop or ng, or the whole
		// push refused.
		return answered{res: ProposeResult{Answer: PushAnswer{
			PushID: p.PushID, Commands: d.commands, Refused: d.refused, Detail: d.detail,
		}}}
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		return answered{err: fmt.Errorf("code: push %s was in the record of operation %s "+
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
	statelog.ReasonEvicted: true, statelog.ReasonDeferred: true, statelog.ReasonBehind: true,
	statelog.ReasonBelowFloor: true, statelog.ReasonFloorUnknown: true,
	statelog.ReasonWrongStream: true, statelog.ReasonLogTruncated: true, statelog.ReasonSkew: true,
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
		// THE BATCH'S RECORD LANDED AND APPLIES NOWHERE: a gate drops it at
		// the position on every node. JUDGED BY THE REFUSAL'S KIND AND BY
		// WHOSE RECORD IT NAMES, never by its position alone — a refusal
		// before the append names a position too, and so does an eviction
		// found at ANOTHER operation's record, newest on the subject when the
		// batch's answer was lost (statelog.Unavailable.Landed). Answered
		// here, each told a client its push was durable there and lost, when
		// nothing of it was known to be on the log and the runner-up could
		// commit it.
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
		failed := fmt.Errorf("code: the batch of operation %s: %w", opID, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", kindPush, "repo", repoID, "op_id", opID,
			"error", err)
		for i := range out {
			out[i].err = failed
		}
	}
}

// purgedDetail is the sentence of a batch the repository's deletion marker
// refused: [RepoGone]'s purged state, NAMED BY THE MARKER — the row the gate
// that refused it read, which holds the name for ever — in one point read over
// its primary key, the decide's own (deletionIn), so one state is one sentence
// whichever path refused it. Named by the id instead, a client is told of a
// repository it never typed and to declare "its name" without being told what
// that is.
//
// The id names it only where the read cannot: no marker row (the estate this
// node holds was replaced since the gate read it), or a read that failed —
// which leaves the refusal what it is, because the marker answered it and the
// name is only how the sentence says so.
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
