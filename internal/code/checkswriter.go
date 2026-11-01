package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
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
// reason; and it is refused busy AT ONCE past any of three bounds, the change
// writer's too (flight.go): [MaxCheckWritesWaiting] writes of its own
// repository, [ChecksWriterNodeWrites] across every repository — the writer's
// share of the node's proposal lane — and [ChecksWriterNodeBytes] of them.
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
//     it dispatched under it, with the attempts it queues at them
//     (checksdispatch.go) — decides each attempt by the arithmetic above in
//     its one snapshot, dropping from its record every one the rows hold or
//     refuse; it is answered from the rows, unchanged, where none is left to
//     queue and the target's reading and every head's dispatch already say
//     it, at its tip or a later one and naming its file, and refused on an
//     archived repository as an attempt queued there is.
//
// # Its scope names what its decide reads
//
// A check's record WRITES only its repository's checks, and its decide READS
// more: the repository's row — archived, deleted, its owner and visibility,
// which a seat's rerun is judged by — on every arm; the changes a dispatch
// queues at; and the change a rerun's authors or a failed head's routing are
// read from. A node holding a record it cannot decode that rewrote any of
// those holds them stale, and a write decided from them there — a rerun
// admitted on a revoked grant, a dispatch queuing a head a deferred retarget
// moved — must be answered unavailable, so the router asks another node
// (M5-D33, as the change writer's and the queue's records do). So every
// record names [RepoPath] beside [ChecksPath], and a queue or a dispatch the
// [ChangePath] of every change it queues at ([checksScope]); a change the
// decide FINDS in its snapshot — a rerun's head change, a failed head's —
// it probes itself and names on its record ([checksDecision.changeRead]),
// since no request could name it in advance. THE ENVELOPE CARRIES IT: a
// scope is readable at every version, so no versionedFields row follows.
//
// A rerun and a report — a principal's request, a seat's or a person's — are
// decided by the asker's standing (checkasks.go): a seat's rerun from the
// deciding node's chart ([ChecksWriterOptions.SeatRules]), which a node
// holding none yet, or one confirmed behind the company's configuration
// ([ChecksWriterOptions.Current]), answers [ErrNotServing] — the router asks
// the repository's runner-up under the same operation id, as the committer
// refuses a seat's push there. A principal's request is only ever one of
// those two arms: a queue, a claim or an ending naming a principal is no
// request this writer decides ([ErrInvalidProposal]).
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
	// AgentIDs resolves a seat's handle to the agent id its day is counted
	// under, as the committer's does ([CommitterOptions.AgentIDs]). REQUIRED
	// with Events: an ending that fails a change's head is counted on its
	// owner's seat day by that seat's agent id, which only the chart says.
	AgentIDs func(handle string) (string, bool)
	// Now is the decide's clock — a record's authored instant; nil takes the
	// wall clock in UTC.
	Now func() time.Time
	// SeatRules is the deciding node's epoch's chart, which a seat's rerun
	// is judged by ([ChecksRequest.JudgesChart]) — its grant on the
	// repository, and whoever its turn works for — read once a write. False
	// where this node has installed no epoch yet: such a write is then
	// answered [ErrNotServing]. REQUIRED: a seat's grant is never judged by a
	// default.
	SeatRules func() (SeatRules, bool)
	// Current is whether this node serves the company's configuration,
	// false exactly while it is CONFIRMED behind it, and nil is current;
	// while it answers false every write the chart judges is answered
	// [ErrNotServing], and the engine's own as ever.
	Current func() bool
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

// ChecksWriter is this node's writer of repositories' checks — one write of a
// repository's checks in flight at a time, and repositories side by side.
type ChecksWriter struct {
	pub       Publisher
	db        store.ReplicatedReader
	nodeID    string
	retention func() time.Duration
	seatRules func() (SeatRules, bool)
	current   func() bool
	events    EventPublisher
	agentIDs  func(handle string) (string, bool)
	now       func() time.Time
	metrics   *metrics.Recorder
	log       *slog.Logger

	// flights is each repository's single flight, the three bounds a write is
	// admitted under, and the publishes' own lifetime (flight.go).
	flights *flights
}

