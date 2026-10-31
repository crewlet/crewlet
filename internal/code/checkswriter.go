package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CHECKS WRITER: a repository's check records, published on the
// repository's checks subject ([KindChecks]) by the repository's COMMITTING
// DATA NODE, and by no git node.
//
// A check's records — an attempt queued by the repository's home, claimed and
// ended by the git node that runs it — are each asked of the node the estate
// keys the repository's writes to (internal/estate's code.checks, keyed by the
// repository as a push is), and decided there by this writer, the committer's
// sibling: it shares none of the committer's queue, budget or batches, since a
// check shares nothing with the repository's pushes. Each write is a record of
// its own under its asker's operation id, decided in one snapshot and published
// ADDITIVE ([statelog.PatternAdditive]), as every record on the subject is.
//
// # One write of a repository's checks in flight at a time, awaiting its apply
//
// The subject carries no expectation, so nothing at the broker orders two
// writes about one attempt: two claims of one attempt published at once from
// one node would both land, and the join would keep one runner's — a box made
// for nothing by the other. So the writer takes ONE REPOSITORY'S CHECK WRITES
// ONE AT A TIME — a SINGLE FLIGHT a repository, held from the decide to the
// apply of its record on this node, which the publish waits for — and
// repositories side by side: every decide reads the rows every earlier write
// of the repository left, so a second claim of an attempt is refused `claimed`
// BEFORE it appends, an attempt queued twice is answered from its row, and a
// duplicate appends only across a writer failover — two nodes each believing
// they commit for the repository — where the join converges the rows and the
// loser of a claim, reading the row back ([ChecksResult.Holder]), makes no box.
// A write waits for its turn while its asker's attempt leaves room for a
// publish and its answer ([CommitBudget], [ReplyMargin]), and is refused busy,
// undecided, the moment it does not — the change writer's rule, and for its
// reason. And AT MOST SO MANY WAIT ([ChecksWritesPerRepo] of one repository,
// [ChecksWritesPerNode] in all): each holds its answer slot on the estate's
// proposals lane while it waits, so a write past either bound is answered busy
// at once, undecided, rather than holding a slot a push is waiting for
// (flights.go).
//
// # Its decide: attempt arithmetic and the claim
//
//   - An attempt is QUEUED only after the one before it ended — the attempts
//     of one check of one commit FOR ONE TARGET BRANCH, its key's ([CheckKey]):
//     the home's dispatch is a commit's first attempt for its target — or
//     the next after an attempt its dispatch errored for a checks file that
//     did not run ([CheckReasonDefsInvalid]), once the target's file runs —
//     and its requeue the next after an attempt its infrastructure ended
//     ([CheckReason.Requeued]) while that attempt is below
//     [AutoRequeueAttempts]; no attempt passes [MaxCheckAttempts]; an attempt
//     already queued is answered from its row, unchanged, so a home's second
//     dispatch of a head for its target queues nothing — and its dispatch for
//     another target queues that target's own.
//   - A CLAIM is refused `claimed`, naming the holder, where another runner
//     holds the attempt or it has ended; the holder's own claim asked again is
//     unchanged, and its later record naming the box is decided as the box's
//     one setting.
//   - An ENDING the join would not keep — one after a measurement, or a later
//     infrastructure ending beside an earlier one — is answered from the row,
//     unchanged, and appends nothing.
//   - A DISPATCH — the home's reading of a target's checks file and the heads
//     it dispatched under it (checksdispatch.go) — is answered from the rows,
//     unchanged, where the target's reading and every head's dispatch already
//     say it, at its tip or a later one and naming its file, and refused on an
//     archived repository as an attempt queued there is.
//
// A rerun and a report — a principal's request, a seat's or a person's — are
// judged by the asker's grant, which this writer holds no chart to judge: it
// decides the engine's own records alone, and refuses a principal's as no
// request it decides ([ErrInvalidProposal]).
//
// # The ending it announces
//
// An ending whose record APPLIED, uncollapsed, and which its attempt's row
// keeps, is announced ONCE, by this node, as a `code_check_finished` event on
// the node's own event stream ([ChecksWriterOptions.Events]) — what a node's
// day of usage counts a check from. Never by an applier, which every data node
// runs: the event would be counted once a node.
//
// AN ENDING THAT REPLACES ONE THE ROW KEPT — a measurement after an
// infrastructure ending, a slow runner's exit after the home's sweep errored
// its attempt runner_lost — is announced naming the ending it replaced
// (Replaces), and counted under it rather than as the attempt's first: the
// first was announced and counted when it applied, and the row keeps one
// ending, so a day files the second as a CORRECTION and the attempt is counted
// once. Unmarked, every such attempt was two endings in a day and in
// crewlet.code.checks.finished.
//
// # The publish is the writer's
//
// A write's publish runs under the writer's own lifetime and one CommitBudget,
// never its asker's context, for the change writer's reason;
// [ChecksWriter.Stop] refuses every new write and every one still waiting for
// its turn [ErrNotServing], so the router asks the runner-up, and waits for
// the publishes in flight.

