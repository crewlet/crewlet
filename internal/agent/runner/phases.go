package runner

import (
	"context"
	"fmt"

	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerfit"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
	"github.com/crewlet/crewlet/internal/tracing"
)

var log = logging.Get("agent.runner")

// Caps are the round-cap extension's switch and step, for the one phase it can
// reach: onboarding, the only engine-driven phase with a ceiling above its
// budget ([Onboarding.Ceiling]). The executor is bounded by its run's turn cap
// (role.sandbox.max_turns, else providers.sandbox.default_max_turns), not
// rounds; the reviewer holds one submission tool, so its budget is structural.
type Caps struct {
	ExtensionStep int
	ExtensionOn   bool
}

// reviewRounds is the reviewer's whole budget: one submission, the tool loop's
// two finishing correctives when a model answers without calling it, and one
// spare. The correctives are what a reviewer that stopped gets INSTEAD of the
// rescue, which would send the whole turn back for another executor round.
const reviewRounds = 4

// Config is everything a runner needs that does not change between rounds.
type Config struct {
	Seat     prompts.Seat
	Registry *tools.Registry
	Models   *phase.Registry
	Caps     Caps

	// Withheld are the first-party tools of Registry this seat is not offered on
	// any surface of the turn ([Runner.seatSnapshot]): those the authority table
	// refuses the seat before any object is named (internal/agent/builtin's
	// Withheld, a row naming a role grant its role lacks) and the code writes where
	// its code tier writes no repository (builtin's CodeWritesWithheld).
	//
	// THE REGISTRY IS THE EPOCH'S, shared by every seat, so a tool only some seats
	// may call is registered for all and left off the rest's surfaces here;
	// offered and refused at every call, a model would reach for it round after
	// round. Nil withholds nothing.
	Withheld []string

	// Judge decides round-cap extensions. Nil means every exhaustion goes
	// straight to the rescue path.
	Judge extension.Judge

	// Compact rewrites a payload past its budget with the seat's auxiliary model
	// (prior rounds' argument values, failed calls' errors and text). The zero
	// value rewrites nothing: the ledger renders a draft whole and omits a payload
	// by size, never a cut (internal/agent/ledger/ledgerfit).
	Compact compact.Bound

	// Subagent is what this turn needs to spawn sub-agents: the company's caps and
	// a way to read the seat's remaining allowance. Nil leaves spawn_subagent off
	// every surface.
	Subagent *SubagentConfig

	// Budget is the shared token counter a turn charges. Nil disables the
	// per-round charge (embedded single-node).
	Budget toolloop.BudgetMeter

	// Metrics is the node's recorder every surface of the turn counts into
	// ([metrics.ToolPolicyRefusalLoops], [metrics.ToolArgsOutsideSchema]), the
	// executor's bridged calls included. Nil counts nothing.
	Metrics *metrics.Recorder

	// Sessions is where every engine-driven phase serves its tools to its
	// harness: this process's one [toolsession.Host], whose directory admits a
	// harness's relay. Required; the executor's run alone reaches the seat's tools
	// over the bridge instead.
	Sessions *toolsession.Host

	// Fence stops a turn whose seat this node no longer holds, or one a person
	// stopped: checked before the executor's launch and at the top of every round
	// of every phase, before tokens are spent.
	//
	// Admission ([internal/seat.Host.MayStart]) proves only that the seat was held
	// when the turn STARTED; a node losing a seat mid-turn would keep calling
	// models and tools and publishing records for a seat a peer now runs. Nil
	// disables it (single node, tests with no seat host).
	Fence func() error

	// Task is the ask, and Endings how the conversation's earlier turns ended
	// (internal/agent/prompts' BuildConversationEndings), rendered at dispatch.
	// Both are fixed for the turn, Endings as [Config.Context] is (frozen on park,
	// thawed from it) and empty with [Config.Resume].
	Task    string
	Endings string

	// Access is the seat's "## Your access" section and CapabilityChanges what
	// changed since it was last told (internal/capability), rendered at dispatch
	// and frozen with Endings; both empty with [Config.Resume].
	Access            string
	CapabilityChanges string

	// Reply says who is waiting for this turn, so the executor's submission can be
	// checked against a fact the model does not control ([turn.Reply]).
	Reply turn.Reply

	// PickupAnswer reads off the tracker's rows how this turn left the item a work
	// pickup handed it ([turn.Reply.Item], ADR-0122). Required where Reply names an
	// item; read by the submission check and after every phase.
	PickupAnswer func(ctx context.Context) (types.PickupAnswer, error)

	// Context is what this seat remembers, its company has written down, it has
	// done before, and who it is talking to, rendered by the caller BEFORE the turn.
	//
	// Strings rather than a seam, and that is the freeze: the executor is launched
	// again on a self_iterate loop, and a runner able to re-fetch would build a
	// different system prompt each launch, costing the whole cached prefix and
	// changing context under mid-way work. A turn wanting fresher knowledge uses
	// search_knowledge.
	//
	// EMPTY WITH [Config.Resume]: a collected turn's context is the one its park
	// froze ([execstate.State.Context]); [New] refuses a resume handed both.
	Context prefetch.Blocks

	// Skills is the company's tool-skill registry. Nil (none published) disarms the
	// required-skill guard too: nothing to enforce.
	Skills *skills.Registry

	// SkipNames are the meta-tools the ledger filters out.
	SkipNames []string

	// Publisher receives the phase telemetry (telemetry.go), the ONLY source of
	// every agent_* event: without it a seat renders idle for the whole turn and
	// leaves no durable record it ran. Nil publishes nothing (direct tests, and
	// sub-agents, whose host phase is the visible one).
	Publisher queue.Publisher

	// Turn identifies the turn these events belong to.
	Turn Turn

	// OnPhase is called SYNCHRONOUSLY as each phase OPENS, immediately before its
	// first provider call, so a blocking implementation delays the turn (the
	// engine's only writes state and wakes a goroutine,
	// [notify.StatusSession.Phase]). Nil for anything driving a runner directly.
	//
	// It feeds the working indicator, which must not depend on a broker round trip
	// to learn the phase. ONE SEAM, wired where agent_phase_started is published
	// ([emitter.started]), so the name an indicator shows and the one dashboards
	// read cannot drift; it fires with no publisher too, since a person is still
	// waiting.
	OnPhase func(phase.Phase)

	// Onboarding wires the dedicated first-turn pass. Zero disables it, as a node
	// with no marker store has: a pass that could never be marked would run every
	// turn.
	Onboarding Onboarding

	// Resume collects the executor run the turn parked on. Non-nil makes
	// this runner's turn a RESUME: its first round collects what the run
	// did rather than launching one. See [Runner.Resume].
	Resume *Resume

	// Launcher starts the executor: every seat's executor is its coding harness's
	// own run, detached in a box. REQUIRED: [New] refuses a runner without one
	// rather than fail a turn at its first Execute (agentrun.go).
	Launcher AgentLauncher

	// Ask is the tool the executor's run asks a person through and parks on
	// (`ask_and_wait`, ADR-0068), offered beside `submit_work`, the run's two ends.
	// Nil offers none.
	Ask RunAsk

	// Checkout is the tool the executor's run checks a Gitlet repository out
	// through (`code_checkout`, ADR-0093), offered beside the ask: it grants a
	// repository to the running job's row, as no other phase does. Nil offers none.
	Checkout tools.Callable

	// Hold records a call the executor's run makes to a tool at Needs approval as
	// the question it parks on (internal/agent/builtin's CallHold, ADR-0077),
	// installed on that run's surface alone. Nil holds none and the surface
	// refuses every such call, as every other phase's does.
	Hold tools.Holder

	// Runs is what the executor's run sees the runs in flight around it with
	// (ADR-0176): the read its launch's brief lists, and the tools on its
	// surface (`list_runs`, `message_run`, `wait_for_run`), offered to the run
	// where that read found any. Nil reads and offers none.
	Runs RunsAround

	// Steer is where the turn's notes from a person wait (internal/agent/steer):
	// taken at the opening of every run of the REVIEWER and by the brief of every
	// launch of the executor. Never a worker's (a leaf its parent directs), the
	// onboarding pass's (orientation) or the extension judge's (a question about
	// the phase): a note waiting while one of those runs is read by the next step
	// of the turn's own.
	//
	// A note read once is carried into every later phase of the turn as the same
	// message, so a correction binds later work and the reviewer judging it
	// ([Runner.steered]). Nil is a turn nobody can steer (tests, direct runners).
	Steer SteerSource
}

// SteerSource is where a turn's notes from a person wait, as the runner reads
// them: the first segment's box ([steer.Box]), or the record of the executor
// a resumed segment collects. Declared here, by the consumer.
type SteerSource interface {
	// Take takes every note waiting, recording it read where at says, and answers
	// what it took.
	Take(ctx context.Context, at steer.Reading) []steer.Note

	// Brief is the notes a launch's brief takes, oldest first, and every note the
	// turn has read whose read its record marks unanswered as the brief read it
	// ([steer.Briefing]). They are the launch's from here: recorded read by the
	// write opening the run's job, or handed back by [SteerSource.Return] where the
	// launch fails first.
	Brief(ctx context.Context) steer.Briefing

	// Return hands back notes [SteerSource.Brief] gave a launch that failed BEFORE
	// IT WROTE ITS RUN'S RECORD ([Launched.LaunchID] empty): they reached nobody. A
	// box keeps them at its front or reports them expired when it closes; a run's
	// record, which a brief only peeks, still holds them waiting. A launch failing
	// after the write is never handed back: the record holds the notes read by a
	// job that never ran, and its ending reports them once.
	Return(ctx context.Context, notes []steer.Note)

	// Waiting reports a note waiting WITHOUT TAKING IT or blocking: the tool loop
	// asks it under its dispatcher's lock, to end a run once the calls in flight
	// are answered so the next run opens with the note.
	Waiting() bool
}

// RunAsk is the tool an executor's run asks a person through and parks on
// (`ask_and_wait`, ADR-0068), as the runner offers it.
//
// Gate is the run's submission as its surface offers it beside the ask: a job
// that asked is parked, and its harness is told so if it submits rather than
// ends. The collection holds the same line ([Resume.Answered]), so a
// submission let through in a race with the ask is still not the round's.
type RunAsk interface {
	tools.Callable
	Gate(submit tools.Callable) tools.Callable
}

// RunsAround is the runs in flight around an executor's run and the tools it
// sees them with ([Config.Runs]). Declared here, by the consumer.
type RunsAround interface {
	// Around is every other run in flight around turnID's run.
	Around(ctx context.Context, turnID string) ([]prompts.RunAround, error)

	// Tools is the tools the run is offered while Around finds any.
	Tools() []tools.Callable
}

// Telling is one late call of the round a job's session is known to hold
// ([Resume.Tellings]): its place in the round's log ([BridgedCall.Appended])
// and among the turn's reads of the write that recorded it, which a capture's
// note cursor is read against ([SessionBasis.NoteCursor]).
type Telling struct {
	Appended, Place int

	// Hook says a hook's step read the call (sandbox.CallTelling.Hook): held as
	// the job's session showed it, which a box whose release the resume was
	// not read in may not rebuild ([UnheldTelling]).
	Hook bool
}

