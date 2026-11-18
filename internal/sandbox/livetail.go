package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// What a running executor has said so far, pulled from the node that reads its
// box. Until collection the transcript stays in the box; the launch's
// `executor_collected` record then carries it, held to [MaxRunTextBytes].
//
// A QUESTION, never a published stream: an event per poll per run would fill the
// event store for nobody, since almost no run is watched. `executor_tail{turn_id,
// launch_id}` is answered by the run's owner ([PendingRun.Owner]) or, for a box
// on an engine host, by the node that launched it ([PendingRun.LaunchNode]), the
// only node that reaches it (boxhost.go). No event, no row; what the owner keeps
// while somebody looks, and the delta an answer sends, is livefeed.go's.

// No outcome ([TailOutcome]) is an empty answer standing in for another:
// `owner_silent` is NAMED because "the run said nothing" and "the node that reads
// it did not answer" send a reader to opposite places, and a polling screen
// cannot recover from the second shown as an empty tail. `launching` is NOT a
// stopped run: a screen that took it for one stopped asking about a run that had
// not begun. A node with no presence is asked like any other. `box_paused` and
// `box_gone` are the box's state, not failures.
//
// A box that could not be read is an ERROR, and only its CLASS crosses the wire
// ([TailFailure]): the reading node logs the text and answers what clears it, so
// the query surface tells a wait (a provider that did not answer) from a setup
// somebody must fix.

// TailReadBudget is how long a tail request waits for the node that answers:
// the fleet read budget every other scatter answers inside (eventfan's
// FleetReadBudget, memread's DefaultBudget).
//
// The request carries it ([tailRequest.BudgetMS]) so it bounds the answerer
// too: its answer waits for the box only [answerShare] of it, freeing the
// answer slot once the asker has given up. The read itself is shared and goes
// on ([LiveFeeds]), and the next request is answered from what it found.
const TailReadBudget = 2 * time.Second

// answerShare is the part of an asker's budget an answer may spend waiting for
// the box, leaving the rest for the reply to cross the broker: an answer
// composed just as the asker gives up is the silence it was meant to avoid.
const answerShare = 0.75

// OutputSource is which of a job's two accounts of itself an [Output] is.
type OutputSource string

const (
	// SourceTranscript is the runner's parsed activity log — tool calls,
	// shell commands, what the agent said.
	SourceTranscript OutputSource = "transcript"
	// SourceStderr is the raw error stream, for an agent whose output
	// cannot be parsed until it finishes.
	SourceStderr OutputSource = "stderr"
	// SourceNone is a job that has written nothing either reader can show.
	SourceNone OutputSource = "none"
)

// Valid reports whether this build knows the source.
func (s OutputSource) Valid() bool {
	return s == SourceTranscript || s == SourceStderr || s == SourceNone
}

// Output is what a job has said about itself, as one answer carries it: what
// the asker's cursor lacks ([TailCursor]), so how much a view holds costs
// nothing per poll. Epoch, Start, End and Digest are always present, and an
// asker holds at most [MaxRunTextBytes].
type Output struct {
	// Text is the output, REDACTED (the box's environment holds the seat's
	// credentials, and this reaches a screen): what follows the asker's
	// offset or, on a Reset, the last [MaxRunTextBytes] in whole lines.
	Text string `json:"text"`
	// Source says which account Text is.
	Source OutputSource `json:"source"`
	// Cut is whether output came before what this answer carries and is
	// not in it: on a reset, before the reset's start.
	Cut bool `json:"cut"`
	// AsOf is when the box was read, on the owning node's clock.
	AsOf time.Time `json:"as_of"`
	// Finished is whether the job has written its done marker: it is over
	// and waiting to be collected, so this output will not grow again.
	Finished bool `json:"finished"`

	// Epoch names the reading the offsets count in; a cursor naming
	// another is answered with a Reset.
	Epoch string `json:"epoch"`
	// Start and End are the offsets of Text's first byte and of the byte
	// after its last, in the epoch's text (UTF-8 bytes); End is the asker's
	// next offset. Never omitempty: ZERO IS AN OFFSET (a reading that has
	// settled nothing answers at end 0), and a screen comparing a delta's
	// start, or a REST caller sending `end` back, needs it present.
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	// Digest covers the window an asker holding through End holds; it goes
	// back on the next request, beside End.
	Digest string `json:"digest"`
	// Reset is whether Text REPLACES what the asker holds rather than
	// following it: its cursor named another reading, more than it may hold
	// was written since, or its digest did not match.
	Reset bool `json:"reset,omitempty"`
	// Front is whether the reading began after the job's own start, so
	// output before Start was never read at all.
	Front bool `json:"front,omitempty"`
	// Held is how many bytes the job has written that are not shown yet,
	// because what they redact to is not settled: a line not finished, a
	// private key whose END may still come, a password whose value is on a
	// later line.
	Held int `json:"held,omitempty"`
}