// ChecksWriterOptions configure a checks writer.
type ChecksWriterOptions struct {
	// Publisher is the code log's write authority.
	Publisher Publisher
	// DB is the replicated estate an applied write's answer is read from.
	DB store.ReplicatedReader
	// NodeID is the deciding node, which every record names as its actor.
	NodeID string
	// Retention is the deciding node's epoch's code.deleted_retention_days:
	// when a deleted repository's purge may begin, which a write refused
	// repo_deleted is told ([RepoGone]). REQUIRED, as the committer's is.
	Retention func() time.Duration
	// Events is this node's event stream, which an applied ending is
	// announced on (`code_check_finished`); nil announces none — a node with
	// no queue records no event of any kind.
	Events EventPublisher
	// Now is the decide's clock — a record's authored instant; nil takes the
	// wall clock in UTC.
	Now func() time.Time
	// Metrics records what the writer queued, ended and claimed; nil records
	// nothing.
	Metrics *metrics.Recorder
	// Logger is where a failed write is reported; nil is the package's.
	Logger *slog.Logger
}

// EventPublisher is a node's event stream as the checks writer announces an
// applied ending on it, the committer an applied merge and an opened change,
// and the change writer a review: queue.Queue satisfies it.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, ev *events.Event) error
}

// checkEventBudget bounds the announcement of one applied record's event — a
// check's ending, a change's merge, a change's opening or a review (the last
// two the read of the rows they name and their publishes together):
// FIVE SECONDS, the JetStream client's own request timeout — what nats.go
// gives a publish whose context carries no deadline. Published after the
// write's answer is handed back, so no asker waits on it; a stream that has
// not acknowledged in that long costs the node's day the one check's,
// opening's or review's count, or the feed the one merge, which the rows on
// the code log still hold.
const checkEventBudget = 5 * time.Second

const (
	// ChecksWritesPerRepo is the most writes of ONE repository's checks this
	// node's checks writer holds at once — the one in flight and those
	// waiting their turn behind it: 128, twice MaxChecksPerFile. A
	// repository's checks are written a head at a time — the home queues a
	// head's checks, at most a file's sixty-four, and the git nodes running
	// them claim, name a box for and end each — so the most a repository asks
	// at once is a head's whole dispatch beside a whole round of claims of
	// the head before it. A write holds its turn from its decide to its
	// record's apply, milliseconds when the log is healthy, so a full bound
	// drains in under a second; a write past it is answered busy at once,
	// undecided, and its asker — a runner's claim walk, the home's next pass
	// — sends it again.
	ChecksWritesPerRepo = 2 * MaxChecksPerFile

	// ChecksWritesPerNode is the most writes of repositories' checks this
	// node's checks writer holds at once, every repository's together: 512,
	// four repositories' ChecksWritesPerRepo — a node writes the checks of
	// every repository rendezvous ranks it first for, and four bursts at once
	// is CommitterNodeBytes' reach for pushes. Each holds an answer slot of
	// the estate's proposals lane while it waits, which is sized to hold this
	// beside the committer's whole queue (internal/estate's proposalAnswers),
	// so check writes never take a slot a push is waiting for; past it a
	// write is answered busy at once. A slot costs a goroutine and the write
	// it carries: a claim or a box a few hundred bytes, an ending at most its
	// summary, tail and failures (CheckSummaryBytes, CheckTailBytes,
	// MaxCheckFailureBytes), some 86 KiB, so a full bound of the widest is
	// some 43 MiB, under CommitterNodeBytes' 64 MiB.
	ChecksWritesPerNode = 4 * ChecksWritesPerRepo
)

// ChecksWriter is this node's writer of repositories' checks — one write of a
// repository's checks in flight at a time, and repositories side by side.
type ChecksWriter struct {
	pub       Publisher
	db        store.ReplicatedReader
	nodeID    string
	retention func() time.Duration
	events    EventPublisher
	now       func() time.Time
	metrics   *metrics.Recorder
	log       *slog.Logger

	// clock is the wait's clock — the wall clock's monotonic reading, never
	// Now — and a test's own (export_test.go).
	clock func() time.Time

	// life is the publishes' context: the writer's own lifetime, never an
	// asker's. Ended by Stop once its wait is over.
	life context.Context
	end  context.CancelFunc

	// flights is each repository's single flight on this node, bounded at
	// ChecksWritesPerRepo and ChecksWritesPerNode.
	flights *flights
}