// Resume is the collection of the executor run a turn parked on.
type Resume struct {
	State execstate.State

	// Continued says the box of the job State parked said it took up the session
	// its launch continued, by the bridge token its harness dialled
	// (sandbox.LaunchRecord.SessionStart): the one word on which session the job ran
	// in that its model could not forge. False where it started a session of its
	// own, continued none, or said nothing; every rebuild of the run's skill guard
	// takes that for a session of its own ([Resume.parked]).
	Continued bool

	// HarnessEnd is how the parked job's harness ended (sandbox.LaunchRecord's
	// HarnessEnd): its own run (job over, whose box copy holds every call, note
	// and load); cut off (killed, crashed, stopped, errored, or ended before its
	// harness started); or nothing (record not taken). ANYTHING BUT AN OWN RUN
	// ([sandbox.HarnessEnd.EndedOwnRun]) may lack calls its harness made and had
	// answered unwritten, and is not known to hold the brief, so a relaunch
	// continuing in the box tells that session it was cut off, of every call and
	// note read, and arms none of the job's own loads ([Runner.inPlaceAfter],
	// [Runner.inPlaceNotes], [Runner.sessionDecided]): the safe direction. An own
	// run's copy is told only the late calls and the reads its brief counted
	// unlisted (Owed), and first that a submission among them is not the
	// round's ([Runner.calledAfter]).
	HarnessEnd sandbox.HarnessEnd

	// Owed is the reads of the round the job's brief counted without listing them,
	// its read cap spent, by place in the round's log
	// (sandbox.LaunchRecord.OmittedCalls). The box's session copy holds them only
	// as that count, so a relaunch carrying the round on there tells them first, in
	// words of their own, where the harness ended its own run
	// ([Runner.inPlaceAfter]), and its guard counts none of their loads ([heldAt]).
	// Nil where none were left out or the record names none: the copy is then read
	// as holding every call its brief was to tell.
	Owed []int

	// LateOwed is the late calls that same brief counted without listing
	// (sandbox.LaunchRecord.LateOmittedCalls), apart from Owed: every brief tells
	// them again, and the copy is credited no telling of one ([heldByTelling]).
	LateOwed []int

	// Tellings are the late calls of the round's log a job's own session is known
	// to hold, each at the place among the turn's reads of the write that recorded
	// it (sandbox.RunSteer.Tellings, the log's round alone): a continuation of that
	// session is not told the call again ([heldByTelling]). Nil where none.
	Tellings []Telling

	// ContextTokens is what the session of the parked job's box carried at its
	// newest request the engine measured (sandbox.LaunchRecord.Context), or the
	// figure carried along the copy's lineage where the job made none. A relaunch
	// continuing the round in that box sends the copy's whole context again before
	// its brief, so its floor is the larger of this and the newest capture's
	// measure ([Continuation.ContextTokens]). Zero where the record holds none.
	ContextTokens int64

	// NextRequest is the harness's own estimate of the copy's next request
	// (sandbox.LaunchRecord.NextRequest), which a relaunch continuing in that box
	// is held to with its brief ([Continuation.NextRequest]). Zero where none.
	NextRequest sandbox.NextRequest

	// Count is what the copy's harness counts the parked job's box's session at
	// before its next request (sandbox.LaunchRecord.ResumedCount): what a
	// continuing relaunch judges its first request by, and, where it continues a
	// kept capture taken whole in the same box, the count the capture's own is held
	// beside ([keptCount]). Nil where nothing read the copy, read as a copy that
	// may compact. CountProvider is the provider of the reply an OpenCode count is
	// anchored on (sandbox.LaunchRecord.CountProvider), "" for any other.
	Count         *sandbox.ResumedCount
	CountProvider string

	// BasisRestart is the newest restart of the session the parked job took up, as
	// its launch knew it (sandbox.LaunchRecord.RestartBasis): "" for none, nil
	// where unknown. The job's copy and a capture of the job are compared with it
	// to know whether the job restarted its session after its brief
	// (sandbox.RestartedSince).
	BasisRestart *string

	// Leads is the turn's leads as the run keeps them beside its record, opened,
	// oldest first ([ResumeLead], sandbox.PendingStore.Leads): what carried each
	// session of the work on. Those the job's brief told are marked
	// ([ResumeLead.Told]) and told again to a continuation that may lack that
	// brief ([Runner.keptSession], [Runner.inPlaceAfter]); LeadsCut is how many
	// earlier leads are not kept (past the bound, or the store could not give them
	// back), said beside them.
	Leads    []ResumeLead
	LeadsCut int

	// LeadsUnread is the leads the job's brief told where the store could not give
	// the turn's leads back at all, by number in the run's leads part
	// (sandbox.LaunchRecord.ToldLeads, sandbox.TurnLeads.Unread): none is in Leads,
	// and LeadsCut counts them. A continuation of that session says them among the
	// leads not kept and hands them on (sandbox.BriefTold.ToldUnread), so the job
	// its launch opens marks them. Nil where the leads were read.
	LeadsUnread []int

	// Base is the session the conversation's last turn left, as the turn's row
	// froze it (sandbox.PendingRun.Base), which a task lead told again says
	// ([prompts.NextTurnAgain]). Nil where the turn continues none.
	Base *BaseBasis

	// Answer is the run's closing text: the phase's prose, read by the reviewer as
	// what the agent produced.
	Answer string

	// Bridged is what the run called over the MCP bridge, from the run's own
	// durable log kept beside its row, in log order; each carries its place in the
	// order the bridge admitted it ([BridgedCall.Seq]), the order the resume
	// records them in. No round, which is this package's to state ([runRound]).
	//
	// It is the whole record of the phase and must come from the log: the process
	// resuming may not be the one that launched, so its surface has executed nothing.
	Bridged []BridgedCall

	// Unfinished is the round's calls the log held IN FLIGHT when read
	// (sandbox.BridgeLog.InFlight): begun, identity on the record, not finished
	// when the collection stopped waiting (past their bound or the turn's cap,
	// process gone, or update refused), so outcome unknown
	// ([ledger.Call.Unfinished], [UnfinishedNote]). Seeded into the turn's call log
	// by identity ([seedCalls]), listed after the round's calls for the reviewer
	// and tool log, told to a run carrying the round on beside its late calls
	// ([Runner.calledAfter]); never a submission, a delivery or a load.
	Unfinished []BridgedCall

	// Elided is the round's calls the log does not list, by identity
	// (sandbox.BridgeLog.Unlisted): a long run's dropped middle. Still calls of the
	// round: seeded into the call log ([seedCalls]), counted by the delivery checks
	// and carried on in the record ([turn.Work.Elided]).
	Elided []ledger.Elided

	// Absorbed is the calls the run's call log took from the round's calls' work (a
	// delegated worker's under a bridged `delegate`), by identity
	// (sandbox.BridgeLog.Absorbed): seeded beside the round's calls ([seedCalls])
	// and carried on in the record ([turn.Work.Absorbed]), read by nothing else.
	Absorbed []ledger.Elided

	// LateElided is how many of Elided's calls the log took late
	// (sandbox.BridgeLog.LateElided): dropped, they keep no mark, so every
	// relaunch continuing the round's session is told how many, in the late calls'
	// words, whatever its save point ([Runner.calledAfter],
	// prompts.AfterSave.LateDropped): no save point holds a late call.
	LateElided int

	// Answered is the place ([BridgedCall.Seq]) of the last call before a person's
	// answer carried the round on, zero where none has
	// (sandbox.BridgeLog.AnsweredThrough). Only a submission after it is the
	// round's ([sinceAnswer]).
	Answered int

	// BridgedRead is the read of the round's log Bridged is
	// (sandbox.ResumeRequest.BridgedRead): every call a brief built from Bridged
	// can tell. Every relaunch hands it to its launch ([AgentRunRequest.BriefRead]),
	// whose opening marks late each call the log took after it ([ledger.Call.Late]).
	BridgedRead sandbox.LogRead

	// Run describes the detached run this resume is collecting.
	Run RunRecord

	// Asked is set when a person's answer, not the run's completion, drives this
	// resume: the turn CARRIES THE RUN ON with it ([Asked]). Answer is then empty
	// and Bridged holds the calls made before it asked.
	Asked *Asked

	// Lost is set when the run's job was LOST with its host rather than ended: the
	// turn carries the round on ([Lost]) unless its calls already hold the
	// submission. Answer is the job's own account, marked cut off.
	Lost *Lost

	// Rotated is set when the job ended refused by its vendor on a sign-in
	// replaced while it ran: the turn carries the round on once with the new
	// sign-in ([Rotated]), unless its calls already hold the submission.
	Rotated *Rotated

	// Refused is set when the job ended on a request its provider refused in a
	// session it continued: the turn carries the round on once in a fresh session
	// ([Refused]), unless its calls already hold the submission.
	Refused *Refused

	// Limited is set when the job met its model's usage limit before any work:
	// the turn carries the round on once, fresh, on the next member ([Limited]).
	Limited *Limited

	// RequestRefused is the provider's message where the job this resume COLLECTS
	// ended on a request refused in a session it started or after the round was
	// carried on once: the round ends the turn with it
	// ([turn.Work.RequestRefused]). Empty otherwise.
	RequestRefused string

	// Steered is every note the turn had read when the run was collected, and
	// where, in order read, off the run's record (a brief's, the run's own, a
	// reviewer's). The resumed turn opens every phase with them
	// ([Runner.steered]); the collected run's own reads are its record's
	// ([Runner.resumeAgentRun]).
	Steered []steer.Read

	// Served is every note, by id, the run's record says its bridge served a
	// harness while it waited (sandbox.RunNote.Served): a polling harness is
	// answered a note only once it is recorded served and records it read only on
	// confirming delivery, so a session continued from a job served one may hold it
	// whatever the record says of its read ([Runner.toldNotes]). A fresh run is
	// told it as every note.
	Served []string

	// Wakes are the wakes on the run's record a session continued from it may
	// hold (sandbox.RunNote.Wake), in the order filed: each a step read, the
	// turn's worked, and each its job was served and never shown reading. A
	// continuation that may lack one tells it again ([Runner.toldWakes]), and a
	// fresh run is told every one read ([Runner.wakesRead]).
	Wakes []HeldWake
}

// HeldWake is a wake a run was handed ([Resume.Wakes]): the wake; its place
// among the turn's reads, its read's or else its hook serve's, zero for
// neither; whether a step read it; and whether that read is one no session of
// its job is known to hold, taken before the session's last restart
// (Restarted) or at a hook's step (Hook, for a box [UnheldTelling] names).
type HeldWake struct {
	steer.Wake
	Place     int
	Read      bool
	Restarted bool
	Hook      bool
}

// ResumeLead is one lead of the turn's, as the resume opened it.
type ResumeLead struct {
	// Lead is the lead, opened; one that did not open on this node names its
	// kind, job and round and why ([sandbox.Lead.Unread]), and is told as a
	// message that cannot be read here.
	Lead sandbox.Lead

	// Told says the brief the record's job's run read told it
	// (sandbox.LaunchRecord.ToldLeads).
	Told bool

	// Result is what a held lead's call returned, opened from the seal its row put
	// on it (sandbox.CallSeal.OpenKept); nil for every other lead and for one that
	// did not open here, told as a call whose result cannot be read on this node.
	Result *sandbox.HeldResult
}

// parked is the job's parked state as the session it ran in holds it: State,
// whose skills ([execstate.State.LoadedSkills]) are that session's only where
// its box said it took the session up ([Resume.Continued]). Otherwise the
// session read none of them, and every guard rebuilt for it holds only what the
// job itself loaded, replayed off its calls ([Runner.rebuiltSurface]).
//
// THE SAFE DIRECTION FOR A WORD THAT NEVER CAME: a guard short of a skill asks
// the run to load it again; one holding a skill its session never read lets
// through a call the skill exists to hold back.
func (r Resume) parked() execstate.State {
	state := r.State
	if !r.Continued {
		state.LoadedSkills = nil
	}
	return state
}

