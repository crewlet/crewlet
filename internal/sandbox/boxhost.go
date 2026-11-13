package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	// it finished where its turn's cap has passed, and ENDED first where it
	// still works then ([Coordinator.endJob]), its result read back, and the
	// box paused for whatever comes next ([Coordinator.readBox]).
	BoxReadCollect BoxRead = "collect"
	// BoxReadStop is a stopped run's read of what it had spent by then, its
	// job ended first ([Coordinator.endJob]), with nothing paused: its ending
	// reclaims the box next.
	BoxReadStop BoxRead = "stop"
	// BoxReadLoss is a lost run's box, read for the job's harness session
	// before its ending reclaims it ([Coordinator.keepLostSession]): the box
	// attached, never woken, its job ended where it still works, and nothing
	// else of it read — what a lost job spent is no question a loss asks.
	BoxReadLoss BoxRead = "loss"
)

// BoxReads are the reads this build makes.
var BoxReads = []BoxRead{BoxReadCollect, BoxReadStop, BoxReadLoss}

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
// FOUR KINDS OF FAILURE, each told apart because each is handled apart: the
// host said the box is gone (wrapping [ErrBoxGone]); the host did not answer
// ([ErrBoxHostSilent]), or stopped as it read, which is the same fact, carried
// apart from the host's own words ([BoxReadReply.Withdrawn]); the host's
// coordination store did not answer its read of the run's row, which licenses
// the read ([ErrStoreSilent]), carried apart the same way
// ([BoxReadReply.Unsettled]); and anything else the host said, which is the
// box's own failure to be read, counted like one read here.
type BoxReader interface {
	ReadBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error)
}

// ErrBoxHostSilent is a box read the node whose host the box is on did not
// answer: it is stopped, restarting, or gone, or it stopped as it read
// ([BoxReadReply.Withdrawn]).
//
// NOT A FAILURE OF THE BOX, which is why a collection handed it gives its
// claim back UNCOUNTED and acknowledges the completion: the host announces the
// completion again on its own next poll once it is back, and a host that never
// comes back is found gone by the fleet's waiter, which has the run's round
// carried on in a box of its own ([Waiter.goneAfter], [LossLaunchNodeGone]).
// Counted, a host that restarted for a minute would have its runs given up as
// unreadable.
var ErrBoxHostSilent = errors.New("sandbox: the node whose host the box is on did not answer")

// ErrStoreSilent is a step a collection cannot go on without that the
// coordination store did not answer: the host's read of the run's row, which
// licenses a read of its box ([Coordinator.ReadHostedBox]); the watch of the
// run's log a collection waits on its calls in flight by
// ([Coordinator.awaitCalls]); and the record of the sessions its job ran out of
// context in ([PendingStore.RetireSessions]). Each wraps the store's error in
// it ([storeSilent]).
//
// NOT A FAILURE OF THE BOX, which answered every read, nor of its host, which
// is up: a collection handed it gives its claim back UNCOUNTED and has its
// completion retried ([Coordinator.collectFailed]), a lost job's round is
// never carried on without its read for it ([Coordinator.collectGivenUp]), and
// a stop's read of it is asked again, for as long as a silent host is where the
// silence is the host's alone ([Coordinator.readStopped]). Counted, a
// store silent for the give-up window settled a run whose box held its result
// as one whose result was lost, and carried a lost job's round on uncharged
// while its box held what the job had spent.
var ErrStoreSilent = errors.New("sandbox: the coordination store did not answer a step a collection " +
	"cannot go on without")

