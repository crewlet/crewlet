package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CHANGE WRITER: a change's conversation, published on the change's own
// subject by the repository's committing data node.
//
// A record of a change's conversation ([ConversationOps]) arbitrates on the
// CHANGE ([KindChange]), never on its repository: a comment shares nothing with
// the pushes that move the change's branches, and decided in the committer's
// queue it would wait behind their batches. So the writer is the committer's
// sibling on the same node — the estate asks a conversation write of the
// repository's committing data node, keyed by the repository as a push is
// (internal/estate's code.conversation) — and it shares none of the committer's
// queue, budget or batches: each write is a record of its own, under the
// asker's operation id, decided in one snapshot and published under
// [statelog.PatternArbitrated] with the asker's Seen as its session floor.
//
// # One write of a change in flight at a time
//
// Two writes of one change published at once from one node read one anchor,
// and one of them loses a compare-and-set round to the other and decides
// again — a review lost to a comment beside it, sixteen rounds of losing for a
// thread a burst of comments lands in. So the writer takes ONE CHANGE'S WRITES
// ONE AT A TIME — a SINGLE FLIGHT a change, held from the decide's first round
// to its answer — and writes of different changes side by side (flight.go). A
// write waits for its turn while its asker's attempt leaves room for a publish
// and its answer ([CommitBudget], [ReplyMargin]), and is refused busy,
// undecided, the moment it does not — the committer's rule for a push's wait in
// its queue, and for its reason: past it the asker hands the same operation id
// to the repository's runner-up, which then contends with this node on the
// change's subject. Two nodes writing one change — a runner-up asked while the
// committing node answered slowly — still meet at the broker, where exactly one
// record lands a round.
//
// # Bounded as the committer is
//
// A write holding or waiting for its change's turn holds the estate's answer
// slot on the node's proposal lane, which pushes are answered on too, and its
// decoded request — a review up to [MaxConversationBytes]. So it is refused
// busy AT ONCE, undecided, past any of three bounds, the committer's rule for a
// proposal: [MaxChangeWritesWaiting] writes of its own change, a burst written
// faster than the change's single flight lands them or queued behind a publish
// an election holds; [ChangeWriterNodeWrites] writes across every change, the
// writer's share of the proposal lane; and [ChangeWriterNodeBytes] of them,
// weighed by what each record carries.
//
// # The publish is the writer's
//
// A write's publish runs under the writer's own lifetime and one CommitBudget,
// never its asker's context: an asker that hangs up mid-publish stops waiting,
// and the record may land — its answer is then the rows, which the asker's
// retry under the same operation id collapses onto. [ChangeWriter.Stop] refuses
// every new write and every one still waiting for its turn [ErrNotServing], so
// the router asks the runner-up, and waits for the publishes in flight.
//
// # A seat's write is decided only by a node that can judge it
//
// A write the chart judges ([ConversationWrite.JudgesChart]) — a seat's, and an
// ask of reviewers by anybody — is refused [ErrNotServing] by a node confirmed
// behind the company's configuration ([ChangeWriterOptions.Current]) and by one
// holding no chart yet ([ChangeWriterOptions.SeatRules]), as the committer
// refuses a seat's push there: the router asks the repository's runner-up under
// the same operation id. A person's comment or resolution is decided as ever.

// ChangeWriterOptions configure a change writer.
type ChangeWriterOptions struct {
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
	// SeatRules is the deciding node's epoch's chart, which a write that
	// [ConversationWrite.JudgesChart] is judged by — a seat's grant on the
	// repository, and every reviewer an ask names — read once a write. False
	// where this node has installed no epoch yet: such a write is then
	// answered [ErrNotServing], so the router hands it to the repository's
	// runner-up under the same operation id. REQUIRED: a reviewer is never
	// judged by a default.
	SeatRules func() (SeatRules, bool)
	// Current is whether this node serves the company's configuration (the
	// engine's posture gate), false exactly while it is CONFIRMED behind it,
	// and nil is current. While it answers false every write the chart judges
	// is answered [ErrNotServing], as it is queued and again once its turn
	// comes; a person's comment or resolution is decided as ever.
	Current func() bool
	// Now is the decide's clock — a record's authored instant; nil takes the
	// wall clock in UTC.
	Now func() time.Time
	// Events is this node's event stream, which a review whose record this
	// writer landed is announced on (`review_submitted`, announce.go); nil
	// announces none — a node with no queue records no event of any kind.
	Events EventPublisher
	// AgentIDs resolves a seat's handle to the agent id its day is counted
	// under, as the committer's does ([CommitterOptions.AgentIDs]).
	// REQUIRED with Events: a review announced without its reviewer's and
	// its change owner's ids is counted on no seat's day.
	AgentIDs func(handle string) (string, bool)
	// Metrics records every write's outcome; nil records nothing.
	Metrics *metrics.Recorder
	// Logger is where a failed write is reported; nil is the package's.
	Logger *slog.Logger
}

