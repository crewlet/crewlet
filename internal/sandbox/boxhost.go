package sandbox

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
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
// THREE KINDS OF FAILURE, each told apart because each is handled apart: the
// host said the box is gone (wrapping [ErrBoxGone]); the host gave no account
// of the box ([ErrBoxHostSilent]) — it did not answer, or answered that it was
// busy with other reads of its boxes ([ErrBoxHostBusy]), or this node could
// not put the read to it at all ([ErrBoxNotAsked]), each of which says nothing
// about the box and is handled as a host that did not answer; and anything
// else the host said, which is the box's own failure to be read, counted like
// one read here.
type BoxReader interface {
	ReadBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error)
}

// ErrBoxHostSilent is a box read the node whose host the box is on gave no
// account of the box: it is stopped, restarting, or gone — or it answered that
// it was busy ([ErrBoxHostBusy]), or this node could not ask it
// ([ErrBoxNotAsked]), which wrap this.
//
// NOT A FAILURE OF THE BOX, which is why a collection handed it gives its
// claim back UNCOUNTED and acknowledges the completion: the host announces the
// completion again on its own next poll once it is back, and a host that never
// comes back is found gone by the fleet's waiter, which has the run's round
// carried on in a box of its own ([Waiter.goneAfter], [LossLaunchNodeGone]).
// Counted, a host that restarted for a minute would have its runs given up as
// unreadable.
var ErrBoxHostSilent = errors.New("sandbox: the node whose host the box is on did not answer")

// ErrBoxNotAsked is a box read this node could not put to the box's host at all
// — its broker unreachable, or no time left to ask: no evidence about the box,
// and none about its host.
var ErrBoxNotAsked = fmt.Errorf("%w: this node could not ask it", ErrBoxHostSilent)

