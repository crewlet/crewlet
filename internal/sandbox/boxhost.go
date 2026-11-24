package sandbox

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
)

// Who may touch a box.
//
// Every box lives on its launching node's host ([PendingRun.LaunchNode]);
// elsewhere it is gone ([ErrBoxGone]). The seat can move mid-job, so every
// touch of the box is made on its host:
//   - poll, keepalive and pause reaper: the host's own waiter ([Waiter]); a
//     host gone for good has its runs ended by [Waiter.goneAfter].
//   - collection and a stopped run's spend read: by the seat's holder, which
//     ASKS the host for the box-reading part ([BoxReader]) and makes the part a
//     model may write ([Coordinator.collect]).
//   - reclaim of a box its row no longer names: the host's waiter
//     ([Waiter.sweep]), nudged by a node ending or relaunching the run ([Nudge]).

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

// BoxReader asks the node whose host a run's box is on to read it back (the
// part of a collection that touches the box, [Coordinator.readBox]), for a node
// that holds the run's seat and not its box. What comes back is the run's own
// account, floored and bounded ([boundBox]), and whether its turn's cap ended
// it; the asker fits the rest ([Coordinator.fitText]). Four failures, each
// handled apart: the box is gone ([ErrBoxGone]); the host gave no account
// ([ErrBoxHostSilent], [BoxReadReply.Withdrawn], [ErrBoxHostBusy],
// [ErrBoxNotAsked]); the host's store did not answer its read of the row
// ([ErrStoreSilent], [BoxReadReply.Unsettled]); and anything else, the box's
// own failure, counted. See the package doc, "Box host failures".
type BoxReader interface {
	ReadBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error)
}

// ErrBoxHostSilent is a box read the box's host gave no account of: stopped,
// restarting, gone, or stopped as it read ([BoxReadReply.Withdrawn]), or it
// answered busy ([ErrBoxHostBusy]), or this node could not ask it
// ([ErrBoxNotAsked]), which wrap this. Not the box's failure: the claim goes
// back uncounted and the completion is acknowledged, the host's waiter
// announcing it again once back and a host that never returns found gone
// ([Waiter.goneAfter]). Acknowledged only where no clock runs on that host's
// store and the row says nothing of one ([Coordinator.hostClocked],
// [Coordinator.unannouncedJob]). See the package doc, "Box host failures".
var ErrBoxHostSilent = errors.New("sandbox: the node whose host the box is on did not answer")

// ErrStoreSilent is a step a collection cannot go on without that the
// coordination store did not answer: the host's read of the row, the log watch,
// the session retirements, the record of a job lost with its host, the park's
// record, and the claim, or this node's refusal of a claim while a release is
// owed ([Coordinator.claimCompletion]); each wraps the store's error in it
// ([storeSilent]). It is the node's failure, never the message's
// (internal/engine's controlOutcome), and neither the box's nor its host's: the
// claim goes back uncounted, bounded by the box host's clock ([hostUnsettled])
// or by this node's for a stop ([Coordinator.readStopped]). See the package
// doc, "Box host failures".
var ErrStoreSilent = errors.New("sandbox: the coordination store did not answer a step a collection " +
	"cannot go on without")

// storeSilent is err, the store's error at a step a collection cannot go on
// without, wrapped in [ErrStoreSilent], unless ctx has ended, which is then the
// cause and returned as it is. ctx is the node making the step's, the host's at
// its read of the row ([Coordinator.ReadHostedBox]). The collecting node hands
// back its own cancellation uncounted and counts a deadline the delivery ran
// out of ([Coordinator.collectFailed]); on the host a cancellation is the host
// stopping as it read ([BoxReadReply.Withdrawn]), a deadline the read running
// out of the asker's time ([AnswerBoxRead]).
func storeSilent(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStoreSilent, err)
}

// boxAnsweredError is a read's failure at a step made once its box answered:
// the verdict on its job's served notes the store did not answer
// ([Coordinator.settleReceipts]). It carries how the job's harness ended,
// so the hand-back that follows ends the run of failed collections and records
// that end in its own write ([Coordinator.collectFailedAt]).
type boxAnsweredError struct {
	err error
	end HarnessEnd
}

