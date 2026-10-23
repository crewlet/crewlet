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

// What a running coding run has said so far, pulled from the node that owns it.
//
// # Why a pull, and why from the owner
//
// A detached run writes its own account of itself into its box — the streamed
// transcript a runner parses, or its stderr — and nothing leaves the box until
// the run is collected, when the transcript, held to the record's bound (its
// start and its end, [MaxRunTextBytes]), rides the run's
// `agent_phase_completed{phase: sandbox}` record. Between the launch and the
// collection a person watching the run had nothing: the run could be minutes
// or hours in, and the turn trace showed a bar and no words.
//
// A PUBLISHED stream of it was the alternative, and it is the wrong one: an
// event per poll per run would fill the event store with the same text over and
// over for nobody, since almost nobody is watching almost every run. So it is a
// QUESTION, asked only while somebody looks: `sandbox_tail{turn_id, launch_id}`,
// answered from the box by the node whose incarnation the run's record names as
// its owner ([PendingRun.Owner]). There is no event and no row. What the owner
// keeps while somebody looks, and how an answer sends only what the asker
// lacks, is livefeed.go's.
//
// # Outcomes, never an empty answer standing in for another
//
// The TAIL itself ([Output], possibly empty — a run that has written nothing
// yet has nothing to show). `launching`, a job whose box is being made and
// whose agent has not started: NOT a run that stopped, and a screen that took
// it for one stopped asking about a run that had not begun. `not_running`, with
// the record's own status, when the launch asked about is not a running job:
// it finished and was collected, it parked on a question, a later launch
// replaced it, or no record is left at all. And `owner_silent{node}` when the
// node that owns the run did not answer inside the fleet read budget — NAMED,
// because "the run said nothing" and "the node that could ask it did not
// answer" send a reader to opposite places, and collapsing the second into an
// empty tail is exactly the lie a screen that polls cannot recover from. An
// owner with no presence — draining, or its heartbeat lapsed — is asked like
// any other, and its silence is that same `owner_silent`. `box_paused` is a
// running record whose box is paused, which a reader never wakes.
//
// A box that the owner could not read is an ERROR rather than an outcome, and
// what crosses the wire is its CLASS, never its text ([TailFailure]): the
// owner logs the failure it met and answers what clears it — a class its
// backend marked ([Why]), the run's seat leaving it, a condition of its own a
// wait clears, or a fault of its own — and the asker answers that class, so
// the query surface says a provider that did not answer is a wait and a setup
// is somebody's to fix. The text used to cross, and the asker could only
// report it as a fault of the node that asked.

// TailReadBudget is how long a tail request waits for the owning node to
// answer: the fleet read budget every other scatter in this engine answers
// inside (eventfan's FleetReadBudget, memread's DefaultBudget).
//
// It bounds the OWNER too, which nothing used to: the request carries it
// ([tailRequest.BudgetMS]), and the owner's answer waits for the box only
// [answerShare] of it, so an asker that gave up frees its answer slot instead
// of leaving the read running in it. The read itself is shared and goes on
// ([LiveFeeds]); the next request is answered from what it found. The
// dashboard chains its polls after each answer, so one open view has one
// request in flight, and any number of views share one read of the box.
const TailReadBudget = 2 * time.Second

// answerShare is the part of an asker's budget an owner's answer may spend
// waiting for the box: three quarters, leaving the rest for the reply to cross
// the broker before the asker stops listening — an answer composed just as
// the asker gives up is the silence it was meant to avoid.
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
// the asker's CURSOR lacks ([TailCursor]).
//
// The fields a reader compares against what it holds — Epoch, Start, End and
// Digest — are ALWAYS present, and an asker holds at most what the run's record
// will hold, [MaxRunTextBytes]: an answer is sent only what it lacks, so how
// much a view holds costs nothing per poll.
type Output struct {
	// Text is the output, REDACTED — the box's environment holds the
	// seat's credentials, and this reaches a screen: what follows the
	// asker's offset, or — on a Reset — the last [MaxRunTextBytes] in whole
	// lines, which replaces what it held.
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
	// after its last, in the epoch's text (UTF-8 bytes). End is what the
	// asker holds through once it has this answer — its next offset.
	//
	// NEVER OMITTED, because ZERO IS AN OFFSET: a reading that has settled
	// nothing yet answers at end 0 and is followed from start 0, and
	// `omitempty` folded both into "absent". A screen comparing a delta's
	// start with what it held through then compared a missing field with 0,
	// threw a valid delta away and asked for a reset; a REST caller told to
	// send `end` back had no `end` to send.
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
	// TailOwnerSilent is an owning node that did not answer inside the
	// budget, or a run no node holds right now.
	TailOwnerSilent TailOutcome = "owner_silent"
	// TailBoxPaused is a running record whose box is PAUSED, which its
	// backend cannot read without waking it ([ErrBoxPaused]) — and a peek
	// never wakes a box. Its job has finished: only a collection pauses a
	// running run's box, so this is the moment between that collection and
	// the record moving on, or a collection whose claim was handed back to
	// be tried again. NOT terminal: the next answer says which.
	TailBoxPaused TailOutcome = "box_paused"
)