// TailCursor is how much of a reading an asker holds: through Offset of the
// reading named Epoch, with the Digest the owner gave it there. The zero
// cursor holds nothing, and is answered with a Reset — a first request, and a
// view that lost step with its owner.
type TailCursor struct {
	Epoch  string `json:"epoch"`
	Offset int64  `json:"offset"`
	Digest string `json:"digest"`
}

// TailQuery is one tail request, as the read surface hands it over.
type TailQuery struct {
	TurnID   string
	LaunchID string
	// Cursor is what the asker holds; the zero cursor holds nothing.
	Cursor TailCursor
}

// TailOutcome is what one tail request concluded.
type TailOutcome string

const (
	// TailRunning is an answer: the tail of a running job.
	TailRunning TailOutcome = "tail"
	// TailLaunching is a job being launched — its box made and provisioned,
	// its agent not started — which has nothing to show YET.
	TailLaunching TailOutcome = "launching"
	// TailNotRunning is a launch that is not a running job — see
	// [LiveLaunch].
	TailNotRunning TailOutcome = "not_running"
	// TailOwnerSilent is the node that reads the run's box — its owner, or
	// the node that launched a box on an engine host ([tailHost]) — not
	// answering inside the budget, or a box in a remote cell whose run no
	// node holds right now.
	TailOwnerSilent TailOutcome = "owner_silent"
	// TailBoxPaused is a running record whose box is PAUSED, which its
	// backend cannot read without waking it ([ErrBoxPaused]), and a peek
	// never wakes a box. Only a collection pauses a running run's box, so
	// its job has finished; NOT terminal: the next answer says whether the
	// record moved on or the collection is being tried again.
	TailBoxPaused TailOutcome = "box_paused"
	// TailBoxGone is a running record whose box its backend says is gone
	// for good ([ErrBoxGone]): reaped, or a local box whose root did not
	// outlive its node. Its waiter records the job lost and carries its
	// round on in a fresh run ([LossBoxGone]), so it is NOT terminal for a
	// reader, which asks until the record moves on.
	TailBoxGone TailOutcome = "box_gone"
)

// TailOutcomes is every outcome this build answers.
var TailOutcomes = []TailOutcome{
	TailRunning, TailLaunching, TailNotRunning, TailOwnerSilent, TailBoxPaused, TailBoxGone,
}

// Valid reports whether this build knows the outcome.
func (o TailOutcome) Valid() bool { return slices.Contains(TailOutcomes, o) }

// TailAnswer is one tail request's answer, as the read surface serves it.
type TailAnswer struct {
	Outcome  TailOutcome `json:"outcome"`
	TurnID   string      `json:"turn_id"`
	LaunchID string      `json:"launch_id"`
	// Node is the node that reads the run's box ([tailHost]), whether it
	// answered or not. Empty only for a box in a remote cell whose run no
	// node holds.
	Node string `json:"node,omitempty"`
	// Status is the run record's own status, on `not_running`: why the
	// launch is not a running job. Empty where no record is left.
	Status string `json:"status,omitempty"`
	// Output is the tail, on `tail` only.
	Output *Output `json:"output,omitempty"`
}