// storeSilent is err, the store's error at a step a collection cannot go on
// without, wrapped in [ErrStoreSilent]: the store did not answer it.
//
// UNLESS ctx HAS ENDED, which is then the cause and is returned as it is. ctx
// is the context of the node that makes the step, which at the host's read of
// the run's row ([Coordinator.ReadHostedBox]) is the HOST'S: its answerer's own
// lifetime, held to its share of the asker's time. On the node that collects,
// the collection judges it as it judges every read its context ended
// ([Coordinator.collectFailed]): a cancellation of this node's own is handed
// back uncounted whatever it cut, and a deadline the delivery ran out of is
// counted, or a collection that always outlasts its delivery would be asked
// again for ever, its last step the store's. On the host, the reply says which
// ([AnswerBoxRead]): a cancellation is the host stopping as it read
// ([BoxReadReply.Withdrawn]), which its asker hands back uncounted, and a
// deadline is the read running out of the asker's time, which it counts.
func storeSilent(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStoreSilent, err)
}

// ErrBoxNotHosted is a box read refused by the node asked: the run is not one
// whose box is on its host, or the run is not in the state the read is made
// in — its row moved on since the asker read it.
var ErrBoxNotHosted = errors.New("sandbox: the node asked does not hold this run's box in the state the read needs")

// BoxReadBudget is how long a node waits for a box's host to make a read of
// run's box ([BoxReader]) — by what the read it asks may do there — before
// taking it for a host that did not answer.
//
// [pollBound] — sixty seconds, the control plane's whole-request budget — for
// the reason the poll is bounded by it: what the host does is the poll's own
// work (a reconnect, a probe) and the run's own files read back and its box
// paused, all of it on its own host. And the TAKE of the job's harness session
// beside it ([SessionTakeBudget]), with the room the take leaves after it —
// once for its own records, the stamp of what came of it and the digest it
// leaves in the box, beside which a collection records how its job's harness
// ended, with no time of its own ([Coordinator.markHarnessEnd]), and once for
// the pause and the answer ([discardGrace] each, [Coordinator.takeContext]) —
// because the read is where the session is taken out of the box, on the host
// and before the box is paused: the host reads only for its share of the
// budget ([BoxReadAnswerShare]), so all of it is counted at that share, and a
// take at its bound fits a host's read whole rather than being cut by the
// host's own deadline. Only the take: the upload and the record are made after
// the host has answered, off the read ([Coordinator.keepCapture]), so no asker
// waits on an upload.
//
// AND A READ THAT MAY END THE JOB HOLDS THE JOB'S END BEFORE ITS TAKE
// ([JobEndBudget]): a stop's and a loss's end it first ([Coordinator.endJob]),
// and so does a collection's where the turn's cap has passed with the job still
// working ([Coordinator.readBox]), at most the end's wait and the kill past it,
// and that is counted at the host's share as the take is. The host's share of
// such a read's budget is then the end's 145 seconds, the take's 240 and its two
// graces, and 45 — the poll's bound at the share — for the read's own steps, so
// a take after an end at its bound is handed its whole budget all the same
// ([Coordinator.takeContext]).
//
// EVERY COLLECTION OF A TURN WITH A CAP holds the end, whether or not the cap
// has passed on the asker's clock: the host decides whether to end the job on
// its own clock, as it reads, and nothing in a completion tells the asker
// whether its job finished or the host's waiter announced it at the cap. A
// collection of a turn with none never ends its job, and waits for the take
// alone.
//
// What that costs is the wait for a host that has truly stopped — about eight
// minutes for a collection of a turn with no cap before the asker hands the
// claim back, where it was one, and about eleven for every other read — which a
// host that is only slow, ending a job at its bound or packing a session at its
// bound, must not be taken for. Ordinary reads answer in seconds, a job ending
// in a few and a session of a day's work being a mebibyte or two. A read that
// has not finished in that is a host that is not answering, and is answered as
// one ([ErrBoxHostSilent]); it announces the completion again from its own next
// poll.
func BoxReadBudget(run PendingRun, read BoxRead) time.Duration {
	held := SessionTakeBudget + 2*discardGrace
	if read.mayEnd(run) {
		held += JobEndBudget
	}
	return pollBound + time.Duration(float64(held)/BoxReadAnswerShare)
}

