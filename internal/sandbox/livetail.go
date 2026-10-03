package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// What a running coding run has said so far, pulled from the node that owns it.
//
// # Why a pull, and why from the owner
//
// A detached run writes its own account of itself into its box — the streamed
// transcript a runner parses, or its stderr — and nothing leaves the box until
// the run is collected, when the whole of it rides the run's
// `agent_phase_completed{phase: sandbox}` record. Between the launch and the
// collection a person watching the run had nothing: the run could be minutes
// or hours in, and the turn trace showed a bar and no words.
//
// A PUBLISHED stream of it was the alternative, and it is the wrong one: an
// event per poll per run would fill the event store with the same text over and
// over for nobody, since almost nobody is watching almost every run. So it is a
// QUESTION, asked only while somebody looks: `sandbox_tail{turn_id, launch_id}`,
// answered from the box by the node whose incarnation the run's record names as
// its owner ([PendingRun.Owner]). There is no event and no row.
//
// # Three outcomes, never two
//
// The TAIL itself ([Output], possibly empty — a run that has written nothing
// yet has nothing to show). `not_running`, with the record's own status, when
// the launch asked about is not a running job: it finished and was collected,
// it parked on a question, a later launch replaced it, or no record is left at
// all. And `owner_silent{node}` when the node that owns the run did not answer
// inside the fleet read budget — NAMED, because "the run said nothing" and "the
// node that could ask it did not answer" send a reader to opposite places, and
// collapsing the second into an empty tail is exactly the lie a screen that
// polls cannot recover from. A fourth, `owner_upgrading{node}`, is the owner's
// BUILD saying it serves no such question — what a rolling upgrade looks like —
// which is neither silence (retrying cannot help) nor a run that stopped.
//
// A box that the owner could not read is an ERROR rather than an outcome: it
// clears by asking again, and the reader shows the owner's own sentence.

// MaxLiveOutputBytes is how much of a running job's account of itself one peek
// carries: the LAST 8 KiB.
//
// A peek answers "what is it doing now", polled every few seconds while a
// person has the run open, and every answer crosses the broker whole. 8 KiB is
// a hundred-odd lines of a transcript — the current step and the few before it
// — at a size a poll can repeat indefinitely without being noticed; the WHOLE
// transcript is on the run's phase record the moment it is collected
// ([github.com/crewlet/crewlet/internal/sandbox/codingagent.MaxTranscriptBytes],
// 256 KiB), so nothing is lost by the live view being a window.
const MaxLiveOutputBytes = 8 << 10

// TailReadBudget is how long a tail request waits for the owning node to
// answer: the fleet read budget every other scatter in this engine answers
// inside (eventfan's FleetReadBudget, memread's DefaultBudget). The dashboard
// polls every three seconds, so a budget under the interval means at most one
// request is in flight per open run.
const TailReadBudget = 2 * time.Second

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

// Output is what a job has said about itself so far — a [Runner.Peek].
type Output struct {
	// Text is the last [MaxLiveOutputBytes] of it, REDACTED: the box's
	// environment holds the seat's credentials, and this reaches a screen.
	Text string `json:"text"`
	// Source says which account Text is.
	Source OutputSource `json:"source"`
	// Cut is whether Text lost its front to the bound.
	Cut bool `json:"cut"`
	// AsOf is when the box was read, on the owning node's clock.
	AsOf time.Time `json:"as_of"`
	// Finished is whether the job has written its done marker (or a
	// terminal event): it is over and waiting to be collected, so this
	// output will not grow again.
	Finished bool `json:"finished"`
}

// TailOutcome is what one tail request concluded.
type TailOutcome string

const (
	// TailRunning is an answer: the tail of a running job.
	TailRunning TailOutcome = "tail"
	// TailNotRunning is a launch that is not a running job — see
	// [LiveLaunch].
	TailNotRunning TailOutcome = "not_running"
	// TailOwnerSilent is an owning node that did not answer inside the
	// budget, or a run no node holds right now.
	TailOwnerSilent TailOutcome = "owner_silent"
	// TailOwnerUpgrading is an owning node whose build serves no tail
	// request ([coord.FeatureSandboxTail]).
	TailOwnerUpgrading TailOutcome = "owner_upgrading"
)

// Valid reports whether this build knows the outcome.
func (o TailOutcome) Valid() bool {
	switch o {
	case TailRunning, TailNotRunning, TailOwnerSilent, TailOwnerUpgrading:
		return true
	}
	return false
}

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

// TailAsker scatters one request — the half of [queue.EventQueue] a tail needs.
type TailAsker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// TailFeatures says what one incarnation's build can do — the half of
// [coord.FeatureReader] a tail needs.
type TailFeatures interface {
	OwnerFeature(ctx context.Context, owner string, feature coord.Feature) (bool, error)
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

	// Manager is this node's sandbox manager, resolved per call because an
	// apply swaps it; nil on a node with no sandbox backend.
	Manager func() *Manager

	// Queue asks a peer. Nil is a node with no broker, which is the whole
	// fleet: every run is its own.
	Queue TailAsker

	// Features says whether the owning incarnation's build answers a tail
	// request at all. Required with Queue, for internal/learning/memread's
	// reason: asking
	// an older build waits out the whole budget for a reply that can never
	// come, on every poll, and then reports an owner that "did not answer".
	Features TailFeatures

	// Budget bounds a peer's answer; zero is [TailReadBudget].
	Budget time.Duration

	// Now stamps a local peek; nil is the wall clock.
	Now func() time.Time
}

func (r *TailReader) budget() time.Duration {
	if r.Budget > 0 {
		return r.Budget
	}
	return TailReadBudget
}