// BridgedCall is one call an executor run made over the MCP bridge, as the
// run's durable log recorded it: the call the turn's record reads, and when it
// ran, how long, and who answered.
//
// ITS OWN TYPE rather than a [ledger.Call], the delivery check's view, which
// has nowhere for a call's place or timing: carried as one, a resumed run's
// record stated every call as untimed, unattributed and in finish order.
type BridgedCall struct {
	// Call is what the call did. A late one ([ledger.Call.Late]) is HELD BY NO
	// SAVE POINT ([heldAt]): a continuation built after it landed is told it in
	// words of its own unless a telling credits it ([heldByTelling]), no rebuilt
	// guard counts a load it made, and it is never the round's submission
	// ([sinceAnswer]).
	ledger.Call

	// Seq is the call's place in the order the bridge admitted the run's calls,
	// from 1: the order the harness made them, not the order they finished. One
	// order for the round: a job opened after a recovery, or carrying on one that
	// asked a person ([Asked]), numbers after the calls before it
	// (sandbox.BridgeLog.LastSeq).
	Seq int

	// Appended is the call's place in the order the round's log TOOK its calls,
	// from 1 (sandbox.BridgeCall.Appended): what a continued session's save point
	// is read against ([SessionBasis.Through]), since a call the log took after the
	// read that placed the save may be unanswered in the session whatever its Seq.
	// Zero is read by every save point as after it.
	Appended int

	// Abandoned is a call nobody waited for by the time it returned
	// (sandbox.BridgeCall.Abandoned): its harness cancelled it (Claude Code and
	// OpenCode cancel at the server's timeout and discard a late answer) or hung up
	// on the response, or its job's bridge session ended while it ran. It ran and
	// its effect happened, while the job's session shows it with no outcome, failed
	// or "still running". HELD BY NO SAVE POINT, as a late call is ([heldAt],
	// [heldByTelling]), and no rebuilt guard counts a load it made. Unlike a late
	// call it may be the round's submission ([sinceAnswer]): its job, never
	// replaced, made it.
	Abandoned bool

	// StartedAt is when the call was handed to the seat's surface and Duration how
	// long it took to answer, measured at the bridge since the call ran inside the
	// harness's loop. Both zero on a call answered without running; zero means
	// "not measured", never "instant".
	StartedAt time.Time
	Duration  time.Duration

	// Origin and Server are who answered the call — a builtin, or an MCP
	// tool and the server it came from — and empty where no tool did.
	Origin string
	Server string

	// Source is whose tool the call named, answered or refused, and Refusal
	// the class of a failed call the engine classified (see
	// [toolloop.Execution]).
	Source  string
	Refusal mcp.Refusal

	// ArgsBytes and OutputBytes are the size of the call's whole arguments and
	// output where the record kept less (sandbox.BridgeCall.ArgsBytes), zero where
	// whole. A reader that ACTS on a payload decides by them: the collection never
	// replays a submission its record did not keep whole ([replaySubmission]).
	ArgsBytes, OutputBytes int

	// Held is a call held for a person's approval, recorded failed though nothing
	// failed (it is the run's question), and Approved the call they let be made
	// (sandbox.BridgeCall): the two whose arguments every record withholds
	// ([ledger.Call.Withheld]), which the execute phase's record says in place.
	Held, Approved bool

	// HeldID names the held call an Approved call was made for
	// (sandbox.BridgeCall.HeldID), "" on every other call: it tells the call a
	// person approved for the answer a relaunch carries the round on with, whose
	// outcome that relaunch's lead tells ([Asked.Held]), from one an earlier answer
	// approved, which the brief tells among the round's calls ([Runner.calledAfter]).
	HeldID string

	// CallID is the id the call's harness gave it (sandbox.BridgeCall.CallID):
	// for Claude Code the model's tool_use id, which the session's transcript
	// files the call under. Carried for an in-flight call too. Empty where the
	// harness sent none (OpenCode), on a skill load a hook reported, and on a
	// call a person approved.
	//
	// THE BOX'S WORD, AND NO PART OF THE CALL'S IDENTITY: nothing finds,
	// compares or deduplicates by it. It is read only to ask which calls a
	// cut-off kept session holds (sandbox.HoldingsAsk). Carried HERE, NOT ON
	// [ledger.Call], which the record persists and the telling compares whole
	// ([leftOut]): no brief line shows it.
	CallID string

	// ArgsText is the call's arguments as its log keeps their text
	// (sandbox.BridgeCall.Args), unchanged: "" where none is kept, a marker's text
	// where cut ([BridgedCall.ArgsBytes]). Read only to key an OpenCode call's
	// arguments for a reading of what a cut-off session holds
	// ([Runner.holdingsAsk]), by the log's own rule on both sides
	// (internal/tools/toolargs' Key), never rebuilt from Args, which reads "" and
	// unreadable text alike as none.
	ArgsText string
}

// bridgedLedger is an executor run's calls as the turn's record reads them:
// what each call did, without the measurement the bridge took of it.
func bridgedLedger(bridged []BridgedCall) []ledger.Call {
	out := make([]ledger.Call, 0, len(bridged))
	for _, call := range bridged {
		out = append(out, call.Call)
	}
	return out
}

// RunRecord is what a phase event says about the run an executor was and the
// box it ran in.
//
// It lets the executor's record name the harness, box and deliveries
// (`harness`, `sandbox_id`, `delivered_refs`), which only the harnesses report.
// What the run cost is NOT here: it is each launch's own fact, on the
// `executor_collected` record its collection publishes.
//
// Carried rather than re-derived, as the run's placement is: the resume may be
// another process on another node, days later, under a re-applied company
// configuration, and the member of the seat's executor chain that took the
// launch is the launch's own answer, not the chain's head.
type RunRecord struct {
	// Harness is the harness that did the work; SandboxID the box it ran in.
	Harness   string
	SandboxID string

	// ProviderKey is the providers.llm entry the run was launched on and Model the
	// model it ran: the member of the seat's executor chain that took the launch.
	// The Execute record names them as what served the phase, so spend by provider
	// and a turn's `execute_model` are charged to the member that ran.
	ProviderKey string
	Model       string

	// DeliveredRefs are the branches and pull requests the run produced.
	DeliveredRefs []string

	// RefsElided is how many more the run reported than DeliveredRefs lists,
	// which the coordinator bounds — said on the record rather than dropped.
	RefsElided int

	// Pushed is what the run's push record says it moved on Gitlet, PushedElided
	// how many more refs it names than Pushed lists, and PushedUnread why it could
	// not be read: the engine's account beside the run's own refs
	// ([types.AgentPhaseCompleted.PushedRefs]).
	Pushed       []types.PushedRefRecord
	PushedElided int
	PushedUnread string

	// LaunchID is the job this resume collected ([sandbox.PendingRun.LaunchID]
	// when the claim took it): it tells one collected run's record from the next,
	// as a turn can launch more than once.
	LaunchID string

	// RefusedReplies are the replies the engine refused as too large to hand the
	// run ([turn.Work.RefusedReplies], sandbox.PendingRun.RoundRefusals), carried
	// to the round's reviewer.
	RefusedReplies []turn.RefusedReply
}

// Sandboxed reports whether the record names the run's harness or its box: whether
// the phase collected a run at all, rather than closing on a launch that never
// opened one.
func (r RunRecord) Sandboxed() bool { return r.Harness != "" || r.SandboxID != "" }

// Runner implements [turn.Phases] against real models and real tools. One per
// turn ([Company.RunnerFor]), which makes the spend tally a per-turn fact.
type Runner struct {
	cfg   Config
	spend Spend

	// mu guards suspension, which the Execute phase writes and the engine
	// reads once the turn returns, and guard, which surfaceWith writes and
	// a park reads back.
	mu         sync.Mutex
	suspension *Suspension

	// guard is the required-skill gate of the phase being built or run, or nil.
	//
	// A field, not a return value, because only ONE caller wants it: a park, which
	// persists the keys the run's surface had loaded so the collection's surface
	// is armed alike. One phase runs at a time on a runner.
	guard *skills.Guard

	// steered is every note this turn has read, and where, in read order, opened
	// into every LATER phase's conversation after its opening messages and into
	// every later run's brief, each as the message the model read
	// ([prompts.SteerMessage]).
	//
	// A phase is a fresh conversation, so a note read in one is gone from the next
	// unless carried: a later reviewer would judge against the uncorrected task.
	// Carried as the SAME message, not summarised: the words are the person's and
	// a paraphrase the engine's. A resuming turn starts from the reads its run's
	// record holds ([Resume.Steered]). Guarded by mu.
	steered []steer.Read

	// placed is the place among the run's record's reads of every note this
	// segment's source handed over and of every note the brief carries whose place
	// the record holds, by id ([steer.Briefing.Orders]); nil where no record. A
	// brief tells reads in that order ([Runner.inReadOrder]). Guarded by mu.
	placed map[string]int

	// sent is every record this runner publishes off its request's path (the frame
	// stating the fresh brief a run whose box started its own session read,
	// [Runner.sessionStarted]), each bounded by its own budget: the runner's
	// goroutines, which [Runner.Sent] waits for.
	sent sync.WaitGroup
}

// Sent waits until every record this runner publishes off a request's path has
// gone out or given up within its budget ([startedFrameBudget]), so a caller
// reading what they publish (a test) does not race them.
//
// NOTHING ON THE ENGINE'S PATH WAITS HERE and nothing orders such a record
// after the round's later ones: the round is closed by a later segment's
// runner on whichever node claims the run, which cannot wait on this one. A
// frame published while the broker is slow can land after the round's
// completion; one past its budget is dropped, and the collection still says
// the run started a session of its own.
func (r *Runner) Sent() { r.sent.Wait() }

var _ turn.Phases = (*Runner)(nil)

// New builds a runner.
//
// A runner with no session host is refused here, not at its first phase: every
// turn runs a phase that offers tools (the reviewer at least), so it would fail
// after the run launched and prefetch was spent, naming a toolloop field.
func New(cfg Config) (_ *Runner, err error) {
	defer origin.Stamp(&err)
	switch {
	case cfg.Registry == nil:
		return nil, fmt.Errorf("runner: no tool registry")
	case cfg.Models == nil:
		return nil, fmt.Errorf("runner: no provider registry")
	case cfg.Sessions == nil:
		return nil, fmt.Errorf("runner: no tool session host (Config.Sessions) — " +
			"every phase serves its tools to its harness on this process's one host")
	case cfg.Launcher == nil:
		// AN ASSEMBLY MISTAKE, named where it is made: every executor is a launched run.
		return nil, fmt.Errorf("runner: no executor launcher (Config.Launcher) — " +
			"every seat's executor is its harness's own run, and the " +
			"assembler must supply what launches it")
	case cfg.Resume != nil && frozen(cfg.Context, cfg.dispatched()) != (execstate.Frozen{}):
		// TWO SOURCES FOR ONE FROZEN THING, no honest way to pick: the parked state is
		// what the first launch was built from, and a context fetched again would be a
		// second answer to the same question. Refused where assembled.
		return nil, fmt.Errorf("runner: a resumed turn's context is the one its parked " +
			"state froze at the turn's start — leave Config.Context and " +
			"Config.Endings, Config.Access and Config.CapabilityChanges empty with Config.Resume")
	}
	if cfg.Resume != nil {
		var d dispatched
		cfg.Context, d = thawed(cfg.Resume.State.Context)
		cfg.Endings, cfg.Access, cfg.CapabilityChanges = d.endings, d.access, d.changes
	}
	return &Runner{cfg: cfg}, nil
}

// Execute is the executor: one agentic pass, launched as the seat's harness's
// own run detached in its box and collected through [Runner.Resume]. It is
// given every tool its seat holds but those of a server running in its box
// ([executorActive]), none in its prompt: each reaches its harness over the
// bridge, which defers a large server's tools itself (internal/api/mcpbridge,
// servers.go). One phase, not two: a separate planner NAMED tools against a
// catalogue it was never shown. TWO GATES BEFORE ANYTHING IS BUILT: A REFUSAL
// ALREADY CERTAIN launches nothing (the run is post-charged at collection, so
// the meter is asked here and again once the brief is built,
// [Runner.executeAsAgentRun]); THE SEAT FENCE, because a run outlives its
// segment and is not fenced once running, so the launch is the last moment a
// moved or stopped seat can be kept from starting one.
func (r *Runner) Execute(ctx context.Context, round int, notes string, history []ledger.Iteration) (_ turn.Work, _ turn.Surface, err error) {
	defer origin.Stamp(&err)
	if err := toolloop.Refusal(ctx, r.cfg.Budget); err != nil {
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, err)
	}
	if r.cfg.Fence != nil {
		if err := r.cfg.Fence(); err != nil {
			return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, err)
		}
	}
	work, surface, ran := r.executeAsAgentRun(ctx, round, notes, history)
	if ran == nil && !work.Suspended && r.cfg.Reply.Item != "" {
		work.Answer = r.pickupAnswer(ctx)
	}
	return work, surface, ran
}

