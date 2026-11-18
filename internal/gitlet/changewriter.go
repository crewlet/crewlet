package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CHANGE WRITER decides a change's conversation ([ConversationOps]) on the
// change's own subject, beside the repository's committer on its committing
// data node but sharing none of its queue, budget or batches: a comment shares
// nothing with the pushes that move the change's branches, and would wait
// behind their batches (package doc, "A change's conversation is rows on its
// own subject"). One change's writes run ONE AT A TIME, a single flight from
// the decide's first round to its answer, different changes' side by side
// (flight.go): two published at once from one node read one anchor and lose
// compare-and-set rounds to each other. A write waits its turn only while its
// asker's deadline leaves a publish and its answer ([CommitBudget],
// [ReplyMargin]) — the committer's rule: past it the asker hands the operation
// id to the runner-up, which contends with this node; the broker lands one.

// People answers who holds a human seat: the person the identity directory
// binds to it, holding the apps their grants name — none for a seat nobody
// holds or a person who may not act — or the error that kept the directory
// from answering. The engine's is the requester ceiling's own composition
// (internal/engine's turnRequesters, ADR-0109), so a person is one principal
// whether an agent works for them or a change asks them to review it.
type People interface {
	Requester(ctx context.Context, seat string) (iam.Principal, error)
}

// ChangeWriterOptions configure a change writer.
type ChangeWriterOptions struct {
	// Publisher is the code log's write authority.
	Publisher Publisher
	// DB is the replicated estate an applied write's answer is read from.
	DB store.ReplicatedReader
	// NodeID is the deciding node, which every record names as its actor.
	NodeID string
	// Retention is the epoch's code.deleted_retention_days: when a deleted
	// repository's purge may begin, which a write refused repo_deleted is
	// told ([RepoGone]). REQUIRED.
	Retention func() time.Duration
	// SeatRules is the epoch's chart, read once a write, which judges a write
	// that [ConversationWrite.JudgesChart] — a seat's grant on the repository
	// and every reviewer an ask names. False while this node has installed no
	// epoch: such a write is answered [ErrNotServing], so the router asks the
	// runner-up. REQUIRED: a reviewer is never judged by a default.
	SeatRules func() (SeatRules, bool)
	// Current is whether this node serves the company's configuration (the
	// engine's posture gate): false only while CONFIRMED behind it; nil is
	// current. While false, a write the chart judges is answered
	// [ErrNotServing] when queued and again at its turn, so the router asks
	// the runner-up; a person's comment or resolution is decided as ever.
	Current func() bool
	// Now is the decide's clock — a record's authored instant; nil takes the
	// wall clock in UTC.
	Now func() time.Time
	// Events is where a review this writer landed is announced
	// (`review_submitted`, announce.go); nil announces none.
	Events EventPublisher
	// AgentIDs resolves a seat's handle to the agent id its day is counted
	// under ([CommitterOptions.AgentIDs]). REQUIRED with Events: a review
	// announced without its reviewer's and change owner's ids is counted on
	// no seat's day.
	AgentIDs func(handle string) (string, bool)
	// People answers whether each person an ask for review names uses Gitlet
	// ([People]). REQUIRED: a person is never asked to review on a default.
	People People
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
	people    People
	metrics   *metrics.Recorder
	log       *slog.Logger

	// flights is each change's single flight, the three bounds a write is
	// admitted under, and the publishes' own lifetime (flight.go).
	flights *flights
}

// changeFlights is what a change writer's single flights admit, the
// committer's rule for a proposal. A waiting write holds the estate's answer
// slot on the proposal lane pushes share, and its decoded request (a review up
// to [MaxConversationBytes]), so past any bound it is refused busy at once,
// undecided: [MaxChangeWritesWaiting] of its change (a burst, or writes queued
// behind a publish an election holds), [ChangeWriterNodeWrites] across every
// change (the writer's share of the lane), and [ChangeWriterNodeBytes].
var changeFlights = flightBounds{
	key: MaxChangeWritesWaiting, node: ChangeWriterNodeWrites, bytes: ChangeWriterNodeBytes,
	keyBound: "MaxChangeWritesWaiting", nodeBound: "ChangeWriterNodeWrites", bytesBound: "ChangeWriterNodeBytes",
	noun: "change", writes: "writes", closed: errChangeWriterStopping,
}