// ChangeWriter is this node's writer of changes' conversations — one write of
// a change in flight at a time, and changes side by side.
type ChangeWriter struct {
	pub       Publisher
	db        store.ReplicatedReader
	nodeID    string
	retention func() time.Duration
	seatRules func() (SeatRules, bool)
	current   func() bool
	now       func() time.Time
	events    EventPublisher
	agentIDs  func(string) (string, bool)
	metrics   *metrics.Recorder
	log       *slog.Logger

	// flights is each change's single flight, the three bounds a write is
	// admitted under, and the publishes' own lifetime (flight.go).
	flights *flights
}

// changeFlights is what a change writer's single flights admit.
var changeFlights = flightBounds{
	key: MaxChangeWritesWaiting, node: ChangeWriterNodeWrites, bytes: ChangeWriterNodeBytes,
	keyBound: "MaxChangeWritesWaiting", nodeBound: "ChangeWriterNodeWrites", bytesBound: "ChangeWriterNodeBytes",
	noun: "change", writes: "writes", closed: stopping,
}

// NewChangeWriter builds a change writer, refusing what it cannot work without.
func NewChangeWriter(o ChangeWriterOptions) (*ChangeWriter, error) {
	switch {
	case o.Publisher == nil:
		return nil, errors.New("code: a change writer needs the code log's write authority — every write " +
			"of a change's conversation is a record on that log")
	case o.DB.IsZero():
		return nil, errors.New("code: a change writer needs the replicated estate a write's answer is read " +
			"from — an applied write is only ever reported from its rows")
	case !config.ValidNodeID(o.NodeID):
		return nil, fmt.Errorf("code: a change writer needs the deciding node's id, which every record names "+
			"as its actor; %q is not one config admits", o.NodeID)
	case o.Retention == nil:
		return nil, errors.New("code: a change writer needs the epoch's code.deleted_retention_days — a write " +
			"to a deleted repository is told when its purge may begin, which a default would misstate")
	case o.SeatRules == nil:
		return nil, errors.New("code: a change writer needs the epoch's chart (SeatRules) — a seat's write and " +
			"every reviewer an ask names are judged by the chart, never by a default")
	case o.Events != nil && o.AgentIDs == nil:
		return nil, errors.New("code: a change writer that announces on an event stream needs AgentIDs — a " +
			"review is counted on its reviewer's and its change owner's seat days by their agent ids, which only " +
			"the chart says")
	}
	w := &ChangeWriter{
		pub: o.Publisher, db: o.DB, nodeID: o.NodeID, retention: o.Retention, seatRules: o.SeatRules,
		current: o.Current, now: o.Now, events: o.Events, agentIDs: o.AgentIDs, metrics: o.Metrics,
		log: o.Logger, flights: newFlights(changeFlights),
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	if w.log == nil {
		w.log = logging.Get("code")
	}
	return w, nil
}

// Converse decides one write of a change's conversation and answers what
// became of it: a write past one of the writer's three bounds — its change's
// writes ([MaxChangeWritesWaiting]), every change's
// ([ChangeWriterNodeWrites]) and their bytes ([ChangeWriterNodeBytes]) — is
// answered busy undecided at once; one whose asker's DEADLINE leaves too
// little for a publish and its answer ([CommitBudget] and [ReplyMargin]) is
// answered busy undecided — at once, or the moment that is so while it waits
// for its change's turn — and one whose asker has none waits at most a
// CommitBudget; a stopping writer answers [ErrNotServing], and so does one
// that cannot judge a write the chart judges — behind the company's
// configuration, or holding no chart yet; a write that is not one answers
// [ErrInvalidProposal]. ctx bounds only this caller's wait.
//
// ITS OPERATION ID IS THE WRITE'S OWN, so a write its asker hands the
// repository's runner-up after this node went silent is the same write there,
// collapsed by the ledger onto the record that landed — a comment onto the
// comment it said, never a second. Applied is answered FROM THE ROWS the
// record wrote, never from the decision.
func (w *ChangeWriter) Converse(ctx context.Context, cw ConversationWrite) (ConversationResult, error) {
	weight, err := cw.measure()
	if err != nil {
		w.count(cw, ConversationResult{}, err)
		return ConversationResult{}, err
	}
	res, err, counted := w.converse(ctx, cw, weight)
	if !counted {
		w.count(cw, res, err)
	}
	return res, err
}

// stopping is the refusal of a write a stopping writer decides nothing of.
var stopping = fmt.Errorf("%w: the change writer is stopping", ErrNotServing)

// converse is [ChangeWriter.Converse] once the write is known to be one,
// weighing weight, and whether its outcome is counted already — a publish
// counts its own, since it ends whether or not its asker still waits.
func (w *ChangeWriter) converse(ctx context.Context, cw ConversationWrite, weight int) (ConversationResult, error,
	bool) {

	if why := w.behindFor(cw); why != "" {
		return ConversationResult{}, fmt.Errorf("%w: %s", ErrNotServing, why), false
	}
	hold, busy, err := w.flights.take(ctx, cw.ChangeID(), weight)
	switch {
	case err != nil:
		return ConversationResult{}, err, false
	case busy != "":
		return busyConversation(busy), nil, false
	}
	// ITS TURN: judged again, since the revision this node failed to apply
	// may be the one its wait spanned, and its chart read once.
	if why := w.behindFor(cw); why != "" {
		hold.release()
		return ConversationResult{}, fmt.Errorf("%w: %s", ErrNotServing, why), false
	}
	var seats SeatRules
	if cw.JudgesChart() {
		var held bool
		if seats, held = w.seatRules(); !held {
			hold.release()
			return ConversationResult{}, fmt.Errorf("%w: this node holds no chart yet to judge the write by; "+
				"the repository's next committing node decides it", ErrNotServing), false
		}
	}
	if !w.flights.begin() {
		hold.release()
		return ConversationResult{}, stopping, false
	}
	type answer struct {
		res ConversationResult
		err error
	}
	done := make(chan answer, 1)
	//nolint:contextcheck // The publish is the writer's, not this caller's: it runs under the writer's own lifetime, so an asker that hangs up mid-publish never leaves the change's next write deciding beside a record still in flight.
	go func() {
		defer w.flights.done()
		res, err, fresh := w.write(cw, seats)
		// THE CHANGE'S NEXT WRITE WAITS ON NONE OF THE ANNOUNCEMENT: its
		// turn is released with the answer, and the announcement — a read
		// and a publish, bounded by checkEventBudget — runs after both,
		// still inside this publish's flight so Stop waits for it.
		hold.release()
		w.count(cw, res, err)
		done <- answer{res, err}
		if fresh {
			w.announceReview(cw)
		}
	}()
	select {
	case a := <-done:
		return a.res, a.err, true
	case <-ctx.Done():
		// AN ANSWER ALREADY GIVEN WINS over the asker's context ending,
		// for the committer's reason (Committer.await).
		select {
		case a := <-done:
			return a.res, a.err, true
		default:
		}
		return ConversationResult{}, ctx.Err(), true
	}
}

// behindFor is why this node may not decide cw now, or "": a write the chart
// judges, while the node is confirmed behind the company's configuration.
func (w *ChangeWriter) behindFor(cw ConversationWrite) string {
	if !cw.JudgesChart() || w.current == nil || w.current() {
		return ""
	}
	return "this node is behind the company's configuration and decides no write the chart judges from the " +
		"chart the fleet replaced; the repository's next committing node decides it"
}

// busyConversation is a write refused busy, undecided, saying detail.
func busyConversation(detail string) ConversationResult {
	return ConversationResult{Refusal: ConversationRefusalBusy, Detail: detail}
}

// Close refuses every new write [ErrNotServing], and every one waiting for its
// change's turn, leaving the publishes in flight to run — the first half of
// [ChangeWriter.Stop], which a teardown stopping several writers under one bound
// takes for all of them before it drains any (internal/engine's
// stopTogether). Safe to call more than once.
func (w *ChangeWriter) Close() { w.flights.close() }

// Stop refuses every new write [ErrNotServing], and every one waiting for its
// change's turn — a node that decided nothing, so the router asks the
// runner-up — and waits for the publishes in flight, which are FINISHED rather
// than abandoned so their askers get their outcome. If ctx ends first, they
// are cancelled and answered from what their publish returned. Safe to call
// more than once.
func (w *ChangeWriter) Stop(ctx context.Context) { w.flights.stop(ctx) }

// write publishes one write of a change's conversation and answers it — under
// the writer's own lifetime and one CommitBudget — and whether it is a review
// this call landed, which is announced once its answer is handed back
// (announce.go): never one answered unchanged, from no record, nor a retry the
// ledger collapsed onto an earlier landing, whose call announced it.
func (w *ChangeWriter) write(cw ConversationWrite, seats SeatRules) (ConversationResult, error, bool) {
	ctx, cancel := context.WithTimeout(w.flights.life, CommitBudget)
	defer cancel()
	op, _ := cw.record()
	retention := w.retention()
	repoID, changeID := cw.RepoID(), cw.ChangeID()
	subject := ChangeSubject(changeID)
	scope := []string{RepoPath(repoID), ChangePath(changeID)}
	if cw.Mergeability != nil {
		// A MERGEABILITY'S READINESS JUDGES ITS TARGET'S MERGE GATE, which
		// reads the repository's checks (gatejudge.go), and stamps the
		// position of its head's checks it read (Mergeability.ChecksPosition):
		// its scope names them beside the repository and the change, so a
		// node holding a deferred record of them decides none (M5-D33).
		scope = []string{RepoPath(repoID), ChecksPath(repoID), ChangePath(changeID)}
	}
	var (
		last    conversationDecision
		decided bool
	)
	res, err := w.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: cw.OpID, Pattern: statelog.PatternArbitrated, Session: cw.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET AT THE TOP OF EVERY ROUND: only the last round says
			// what the publish decided.
			last, decided = conversationDecision{}, false
			d, err := decideConversation(ctx, tx, cw, seats, retention)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.payload == nil {
				return statelog.Decision{Version: d.version}, nil
			}
			decision, err := recordFor(stamp, subject, op, scope, cw.OpID, Actor{ID: w.nodeID, Kind: ActorSystem},
				d.payload, w.now())
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := withinConversation(decision, d); err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = d.version
			return decision, nil
		},
	})
	out, answerErr := w.answer(ctx, cw, last, decided, res, err)
	fresh := cw.Review != nil && answerErr == nil && out.Outcome == statelog.OutcomeApplied && !out.Unchanged &&
		!res.Collapsed
	return out, answerErr, fresh
}