// NewChecksWriter builds a checks writer, refusing what it cannot work
// without.
func NewChecksWriter(o ChecksWriterOptions) (*ChecksWriter, error) {
	switch {
	case o.Publisher == nil:
		return nil, errors.New("code: a checks writer needs the code log's write authority — every write of " +
			"a repository's checks is a record on that log")
	case o.DB.IsZero():
		return nil, errors.New("code: a checks writer needs the replicated estate a write's answer is read " +
			"from — an applied write is only ever reported from its rows")
	case !config.ValidNodeID(o.NodeID):
		return nil, fmt.Errorf("code: a checks writer needs the deciding node's id, which every record names "+
			"as its actor; %q is not one config admits", o.NodeID)
	case o.Retention == nil:
		return nil, errors.New("code: a checks writer needs the epoch's code.deleted_retention_days — a write " +
			"about a deleted repository is told when its purge may begin, which a default would misstate")
	}
	w := &ChecksWriter{
		pub: o.Publisher, db: o.DB, nodeID: o.NodeID, retention: o.Retention, events: o.Events, now: o.Now,
		metrics: o.Metrics, log: o.Logger, clock: time.Now,
		flights: newFlights("repository", ChecksWritesPerRepo, "code.ChecksWritesPerRepo", ChecksWritesPerNode,
			"code.ChecksWritesPerNode"),
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	if w.log == nil {
		w.log = logging.Get("code")
	}
	w.life, w.end = context.WithCancel(context.Background())
	return w, nil
}

// Validate refuses a request no correct asker forms ([ErrInvalidProposal],
// naming the field): one setting none or more than one record, an operation
// id this engine does not accept, a floor on another log, a record its own
// Validate refuses, a dispatch of any attempt but a commit's first or a
// requeue of its first — and a principal's record, a rerun or a report, which
// this writer decides none of (see the file's doc). An attempt past
// [MaxCheckAttempts] is no invalid request: its asker formed it from the
// attempts it read, and it is refused attempts_exhausted, from the rows.
func (r ChecksRequest) Validate() error {
	set := 0
	for _, isSet := range []bool{r.Queued != nil, r.Started != nil, r.Finished != nil, r.Dispatched != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: a write of a repository's checks is exactly one of an attempt queued, a claim, "+
			"an ending and a dispatch, and this one sets %d", ErrInvalidProposal, set)
	}
	if err := statelog.CheckCallerOpID(r.OpID); err != nil {
		return badField("op_id", "%v", err)
	}
	if !r.Seen.IsZero() {
		switch {
		case r.Seen.Stream != topics.CodeLogStream:
			return badField("seen", "%s is a position on another log than the code log (%s)", r.Seen,
				topics.CodeLogStream)
		case r.Seen.Valid() != nil:
			return fmt.Errorf("%w: seen: %w", ErrInvalidProposal, r.Seen.Valid())
		}
	}
	op, payload := r.record()
	switch p := payload.(type) {
	case CheckQueued:
		switch {
		case p.Reason != QueueDispatch && p.Reason != QueueRequeue:
			return badField("queued.reason", "%q: a rerun and a report are a principal's, decided only "+
				"judged by the asker's grant, and this writer decides the engine's own dispatch and requeue",
				p.Reason)
		case p.Reason == QueueDispatch && p.Attempt < 1:
			return badField("queued.attempt", "%d: attempts count from 1", p.Attempt)
		case p.Reason == QueueRequeue && p.Attempt < 2:
			return badField("queued.attempt", "%d: a requeue follows an attempt that ended", p.Attempt)
		}
		// AN ATTEMPT PAST THE LAST is judged against the rows, so its
		// other fields are held to the record's rules at the last.
		if p.Attempt > MaxCheckAttempts {
			p.Attempt = MaxCheckAttempts
		}
		payload = p
	case CheckStarted:
		if p.Placement == PlacementExternal {
			return badField("started.placement", "a check a person's CI runs is a report, a principal's, "+
				"decided only judged by the asker's grant, and this writer decides the engine's own claims")
		}
	case CheckFinished:
		if p.Reporter != nil {
			return badField("finished.reporter", "a reported ending is a principal's, decided only judged "+
				"by the asker's grant, and this writer decides the engine's own endings")
		}
	}
	if err := payload.(interface{ Validate() error }).Validate(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidProposal, op, err)
	}
	return nil
}

// record is the op and the payload r carries — "" and nil for a request that
// carries none, which [ChecksRequest.Validate] refuses before anything reads
// this.
func (r ChecksRequest) record() (OpKind, any) {
	switch {
	case r.Queued != nil:
		return OpCheckQueued, *r.Queued
	case r.Started != nil:
		return OpCheckStarted, *r.Started
	case r.Finished != nil:
		return OpCheckFinished, *r.Finished
	case r.Dispatched != nil:
		return OpCheckDispatched, *r.Dispatched
	}
	return "", nil
}

// Key is the attempt r is about — none for a dispatch, which is about a
// target and its heads.
func (r ChecksRequest) Key() CheckKey {
	switch {
	case r.Queued != nil:
		return r.Queued.CheckKey
	case r.Started != nil:
		return r.Started.CheckKey
	case r.Finished != nil:
		return r.Finished.CheckKey
	}
	return CheckKey{}
}

// RepoID is the repository r is about — "" for a request that carries no
// record, which the estate's router refuses as it refuses a push naming no
// repository. It is the ONE answer the router keys r by.
func (r ChecksRequest) RepoID() string {
	if r.Dispatched != nil {
		return r.Dispatched.RepoID
	}
	return r.Key().RepoID
}

// Publish decides one write of a repository's checks and answers what became
// of it: a write whose asker's DEADLINE leaves too little for a publish and its
// answer is answered busy undecided — at once, or the moment that is so while
// it waits for its repository's turn — and one whose asker has none waits at
// most a CommitBudget; a stopping writer answers [ErrNotServing]; a request
// that is not one answers [ErrInvalidProposal]. ctx bounds only this caller's
// wait.
//
// ITS OPERATION ID IS THE WRITE'S OWN, so a write its asker hands the
// repository's runner-up after this node went silent is the same write there,
// collapsed by the ledger onto the record that landed. Applied is answered
// FROM THE ROWS the record wrote, never from the decision.
func (w *ChecksWriter) Publish(ctx context.Context, r ChecksRequest) (ChecksResult, error) {
	if err := r.Validate(); err != nil {
		w.count(r, ChecksResult{}, err, checksLanded{})
		return ChecksResult{}, err
	}
	res, err, counted := w.publish(ctx, r)
	if !counted {
		w.count(r, res, err, checksLanded{})
	}
	return res, err
}

// stoppingChecks is the refusal of a write a stopping writer decides nothing
// of.
var stoppingChecks = fmt.Errorf("%w: the checks writer is stopping", ErrNotServing)

// checksAgain is what a write refused busy or unavailable is told to do.
const checksAgain = "send the write again"