// StatusReplaced is the status a `not_running` answer names for a launch a
// later one replaced on the same run: the record is running, but not the job
// that was asked about.
const StatusReplaced = "replaced"

// PendingReader reads one run's record — the half of [PendingStore] a tail
// needs.
type PendingReader interface {
	Get(ctx context.Context, turnID string) (PendingRun, bool, error)
}

// LiveLaunch reads whether the launch named is a RUNNING job, and the record
// it read that from.
//
// Running means the record exists, still holds that launch, and is in
// [StatusRunning]. Otherwise the status says why: the record's own,
// [StatusReplaced] for a record on a later launch, or "" for no record (a
// settled run's record is deleted, so a run just collected reads so).
//
// AN UNREADABLE RECORD IS AN ERROR, never `not_running`: a screen told the run
// stopped would stop polling a run that is still going.
func LiveLaunch(ctx context.Context, store PendingReader, turnID, launchID string) (PendingRun, bool, string, error) {
	run, ok, err := store.Get(ctx, turnID)
	if err != nil {
		return PendingRun{}, false, "", fmt.Errorf("sandbox: read run %s: %w", turnID, err)
	}
	switch {
	case !ok:
		return PendingRun{}, false, "", nil
	case run.LaunchID != launchID:
		return run, false, StatusReplaced, nil
	case run.Status != StatusRunning:
		return run, false, run.Status, nil
	}
	return run, true, "", nil
}

// notRunning is the outcome for a launch [LiveLaunch] did not find running.
func notRunning(status string) TailOutcome {
	if status == StatusLaunching {
		return TailLaunching
	}
	return TailNotRunning
}

