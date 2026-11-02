package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Who may touch a box.
//
// # A local box is its launching node's
//
// A box in a `direct` or `container` cell is a process tree or a container on
// the host of the node that launched it ([PendingRun.LaunchNode]), and no
// other node can reach it: asked for by id anywhere else, the local backend
// finds no such box and says it is gone ([ErrBoxGone]). The run outlives that
// fact — the seat it belongs to can move to another node while the job is
// still working (a rebalance, a drain), and its tool calls follow the seat
// through the bridge — so every touch of the box itself is made on its host:
//
//   - THE POLL, THE KEEPALIVE AND THE PAUSE REAPER are the launching node's
//     own waiter ([DrivesLocal]), on every node, whoever holds the fleet's
//     waiter duty; the duty's waiter drives only the boxes every node reaches
//     ([DrivesRemote]).
//   - THE COLLECTION is made by the seat's holder, which claims the
//     completion and resumes the turn — and where that is not the box's host,
//     the part that reads the box is ASKED of the host ([BoxReader]) and the
//     rest, the part a model may write, is made here ([Coordinator.collect]).
//     So is a stopped run's read of what it spent.
//   - THE RECLAIM of a box its row no longer names is the host's: its own
//     waiter reclaims every box on its host whose run no longer names it
//     ([Waiter.sweep]), and a node that ends such a run, or relaunches it,
//     leaves the box to it and tells it to look now ([Nudge]).
//
// A box in a cell every node reaches (`e2b`) is touched wherever the work is,
// as it always was.

// BoxRead is which read of a box a node asks its host to make.
type BoxRead string

const (
	// BoxReadCollect is a completion's collection: the job asked whether
	// it finished where its turn's cap has passed, its result read back,
	// and the box paused for whatever comes next ([Coordinator.readBox]).
	BoxReadCollect BoxRead = "collect"
	// BoxReadStop is a stopped run's read of what it had spent by then,
	// with nothing paused: its ending reclaims the box next.
	BoxReadStop BoxRead = "stop"
)

// BoxReads are the reads this build makes.
var BoxReads = []BoxRead{BoxReadCollect, BoxReadStop}

// Valid reports whether this build knows the read.
func (r BoxRead) Valid() bool { return slices.Contains(BoxReads, r) }

// BoxReader asks the node whose host a run's box is on to read it back — the
// part of a collection that touches the box ([Coordinator.readBox]) — for a
// node that holds the run's seat and not its box.
//
// What comes back is the run's own account, floored and with its transcript
// and refs bounded ([boundBox]), and whether its turn's cap ended it; the
// asker fits the rest ([Coordinator.fitText]).
//
// THREE KINDS OF FAILURE, each told apart because each is handled apart: the
// host said the box is gone (wrapping [ErrBoxGone]); the host did not answer
// ([ErrBoxHostSilent]); and anything else the host said, which is the box's
// own failure to be read, counted like one read here.
type BoxReader interface {
	ReadBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error)
}

// ErrBoxHostSilent is a box read the node whose host the box is on did not
// answer: it is stopped, restarting, or gone.
//
// NOT A FAILURE OF THE BOX, which is why a collection handed it gives its
// claim back UNCOUNTED and acknowledges the completion: the host announces the
// completion again on its own next poll once it is back, and a host that never
// comes back is found gone by the fleet's waiter, which has the run's round
// carried on in a box of its own ([Waiter.goneAfter], [LossLaunchNodeGone]).
// Counted, a host that restarted for a minute would have its runs given up as
// unreadable.
var ErrBoxHostSilent = errors.New("sandbox: the node whose host the box is on did not answer")

// ErrBoxNotHosted is a box read refused by the node asked: the run is not one
// whose box is on its host, or the run is not in the state the read is made
// in — its row moved on since the asker read it.
var ErrBoxNotHosted = errors.New("sandbox: the node asked does not hold this run's box in the state the read needs")

// BoxReadBudget is how long a node waits for a box's host to read the box back
// ([BoxReader]) before taking it for a host that did not answer.
//
// [pollBound] — sixty seconds, the control plane's whole-request budget — for
// the reason the poll is bounded by it: what the host does is the poll's own
// work (a reconnect, a probe) and the run's own files read back and its box
// paused, all of it on its own host. A read that has not finished in that
// is a host that is not answering, and is answered as one ([ErrBoxHostSilent]);
// it announces the completion again from its own next poll.
const BoxReadBudget = pollBound