// publish is [ChecksWriter.Publish] once the request is known to be one, and
// whether its outcome is counted already — a publish counts its own, since it
// ends whether or not its asker still waits.
func (w *ChecksWriter) publish(ctx context.Context, r ChecksRequest) (ChecksResult, error, bool) {
	arrived := w.clock()
	deadline, hasDeadline := ctx.Deadline()
	wait := CommitBudget
	if hasDeadline {
		if wait = deadline.Sub(arrived) - CommitBudget - ReplyMargin; wait <= 0 {
			return busyChecks(w.shortOfTime(deadline.Sub(arrived))), nil, false
		}
	}
	repoID := r.RepoID()
	j := w.flights.join(repoID)
	switch {
	case j.stopped:
		return ChecksResult{}, stoppingChecks, false
	case j.busy != "":
		return busyChecks(j.busy + "; " + checksAgain), nil, false
	}
	turn := j.flight
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case turn.token <- struct{}{}:
	case <-timer.C:
		w.flights.leave(repoID, turn)
		if hasDeadline {
			return busyChecks(w.shortOfTime(deadline.Sub(w.clock()))), nil, false
		}
		return busyChecks(fmt.Sprintf("the write waited %s behind this repository's other check writes on "+
			"this node, and a write waits at most %s for its turn; %s", w.clock().Sub(arrived).Round(
			time.Millisecond), CommitBudget, checksAgain)), nil, false
	case <-ctx.Done():
		w.flights.leave(repoID, turn)
		return ChecksResult{}, ctx.Err(), false
	case <-w.flights.stopping:
		w.flights.leave(repoID, turn)
		return ChecksResult{}, stoppingChecks, false
	}
	release := func() {
		<-turn.token
		w.flights.leave(repoID, turn)
	}
	if !w.flights.launch() {
		release()
		return ChecksResult{}, stoppingChecks, false
	}
	type answer struct {
		res ChecksResult
		err error
	}
	done := make(chan answer, 1)
	//nolint:contextcheck // The publish is the writer's, not this caller's: it runs under the writer's own lifetime, so an asker that hangs up mid-publish never leaves the repository's next check write deciding beside a record still in flight.
	go func() {
		defer w.flights.landed()
		res, err, own := w.write(r)
		// THE TURN IS RELEASED ONCE THE ANSWER IS KNOWN — the record applied
		// here, or the wait for it gave up — and before the announcement,
		// which no write of the repository waits on.
		release()
		w.count(r, res, err, own)
		done <- answer{res, err}
		if own.ending != nil {
			w.announce(own.ending)
		}
	}()
	select {
	case a := <-done:
		return a.res, a.err, true
	case <-ctx.Done():
		// AN ANSWER ALREADY GIVEN WINS over the asker's context ending, for
		// the committer's reason.
		select {
		case a := <-done:
			return a.res, a.err, true
		default:
		}
		return ChecksResult{}, ctx.Err(), true
	}
}

// shortOfTime is the refusal of a write whose asker stops waiting in left —
// no more than a publish and its answer may take.
func (w *ChecksWriter) shortOfTime(left time.Duration) string {
	left = min(max(left, 0), CommitBudget+ReplyMargin)
	return fmt.Sprintf("the write's asker stops waiting in %s, and a publish started now may take %s and its "+
		"answer %s more; %s", left.Round(time.Millisecond), CommitBudget, ReplyMargin, checksAgain)
}

// busyChecks is a write refused busy, undecided, saying detail.
func busyChecks(detail string) ChecksResult {
	return ChecksResult{Refusal: CheckRefusalBusy, Detail: detail}
}

// Stop refuses every new write [ErrNotServing], and every one waiting for its
// repository's turn — a node that decided nothing, so the router asks the
// runner-up — and waits for the publishes in flight, which are FINISHED rather
// than abandoned so their askers get their outcome. If ctx ends first, they
// are cancelled and answered from what their publish returned. Safe to call
// more than once.
func (w *ChecksWriter) Stop(ctx context.Context) { w.flights.stop(ctx, w.end) }

// checksDecision is what one round of a write's decide concluded: a payload
// to append, or none — refused, or unchanged — with the row it read.
type checksDecision struct {
	payload   any
	refusal   CheckRefusal
	detail    string
	unchanged bool
	// row is the attempt as the snapshot held it, nil for none.
	row *CheckRow
}

// checksLanded is what a write's answer says of its OWN record, which the
// result alone cannot: whether this call's record applied here — never a
// retry the ledger collapsed onto an earlier landing, whose call counted and
// announced it — and the ending it announces, nil for none.
type checksLanded struct {
	fresh  bool
	ending *types.CodeCheckFinished
}