// TailAsker scatters one request — the half of [queue.EventQueue] a tail needs.
type TailAsker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// TailServer makes a process one of a subject's answerers.
type TailServer interface {
	Serve(ctx context.Context, subject string, answers int, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// TailReader answers a tail request from the node that reads the run's box.
type TailReader struct {
	// Owner is this process's incarnation, which is what a run's record
	// names as its owner and what a request is addressed to.
	Owner string

	// Node is this node's id, which is what a run's record names as the
	// node a box on an engine host was launched on, and what a request
	// about such a run is addressed to ([tailHost]).
	Node string

	// Pending is the fleet's run records.
	Pending PendingReader

	// Feeds is this node's own readings, for a run it owns ([LiveFeeds]);
	// nil on a node with no sandbox backend, which owns no run.
	Feeds *LiveFeeds

	// Queue asks a peer. Nil is a node with no broker, which is the whole
	// fleet: every run is its own.
	Queue TailAsker

	// Budget bounds a peer's answer; zero is [TailReadBudget].
	Budget time.Duration

	// Condition tells a condition of this node's own from a fault, for a
	// box it reads itself: the rule [ServeTail] takes, so a failure is
	// classed and logged alike whichever node was asked. Required
	// ([errNoCondition]).
	Condition func(error) bool
}

// errNoCondition is a reader built without the rule it classes its own
// failures by.
var errNoCondition = errors.New("sandbox: a tail reader needs the rule it tells a " +
	"condition from a fault by")

func (r *TailReader) budget() time.Duration {
	if r.Budget > 0 {
		return r.Budget
	}
	return TailReadBudget
}

// Tail answers one request.
func (r *TailReader) Tail(ctx context.Context, q TailQuery) (TailAnswer, error) {
	if r.Condition == nil {
		return TailAnswer{}, errNoCondition
	}
	answer := TailAnswer{TurnID: q.TurnID, LaunchID: q.LaunchID}
	run, running, status, err := LiveLaunch(ctx, r.Pending, q.TurnID, q.LaunchID)
	if err != nil {
		return TailAnswer{}, err
	}
	answer.Node = nodeOf(run.Owner)
	host := tailHost(run)
	if host != "" {
		answer.Node = host
	}
	if !running {
		// As [ServeTail] does: a stopped launch is never asked about again,
		// so this node's reading of it (held only if it owns the run) goes
		// now, not on a sweep. Another owner lets its own go on its clock
		// ([liveIdle]).
		if r.Feeds != nil {
			r.Feeds.Forget(q.TurnID, q.LaunchID)
		}
		answer.Outcome, answer.Status = notRunning(status), status
		return answer, nil
	}
	switch {
	case host != "":
		// A BOX ON AN ENGINE HOST is read on that host, whoever holds the
		// seat: no other node can reach it.
		if host == r.Node || r.Queue == nil {
			return r.local(ctx, run, q.Cursor, answer)
		}
		return r.ask(ctx, run, q.Cursor, answer, host)
	case run.Owner == "":
		// UNCLAIMED, between an owner letting the seat go and the next
		// recovering the run: nobody can ask the box, which "silent" with no
		// node says.
		answer.Outcome = TailOwnerSilent
		return answer, nil
	case run.Owner == r.Owner || r.Queue == nil:
		return r.local(ctx, run, q.Cursor, answer)
	}
	return r.ask(ctx, run, q.Cursor, answer, "")
}

// tailHost is the node a run's tail is read on when that is not its owner: the
// node that launched a box on an engine host, which alone can reach it — ""
// for a box every node reaches, whose owner reads it.
func tailHost(run PendingRun) string {
	if Placement(run.Placement).OnEngineHost() {
		return run.LaunchNode
	}
	return ""
}

// local answers from this node's own reading, inside the share of the budget
// an owner answering a peer would have.
//
// A failure is logged HERE, as one met answering a peer is ([logTailFailure]):
// this node met it and is the only one reading the box. It is returned with its
// marks intact for the query surface to class, wrapped in [sourceLogged] so that
// surface logs a trace rather than the same failure again.
func (r *TailReader) local(ctx context.Context, run PendingRun, cursor TailCursor, answer TailAnswer) (TailAnswer, error) {
	fail := func(err error) (TailAnswer, error) {
		logTailFailure(ctx, run.TurnID, run.LaunchID, tailFailureOf(err, r.Condition), err)
		return TailAnswer{}, sourceLogged{err}
	}
	if r.Feeds == nil {
		return fail(errNoBackend())
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(float64(r.budget())*answerShare))
	defer cancel()
	out, err := r.Feeds.Answer(ctx, run, cursor)
	switch {
	case errors.Is(err, ErrBoxPaused):
		answer.Outcome = TailBoxPaused
	case errors.Is(err, ErrBoxGone):
		answer.Outcome = TailBoxGone
	case err != nil:
		return fail(err)
	default:
		answer.Outcome, answer.Output = TailRunning, &out
	}
	return answer, nil
}

// sourceLogged is a failure the node that met it has logged already
// ([logTailFailure]). It unwraps, so every classification reads through it, and
// a surface that logs what it answered writes only a trace of it.
type sourceLogged struct{ error }

func (e sourceLogged) Unwrap() error { return e.error }

// LoggedAtSource says the failure is in the log of the node that met it.
func (sourceLogged) LoggedAtSource() bool { return true }

// ask puts the request to the incarnation the record names — or, with host
// set, to the node a box on an engine host was launched on ([tailHost]).
func (r *TailReader) ask(ctx context.Context, run PendingRun, cursor TailCursor, answer TailAnswer,
	host string) (TailAnswer, error) {
	req := tailRequest{
		Version: tailWireVersion, TurnID: run.TurnID, LaunchID: run.LaunchID, Owner: run.Owner,
		Cursor: cursor, BudgetMS: r.budget().Milliseconds(), Node: host,
	}
	if host != "" {
		req.Owner = ""
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return TailAnswer{}, fmt.Errorf("sandbox: encode a tail request: %w", err)
	}
	askCtx, cancel := context.WithTimeout(ctx, r.budget())
	defer cancel()
	replies, err := r.Queue.Ask(askCtx, topics.ObserveExecutorTail, raw, 1)
	if err != nil {
		return TailAnswer{}, fmt.Errorf("sandbox: %s could not be asked about run %s: %w",
			answer.Node, run.TurnID, err)
	}
	for _, body := range replies {
		var rep tailReply
		if json.Unmarshal(body, &rep) != nil || rep.Owner != req.Owner || rep.Node != host {
			continue
		}
		switch {
		case rep.Failed != "":
			// A failure class from a newer node is a fault here ([TailError]);
			// warn first, since only that node's log names it.
			if !rep.Failed.Valid() {
				log.WarnContext(ctx, "sandbox_tail_failure_unknown", "turn_id", run.TurnID,
					"launch_id", run.LaunchID, "node", answer.Node,
					"failure", string(rep.Failed))
			}
			return TailAnswer{}, &TailError{Node: answer.Node, TurnID: run.TurnID,
				Failure: rep.Failed}
		case rep.Paused:
			answer.Outcome = TailBoxPaused
		case rep.Gone:
			answer.Outcome = TailBoxGone
		case rep.Output != nil:
			answer.Outcome, answer.Output = TailRunning, rep.Output
		default:
			// The owner's own read found the launch stopped: its answer wins.
			answer.Outcome, answer.Status = notRunning(rep.Status), rep.Status
		}
		return answer, nil
	}
	answer.Outcome = TailOwnerSilent
	return answer, nil
}

// ServeTail makes this node an answerer for tail requests about the runs it
// owns, or whose box it launched on its own host (a request addressed to a node,
// [tailHost]). Every node serves; only the addressed incarnation answers, from
// its OWN read of the record: the asker's is a moment old, and a box read after
// its collection would show a finished job as running.
//
// Each answer is bounded by its asker's budget ([tailRequest.answerBudget]), or
// a slow box fills the answer slots every request shares with answers nobody
// reads. A failure is answered by its class and logged here, with its text
// ([tailFailureOf]); condition is the engine's one rule ([builtin.Condition]),
// handed in because its sentinels live above this package. Nil is refused.
func ServeTail(ctx context.Context, q TailServer, owner, node string, pending PendingReader,
	feeds *LiveFeeds, condition func(error) bool,
) (queue.Unsubscribe, error) {
	if condition == nil {
		return nil, errors.New("sandbox: a tail answerer needs the rule it tells a " +
			"condition from a fault by")
	}
	fail := func(ctx context.Context, req tailRequest, err error) TailFailure {
		failure := tailFailureOf(err, condition)
		logTailFailure(ctx, req.TurnID, req.LaunchID, failure, err)
		return failure
	}
	return q.Serve(ctx, topics.ObserveExecutorTail, queue.ConcurrentAnswers, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req tailRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("sandbox: a tail request this build cannot read: %w", err)
		}
		hosted := req.Node != ""
		if (hosted && req.Node != node) || (!hosted && req.Owner != owner) {
			return nil, errTailNotAddressed
		}
		ctx, cancel := context.WithTimeout(ctx, req.answerBudget())
		defer cancel()
		reply := tailReply{Version: tailWireVersion, Owner: req.Owner, Node: req.Node}
		run, running, status, err := LiveLaunch(ctx, pending, req.TurnID, req.LaunchID)
		switch {
		case err != nil:
			reply.Failed = fail(ctx, req, err)
		case !running:
			reply.Status = status
			if feeds != nil {
				feeds.Forget(req.TurnID, req.LaunchID)
			}
		case hosted && tailHost(run) != node:
			// The box moved off this node's host since the asker's read; the
			// next request finds the node that reads it now.
			log.DebugContext(ctx, "sandbox_tail_moved", "turn_id", run.TurnID,
				"launch_id", run.LaunchID, "node", tailHost(run), "owner", nodeOf(run.Owner))
			reply.Failed = TailFailedMoving
		case !hosted && run.Owner != owner:
			// The seat moved since the asker's read: say so rather than read
			// a box this node no longer drives.
			log.DebugContext(ctx, "sandbox_tail_moved", "turn_id", run.TurnID,
				"launch_id", run.LaunchID, "owner", nodeOf(run.Owner))
			reply.Failed = TailFailedMoving
		case feeds == nil:
			reply.Failed = fail(ctx, req, errNoBackend())
		default:
			out, err := feeds.Answer(ctx, run, req.Cursor)
			switch {
			case errors.Is(err, ErrBoxPaused):
				reply.Paused = true
			case errors.Is(err, ErrBoxGone):
				reply.Gone = true
			case err != nil:
				reply.Failed = fail(ctx, req, err)
			default:
				reply.Output = &out
			}
		}
		return json.Marshal(reply)
	})
}