// TailOutcomes is every outcome this build answers.
var TailOutcomes = []TailOutcome{
	TailRunning, TailLaunching, TailNotRunning, TailOwnerSilent, TailBoxPaused,
}

// Valid reports whether this build knows the outcome.
func (o TailOutcome) Valid() bool { return slices.Contains(TailOutcomes, o) }

// TailAnswer is one tail request's answer, as the read surface serves it.
type TailAnswer struct {
	Outcome  TailOutcome `json:"outcome"`
	TurnID   string      `json:"turn_id"`
	LaunchID string      `json:"launch_id"`
	// Node is the node that owns the run — the one that answered, or the one
	// that did not. Empty for a run no node holds.
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
// [StatusRunning]. Everything else is `not_running` with the reason as a status:
// the record's own (`launching`, `awaiting_clarification`, `resumed`,
// `reseed`), [StatusReplaced] for a record that moved on to a later launch, or
// "" for a record that is gone — a settled run's record is deleted, so a run
// collected a moment ago reads exactly like that.
//
// AN UNREADABLE RECORD IS AN ERROR, never `not_running`: the store could not
// say, and a screen told the run stopped would stop polling a run that is
// still going.
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
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// TailReader answers a tail request from the node that owns the run.
type TailReader struct {
	// Owner is this process's incarnation, which is what a run's record
	// names as its owner and what a request is addressed to.
	Owner string

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

	// Condition is the rule this node tells a condition of its own from a
	// fault by, for a run it owns and answers for itself: the same rule
	// [ServeTail] takes, so a failure is classed and logged alike whichever
	// node the person asked. Required: [TailReader.Tail] refuses a reader
	// with none.
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
	if !running {
		// The same as a peer's answer does: a launch that stopped is never
		// asked about again, so its reading here is let go now rather than
		// a sweep later. These are THIS node's readings, which hold the
		// launch only when this node owns its run; the owner of anybody
		// else's run is never asked again, and lets its reading go on its
		// own clock ([liveIdle]).
		if r.Feeds != nil {
			r.Feeds.Forget(q.TurnID, q.LaunchID)
		}
		answer.Outcome, answer.Status = notRunning(status), status
		return answer, nil
	}
	switch {
	case run.Owner == "":
		// UNCLAIMED: the instant between an owner letting the seat go and
		// the next one recovering its run. Nobody can ask the box, and
		// "silent" with no node says exactly that.
		answer.Outcome = TailOwnerSilent
		return answer, nil
	case run.Owner == r.Owner || r.Queue == nil:
		return r.local(ctx, run, q.Cursor, answer)
	}
	return r.ask(ctx, run, q.Cursor, answer)
}

// local answers from this node's own reading, inside the share of the budget
// an owner answering a peer would have.
//
// A FAILURE IS LOGGED HERE, AS THE OWNER LOGS ONE IT MEETS ANSWERING A PEER
// ([logTailFailure], `sandbox_tail_failed` with its class): this node is the
// one that met it, and on a single node, or wherever the node asked holds the
// seat, it is the only owner there is. It is returned as it was met, its marks
// intact, for the query surface to class by the one rule every surface tells a
// condition by, and marked as logged ([sourceLogged]) so that surface writes
// its own line as a trace rather than the same failure a second time.
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
	case err != nil:
		return fail(err)
	default:
		answer.Outcome, answer.Output = TailRunning, &out
	}
	return answer, nil
}

// sourceLogged is a failure the node that met it has logged already, with its
// class and its text ([logTailFailure]). It unwraps to the failure, so every
// classification reads through it, and says it was logged so a surface that
// logs what it answered writes a trace, never the failure again at its level.
type sourceLogged struct{ error }

func (e sourceLogged) Unwrap() error { return e.error }

// LoggedAtSource says the failure is in the log of the node that met it.
func (sourceLogged) LoggedAtSource() bool { return true }