// write publishes one write of a repository's checks and answers it — under
// the writer's own lifetime and one CommitBudget — with what landed of its
// own record.
func (w *ChecksWriter) write(r ChecksRequest) (ChecksResult, error, checksLanded) {
	ctx, cancel := context.WithTimeout(w.life, CommitBudget)
	defer cancel()
	op, _ := r.record()
	retention := w.retention()
	repoID := r.RepoID()
	subject := ChecksSubject(repoID)
	scope := []string{ChecksPath(repoID)}
	var (
		last    checksDecision
		decided bool
	)
	res, err := w.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: r.OpID, Pattern: statelog.PatternAdditive, Session: r.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			last, decided = checksDecision{}, false
			d, err := decideChecks(ctx, tx, r, retention)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.payload == nil {
				return statelog.Decision{}, nil
			}
			return recordFor(stamp, subject, op, scope, r.OpID, Actor{ID: w.nodeID, Kind: ActorSystem},
				d.payload, w.now())
		},
	})
	if err != nil {
		res, err := w.refused(ctx, r, err)
		return res, err, checksLanded{}
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() && r.Dispatched != nil {
			return w.dispatchApplied(ctx, r, res)
		}
		if !res.Position.IsZero() {
			// AN ENDING THE ROW ALREADY KEPT, which this one's decide chose
			// to replace — a measurement over an infrastructure ending — is
			// what the announcement names it a correction of.
			var replacing CheckState
			if decided && r.Finished != nil && last.row != nil && !last.row.Finished.IsZero() {
				replacing = last.row.State
			}
			return w.applied(ctx, r, res, replacing)
		}
		// NOTHING WAS APPENDED: the decide refused the write or found it
		// unchanged, and its last round says which.
		switch {
		case decided && last.refusal != "":
			out := ChecksResult{Refusal: last.refusal, Detail: last.detail, Row: last.row}
			if last.refusal == CheckRefusalClaimed && last.row != nil {
				out.Holder = last.row.Runner
			}
			return out, nil, checksLanded{}
		case decided && last.unchanged:
			out := ChecksResult{Outcome: statelog.OutcomeApplied, Unchanged: true, Row: last.row}
			if last.row != nil {
				out.Holder = last.row.Runner
				out.Position = statelog.Later(last.row.Queued, last.row.Finished)
			}
			return out, nil, checksLanded{}
		}
		return ChecksResult{}, fmt.Errorf("code: the publish of operation %s answered applied with no "+
			"record, and its decide neither refused nor found it unchanged", r.OpID), checksLanded{}
	case statelog.OutcomePending:
		return ChecksResult{Outcome: statelog.OutcomePending, Position: res.Position}, nil, checksLanded{}
	}
	return ChecksResult{Outcome: statelog.OutcomeUnknown}, nil, checksLanded{}
}

// decideChecks is one round of a write's decide, in ONE snapshot — see the
// file's doc.
func decideChecks(ctx context.Context, tx *sql.Tx, r ChecksRequest, retention time.Duration) (checksDecision,
	error) {

	key, repoID := r.Key(), r.RepoID()
	repo, found, err := pushRepoIn(ctx, tx, repoID)
	if err != nil {
		return checksDecision{}, err
	}
	var d checksDecision
	switch {
	case !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging:
		detail, err := goneDetail(ctx, tx, repoID, repo, found, retention)
		if err != nil {
			return checksDecision{}, err
		}
		return d.refuse(CheckRefusalRepoDeleted, detail), nil
	case (r.Queued != nil || r.Dispatched != nil) && repo.status == RepoStatusArchived:
		return d.refuse(CheckRefusalRepoArchived, fmt.Sprintf("repository %s is archived and read-only "+
			"(code.repos archived: true): nothing moves in it to be checked", repo.name)), nil
	case r.Dispatched != nil:
		return decideDispatched(ctx, tx, *r.Dispatched, d)
	}
	named := func(k CheckKey) string {
		return fmt.Sprintf("attempt %d of check %s of %s for %s on %s", k.Attempt, k.Name, k.OID, k.Target,
			repo.name)
	}
	row, held, err := checkRowIn(ctx, tx, key)
	if err != nil {
		return checksDecision{}, err
	}
	if held {
		d.row = &row
	}
	switch {
	case r.Queued != nil:
		return decideQueued(ctx, tx, *r.Queued, d, held, named)
	case r.Started != nil:
		return decideStarted(*r.Started, d, held, named)
	}
	return decideFinished(*r.Finished, d, held, named)
}

// refuse is d refused with code and its sentence.
func (d checksDecision) refuse(code CheckRefusal, detail string) checksDecision {
	d.refusal, d.detail, d.payload = code, detail, nil
	return d
}