// pickupAnswer reads how the turn has left the item its reply names
// (ADR-0122). A failed read answers [types.PickupUnread], LOGGED, which the
// checks accept: the turn falls back to the delivery every turn owes rather
// than loop on an unreachable store, and an item left in words comes back on
// its recheck.
func (r *Runner) pickupAnswer(ctx context.Context) types.PickupAnswer {
	if r.cfg.PickupAnswer == nil {
		return types.PickupUnread
	}
	answer, err := r.cfg.PickupAnswer(ctx)
	if err == nil && !answer.Valid() {
		err = fmt.Errorf("the read answered %q, which this build does not know", answer)
	}
	if err != nil {
		log.WarnContext(ctx, "pickup_answer_unread", "item", r.cfg.Reply.Item,
			"error", err.Error(), "detail", "how the pickup's turn left its item could not be "+
				"read, so any delivery counts for this check")
		return types.PickupUnread
	}
	if answer.Moot() {
		log.InfoContext(ctx, "pickup_moot", "item", r.cfg.Reply.Item, "answer", string(answer),
			"detail", "the item left the seat while its pickup's turn ran, so no typed "+
				"answer is owed on it")
	}
	return answer
}

// executorPrompt is the executor's opening: its system prompt and the turn's
// ask, handed to its harness as an appended system prompt and the prompt it
// runs ([AgentRunRequest]).
//
// THE BUILT SURFACE, not the snapshot: it also holds what [Runner.surfaceWith]
// adds (submit_work, `delegate`), so every section gated on a tool being
// offered reads built's names; from the snapshot `## Your workers` was rendered
// for no seat. No tool is listed (definitions reach the run over the bridge)
// and NO TOOL SKILL either: they are the harness's own ([Runner.handSkills]).
func (r *Runner) executorPrompt(ctx context.Context, notes string,
	history []ledger.Iteration, built *tools.Surface, fresh []steer.Note,
) (system, user prompts.Prompt) {
	return r.executorSystem(built), r.executorBrief(ctx, notes, history, fresh)
}

// executorSystem is the executor's system prompt, over the surface it is
// launched with — see [Runner.executorPrompt].
func (r *Runner) executorSystem(built *tools.Surface) prompts.Prompt {
	return prompts.BuildExecutor(r.cfg.Seat, prompts.ExecutorInput{
		AvailableTools: built.Universe().Names(),

		ThreadContext:       r.cfg.Context.ThreadContext,
		PersonalMemory:      r.cfg.Context.PersonalMemory,
		RelevantKnowledge:   r.cfg.Context.RelevantKnowledge,
		EpisodeRecall:       r.cfg.Context.EpisodeRecall,
		CounterpartyProfile: r.cfg.Context.CounterpartyProfile,
		SynthesizedSkills:   r.cfg.Context.SynthesizedSkills,
		OpenAsks:            r.cfg.Context.OpenAsks,
		Systems:             r.cfg.Context.Systems,
		Workers:             r.workerCatalogue(),
		Access:              r.cfg.Access,
	})
}

// executorBrief is the turn's own ask as a run that starts a session of its own
// reads it — see [Runner.executorPrompt]. It reads no surface, so a launch
// writes it before its surface is built.
func (r *Runner) executorBrief(ctx context.Context, notes string, history []ledger.Iteration,
	fresh []steer.Note) prompts.Prompt {
	return prompts.BuildPhaseUserMessage(prompts.UserMessage{
		TaskDescription: r.taskFor(notes),
		PriorWork:       r.priorWork(ctx, history),
		// HOW THE CONVERSATION'S EARLIER TURNS ENDED, as frozen at dispatch: every
		// brief tells the same account.
		Endings: r.cfg.Endings,
		// AND WHAT CHANGED IN THE SEAT'S ACCESS, frozen beside the endings.
		CapabilityChanges: r.cfg.CapabilityChanges,
		// EVERY NOTE THE TURN HAS READ, so a run launched after a correction starts
		// from the turn as corrected, then the notes this brief is first to read: the
		// run is a fresh session, so its brief carries them ([Runner.steered]). IN
		// THE ORDER THE RECORD READ THEM, including a lost take's note and a hook's
		// served one ([Runner.inReadOrder]). Then every wake a step of the turn's
		// run read, which the turn worked ([Runner.wakesRead]).
		Steered: append(r.readFirst(fresh), r.wakesRead()...),
	})
}

// readFirst is every note the turn has read, rendered, in the order its record
// read them, a note of fresh whose place the record holds already among them,
// and then the rest of fresh ([Runner.inReadOrder]).
func (r *Runner) readFirst(fresh []steer.Note) []string {
	reads, rest := r.inReadOrder(r.readsSince(0), fresh)
	out := make([]string, 0, len(reads)+len(rest))
	for _, read := range reads {
		out = append(out, prompts.SteerMessage(read.By, read.Text))
	}
	return append(out, renderedNotes(rest)...)
}

// renderedNotes is notes as the model reads each ([prompts.SteerMessage]).
func renderedNotes(notes []steer.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, prompts.SteerMessage(n.By, n.Text))
	}
	return out
}

// priorWork renders the turn's earlier rounds, every payload past its budget
// rewritten by the seat's auxiliary model first.
//
// Rewritten at render, not when the round closed: the record keeps payloads
// whole, so the bound is a property of what this prompt can carry. The
// compactor caches by content, so the block re-rendered for the reviewer pays
// once per payload.
func (r *Runner) priorWork(ctx context.Context, history []ledger.Iteration) string {
	fitted := ledgerfit.Fit(ctx, r.cfg.Compact, ledger.IterationPieces(history, r.cfg.SkipNames))
	return ledger.RenderIterations(history, r.cfg.SkipNames, fitted)
}

// work is one collected executor run, assembled for reporting.
type work struct {
	submit  *structured.Tool[workPayload]
	res     phaseResult
	surface *tools.Surface

	// run names the run and the box it ran in.
	run RunRecord
}

// finishWork turns a collected executor run into the turn's Work, publishing
// the phase record on the way.
func (r *Runner) finishWork(phaseCtx context.Context, round int, w work) (turn.Work, turn.Surface) {
	payload, submitted := w.submit.Value()
	if !submitted {
		// THE RESCUE PATH. A run that ended without submit_work (harness stopped, hit
		// its turn cap, or answered in prose) has text and no account of itself.
		// Discarding wastes what it did; calling it delivered puts words in its mouth.
		//
		// So the outcome is INCOMPLETE and the work marked rescued: the engine wrote
		// this word, so no fast path may act on it and the reviewer judges the record.
		//
		// AND NO SUMMARY: Summary is the INTENT line, rendered whole as "set out to" to
		// every later round and the reviewer; copying the transcript into it put a
		// whole run's output into the one unbounded slot, twice per round, under a
		// heading calling it the executor's own account. Its prose is already Text.
		log.WarnContext(phaseCtx, "work_never_submitted", "round", round)
		payload = workPayload{Outcome: string(turn.OutcomeIncomplete)}
	}

	// NO PROMPT ON THE RECORD: it went out on the launch's started record, and
	// this one collects a run; publishing it again would show a prompt not sent
	// this time.
	r.emitter().completed(phaseCtx, phaseRecord{
		Phase: phase.Execute, Iteration: round,
		Result: w.res.Result, Elapsed: w.res.Elapsed,
		StartedAt: w.res.StartedAt, Caps: w.res.Caps,
		Decision: payload.Outcome, Rescued: !submitted,
		Run:       w.run,
		RunSteers: w.res.RunSteers,
		RunElided: w.res.RunElided,
		// EVERYTHING IT HELD, no catalogue beside it: an executor holds every tool it
		// can reach from launch, so nothing was left to discover. Its in-box MCP
		// servers are the box's, not on the list.
		Available: w.surface.Active(),
	})

	return turn.Work{
		Outcome:       turn.Outcome(payload.Outcome),
		Summary:       payload.Summary,
		Deliveries:    payload.Deliveries,
		Evidence:      payload.Evidence,
		OpenQuestions: payload.OpenQuestions,
		Text:          w.res.Text,
		Calls:         calls(w.surface),
		Refs:          w.run.DeliveredRefs,
		RefsElided:    w.run.RefsElided,
		// AND WHAT THE RUN WAS NEVER GIVEN, for its reviewer: a reply refused as too
		// large is in no call.
		RefusedReplies: w.run.RefusedReplies,
		Rescued:        !submitted,
	}, describe(w.surface)
}

// discoverable is every MCP-served tool of a surface's universe where the
// surface can discover one (it holds `activate_tool`), and nothing where it
// cannot.
func discoverable(s *tools.Surface) []string {
	if !slices.Contains(s.Active(), toolname.Activate) {
		return nil
	}
	return s.Universe().MCPNames()
}

// Review judges the round.
//
// It is given the tool LOG as evidence and told the narration is not: shown
// only what the executor said, a reviewer grades prose. The log is the run's
// BRIDGED calls only (its harness's shell, edit and test work is in no log), so
// the reviewer is also handed the branches and pull requests the run reported
// delivering, and its rules say what the log cannot hold
// ([prompts.ReviewHeader]).
//
// A rule for some seats only is gated on the seat's surface: `a2a_ask`, named
// where the registry carries it.
func (r *Runner) Review(ctx context.Context, round int, w turn.Work, history []ledger.Iteration) (_ turn.Review, err error) {
	defer origin.Stamp(&err)
	snapshot := r.seatSnapshot()
	_, offersA2A := snapshot.Lookup(toolname.A2AAsk)
	submit := structured.New(SubmitReviewTool, submitReviewDescription, reviewSchema, decodeReview)
	surface, err := r.surfaceWith(ctx, r.ownMeter(), phase.Review, round, snapshot, submit, nil)
	if err != nil {
		return turn.Review{}, err
	}

	// VERBATIM. Review's evidence log takes no FormatOptions budget: those belong
	// to the cross-round ledger, and a reviewer judging an elided log judges a
	// summary.
	offer := r.cfg.Skills.Offer()
	system := prompts.BuildReview(r.cfg.Seat, prompts.ReviewInput{
		Intent:            w.Summary,
		Outcome:           string(w.Outcome),
		Rescued:           w.Rescued,
		Evidence:          w.Evidence,
		OpenQuestions:     w.OpenQuestions,
		Produced:          reviewArtifact(w),
		ToolLog:           reviewLog(w, r.cfg.SkipNames),
		RefusedReplies:    refusedReplies(w.RefusedReplies),
		Delivered:         prompts.DeliveredRefs(w.Refs, w.RefsElided),
		EarlierIterations: r.priorWork(ctx, history),
		OffersA2A:         offersA2A,
		// THE SKILLS AN OPERATOR SCOPED TO REVIEW (`phases: [review]`).
		Skills: offer.Catalogue(),
		// The servers the seat's registry holds, not the reviewer's own surface (its
		// submission alone): a review skill keys on what the work could reach.
		MCPServers: snapshot.MCPServers(),
	})
	r.emitter().skillsInjected(ctx, phase.Review, offer.Drain())
	user := reviewTask(r.cfg.Task)

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Review, surface: surface, system: system, user: user,
		rounds: reviewRounds, iteration: round,
		// NEVER FORCED, like every phase: a request cannot force a call, and several
		// current models answer a forced choice with a 400, fatal to the turn. The
		// submission named here is what makes a round of prose get asked again
		// (toolloop.Config.TerminateAfter).
		terminateAfter: []string{SubmitReviewTool},
		steerable:      true,
	})
	if err != nil {
		// Nothing to salvage and nothing lost: the reviewer's surface has its
		// submission tool and no catalogue, so its record proves no outward write.
		// What the EXECUTOR did is in the caller's hands ([turn.Run] reads it off the
		// Work).
		return turn.Review{}, err
	}

	payload, submitted := submit.Value()
	if !submitted {
		// A reviewer that never decided must not silently pass the turn: defaulting to
		// `done` makes "judged good" and "nothing judged it" identical downstream.
		// Loop back; the delivery gate and stall guard bound it.
		log.WarnContext(ctx, "review_never_submitted", "round", round, "rounds_used", res.Rounds)
		rescue := turn.Review{
			Decision: phase.SelfIterate,
			Notes: "The review phase produced no decision. Re-check what the " +
				"executor set out to do against what the tool log says it did, " +
				"and call " + SubmitReviewTool + ".",
		}
		r.emitter().completed(phaseCtx, reviewRecord(round, system, user, res,
			string(rescue.Decision), rescue.Notes, true, surface))
		return rescue, nil
	}
	r.emitter().completed(phaseCtx, reviewRecord(round, system, user, res,
		payload.Decision.String(), payload.Notes, false, surface))
	return turn.Review{
		Decision:      payload.Decision,
		Notes:         payload.Notes,
		CompletedWork: payload.CompletedWork,
		FinalArtifact: payload.FinalArtifact,
	}, nil
}