// answer is a write's answer from the publish's result and the last round's
// decision.
func (w *ChangeWriter) answer(ctx context.Context, cw ConversationWrite, last conversationDecision, decided bool,
	res statelog.Result, err error) (ConversationResult, error) {

	if err != nil {
		return w.refused(ctx, cw, err)
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() {
			return w.applied(ctx, cw, res.Position)
		}
		// NOTHING WAS APPENDED: the decide refused the write or found it
		// unchanged, and its last round says which. A publish that answered
		// with no record and no decision broke the publisher's contract,
		// and says so rather than answering from nothing.
		switch {
		case decided && last.refusal != "":
			return ConversationResult{Refusal: last.refusal, Detail: last.detail}, nil
		case decided && last.unchanged:
			return ConversationResult{Outcome: statelog.OutcomeApplied, Position: unpacked(last.position),
				Unchanged: true}, nil
		}
		return ConversationResult{}, fmt.Errorf("code: the publish of operation %s answered applied with no "+
			"record, and its decide neither refused nor found it unchanged", cw.OpID)
	case statelog.OutcomePending:
		return cw.made(ConversationResult{Outcome: statelog.OutcomePending, Position: res.Position}), nil
	}
	return cw.made(ConversationResult{Outcome: statelog.OutcomeUnknown}), nil
}