// nodeOf is the node an incarnation belongs to: the stable id before the
// incarnation's own suffix (`<node>:<uuid>`), which is what a reader knows the
// node by.
func nodeOf(owner string) string {
	node, _, _ := strings.Cut(owner, ":")
	return node
}

// tailWireVersion is the request and reply shape this build speaks. Evolution
// is ADDITIVE within a version.
const tailWireVersion = 1

// errTailNotAddressed is a request for another incarnation; the serve answers
// nothing for it.
var errTailNotAddressed = errors.New("sandbox: the tail request is addressed to another incarnation")

// tailRequest is one request on the wire.
type tailRequest struct {
	Version  int    `json:"v"`
	TurnID   string `json:"turn_id"`
	LaunchID string `json:"launch_id"`
	Owner    string `json:"owner"`
	// Node addresses the request to the node a box on an engine host was
	// launched on, in place of Owner ([tailHost]).
	Node string `json:"node,omitempty"`

	// Cursor is what the asker holds; the zero cursor holds nothing.
	Cursor TailCursor `json:"cursor"`
	// BudgetMS is how long the asker waits for the reply; zero is
	// [TailReadBudget].
	BudgetMS int64 `json:"budget_ms,omitempty"`
}

// answerBudget is how long the owner's answer may wait for the box: its share
// of the asker's budget ([answerShare]).
func (r tailRequest) answerBudget() time.Duration {
	budget := TailReadBudget
	if r.BudgetMS > 0 {
		budget = time.Duration(r.BudgetMS) * time.Millisecond
	}
	return time.Duration(float64(budget) * answerShare)
}