// reviewTask frames the trigger for the reviewer. THE SAME BYTES MEAN
// DIFFERENT THINGS TO THE TWO PHASES: the executor is HANDED this text as the
// thing to do, but a reviewer asked about a turn already underway reads it
// bare as a request just arrived: beside an `## Earlier rounds` block showing a
// clarifying question it concluded the sender asked again, and built its
// correction on a message nobody sent. So it reaches the reviewer as labelled
// evidence.
//
// The label is CONSTANT, unlike [Runner.taskFor]'s correction: this is re-sent
// every round, and a per-round frame would move bytes the prefix cache is keyed
// on. Two sections: the label, and the trigger whole (a vendor body's headings
// are the trigger's).
func reviewTask(task string) prompts.Prompt {
	b := prompts.NewBuilder("")
	b.Lead("reference", "For reference", "The trigger this turn is answering, for "+
		"reference. It is the message the agent was ALREADY working on when the "+
		"rounds below ran — not a new one that arrived during the turn, and not a "+
		"repeat of it:\n\n")
	b.Lead("task", "Task", task)
	return b.Build()
}

// reviewRecord builds Review's completed record: a function because Review
// reports from TWO places (its payload and its rescue), which must describe the
// same phase; written twice, the rescue is the one that loses a field.
func reviewRecord(round int, system, user prompts.Prompt, res phaseResult,
	decision, notes string, rescued bool, surface *tools.Surface,
) phaseRecord {
	return phaseRecord{
		Phase: phase.Review, Iteration: round, System: system, User: user,
		Result: res.Result, Exhausted: res.Exhausted, Elapsed: res.Elapsed,
		StartedAt: res.StartedAt, Caps: res.Caps,
		Decision: decision, Notes: notes, Rescued: rescued,
		Available: surface.Active(), Picks: res.Picks,
	}
}

// runPhase drives one engine-loop phase, extending it when the judge allows:
// the reviewer and the onboarding pass. Never the executor, which is its
// harness's own run.
//
// The extension loop lives here, not in the extension package, because only the
// caller knows what FINISHING looks like for its phase (the reviewer's
// submission, onboarding's mark_onboarded).
//
// phaseRun is one phase's inputs: a struct, since three adjacent ints (rounds,
// ceiling, iteration) and two strings make a signature where a transposition
// compiles and runs the phase with the wrong budget.
type phaseRun struct {
	phase   phase.Phase
	surface *tools.Surface

	// system and user open the conversation.
	system prompts.Prompt
	user   prompts.Prompt

	rounds    int
	ceiling   int
	iteration int

	// terminateAfter names tools that end the loop once they have run
	// SUCCESSFULLY, and by naming them declares the phase finishes by one: a round
	// ending in prose is re-prompted with a corrective naming them.
	//
	// That is what makes a phase end in its submission: a request cannot force a
	// call ([llm.Request.Tools]), and a forced choice is ignored by some endpoints
	// and refused with a 400 by several current models, which is not retried down
	// the chain and failed every review and onboarding pass before the corrective
	// could run.
	terminateAfter []string

	// steerable is whether a person's notes are drained into this phase's
	// rounds — the reviewer, and nothing else. See [Config.Steer].
	steerable bool
}

// runPhase returns the PHASE'S CONTEXT as well as its result: the caller
// publishes agent_phase_completed under this phase's span, not the turn's, so
// the dashboard's trace tree is trigger -> turn -> {execute, review}. The span
// has ENDED by then, which is fine: a span id is a label, not a lifetime.
//
// Returned under its own name, not shadowing the caller's ctx: work the caller
// does AFTER the phase must not be attributed to a closed span.
func (r *Runner) runPhase(ctx context.Context, in phaseRun) (_ context.Context, _ phaseResult, err error) {
	defer origin.Stamp(&err)
	ph, surface := in.phase, in.surface
	// ONE SPAN PER PHASE, wrapping the WHOLE extension loop, not each
	// toolloop.Run: an extended phase is one phase that ran longer.
	//
	// The attributes are thin: everything about what a phase DID is on
	// agent_phase_completed, and duplicating it would send whole prompts to a
	// collector that is not the event store. What no event records is DURATION.
	ctx, span := tracing.Start(ctx, "agent.runner", "agent.turn."+string(ph),
		attribute.String("crewlet.phase", string(ph)),
		attribute.Int("crewlet.iteration", in.iteration))
	defer span.End()
	// THE SAME BRACKET AS THE SPAN: the span and agent_phase_completed disagreeing
	// about one phase's duration is worse than either absent. Monotonic, so a clock
	// correction cannot report a negative duration.
	began := time.Now()
	segment := began.UTC()
	system, user := in.system, in.user
	iteration, ceiling := in.iteration, in.ceiling
	terminateAfter := in.terminateAfter
	// The phase's own record of what it managed, so the FAILURE path has something
	// to report: a phase that dies returns no Result, leaving a dashboard with an
	// in-flight call and no reason.
	emit := r.emitter()
	progress := &toolloop.Progress{}
	// The PHASE's record, accumulated across every tool-loop invocation; the
	// failure path reports it too.
	var out phaseResult
	// The extension policy: every record this phase publishes states its ceiling.
	policy := extension.Policy{
		Enabled: r.cfg.Caps.ExtensionOn, RoundStep: r.cfg.Caps.ExtensionStep, Ceiling: ceiling,
	}
	// The phase's own wall clock, stamped on the way out. A closure, not a line per
	// `return`: a measurement written at one of the two success returns and not the
	// other leaves only the EXTENDED phases without a duration.
	done := func() (context.Context, phaseResult, error) {
		out.Elapsed = time.Since(began)
		out.StartedAt = segment
		out.Caps = capsOf(policy, out.Result.MaxRounds)
		return ctx, out, nil
	}
	// Returns the phase context too, so `return fail(err)` stays one line.
	fail := func(err error) (context.Context, phaseResult, error) {
		// The span carries the failure too: the event records WHAT broke, the span
		// makes it findable in a trace.
		tracing.Fail(span, err)
		emit.completed(ctx, phaseRecord{
			Phase: ph, Iteration: iteration, System: system, User: user, Picks: out.Picks,
			// FOLDED onto what the phase already had, as the success path folds it.
			// `progress` is reused across invocations and `record` REPLACES, so it holds
			// only the invocation that died: a phase that ran twenty rounds, was extended
			// and failed on extension round 2 published a record of two rounds with only
			// their tokens. `out` is read here, not captured: the closure sees what the
			// loop has accumulated when it fails.
			Result: foldOnto(out, progress.Snapshot()), Available: surface.Active(),
			// A PHASE THAT DIED STILL TOOK TIME, and on a timeout it says so: a provider
			// hanging four minutes and one refusing in fifty milliseconds are otherwise the
			// same record.
			Elapsed:   time.Since(began),
			StartedAt: segment,
			Caps:      capsOf(policy, maxRounds(out, in.rounds)),
			// A PHASE A PERSON STOPPED DID NOT FAIL: it carries the stop as its error but
			// is not marked failed, since a turn is listed failed when any row is.
			Failed: !turn.Stopped(err), Err: err,
		})
		return ctx, phaseResult{}, err
	}

	// THE WALK, not the read: on an LLM fleet the balancer is asked here, at
	// the start of the phase's work, and its picks are taken as members take
	// the calls.
	walk, err := r.cfg.Models.Walk(r.cfg.Seat.Role, ph)
	if err != nil {
		return fail(err)
	}
	members := walk.Members
	out.Picks = walk.Picks
	// A chain even for one member: the wrapper is a pass-through there, and
	// uniform behaviour (failing, logging, reporting) is worth the allocation.
	//
	// OnFallback is wired HERE as the innermost frame that knows the turn and
	// iteration; it fires synchronously inside the chain's loop before the next
	// member is tried, so `ctx` and `iteration` are the failing call's.
	provider, err := chain.New(members, chain.Options{
		OnFallback: func(f chain.Fallback) { emit.fallback(ctx, ph, iteration, f) },
		Fleet:      walk.Picks,
		Begin:      func(key string) func() { return r.cfg.Models.Begin(r.cfg.Seat.Role, ph, key) },
	})
	if err != nil {
		return fail(fmt.Errorf("runner: %s: %w", ph, err))
	}
	if len(walk.Picks) > 0 {
		emit.fleetPicked(ctx, ph, walk.Picks[0])
	}

	// Published BEFORE the first provider call, so a thinking seat says which phase
	// it is in; the completed event is the durable record and may be minutes away.
	//
	// THE SURFACE TRAVELS WITH THE TEXT: every request carries the tool
	// definitions, which the harness lists from the phase's session and the vendor
	// bills as input.
	emit.started(ctx, ph, iteration, system, user, surface,
		capsOf(policy, out.Rounds+in.rounds))

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: system.Text},
		{Role: llm.RoleUser, Content: user.Text},
	}
	// EVERY NOTE THE TURN HAS ALREADY READ, after the opening, in read order, so a
	// new phase starts from the turn as corrected ([Runner.steered]).
	for _, note := range r.carriedSteers() {
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: note})
	}
	budget := in.rounds
	// The finishing correctives the run of declines being CONTINUED has already
	// had: nonzero only for an invocation picking up a corrective the last one
	// withheld (toolloop.Config.CorrectivesSpent).
	correctivesSpent := 0
	// Rounds run past an invocation's budget WITHOUT the judge to read a withheld
	// corrective: a phase's own allowance of finishing correctives and no more,
	// else a phase working and declining by turns extends itself on nobody's
	// decision.
	unjudged := 0
	// WHAT THE PHASE'S FRAMES HAVE REDACTED, across its invocations, which fold
	// onto one another: every frame shows the phase's words again.
	words := new(liveWords)
	for {
		// What the phase holds BEFORE this invocation, by value: the loop calls
		// OnProgress from inside Run, and `out` is written only after it returns.
		//
		// The loop numbers rounds per INVOCATION from 1, so every publish is folded
		// onto what came before, and BOTH halves of the fold matter. Without the
		// offset the second invocation restarts at 1 and consumers ordering on the
		// round number see the phase run backwards (the live projection's stale-round
		// guard dropped the extension; the ledger merged extension round 1 into round
		// 1). Without the prefix the live view, which rebuilds a call per frame,
		// collapsed a twenty-round ledger to one round when the extension's first
		// frame landed.
		prior := out
		// The rounds already behind this invocation, so a tool asking which round it is
		// in hears the PHASE's number, as the records are renumbered onto.
		loopCtx := toolloop.WithRoundOffset(ctx, prior.Rounds)
		var steering toolloop.Steerer
		if in.steerable && r.cfg.Steer != nil {
			steering = phaseSteer{r: r, ctx: ctx, ph: ph, iteration: iteration, offset: prior.Rounds}
		}
		res, err := toolloop.Run(loopCtx, toolloop.Config{
			Provider: provider, Messages: messages, Surface: surface,
			Host: r.cfg.Sessions, ServerTitle: sessionTitle(r.cfg.Seat, ph),
			// The chain's head, standing in until a completion names the serving member
			// (toolloop.Config).
			ProviderKey: members[0].Key,
			MaxRounds:   budget, Budget: r.cfg.Budget,
			Fence:            r.cfg.Fence,
			CorrectivesSpent: correctivesSpent,
			// A phase that has SUBMITTED is finished: without this the loop asks again and
			// the phase spends its round budget re-deciding.
			TerminateAfter: terminateAfter,
			// Progress feeds the failure path; OnProgress the live view: one is read after
			// the loop returns nothing, the other while it runs.
			Progress: progress,
			// The live view is the only reason to stream.
			StreamPartials: true,
			// A person's notes, into the reviewer's rounds only.
			Steer: steering,
			OnProgress: func(live toolloop.Result) {
				folded := foldOnto(prior, live)
				emit.progress(ctx, ph, iteration, words, folded, capsOf(policy, folded.MaxRounds))
			},
		})
		if err != nil {
			return fail(fmt.Errorf("runner: %s: %w", ph, err))
		}
		out.Text = res.Text
		out.Exhausted = res.ExhaustedRounds
		// The same fold the live view got, so the record and the frames describe one
		// phase.
		out.Result = foldOnto(prior, *res)
		out.Rounds = prior.Rounds + res.RoundsUsed
		messages = res.Messages

		// A CORRECTIVE THE LOOP WITHHELD IS SENT HERE, when the phase may run past
		// this invocation's budget. Its last round ended without the submission and had
		// no round left to be asked in (the round a phase most naturally submits on),
		// so the outcome turned on WHICH round a decline landed on: the same fenced
		// JSON was re-asked and submitted a round before the cap, but on the cap was
		// rescued as incomplete with the ceiling's rounds unspent.
		//
		// No judge: its question is whether a phase still working deserves more
		// rounds, and this one has finished and lacks only the reporting call. Bounded
		// instead by the allowance the loop would have spent inside the budget, shared
		// with it (CorrectivesSpent), and the phase's ceiling, which an operator who
		// turned extensions off sets to the budget.
		if res.Withheld != "" && policy.Enabled {
			room := min(toolloop.MaxFinishingCorrectives-res.CorrectivesSpent,
				toolloop.MaxFinishingCorrectives-unjudged, policy.Headroom(out.Rounds))
			if room > 0 {
				log.InfoContext(ctx, "phase_asked_for_submission_past_budget", "phase", ph,
					"iteration", iteration, "rounds_used", out.Rounds, "granted", room)
				messages = append(messages, llm.Message{Role: llm.RoleUser, Content: res.Withheld})
				budget, unjudged = room, unjudged+room
				correctivesSpent = res.CorrectivesSpent + 1
				continue
			}
		}

		if !res.ExhaustedRounds {
			// Read off the phase TOTALS, never `res`: an extended phase ran the loop more
			// than once.
			span.SetAttributes(attribute.Int("crewlet.rounds", out.Rounds))
			return done()
		}
		fence := surface.Turn().FenceBoundary()
		granted, decision := r.consider(ctx, ph, iteration, out.Rounds, policy, extension.Request{
			Phase: ph, Task: r.cfg.Task,
			Calls: calls(surface), LastText: res.Text, RoundsUsed: out.Rounds,
			Boundary: fence,
		})
		if granted <= 0 {
			log.InfoContext(ctx, "phase_not_extended", "phase", ph, "iteration", iteration,
				"rounds_used", out.Rounds, "reason", decision.Reason)
			return done()
		}
		log.InfoContext(ctx, "phase_extended", "phase", ph, "iteration", iteration,
			"granted", granted, "rounds_used", out.Rounds, "reason", decision.Reason)
		messages = append(messages, llm.Message{
			Role:    llm.RoleUser,
			Content: extension.Nudge(fence, ph, granted, decision.Reason),
		})
		// An exhausted phase was still calling tools: no run of declines is being
		// continued.
		budget, correctivesSpent = granted, 0
	}
}

