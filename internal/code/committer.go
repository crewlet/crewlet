package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
// group commit of sixty-four small pushes holds the lane 0.16–0.19 ms a push
// against 3.0–4.0 ms for one alone).
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
// The estate router gives the node it asks ONE write attempt, two
// CommitBudgets long, and past it hands the same push id to the runner-up —
// whose committer then contends with this one on the repository's subject.
// So a push is decided here only while both halves of its answer fit that
// attempt: it may WAIT in the queue for less than one CommitBudget, and its
// batch is bounded by another. A push still queued a whole budget after it
// arrived — behind however many batches its repository's queue holds — is
// answered busy as its batch would start, decided by nobody and appended by
// nobody; and one whose caller has already gone is dropped there, because its
// asker may have handed it on and nobody here is waiting for it. A push
// already in a batch keeps the rule above: the batch goes on without its
// caller.
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
// # Bounded in count and in bytes
//
// A repository's queue holds at most CommitterInbox proposals and
// CommitterInboxBytes of entries; past either a push is refused busy AT
// ONCE, never blocked — sixteen full records waiting behind one in flight is
// a repository taking pushes faster than one node commits them.

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
	// Now is the decide's clock — what upload_expired judges a pack's age by
	// and a record's authored instant; nil takes the wall clock in UTC.
	Now func() time.Time
	// Metrics records the batch sizes and the busy refusals; nil records
	// nothing.
	Metrics *metrics.Recorder
	// Logger is where a failed batch is reported; nil is the package's.
	Logger *slog.Logger
}