// tailReply names its owner — or the node it was addressed to — because a
// scatter's replies carry no sender.
type tailReply struct {
	Version int     `json:"v"`
	Owner   string  `json:"owner"`
	Node    string  `json:"node,omitempty"`
	Output  *Output `json:"output,omitempty"`
	Status  string  `json:"status,omitempty"`

	// Failed is why the node addressed could not answer, as a class and
	// never its error's text, which stays in that node's log
	// ([TailFailure]).
	Failed TailFailure `json:"failed,omitempty"`

	// Paused is [TailBoxPaused]: the box is paused and was left so.
	Paused bool `json:"paused,omitempty"`
	// Gone is [TailBoxGone]: the box's backend says it is gone for good.
	Gone bool `json:"gone,omitempty"`
}

// TailFailure is why the node that reads a run's box ([tailHost]) could not
// answer a tail request: a CLASS, never the error's text. Every [Why] is one,
// and so are [TailFailedMoving], [TailFailedUnanswered] and [TailFailedFault].
//
// Not the text: it names hosts, statuses and a provider's own body, which
// belong in the log of the node that met them, and an asker given only words
// could report nothing but a fault of its own.
type TailFailure string

const (
	// TailFailedMoving is a run that moved off the node asked between the
	// asker's read of its record and that node's: its seat left the owner,
	// or its box is on another node's host now ([tailHost]). The next
	// request finds the node that reads it now.
	TailFailedMoving TailFailure = "moving"
	// TailFailedUnanswered is a condition of the node asked that waiting
	// clears: what it reads the run's record from did not answer, or it is
	// stopping and has let its readings go.
	TailFailedUnanswered TailFailure = "unanswered"
	// TailFailedFault is a fault of the node asked, which its log names.
	TailFailedFault TailFailure = "fault"
)