// offsetRounds renumbers one loop invocation's rounds onto the phase's own
// scale, so an extension continues the count.
//
// Copies rather than mutates: the caller's Result is the loop's live snapshot,
// published from another goroutine.
func offsetRounds(res toolloop.Result, prior int) toolloop.Result {
	if prior <= 0 {
		return res
	}
	res.RoundsUsed += prior
	execs := make([]toolloop.Execution, len(res.Executions))
	for i, ex := range res.Executions {
		ex.Round += prior
		execs[i] = ex
	}
	res.Executions = execs
	narration := make([]toolloop.Narration, len(res.Narration))
	for i, n := range res.Narration {
		n.Round += prior
		narration[i] = n
	}
	res.Narration = narration
	timed := make([]toolloop.Round, len(res.Rounds))
	for i, r := range res.Rounds {
		r.Round += prior
		timed[i] = r
	}
	res.Rounds = timed
	// A note's round too, or one read in extension round 1 is drawn beside the
	// phase's first.
	steers := make([]toolloop.SteerMark, len(res.Steers))
	for i, m := range res.Steers {
		m.Round += prior
		steers[i] = m
	}
	res.Steers = steers
	// A harness run's rounds too; a run that made no request names none (zero),
	// which stays zero.
	invocations := make([]toolloop.Invocation, len(res.Invocations))
	for i, inv := range res.Invocations {
		if inv.FirstRound > 0 {
			inv.FirstRound += prior
			inv.LastRound += prior
		}
		invocations[i] = inv
	}
	res.Invocations = invocations
	// The calls in flight are on the same scale.
	running := make([]toolloop.RunningCall, len(res.Running))
	for i, c := range res.Running {
		c.Round += prior
		running[i] = c
	}
	res.Running = running
	// The round IN FLIGHT is on the same scale, or it COLLIDES with a committed
	// one: a consumer keys the ledger on the round number (the dashboard's
	// `rounds()` builds one block per number), so an unshifted partial from
	// extension round 1 would overwrite the phase's committed round 1 while the
	// running round got no block. The abandoned attempts carry the same number.
	if res.Partial != nil {
		shifted := *res.Partial
		shifted.Round += prior
		abandoned := make([]toolloop.Narration, len(res.Partial.Abandoned))
		for i, a := range res.Partial.Abandoned {
			a.Round += prior
			abandoned[i] = a
		}
		shifted.Abandoned = abandoned
		res.Partial = &shifted
	}
	return res
}

// consider asks the round-cap judge, and makes the call visible.
//
// The judge is a model call outside the tool loop, where every other call is
// metered, so it is wrapped here, not in [extension.Consider] (policy plus a
// model, knowing nothing of spans, meters or events).
//
// A CHARGE THAT REFUSES DOES NOT FAIL THE PHASE: an extension is a generosity on
// a phase already out of rounds, so an over-budget judgement is logged,
// published and treated as "no extension". Its tokens are on the counter
// anyway ([toolloop.BudgetMeter]), so the next phase's first call is refused
// before it is made. AND NO JUDGE IS CALLED ON A REFUSAL ALREADY CERTAIN (a
// delegated worker refused inside the last round): it would be billed then refused.
func (r *Runner) consider(ctx context.Context, ph phase.Phase, iteration, hostRound int,
	policy extension.Policy, req extension.Request,
) (int, extension.Decision) {
	// One span per judge call, nested under the phase: the only place a reader sees
	// what the judgement COST IN TIME, which no event records, and a judge on a
	// slow cheap model is a stall mid-phase.
	ctx, span := tracing.Start(ctx, "agent.runner", "agent.turn.judge",
		attribute.String("crewlet.phase", string(ph)),
		attribute.Int("crewlet.iteration", iteration),
		attribute.Int("crewlet.rounds", req.RoundsUsed))
	defer span.End()

	// A REFUSAL ALREADY CERTAIN calls no judge, only where the judge WOULD be
	// called: a policy declining to ask, or a seat with no judge, costs nothing and
	// its own reason is the truer account.
	if asks, _ := policy.ShouldAsk(req.RoundsUsed); asks && r.cfg.Judge != nil {
		if refused := toolloop.Refusal(ctx, r.cfg.Budget); refused != nil {
			log.WarnContext(ctx, "extension_judge_over_budget", "phase", ph,
				"iteration", iteration, "tokens", 0, "error", refused.Error(),
				"detail", "the budget had already refused this turn, so the judge was not called")
			span.SetAttributes(attribute.Bool("crewlet.judge_called", false))
			return 0, extension.Rescue(extension.ReasonBudgetExhausted)
		}
	}

	// THE JUDGE'S OWN WALL CLOCK, bracketed here for the span's reason: it
	// separates a judge stalling a phase from one that simply said no, otherwise
	// the same record.
	began := time.Now()
	granted, decision := extension.Consider(ctx, r.cfg.Judge, policy, req)
	judged := time.Since(began)
	if !decision.Asked {
		// The policy declined to ask, or there is no judge: nothing was called to
		// report or charge, and an event would claim a call that never happened.
		span.SetAttributes(attribute.Bool("crewlet.judge_called", false))
		return granted, decision
	}
	span.SetAttributes(
		attribute.Bool("crewlet.judge_called", true),
		attribute.String("crewlet.model", decision.Model),
		attribute.Bool("crewlet.extended", granted > 0),
		attribute.Int("crewlet.granted", granted),
		attribute.Int("crewlet.total_tokens", decision.Tokens()))

	if err := charge(ctx, r.cfg.Budget, decision.Tokens()); err != nil {
		log.WarnContext(ctx, "extension_judge_over_budget", "phase", ph,
			"iteration", iteration, "tokens", decision.Tokens(), "error", err.Error())
		granted = 0
	}
	r.emitter().judged(ctx, ph, iteration, hostRound, granted, decision, began, judged)
	return granted, decision
}

// charge meters a model call the tool loop did not make, once made: the meter
// records it whether or not it fits.
//
// An unreachable counter is NOT a refusal (the loop's own three-valued rule),
// but here both end the same way: the caller declines to extend and says why.
func charge(ctx context.Context, meter toolloop.BudgetMeter, tokens int) error {
	if meter == nil || tokens <= 0 {
		return nil
	}
	outcome, err := meter.Spend(ctx, tokens)
	if err != nil {
		return fmt.Errorf("charging the extension judge: %w", err)
	}
	if outcome.OK {
		return nil
	}
	scope := outcome.Scope
	if scope == "" {
		scope = "org"
	}
	return fmt.Errorf("the %s budget refused %d tokens (%d of %d used)",
		scope, tokens, outcome.Used, outcome.Limit)
}

// maxRounds is the cap a phase is under when it has no invocation of its own to
// read it from — the failure path, which can die before the loop echoed one.
func maxRounds(out phaseResult, base int) int {
	if out.Result.MaxRounds > 0 {
		return out.Result.MaxRounds
	}
	return out.Rounds + base
}

// foldOnto merges one loop invocation's record onto the rounds already behind
// it, producing the record of the PHASE.
//
// One function for the live frame, the completed record and the failure record,
// which must agree on what an extended phase is. Anything a loop invocation
// counts from zero is folded here; anything that carries across (the
// conversation, so `Text`) is taken from the invocation as it stands.
func foldOnto(done phaseResult, live toolloop.Result) toolloop.Result {
	res := offsetRounds(live, done.Rounds)
	res.Executions = append(
		append([]toolloop.Execution(nil), done.Result.Executions...), res.Executions...)
	res.Narration = append(
		append([]toolloop.Narration(nil), done.Result.Narration...), res.Narration...)
	res.Rounds = append(
		append([]toolloop.Round(nil), done.Result.Rounds...), res.Rounds...)
	res.Steers = append(
		append([]toolloop.SteerMark(nil), done.Result.Steers...), res.Steers...)
	// Every harness run of the phase, the extension's after the original's.
	res.Invocations = append(
		append([]toolloop.Invocation(nil), done.Result.Invocations...), res.Invocations...)
	res.RoundsUsed = done.Rounds + live.RoundsUsed
	// The cap the PHASE is under: every round behind it plus what this invocation
	// was granted.
	res.MaxRounds = done.Rounds + live.MaxRounds
	res.InputTokens += done.Result.InputTokens
	res.OutputTokens += done.Result.OutputTokens
	res.CacheRead += done.Result.CacheRead
	res.CacheWrite += done.Result.CacheWrite
	// Accumulated like the token counts: an EXTENDED phase's second invocation
	// counts only its own rounds.
	res.EmptyAnswers += done.Result.EmptyAnswers
	// The model that served the phase, not the round that died: an invocation
	// failing before its first completion names nobody, reading as a phase that
	// never reached a provider.
	if res.Model == "" {
		res.Model = done.Result.Model
	}
	// The entry, by the same rule and for the same reason.
	if res.ProviderKey == "" {
		res.ProviderKey = done.Result.ProviderKey
	}
	return res
}