// Committer is this node's group commit of pushes, a goroutine a repository
// with work.
type Committer struct {
	pub     Publisher
	db      store.ReplicatedReader
	nodeID  string
	limits  func() Limits
	now     func() time.Time
	metrics *metrics.Recorder
	log     *slog.Logger

	// idle is CommitterIdle, and a test's shorter value (export_test.go).
	idle time.Duration
	// clock is the queue's clock, which a push's wait is measured by — the
	// wall clock's monotonic reading, never Now (the decide's clock, which a
	// caller may pin), and a test's own (export_test.go).
	clock func() time.Time

	// life is the batches' context: the committer's own lifetime, never a
	// caller's — THE ONE CONTEXT STORED IN A STRUCT, because a batch is
	// every caller's in it and none of theirs. Ended by Stop once its wait
	// is over.
	life context.Context
	end  context.CancelFunc

	mu       sync.Mutex
	repos    map[string]*repoCommitter
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

// queued is one proposal waiting for its batch.
type queued struct {
	p     Proposal
	bytes int // entryBound(p)
	rows  int // rowBound(p)
	// arrived is when the proposal was queued, by the committer's clock;
	// gone is its caller's context ending — nil for a caller whose context
	// never ends.
	arrived time.Time
	gone    <-chan struct{}
	// answer receives exactly one value; capacity 1, so the goroutine never
	// blocks on a caller that hung up.
	answer chan answered
}

// answered is one proposal's answer.
type answered struct {
	res ProposeResult
	err error
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
	}
	c := &Committer{
		pub: o.Publisher, db: o.DB, nodeID: o.NodeID, limits: o.Limits, now: o.Now,
		metrics: o.Metrics, log: o.Logger, idle: CommitterIdle,
		clock: time.Now, repos: map[string]*repoCommitter{}, stopping: make(chan struct{}),
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
// what became of it. A full queue answers busy at once, and a push still
// queued a whole [CommitBudget] after it arrived answers busy undecided; a
// stopping committer answers [ErrNotServing]; a proposal that is not one
// answers [ErrInvalidProposal]. ctx bounds only this caller's wait: a push
// whose caller has gone before its batch starts is never decided, and the
// batch it joined goes on without it.
func (c *Committer) Propose(ctx context.Context, p Proposal) (ProposeResult, error) {
	bytes, err := p.measure()
	if err != nil {
		return ProposeResult{}, err
	}
	q := &queued{p: p, bytes: bytes, rows: rowBound(p), arrived: c.clock(), gone: ctx.Done(),
		answer: make(chan answered, 1)}
	//nolint:contextcheck // The batch is the committer's, not this caller's: it runs under the committer's own lifetime so a caller that hangs up never cancels the batch other callers are in.
	if refused, full := c.enqueue(q); full {
		if c.metrics != nil {
			c.metrics.Add(metrics.CodeCommitBusy, 1, nil)
		}
		return ProposeResult{Answer: refuseAll(p, RefusalBusy, refused)}, nil
	} else if refused != "" {
		return ProposeResult{}, fmt.Errorf("%w: %s", ErrNotServing, refused)
	}
	select {
	case a := <-q.answer:
		return a.res, a.err
	case <-ctx.Done():
		return ProposeResult{}, ctx.Err()
	}
}

// enqueue puts q on its repository's queue, starting the repository's
// goroutine when it has none. It answers why it did not: full for a queue at
// its count or its bytes, and a sentence either way.
//
// THE SEND IS UNDER mu, which is what makes the idle exit race-free: a
// goroutine leaves the map only under mu and only with an empty queue, so a
// proposal either reaches a goroutine that will drain it or starts a new one.
func (c *Committer) enqueue(q *queued) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return "the committer is stopping", false
	}
	rc := c.repos[q.p.RepoID]
	if rc == nil {
		rc = &repoCommitter{repoID: q.p.RepoID, inbox: make(chan *queued, CommitterInbox)}
		c.repos[rc.repoID] = rc
		c.wg.Add(1)
		go c.run(rc)
	}
	if rc.queued+q.bytes > CommitterInboxBytes {
		return fmt.Sprintf("%d bytes of pushes are queued for this repository on this node, "+
			"and %d more would pass the %d it holds", rc.queued, q.bytes, CommitterInboxBytes), true
	}
	select {
	case rc.inbox <- q:
		rc.queued += q.bytes
		return "", false
	default:
		return fmt.Sprintf("%d pushes are queued for this repository on this node, the most "+
			"it holds", len(rc.inbox)), true
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
	var carry *queued
	for {
		select {
		case <-c.stopping:
			c.refuseQueued(rc, carry)
			return
		default:
		}
		first := carry
		carry = nil
		if first == nil {
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
		var batch []*queued
		batch, carry = c.gather(rc, first)
		c.commit(rc.repoID, batch)
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(c.idle)
	}
}

// gather is first and every proposal already queued behind it that fits the
// record — WITHOUT LINGER: a batch is what was waiting, never what might
// come. A proposal [Committer.admit] turns away is answered and left out; the
// first that does not fit is carried to lead the next batch, and admitted
// again then, since that batch is the one it would start in.
func (c *Committer) gather(rc *repoCommitter, first *queued) ([]*queued, *queued) {
	batch := []*queued{first}
	bytes, rows := recordOverheadBytes+first.bytes, first.rows
	for len(batch) < MaxPushesPerRecord {
		select {
		case q := <-rc.inbox:
			c.dequeued(rc, q)
			if !c.admit(q) {
				continue
			}
			if bytes+q.bytes > MaxRecordBytes || recordRows(rows+q.rows) > MaxRecordRows {
				return batch, q
			}
			batch = append(batch, q)
			bytes += q.bytes
			rows += q.rows
		default:
			return batch, nil
		}
	}
	return batch, nil
}

// admit reports whether q may be decided in the batch about to start, and
// answers it when it may not — judged as the batch forms, because that is
// when deciding it would begin.
//
// A CALLER THAT HAS GONE is answered to nobody: its asker abandoned the
// attempt, and may already have handed the same push id to the runner-up, so
// deciding it here would contend with that node on the repository's subject
// for a push nobody here is waiting for. A push that has WAITED A WHOLE
// BUDGET is answered busy: its batch may take another, and an answer past
// both comes after its asker's attempt has ended. Neither is decided, and
// neither appends anything; the client runs git push again.
func (c *Committer) admit(q *queued) bool {
	select {
	case <-q.gone:
		return false
	default:
	}
	waited := c.clock().Sub(q.arrived)
	if waited < CommitBudget {
		return true
	}
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCommitBusy, 1, nil)
	}
	q.answer <- answered{res: ProposeResult{Answer: refuseAll(q.p, RefusalBusy, fmt.Sprintf(
		"the push waited %s behind this repository's earlier pushes on this node, and a push "+
			"waits at most %s before its own record is written; run git push again",
		waited.Round(time.Millisecond), CommitBudget))}}
	return false
}

// dequeued takes q's bytes off rc's queue.
func (c *Committer) dequeued(rc *repoCommitter, q *queued) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rc.queued -= q.bytes
}