func (e *boxAnsweredError) Error() string { return e.err.Error() }

func (e *boxAnsweredError) Unwrap() error { return e.err }

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
// run's box ([BoxReader]) before taking it for a host that did not answer, by
// what the read may do there: [pollBound], plus the take of the job's harness
// session ([SessionTakeBudget]) with its read of the row ([captureReadBudget]),
// its reservation's reason ([sessionRecordBudget]) and graces, each change of
// the row at coord.MaxValueCrossing, the job's end ([JobEndBudget]) for every
// collection of a turn with a cap and for a stop or loss, and the main-thread
// read ([MainThreadReadWait]) with, where the copy holds a hook's serve, its
// verdict ([hookVerdictWait]), and the keep of its newest summary where its
// runner reads one ([summaryKeepRoom]), all at the host's share
// ([BoxReadRequest.AnswerBudget]): a slow host is not a stopped one. Only a
// node asking another node's host waits on it ("Box read budget").
func BoxReadBudget(run PendingRun, read BoxRead, traits ThreadTraits) time.Duration {
	held := SessionTakeBudget + captureReadBudget + sessionRecordBudget + 3*discardGrace +
		boxReadChanges*coord.MaxValueCrossing
	if read.mayEnd(run) {
		held += JobEndBudget
	}
	if read.recordsTheCap(run) {
		held += discardGrace + coord.MaxValueCrossing
	}
	if asksMainThread(run, read, traits) {
		held += MainThreadReadWait
	}
	if asksTakeReceipts(run, traits) {
		held += hookVerdictWait + coord.MaxValueCrossing
	}
	if traits.Summaries {
		held += summaryKeepRoom
	}
	return pollBound + time.Duration(float64(held)/BoxReadAnswerShare)
}

// boxReadChanges is how many changes of a run's row every read of its box gives
// room to on its host: the reservation of the capture's place
// ([Coordinator.reserveRoom]), and the two of the take's records' room — the
// stamp of what came of the take and how its job's harness ended
// ([Coordinator.takeRecordsRoom]) — and the pause's record
// ([Coordinator.pauseRoom]). A read that makes fewer is given the room all the
// same, since the host's rooms are the read's whatever it asks. A fifth, the
// cap's record ([BoxRead.recordsTheCap]), and the verdict on a hook's serves
// ([PendingRun.asksReceipts]) are counted where a read may make them. The
// take's read of its row is a read, not a change ([captureReadBudget]).
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
// it ends a job still working once that cap has passed ([Coordinator.readBox]):
// every such collection, as the host judges the cap on its own clock.
func (r BoxRead) mayEnd(run PendingRun) bool {
	switch r {
	case BoxReadStop, BoxReadLoss:
		return true
	case BoxReadCollect:
		return !run.Deadline.IsZero()
	}
	return false
}

// BoxReadAnswerShare is the part of its asker's wait a host's read may take,
// counted from the request's arrival on the host's own clock
// ([BoxReadRequest.AnswerBudget]), leaving the rest for the answer to cross the
// broker: [answerShare], a tail's.
//
// THE QUARTER LEFT HOLDS THE CROSSINGS ALONE: clock distance and waits for the
// host's own reads need none of it. The request's two hops at the slowest pace
// ([objstore.PaceFor]: thirty seconds a hop) and the reply's two at the broker's
// ceiling (four minutes a hop) are nine minutes, against about twenty-two in the
// quarter of the shortest budget (an uncapped collection's, about 90). An absolute
// margin would cut a dead host's wait by ten minutes for a second rule beside the
// tail's. A wait its asker's context cuts far below budget is that cut's defect (#1405).
const BoxReadAnswerShare = answerShare

// BoxReadChannel names the signed channel a box read crosses
// (internal/signedask): an INSTRUCTION, since the host carries it out —
// it pauses the box a collection reads — and the fleet signs what a node
// carries out for another.
const BoxReadChannel = "sandbox.box_read"