type phaseResult struct {
	Text      string
	Rounds    int
	Exhausted bool

	// Elapsed is the phase's own wall clock, around the whole extension loop, not
	// one invocation: an extended phase is one phase that ran longer, as its span is.
	//
	// Measured here because this is the only frame that brackets the phase;
	// callers assemble the record AFTER decoding a payload, and a clock read there
	// would fold the decode in differently on every path.
	Elapsed time.Duration

	// StartedAt is when this segment of the phase began and Caps the round
	// allowance it ended under, stamped by the bracketing frame as Elapsed is.
	StartedAt time.Time
	Caps      roundCaps

	// Result is the loop's own outcome, kept whole so the phase reports what it
	// spent and called. The fields above are what the loop's CALLER acts on;
	// re-deriving one from the other is how the two come to disagree.
	Result toolloop.Result

	// RunSteers is what a collected executor read of the turn's notes, in its brief
	// and at its harness loop's boundaries, at the run's one round with each
	// boundary. An engine-loop run's reads are at its rounds in its Result; an
	// executor's are on its record ([Runner.resumeAgentRun]).
	RunSteers []types.PhaseSteer

	// RunElided is the calls a collected executor's run made that its
	// record did not keep, by identity ([Resume.Elided]): said on the
	// phase's record by tool, since the calls it lists are only the kept
	// ones ([types.AgentPhaseCompleted.ElidedCalls]).
	RunElided []ledger.Elided

	// Picks are the LLM fleet's picks of the chain the phase ran on, nil on
	// a pinned one ([phaseRecord.Picks]).
	Picks []*fleet.Pick
}

// surfaceWith builds a phase surface, registering the phase's submission tool
// on a PRIVATE copy of the snapshot: the tool is per-phase state, and the shared
// registry would show one phase's submission to the next.
//
// The `extra` set is per-TURN: today the sub-agent spawner, bound to one parent
// turn's grant, which must reach the surface a run is collected against as well
// as the one it launched with. Injected in this single funnel, so a phase losing
// the tool mid-turn is not representable.
//
// m is where the surface's own calls are charged: the runner's own segment for
// every surface but the executor's ([meter]).
func (r *Runner) surfaceWith(ctx context.Context, m meter, ph phase.Phase, round int,
	snapshot tools.Snapshot, submit tools.Callable, active []string, loaded ...string,
) (_ *tools.Surface, err error) {
	defer origin.Stamp(&err)
	// THE EXECUTOR'S OTHER WAY TO END: parking on a person's answer. Here, in the
	// funnel the launch and the collection both take, so a rebuilt surface holds
	// the tool its run called. The run's submission is held to it: a job that
	// asked is parked and takes none ([RunAsk]).
	asks := ph == phase.Execute && r.cfg.Ask != nil
	if asks && submit != nil {
		submit = r.cfg.Ask.Gate(submit)
	}
	if submit != nil {
		var err error
		snapshot, err = snapshot.With(tools.Entry{Tool: submit, Origin: tools.OriginBuiltin})
		if err != nil {
			return nil, fmt.Errorf("runner: %s: %w", ph, err)
		}
		active = append([]string{submit.Name()}, active...)
	}
	if asks {
		var err error
		snapshot, err = snapshot.With(tools.Entry{Tool: r.cfg.Ask, Origin: tools.OriginBuiltin})
		if err != nil {
			return nil, fmt.Errorf("runner: %s: %w", ph, err)
		}
		active = append(active, r.cfg.Ask.Name())
	}
	// AND THE TOOLS FOR THE RUNS AROUND IT, on every executor surface's
	// snapshot so a rebuilt one holds what its run called; ACTIVE only where the
	// launch found runs around it ([Runner.runsAround]), which the run's state
	// keeps for every rebuild.
	if ph == phase.Execute && r.cfg.Runs != nil {
		for _, tool := range r.cfg.Runs.Tools() {
			if _, held := snapshot.Lookup(tool.Name()); held {
				continue
			}
			var err error
			snapshot, err = snapshot.With(tools.Entry{Tool: tool, Origin: tools.OriginBuiltin})
			if err != nil {
				return nil, fmt.Errorf("runner: %s: %w", ph, err)
			}
		}
	}
	// AND ITS CHECKOUT, in the same funnel: a collection's rebuilt surface holds
	// the tool its run called.
	if ph == phase.Execute && r.cfg.Checkout != nil {
		var err error
		snapshot, err = snapshot.With(tools.Entry{Tool: r.cfg.Checkout, Origin: tools.OriginBuiltin})
		if err != nil {
			return nil, fmt.Errorf("runner: %s: %w", ph, err)
		}
		active = append(active, r.cfg.Checkout.Name())
	}

	// The discovery pair reaches its surface through a closure, a real cycle:
	// activate must mutate the Surface the loop reads definitions from, so the
	// tools cannot exist before it, and it cannot resolve them until they are in
	// its snapshot. The closure is read at call time.
	var surface *tools.Surface
	if discovers(ph) {
		// AS THE ENTRIES DiscoveryTools BUILDS, hints and all: rewrapped, the pair
		// would reach the surface unannotated and every reader of the hints (a
		// worker's guard) would refuse discovery as an unclassifiable write.
		for _, entry := range DiscoveryTools(func() *tools.Surface { return surface }) {
			var err error
			snapshot, err = snapshot.With(entry)
			if err != nil {
				return nil, fmt.Errorf("runner: %s: %w", ph, err)
			}
			active = append(active, entry.Name())
		}
	}

	// The sub-agent spawner, on the same closure: the child inherits the parent's
	// LIVE active list, which does not exist until this surface does. Built HERE,
	// the single funnel, so launch and collection get it from one place and a
	// collection losing the tool mid-run is not representable.
	if entry := r.spawnEntry(ctx, m, ph, round, snapshot,
		func() *tools.Surface { return surface }); entry.Tool != nil {
		next, err := snapshot.With(entry)
		if err != nil {
			// A collision here is an operator's MCP server publishing a first-party tool
			// name. LOSING THE SPAWNER is the proportionate answer: failing the surface
			// would kill every Execute phase on that seat while the server is configured.
			// The subagent package makes the same choice one level down.
			log.WarnContext(ctx, "spawn_tool_skipped", "phase", ph,
				"tool", entry.Tool.Name(), "error", err.Error())
		} else {
			snapshot = next
			active = append(active, entry.Tool.Name())
		}
	}
	// Bound to the turn, so a seat-scoped tool knows who is calling without the
	// seat travelling through the model's arguments, AS THIS PHASE so a tool can
	// say which leg did it ([turnctx.Turn.InPhase]).
	surface = tools.NewSurface(ph.String(), snapshot, active).
		ForTurn(m.turn.InPhase(types.Phase(ph)))
	if hold := r.holderFor(ph); hold != nil {
		surface = surface.WithHolder(hold)
	}
	if r.cfg.Metrics != nil {
		surface = surface.WithCounter(r.cfg.Metrics)
	}
	// THE GUARD IS BUILT FROM THE FINISHED SURFACE, so what it enforces and what
	// the catalogue showed cannot disagree: both match skills against the universe
	// (catalogueSurface), and the catalogue's "required" marker is the promise this
	// keeps.
	guard := r.guardFor(ph, round, surface, loaded)
	// RECORDED ONLY HERE, which keeps it the TURN's guard: a sub-agent's child
	// surface is built elsewhere, and recording inside guardFor would let a child
	// spawned mid-run overwrite the executor's loaded set, so a park would persist
	// the worker's.
	r.mu.Lock()
	r.guard = guard.skills()
	r.mu.Unlock()
	return surface.WithGuard(guard.tools()), nil
}

// holderFor is what records a call to a Needs-approval tool on ph's surface:
// the executor's alone, whose run has a row the call parks on (ADR-0077). Every
// other phase runs on the engine host and its surface refuses such a call
// ([tools.Holder]).
func (r *Runner) holderFor(ph phase.Phase) tools.Holder {
	if ph != phase.Execute {
		return nil
	}
	return r.cfg.Hold
}

// discovers reports whether a phase is offered the discovery pair,
// list_mcp_server_tools and activate_tool.
//
// Onboarding is: its prompt lists the tools it can still discover and tells the
// model how. The executor is not: it holds every tool from launch, each MCP
// server under its own name, and its harness defers them by its own mechanism
// (Claude Code's tool search, OpenCode's Code Mode; internal/api/mcpbridge,
// servers.go). The reviewer must not be: activate_tool reaches every tool in the
// snapshot, MCP writes included, so a reviewer offered the pair could post to a
// thread or edit a ticket while grading whether the executor should have.
func discovers(ph phase.Phase) bool {
	return ph == phase.Onboarding
}

// armedGuard is one built gate: the concrete guard for the runner's
// bookkeeping and the interface value the surface installs.
//
// Two views because A TYPED NIL WOULD NOT BE NIL: a *skills.Guard returned as a
// tools.Guard is a non-nil interface wrapping nil, so every tool call would take
// the guard path to be told yes.
type armedGuard struct {
	guard *skills.Guard
	wrap  tools.Guard
}

func (a armedGuard) skills() *skills.Guard { return a.guard }
func (a armedGuard) tools() tools.Guard    { return a.wrap }

// guardFor arms the required-skill gate for one phase session, or nil.
//
// PER SESSION: "loaded" means "the body is in this model's context", and the
// executor, reviewer and each sub-agent are separate histories. A round-cap
// extension continues the same session AND surface, so a load carries across; a
// self_iterate launches a fresh run and so a fresh guard, as its context started
// over too.
//
// The loaded keys are a PARKED RUN'S seed: what its guard held at launch, which
// every later segment's rebuild restores before replaying what the run loaded
// over the bridge since ([Runner.rebuiltSurface]); rebuilt empty, the guard
// would refuse calls that session had unlocked. Empty for every other phase.
func (r *Runner) guardFor(ph phase.Phase, iteration int, surface *tools.Surface, loaded []string) armedGuard {
	// THE LOADER MUST BE ACTIVE, not merely in the universe the triggers match
	// over: a guard is armed only where the session can satisfy it.
	if r.cfg.Skills == nil || !slices.Contains(surface.Active(), toolname.LoadToolSkill) {
		return armedGuard{}
	}
	guard := skills.NewGuard(r.cfg.Skills, promptPhase(ph), catalogueSurface(surface))
	if guard == nil {
		return armedGuard{}
	}
	guard.Restore(loaded)
	return armedGuard{
		guard: guard,
		wrap:  &reportingGuard{guard: guard, emit: r.emitter(), phase: ph, iteration: iteration},
	}
}

// promptPhase maps a runner phase onto the prompt package's own: explicit, not a
// string cast, since a phase added here and unmapped would become the zero
// value, matching no skill and arming no guard.
func promptPhase(ph phase.Phase) prompts.Phase {
	switch ph {
	case phase.Execute:
		return prompts.PhaseExecute
	case phase.Review:
		return prompts.PhaseReview
	case phase.Subagent:
		return prompts.PhaseSubagent
	default:
		// Onboarding and anything later: not a phase skills are scoped to; the empty
		// value matches none, the honest answer.
		return ""
	}
}

// catalogueSurface is the tool surface as skill triggers see it: every tool in
// the surface's UNIVERSE (held and discoverable), never its active list alone.
// A surface activates anything in its universe by name and an executor starts
// with no MCP tool active, so a guard armed over the active list let an
// activated MCP tool through without its required skill.
func catalogueSurface(surface *tools.Surface) prompts.Surface {
	universe := surface.Universe()
	return prompts.Surface{Tools: universe.Names(), MCPServers: universe.MCPServers()}
}