// Valid reports whether this build knows the failure: a [Why], or one of the
// answering node's own.
func (f TailFailure) Valid() bool {
	switch f {
	case TailFailedMoving, TailFailedUnanswered, TailFailedFault:
		return true
	}
	return Why(f).Valid()
}

// ErrTailUnanswered is a tail request the node that reads its run's box could
// not answer for a reason of its own that waiting clears — [TailFailedMoving]
// and [TailFailedUnanswered]. The engine's condition rule tells it as a wait.
var ErrTailUnanswered = errors.New("sandbox: the node that reads the run's box could not " +
	"answer just now")

// TailError is a tail request the node that reads its run's box answered with
// a [TailFailure]. It is classed by that failure alone: a [Why] by [WhyOf],
// that node's conditions as [ErrTailUnanswered], and a fault — or a failure a
// newer build named that this one does not know — as nothing, which is what a
// fault is. Its text is for this node's log and names where the detail is.
type TailError struct {
	// Node is the node that reads the run's box ([tailHost]), which logged
	// the failure.
	Node string
	// TurnID is the run.
	TurnID string
	// Failure is what that node answered.
	Failure TailFailure
}

func (e *TailError) Error() string {
	return fmt.Sprintf("sandbox: %s reads run %s's box and could not answer (%s); its log "+
		"has why", e.Node, e.TurnID, e.Failure)
}

// Is makes the answering node's conditions [ErrTailUnanswered].
func (e *TailError) Is(target error) bool {
	return target == ErrTailUnanswered &&
		(e.Failure == TailFailedMoving || e.Failure == TailFailedUnanswered)
}

// LoggedAtSource says the failure is in the log of the node that met it, with
// its text: the asker's own line about it is a trace.
func (*TailError) LoggedAtSource() bool { return true }

// why is the class that node's backend marked, where the failure is one.
func (e *TailError) why() (Why, bool) {
	why := Why(e.Failure)
	return why, why.Valid()
}

// tailFailureOf is the class of a failure a node met answering a tail request
// for a box it reads: the [Why] a backend marked it with,
// [TailFailedUnanswered] for one of its own a wait clears — a reading it let
// go ([ErrTailUnanswered]) or what condition says is a condition — and
// [TailFailedFault] for the rest.
func tailFailureOf(err error, condition func(error) bool) TailFailure {
	if why, ok := WhyOf(err); ok {
		return TailFailure(why)
	}
	if errors.Is(err, ErrTailUnanswered) || condition(err) {
		return TailFailedUnanswered
	}
	return TailFailedFault
}

// logTailFailure writes a failure the node that reads the box met, with its
// text, the only place the text goes: at WARN for a fault or a class no wait
// clears, which somebody must act on, and at DEBUG for one a wait clears, since
// a screen asks again every few seconds and a line per ask is noise.
func logTailFailure(ctx context.Context, turnID, launchID string, failure TailFailure, err error) {
	level := slog.LevelDebug
	if failure == TailFailedFault || (Why(failure).Valid() && !Why(failure).Waits()) {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "sandbox_tail_failed", "turn_id", turnID, "launch_id", launchID,
		"failure", string(failure), "error", err.Error())
}

// errNoBackend is a node that reads a run's box with no sandbox backend to
// read it through: an apply removed providers.sandbox while the run went on,
// which no wait changes and a person does (WhyConfiguration).
func errNoBackend() error {
	return Because(WhyConfiguration, errors.New("sandbox: this node reads the run's box and "+
		"has no sandbox backend to reach it with; providers.sandbox was removed by an apply"))
}