// Tail answers one request.
func (r *TailReader) Tail(ctx context.Context, turnID, launchID string) (TailAnswer, error) {
	answer := TailAnswer{TurnID: turnID, LaunchID: launchID}
	run, running, status, err := LiveLaunch(ctx, r.Pending, turnID, launchID)
	if err != nil {
		return TailAnswer{}, err
	}
	if !running {
		answer.Outcome, answer.Status = TailNotRunning, status
		answer.Node = nodeOf(run.Owner)
		return answer, nil
	}
	answer.Node = nodeOf(run.Owner)
	switch {
	case run.Owner == "":
		// UNCLAIMED: the instant between an owner letting the seat go and
		// the next one recovering its run. Nobody can ask the box, and
		// "silent" with no node says exactly that.
		answer.Outcome = TailOwnerSilent
		return answer, nil
	case run.Owner == r.Owner || r.Queue == nil:
		out, err := peekLocal(ctx, r.Manager, run, r.now())
		if err != nil {
			return TailAnswer{}, err
		}
		answer.Outcome, answer.Output = TailRunning, &out
		return answer, nil
	}
	return r.ask(ctx, run, answer)
}

func (r *TailReader) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// ask puts the request to the incarnation the record names.
func (r *TailReader) ask(ctx context.Context, run PendingRun, answer TailAnswer) (TailAnswer, error) {
	serves, err := r.Features.OwnerFeature(ctx, run.Owner, coord.FeatureSandboxTail)
	if err != nil {
		return TailAnswer{}, fmt.Errorf("sandbox: whether %s, which owns run %s, can answer "+
			"could not be read: %w", answer.Node, run.TurnID, err)
	}
	if !serves {
		answer.Outcome = TailOwnerUpgrading
		return answer, nil
	}
	raw, err := json.Marshal(tailRequest{
		Version: tailWireVersion, TurnID: run.TurnID, LaunchID: run.LaunchID, Owner: run.Owner,
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
		case rep.Error != "":
			return TailAnswer{}, fmt.Errorf("sandbox: %s owns run %s and answered: %s",
				answer.Node, run.TurnID, rep.Error)
		case rep.Output != nil:
			answer.Outcome, answer.Output = TailRunning, rep.Output
		default:
			// The owner re-read the record and the launch had stopped
			// running in between: its answer, not ours.
			answer.Outcome, answer.Status = TailNotRunning, rep.Status
		}
		return answer, nil
	}
	answer.Outcome = TailOwnerSilent
	return answer, nil
}

// peekLocal reads a run's box on this node.
func peekLocal(ctx context.Context, manager func() *Manager, run PendingRun, now time.Time) (Output, error) {
	var m *Manager
	if manager != nil {
		m = manager()
	}
	if m == nil {
		return Output{}, errors.New("sandbox: this node owns the run and has no sandbox " +
			"backend to reach its box with — providers.sandbox was removed by an apply")
	}
	box, runner, err := m.Reconnect(ctx, Placement(run.Placement), run.SandboxID, run.CodingAgent)
	if err != nil {
		return Output{}, fmt.Errorf("sandbox: reach the box of run %s: %w", run.TurnID, err)
	}
	out, err := runner.Peek(ctx, box, RunHandle{CommandID: run.CommandID, SessionID: run.SessionID})
	if err != nil {
		return Output{}, fmt.Errorf("sandbox: read the box of run %s: %w", run.TurnID, err)
	}
	if out.AsOf.IsZero() {
		out.AsOf = now
	}
	if !out.Source.Valid() {
		out.Source = SourceNone
	}
	return out, nil
}

// ServeTail makes this node an answerer for tail requests about the runs it
// owns.
//
// EVERY NODE SERVES and only the addressed incarnation answers — and it answers
// from its OWN read of the record, because the asker's read is a moment old
// and a collection may have landed since: a box read after its run was
// collected would show a finished job as running.
func ServeTail(ctx context.Context, q TailServer, owner string, pending PendingReader,
	manager func() *Manager, now func() time.Time,
) (queue.Unsubscribe, error) {
	return q.Serve(ctx, topics.ObserveSandboxTail, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req tailRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("sandbox: a tail request this build cannot read: %w", err)
		}
		if req.Owner != owner {
			return nil, errTailNotAddressed
		}
		reply := tailReply{Version: tailWireVersion, Owner: owner}
		run, running, status, err := LiveLaunch(ctx, pending, req.TurnID, req.LaunchID)
		switch {
		case err != nil:
			reply.Error = "its record of the run could not be read: " + err.Error()
		case !running:
			reply.Status = status
		case run.Owner != owner:
			// The seat moved between the asker's read and this one; the
			// new owner is who can answer, and saying so is the honest
			// reply rather than a peek at a box this node no longer drives.
			reply.Error = "the run moved to " + nodeOf(run.Owner) + " — ask again"
		default:
			stamp := time.Now().UTC()
			if now != nil {
				stamp = now().UTC()
			}
			out, err := peekLocal(ctx, manager, run, stamp)
			if err != nil {
				log.WarnContext(ctx, "sandbox_tail_failed", "turn_id", run.TurnID, "error", err.Error())
				reply.Error = err.Error()
			} else {
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

type tailRequest struct {
	Version  int    `json:"v"`
	TurnID   string `json:"turn_id"`
	LaunchID string `json:"launch_id"`
	Owner    string `json:"owner"`
}

// tailReply names its owner because a scatter's replies carry no sender.
type tailReply struct {
	Version int     `json:"v"`
	Owner   string  `json:"owner"`
	Output  *Output `json:"output,omitempty"`
	Status  string  `json:"status,omitempty"`
	Error   string  `json:"error,omitempty"`
}