// checkFlights is what a checks writer's single flights admit.
var checkFlights = flightBounds{
	key: MaxCheckWritesWaiting, node: ChecksWriterNodeWrites, bytes: ChecksWriterNodeBytes,
	keyBound: "MaxCheckWritesWaiting", nodeBound: "ChecksWriterNodeWrites", bytesBound: "ChecksWriterNodeBytes",
	noun: "repository", writes: "check writes", closed: stoppingChecks,
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
	case o.SeatRules == nil:
		return nil, errors.New("code: a checks writer needs the epoch's chart (SeatRules) — a seat's rerun is " +
			"judged by its grant on the repository, never by a default")
	case o.Events != nil && o.AgentIDs == nil:
		return nil, errors.New("code: a checks writer that announces on an event stream needs AgentIDs — an " +
			"ending that fails a change's head is counted on its owner's seat day by the owner's agent id, which " +
			"only the chart says")
	}
	w := &ChecksWriter{
		pub: o.Publisher, db: o.DB, nodeID: o.NodeID, retention: o.Retention, seatRules: o.SeatRules,
		current: o.Current, events: o.Events, agentIDs: o.AgentIDs, now: o.Now,
		metrics: o.Metrics, log: o.Logger, flights: newFlights(checkFlights),
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	if w.log == nil {
		w.log = logging.Get("code")
	}
	return w, nil
}

// Validate refuses a request no correct asker forms ([ErrInvalidProposal],
// naming the field): one setting none or more than one record, an operation
// id this engine does not accept, a floor on another log, a record its own
// Validate refuses, a dispatch of any attempt but a commit's first or a
// requeue of its first — and a queue, a claim or an ending naming a
// principal, whose writes are a rerun and a report alone (checkasks.go). An
// attempt past [MaxCheckAttempts] is no invalid request: its asker formed it
// from the attempts it read, and it is refused attempts_exhausted, from the
// rows.
func (r ChecksRequest) Validate() error {
	set := 0
	for _, isSet := range []bool{r.Queued != nil, r.Started != nil, r.Finished != nil, r.Dispatched != nil,
		r.Rerun != nil, r.Report != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: a write of a repository's checks is exactly one of an attempt queued, a claim, "+
			"an ending, a dispatch, a rerun and a report, and this one sets %d", ErrInvalidProposal, set)
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
	switch {
	case r.Rerun != nil:
		if err := r.Rerun.validate(r.Asker); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidProposal, err)
		}
		return nil
	case r.Report != nil:
		if err := r.Report.validate(r.Asker); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidProposal, err)
		}
		return nil
	case r.Asker != (Principal{}):
		return badField("asker", "the engine's own records name no asker: a principal's write is a rerun or "+
			"a report")
	}
	op, payload := r.record()
	switch p := payload.(type) {
	case CheckQueued:
		switch {
		case p.Reason != QueueDispatch && p.Reason != QueueRequeue:
			return badField("queued.reason", "%q: a rerun is a principal's, asked as one (rerun), and this "+
				"arm is the engine's own dispatch and requeue", p.Reason)
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
				"asked as one (report), and this arm is the engine's own claims")
		}
	case CheckFinished:
		if p.Reporter != nil {
			return badField("finished.reporter", "a reported ending is a principal's, asked as a report, "+
				"and this arm is the engine's own endings")
		}
	}
	if err := payload.(interface{ Validate() error }).Validate(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidProposal, op, err)
	}
	return nil
}

// JudgesChart reports whether r's decide reads the chart: a seat's rerun,
// judged by its grant and by whoever its turn works for. A person's rerun
// and report, and the engine's own records, consult none — so a node behind
// the company's configuration decides them as a current one would.
func (r ChecksRequest) JudgesChart() bool { return r.Rerun != nil && r.Asker.Kind.readsChart() }

// op is the op r's record is: the engine's own record's, or the one a
// principal's write decides into — a rerun's queue, a report's claim or
// ending.
func (r ChecksRequest) op() OpKind {
	switch {
	case r.Rerun != nil:
		return OpCheckQueued
	case r.Report != nil:
		op, _ := r.Report.record(r.Asker, time.Time{})
		return op
	}
	op, _ := r.record()
	return op
}