// ErrBoxHostBusy is a box read the host answered without reading
// ([BoxReadReply.Busy]): no evidence about the box, but the host is there.
var ErrBoxHostBusy = fmt.Errorf("%w: it was busy with other reads of its boxes", ErrBoxHostSilent)

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
// beside it ([SessionTakeBudget]), and the room its reservation of the
// capture's place is given beside it ([Coordinator.reserveRoom]), with the
// room the take leaves after it: a grace for its own record (the stamp of
// what came of it, or the digest it leaves in the box), another for how a
// collection's job's harness ended ([Coordinator.markHarnessEnd],
// [Coordinator.takeRecordsRoom]), and a third for the pause, its record and
// the answer ([Coordinator.pauseRoom]). They are counted because the read is
// where the session is taken out of the box, on the host and before the box is
// paused: the host reads only for its share of the wait the request carries,
// counted from the moment the request reaches it, on its own clock
// ([BoxReadRequest.AnswerBudget]), so all of it is counted at that share, and
// a take at its bound fits whole a read the host makes as the request arrives
// rather than being cut by the host's own deadline. A read that first waited
// for one of the reads a host makes at once is handed only what is left of the
// share, so one that waited long may be cut by that deadline and answer that
// it could not read the box: reading on would take the session out of the box
// for an asker that had gone. Only the take: the upload and the record are
// made off the read, from the moment the session is spooled
// ([Coordinator.keepCapture]), so no asker waits on an upload.
//
// # Each change of the run's row the host's read makes, at the slowest crossing
//
// The reservation, the stamp of what came of the take, how the job's harness
// ended and the pause's record each read the run's row whole and write it
// whole ([boxReadChanges]), and the host may reach the store across a link the
// engine allows to be as slow as thirty seconds a mebibyte, where a change of a
// row at its bound takes thirteen minutes ([RowBudget]). The host gives each
// its crossing on its own connection; the asker cannot know which connection
// that is, so it counts the slowest node's, coord.MaxValueCrossing, for each, as
// every wait one node keeps on another node's change of a run's row does
// ([standbyBudget]). Held to their flat graces, each was cut while its bytes
// were still crossing: the place went unreserved, so no capture was made, or
// the stamp of what came of the take, or how the harness ended, went
// unrecorded (crewlet/crewlet#1151).
//
// AND A READ THAT MAY END THE JOB HOLDS THE JOB'S END BEFORE ITS TAKE
// ([JobEndBudget]): a stop's and a loss's end it first ([Coordinator.endJob]),
// and so does a collection's where the turn's cap has passed with the job still
// working ([Coordinator.readBox]), at most the end's wait and the kill past it,
// and that is counted at the host's share as the take is — and for a
// collection, the record that the cap ended a job it saw working, one more
// change of the row, made on its own context before the end's wait begins,
// within a teardown's grace and the row's crossing
// ([Coordinator.markEndedAtDeadline]), so it is counted at both, as every
// other change is at its reason and its crossing. The host's share of such a
// read's budget is then the end's 145 seconds and that record's grace, the
// take's 240 and its reservation's room, its three graces, the crossings of the
// changes, and 45 — the poll's bound at the share — for the read's own steps,
// so a take after an end at its bound and its record at theirs is handed its
// whole budget all the same ([Coordinator.takeContext]).
//
// EVERY COLLECTION OF A TURN WITH A CAP holds the end, whether or not the cap
// has passed on the asker's clock: the host decides whether to end the job on
// its own clock, as it reads, and nothing in a completion tells the asker
// whether its job finished or the host's waiter announced it at the cap. A
// collection of a turn with none never ends its job, and waits for the take
// alone.
//
// AND A CLAUDE CODE COLLECTION HOLDS THE READ OF ITS JOB'S MAIN THREAD, made once
// on the host before the take within [MainThreadReadWait] (mainthread.go),
// counted at the host's share as the take is: fifteen seconds more for the host,
// twenty for its asker. Another harness's collection, a stop's and a loss's ask
// the thread nothing, and wait as they did.
//
// What that costs is the wait for a host that has truly stopped (about
// seventy-eight minutes for a collection of a turn with no cap before the
// asker hands the claim back, where it was one, about eighty-one and a half
// for a stop's or a loss's read, and about ninety-nine and a half for a
// collection of a turn with a cap), which a host that is only slow, ending a
// job at its bound, packing a session at its bound or changing a run's row at
// its bound across a link at the slowest pace allowed, must not be taken for.
// Ordinary reads answer in seconds, a job ending in a few and a session of a
// day's work being a mebibyte or two. A read that has not finished in that is
// a host that is not answering, and is answered as one ([ErrBoxHostSilent]);
// it announces the completion again from its own next poll. Only a node that
// asks ANOTHER node's host waits on it: a box this node hosts it reads itself.
func BoxReadBudget(run PendingRun, read BoxRead) time.Duration {
	held := SessionTakeBudget + sessionRecordBudget + 3*discardGrace + boxReadChanges*coord.MaxValueCrossing
	if read.mayEnd(run) {
		held += JobEndBudget
	}
	if read.recordsTheCap(run) {
		held += discardGrace + coord.MaxValueCrossing
	}
	if asksMainThread(run, read) {
		held += MainThreadReadWait
	}
	return pollBound + time.Duration(float64(held)/BoxReadAnswerShare)
}

// boxReadChanges is how many changes of a run's row every read of its box gives
// room to on its host: the reservation of the capture's place
// ([Coordinator.reserveRoom]), and the two of the take's records' room — the
// stamp of what came of the take and how its job's harness ended
// ([Coordinator.takeRecordsRoom]) — and the pause's record
// ([Coordinator.pauseRoom]). A read that makes fewer is given the room all the
// same, since the host's rooms are the read's whatever it asks.
const boxReadChanges = 4