// made is res naming what cw makes that is named apart from its change — a
// comment's id, a review's and its findings' — every one derived from the
// write's operation id, so it is the same on every node and every retry.
func (cw ConversationWrite) made(res ConversationResult) ConversationResult {
	switch {
	case cw.Comment != nil:
		res.ID = CommentID(cw.OpID)
	case cw.Review != nil:
		res.ID = ReviewID(cw.OpID)
		for i := range cw.Review.Findings {
			res.FindingIDs = append(res.FindingIDs, FindingID(cw.OpID, i))
		}
	}
	return res
}

// applied answers a write whose record applied at at, FROM THE ROWS: the
// change's conversation, whose position every record of it moves to its own,
// holds at or past at only once the record — or one after it — applied. A
// conversation that does not is a record that applied and wrote nothing this
// node holds: a change gone under it, which no writer of this domain forms
// while its repository lives, so it is logged at ERROR and answered unknown —
// never applied, which would hand its asker a comment nobody can read.
func (w *ChangeWriter) applied(ctx context.Context, cw ConversationWrite, at statelog.Position) (ConversationResult,
	error) {

	var position int64
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		_, position, _, err = conversationIn(ctx, tx, cw.ChangeID())
		return err
	}); err != nil {
		failed := fmt.Errorf("code: the record of operation %s applied at %s and its change's conversation "+
			"could not be read: %w", cw.OpID, at, err)
		w.log.WarnContext(ctx, "code_conversation_failed", "repo", cw.RepoID(), "change", cw.ChangeID(),
			"op_id", cw.OpID, "error", failed)
		return ConversationResult{}, failed
	}
	if position < at.Packed() {
		w.log.ErrorContext(ctx, "code_conversation_unanswerable", "repo", cw.RepoID(), "change", cw.ChangeID(),
			"op_id", cw.OpID, "position", at.String())
		return cw.made(ConversationResult{Outcome: statelog.OutcomeUnknown, Detail: fmt.Sprintf("the write's "+
			"record applied at %s and the change's conversation this node holds is not at it", at)}), nil
	}
	return cw.made(ConversationResult{Outcome: statelog.OutcomeApplied, Position: at}), nil
}