// BoxReadClockAllowance is how far apart the box-read channel allows the two
// nodes' clocks to stand (internal/signedask's ClockAllowance): the most any
// two machines stand apart while no node raises `clock_skew`
// ([objstore.AssumedClockSkew], twice). A host ahead of its asker by up to that
// still takes the read, whose length the request's wait sets on the host's
// clock ([BoxReadRequest.BudgetMS]); past it the operator must fix the clock.
//
// The cost is a replay window two hours longer at a node that restarted and
// forgot its ids, for a read the run's row licenses anyway
// ([Coordinator.ReadHostedBox]): a replay is made only in the state that read
// is made in.
const BoxReadClockAllowance = 2 * objstore.AssumedClockSkew

// BoxReadWireVersion is the request and reply shape this build speaks, and the
// only one its host answers. A member a later build adds is refused wherever a
// request sets it ([BoxReadRequest.Unread]), so a later build leaves a new
// field at its zero; a change of an existing field's meaning, or a reply field
// an asker must understand, moves the version. Version 2 is the asker's wait as
// a duration ([BoxReadRequest.BudgetMS]), a busy host ([BoxReadReply.Busy]),
// and a reply telling a withdrawn read and a silent store from the box's
// failure ([BoxReadReply.Withdrawn], [BoxReadReply.Unsettled]).
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
// waited ago, both on the host's clock: what is left of [BoxReadAnswerShare] of
// the carried wait, counted from arrival, so the read ends by arrival plus that
// share whatever it waited for, and the last quarter stays whole for the
// crossings. A share of what the asker still waits would end later the longer
// the read waited: at the waiting bound's end it would leave a sixteenth of the
// wait for the crossings, under the nine minutes [BoxReadAnswerShare] counts,
// and the host would take a session out of the box for an asker that had gone.
// Zero once the share is spent. Not ok for a request carrying no wait, or one
// too long to be a duration, which a host refuses as another build's.
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
// and turn it names and the host can log what it refused and for which run; the
// host refuses it (internal/engine's box reads), as a data node refuses an
// estate request with an undeclared key (internal/estate's wire rule). WHICH
// members are declared is the decoder's own answer, asked one member at a time,
// never a list of names kept beside the fields. The request has no nested
// object, so its top-level members are all there is.
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
	// Unsettled is a read that failed because the host's coordination store
	// did not answer a step it cannot go on without ([ErrStoreSilent]), Error
	// saying which: a flag of its own, since Error is only words, and an asker
	// that could not tell the store's silence from the box's failure would
	// count it against a box that is up. Part of [BoxReadWireVersion] 2.
	Unsettled bool `json:"unsettled,omitempty"`
	// Answered, beside Unsettled, says the box answered before the silent
	// step ([boxAnsweredError]), and HarnessEnd how its job's harness ended.
	// Additive: an asker that ignores them leaves the run of failed
	// collections open and records no end, as before them.
	Answered   bool       `json:"answered,omitempty"`
	HarnessEnd HarnessEnd `json:"harness_end,omitempty"`
	// Withdrawn is a read the host's own lifetime cut short, Error saying
	// where: the host withdrew as an answerer of box reads (internal/queue's
	// AnswerFunc runs under the answerer's context, which its withdrawal
	// cancels), draining or restarting, while it read. No failure of the
	// box, which a host going away never finished reading: the asker reads it
	// as a host that did not answer ([ErrBoxHostSilent]), whose reply still
	// crosses the broker, so it learns that now rather than at its deadline.
	// A read its share of the asker's time cut is no withdrawal: that is the
	// box's read running out of time, said in Error alone. Part of
	// [BoxReadWireVersion] 2.
	Withdrawn bool `json:"withdrawn,omitempty"`
	// Busy is a host that made no read for this request: it was making as many
	// reads of its boxes as it makes at once with as many waiting, or this
	// request's share of its asker's wait ran out while it waited for one.
	// Ask again; the host is there.
	Busy bool `json:"busy,omitempty"`
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
		// THE HOST'S STORE, NEVER THE BOX: it did not answer a step the read
		// cannot go on without, the row's read or, where Answered, the
		// verdict on its job's served notes after the box answered.
		err := hostSaid("sandbox: node "+r.Node+"'s coordination store did not answer", r.Error, ErrStoreSilent)
		if r.Answered {
			return &boxAnsweredError{err: err, end: r.HarnessEnd}
		}
		return err
	case r.Error != "":
		return fmt.Errorf("sandbox: node %s could not read the box: %s", r.Node, r.Error)
	case r.Busy:
		return fmt.Errorf("%w: node %s", ErrBoxHostBusy, r.Node)
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
		if answered := (*boxAnsweredError)(nil); errors.As(err, &answered) {
			reply.Answered, reply.HarnessEnd = true, answered.end
		}
	case err != nil:
		reply.Error = err.Error()
	default:
		reply.Result, reply.Ended = &result, ended
	}
	return reply
}