// ask puts the request to the incarnation the record names.
func (r *TailReader) ask(ctx context.Context, run PendingRun, cursor TailCursor, answer TailAnswer) (TailAnswer, error) {
	raw, err := json.Marshal(tailRequest{
		Version: tailWireVersion, TurnID: run.TurnID, LaunchID: run.LaunchID, Owner: run.Owner,
		Cursor: cursor, BudgetMS: r.budget().Milliseconds(),
	})
	if err != nil {
		return TailAnswer{}, fmt.Errorf("sandbox: encode a tail request: %w", err)
	}
	askCtx, cancel := context.WithTimeout(ctx, r.budget())
	defer cancel()
	replies, err := r.Queue.Ask(askCtx, topics.ObserveSandboxTail, raw, 1)
	if err != nil {
		return TailAnswer{}, fmt.Errorf("sandbox: %s could not be asked about run %s: %w",
			answer.Node, run.TurnID, err)
	}
	for _, body := range replies {
		var rep tailReply
		if json.Unmarshal(body, &rep) != nil || rep.Owner != run.Owner {
			continue
		}
		switch {
		case rep.Failed != "":
			// A FAILURE A NEWER OWNER NAMED that this build doesn't know is
			// answered as a fault ([TailError]), and said here first, since
			// the owner's log names a class this node's reader can't.
			if !rep.Failed.Valid() {
				log.WarnContext(ctx, "sandbox_tail_failure_unknown", "turn_id", run.TurnID,
					"launch_id", run.LaunchID, "owner", answer.Node,
					"failure", string(rep.Failed))
			}
			return TailAnswer{}, &TailError{Node: answer.Node, TurnID: run.TurnID,
				Failure: rep.Failed}
		case rep.Paused:
			answer.Outcome = TailBoxPaused
		case rep.Output != nil:
			answer.Outcome, answer.Output = TailRunning, rep.Output
		default:
			// The owner re-read the record and the launch had stopped
			// running in between: its answer, not ours.
			answer.Outcome, answer.Status = notRunning(rep.Status), rep.Status
		}
		return answer, nil
	}
	answer.Outcome = TailOwnerSilent
	return answer, nil
}

