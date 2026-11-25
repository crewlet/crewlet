package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/queue"
)

// Resumer re-enters a turn parked on its executor's run: the one seam between
// the sandbox and agent layers, handed the run's opaque state and the answer
// text, so this package imports nothing of the agent. It returns an error
// wrapping [ErrResumeAbandoned] when a retry must not repeat the break,
// [ErrResumeUnavailable] when this node cannot resume the run, and
// [ErrResumeElsewhere] where another node or build can. See the package doc,
// "Resume routing".
type Resumer interface {
	Resume(ctx context.Context, req ResumeRequest) error
}

// ResumeRequest is one re-entry into a suspended turn.
type ResumeRequest struct {
	Run PendingRun

	// Bridged is the run's bridged tool log as its job left it
	// ([PendingStore.BridgeLog]), plus the call a person approved that the log
	// is still owed ([BridgeLog.withHeld]): the calls the resumed phase is
	// judged on. Read once the job is collected, beside the row.
	Bridged BridgeLog

	// BridgedRead is the read Bridged was made from ([BridgeLog.Read]), taken
	// BEFORE the held call is added: that call is numbered after any call that
	// landed in between, so a count including it would leave the first of those
	// unmarked. Every relaunch hands it to its launch ([LaunchRequest.BriefRead])
	// to mark late the calls no brief told, or carries it into a new round
	// ([PendingRun.BriefRead]). For a log on another job it is the empty read
	// the store answers ([PendingStore.BridgeLog]), never applied.
	BridgedRead LogRead

	// Leads is the turn's leads kept beside the run's row ([PendingStore.Leads]):
	// oldest first, plus how many earlier ones are not kept. A part the store
	// could not read is said unread, never "no leads" ([Coordinator.leadsAt],
	// [TurnLeads.Unread]).
	Leads TurnLeads

	// Answer is what the resumed turn files as its executor's report: the
	// collected run's account of itself ([runReport]), already redacted because
	// it is published on the phase record. Empty on a person's answer
	// ([ResumeRequest.Asked]).
	Answer string

	// Asked is set when a PERSON'S ANSWER drives the resume of a run that
	// parked on a question: what was asked, the reply, and what the run left
	// behind. Nil for a completion.
	//
	// THE TURN CARRIES THE RUN ON, and never files the answer as the run's
	// report: the round is not over, so the turn relaunches its executor
	// (continuing the harness's session in the box the run asked from, else a
	// fresh run told the question, answer and branch; [Continuation]) with the
	// answer as its next prompt. Filed as the report, the run was rescued as
	// incomplete and never saw the reply.
	Asked *AskedRun

	// Lost is set when the job a completion announced was LOST with its host
	// rather than ended ([PendingRun.Lost]). Nil for every other resume.
	//
	// THE TURN CARRIES THE ROUND ON, as for a question: what the cut-off job
	// left is no account of the round, so the turn relaunches for the same round
	// (in the lost job's box where this node may continue in it, else a fresh
	// one), told where to look. Except where the round's calls already hold its
	// submission: then the round is collected and Answer is the job's account,
	// marked cut off. Past [LostRun.Limit] losses the round ends, saying why.
	Lost *LostRun

	// Success is whether the launch itself succeeded, for the resumed
	// phase's own record.
	Success bool

	// Trigger is the event that caused the resume, carried so the resumed
	// turn's telemetry names what woke it.
	Trigger *events.Event

	// DeliveredRefs are what the run reported producing, for the resumed phase's
	// event to display; nothing judges delivery on them ([Result.DeliveredRefs]).
	// Empty on a person's answer: no new run finished.
	//
	// The run's COST is deliberately not here: it rides each launch's own record
	// (`executor_collected`, [Coordinator.publishCollected]), which an answer's
	// resume, carrying no run, could not report.
	DeliveredRefs []string

	// DeliveredRefsElided is how many more refs the run reported than
	// DeliveredRefs lists ([Result.DeliveredRefsElided]).
	DeliveredRefsElided int

	// Pushed, PushedElided and PushedUnread are what the push record says the
	// run moved on Gitlet ([Result.Pushed]). Empty, like the refs, on a person's
	// answer.
	Pushed       []PushedRef
	PushedElided int
	PushedUnread string

	// InputTokens and OutputTokens are what the collected job cost, charged to
	// the turn's work item by the resumed segment (ADR-0022).
	//
	// UNLIKE the refs, a person's answer carries them too: the tokens of the job
	// that asked, recorded when it parked ([PendingRun.ParkedInputTokens]). The
	// answer's resume is the ONLY segment that job gets, so it pays.
	InputTokens  int
	OutputTokens int

	// CacheReadTokens and CacheWriteTokens are the share of InputTokens the
	// prompt cache served and stored ([Result.CacheReadTokens]), a breakdown and
	// never an addition, carried the same two ways.
	CacheReadTokens  int
	CacheWriteTokens int

	// Begin is what the resumer calls ONCE, when the segment is certain to run
	// and before anything of it does, after every check that can still send the
	// resume back as a retry. An error means it must not run. Nil commits nothing.
	//
	// IT IS WHERE A RESUME BECOMES A TURN. For a recorded answer it writes that
	// the turn took it ([PendingStore.TakeAnswer]): a claim stopping before this
	// still owes the reply to the seat, one after has used it. It records the
	// delivery as worked ([CoordinatorOptions.Spent]) and tells a resume that
	// broke before its turn from one that broke running it
	// ([ErrResumeAbandoned]). The round's carry is recorded on a collected job's
	// claim ([LaunchRecord.ResumeCarry]), best effort.
	Begin func(ctx context.Context, carry ResumeCarry) error

	// Engine is what the ENGINE spent on the job outside every segment of the
	// turn (its bridged calls, [LaunchRecord.Bridged], and the condensation,
	// [LaunchRecord.Condensed]), charged here because no segment tallied it.
	//
	// A COLLECTION RETRIED after a failed resume condenses again and only the
	// attempt the landing resume runs from reaches the item, so the item errs
	// short of the turn, never past it.
	Engine EngineSpend

	// Ended is how the harness said the run ended (answer, error, success)
	// VERBATIM, beside Answer's account. A spent plan or rate limit is in there
	// and only the resuming node can act on it: it holds the entry the run
	// launched on, and its next launch (a reviewer's send-back) must not walk
	// into the same limit. Zero on a person's answer.
	Ended RunEnding
}

// RunEnding is how a collected run said it ended — see [ResumeRequest.Ended].
type RunEnding struct {
	Text  string
	Error string
	// Event is the harness's own terminal event — see [Result.Event].
	Event string
	// Limits is the run's last limit report — see [Result.Limits].
	Limits string
	// NoLimitExit says the run's exit names no limit — see [Result.NoLimitExit].
	NoLimitExit bool
	Success     bool
	// Refused says the provider refused the request its harness made as built
	// ([requestRefused]): the resume carries its round on once in a fresh
	// session, or ends the turn on the provider's message.
	Refused bool
	// At is when the ending was FIRST read back from its box
	// ([LaunchRecord.CollectedAt]): the instant a vendor reset named as a time of
	// day ("resets 4:30pm") is the next occurrence after. The first, because a
	// resume held for hours reads the same ending, and against the later instant
	// a 4:30pm already past would be read as the next day's.
	At time.Time
}