// mayEnd reports whether the read may end run's job before it takes its
// session ([Coordinator.endJob]): a stop's and a loss's always do, and a
// collection's does where run's turn has a cap ([PendingRun.Deadline]), since
// it ends a job still working once that cap has passed ([Coordinator.readBox]).
func (r BoxRead) mayEnd(run PendingRun) bool {
	switch r {
	case BoxReadStop, BoxReadLoss:
		return true
	case BoxReadCollect:
		return !run.Deadline.IsZero()
	}
	return false
}

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
	// Unsettled is a read that failed because the host's coordination store
	// did not answer a step it cannot go on without ([ErrStoreSilent]), Error
	// saying which: a flag of its own, since Error is only words, and an asker
	// that could not tell the store's silence from the box's failure would
	// count it against a box that is up. Added within [BoxReadWireVersion] 1.
	Unsettled bool `json:"unsettled,omitempty"`
	// Withdrawn is a read the host's own lifetime cut short, Error saying
	// where: the host withdrew as an answerer of box reads (internal/queue's
	// AnswerFunc runs under the answerer's context, which its withdrawal
	// cancels), draining or restarting, while it read. No failure of the
	// box, which a host going away never finished reading: the asker reads it
	// as a host that did not answer ([ErrBoxHostSilent]), whose reply still
	// crosses the broker, so it learns that now rather than at its deadline.
	// A read its share of the asker's time cut is no withdrawal: that is the
	// box's read running out of time, said in Error alone. Added within
	// [BoxReadWireVersion] 1: an asker that does not know the flag reads Error
	// as the box's failure and counts it, as one always did.
	Withdrawn bool `json:"withdrawn,omitempty"`
}

// Answers is the id of the request this reply answers.
func (r BoxReadReply) Answers() string { return r.Request }

// Err is the reply as the asker acts on it (see [BoxReader] for the four kinds
// of failure), or nil for a reply carrying the run's account. A failure the
// reply flags reads as what its flag names, in the host's own words, with the
// sentinel's sentence said once ([hostSaid]).
func (r BoxReadReply) Err() error {
	switch {
	case r.Gone:
		return hostSaid("sandbox: node "+r.Node+" found the box gone", r.Error, ErrBoxGone)
	case r.Refused != "":
		return hostSaid("sandbox: node "+r.Node+" refused the read", r.Refused, ErrBoxNotHosted)
	case r.Withdrawn:
		return hostSaid("sandbox: node "+r.Node+" stopped as it read the box", r.Error, ErrBoxHostSilent)
	case r.Unsettled:
		// THE HOST'S STORE, NEVER THE BOX: the row that licenses the read
		// was not read, so no read of the box was made to fail.
		return hostSaid("sandbox: node "+r.Node+"'s coordination store did not answer", r.Error, ErrStoreSilent)
	case r.Error != "":
		return fmt.Errorf("sandbox: node %s could not read the box: %s", r.Node, r.Error)
	case r.Result == nil:
		return fmt.Errorf("sandbox: node %s answered the box read with no account of the run", r.Node)
	}
	return nil
}

// hostSaid is a failure a host's reply flags, as its asker reads it: lead, what
// the flag names, then the host's own words, then kind's sentence, ONLY where
// those words do not hold it already. A host's words are its error's text,
// which holds the sentinel's sentence where that text was built around the
// sentinel, as a refused read's and the store's silence's always are: the
// sentence added again made the asker's log say each of those twice in one
// line. A backend's own sentence for a box that is gone ([boxGone]) does not
// hold it, the sentinel riding beside the text rather than in it, nor does a
// read the host's withdrawal cut, and each is said with it once. errors.Is
// answers kind, and no other sentinel, either way: the words are text, and only
// the flag says what they are.
func hostSaid(lead, words string, kind error) error {
	text := lead + ": " + words
	if !strings.Contains(words, kind.Error()) {
		text += ": " + kind.Error()
	}
	return &flaggedFailure{text: text, kind: kind}
}