// BoxReadAnswerShare is the part of [BoxReadBudget] a host's read may take,
// leaving the rest for its answer to cross the broker before the asker stops
// listening: [answerShare], a tail's.
const BoxReadAnswerShare = answerShare

// BoxReadChannel names the signed channel a box read crosses
// (internal/signedask): an INSTRUCTION, since the host carries it out —
// it pauses the box a collection reads — and the fleet signs what a node
// carries out for another.
const BoxReadChannel = "sandbox.box_read"

// BoxReadWireVersion is the request and reply shape this build speaks.
// Evolution is ADDITIVE within a version.
const BoxReadWireVersion = 1

// BoxReadRequest is one box read, as it crosses the broker.
type BoxReadRequest struct {
	Version int `json:"v"`
	// ID and Deadline bind the request: see internal/signedask.
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	// Node is the node asked — the one whose host the box is on
	// ([PendingRun.LaunchNode]). Every node serves the subject and only
	// that one answers.
	Node      string  `json:"node"`
	TurnID    string  `json:"turn_id"`
	LaunchID  string  `json:"launch_id"`
	SandboxID string  `json:"sandbox_id"`
	Read      BoxRead `json:"read"`
}

// Binding is the id and the deadline the request was minted under.
func (r BoxReadRequest) Binding() (string, time.Time) { return r.ID, r.Deadline }

// BoxReadReply is a host's answer to one box read: the run's account read off
// its box ([Coordinator.ReadHostedBox]), or why there is none.
type BoxReadReply struct {
	Version int `json:"v"`
	// Request is the id of the request this answers.
	Request string `json:"request"`
	// Node is the node that answered, which a scatter's replies do not
	// otherwise carry.
	Node string `json:"node"`
	// Result is the run's account, floored and with its transcript and
	// refs bounded ([boundBox]), its text held to what one reply carries
	// ([CarriedResult]); Ended is whether its turn's cap ended it. Nil on
	// every other answer.
	Result *Result `json:"result,omitempty"`
	Ended  bool    `json:"ended,omitempty"`
	// Gone is a box the host found no longer there ([ErrBoxGone]).
	Gone bool `json:"gone,omitempty"`
	// Refused is why the host made no read: the run's row does not license
	// it ([ErrBoxNotHosted]).
	Refused string `json:"refused,omitempty"`
	// Error is a read that failed, in the host's own words.
	Error string `json:"error,omitempty"`
}

// Answers is the id of the request this reply answers.
func (r BoxReadReply) Answers() string { return r.Request }

// Err is the reply as the asker acts on it — see [BoxReader] for the three
// kinds of failure — or nil for a reply carrying the run's account.
func (r BoxReadReply) Err() error {
	switch {
	case r.Gone:
		return fmt.Errorf("sandbox: node %s found the box gone: %s: %w", r.Node, r.Error, ErrBoxGone)
	case r.Refused != "":
		return fmt.Errorf("%w: node %s: %s", ErrBoxNotHosted, r.Node, r.Refused)
	case r.Error != "":
		return fmt.Errorf("sandbox: node %s could not read the box: %s", r.Node, r.Error)
	case r.Result == nil:
		return fmt.Errorf("sandbox: node %s answered the box read with no account of the run", r.Node)
	}
	return nil
}

// AnswerBoxRead is a host's reply to one admitted box read: the read made by
// host ([Coordinator.ReadHostedBox]), or why it was not.
func AnswerBoxRead(ctx context.Context, host *Coordinator, req BoxReadRequest) BoxReadReply {
	reply := BoxReadReply{Version: BoxReadWireVersion, Request: req.ID, Node: host.node}
	result, ended, err := host.ReadHostedBox(ctx, req.TurnID, req.LaunchID, req.SandboxID, req.Read)
	switch {
	case errors.Is(err, ErrBoxNotHosted):
		reply.Refused = err.Error()
	case errors.Is(err, ErrBoxGone):
		reply.Gone, reply.Error = true, err.Error()
	case err != nil:
		reply.Error = err.Error()
	default:
		reply.Result, reply.Ended = &result, ended
	}
	return reply
}