// recordsTheCap reports whether a host's read of run's box may make, beside
// [boxReadChanges], the record that the turn's cap ended a job it saw working
// ([Coordinator.markEndedAtDeadline]), a fifth change of the row counted in
// [BoxReadBudget] at its reason and the slowest node's crossing: a collection
// of a turn with a cap, which may end its job ([BoxRead.mayEnd]).
func (r BoxRead) recordsTheCap(run PendingRun) bool {
	return r == BoxReadCollect && r.mayEnd(run)
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

// BoxReadAnswerShare is the part of what its asker waits that a host's read may
// take, counted from the moment the request reaches the host on the host's own
// clock ([BoxReadRequest.AnswerBudget]), leaving the rest for the answer to
// cross the broker before the asker stops listening: [answerShare], a tail's,
// which a tail's owner applies to the wait its request carries the same way.
//
// THE QUARTER LEFT HOLDS THE CROSSINGS ALONE. Two clocks' distance needs none
// of it, since the host counts the wait on its own clock from the request's
// arrival, and nor does a wait for one of the host's reads at once, which is
// counted against the share (internal/engine's box reads). What is left is the
// request's two hops at the slowest pace the engine allows ([objstore.PaceFor]:
// under a mebibyte, thirty seconds a hop) and the reply's two at the broker's
// ceiling (eight mebibytes, four minutes a hop): nine minutes in all, against
// more than nineteen, the quarter of the shortest wait a read asks at its
// budget — an uncapped collection's, about seventy-eight minutes. An absolute
// margin for the crossings would shorten a dead host's wait by about ten
// minutes of those seventy-eight, at the cost of a second rule beside the
// tail's for the same question, so the share stays the tail's.
// A wait its asker's own context cuts far below its budget leaves a quarter no
// share could fill at that pace; that is the cut's own defect, not the share's
// (crewlet/crewlet#1405).
const BoxReadAnswerShare = answerShare

// BoxReadChannel names the signed channel a box read crosses
// (internal/signedask): an INSTRUCTION, since the host carries it out —
// it pauses the box a collection reads — and the fleet signs what a node
// carries out for another.
const BoxReadChannel = "sandbox.box_read"

// BoxReadClockAllowance is how far apart the box-read channel allows the asking
// and the answering node's clocks to stand (internal/signedask's
// ClockAllowance): the most any two of the fleet's machines stand apart while
// no node raises `clock_skew` ([objstore.AssumedClockSkew], twice). A host
// ahead of its asker by up to that still takes the read, whose length the
// request's own wait sets on the host's clock ([BoxReadRequest.BudgetMS]).
// Past it, the host's clock is one the operator must fix, and the read is
// refused as one whose asker has gone.
//
// What the two hours cost is a replay window two hours longer, at a node that
// restarted inside it and so forgot the ids it took, for a read the run's row
// licenses anyway ([Coordinator.ReadHostedBox]): a replayed collection, stop or
// loss is made only where the row is still in the state that read is made in.
const BoxReadClockAllowance = 2 * objstore.AssumedClockSkew

// BoxReadWireVersion is the request and reply shape this build speaks, and the
// only one its host answers.
//
// A MEMBER A LATER BUILD ADDS is refused by this build's host wherever a request
// sets it, because the request decodes naming what it does not declare
// ([BoxReadRequest.Unread]). So a later build leaves a field it adds out at its
// zero, and a request that sets none is read as before. A change of what an
// existing field MEANS, or a reply field an asker must understand, moves the
// version. Version 2 is the request carrying its asker's wait as a duration
// ([BoxReadRequest.BudgetMS]) and a host that may answer busy
// ([BoxReadReply.Busy]).
const BoxReadWireVersion = 2

// BoxReadRequest is one box read, as it crosses the broker.
type BoxReadRequest struct {
	Version int `json:"v"`
	// ID and Deadline bind the request against a replay: see
	// internal/signedask. Deadline is the asker's instant, by its own clock,
	// after which it no longer waits — the replay binding ONLY: the host
	// never reads it as how long it may read, which on its own clock is the
	// distance between two machines' clocks plus the wait ([BudgetMS]).
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	// BudgetMS is how long the asker waits for the reply, in milliseconds, as
	// its own clock measured it when it sealed the request: the read's budget
	// ([BoxReadBudget]), or less where the asker's own context ends sooner.
	// The host reads for its share of that wait, counted from the moment
	// the request reached it, on the host's own clock
	// ([BoxReadRequest.AnswerBudget]) — never from Deadline, the asker's
	// instant, which only binds the request against a replay
	// (internal/signedask). Zero is no wait, and refused.
	BudgetMS int64 `json:"budget_ms,omitzero"`
	// Node is the node asked — the one whose host the box is on
	// ([PendingRun.LaunchNode]). Every node serves the subject and only
	// that one answers.
	Node      string  `json:"node"`
	TurnID    string  `json:"turn_id"`
	LaunchID  string  `json:"launch_id"`
	SandboxID string  `json:"sandbox_id"`
	Read      BoxRead `json:"read"`

	// unread is every member of the request as it crossed that this build
	// does not declare ([BoxReadRequest.UnmarshalJSON]): a request that
	// carries one is refused rather than read short ([BoxReadRequest.Unread]).
	unread []string
}

// Binding is the id and the deadline the request was minted under: the replay
// binding, never the length of the read ([BoxReadRequest.BudgetMS]).
func (r BoxReadRequest) Binding() (string, time.Time) { return r.ID, r.Deadline }

// AnswerBudget is how long the host may read for a request that reached it
// waited ago, both on the host's own clock: what is left of its share, which
// is [BoxReadAnswerShare] of the wait the request carried, counted from its
// arrival — so whatever the read waited for one of the host's reads at once,
// it ends by the arrival plus that share, and the last quarter of its asker's
// wait is left whole for the request's and the answer's crossings. A share of
// what the asker still waits instead would end a read that waited later the
// longer it waited: at the waiting bound's end, three quarters of the way
// through its asker's wait, it would read on for three quarters of the last
// quarter, leaving a sixteenth of the wait for the crossings, under the nine
// minutes [BoxReadAnswerShare] counts for them at the slowest pace, and the
// host would take a session out of the box for an asker that had gone. Zero
// once the share is spent. Not ok for a request that carries no wait, or one too
// long to be a duration, which a host refuses as another build's.
func (r BoxReadRequest) AnswerBudget(waited time.Duration) (time.Duration, bool) {
	if r.BudgetMS <= 0 || r.BudgetMS > int64(math.MaxInt64/time.Millisecond) {
		return 0, false
	}
	share := time.Duration(float64(time.Duration(r.BudgetMS)*time.Millisecond) * BoxReadAnswerShare)
	return max(0, share-waited), true
}

// Unread is every top-level member of the request as it crossed that this
// build does not declare, sorted; nil for a request this build reads whole.
func (r BoxReadRequest) Unread() []string { return r.unread }

// UnmarshalJSON decodes the request as json.Unmarshal would, and records the
// members no field declares ([BoxReadRequest.Unread]).
//
// RECORDED RATHER THAN REFUSED HERE, so the request still decodes with the node
// and the turn it names, and the host it names can log what it refused and for
// which run; the host refuses it (internal/engine's box reads), as a data node
// refuses an estate request whose arguments hold a key its build does not
// declare (internal/estate's wire rule) rather than run it without them. WHICH
// members are declared is the decoder's own answer, asked one member at a time
// under its rule for a member no field declares, never a list of names kept
// here beside the fields or a second spelling of how it matches them. The
// request has no nested object, so its top-level members are all there is to
// name.
func (r *BoxReadRequest) UnmarshalJSON(data []byte) error {
	type declared BoxReadRequest // the fields alone, without this method
	if err := json.Unmarshal(data, (*declared)(r)); err != nil {
		return err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	r.unread = nil
	for name := range members {
		probe, err := json.Marshal(map[string]any{name: nil})
		if err != nil {
			return fmt.Errorf("sandbox: name the box read's member %q: %w", name, err)
		}
		if errors.Is(jsonv2.Unmarshal(probe, new(declared), boxReadMemberRule), jsonv2.ErrUnknownName) {
			r.unread = append(r.unread, name)
		}
	}
	slices.Sort(r.unread)
	return nil
}

// boxReadMemberRule is json.Unmarshal's own decoding, refusing a member no
// field declares and reporting it as the typed error that says so.
var boxReadMemberRule = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsonv2.RejectUnknownMembers(true),
	json.ReportErrorsWithLegacySemantics(false))

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
	// Busy is a host that made no read for this request: it was making as many
	// reads of its boxes as it makes at once with as many waiting, or this
	// request's share of its asker's wait ran out while it waited for one.
	// Ask again; the host is there.
	Busy bool `json:"busy,omitempty"`
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
	case r.Busy:
		return fmt.Errorf("%w: node %s", ErrBoxHostBusy, r.Node)
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
	case read == BoxReadLoss && !c.capturesLoss(run):
		return Result{}, false, fmt.Errorf("%w: run %s has no loss decided whose box a capture reads",
			ErrBoxNotHosted, turnID)
	case read == BoxReadLoss:
		// THE LAST READ OF THE BOX ANYTHING MAKES, licensed by the loss its
		// ending decided: the job ended and its session taken here, and kept
		// off the read.
		spooled, kept := c.readLost(ctx, run)
		c.keepLater(ctx, spooled)
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