// flaggedFailure is a failure a host's reply flags ([hostSaid]): its text, and
// the sentinel the flag stands for.
type flaggedFailure struct {
	text string
	kind error
}

func (f *flaggedFailure) Error() string { return f.text }

func (f *flaggedFailure) Unwrap() error { return f.kind }

// AnswerBoxRead is a host's reply to one admitted box read: the read made by
// host ([Coordinator.ReadHostedBox]), or why it was not.
//
// ctx IS THE HOST'S OWN: its answerer's lifetime, which its withdrawal cancels,
// held to its share of the asker's time. A read that failed once it was
// cancelled is said apart from every other failure, whatever step the
// cancellation cut ([BoxReadReply.Withdrawn]), unless it had found the box gone
// or the row refusing the read, which the host decided before it stopped; one
// its deadline cut is the read running out of the asker's time, and is said in
// the host's words alone.
func AnswerBoxRead(ctx context.Context, host *Coordinator, req BoxReadRequest) BoxReadReply {
	reply := BoxReadReply{Version: BoxReadWireVersion, Request: req.ID, Node: host.node}
	result, ended, err := host.ReadHostedBox(ctx, req.TurnID, req.LaunchID, req.SandboxID, req.Read)
	switch {
	case errors.Is(err, ErrBoxNotHosted):
		reply.Refused = err.Error()
	case errors.Is(err, ErrBoxGone):
		reply.Gone, reply.Error = true, err.Error()
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		// THE HOST STOPPING, not the box, nor its store: a step it cut
		// fails with the cancellation's own error, which storeSilent leaves
		// as it is, or with whatever the step saw as it ended.
		reply.Withdrawn, reply.Error = true, err.Error()
	case errors.Is(err, ErrStoreSilent):
		reply.Unsettled, reply.Error = true, err.Error()
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
// claimed ([StatusResumed]), a stop's on a run whose stop is decided and not
// yet read, and a loss's on a run whose loss is decided and whose job's
// session no take of it ended has settled ([Coordinator.capturesLoss]): beside
// one still being kept, the loss's read stands by for it. So a request —
// even a replayed or a forged one past the channel's own checks — can make no
// read the run's own protocol is not making: a running job's box is never
// paused under it, and no job is ended under it that is not already decided
// stopped or lost, or claimed past its turn's cap — as this node's clock reads
// the row's, or as the job's record says an earlier collection of it found it
// ([LaunchRecord.EndedAtDeadline]) — a stop's and a loss's read end the job
// first, and a collection's a job still working past its cap
// ([Coordinator.endJob]), here on its host, the only node that can.
func (c *Coordinator) ReadHostedBox(ctx context.Context, turnID, launchID, sandboxID string,
	read BoxRead) (Result, bool, error) {

	if !read.Valid() {
		return Result{}, false, fmt.Errorf("%w: %q is no read this build makes", ErrBoxNotHosted, read)
	}
	run, found, err := c.pending.Get(ctx, turnID)
	if err != nil {
		// THE STORE'S SILENCE, NEVER THE BOX'S ([ErrStoreSilent]): the row is
		// the license, and a read it did not license was never made, so the
		// asker hears that the store did not answer rather than that the box
		// could not be read.
		return Result{}, false, storeSilent(ctx, fmt.Errorf("sandbox: reading run %s to read its box: %w",
			turnID, err))
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
	case read == BoxReadLoss && !c.capturesLoss(run):
		return Result{}, false, fmt.Errorf("%w: run %s has no loss decided whose box a capture reads",
			ErrBoxNotHosted, turnID)
	case read == BoxReadLoss:
		// THE LAST READ OF THE BOX ANYTHING MAKES, licensed by the loss its
		// ending decided: the job ended and its session taken here, and kept
		// off the read.
		spooled, kept := c.readLost(ctx, run)
		c.keepLater(spooled)
		return Result{Kept: kept}, false, nil
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