// decideQueued is the attempt arithmetic: the attempt queued already is
// answered from its row; otherwise it follows the latest attempt, which has
// ended, and — for a requeue — ended for its infrastructure below
// AutoRequeueAttempts; and none passes MaxCheckAttempts.
func decideQueued(ctx context.Context, tx *sql.Tx, q CheckQueued, d checksDecision, held bool,
	named func(CheckKey) string) (checksDecision, error) {

	if held && !d.row.Queued.IsZero() {
		// THE ATTEMPT IS QUEUED: a second dispatch of a head, a requeue a
		// failover asked twice — whatever it carried, the row's queue is the
		// first one's, and a second record would change nothing.
		d.unchanged = true
		return d, nil
	}
	if held {
		// A ROW WITH NO QUEUE no writer of this build makes: every attempt
		// it records begins queued.
		return checksDecision{}, fmt.Errorf("code: %s holds no queue — no writer records an attempt it did "+
			"not queue", named(q.CheckKey))
	}
	latest, found, err := latestCheckIn(ctx, tx, q.CheckKey)
	if err != nil {
		return checksDecision{}, err
	}
	prev := q.CheckKey
	prev.Attempt--
	switch {
	case !found && q.Attempt == 1:
		d.payload = q
		return d, nil
	case !found:
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("%s follows %s, and no attempt of the check of "+
			"that commit is held", named(q.CheckKey), named(prev))), nil
	case latest.Attempt > q.Attempt:
		// A LATER ATTEMPT IS HELD and this one is not — past the history,
		// its row removed: the check has moved on from it.
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("%s is no longer held; the check's latest is "+
			"attempt %d", named(q.CheckKey), latest.Attempt)), nil
	case latest.Attempt < q.Attempt-1:
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("%s follows %s, which is not held; the check's "+
			"latest is attempt %d", named(q.CheckKey), named(prev), latest.Attempt)), nil
	case !latest.State.Ended():
		d.row = &latest
		return d.refuse(CheckRefusalNotFinished, fmt.Sprintf("%s is still %s; a check of a commit runs one "+
			"attempt at a time", named(prev), latest.State)), nil
	case q.Attempt > MaxCheckAttempts:
		d.row = &latest
		return d.refuse(CheckRefusalAttemptsExhausted, fmt.Sprintf("%s was the last: a check of a commit "+
			"runs at most %d attempts (code.MaxCheckAttempts)", named(prev), MaxCheckAttempts)), nil
	case q.Reason == QueueDispatch && (latest.State != CheckStateErrored || latest.Reason != CheckReasonDefsInvalid):
		// A DISPATCH FOLLOWS ONLY ITS OWN ERROR: the head's dispatch under a
		// checks file that runs, after an earlier one errored the check for a
		// file that did not — which nobody ran, so nothing of the commit was
		// measured. Any other ending is a rerun's to follow.
		d.row = &latest
		return d.refuse(CheckRefusalRerunGenuine, fmt.Sprintf("%s ended %s (%s); the home's dispatch queues "+
			"again only an attempt a dispatch errored for a checks file that did not run, and a rerun runs any "+
			"other", named(prev), latest.State, latest.Reason)), nil
	case q.Reason == QueueRequeue && (latest.State != CheckStateErrored || !latest.Reason.Requeued()):
		d.row = &latest
		return d.refuse(CheckRefusalRerunGenuine, fmt.Sprintf("%s ended %s (%s), which its infrastructure "+
			"did not cause; the home queues again only an attempt its runner lost or its box or setup "+
			"failed", named(prev), latest.State, latest.Reason)), nil
	case q.Reason == QueueRequeue && latest.Attempt >= AutoRequeueAttempts:
		d.row = &latest
		return d.refuse(CheckRefusalAttemptsExhausted, fmt.Sprintf("%s ended %s, and the home queues a "+
			"check again on its own to at most attempt %d (code.AutoRequeueAttempts); a rerun runs it again",
			named(prev), latest.Reason, AutoRequeueAttempts)), nil
	}
	d.payload = q
	return d, nil
}

// decideStarted is the claim: refused `claimed` where another runner holds
// the attempt or it has ended; the holder's own claim asked again unchanged;
// and its later record naming the box decided as the box's one setting.
func decideStarted(s CheckStarted, d checksDecision, held bool, named func(CheckKey) string) (checksDecision,
	error) {

	if !held {
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("%s is not held: a runner claims an attempt the "+
			"rows hold queued", named(s.CheckKey))), nil
	}
	row := d.row
	switch {
	case row.State.Ended():
		return d.refuse(CheckRefusalClaimed, fmt.Sprintf("%s has ended %s", named(s.CheckKey), row.State)), nil
	case row.State == CheckStateRunning && !sameClaim(*row, s):
		return d.refuse(CheckRefusalClaimed, fmt.Sprintf("%s is held by runner %s, claimed at %s",
			named(s.CheckKey), row.Runner, row.StartedAt.Format(time.RFC3339Nano))), nil
	case row.State == CheckStateRunning:
		// THE HOLDER'S OWN: its claim sent again, or its record naming the
		// box it made — which sets the box once, and the borrowed flag on.
		switch {
		case s.Placement != row.Placement:
			return checksDecision{}, fmt.Errorf("%w: started.placement: %s is claimed for a %s cell, and its "+
				"holder names %s", ErrInvalidProposal, named(s.CheckKey), row.Placement, s.Placement)
		case s.Box != "" && row.Box != "" && s.Box != row.Box:
			return checksDecision{}, fmt.Errorf("%w: started.box: %s runs in box %s, and its holder names "+
				"%s — a holder names its one box once", ErrInvalidProposal, named(s.CheckKey), row.Box, s.Box)
		case (s.Box == "" || s.Box == row.Box) && (!s.Borrowed || row.Borrowed):
			d.unchanged = true
			return d, nil
		}
		d.payload = s
		return d, nil
	case s.Box != "":
		return checksDecision{}, fmt.Errorf("%w: started.box: %s holds no claim, and a box is named by its "+
			"claim's holder once its claim is recorded", ErrInvalidProposal, named(s.CheckKey))
	}
	d.payload = s
	return d, nil
}

// sameClaim reports whether row's run is claim s's: the same runner, claiming
// at the same instant AS THE ROW HOLDS IT — a claim's key, which the join
// keeps by, at the store's microsecond (store.EncodeTime), never the
// nanoseconds a runner's clock gave the record: compared there, the holder's
// own record naming its box was another runner's claim, refused.
func sameClaim(row CheckRow, s CheckStarted) bool {
	return row.Runner == s.Runner && store.EncodeTime(row.StartedAt) == store.EncodeTime(s.StartedAt)
}