// NewChangeWriter builds a change writer, refusing what it cannot work without.
func NewChangeWriter(o ChangeWriterOptions) (*ChangeWriter, error) {
	switch {
	case o.Publisher == nil:
		return nil, errors.New("gitlet: a change writer needs the code log's write authority — every write " +
			"of a change's conversation is a record on that log")
	case o.DB.IsZero():
		return nil, errors.New("gitlet: a change writer needs the replicated estate a write's answer is read " +
			"from — an applied write is only ever reported from its rows")
	case !config.ValidNodeID(o.NodeID):
		return nil, fmt.Errorf("gitlet: a change writer needs the deciding node's id, which every record names "+
			"as its actor; %q is not one config admits", o.NodeID)
	case o.Retention == nil:
		return nil, errors.New("gitlet: a change writer needs the epoch's code.deleted_retention_days — a write " +
			"to a deleted repository is told when its purge may begin, which a default would misstate")
	case o.SeatRules == nil:
		return nil, errors.New("gitlet: a change writer needs the epoch's chart (SeatRules) — a seat's write and " +
			"every reviewer an ask names are judged by the chart, never by a default")
	case o.People == nil:
		return nil, errors.New("gitlet: a change writer needs the identity directory's people (People) — a person " +
			"an ask for review names is asked only where they use Gitlet, never on a default")
	case o.Events != nil && o.AgentIDs == nil:
		return nil, errors.New("gitlet: a change writer that announces on an event stream needs AgentIDs — a " +
			"review is counted on its reviewer's and its change owner's seat days by their agent ids, which only " +
			"the chart says")
	}
	w := &ChangeWriter{
		pub: o.Publisher, db: o.DB, nodeID: o.NodeID, retention: o.Retention, seatRules: o.SeatRules,
		current: o.Current, now: o.Now, events: o.Events, agentIDs: o.AgentIDs, people: o.People,
		metrics: o.Metrics, log: o.Logger, flights: newFlights(changeFlights),
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	if w.log == nil {
		w.log = logging.Get("gitlet")
	}
	return w, nil
}

// Converse decides one write of a change's conversation and answers what
// became of it. It is answered busy, undecided, past one of the writer's
// bounds ([MaxChangeWritesWaiting], [ChangeWriterNodeWrites],
// [ChangeWriterNodeBytes]) or the moment its asker's deadline leaves too
// little for a publish and its answer ([CommitBudget], [ReplyMargin]), and
// waits at most a CommitBudget where there is none; [ErrNotServing] by a
// stopping writer, and for a write the chart judges by one behind the
// company's configuration or holding no chart; [ErrInvalidProposal] if it is
// no write. ctx bounds only this caller's wait. ITS OPERATION ID IS THE WRITE'S
// OWN, so the same write asked of the runner-up collapses in the ledger onto
// the record that landed, never a second comment. Applied is answered FROM THE
// ROWS, never from the decision.
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

// errChangeWriterStopping is the refusal of a write a stopping writer decides
// nothing of.
var errChangeWriterStopping = fmt.Errorf("%w: the change writer is stopping", ErrNotServing)

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
	// AND WHO OF THE PERSONS IT ASKS MAY NOT BE ASKED, from the identity
	// directory, before the decide, which reads nothing but its snapshot.
	noGitlet, unsaid := w.withoutGitlet(ctx, cw, seats.Roster)
	if unsaid != "" {
		hold.release()
		return busyConversation(unsaid), nil, false
	}
	if !w.flights.begin() {
		hold.release()
		return ConversationResult{}, errChangeWriterStopping, false
	}
	type answer struct {
		res ConversationResult
		err error
	}
	done := make(chan answer, 1)
	//nolint:contextcheck // The publish is the writer's, not this caller's: it runs under the writer's own lifetime, so an asker that hangs up mid-publish never leaves the change's next write deciding beside a record still in flight.
	go func() {
		defer w.flights.done()
		res, err, fresh := w.write(cw, seats, noGitlet)
		// THE CHANGE'S NEXT WRITE WAITS ON NONE OF THE ANNOUNCEMENT
		// (bounded by checkEventBudget), which runs after the answer but
		// inside this publish's flight, so Stop waits for it.
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

// withoutGitlet is the persons an ask for review names who may not be asked,
// since reading a change is reading its code: each human seat whose holder
// the identity directory says cannot use Gitlet (ADR-0110: unheld, may not
// act, or not given the app). Where the directory cannot say, it is instead
// the sentence the write is answered busy with — never a refusal of somebody
// who may use the app. Nil for any other write or an ask naming no person.
// Read before the decide, which is pure over its snapshot: ONE POINT READ A
// PERSON on the asker's deadline (at most [MaxReviewersPerChange]); a handle
// with no seat, and an agent's seat, are left to the decide.
func (w *ChangeWriter) withoutGitlet(ctx context.Context, cw ConversationWrite, roster Roster) (map[string]bool,
	string) {

	if cw.Reviewers == nil || roster == nil {
		return nil, ""
	}
	var out map[string]bool
	asked := map[string]bool{}
	for _, rv := range cw.Reviewers.Request {
		seat, ok := roster.Seat(rv.Handle)
		if !ok || !seat.Human || asked[rv.Handle] {
			continue
		}
		asked[rv.Handle] = true
		p, err := w.people.Requester(ctx, rv.Handle)
		if err != nil {
			return nil, fmt.Sprintf("the identity directory could not say now whether the person holding %q uses "+
				"Gitlet, which an ask for their review is decided by (%v): send it again", rv.Handle, err)
		}
		if !p.Can(iam.GrantGitlet) {
			if out == nil {
				out = map[string]bool{}
			}
			out[rv.Handle] = true
		}
	}
	return out, ""
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

// Close is the first half of [ChangeWriter.Stop], refusing without draining,
// so a teardown stopping several writers under one bound closes all of them
// before it drains any (internal/engine's stopTogether). Safe to call more
// than once.
func (w *ChangeWriter) Close() { w.flights.close() }

// Stop refuses every new write [ErrNotServing], and every one waiting for its
// change's turn, so the router asks the runner-up, and waits for the
// publishes in flight, FINISHED rather than abandoned so their askers get
// their outcome. If ctx ends first, they are cancelled and answered from what
// their publish returned. Safe to call more than once.
func (w *ChangeWriter) Stop(ctx context.Context) { w.flights.stop(ctx) }

// write publishes cw under the writer's own lifetime and one CommitBudget,
// never its asker's context: an asker that hangs up stops waiting, and its
// retry under the same operation id collapses onto a record that landed. It
// answers cw and whether it is a review this call landed, to announce
// (announce.go) — never one answered unchanged, nor a collapsed retry, whose
// first call announced it.
func (w *ChangeWriter) write(cw ConversationWrite, seats SeatRules, noGitlet map[string]bool) (ConversationResult,
	error, bool) {

	ctx, cancel := context.WithTimeout(w.flights.life, CommitBudget)
	defer cancel()
	op, _ := cw.record()
	retention := w.retention()
	repoID, changeID := cw.RepoID(), cw.ChangeID()
	subject := ChangeSubject(changeID)
	scope := []string{RepoPath(repoID), ChangePath(changeID)}
	if cw.Mergeability != nil {
		// A MERGEABILITY'S READINESS READS THE REPOSITORY'S CHECKS
		// (gatejudge.go, Mergeability.ChecksPosition), so its scope names
		// them too: a node holding a deferred record of them decides none
		// (M5-D33).
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
			d, err := decideConversation(ctx, tx, cw, seats, retention, noGitlet)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.payload == nil {
				return statelog.Decision{Version: d.version}, nil
			}
			formed := op
			if d.op != "" {
				// THE RECORD THE DECIDE FORMED in place of the one the write
				// carries: a mergeability kept by what it moved.
				formed = d.op
			}
			decision, err := recordFor(stamp, subject, formed, scope, cw.OpID,
				Actor{ID: w.nodeID, Kind: ActorSystem}, d.payload, w.now())
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
		// NOTHING WAS APPENDED: the decide's last round refused the write or
		// found it unchanged; anything else broke the publisher's contract.
		switch {
		case decided && last.refusal != "":
			return ConversationResult{Refusal: last.refusal, Detail: last.detail}, nil
		case decided && last.unchanged:
			return ConversationResult{Outcome: statelog.OutcomeApplied, Position: unpacked(last.position),
				Unchanged: true}, nil
		}
		return ConversationResult{}, fmt.Errorf("gitlet: the publish of operation %s answered applied with no "+
			"record, and its decide neither refused nor found it unchanged", cw.OpID)
	case statelog.OutcomePending:
		return cw.made(ConversationResult{Outcome: statelog.OutcomePending, Position: res.Position}), nil
	}
	return cw.made(ConversationResult{Outcome: statelog.OutcomeUnknown}), nil
}

// made is res naming what cw makes apart from its change (a comment's id, a review's and its
// findings'), each derived from the write's operation id so it is the same on every node and
// every retry; an edit or a delete names the comment it names.
func (w ConversationWrite) made(res ConversationResult) ConversationResult {
	switch {
	case w.Comment != nil:
		res.ID = CommentID(w.OpID)
	case w.CommentEdit != nil:
		res.ID = w.CommentEdit.CommentID
	case w.CommentDelete != nil:
		res.ID = w.CommentDelete.CommentID
	case w.Review != nil:
		res.ID = ReviewID(w.OpID)
		for i := range w.Review.Findings {
			res.FindingIDs = append(res.FindingIDs, FindingID(w.OpID, i))
		}
	}
	return res
}

// applied answers a write whose record applied at at, FROM THE ROWS: every
// record moves the change's conversation to its own position. One short of at
// means a change gone under the record, which no writer forms while its
// repository lives, so it is logged at ERROR and answered unknown — never
// applied, which would hand its asker a comment nobody can read.
func (w *ChangeWriter) applied(ctx context.Context, cw ConversationWrite, at statelog.Position) (ConversationResult,
	error) {

	var position int64
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		_, position, _, err = conversationIn(ctx, tx, cw.ChangeID())
		return err
	}); err != nil {
		failed := fmt.Errorf("gitlet: the record of operation %s applied at %s and its change's conversation "+
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
		w.log.Log(ctx, invalidLevel(err), "code_conversation_invalid", "repo", cw.RepoID(), "change",
			cw.ChangeID(), "op_id", cw.OpID, "error", err)
		return ConversationResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		return ConversationResult{Refusal: ConversationRefusalBusy, Detail: "the change kept moving for every " +
			"round the write may take; " + conversationAgain}, nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		// A FACT ABOUT THE REPOSITORY, whose deletion marker drops every
		// record about it; the position is named only where it dropped THIS
		// record.
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
	return ConversationResult{}, fmt.Errorf("gitlet: conversation operation %s: %w", cw.OpID, err)
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

// conversationOutcome is what became of a write, as
// crewlet.code.conversation.writes names it: `handed_on` for one the router
// asks the runner-up of ([handedOn]); `failed` for any other error; the
// refusal's own code; `unchanged`; otherwise applied, pending or unknown.
func conversationOutcome(res ConversationResult, err error) string {
	switch {
	case handedOn(err):
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