// Accountant post-charges a collected run's tokens.
//
// The charge comes AFTER the spend, so it stops nothing, and is RECORDED
// whatever the caps say: a dropped spend would under-state the company when its
// cap bound. The boolean says the run took a counter PAST its cap; both counters
// hold it either way. companyCharged says an earlier attempt charged the
// COMPANY's counter and failed on the seat's, so this records the seat's share
// alone; a [coord.SeatUncountedError] is that partial, reached now. Any other
// error records nothing the caller can count on.
type Accountant interface {
	Charge(ctx context.Context, agentID, handle string, tokens int, companyCharged bool) (refused bool, err error)
}

// AudienceResolver resolves the audience a harness named for its question
// — "requester", "manager", "team", or a name it typed — to the seats it means,
// for the run that asked.
//
// Implemented by the engine, which holds the chart; this package knows the label
// and the run, not who leads whom. A PURE ANSWER with no error: a walk over a
// chart in memory, and a label resolving to nobody is part of the answer
// ([Audience.Fallback]), never a failure of the park.
type AudienceResolver interface {
	ResolveAudience(run PendingRun, label string) Audience

	// MayAnswer reports whether whoever holds seat may answer a parked question
	// NOW, read when the reply arrives and never at the park: an agent seat the
	// chart holds, or a human seat whose person may act. A suspended or removed
	// person, or an unheld human seat, answers nothing and the reply runs as an
	// ordinary message. An error is UNKNOWN, which keeps the reply's place.
	MayAnswer(ctx context.Context, seat string) (bool, error)
}