// decideFinished is an ending: answered from the row, unchanged, where the
// join would keep the row's own ending over it — after a measurement, or
// beside an earlier ending of the same kind.
func decideFinished(f CheckFinished, d checksDecision, held bool, named func(CheckKey) string) (checksDecision,
	error) {

	if !held {
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("%s is not held: an ending ends an attempt the "+
			"rows hold", named(f.CheckKey))), nil
	}
	if d.row.Placement == "" && f.Reason.ranIn() {
		// A RUN'S ENDING OF AN ATTEMPT NO RUNNER CLAIMED no writer forms: a
		// runner ends what it claimed, and names its claim's position as its
		// floor, so this node holds the claim before it decides.
		return checksDecision{}, fmt.Errorf("%w: finished.reason: %s holds no claim, and an attempt ends %s "+
			"only once a runner claimed it", ErrInvalidProposal, named(f.CheckKey), f.Reason)
	}
	if !d.row.Finished.IsZero() {
		kept := checkEnding{state: string(d.row.State), finishedAt: store.EncodeTime(d.row.FinishedAt),
			reporter: d.row.Reporter, position: d.row.Finished.Packed()}
		// THE NEW ENDING LANDS PAST EVERY RECORD THIS SNAPSHOT HOLDS, so its
		// position is past the kept one's whatever it turns out to be.
		ending := checkEnding{state: string(f.State), finishedAt: store.EncodeTime(f.FinishedAt),
			position: math.MaxInt64}
		if !endsBefore(ending, kept) {
			d.unchanged = true
			return d, nil
		}
	}
	d.payload = f
	return d, nil
}

// applied answers a write whose record applied at res.Position, FROM THE ROW:
// the attempt's row holds what the record carried — a queue, a claim, an
// ending — only once the record, or one after it, applied. A row that does not
// is a record that applied and wrote nothing this node holds — the repository
// purged under it — logged at ERROR and answered unknown, never applied. The
// record is this call's own unless the ledger collapsed the call onto an
// earlier landing, and an ending the row KEEPS of its own is the one this node
// announces — naming the ending it replaced, where its decide read one on the
// row (replacing), so the announcement is a correction of that one and never
// a second ending of the attempt.
func (w *ChecksWriter) applied(ctx context.Context, r ChecksRequest, res statelog.Result,
	replacing CheckState) (ChecksResult, error, checksLanded) {

	key, at := r.Key(), res.Position
	var (
		row       CheckRow
		held      bool
		repoName  string
		readError error
	)
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		if row, held, err = checkRowIn(ctx, tx, key); err != nil || !held {
			return err
		}
		switch err := tx.QueryRowContext(ctx, repoNameSQL, key.RepoID).Scan(&repoName); {
		case errors.Is(err, sql.ErrNoRows):
			repoName = key.RepoID
		case err != nil:
			return err
		}
		return nil
	}); err != nil {
		readError = fmt.Errorf("code: the record of operation %s applied at %s and its attempt's row could not "+
			"be read: %w", r.OpID, at, err)
		w.log.WarnContext(ctx, "code_checks_failed", "repo", key.RepoID, "op_id", r.OpID, "error", readError)
		return ChecksResult{}, readError, checksLanded{}
	}
	wrote := held
	switch {
	case !held:
	case r.Queued != nil:
		wrote = !row.Queued.IsZero()
	case r.Started != nil:
		wrote = row.Placement != ""
	case r.Finished != nil:
		wrote = !row.Finished.IsZero()
	}
	if !wrote {
		w.log.ErrorContext(ctx, "code_checks_unanswerable", "repo", key.RepoID, "op_id", r.OpID,
			"position", at.String())
		return ChecksResult{Outcome: statelog.OutcomeUnknown, Detail: fmt.Sprintf("the write's record applied "+
			"at %s and the attempt's row this node holds does not carry it", at)}, nil, checksLanded{}
	}
	out := ChecksResult{Outcome: statelog.OutcomeApplied, Position: at, Row: &row, Holder: row.Runner}
	// A RETRY COLLAPSED ONTO AN EARLIER LANDING is that landing's, whose call
	// counted and announced it.
	if res.Collapsed {
		return out, nil, checksLanded{}
	}
	own := checksLanded{fresh: true}
	// THE ENDING ANNOUNCED IS THE ONE THE ROW KEEPS: an ending the join set
	// aside is no ending of the attempt's.
	ours := !row.Finished.IsZero() && row.Finished.Packed() == at.Packed()
	if (r.Finished != nil && ours) || (r.Queued != nil && (r.Queued.Skipped || r.Queued.Invalid != "") &&
		row.State.Ended() && ours) {
		own.ending = &types.CodeCheckFinished{RepoID: key.RepoID, RepoName: repoName, OID: key.OID,
			Name: key.Name, Attempt: key.Attempt, Purpose: string(row.Purpose), State: string(row.State),
			Reason: string(row.Reason), Placement: string(row.Placement), ChangeID: row.ChangeID,
			Target: row.Target, Required: row.Required, DurationMs: row.DurationMs, Replaces: string(replacing)}
	}
	return out, nil, own
}

// dispatchApplied answers a dispatch whose record applied at res.Position,
// FROM THE ROWS: its target's reading, which the record — or one that read a
// later tip — wrote. A target the rows hold no reading of is a record that
// applied and wrote nothing this node holds — the repository purged under it
// — logged at ERROR and answered unknown, never applied.
func (w *ChecksWriter) dispatchApplied(ctx context.Context, r ChecksRequest, res statelog.Result) (ChecksResult,
	error, checksLanded) {

	d := r.Dispatched
	var held bool
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var tip int64
		var defs string
		switch err := tx.QueryRowContext(ctx, checkTargetDefsSQL, d.RepoID, d.Target).Scan(&tip, &defs); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		held = true
		return nil
	}); err != nil {
		err = fmt.Errorf("code: the dispatch of operation %s applied at %s and its target's reading could not "+
			"be read: %w", r.OpID, res.Position, err)
		w.log.WarnContext(ctx, "code_checks_failed", "repo", d.RepoID, "op_id", r.OpID, "error", err)
		return ChecksResult{}, err, checksLanded{}
	}
	if !held {
		w.log.ErrorContext(ctx, "code_checks_unanswerable", "repo", d.RepoID, "op_id", r.OpID,
			"position", res.Position.String())
		return ChecksResult{Outcome: statelog.OutcomeUnknown, Detail: fmt.Sprintf("the dispatch's record applied "+
			"at %s and this node holds no reading of target %s", res.Position, d.Target)}, nil, checksLanded{}
	}
	return ChecksResult{Outcome: statelog.OutcomeApplied, Position: res.Position}, nil,
		checksLanded{fresh: !res.Collapsed}
}