// hosts reports whether this node can reach run's box itself: one in a cell
// every node reaches, or one on this node's own host — launched here.
//
// A coordinator with no manager reaches nothing and reads nothing either way,
// so it answers true and lets the manager's own refusal say why.
func (c *Coordinator) hosts(run PendingRun) bool {
	m := c.mgr()
	return m == nil || !m.OnLaunchHost(run) || run.LaunchNode == c.node
}

// boxResult is the part of a read that touches the box, made where the box
// is: by this node where it hosts the box, and asked of the node that does
// otherwise ([BoxReader]).
func (c *Coordinator) boxResult(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error) {
	if c.hosts(run) {
		return c.readBox(ctx, run, read)
	}
	if c.boxes == nil {
		// A WIRING FAULT: a node that runs seats is given the way to ask
		// a peer for a box. Said as a failure to read the box, which is
		// what it is from here — counted, so it ends in time.
		return Result{}, false, fmt.Errorf("sandbox: run %s's %s box is on %s's host, and this "+
			"node was given no way to ask that node to read it", run.TurnID,
			c.mgr().CellOf(run), run.LaunchNode)
	}
	return c.boxes.ReadBox(ctx, run, read)
}

// ReadHostedBox makes, for the node that asked, the read of a box on this
// node's host ([BoxReader]): the run's row read again here, and the read made
// only where the row still licenses it.
//
// THE ROW IS THE LICENSE, not the request. The read is made only for the job
// and the box the asker named, on a run whose box this node launched, in the
// state the read is made in: a collection's on a run whose completion is
// claimed ([StatusResumed]), and a stop's on a run whose stop is decided and
// not yet read. So a request — even a replayed or a forged one past the
// channel's own checks — can make no read the run's own protocol is not
// making: a running job's box is never paused under it.
func (c *Coordinator) ReadHostedBox(ctx context.Context, turnID, launchID, sandboxID string,
	read BoxRead) (Result, bool, error) {

	if !read.Valid() {
		return Result{}, false, fmt.Errorf("%w: %q is no read this build makes", ErrBoxNotHosted, read)
	}
	run, found, err := c.pending.Get(ctx, turnID)
	if err != nil {
		return Result{}, false, fmt.Errorf("sandbox: reading run %s to read its box: %w", turnID, err)
	}
	switch {
	case !found:
		return Result{}, false, fmt.Errorf("%w: run %s has no record", ErrBoxNotHosted, turnID)
	case run.LaunchID != launchID || run.SandboxID != sandboxID:
		return Result{}, false, fmt.Errorf("%w: run %s holds job %s in box %s, not job %s in box %s",
			ErrBoxNotHosted, turnID, run.LaunchID, run.SandboxID, launchID, sandboxID)
	case run.LaunchNode != c.node || !c.mgr().OnLaunchHost(run):
		return Result{}, false, fmt.Errorf("%w: run %s's box is not on this node's host",
			ErrBoxNotHosted, turnID)
	case read == BoxReadCollect && run.Status != StatusResumed:
		return Result{}, false, fmt.Errorf("%w: run %s is %s, and a collection reads the box of "+
			"a run whose completion is claimed", ErrBoxNotHosted, turnID, run.Status)
	case read == BoxReadStop && (run.Stop == nil || run.Stop.Collected):
		return Result{}, false, fmt.Errorf("%w: run %s has no stop waiting to read its box",
			ErrBoxNotHosted, turnID)
	}
	return c.readBox(ctx, run, read)
}

// tellHost tells the node whose host run's box is on to look at the run now
// ([NudgeReleased]): this node has just left that box to it — an ending that
// would have reclaimed it, a relaunch that does not continue in it — and its
// own waiter reclaims it the moment no row names it, rather than at its next
// pass. Best effort: the pass is the guarantee.
func (c *Coordinator) tellHost(ctx context.Context, run PendingRun) {
	if c.nudge == nil {
		return
	}
	c.nudge(ctx, Nudge{TurnID: run.TurnID, LaunchID: run.LaunchID, Why: NudgeReleased})
}