// CoordinatorOptions configures a [Coordinator].
type CoordinatorOptions struct {
	Queue   Publisher
	Pending PendingStore
	Manager *Manager

	// FailureKind is the classifier's word for a failed resume, carried by the
	// run's lost announcement. It sits above this package; nil leaves it unsaid.
	FailureKind func(error) string

	// NodesFailure reports whether a resume's failure is the node's or its
	// infrastructure's rather than the run's (a coordination store, state log,
	// store or disk that did not answer, a dead queue, this node's stop). It is
	// handed back like any other but not counted toward the give-up that ends
	// the run ([Coordinator.resumeGivenUp]): an outage says nothing about whether
	// the turn can be re-entered. Classified above this package (internal/engine's
	// nodesFailure); nil counts every failure as the run's.
	NodesFailure func(context.Context, error) bool

	// ResumableElsewhere reports whether a live node other than this one may
	// resume run: one whose build reads the run's parked-state version, or, for
	// a reply kind this build does not read, one on another build. A failure on
	// [ErrResumeElsewhere] is not counted toward the give-up while one may, nor
	// while the answer cannot be read; once none may, it is, so a rollback cannot
	// hold the seat for ever ([Coordinator.runsOwnFailure]). Read above this
	// package (internal/engine's resumableElsewhere); nil counts every such
	// failure as the run's.
	ResumableElsewhere func(ctx context.Context, run PendingRun) (bool, error)

	// OperatorsCondition reports the kind of a resume failure only an operator
	// clears (a disk full, read-only or failing, open files at their limit, nodes
	// holding different keys or builds), false for any other. The give-up counts
	// no such failure, so the run waits for as long as the condition lasts and is
	// counted in [Coordinator.ResumesBlocked], from which data nodes raise the
	// executor_resume_blocked alarm. Classified above this package
	// (internal/agent/builtin's OperatorKind); nil says none.
	OperatorsCondition func(error) (string, bool)

	// Resume re-enters a suspended turn. Required: [NewCoordinator] refuses a
	// coordinator without it.
	//
	// "This node cannot resume this run" is the resumer's answer to give, by
	// wrapping [ErrResumeUnavailable]: only the engine knows which seats it holds
	// and which conversation formats it reads. It gives the claim back so the
	// node that holds the seat can win it.
	Resume Resumer

	// Account post-charges collected tokens. Nil skips accounting.
	Account Accountant

	// Audience resolves who a parked question may be answered by, at the park.
	// Required, like Resume: only the engine holds the chart, and without it
	// every question would be recorded as waiting on nobody
	// ([PendingRun.AudienceHandles]).
	Audience AudienceResolver

	// Answers seals a person's answer onto the run and opens it before a resume
	// reads it ([NewAnswerSeal], over the fleet keyring). Required: the words
	// live on the row only inside the seal. See answerseal.go.
	Answers *AnswerSeal

	// Leads seals the leads a launch's brief opened with into the run's leads,
	// for every later brief to a session that may lack that brief
	// ([NewLeadSeal], over the fleet keyring; leads.go). Nil keeps no lead (a
	// test's coordinator).
	Leads *LeadSeal

	// Ended is called for every run this node finishes with, however, and before
	// every collection's wait for calls in flight (drain.go), so more than once
	// per run: it is idempotent. It exists for credentials a run HOLDS: a bridge
	// token carries no expiry, so an open session would let a box that outlived
	// its run keep a working key to a seat's surface (internal/api/mcpbridge). A
	// parked run's session ends at its park's collection ([Coordinator.drainCalls]);
	// waiting notes stay on the record ([CoordinatorOptions.SteersEnded]). IT
	// WAITS FOR NO CALL STILL RUNNING (its update may land later,
	// [BridgeLog.InFlight]): collections wait for those on the run's log, never
	// the bridge (drain.go).
	Ended func(runID string)

	// Recovered is told of each run this node takes over still RUNNING in its
	// box, as it recovers the seat ([Coordinator.RecoverSeat]): before the seat's
	// mailbox opens, once the run is fenced to this lease or could not be written
	// either way. A run a newer lease owns is not reported, nor a parked run (its
	// box calls nothing) or a reaped tail. Like Ended, it exists for what a box
	// REACHES: its bridge session lived in the launching process, so a node that
	// restarted or took the seat over serves it again (internal/api/mcpbridge).
	// Nil serves nothing again.
	Recovered func(ctx context.Context, run PendingRun)

	// ConnectSettled waits for this node's write, under way, of the record that a
	// run's harness connected its bridge ([PendingStore.MarkBridgeConnected]),
	// which is made off the harness's list, on its completion or ctx's end (its
	// error), and answers whether this node saw the harness connect with no write
	// of it landed. A collection reading a row without the mark settles it and
	// reads the row again, or makes the write itself, so a run that listed is
	// never failed as one that never connected. Nil settles none.
	ConnectSettled func(ctx context.Context, turnID string) (unrecorded bool, err error)

	// Unresumable judges each run parked on a question as its seat is recovered:
	// non-nil is a run no build will ever resume, ended there rather than left
	// holding its seat for an answer it cannot take. Nil judges none.
	Unresumable func(ctx context.Context, run PendingRun) (*Unresumable, error)

	// Stopped is called once for every way a run's engine-side life ENDS, and for
	// the one way it stops without ending, a park: nothing is working behind this
	// seat and turn any more. A turn suspended into the run returned its frame,
	// so only this call lets the working indicator drop. It fires on ordinary
	// endings too: a gate on "had a suspended turn" is a second opinion. Reported
	// WHERE THE TRANSITION IS MADE, NOT WHERE A LIST SAYS (enumerated stops missed
	// one each time): every ending goes through [Coordinator.endRecord],
	// [Coordinator.park] reports itself, each only where the transition was THIS
	// call's. Over-reporting is harmless, under-reporting the defect. After the
	// durable write (a retry could still resume the turn and find its hold gone),
	// before the announcement (a publish can block). NOT Ended, keyed on a credential.
	Stopped func(ctx context.Context, handle, turnID string)

	// Parked is told every run whose question just landed on its row as
	// awaiting: what a held call put to delegates wakes them from (ADR-0125).
	// After the durable write, so a woken delegate finds the call waiting; nil
	// tells nobody.
	Parked func(ctx context.Context, run PendingRun)

	// WaitedFor is how the run a run parked by `wait_for_run` waits for left
	// its seat's slots, read now, and false while it holds one: what the run is
	// carried on with ([Coordinator.OnWaitOver]). Nil carries no wait on.
	WaitedFor func(ctx context.Context, run PendingRun) (types.RunWaited, bool)

	// CloseStopped ends the turn of a run a person stopped ([Coordinator.StopSeat])
	// and answers how it ended for the executor log ([Coordinator.publishEnd]):
	// by whom and why, whether its reply reached anybody ([RunCalls]), and the
	// answer to the colleague whose ask the turn took up ([PendingRun.ColleagueAsk]).
	// A turn PARKED on the run is also closed on the record
	// ([types.AgentTurnStopped], [RunStop.Collected]). The engine implements it:
	// only the agent layer reads [PendingRun.ExecuteState]. EVERY STOP, PARKED OR
	// NOT: the END is this step's, since recorded by the launching frame its
	// tombstone refused the job's session. A STEP OF THE ENDING, before the record
	// is deleted, decided ONCE ([PendingRun.OwedEnd]), under identities from the
	// run and ending ([RecordedEnding.ID]) so a repeat is the same records. An
	// error keeps the ending for retry. Nil closes nothing.
	CloseStopped func(ctx context.Context, run PendingRun, calls RunCalls) (executorkeep.Ending, error)

	// TurnLost is handed every run whose decided ending is a LOSS
	// ([PendingRun.lost]), to settle what the parked turn still owed outside the
	// run: the colleague whose a2a_ask woke it ([PendingRun.ColleagueAsk]) is told
	// it was lost, and how the turn ended is recorded ([Coordinator.publishEnd],
	// [RunCalls]). THE ONE TERMINAL OF A PARKED TURN NO TURN FRAME ENDS (a resume
	// ends a collected run's, [CoordinatorOptions.CloseStopped] a stop's); without
	// it the ask waited for the idle sweep. A STEP OF THE ENDING, after the
	// announcement and before the record is deleted, decided ONCE
	// ([PendingRun.OwedEnd]); BEST EFFORT toward the asker, whose refusal must not
	// hold the seat's inbox. Nil does nothing.
	TurnLost func(ctx context.Context, run PendingRun, calls RunCalls) executorkeep.Ending

	// SteersEnded publishes what became of every note a person sent the run's
	// turn that no step reported reading ([PendingRun.EndedNotes]): DELIVERED
	// (told again by a later launch's brief), UNCONFIRMED (handed to a harness,
	// never confirmed), MISSED (a take whose answer was lost recorded them read,
	// no later job told them), EXPIRED (still WAITING, [RunSteer], or first read
	// by a job that never started). The engine implements it: only it opens a
	// note's seal. A STEP OF THE ENDING like CloseStopped, repeated after a crash
	// under identities from the run, ending and note ([RecordedEnding.ID],
	// [RecordedEnding.At]); an error keeps the ending for retry. A note filed once
	// the ending is decided is refused ([PendingStore.FileNote]). Nil publishes
	// nothing.
	SteersEnded func(ctx context.Context, run PendingRun) error

	// SteersConfirmed publishes, as delivered, every note and wake item a hook's
	// serve to the run's job is recorded as having handed over ([readByServeOf]),
	// the engine claiming each wake. Called on every Claude Code box read, asked
	// or not ([Coordinator.settleReceipts]), so a verdict whose answer was
	// lost is published by the next read. Ids derive from the turn and each note,
	// so a repeat is the same records; nil publishes nothing.
	SteersConfirmed func(ctx context.Context, run PendingRun, notes []RunNote)

	// WakesEnded reports the wake items of a job a relaunch replaced that no
	// step read, which its write removed ([PriorBox.Wakes]): each unconfirmed
	// or unread, its delivery left to a turn of its own. Called once the write
	// landed, never on the launch's path to fail it: the engine publishes off it,
	// under an id from the turn and the wake. Nil reports nothing.
	WakesEnded func(ctx context.Context, run PendingRun, wakes []RunNote)

	// Served is every other conversation run's turn served, each one whose
	// wake a step of the run read, as its end names them on the executor log
	// ([TurnEnd.Served]): the engine's to say, since only it opens a wake's
	// seal. Nil names none.
	Served func(ctx context.Context, run PendingRun) []string

	// SeatCap is the most runs a seat holds at once, at most one per
	// conversation (seatcap.go): the engine's, from the seat's
	// sandbox.max_concurrent_runs, read at every check so an apply moves the
	// next one. Nil, or an answer below one, is one.
	SeatCap func(handle string) int

	// Busy is told a seat's runs holding it went from none to some on this
	// node or back, or that this node let the seat go: the engine's router of
	// the seat's held inbox follows it, reading [Coordinator.SeatRuns]. Not an
	// owed answer's hold, which leaves no run to hand a wake to. Called outside
	// the coordinator's lock, possibly out of order, hence a read rather than a
	// value. Nil tells nobody.
	Busy func(handle string)

	// FoldInAnswered is told a seat a person answered a fold-in of one of its
	// runs (foldin.go): the answer is on the run's row already, and the
	// engine's router of the seat reads it at once rather than at its
	// backstop. Nil tells nobody.
	FoldInAnswered func(handle string)

	// Hold holds a seat's inbox while one of its runs holds the seat or an
	// answer it owes awaits its resume ([SeatHold]). Nil holds nothing (a test's
	// coordinator).
	Hold SeatHold

	// Admit says whether a seat may run a retried resume now ([Admission]). Nil
	// admits every retry.
	Admit Admission

	// Gate is the node's admission, which every resume passes before it claims
	// its run, on every route and told which ([TurnGate]). Nil admits every turn.
	//
	// NOT Admit, the screening a delivery applies (lease, pause, model, posture),
	// which a retried answer asks because it runs outside the screened delivery.
	// The gate is whether this process may start the turn at all or would meet a
	// known condition: its models' limit, a park this node holds the seat in, and
	// for an answer (first call a launch) the launch's budget. A completion is
	// asked no budget: its job is collected and charged first, and a refusing
	// budget holds it then ([ErrResumeHeld]).
	Gate TurnGate

	// Collections is how many reads of its runs' boxes this node makes itself
	// at once ([Coordinator.collectionSlot]): its turns, node.max_concurrent as
	// its gate resolves it, so the reads the gate does not admit (a completion
	// ended at its turn's deadline, a stop's, a loss's) stay within them. Zero
	// bounds none, as a nil Gate admits every turn; below zero is refused. See
	// the package doc, "A node's collections".
	Collections int

	// Lease is the seat lease this node holds a seat under, stamped on the row by
	// a launch ([PendingStore.BeginLaunch]) and carried back by every later
	// write. It lets the seat's next holder FENCE this node out: a write under
	// the outranked lease is refused (a release that revives a reaped claim, a
	// resumed turn taking an answer the holder is handing back).
	//
	// ASKED AT EVERY WRITE ON A SEAT'S BEHALF rather than carried on each request:
	// a path that forgot would write unfenced unnoticed. THREE ANSWERS: the
	// seat's fence and true (held here); the ZERO fence and true (unfenced, a node
	// with no seat lease); false (a seat host that does not hold the seat), which
	// every write refuses with [ErrSeatNotHeld] (the zero fence there constrains
	// nothing). Nil answers the zero fence, held.
	Lease func(handle string) (fence Fence, held bool)

	// Spent records a recorded answer's deliveries as WORKED in the completion
	// ledger, so a later copy of the delivery is dropped, WHATEVER ITS TYPE (an
	// answer by turn runs no turn, so a filter on the ledger's turn set missed
	// it). CALLED THE MOMENT THE ANSWER LEAVES THE RUN: when a turn TAKES it
	// ([ResumeRequest.Begin]) and before an ending or decline hands it back (the
	// next holder's reap also spends a taken answer). Not when the turn returns:
	// the node can stop mid-turn after its reply reached the person. A spent
	// reply not yet gone is not lost: the row, never the delivery, resumes it or
	// hands it back ([declinedCopyID]). A REFUSAL IS SPENT ONLY ONCE A TURN THAT
	// TOOK IT KEEPS IT ([Coordinator.spendRefused]): the row carries none of its
	// reply ([AnswerRefused]). FAILS OPEN: a missed write costs a recoverable
	// duplicate turn, where refusing to move the answer would lose it.
	Spent func(ctx context.Context, handle string, evs []*events.Event)

	// WasSpent reports whether a delivery is recorded as worked in the ledger
	// [CoordinatorOptions.Spent] writes, for an answer by turn coming round after
	// it was spent ([Coordinator.OnAnswerGiven]) and a refused reply's delivery
	// whose refusal another attempt kept ([Coordinator.refusedReply]). FAILS
	// OPEN: unreadable, the answer reaches [Coordinator.AnswerByTurn], which asks
	// the run, and the refused reply is decided by its row ([PendingRun.RoundRefusals]),
	// running as a duplicate only where another attempt kept the refusal and the
	// run has since ended. NEVER THE WORD on a delivery whose refusal this node's
	// own attempt kept. Nil reads every delivery as unspent.
	WasSpent func(ctx context.Context, handle string, ev *events.Event) bool

	// After schedules f to run after d in its own goroutine and returns a cancel
	// reporting whether f had not started (time.AfterFunc's contract). Nil is
	// time.AfterFunc; a test replaces it to fire a retry when it chooses. It must
	// never call f before it returns.
	After func(d time.Duration, f func()) (stop func() bool)

	// Condense rewrites a collected run's report or failure detail past
	// [MaxRunTextBytes] ([Condenser]). Nil condenses nothing: such a piece keeps
	// its whole lines up to the bound and says how many it left out.
	Condense Condenser

	// Node is this node's id: written on every run it launches as the node whose
	// host its box is on ([PendingRun.LaunchNode]) and compared with every run it
	// reads, since a box is touched only on its host (boxhost.go). Empty
	// only in a test's coordinator, whose launched runs are all its own.
	Node string

	// Incarnation is this PROCESS's incarnation, `<node>:<uuid>`, which its seat
	// leases and presence are held under; Takers holds its TAKER LEASE under it
	// ([coord.ClassTaker], taker.go): claimed at build, renewed every
	// TakerTTL / [coord.KeptRenewals], released by [Coordinator.Stop] after the
	// last keep. Every capture names the lease ([SessionCapture.Taker],
	// [SessionCapture.TakerEpoch]); a wait for another process's capture settles
	// from the executor log once that lease lapsed (session.go). Held to
	// [maxIncarnationBytes]. BOTH OR NEITHER ([NewCoordinator] checks): a holder
	// with no lease would read as a taker gone the moment its capture began;
	// neither is a test's coordinator, waited for by stamp age.
	Incarnation string
	Takers      TakerLeases

	// TakerTTL is the taker lease's TTL: the deployment's seat lease TTL, so a
	// stopped taker reads as gone as fast as its seats move ([takerLease]). Zero
	// takes [seat.SeatLeaseTTL]; nothing past [coord.MaxDutyTTL], the longest
	// lease the store keeps outside seats and presence, is asked for.
	TakerTTL time.Duration

	// Boxes reads a run's box on the node whose host it is on, for a run this
	// node holds the seat of but not the box ([BoxReader]). Nil reads no peer's
	// box: a collection of one fails.
	Boxes BoxReader

	// Nudge tells the waiter driving a run to look at it now ([Nudge]); sent
	// where this coordinator leaves a box to its host ([Coordinator.tellHost]).
	// Nil tells nobody and the host's next pass finds it.
	Nudge func(ctx context.Context, n Nudge)

	// Refresh is handed the box a job CONTINUES IN (kept box reattached,
	// [PriorBox.ContinuesIn]) before the job starts, to renew what expires
	// ([RefreshFunc]), the hook the waiters run on every poll. AT THE START TOO:
	// a kept box runs no setup ([Coordinator.acquire]) and no poll reaches a
	// paused one, so a token can have expired and a job continued after an answer
	// ("go ahead and push") would act on it first. A fresh box needs none.
	// WHAT IT WRITES IS SEALED WITH THE JOB: the job's set does not exist yet, so
	// the launch's recorder keeps each token for it ([Coordinator.sealHanded]); a
	// launch failing before reclaims the box. A failing refresh is logged by the
	// implementation, the job starting with what the box holds. Nil refreshes none.
	Refresh RefreshFunc

	// Pushes reads what a collected run pushed to Gitlet ([RunPushes]). Required,
	// like Resume: without it a run given a repository would reach its turn
	// saying nothing of its pushes, which its reviewer reads as "nothing pushed".
	// On a node not serving Gitlet it answers an error, which the collection SAYS
	// ([Result.PushedUnread]); asked only for a run that may push ([Pushable]).
	Pushes RunPushes

	// Sessions is the executor log's writes (every turn's first read, every
	// capture of a harness session, every turn's end): THE ESTATE ROUTER'S
	// (internal/estate's ExecutorWrites), never a data node's own writer, since a
	// seat's box is wherever its seat runs, a seats-only node with no log
	// included. See session.go. Keep is the engine's part: the company's policy,
	// the scrub's secrets, the instant a turn's operations are minted at, and the
	// sealed upload. Both or neither: either nil keeps no session and records no
	// end (a test's coordinator).
	Sessions SessionWrites
	Keep     SessionKeep

	// Restore is what a relaunch continues its turn's kept session from: the
	// executor log's row read through THE ESTATE ROUTER'S reads, as Sessions
	// writes through its writes, and the sealed object (restore.go). Nil
	// continues no kept session: a relaunch continues at most its own box's.
	Restore SessionRestore

	// Holders is whether a session id is any turn's in the executor log, which a
	// minted session's bind asks of a fresh id (minted.go). Nil refuses every
	// fresh id, so a minting harness's job binds only the session it continued.
	Holders SessionHolders

	// Saves is the fleet's ledger of each seat's saves, which a save of a running
	// job is charged against before its stamp (saveledger.go): the engine's
	// coordination store. Nil takes no save.
	Saves SaveLedgers

	// SpoolDir is where this node spools a capture's sealed session between its
	// take and keep ([Coordinator.spool]), in a directory of the process's own
	// under it ([Coordinator.spoolHere]). Empty is os.TempDir; a test names its
	// own directory so what is left is its own.
	SpoolDir string

	// Spools is told each final take's wait for spool room and the room held
	// ([SpoolMeter]): the engine's meter records them in the node's metrics. Nil
	// measures nothing.
	Spools SpoolMeter

	// Carries is told each collected launch's verdict on its session, once
	// per launch ([SessionMeter]). Nil measures nothing.
	Carries SessionMeter

	// Conversations is told, once the executor log has a turn's end, the
	// verdict its conversation is told ([PendingRun.ConversationNotice],
	// [ConversationTeller]). Nil tells nobody.
	Conversations ConversationTeller

	// AnswerBudget is the node's answer budget every collection of a run's
	// box holds its weight of while it reads ([Runner.Collect]), its peers'
	// payload answers beside. Nil is a budget of this coordinator's own.
	AnswerBudget *queue.AnswerBudget

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Coordinator is the engine's hands on its executors' detached runs.
//
// Ordering makes each transition safe. SUSPEND persists the parked state and holds
// the seat. COMPLETION is claimed AT MOST ONCE, for its own job (collect,
// post-account, publish `executor_collected` once, re-enter the turn); the seat
// stays held until just before the resume, else a queued event takes the slot and
// the redelivery finds the claim flipped, the turn lost. PARK gives the seat BACK
// (answers take days); deliveries are offered to [Coordinator.TryResumeFromAnswer]
// first, the park is DURABLE FIRST ([Coordinator.unclaim] on failure), the first
// reply RECORDED before the resume. RECOVERY re-marks running jobs, inherits
// questions, reaps abandoned tails. STOP ends the seat's runs ([Coordinator.StopSeat]).
type Coordinator struct {
	// failureKind is [CoordinatorOptions.FailureKind].
	failureKind func(error) string
	// nodesFailure is [CoordinatorOptions.NodesFailure].
	nodesFailure func(context.Context, error) bool
	// resumableElsewhere is [CoordinatorOptions.ResumableElsewhere].
	resumableElsewhere func(ctx context.Context, run PendingRun) (bool, error)
	// operatorsCondition is [CoordinatorOptions.OperatorsCondition].
	operatorsCondition func(error) (string, bool)

	queue   Publisher
	pending PendingStore
	manager *Manager
	resume  Resumer
	account Accountant
	// audience is [CoordinatorOptions.Audience].
	audience AudienceResolver
	// answers is [CoordinatorOptions.Answers], leads [CoordinatorOptions.Leads].
	answers  *AnswerSeal
	leads    *LeadSeal
	condense Condenser
	// pushes is [CoordinatorOptions.Pushes].
	pushes RunPushes
	ended  func(runID string)
	// recovered is [CoordinatorOptions.Recovered].
	recovered func(ctx context.Context, run PendingRun)
	// connectSettled is [CoordinatorOptions.ConnectSettled].
	connectSettled func(ctx context.Context, turnID string) (bool, error)
	stopped        func(ctx context.Context, handle, turnID string)
	parked         func(ctx context.Context, run PendingRun)
	// waitedFor is [CoordinatorOptions.WaitedFor].
	waitedFor func(ctx context.Context, run PendingRun) (types.RunWaited, bool)
	// unresumable is [CoordinatorOptions.Unresumable].
	unresumable func(ctx context.Context, run PendingRun) (*Unresumable, error)
	// closeStopped is [CoordinatorOptions.CloseStopped].
	closeStopped func(ctx context.Context, run PendingRun, calls RunCalls) (executorkeep.Ending, error)
	// turnLost is [CoordinatorOptions.TurnLost].
	turnLost func(ctx context.Context, run PendingRun, calls RunCalls) executorkeep.Ending
	// steersEnded is [CoordinatorOptions.SteersEnded], steersConfirmed
	// [CoordinatorOptions.SteersConfirmed] and wakesEnded
	// [CoordinatorOptions.WakesEnded].
	steersEnded     func(ctx context.Context, run PendingRun) error
	steersConfirmed func(ctx context.Context, run PendingRun, notes []RunNote)
	wakesEnded      func(ctx context.Context, run PendingRun, wakes []RunNote)
	// served is [CoordinatorOptions.Served].
	served func(ctx context.Context, run PendingRun) []string
	// seatCap is [CoordinatorOptions.SeatCap].
	seatCap func(handle string) int
	// busy is [CoordinatorOptions.Busy].
	busy func(handle string)
	// foldInAnswered is [CoordinatorOptions.FoldInAnswered].
	foldInAnswered func(handle string)
	// node, boxes and nudge are [CoordinatorOptions.Node], Boxes and Nudge.
	node  string
	boxes BoxReader
	nudge func(ctx context.Context, n Nudge)
	// taker is this process's taker lease ([CoordinatorOptions.Takers]), nil
	// where none is wired.
	taker *takerLease
	// refresh is [CoordinatorOptions.Refresh].
	refresh RefreshFunc
	// sessions and keep are [CoordinatorOptions.Sessions] and Keep. Every wait of
	// the coordinator is a field so a case sets its own: sessionRetry is the
	// first wait between attempts at a record ([sessionRetryBase]), recordBudget
	// all of a record's attempts ([sessionRecordBudget]), beginBudget all of a
	// begin's ([beginRecordBudget]), captureTick how often an ending waiting on a
	// capture reads the row ([captureWaitTick]).
	sessions     SessionWrites
	keep         SessionKeep
	sessionRetry time.Duration
	recordBudget time.Duration
	beginBudget  time.Duration
	captureTick  time.Duration
	// restore is [CoordinatorOptions.Restore]; relaunchWait bounds a
	// relaunch's asks ([relaunchAskWait]), and captureRest how long it waits
	// for a capture of an archive of a size still being kept
	// ([captureRestFor]); holdingsWait bounds reading what a cut-off session
	// holds ([holdingsReadWait]).
	restore      SessionRestore
	holders      SessionHolders // [CoordinatorOptions.Holders]
	relaunchWait time.Duration
	captureRest  func(archive int64) time.Duration
	holdingsWait time.Duration
	// endWait is how long a read waits for a job its script was asked to end
	// ([JobEndWait]); endPoll how often it asks ([jobEndPoll]).
	endWait time.Duration
	endPoll time.Duration
	now     func() time.Time

	// collections is [CoordinatorOptions.Collections] as a semaphore, nil for
	// none, and collectionsWaiting how many reads wait for one
	// ([Coordinator.collectionSlot]).
	collections        chan struct{}
	collectionsWaiting atomic.Int32

	// saves is this node's part in saving its runs' sessions as they work
	// (save.go).
	saves saveState
	// sharedPack is the node's ONE pack slot, taken while a pack runs in a box
	// sharing the node's memory ([Placement.OwnsMemory]), a save's or a take's
	// ([Coordinator.packSlot]). Capacity 1: such a pack is charged to the
	// engine's memory (an OpenCode export holds up to ten times its session),
	// and two read at once could each pass for the other's room. The cost is a
	// drain one pack at a time: 0.4 to 5.2 s an OpenCode export of up to 64 MiB,
	// 0.04 to 0.46 s a Claude Code pack of 1 to 20 MiB (package doc, "A pack in a
	// node's own memory, one at a time").
	sharedPack chan struct{}
	// spoolRoom is the room every spool of this process shares, in bytes
	// ([spoolRoomBytes], spoolroom.go).
	spoolRoom *spoolRoom
	// sessionMeter is [CoordinatorOptions.Carries].
	sessionMeter SessionMeter
	// conversations is [CoordinatorOptions.Conversations].
	conversations ConversationTeller
	// answerBudget is the node's answer budget ([CoordinatorOptions.AnswerBudget]).
	answerBudget *queue.AnswerBudget

	// mainThreadWait bounds reading a job's main thread at its collection
	// ([MainThreadReadWait], [Coordinator.readMainThread]); verdictWait and
	// verdictRetry are a hook verdict's reason and spacing ([hookVerdictWait],
	// [hookVerdictRetry]).
	mainThreadWait time.Duration
	verdictWait    time.Duration
	verdictRetry   time.Duration

	// keepLife is the lifetime every keep runs under, off the read that took its
	// capture ([Coordinator.keepLater]); keeping counts them: [Coordinator.Stop]
	// lets those under way finish for [sessionKeepDrain], then ends them.
	// spoolDir is [CoordinatorOptions.SpoolDir], resolved, and spools the
	// coordinator's own directory under it (spooldir.go).
	keepLife context.Context
	endKeeps context.CancelFunc
	keeping  sync.WaitGroup
	spoolDir string
	spools   ownSpools

	// mu guards runs, the two seat-level answers the inbox screening reads on
	// every delivery, and attempts beside them. In memory rather than a store
	// read per delivery: the seat's owner is the only node running its turns,
	// and RecoverSeat seeds it on takeover.
	mu   sync.Mutex
	runs map[string]seatRuns
	// reserved counts each seat's slots taken by a write out that moves a run
	// into [Holding], and heldRuns is each run counted there, by turn, with
	// the conversations it works: together the seat's cap and its busy
	// conversations (seatcap.go). Under mu.
	reserved map[string]int
	heldRuns map[string]heldRun
	// waitingRuns is each run parked by `wait_for_run` this node counts, by
	// turn: out of [Holding], its keys stay held while it waits, so its mail
	// waits aside behind it (ADR-0176's amendment). Under mu.
	waitingRuns map[string]heldRun
	// freed is each seat's keys its runs leaving [Holding] freed, readmitted
	// on its inbox where no run still holds them ([Coordinator.readmitFreed]),
	// and asideFull each seat whose inbox holds as many deliveries aside as it
	// keeps ([Coordinator.HoldForAside]). Under mu.
	freed     map[string][]freedKey
	asideFull map[string]bool

	// moves counts what this node has done to each seat's runs that a recount
	// from the store must not be read across (every [Coordinator.adjustLocked]
	// transition, deleted ending record and recount written), so a recount can
	// tell one landed while it read. See [Coordinator.syncSeat].
	moves map[string]uint64

	// attempts counts the failed handoffs of one delivery to one parked run, so a
	// resume failing every time stops circling the seat's inbox: a BUDGET PER
	// DELIVERY keyed by the run it is owed to. The bounds, and why per process,
	// are at [MaxAnswerAttempts] and [maxAnswerDeliveries]; the dead-letter
	// margin is [AnswerDeliveryReserve], measured on the delivery.
	attempts map[answerKey]map[string]answerBudget

	// retries are the owed answers this node is retrying the resume (or the
	// hand-back) of, by turn id; owed is which each seat owes, which with its
	// holding count is what its inbox hold is taken for; held whether the hold is
	// applied, inboxes each seat's inbox attachment ([Coordinator.RecoverSeat]),
	// and holdBusy, holdDirty and holdGen the serialisation
	// [Coordinator.reconcileHold] runs under. All under mu.
	retries map[string]*answerRetry
	// releases are the claim hand-backs this node is making or owes, by turn id,
	// recorded before each is sent, each with its write in its run's entry
	// ([Coordinator.writing]); claiming counts the claims of a job's completion
	// asked of the store and not yet answered. Together no claim of a job is
	// taken while its last claim's release is out (claimrelease.go). Under mu.
	releases map[string]*claimRelease
	claiming map[string]int
	// writing are the writes this node has out that move a run between its seat's
	// sets, by turn, recorded before sending and dropped, with the move made, in
	// the step that takes its answer (or that of an earlier write on the run
	// still out): open of a row, recovery takeover, park's question, answer's
	// record/claim/decline, claim release, delete of an ending that counts its
	// run out (countwrite.go). A recount counts every run with a write out where
	// the moves so far leave it, row listed or not ([Coordinator.syncSeat]).
	// Under mu.
	writing map[string]*runWrites
	// unsettled is the clock on each job's host that answered this node's
	// collections when its own coordination store did not, by turn id, until the
	// run settles ([hostUnsettled]). Under mu.
	unsettled map[string]hostUnsettled
	// republish is each re-fire's announcement the broker did not take, by turn
	// id, retried on this node's timer until one lands or this node settles the
	// run ([refirePublish]). Under mu.
	republish map[string]*refirePublish
	// refused are the completions the node's gate refused on a condition, by turn
	// id, kept so the condition clearing re-fires each at once
	// ([Coordinator.Readmit]), not a poll later. Under mu.
	refused map[string]refusedCompletion
	// refiring counts each run's job's re-fires on their way, by turn id
	// ([Coordinator.refireLocked], [Coordinator.liftAndRefireLocked]): a signal's
	// lift of the hold and a seat's recovery start none beside them
	// ([Coordinator.carriedLocked]). Under mu.
	refiring map[string]int
	// liftedDuring is the hold a signal's lift cleared while a re-fire was on its
	// way, by turn id, for the last re-fire to take up ([Coordinator.refired]).
	// Under mu.
	liftedDuring map[string]rowHold
	// blocked are the completions whose last resume failed on a condition only an
	// operator clears, by turn id ([blockedResume]). Under mu.
	blocked   map[string]blockedResume
	owed      map[string]map[string]struct{}
	held      map[string]bool
	inboxes   map[string]queue.Attachment
	holdBusy  map[string]bool
	holdDirty map[string]bool
	holdGen   map[string]uint64

	// signals counts every [Coordinator.Readmit], by condition and seat, so a
	// retry refused while one landed does not wait through it. Under mu; see
	// [Coordinator.waitOwed].
	signals map[signal]uint64

	// launchStops are the stops decided on this node's runs while still
	// LAUNCHING, by turn id, for the launching frame to take
	// ([Coordinator.LaunchStopped]). Under mu.
	launchStops map[string]launchStop

	// finishing are the turns an attempt here is finishing the ending of
	// ([Coordinator.finishOnce]); frames counts the turns whose resumed frame
	// runs here ([Coordinator.framing]). Under mu.
	finishing map[string]bool
	frames    map[string]int

	// recovering are the seats a recovery pass is under way for, by handle, with
	// the claims of their runs the pass carries once done; live the claims of
	// runs this node's attempts hold, by turn id, counted from the gate that let
	// them through until the attempt returns. Between them a seat's recovery pass
	// and its runs' claims here exclude each other (recoverygate.go). Under mu.
	recovering map[string]*seatRecovery
	live       map[string]*liveAttempt

	hold  SeatHold
	admit Admission
	// gate is [CoordinatorOptions.Gate]; see [Coordinator.admitTurn].
	gate  TurnGate
	after func(time.Duration, func()) func() bool
	// spent is [CoordinatorOptions.Spent]; see [Coordinator.spend].
	spent func(ctx context.Context, handle string, evs []*events.Event)
	// wasSpent is [CoordinatorOptions.WasSpent]; see [Coordinator.OnAnswerGiven].
	wasSpent func(ctx context.Context, handle string, ev *events.Event) bool
	// lease is [CoordinatorOptions.Lease]; see [Coordinator.leaseOf].
	lease func(handle string) (Fence, bool)

	// life is the coordinator's own lifetime, which a scheduled retry runs under
	// (it outlives every delivery) and ends with [Coordinator.Stop], which waits
	// for inflight, every scheduled attempt not yet cancelled. Its end also holds
	// every store call on a run's row still to come to its reason alone
	// ([Coordinator.rowContext]).
	life     context.Context
	end      context.CancelFunc
	inflight sync.WaitGroup
	closed   bool

	// lastCalls ends one teardown's grace after the stop ends life, cutting
	// endLastCalls: life CARRIES it ([stopBoundKey]), so every call work on life
	// makes after the stop (a teardown's write, a stamp, a box call, each on its
	// own context that keeps none of life's cancellation) ends by then
	// ([detach]).
	lastCalls    context.Context
	endLastCalls context.CancelCauseFunc
}

// seatRuns is this node's count of one seat's detached runs, by the question
// each set answers.
//
// TWO COUNTS, NOT ONE: "is the seat held" and "is a run waiting for somebody's
// reply" are DISJOINT by construction ([Holding] and [Awaiting] share no status,
// since a run parked on a question must leave the seat free to receive the
// answer). Conflating them left the one delivery the answer match exists for
// arriving at a seat called free, consumed as an ordinary turn.
//
// COUNTED, not boolean: a resumed turn can launch a SECOND run before the first
// settles, so the seat is free only when the last is, and one seat can drive a
// job while another of its runs waits for a person.
type seatRuns struct {
	// holding counts the runs in [Holding], the ones the engine is driving, during
	// which the seat starts no new turn and its inbox is held (seathold.go).
	holding int

	// awaiting counts the runs in [Awaiting], stopped on a question. They hold
	// nothing, and this tells the screening to offer a delivery to the match
	// before anything else consumes it.
	//
	// ERRING HIGH IS SAFE AND ERRING LOW IS NOT, which decides every transition
	// that cannot prove a run left the set: one count too many costs a store
	// lookup that finds nothing, one too few is a person's answer run as an
	// unrelated turn and a box left to expire.
	awaiting int
}

// NewCoordinator validates the options and returns the coordinator, or refuses
// a missing collaborator by name.
func NewCoordinator(opts CoordinatorOptions) (*Coordinator, error) {
	var missing []string
	for _, field := range []struct {
		name   string
		absent bool
	}{
		{"Queue", opts.Queue == nil},
		{"Pending", opts.Pending == nil},
		{"Manager", opts.Manager == nil},
		{"Resume", opts.Resume == nil},
		{"Pushes", opts.Pushes == nil},
		{"Audience", opts.Audience == nil},
		{"Answers", opts.Answers == nil},
	} {
		if field.absent {
			missing = append(missing, "CoordinatorOptions."+field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("sandbox: a coordinator needs %s: a detached run is "+
			"published, recorded, reconnected to, resumed, put to its "+
			"answerers and told what it pushed through them, and its answers "+
			"sealed and opened",
			strings.Join(missing, ", "))
	}
	switch {
	case (opts.Incarnation == "") != (opts.Takers == nil):
		return nil, fmt.Errorf("sandbox: CoordinatorOptions.Incarnation and CoordinatorOptions.Takers come " +
			"together: a taker lease is held under the process's incarnation, and an incarnation named on a " +
			"capture with no lease behind it reads as a taker gone")
	case len(opts.Incarnation) > maxIncarnationBytes:
		// REFUSED HERE, not at every stamp, where every capture would be refused.
		return nil, fmt.Errorf("sandbox: CoordinatorOptions.Incarnation is %d bytes, past the %d a run's "+
			"row carries of one: an incarnation is `<node>:<uuid>`, its node id at most 64 bytes",
			len(opts.Incarnation), maxIncarnationBytes)
	case opts.Collections < 0:
		return nil, fmt.Errorf("sandbox: CoordinatorOptions.Collections is %d: it is how many of its runs' "+
			"boxes the node reads at once, its turns (node.TurnsAtOnce), or zero for no bound",
			opts.Collections)
	}
	c := &Coordinator{
		queue: opts.Queue, pending: opts.Pending, manager: opts.Manager, failureKind: opts.FailureKind,
		resume: opts.Resume, pushes: opts.Pushes, account: opts.Account, audience: opts.Audience,
		nodesFailure:       opts.NodesFailure,
		resumableElsewhere: opts.ResumableElsewhere,
		operatorsCondition: opts.OperatorsCondition,
		answers:            opts.Answers,
		leads:              opts.Leads,
		condense:           opts.Condense,
		ended:              opts.Ended,
		recovered:          opts.Recovered,
		connectSettled:     opts.ConnectSettled,
		unresumable:        opts.Unresumable,
		stopped:            opts.Stopped,
		parked:             opts.Parked,
		waitedFor:          opts.WaitedFor,
		closeStopped:       opts.CloseStopped,
		turnLost:           opts.TurnLost,
		steersEnded:        opts.SteersEnded,
		steersConfirmed:    opts.SteersConfirmed,
		wakesEnded:         opts.WakesEnded,
		served:             opts.Served,
		seatCap:            opts.SeatCap,
		busy:               opts.Busy,
		foldInAnswered:     opts.FoldInAnswered,
		node:               opts.Node,
		boxes:              opts.Boxes,
		nudge:              opts.Nudge,
		refresh:            opts.Refresh,
		sessions:           opts.Sessions,
		keep:               opts.Keep,
		restore:            opts.Restore,
		holders:            opts.Holders,
		saves: saveState{ledger: opts.Saves, slots: make(chan struct{}, maxConcurrentSaves),
			memos: map[string]*saveMemo{}},
		sharedPack:    make(chan struct{}, 1),
		spoolRoom:     newSpoolRoom(opts.Spools),
		sessionMeter:  opts.Carries,
		conversations: opts.Conversations,
		answerBudget:  opts.AnswerBudget,
		relaunchWait:  relaunchAskWait,
		captureRest:   captureRestFor,
		holdingsWait:  holdingsReadWait,
		endWait:       JobEndWait,
		endPoll:       jobEndPoll,

		// THE ONE WAIT every reading of a job's main thread shares.
		mainThreadWait: MainThreadReadWait,
		verdictWait:    hookVerdictWait,
		verdictRetry:   hookVerdictRetry,

		spoolDir:     opts.SpoolDir,
		sessionRetry: sessionRetryBase,
		recordBudget: sessionRecordBudget,
		beginBudget:  beginRecordBudget,
		captureTick:  captureWaitTick,
		now:          opts.Now,
		runs:         map[string]seatRuns{},
		reserved:     map[string]int{},
		heldRuns:     map[string]heldRun{},
		waitingRuns:  map[string]heldRun{},
		freed:        map[string][]freedKey{},
		asideFull:    map[string]bool{},
		moves:        map[string]uint64{},
		attempts:     map[answerKey]map[string]answerBudget{},
		retries:      map[string]*answerRetry{},
		releases:     map[string]*claimRelease{},
		claiming:     map[string]int{},
		writing:      map[string]*runWrites{},
		unsettled:    map[string]hostUnsettled{},
		republish:    map[string]*refirePublish{},
		refused:      map[string]refusedCompletion{},
		refiring:     map[string]int{},
		liftedDuring: map[string]rowHold{},
		blocked:      map[string]blockedResume{},
		owed:         map[string]map[string]struct{}{},
		held:         map[string]bool{},
		inboxes:      map[string]queue.Attachment{},
		holdBusy:     map[string]bool{},
		holdDirty:    map[string]bool{},
		holdGen:      map[string]uint64{},
		signals:      map[signal]uint64{},
		launchStops:  map[string]launchStop{},
		finishing:    map[string]bool{},
		frames:       map[string]int{},
		recovering:   map[string]*seatRecovery{},
		live:         map[string]*liveAttempt{},
		hold:         opts.Hold,
		admit:        opts.Admit,
		gate:         opts.Gate,
		after:        opts.After,
		spent:        opts.Spent,
		wasSpent:     opts.WasSpent,
		lease:        opts.Lease,
	}
	if c.now == nil {
		c.now = time.Now
	}
	if opts.Collections > 0 {
		c.collections = make(chan struct{}, opts.Collections)
	}
	if c.after == nil {
		c.after = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	c.lastCalls, c.endLastCalls = context.WithCancelCause(context.Background())
	c.life, c.end = context.WithCancel(context.WithValue(context.Background(), stopBoundKey{}, c.lastCalls))
	c.keepLife, c.endKeeps = context.WithCancel(context.Background())
	// THE TAKER LEASE LAST, once nothing can refuse the coordinator; held until
	// [Coordinator.Stop] has seen the last keep end.
	if opts.Takers != nil {
		c.taker = newTakerLease(opts.Takers, opts.Incarnation, opts.TakerTTL)
	}
	c.sweepSpools()
	if c.answerBudget == nil {
		c.answerBudget = queue.NewAnswerBudget(nil)
	}
	return c, nil
}

// Stop ends every scheduled retry and waits for any running. An owed answer's
// resume stays recorded on its run for the next holder ([Coordinator.RecoverSeat]).
// WHAT IS RUNNING GETS ONE TEARDOWN'S GRACE FROM THE STOP, ALL OF IT
// ([discardGrace]): later calls end by then, each on a context keeping the stop's
// one deadline ([detach]) (per-call graces held the stop ninety seconds where a
// pod's share is thirty). A KEEP UNDER WAY gets [sessionKeepDrain], then is ended
// and stamped refused. THE TAKER LEASE IS RELEASED ONLY AFTER THE LAST KEEP ENDS,
// else a peer reads a capture whose lease is gone as never-recording and settles it
// refused, tombstoning the turn early; released, not lapsed, so waits settle at once
// ([takerLease.release]).
func (c *Coordinator) Stop() {
	c.mu.Lock()
	c.closed = true
	for _, r := range c.retries {
		if r.stop != nil && r.stop() {
			c.inflight.Done()
		}
		r.stop = nil
	}
	for turnID := range c.republish {
		c.dropRepublishLocked(turnID)
	}
	// Kept completions' re-fires too: nothing waits for them
	// ([Coordinator.keepLocked]) and the next holder owes the announcement.
	for turnID := range c.refused {
		c.dropKeptLocked(turnID)
	}
	c.mu.Unlock()
	c.end()
	grace := time.AfterFunc(discardGrace, func() { c.endLastCalls(errStopGrace) })
	c.inflight.Wait()
	// Nothing is left to wait for, so the grace ends.
	grace.Stop()
	c.endLastCalls(errStopGrace)
	kept := make(chan struct{})
	go func() {
		c.keeping.Wait()
		close(kept)
	}()
	drain := time.NewTimer(sessionKeepDrain)
	defer drain.Stop()
	select {
	case <-kept:
	case <-drain.C:
	}
	c.endKeeps()
	<-kept
	c.closeSpools()
	if c.taker != nil {
		c.taker.stop()
	}
}

// leaseOf is the lease this node writes on a seat's behalf under —
// [CoordinatorOptions.Lease] — and whether it holds the seat at all; the zero,
// unfenced lease, held, where none is wired.
func (c *Coordinator) leaseOf(handle string) (Fence, bool) {
	if c.lease == nil {
		return Fence{}, true
	}
	return c.lease(handle)
}

// notHeld logs a write refused because this node does not hold the seat, and
// returns the refusal for the caller to report.
func notHeld(ctx context.Context, handle, turnID, write string) error {
	log.InfoContext(ctx, "sandbox_seat_not_held",
		"agent", handle, "turn_id", turnID, "write", write,
		"detail", "this node does not hold the seat, so the write is left to the seat's "+
			"holder, whose recovery pass drives the run")
	return fmt.Errorf("sandbox: %s for run %s on seat %q: %w", write, turnID, handle, ErrSeatNotHeld)
}

// SetManager swaps the sandbox manager, for a live reload of providers.sandbox.
//
// RUNS IN FLIGHT KEEP THEIR BOXES REACHABLE: a cell the new catalogue drops is
// carried as a retired backend ([Manager.carrying]), so its jobs are polled,
// collected, reclaimed and reaped through the backend that made them, while new
// runs provision from the new catalogue alone. A NIL MANAGER CHANGES NOTHING:
// it is the removal of providers.sandbox, which validates no launching seat, so
// the last manager stays to finish the runs in flight.
func (c *Coordinator) SetManager(m *Manager) {
	if m == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manager = m.carrying(c.manager)
}

// Manager is the coordinator's current manager, for a caller that needs to
// mint a box: the manager is swapped on an apply, so a caller holding its own
// reference would provision against a provider the company has replaced.
func (c *Coordinator) Manager() *Manager { return c.mgr() }

func (c *Coordinator) mgr() *Manager {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.manager
}

// packSlot waits on ctx for the node's one pack slot ([Coordinator.sharedPack])
// where run's box shares the node's memory, and returns what gives it back, to
// be called as the pack returns; a box that owns its memory takes none.
func (c *Coordinator) packSlot(ctx context.Context, run PendingRun) (give func(), err error) {
	cell := Placement(run.Placement)
	if manager := c.mgr(); manager != nil {
		cell = manager.CellOf(run)
	}
	if cell.OwnsMemory() {
		return func() {}, nil
	}
	select {
	case c.sharedPack <- struct{}{}:
		return func() { <-c.sharedPack }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the node's one pack in a box that shares its memory: %w", ctx.Err())
	}
}

// collectionSlot waits on ctx for one of this node's collections
// ([CoordinatorOptions.Collections]) and returns what gives it back, once
// however often it is called. Held around a read of a run's box made on this
// node and nothing else ([Coordinator.boxResult], [Coordinator.readLostHere]):
// internal/sandbox/executor's base tier counts those reads at the node's turns,
// and a read asked of another node's host is bounded by that host's slots. A
// wait its context ends reads nothing ([errReadUnstarted]); a read that cannot
// start at once says so (`sandbox_collection_waits`).
func (c *Coordinator) collectionSlot(ctx context.Context, turnID string) (func(), error) {
	if c.collections == nil {
		return func() {}, nil
	}
	give := sync.OnceFunc(func() { <-c.collections })
	select {
	case c.collections <- struct{}{}:
		return give, nil
	default:
	}
	waiting := c.collectionsWaiting.Add(1)
	defer c.collectionsWaiting.Add(-1)
	log.InfoContext(ctx, "sandbox_collection_waits", "turn_id", turnID,
		"collections", cap(c.collections), "waiting", waiting,
		"detail", "this node is making as many reads of its runs' boxes as it runs turns, so this "+
			"read waits for one of them to end")
	select {
	case c.collections <- struct{}{}:
		return give, nil
	case <-ctx.Done():
		log.InfoContext(ctx, "sandbox_collection_unstarted", "turn_id", turnID,
			"collections", cap(c.collections), "error", ctx.Err().Error(),
			"detail", "this read's wait for one of the node's collections ended before it started; the "+
				"box was not read, and the read is asked again")
		return nil, fmt.Errorf("%w: waiting for one of this node's %d collections to read %s: %w",
			errReadUnstarted, cap(c.collections), turnID, ctx.Err())
	}
}

// errReadUnstarted is a read of a run's box on this node whose wait for one of
// the node's collections its context ended ([Coordinator.collectionSlot]). The
// box was not asked, so it says nothing of the box: a collection hands its
// claim back uncounted ([Coordinator.collectFailedAt]), and a stop or a loss
// keeps its ending for the retry.
var errReadUnstarted = errors.New("sandbox: the read of the run's box did not start")