// refused answers a write whose publish failed — the committer's mapping of a
// change's ([Committer.refusedChange]) in a conversation's words.
func (w *ChangeWriter) refused(ctx context.Context, cw ConversationWrite, err error) (ConversationResult, error) {
	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal):
		w.log.ErrorContext(ctx, "code_conversation_invalid", "repo", cw.RepoID(), "change", cw.ChangeID(),
			"op_id", cw.OpID, "error", err)
		return ConversationResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		return ConversationResult{Refusal: ConversationRefusalBusy, Detail: "the change kept moving for every " +
			"round the write may take; " + conversationAgain}, nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		// A FACT ABOUT THE REPOSITORY: its deletion marker drops every
		// record about it — on a change's subject by the repository its
		// payload names — and the position is named only where the marker
		// dropped THIS record.
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		return ConversationResult{Refusal: ConversationRefusalRepoDeleted, Position: at,
			Detail: purgedDetail(ctx, w.db, w.log, cw.RepoID(), cw.OpID)}, nil
	case errors.As(err, &refusal) && refusal.Landed():
		return ConversationResult{Refusal: ConversationRefusalNotApplied, Position: refusal.Position,
			Detail: fmt.Sprintf("the write applies nowhere: its record at %s was durable and the code log's %s "+
				"gate drops it on every node; %s", refusal.Position, refusal.Reason, conversationAgain)}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return ConversationResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return ConversationResult{Refusal: ConversationRefusalUnavailable,
			Detail: "the code log refused the write: " + strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
	}
	w.log.WarnContext(ctx, "code_conversation_failed", "repo", cw.RepoID(), "change", cw.ChangeID(),
		"op_id", cw.OpID, "error", err)
	return ConversationResult{}, fmt.Errorf("code: conversation operation %s: %w", cw.OpID, err)
}

// count counts one write's answer under its op and what became of it
// ([conversationOutcome]).
func (w *ChangeWriter) count(cw ConversationWrite, res ConversationResult, err error) {
	if w.metrics == nil {
		return
	}
	op, _ := cw.record()
	w.metrics.Add(metrics.CodeConversationWrites, 1, metrics.Attrs{"op": string(op),
		"outcome": conversationOutcome(res, err)})
}

// conversationOutcome is what became of a write of a change's conversation, as
// crewlet.code.conversation.writes names it: `handed_on` for one this node
// decided nothing of about itself, which the router asks the repository's
// runner-up; `failed` for any other error; the refusal's own code; `unchanged`
// for one answered from the rows with no record; and otherwise its outcome —
// applied, pending or unknown.
func conversationOutcome(res ConversationResult, err error) string {
	switch {
	case errors.Is(err, ErrNotServing):
		return "handed_on"
	case err != nil:
		return "failed"
	case res.Refusal != "":
		return string(res.Refusal)
	case res.Unchanged:
		return "unchanged"
	}
	return string(res.Outcome)
}