// seatSnapshot is the registry as THIS SEAT is offered it: every surface of the
// turn is built from it, so a withheld tool ([Config.Withheld]) is on none: not
// offered, not discoverable, not activatable.
func (r *Runner) seatSnapshot() tools.Snapshot {
	return r.cfg.Registry.Snapshot().Without(r.cfg.Withheld...)
}

// executorUniverse is the snapshot an executor's surface is built from: the
// registry less the phase-scoped tools it must never reach.
//
// THE UNIVERSE, NOT ONLY THE ACTIVE LIST: it is what skill triggers match over,
// a worker's grant is cut from and the delivery check names tools from, so a
// tool off the active list alone was still in front of the executor
// (mark_onboarded, which phaseScoped exists to keep away).
func (r *Runner) executorUniverse(snapshot tools.Snapshot) tools.Snapshot {
	return snapshot.Without(phaseScoped...)
}

// executorActive is what the executor is offered: every tool in its universe
// ([Runner.executorUniverse]) but the tools of a server that runs in its box
// (inBox, [Runner.boxServers]), which its harness reaches there under the same
// name rather than over the bridge.
//
// THE WHOLE SURFACE, not a slice chosen in advance: that was the planner's
// choice, against a catalogue it never saw, and every wrong guess left the
// actor without a tool. The harness now keeps a hundred vendor definitions out
// of each request itself, a server at a time (internal/api/mcpbridge,
// servers.go).
func executorActive(universe tools.Snapshot, inBox []string) []string {
	var out []string
	for _, e := range universe.Entries() {
		if server, fromMCP := e.FromMCP(); fromMCP && slices.Contains(inBox, server) {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// boxServers names the seat's MCP servers that run in its executor's box
// (role.sandbox.mcp.servers), none of whose tools the bridge offers: a run never
// holds one server twice, once in its box and once over the bridge, under the
// same name.
func (r *Runner) boxServers() []string {
	if role := r.cfg.Seat.Role; role != nil && role.Sandbox != nil {
		return role.Sandbox.MCP.Servers
	}
	return nil
}

// phaseScoped names the first-party tools that belong to ONE phase and must not
// be offered by the others.
//
// mark_onboarded is the whole list: onboarding is its own pass, and a seat that
// could mark itself from inside the executor would permanently skip
// orientation, since the marker suppresses the pass for ever after.
//
// Everything else stays available on purpose: lookup_colleague, use_skill,
// query_episodes and refresh_memory are recon the executor does itself, and
// reflect_and_persist lets an agent keep what it learns mid-turn.
var phaseScoped = []string{toolname.MarkOnboarded}

// taskFor prefixes the reviewer's correction to the ask: PREFIXED to the brief,
// not the task description, which also feeds knowledge search and the episode
// record, both wanting the requester's actual ask.
func (r *Runner) taskFor(notes string) string {
	if strings.TrimSpace(notes) == "" {
		return r.cfg.Task
	}
	return r.cfg.Task + "\n\nThe previous round was reviewed. What to do differently:\n" + notes
}

// reviewLog is the round's calls as its reviewer reads them, verbatim
// ([ledger.FormatCalls] with no budget), and then what the run's record did
// not keep of them, said rather than hidden ([ledger.FormatElided]): a
// reviewer handed a long run's record with its middle silently gone judges an
// elision and calls it the run. A value the record left out reads as left out
// in the line it stands in, and a late call's line says an earlier run made
// it ([lateNotes]).
func reviewLog(w turn.Work, skip []string) string {
	text := ledger.FormatCalls(w.Calls, ledger.FormatOptions{Skip: skip, Notes: lateNotes(w.Calls)})
	if elided := ledger.FormatElided(w.Elided, skip); elided != "" {
		text += "\n" + elided
	}
	return text
}

// lateNotes ends each late call's line ([ledger.Call.Late]) with
// [prompts.ReviewLateCall], BY ITS PLACE in calls, as [ledger.FormatOptions.Notes]
// reads them, so a skipped name before it moves the words onto no other line;
// nil where none is late. Only the reviewer's reading tells the round's own
// calls apart: delivery checks and citations count a late call as the turn's.
func lateNotes(calls []ledger.Call) [][]string {
	var notes []string
	for i, call := range calls {
		if !call.Late {
			continue
		}
		if notes == nil {
			notes = make([]string, len(calls))
		}
		notes[i] = prompts.ReviewLateCall
	}
	if notes == nil {
		return nil
	}
	return [][]string{notes}
}

// refusedReplies is the round's refused replies as the review prompt renders
// them ([prompts.RefusedReplies]).
func refusedReplies(replies []turn.RefusedReply) string {
	out := make([]prompts.RefusedReply, 0, len(replies))
	for _, r := range replies {
		out = append(out, prompts.RefusedReply{Messages: r.Messages, Bytes: r.Bytes, MaxBytes: r.MaxBytes})
	}
	return prompts.RefusedReplies(out)
}

// calls converts a surface's record into ledger calls.
func calls(s *tools.Surface) []ledger.Call {
	recorded := s.Calls()
	out := make([]ledger.Call, 0, len(recorded))
	for _, c := range recorded {
		out = append(out, ledger.Call{Name: c.Name, Args: c.Args, Result: c.Output, Failed: c.Failed})
	}
	return out
}

// describe renders the surface the delivery gate judges against.
func describe(s *tools.Surface) turn.Surface {
	u := s.Universe()
	return turn.Surface{
		Catalogue:      u.Names(),
		Deliveries:     u.Deliveries(),
		KnownReads:     u.KnownReads(),
		KnownOpenWorld: u.KnownOpenWorld(),
		PresumedWrites: u.PresumedWrites(),
		// Every MCP-served tool, on a phase that reaches one only by discovering it, so
		// a refusal can tell held tools from ones to find ([turn.Surface.Discoverable]).
		// NONE where no discovery exists: the executor holds every tool it can reach.
		Discoverable: discoverable(s),
	}
}

// reviewArtifact is what the executor produced, handed to the reviewer.
//
// WHOLE: [turn.Work.Text] is the run's closing reply with the draft at its END,
// so a former 2000-rune cut kept the HEAD, the opening of the run's prose, and
// the reviewer's ship-or-loop verdict was asked of a prefix that often lacked
// the work. Bounded by what one reply of the run's model can write (its output
// ceiling), the right place for a ceiling.
//
// "(empty)" for a pass with no text: an absent section reads as one the engine
// forgot to fill in.
func reviewArtifact(w turn.Work) string {
	if w.Text == "" {
		return "(empty)"
	}
	return w.Text
}

// Caps returns the runner's extension settings, so an assembler can assert on
// what it actually wired rather than on what it meant to.
func (r *Runner) Caps() Caps { return r.cfg.Caps }

// phaseSteer is the turn's note box as one phase's loop reads it: each note
// rendered as the message the model reads, recorded as read, carried into the
// turn's later phases and announced as delivered at the round that read it.
type phaseSteer struct {
	r         *Runner
	ctx       context.Context
	ph        phase.Phase
	iteration int
	// offset is the rounds this phase ran before this invocation, so a note is
	// announced on the PHASE's scale.
	offset int
}

func (s phaseSteer) Drain(round int) []toolloop.SteerNote {
	at := steer.Reading{
		Phase: types.Phase(s.ph), Iteration: s.iteration, Round: s.offset + round,
		Boundary: types.SteerBoundaryRound, At: time.Now().UTC(),
	}
	notes := s.r.cfg.Steer.Take(s.ctx, at)
	if len(notes) == 0 {
		return nil
	}
	out := make([]toolloop.SteerNote, 0, len(notes))
	emit := s.r.emitter()
	for _, n := range notes {
		read := steer.Read{Note: n, Reading: at}
		s.r.carrySteer(read)
		emit.steered(s.ctx, read)
		message := prompts.SteerMessage(n.By, n.Text)
		if n.SentBefore {
			message = prompts.SteerMessageSentBefore(n.By, n.Text)
		}
		out = append(out, toolloop.SteerNote{ID: n.ID, Message: message})
	}
	return out
}

// Waiting reports a note waiting, without taking it: the loop ends a run that
// has one once its calls in flight are answered, so the next run reads it.
func (s phaseSteer) Waiting() bool { return s.r.cfg.Steer.Waiting() }

// sessionTitle is how a phase's tool session names itself to its harness: the
// seat and the phase, for the harness's own logs and a person reading them.
func sessionTitle(seat prompts.Seat, ph phase.Phase) string {
	if seat.Role == nil {
		return string(ph)
	}
	return seat.Role.Handle() + " " + string(ph)
}

// carrySteer records a note the turn has read, for its later phases.
func (r *Runner) carrySteer(read steer.Read) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steered = append(r.steered, read)
}

// carriedSteers is every note the turn has read so far, in order, each
// rendered as the model read it.
func (r *Runner) carriedSteers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.steered))
	for _, read := range r.steered {
		out = append(out, prompts.SteerMessage(read.By, read.Text))
	}
	return out
}

// steerBrief is the notes a launch's brief takes (none on a turn nobody can
// steer), once the carried reads are marked unanswered as the record marks them
// now ([Runner.markUnanswered]) and the places the record numbered this
// segment's reads at are known ([Runner.placed]), which every later brief of
// the launch counts.
func (r *Runner) steerBrief(ctx context.Context) []steer.Note {
	if r.cfg.Steer == nil {
		return nil
	}
	briefing := r.cfg.Steer.Brief(ctx)
	r.markUnanswered(briefing.Unanswered)
	r.mu.Lock()
	r.placed = briefing.Orders
	r.mu.Unlock()
	return briefing.Notes
}

// markUnanswered marks unanswered every read the turn carries of a note ids
// names: one the run's record marks unanswered (steer.Briefing.Unanswered),
// its take's settle having landed after the record this segment resumed from
// was read ([Resume.Steered]). A capture stamped before that settle counted the
// note held, so the briefs a relaunch builds stop its cursor before it as the
// record does ([Runner.notesHeldBy]).
func (r *Runner) markUnanswered(ids []string) {
	if len(ids) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, read := range r.steered {
		if slices.Contains(ids, read.ID) {
			r.steered[i].Unanswered = true
		}
	}
}

// retold is every note the turn carries whose read the run's record marks
// unanswered, by id, in the order read ([AgentRunRequest.Retold]): what every
// brief of a launch tells again, built once the reads are marked as the record
// marks them now ([Runner.steerBrief]).
func (r *Runner) retold() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, read := range r.steered {
		if read.Unanswered {
			out = append(out, read.ID)
		}
	}
	return out
}

// steerReturn hands back the notes a launch that failed took for its brief —
// only where it failed before it wrote its run's record: once it named the job
// it opened, the record holds them, and its ending reports them
// ([Launched.LaunchID]).
func (r *Runner) steerReturn(ctx context.Context, launched Launched, notes []steer.Note) {
	if r.cfg.Steer == nil || len(notes) == 0 || launched.LaunchID != "" {
		return
	}
	r.cfg.Steer.Return(ctx, notes)
}

// steerBriefed records the notes a launched run's brief carried as read by the
// job it opened — carried into the turn's later phases, and published
// delivered at the brief — once the launch has returned: a launch that failed
// delivered nothing ([Runner.steerReturn]).
func (r *Runner) steerBriefed(ctx context.Context, iteration int, launch string, notes []steer.Note) {
	if len(notes) == 0 {
		return
	}
	at := steer.Reading{
		Phase: types.PhaseExecute, Iteration: iteration, Round: runRound,
		Boundary: types.SteerBoundaryBrief, Launch: launch, At: time.Now().UTC(),
	}
	emit := r.emitter()
	for _, n := range notes {
		read := steer.Read{Note: n, Reading: at}
		r.carrySteer(read)
		emit.steered(ctx, read)
	}
}

// SteerExpired records the notes a turn took and never read: it ended or
// parked before its next round. The engine calls it with what closing the
// turn's box returned.
func (r *Runner) SteerExpired(ctx context.Context, notes []steer.Note) {
	emit := r.emitter()
	for _, n := range notes {
		emit.steerExpired(ctx, n)
	}
}