// refuseQueued answers carry and everything still queued [ErrNotServing], and
// forgets the repository. Nothing can be queued after it: a proposal is sent
// under mu only while the committer is not stopped, and this runs only once
// Stop has said it is.
func (c *Committer) refuseQueued(rc *repoCommitter, carry *queued) {
	refusal := answered{err: fmt.Errorf("%w: the committer stopped before deciding the push",
		ErrNotServing)}
	if carry != nil {
		carry.answer <- refusal
	}
	for {
		select {
		case q := <-rc.inbox:
			q.answer <- refusal
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

// commit publishes one batch as ONE record and answers every proposal in it.
func (c *Committer) commit(repoID string, batch []*queued) {
	ctx, cancel := context.WithTimeout(c.life, CommitBudget)
	defer cancel()
	proposals := make([]Proposal, len(batch))
	for i, q := range batch {
		proposals[i] = q.p
	}
	// THE OPERATION ID IS MINTED ONCE A BATCH, stable across its rounds, by
	// the wall clock: the ledger judges what an id's mint instant vouches for
	// against its own sweeps, never against the decide's clock.
	opID := statelog.NewOpID(time.Now(), "code.push")
	limits := c.limits()
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	var last round
	res, err := c.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET AT THE TOP OF EVERY ROUND: only the last round says
			// what the publish decided.
			last = round{}
			now := c.now()
			entries, answers, version, err := decideBatch(ctx, tx, repoID, proposals, limits, now)
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
			if err := withinRecord(decision, entries); err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = version
			return decision, nil
		},
	})
	answers := c.answers(ctx, repoID, proposals, last, res, err, opID)
	for i, q := range batch {
		q.answer <- answers[i]
	}
}

// withinRecord refuses a decision past a record's bytes or rows, which the
// batching keeps from ever forming — the check makes the invariant the
// record's own rather than the batcher's.
func withinRecord(d statelog.Decision, entries []PushEntry) error {
	rows := 0
	for _, e := range entries {
		rows++
		for _, c := range e.Commands {
			if c.Result == ResultOK {
				rows++
			}
		}
		if e.Pack != nil {
			rows++
		}
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
// of one batch differ through the clock (an upload past its bound, a
// tombstone past its horizon) and through whatever a peer wrote between them,
// so once a record applied, every proposal whose push has a row is answered
// FROM THAT ROW, whichever round wrote it, and the decision answers only a
// proposal with none.
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
	var rows map[string]PushRow
	if res.Outcome == statelog.OutcomeApplied && !res.Position.IsZero() {
		ids := make([]string, len(batch))
		for i, p := range batch {
			ids[i] = p.PushID
		}
		readErr := c.db.Read(ctx, func(tx *sql.Tx) error {
			var err error
			rows, err = pushRowsIn(ctx, tx, ids)
			return err
		})
		if readErr != nil {
			failed := fmt.Errorf("code: the record of operation %s applied at %s and its push rows "+
				"could not be read: %w", opID, res.Position, readErr)
			c.log.WarnContext(ctx, "code_commit_failed", "repo", repoID, "op_id", opID, "error", failed)
			for i := range out {
				out[i].err = failed
			}
			return out
		}
	}
	c.observeBatch(last, res, rows)
	for i, p := range batch {
		d := last.answers[i]
		if d.dupOf >= 0 {
			out[i] = out[d.dupOf]
			continue
		}
		out[i] = c.answer(repoID, p, d, res, rows, opID)
	}
	return out
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
// publish's result.
func (c *Committer) answer(repoID string, p Proposal, d decided, res statelog.Result,
	rows map[string]PushRow, opID string) answered {

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
		// record's.
		ours := row.Position == res.Position
		answer := ProposeResult{
			Outcome: statelog.OutcomeApplied, Position: row.Position,
			Answer: PushAnswer{
				PushID: p.PushID, Commands: withDetails(row.Commands, d.commands), Collapsed: !ours,
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
			Answer: PushAnswer{PushID: p.PushID, Commands: d.collapsed.Commands, Collapsed: true},
		}}
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
			Answer: PushAnswer{PushID: p.PushID, Commands: d.commands},
		}}
	}
	return answered{res: ProposeResult{
		Outcome: statelog.OutcomeUnknown, OpID: opID,
		Answer: PushAnswer{PushID: p.PushID, Commands: d.commands},
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
		if c.metrics != nil {
			c.metrics.Add(metrics.CodeCommitBusy, uint64(len(batch)), nil)
		}
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
		for i, p := range batch {
			out[i].res = ProposeResult{Position: at, OpID: opID,
				Answer: refuseAll(p, RefusalRepoDeleted, "the repository is purged")}
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
		c.log.WarnContext(ctx, "code_commit_failed", "repo", repoID, "op_id", opID, "error", err)
		for i := range out {
			out[i].err = failed
		}
	}
}