// record is the op and the payload r carries — "" and nil for a request that
// carries none, or a principal's write, whose payload its decide forms.
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
	case r.Rerun != nil:
		return r.Rerun.CheckKey
	case r.Report != nil:
		return r.Report.CheckKey
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
// of it: a write past one of the writer's three bounds — its repository's
// check writes ([MaxCheckWritesWaiting]), every repository's
// ([ChecksWriterNodeWrites]) and their bytes ([ChecksWriterNodeBytes]) — is
// answered busy undecided at once; one whose asker's DEADLINE leaves too
// little for a publish and its answer is answered busy undecided — at once, or
// the moment that is so while it waits for its repository's turn — and one
// whose asker has none waits at most a CommitBudget; a stopping writer answers
// [ErrNotServing]; a request that is not one answers [ErrInvalidProposal]. ctx
// bounds only this caller's wait.
//
// ITS OPERATION ID IS THE WRITE'S OWN, so a write its asker hands the
// repository's runner-up after this node went silent is the same write there,
// collapsed by the ledger onto the record that landed. Applied is answered
// FROM THE ROWS the record wrote, never from the decision.
func (w *ChecksWriter) Publish(ctx context.Context, r ChecksRequest) (ChecksResult, error) {
	weight, err := r.weight()
	if err != nil {
		w.count(r, ChecksResult{}, err, checksLanded{})
		return ChecksResult{}, err
	}
	res, err, counted := w.publish(ctx, r, weight)
	if !counted {
		w.count(r, res, err, checksLanded{})
	}
	return res, err
}

// weight validates r ([ChecksRequest.Validate]) and answers what it weighs in
// its writer's node bytes: the bytes its record's payload encodes to — or, for
// a principal's rerun or report, which carries no record until its decide forms
// one, the bytes of what it asks, which that record carries.
func (r ChecksRequest) weight() (int, error) {
	if err := r.Validate(); err != nil {
		return 0, err
	}
	op, payload := r.record()
	switch {
	case r.Rerun != nil:
		op, payload = r.op(), *r.Rerun
	case r.Report != nil:
		op, payload = r.op(), *r.Report
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: encode it: %w", ErrInvalidProposal, op, err)
	}
	return len(encoded), nil
}

// stoppingChecks is the refusal of a write a stopping writer decides nothing
// of.
var stoppingChecks = fmt.Errorf("%w: the checks writer is stopping", ErrNotServing)

// checksAgain is what a write refused busy or unavailable is told to do.
const checksAgain = "send the write again"