// hosts reports whether this node can reach run's box itself: one on this
// node's own host — launched here.
//
// A coordinator with no manager reaches nothing and reads nothing either way,
// so it answers true and lets the manager's own refusal say why.
func (c *Coordinator) hosts(run PendingRun) bool {
	return c.mgr() == nil || run.LaunchNode == c.node
}

// boxResult is the part of a read that touches the box, made where the box
// is: by this node where it hosts the box, holding one of its collections for
// the read alone ([Coordinator.collectionSlot]), and asked of the node that
// does otherwise ([BoxReader]), whose own slots bound it. A collection's
// answer from another node's host keeps the clock on a host whose store alone
// is silent ([hostUnsettled]).
func (c *Coordinator) boxResult(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error) {
	if c.hosts(run) {
		collecting, err := c.collectionSlot(ctx, run.TurnID)
		if err != nil {
			return Result{}, false, err
		}
		defer collecting()
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
	result, ended, err := c.boxes.ReadBox(ctx, run, read)
	if read == BoxReadCollect {
		c.noteHostStore(ctx, run, err)
	}
	return result, ended, err
}

// hostUnsettled is how long the host of a job's box has answered this node's
// collections of the job that its own store did not answer
// ([BoxReadReply.Unsettled]), since the first of them. A host whose store alone
// is silent is bounded as a silent host is: given up after the host grace
// ([Coordinator.collectGivenUp], [LossLaunchNodeStoreSilent]), the completion
// renewed meanwhile (renew.go). In this node's memory, its verdict also written
// on the job's record ([LaunchRecord.HostStoreSilentSince]). Kept until the run
// settles, the seat is let go, the host answers a read with its store
// answering, or a waiter announces from a read asked since; a failed attempt
// keeps it. See the package doc, "The clock on a box host's store".
type hostUnsettled struct {
	handle, launch string
	since          time.Time
}

// noteHostStore keeps the clock on run's host's store ([hostUnsettled]) from
// err, the host's answer to a collection's read of run's box: started at the
// first answer that its store did not answer, kept through a host that did not
// answer or stopped as it read and a read this node's own context ended, which
// say nothing of that store, and forgotten at any other answer, which the host
// could give only where its store answered its read of the row.
func (c *Coordinator) noteHostStore(ctx context.Context, run PendingRun, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case errors.Is(err, ErrStoreSilent):
		if kept, ok := c.unsettled[run.TurnID]; ok && kept.launch == run.LaunchID {
			return
		}
		c.unsettled[run.TurnID] = hostUnsettled{handle: run.AgentHandle, launch: run.LaunchID, since: c.now()}
		if started, ok := ctx.Value(clockStartKey{}).(*atomic.Bool); ok {
			started.Store(true)
		}
	case errors.Is(err, ErrBoxHostSilent), ctx.Err() != nil:
		// NO WORD OF THE HOST'S STORE: the clock stands as it is.
	default:
		delete(c.unsettled, run.TurnID)
	}
}

// clockStartKey is the context key under which an attempt asks to be told
// whether its own collection started the clock on the store of its box's host
// ([startingClock]).
type clockStartKey struct{}

// startingClock is ctx carrying started, which [Coordinator.noteHostStore]
// sets where a collection made under it starts the clock on the store of its
// box's host, rather than finding one another attempt started: what a re-fire
// renews on at once, where that clock still runs once its attempt is done, the
// run not settled by it ([Coordinator.refire]).
func startingClock(ctx context.Context, started *atomic.Bool) context.Context {
	return context.WithValue(ctx, clockStartKey{}, started)
}