// ServeTail makes this node an answerer for tail requests about the runs it
// owns, from its own readings.
//
// EVERY NODE SERVES and only the addressed incarnation answers — and it answers
// from its OWN read of the record, because the asker's read is a moment old
// and a collection may have landed since: a box read after its run was
// collected would show a finished job as running.
//
// EACH ANSWER IS BOUNDED BY ITS ASKER'S BUDGET. Every tail request a node owns
// shares one registration's answer slots, and an answer used to run on the
// registration's own context for as long as the box took — a box slow to
// read held its slot past the asker's patience, poll after poll, until the
// slots every run on this node needed were full of answers nobody would read.
//
// A FAILURE IS ANSWERED BY ITS CLASS AND LOGGED HERE ([tailFailureOf]), where
// its text is: condition is the rule this node tells a condition of its own
// from a fault by — the engine's one rule ([builtin.Condition]), whose
// sentinels belong to packages above this one, handed in rather than copied
// into a second list here. Nil is refused.
func ServeTail(ctx context.Context, q TailServer, owner string, pending PendingReader,
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
	return q.Serve(ctx, topics.ObserveSandboxTail, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req tailRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("sandbox: a tail request this build cannot read: %w", err)
		}
		if req.Owner != owner {
			return nil, errTailNotAddressed
		}
		ctx, cancel := context.WithTimeout(ctx, req.answerBudget())
		defer cancel()
		reply := tailReply{Version: tailWireVersion, Owner: owner}
		run, running, status, err := LiveLaunch(ctx, pending, req.TurnID, req.LaunchID)
		switch {
		case err != nil:
			reply.Failed = fail(ctx, req, err)
		case !running:
			reply.Status = status
			if feeds != nil {
				feeds.Forget(req.TurnID, req.LaunchID)
			}
		case run.Owner != owner:
			// The seat moved between the asker's read and this one; the
			// new owner is who can answer, and saying so is the honest
			// reply rather than a read of a box this node no longer drives.
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

// tailReply names its owner because a scatter's replies carry no sender.
type tailReply struct {
	Version int     `json:"v"`
	Owner   string  `json:"owner"`
	Output  *Output `json:"output,omitempty"`
	Status  string  `json:"status,omitempty"`

	// Failed is why the owner could not answer, as a class and never its
	// error's text, which stays in the owner's log ([TailFailure]).
	Failed TailFailure `json:"failed,omitempty"`

	// Paused is [TailBoxPaused]: the box is paused and was left so.
	Paused bool `json:"paused,omitempty"`
}

// TailFailure is why the node that owns a run could not answer a tail request,
// as its reply carries it: a CLASS, never the error's text. Every [Why] is one
// — a box failure its backend classed, which says what clears it — and so is
// each of [TailFailedMoving], [TailFailedUnanswered] and [TailFailedFault].
//
// A CLASS AND NOT THE TEXT, because the text was the wrong reader's twice
// over: it names hosts, statuses and a provider's own body, which belong in
// the log of the node that met them, and an asker handed only words could do
// nothing with them but report a fault of its own — a provider outage read,
// on the screen and over REST, as a node that broke.
type TailFailure string

const (
	// TailFailedMoving is a run whose seat left the owner between the
	// asker's read of its record and the owner's: the next request finds
	// the node it moved to.
	TailFailedMoving TailFailure = "moving"
	// TailFailedUnanswered is a condition of the owning node that waiting
	// clears: what it reads the run's record from did not answer, or it is
	// stopping and has let its readings go.
	TailFailedUnanswered TailFailure = "unanswered"
	// TailFailedFault is a fault of the owning node, which its log names.
	TailFailedFault TailFailure = "fault"
)

// Valid reports whether this build knows the failure: a [Why], or one of the
// owner's own.
func (f TailFailure) Valid() bool {
	switch f {
	case TailFailedMoving, TailFailedUnanswered, TailFailedFault:
		return true
	}
	return Why(f).Valid()
}

// ErrTailUnanswered is a tail request the node that owns its run could not
// answer for a reason of its own that waiting clears — [TailFailedMoving] and
// [TailFailedUnanswered]. The engine's condition rule tells it as a wait.
var ErrTailUnanswered = errors.New("sandbox: the node that owns the run could not answer just now")

// TailError is a tail request the node that owns its run answered with a
// [TailFailure]. It is classed by that failure alone: a [Why] by [WhyOf], the
// owner's conditions as [ErrTailUnanswered], and a fault — or a failure a
// newer build named that this one does not know — as nothing, which is what
// a fault is. Its text is for this node's log and names where the detail is.
type TailError struct {
	// Node is the node that owns the run, which logged the failure.
	Node string
	// TurnID is the run.
	TurnID string
	// Failure is what the owner answered.
	Failure TailFailure
}

func (e *TailError) Error() string {
	return fmt.Sprintf("sandbox: %s owns run %s and could not answer (%s); its log has why",
		e.Node, e.TurnID, e.Failure)
}

// Is makes the owner's conditions [ErrTailUnanswered].
func (e *TailError) Is(target error) bool {
	return target == ErrTailUnanswered &&
		(e.Failure == TailFailedMoving || e.Failure == TailFailedUnanswered)
}

// LoggedAtSource says the failure is in the owner's log, with its text: the
// asker's own line about it is a trace.
func (*TailError) LoggedAtSource() bool { return true }

// why is the class the owner's backend marked, where the failure is one.
func (e *TailError) why() (Why, bool) {
	why := Why(e.Failure)
	return why, why.Valid()
}

// tailFailureOf is the class of a failure the owning node met answering a
// tail request: the [Why] a backend marked it with, [TailFailedUnanswered]
// for one of its own a wait clears — a reading it let go ([ErrTailUnanswered])
// or what condition says is a condition — and [TailFailedFault] for the rest.
func tailFailureOf(err error, condition func(error) bool) TailFailure {
	if why, ok := WhyOf(err); ok {
		return TailFailure(why)
	}
	if errors.Is(err, ErrTailUnanswered) || condition(err) {
		return TailFailedUnanswered
	}
	return TailFailedFault
}

// logTailFailure writes a failure the owning node met, with its text, which is
// where the text goes and the only place: at WARN for a fault and for a class
// no wait clears, which somebody has to act on, and at DEBUG for one a wait
// clears — a screen asks again every few seconds for as long as it lasts, and
// a line per ask is noise.
func logTailFailure(ctx context.Context, turnID, launchID string, failure TailFailure, err error) {
	level := slog.LevelDebug
	if failure == TailFailedFault || (Why(failure).Valid() && !Why(failure).Waits()) {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "sandbox_tail_failed", "turn_id", turnID, "launch_id", launchID,
		"failure", string(failure), "error", err.Error())
}

// errNoBackend is a node that owns a run and has no sandbox backend to read
// its box through: an apply removed providers.sandbox while the run went on,
// which no wait changes and a person does.
func errNoBackend() error {
	return Because(WhyConfiguration, errors.New("sandbox: this node owns the run and has "+
		"no sandbox backend to reach its box with; providers.sandbox was removed by an apply"))
}