// announce publishes an applied ending's event on this node's event stream,
// bounded by [checkEventBudget] under the writer's own lifetime: a stream
// that does not take it is logged, and the attempt's row still says it ended.
func (w *ChecksWriter) announce(e *types.CodeCheckFinished) {
	if w.events == nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.life, checkEventBudget)
	defer cancel()
	ev := events.New(*e, events.TraceContext{})
	ev.Source = w.nodeID
	if err := w.events.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		w.log.WarnContext(ctx, "code_check_finished_unpublished", "repo", e.RepoID, "oid", e.OID, "check",
			e.Name, "attempt", e.Attempt, "error", err.Error(),
			"detail", "this node's day of usage does not count this check's ending; its row on the code log "+
				"still holds it")
	}
}

// refused answers a write whose publish failed — the change writer's mapping
// in a check's words.
func (w *ChecksWriter) refused(ctx context.Context, r ChecksRequest, err error) (ChecksResult, error) {
	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal):
		w.log.ErrorContext(ctx, "code_checks_invalid", "repo", r.RepoID(), "op_id", r.OpID, "error", err)
		return ChecksResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		// NO EXPECTATION IS ARBITRATED on the subject, so no round loses one;
		// a conflict here is the publisher's own, answered busy.
		return busyChecks("the code log kept refusing the write's round; " + checksAgain), nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		return ChecksResult{Refusal: CheckRefusalRepoDeleted, Position: at,
			Detail: purgedDetail(ctx, w.db, w.log, r.RepoID(), r.OpID)}, nil
	case errors.As(err, &refusal) && refusal.Landed():
		return ChecksResult{Refusal: CheckRefusalNotApplied, Position: refusal.Position,
			Detail: fmt.Sprintf("the write applies nowhere: its record at %s was durable and the code log's %s "+
				"gate drops it on every node; %s", refusal.Position, refusal.Reason, checksAgain)}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return ChecksResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return ChecksResult{Refusal: CheckRefusalUnavailable,
			Detail: "the code log refused the write: " + strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
	}
	w.log.WarnContext(ctx, "code_checks_failed", "repo", r.RepoID(), "op_id", r.OpID, "error", err)
	return ChecksResult{}, fmt.Errorf("code: checks operation %s: %w", r.OpID, err)
}

// count counts one write's answer: a claim's outcome under
// crewlet.code.checks.claims, and — for this call's OWN record, applied here
// and never a retry the ledger collapsed onto an earlier landing — the
// attempt it queued under crewlet.code.checks.queued and the ending the row
// keeps of it under crewlet.code.checks.finished ([ChecksWriter.applied]'s
// judgement, own).
func (w *ChecksWriter) count(r ChecksRequest, res ChecksResult, err error, own checksLanded) {
	if w.metrics == nil {
		return
	}
	if r.Started != nil && r.Started.Box == "" {
		w.metrics.Add(metrics.CodeChecksClaims, 1, metrics.Attrs{"outcome": claimOutcome(*r.Started, res, err)})
	}
	if err != nil || !own.fresh || res.Row == nil {
		return
	}
	if r.Queued != nil && !r.Queued.Skipped && r.Queued.Invalid == "" {
		w.metrics.Add(metrics.CodeChecksQueued, 1, metrics.Attrs{"purpose": string(r.Queued.Purpose)})
	}
	if own.ending != nil {
		placement, replaced := string(res.Row.Placement), own.ending.Replaces
		if placement == "" {
			placement = "none"
		}
		if replaced == "" {
			// AN ATTEMPT'S FIRST ENDING; a replacement is counted apart,
			// under the ending it replaced, so the attempts that ended are
			// counted once.
			replaced = "none"
		}
		w.metrics.Add(metrics.CodeChecksFinished, 1, metrics.Attrs{"state": string(res.Row.State),
			"placement": placement, "replaced": replaced})
	}
}

// claimOutcome is what became of claim s, as crewlet.code.checks.claims names
// it: `won` for a claim whose record applied and whose row its runner holds;
// `lost` for one whose record applied and whose row another holds — a claim
// another writer published landed first, and the join kept that one; a
// refusal's own code — `claimed` above all; `unchanged` for the holder's own
// claim sent again; `handed_on` for one this node decided nothing of about
// itself, which the router asks the repository's runner-up; `failed` for any
// other error; and otherwise its outcome — pending or unknown.
func claimOutcome(s CheckStarted, res ChecksResult, err error) string {
	switch {
	case errors.Is(err, ErrNotServing):
		return "handed_on"
	case err != nil:
		return "failed"
	case res.Refusal != "":
		return string(res.Refusal)
	case res.Unchanged:
		return "unchanged"
	case res.Outcome == statelog.OutcomeApplied && res.Row != nil:
		if sameClaim(*res.Row, s) {
			return "won"
		}
		return "lost"
	}
	return string(res.Outcome)
}