// hostUnsettledFor is how long the host of run's box has answered this node's
// collections of run's job that its store did not answer, where cause is the
// host's own answer saying so ([BoxReadReply.Err]); false for every other
// cause, a step this node's own store did not answer above all, which no
// bound of the host's ends.
func (c *Coordinator) hostUnsettledFor(run PendingRun, cause error, now time.Time) (time.Duration, bool) {
	var flagged *flaggedFailure
	if !errors.As(cause, &flagged) || !errors.Is(flagged.kind, ErrStoreSilent) {
		return 0, false
	}
	c.mu.Lock()
	kept, ok := c.unsettled[run.TurnID]
	c.mu.Unlock()
	if !ok || kept.launch != run.LaunchID {
		return 0, false
	}
	return now.Sub(kept.since), true
}

// hostClocked reports whether this node keeps a clock on the store of the host
// of the box of turnID's job launch ([hostUnsettled]); while it does, every
// hand-back of the job's completion is renewed (renew.go). No run another party
// ends outlives it while this node holds the seat, since every ending is
// decided by [Coordinator.endRun] fenced to the seat lease, so it never has to
// hear of an ending decided elsewhere; a job its host's waiter records lost is
// no ending. TestNoOtherPartyEndsARunWhoseHostThisNodeClocks holds that fence.
// See the package doc, "The clock on a box host's store".
func (c *Coordinator) hostClocked(turnID, launch string) bool {
	_, clocked := c.hostClockedSince(turnID, launch)
	return clocked
}

// hostClockedSince is [Coordinator.hostClocked], and when the clock started.
func (c *Coordinator) hostClockedSince(turnID, launch string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept, ok := c.unsettled[turnID]
	if !ok || kept.launch != launch {
		return time.Time{}, false
	}
	return kept.since, true
}

// hostStoreSeen is what a claim of run's job, made under ctx, learned of the
// store of the host of its box, for the release that gives the claim back to
// record on the job ([Release.HostStoreSilentSince],
// [LaunchRecord.HostStoreSilentSince]): silent since this node's clock on that
// store started, where it still runs once the claim's collection is done
// ([Coordinator.hostClocked]); answered where the collection read the box, or
// counted a failure to read it, which the host could say only with its store
// answering (failed is what the release counts), or where the delivery the
// claim was made for is a waiter's announcement of the job made from a read of
// the run's row asked after the silence run's record holds ([announcedAfter]);
// and neither for a claim that learned nothing of it, a host that did not
// answer at all among them, which leaves the record as it stands.
func (c *Coordinator) hostStoreSeen(ctx context.Context, run PendingRun, failed failure) (time.Time, bool) {
	if since, clocked := c.hostClockedSince(run.TurnID, run.LaunchID); clocked {
		return since, false
	}
	return time.Time{}, run.BoxAnswered || run.Collected || !failed.collectFailed.IsZero() ||
		announcedAfter(deliveredIn(ctx), run.Launch.HostStoreSilentSince)
}

// ReadHostedBox makes, for the asking node, the read of a box on this host
// ([BoxReader]), the run's row read again here.
//
// THE ROW IS THE LICENSE, not the request: only for the job and box named, on a
// run whose box this node launched, in the state the read is made in: a
// collection's on a claimed completion ([StatusResumed]), a stop's on a decided,
// unread stop, a loss's on a decided loss with an unread box
// ([Coordinator.capturesLoss]). So even a replayed or forged request makes no
// read the run's own protocol is not making: no running job's box is paused and
// no job ended that is not decided stopped or lost, or claimed past its cap (by
// this node's clock or [LaunchRecord.EndedAtDeadline]). A stop's or loss's read
// ends the job first, a collection's one past its cap ([Coordinator.endJob]).
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
	case run.LaunchNode != c.node:
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
		// ending decided: the job ended, its served notes settled and its
		// session taken here, and kept off the read.
		spooled, kept, err := c.readLost(ctx, run)
		c.keepLater(ctx, spooled)
		if err != nil {
			return Result{}, false, err
		}
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