// publish is [ChecksWriter.Publish] once the request is known to be one,
// weighing weight, and whether its outcome is counted already — a publish
// counts its own, since it ends whether or not its asker still waits.
func (w *ChecksWriter) publish(ctx context.Context, r ChecksRequest, weight int) (ChecksResult, error, bool) {
	hold, busy, err := w.flights.take(ctx, r.RepoID(), weight)
	switch {
	case err != nil:
		return ChecksResult{}, err, false
	case busy != "":
		return busyChecks(busy), nil, false
	}
	// A WRITE THE CHART JUDGES is decided only by a node that can judge it:
	// one current to the company's configuration, holding an epoch's chart,
	// read once a write.
	var seats SeatRules
	if r.JudgesChart() {
		var held bool
		switch seats, held = w.seatRules(); {
		case w.current != nil && !w.current():
			hold.release()
			return ChecksResult{}, fmt.Errorf("%w: this node is behind the company's configuration, and a "+
				"seat's rerun is judged by its chart; the repository's next committing node decides it",
				ErrNotServing), false
		case !held:
			hold.release()
			return ChecksResult{}, fmt.Errorf("%w: this node holds no chart yet to judge the write by; the "+
				"repository's next committing node decides it", ErrNotServing), false
		}
	}
	if !w.flights.begin() {
		hold.release()
		return ChecksResult{}, stoppingChecks, false
	}
	type answer struct {
		res ChecksResult
		err error
	}
	done := make(chan answer, 1)
	//nolint:contextcheck // The publish is the writer's, not this caller's: it runs under the writer's own lifetime, so an asker that hangs up mid-publish never leaves the repository's next check write deciding beside a record still in flight.
	go func() {
		defer w.flights.done()
		res, err, own := w.write(r, seats)
		// THE TURN IS RELEASED ONCE THE ANSWER IS KNOWN — the record applied
		// here, or the wait for it gave up — and before the announcement,
		// which no write of the repository waits on.
		hold.release()
		w.count(r, res, err, own)
		done <- answer{res, err}
		for _, e := range own.endings {
			w.announce(e)
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

// busyChecks is a write refused busy, undecided, saying detail.
func busyChecks(detail string) ChecksResult {
	return ChecksResult{Refusal: CheckRefusalBusy, Detail: detail}
}

// Close refuses every new write [ErrNotServing], and every one waiting for its
// repository's turn, leaving the publishes in flight to run — the first half of
// [ChecksWriter.Stop], which a teardown stopping several writers under one bound
// takes for all of them before it drains any (internal/engine's
// stopTogether). Safe to call more than once.
func (w *ChecksWriter) Close() { w.flights.close() }

// Stop refuses every new write [ErrNotServing], and every one waiting for its
// repository's turn — a node that decided nothing, so the router asks the
// runner-up — and waits for the publishes in flight, which are FINISHED rather
// than abandoned so their askers get their outcome. If ctx ends first, they
// are cancelled and answered from what their publish returned. Safe to call
// more than once.
func (w *ChecksWriter) Stop(ctx context.Context) { w.flights.stop(ctx) }

// checksDecision is what one round of a write's decide concluded: a payload
// to append, or none — refused, or unchanged — with the row it read.
type checksDecision struct {
	payload   any
	refusal   CheckRefusal
	detail    string
	unchanged bool
	// row is the attempt as the snapshot held it, nil for none.
	row *CheckRow
	// dropped is, for a dispatch, why each attempt it carried that the rows
	// refused was left out of its record (decideDispatched).
	dropped []string
	// reads are the paths of the changes the decide found in its snapshot
	// and read ([checksDecision.changeRead]), which its record names beside
	// the request's scope.
	reads []string
}

// checksScope is the scope a write of r is published under: [RepoPath] and
// [ChecksPath] of its repository — every arm's decide reads the repository's
// row — and the [ChangePath] of each change it queues at, a dispatch's and an
// attempt queued for a change's head, whose head, target and liveness the
// home read to queue it; past [MaxScopePaths], the whole domain, which covers
// them all (the queue's land rule, queue.go). See the file's doc.
func checksScope(r ChecksRequest) []string {
	repoID := r.RepoID()
	scope := []string{RepoPath(repoID), ChecksPath(repoID)}
	var changes []string
	switch {
	case r.Queued != nil:
		changes = append(changes, r.Queued.ChangeID)
	case r.Dispatched != nil:
		for _, h := range r.Dispatched.Heads {
			changes = append(changes, h.ChangeID)
		}
		for _, a := range r.Dispatched.Attempts {
			changes = append(changes, a.ChangeID)
		}
	}
	changes = slices.DeleteFunc(changes, func(id string) bool { return id == "" })
	slices.Sort(changes)
	changes = slices.Compact(changes)
	if len(scope)+len(changes) > MaxScopePaths {
		return []string{DomainPath()}
	}
	for _, id := range changes {
		scope = append(scope, ChangePath(id))
	}
	return scope
}

// scope is the scope d's record names: the request's, and every change d's
// decide found and read — the whole domain past [MaxScopePaths].
func (d checksDecision) scope(requested []string) []string {
	if len(d.reads) == 0 {
		return requested
	}
	scope := slices.Clone(requested)
	for _, p := range d.reads {
		if !slices.Contains(scope, p) {
			scope = append(scope, p)
		}
	}
	if len(scope) > MaxScopePaths || slices.Contains(requested, DomainPath()) {
		return []string{DomainPath()}
	}
	return scope
}

// changeRead is the guard a decide passes before it reads change's rows,
// having FOUND the change in its own snapshot — a rerun's head change, a
// failed head's — where no request could name it for the framework's probe
// ([statelog.Snap]): a record this node holds and cannot decode whose scope
// covers the change refuses the write unavailable, the framework's own
// `deferred`, so the router asks a node whose rows are current; otherwise the
// change's path joins d's record's scope. The iam domain's wholeDirectory
// precedent, over one change.
func (d *checksDecision) changeRead(ctx context.Context, tx *sql.Tx, change string) error {
	path := ChangePath(change)
	deferral, hit, err := statelog.DeferredIn(ctx, tx, Domain{}, statelog.ScopeSet{Paths: []string{path}})
	if err != nil {
		return fmt.Errorf("code: ask whether this node holds a record about change %s it cannot decode: %w",
			change, err)
	}
	if hit {
		return &statelog.Unavailable{
			Reason:   statelog.ReasonDeferred,
			Position: deferral.Position,
			Detail: fmt.Sprintf("this node holds a record at version %d it cannot decode, at %s, whose scope "+
				"covers change %s — a check's write reads that change's rows, which are stale here, and "+
				"another node can serve it", deferral.Version, deferral.Position, change),
		}
	}
	if !slices.Contains(d.reads, path) {
		d.reads = append(d.reads, path)
	}
	return nil
}

// checksLanded is what a write's answer says of its OWN record, which the
// result alone cannot: whether this call's record applied here — never a
// retry the ledger collapsed onto an earlier landing, whose call counted and
// announced it — the endings it announces, and, for a dispatch, the purpose
// of each attempt its record queued to run, which a single queue's request
// and row say themselves.
type checksLanded struct {
	fresh   bool
	endings []*types.CodeCheckFinished
	queued  []CheckPurpose
}

// write publishes one write of a repository's checks and answers it — under
// the writer's own lifetime and one CommitBudget — with what landed of its
// own record.
func (w *ChecksWriter) write(r ChecksRequest, seats SeatRules) (ChecksResult, error, checksLanded) {
	ctx, cancel := context.WithTimeout(w.flights.life, CommitBudget)
	defer cancel()
	op := r.op()
	retention := w.retention()
	repoID := r.RepoID()
	subject := ChecksSubject(repoID)
	scope := checksScope(r)
	var (
		last    checksDecision
		decided bool
	)
	res, err := w.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: r.OpID, Pattern: statelog.PatternAdditive, Session: r.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			last, decided = checksDecision{}, false
			now := w.now()
			d, err := decideChecks(ctx, tx, r, retention, seats, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.payload == nil {
				return statelog.Decision{}, nil
			}
			return recordFor(stamp, subject, op, d.scope(scope), r.OpID, Actor{ID: w.nodeID, Kind: ActorSystem},
				d.payload, now)
		},
	})
	if err != nil {
		res, err := w.refused(ctx, r, err)
		return res, err, checksLanded{}
	}
	if decided && len(last.dropped) > 0 {
		w.log.DebugContext(ctx, "code_check_dispatch_attempts_dropped", "repo", repoID, "op_id", r.OpID,
			"dropped", last.dropped, "detail", "the rows answered these attempts; the dispatch went on without them")
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() && r.Dispatched != nil {
			var attempts []CheckQueued
			if p, ok := last.payload.(CheckDispatched); decided && ok {
				attempts = p.Attempts
			}
			return w.dispatchApplied(ctx, r, res, attempts)
		}
		if !res.Position.IsZero() {
			// AN ENDING THE ROW ALREADY KEPT, which this one's decide chose
			// to replace — a measurement over an infrastructure ending — is
			// what the announcement names it a correction of.
			var replacing CheckState
			if decided && (r.Finished != nil || op == OpCheckFinished) && last.row != nil &&
				!last.row.Finished.IsZero() && last.row.Attempt == r.Key().Attempt {
				replacing = last.row.State
			}
			// WHOM THE RECORD TELLS, which names the change a person's CI's
			// failure is of: its row names none.
			var told *ChangeRouting
			if f, ok := last.payload.(CheckFinished); decided && ok {
				told = f.Routing
			}
			return w.applied(ctx, r, res, replacing, told)
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
func decideChecks(ctx context.Context, tx *sql.Tx, r ChecksRequest, retention time.Duration, seats SeatRules,
	now time.Time) (checksDecision, error) {

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
	case (r.Queued != nil || r.Dispatched != nil || r.Rerun != nil || r.Report != nil) &&
		repo.status == RepoStatusArchived:
		return d.refuse(CheckRefusalRepoArchived, fmt.Sprintf("repository %s is archived and read-only "+
			"(code.repos archived: true): nothing moves in it to be checked", repo.name)), nil
	}
	named := func(k CheckKey) string {
		return fmt.Sprintf("attempt %d of check %s of %s for %s on %s", k.Attempt, k.Name, k.OID, k.Target,
			repo.name)
	}
	switch {
	case r.Dispatched != nil:
		return decideDispatched(ctx, tx, *r.Dispatched, d, named)
	case r.Rerun != nil:
		return decideRerun(ctx, tx, *r.Rerun, r.Asker, repo, seats, d, named, now)
	case r.Report != nil:
		d, err := decideReport(ctx, tx, *r.Report, r.Asker, d, named, now)
		if err != nil || d.payload == nil {
			return d, err
		}
		// A PERSON'S CI's FAILURE of a change's head gates its merge as the
		// engine's own does (the gate reads every row of the head), so it
		// tells the change's authors as the engine's own does, the person
		// whose CI reported it its actor.
		reporter := r.Asker
		return routeFailedHead(ctx, tx, repo.name, d, &reporter)
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
	d, err = decideFinished(*r.Finished, d, held, named)
	if err != nil || d.payload == nil {
		return d, err
	}
	return routeFailedHead(ctx, tx, repo.name, d, nil)
}

// routeFailedHead stamps whom an ending d records concerns: a check of a live
// change's CURRENT head that failed, or errored past the home's own requeue,
// tells the change's authors and the record's actor — the one reason that is
// news to its actor too ([Reason.WakesActor]). Every other ending concerns
// nobody: a passed check is what readiness reads (the home's next
// mergeability record announces a change ready, never a check record:
// concerns.go's head), a requeued one is not over, and a check of a head the
// change has moved past measures code nobody is asked to fix.
//
// KEYED ON THE CHANGE AND ITS HEAD, never on the row's queue purpose
// ([headChangeIn]): the engine's runner ends a head check queued for one
// change, and a person's CI (reporter, nil for the runner's) reports onto a
// row that names no change at all — and either one failing leaves the gate's
// `check` term unmet. THE ACTOR is whoever made the failure news: the pusher
// of the head the runner measured, and the person whose CI reported it, told
// in their inbox as every person is. The routing's owner is also whose day
// the ending counts on ([ChecksWriter.applied]), so a failure is counted
// exactly where it is told.
func routeFailedHead(ctx context.Context, tx *sql.Tx, repoName string, d checksDecision,
	reporter *Principal) (checksDecision, error) {

	f, ok := d.payload.(CheckFinished)
	if !ok || !f.failsItsHead() {
		return d, nil
	}
	change, found, err := headChangeIn(ctx, tx, &d, f.CheckKey, d.row, reporter != nil)
	if err != nil || !found {
		return d, err
	}
	c := concerning{repoName: repoName, change: change, authors: true, checksFailed: true}
	if reporter != nil {
		c.actor, c.actorKind = reporter.party()
	} else if c.actor, c.actorKind, err = revisionPusher(ctx, tx, change.id, change.revision); err != nil {
		return checksDecision{}, err
	}
	if f.Routing, err = routingFor(ctx, tx, c); err != nil {
		return checksDecision{}, err
	}
	d.payload = f
	return d, nil
}

// headChangeIn is the live change whose CURRENT head an ending of the attempt
// keyed key measures, and false for none. An ending a person's CI reported
// (reported) is on a row that names no change, so it is the live change into
// key.Target whose head is key.OID — one seek of code_changes_target_idx
// ([liveChangeAtHeadSQL]), the first the index holds where two live changes
// share the head, as a head check the home queues names one change and its
// record carries one routing ([CheckFinished.Routing]). The engine's own ending is
// the change the head check row holds was queued for, still live and still at
// key.OID; a check of a tip, a candidate or a landing measures no change's
// head — a fast-forward candidate's commit can BE a change's head, and its
// failure ejects that change rather than telling its authors their head fails.
//
// THE CHANGE IS FOUND IN THE SNAPSHOT, so d's guard probes it before its rows
// are read ([checksDecision.changeRead]): a record this node cannot decode
// that closed, retargeted or moved it leaves them stale.
func headChangeIn(ctx context.Context, tx *sql.Tx, d *checksDecision, key CheckKey, row *CheckRow,
	reported bool) (changeRow, bool, error) {

	var (
		change changeRow
		found  bool
		err    error
	)
	switch {
	case reported:
		change, err = scanChange(tx.QueryRowContext(ctx, liveChangeAtHeadSQL, key.RepoID, key.Target,
			key.OID).Scan)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return changeRow{}, false, nil
		case err != nil:
			return changeRow{}, false, fmt.Errorf("code: read the live change into %s at %s: %w", key.Target,
				key.OID, err)
		}
		if err := d.changeRead(ctx, tx, change.id); err != nil {
			return changeRow{}, false, err
		}
		found = true
	case row != nil && row.Purpose == PurposeHead && row.ChangeID != "":
		if err := d.changeRead(ctx, tx, row.ChangeID); err != nil {
			return changeRow{}, false, err
		}
		if change, found, err = changeRowIn(ctx, tx, row.ChangeID); err != nil {
			return changeRow{}, false, err
		}
	}
	if !found || change.head != key.OID || !change.state.Live() {
		return changeRow{}, false, nil
	}
	return change, true, nil
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
// a second ending of the attempt. told is whom the record tells (its
// routing, nil for none: routeFailedHead).
func (w *ChecksWriter) applied(ctx context.Context, r ChecksRequest, res statelog.Result,
	replacing CheckState, told *ChangeRouting) (ChecksResult, error, checksLanded) {

	key, at := r.Key(), res.Position
	var (
		row       CheckRow
		held      bool
		repoName  string
		owner     Party
		readError error
	)
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		if row, held, err = checkRowIn(ctx, tx, key); err != nil || !held {
			return err
		}
		if repoName, err = announcedRepoName(ctx, tx, key.RepoID); err != nil {
			return err
		}
		// THE OWNER OF THE CHANGE WHOSE HEAD THE ENDING FAILS, whose day
		// counts it (types.CodeCheckFinished.Owner). Of a head check the
		// engine ran, the owner of the change it was queued for, whatever
		// that change did since (failedHeadOwnerIn). Of one a person's CI
		// reported, whose row names no change, the owner of the live change
		// at its head that the record told (routeFailedHead) — so a CI's
		// failure counts where it is told.
		switch {
		case !row.failsItsHead():
		case row.Purpose == PurposeHead && row.ChangeID != "":
			owner, err = failedHeadOwnerIn(ctx, tx, row)
			return err
		case row.Queued.IsZero() && told != nil:
			owner = Party{Handle: told.Owner, Kind: told.OwnerKind}
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
	case r.op() == OpCheckQueued:
		wrote = !row.Queued.IsZero()
	case r.op() == OpCheckStarted:
		wrote = row.Placement != ""
	case r.op() == OpCheckFinished:
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
	if (r.op() == OpCheckFinished && ours) || (r.Queued != nil && (r.Queued.Skipped || r.Queued.Invalid != "") &&
		row.State.Ended() && ours) {
		ending := w.endingOf(key, row, repoName, owner, replacing)
		if row.Queued.IsZero() && told != nil {
			// A REPORTED FAILURE'S ROW NAMES NO CHANGE: the event names the
			// one it was told of, whose owner it counts on.
			ending.ChangeID = told.ChangeID
		}
		own.endings = append(own.endings, ending)
	}
	return out, nil, own
}

// endingOf is the announcement of row's ending — the attempt key, of the
// repository named repoName — naming the owner of the change whose head it
// fails, and the ending it replaced, "" for none.
func (w *ChecksWriter) endingOf(key CheckKey, row CheckRow, repoName string, owner Party,
	replacing CheckState) *types.CodeCheckFinished {

	placement := row.Placement
	if row.Reporter != "" {
		// A REPORTED ENDING is a person's CI's, whether or not it reported
		// the run started first.
		placement = PlacementExternal
	}
	e := &types.CodeCheckFinished{RepoID: key.RepoID, RepoName: repoName, OID: key.OID, Name: key.Name,
		Attempt: key.Attempt, Purpose: string(row.Purpose), State: string(row.State), Reason: string(row.Reason),
		Placement: string(placement), ChangeID: row.ChangeID, Target: row.Target, Required: row.Required,
		DurationMs: row.DurationMs, Replaces: string(replacing), Owner: owner.Handle,
		OwnerKind: string(owner.Kind)}
	e.Agent = agentOf(w.agentIDs, owner.Handle, owner.Kind)
	return e
}

// announcedRepoName is the name an announcement gives the repository repoID:
// its name, or its id where the rows hold none — a purge reached it.
func announcedRepoName(ctx context.Context, tx *sql.Tx, repoID string) (string, error) {
	name, found, err := repoNameIn(ctx, tx, repoID)
	if err != nil || !found {
		return repoID, err
	}
	return name, nil
}

// failedHeadOwnerIn is THE OWNER OF THE CHANGE WHOSE HEAD row's ending fails,
// whose day counts it (types.CodeCheckFinished.Owner) — one seek of the
// changes' key, for that ending alone — and nobody for any other row.
func failedHeadOwnerIn(ctx context.Context, tx *sql.Tx, row CheckRow) (Party, error) {
	if row.Purpose != PurposeHead || row.ChangeID == "" || !row.failsItsHead() {
		return Party{}, nil
	}
	change, found, err := changeRowIn(ctx, tx, row.ChangeID)
	if err != nil || !found {
		return Party{}, err
	}
	return Party{Handle: change.owner, Kind: change.ownerKind}, nil
}

// dispatchApplied answers a dispatch whose record applied at res.Position,
// carrying attempts as its decide formed it, FROM THE ROWS: its target's
// reading, which the record — or one that read a later tip — wrote. A target
// the rows hold no reading of is a record that applied and wrote nothing this
// node holds — the repository purged under it — logged at ERROR and answered
// unknown, never applied. For this call's own record it reads, beside the
// reading, each attempt's row: one the record queued to run is counted, and
// one it recorded ended — skipped, or errored for a file that does not run —
// is announced, as a single queue's would be, where the row keeps it.
func (w *ChecksWriter) dispatchApplied(ctx context.Context, r ChecksRequest, res statelog.Result,
	attempts []CheckQueued) (ChecksResult, error, checksLanded) {

	d := r.Dispatched
	var held bool
	own := checksLanded{fresh: !res.Collapsed}
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
		if !own.fresh || len(attempts) == 0 {
			return nil
		}
		repoName, err := announcedRepoName(ctx, tx, d.RepoID)
		if err != nil {
			return err
		}
		at := res.Position.Packed()
		for _, a := range attempts {
			row, found, err := checkRowIn(ctx, tx, a.CheckKey)
			switch {
			case err != nil:
				return err
			case !found:
				continue
			case a.Skipped || a.Invalid != "":
				// THE ENDING ANNOUNCED IS THE ONE THE ROW KEEPS, as a
				// single queue's.
				if !row.State.Ended() || row.Finished.Packed() != at {
					continue
				}
				owner, err := failedHeadOwnerIn(ctx, tx, row)
				if err != nil {
					return err
				}
				own.endings = append(own.endings, w.endingOf(a.CheckKey, row, repoName, owner, ""))
			case row.Queued.Packed() == at:
				own.queued = append(own.queued, row.Purpose)
			}
		}
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
	return ChecksResult{Outcome: statelog.OutcomeApplied, Position: res.Position}, nil, own
}

// announce publishes an applied ending's event on this node's event stream,
// bounded by [checkEventBudget] under the writer's own lifetime: a stream
// that does not take it is logged, and the attempt's row still says it ended.
func (w *ChecksWriter) announce(e *types.CodeCheckFinished) {
	if w.events == nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.flights.life, checkEventBudget)
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
	if err != nil || !own.fresh {
		return
	}
	// A DISPATCH'S OWN RECORD, which answers no one attempt's row: what it
	// queued and ended is own's, read from the rows it wrote.
	for _, purpose := range own.queued {
		w.metrics.Add(metrics.CodeChecksQueued, 1, metrics.Attrs{"purpose": string(purpose)})
	}
	if res.Row != nil && r.Queued != nil && !r.Queued.Skipped && r.Queued.Invalid == "" {
		w.metrics.Add(metrics.CodeChecksQueued, 1, metrics.Attrs{"purpose": string(r.Queued.Purpose)})
	}
	if res.Row != nil && r.Rerun != nil && !res.Unchanged {
		w.metrics.Add(metrics.CodeChecksQueued, 1, metrics.Attrs{"purpose": string(res.Row.Purpose)})
	}
	for _, e := range own.endings {
		// THE PLACEMENT ITS ANNOUNCEMENT NAMES: a reported ending is a
		// person's CI's, external, whether or not it reported its run started.
		placement, replaced := e.Placement, e.Replaces
		if placement == "" {
			placement = "none"
		}
		if replaced == "" {
			// AN ATTEMPT'S FIRST ENDING; a replacement is counted apart,
			// under the ending it replaced, so the attempts that ended are
			// counted once.
			replaced = "none"
		}
		w.metrics.Add(metrics.CodeChecksFinished, 1, metrics.Attrs{"state": e.State, "placement": placement,
			"replaced": replaced})
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
