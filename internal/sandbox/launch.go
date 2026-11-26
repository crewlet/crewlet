package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// controlSubject is a seat's sandbox-control subject from the id a run's row
// carries, or empty when the row names no routable seat.
//
// ONE PARSE, in one place. The id rides these rows as TEXT — the record is a
// JSON blob on the node estate and the framework that carries it never
// decodes it — so the three publishers that need this subject would otherwise
// each have their own idea of what an unparseable value means. Here it means
// exactly what a nil id means: not routable, say so, and do not publish to a
// subject named after nothing.
func controlSubject(agentID string) string {
	return topics.AgentControl(seatOf(agentID))
}

// inboxSubject is a seat's inbox from the id a run's row carries, or empty when
// the row names no routable seat: where a chat reply the run did not take is
// handed back ([Coordinator.deliverHandBack]). The same one parse as
// [controlSubject], so the two subjects a run is reached on can never disagree
// about whether its row names a seat.
func inboxSubject(agentID string) string {
	return topics.AgentInbox(seatOf(agentID))
}

// seatOf is the one parse both subjects take: a row's text id as a seat id,
// with an unparseable value read as [uuid.Nil], which every topics builder
// answers with an empty subject.
func seatOf(agentID string) uuid.UUID {
	id, err := uuid.Parse(agentID)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// TurnRef identifies the turn a detached run belongs to.
//
// Carried on the row rather than looked up later, because the resume happens
// in a different process from the launch as a matter of routine: a restart, a
// seat handoff, or simply days passing while a person answers a question.
type TurnRef struct {
	// TurnID is the RUN this job belongs to — one execution of a turn, and
	// the key the whole pending-run record is stored under. It has to be
	// unique per execution: a redelivered trigger re-runs, and two runs
	// sharing this value would share one row and one box. See ADR-0017.
	TurnID string

	// WorkKey is the unit of work behind that run, carried so the row can
	// say which trigger it came from after the run outlives its process.
	WorkKey string

	// WorkSince is when that unit of work began, carried beside the key
	// for the same reason — see [PendingRun.WorkSince].
	WorkSince time.Time

	AgentID     string
	AgentHandle string
	Role        string

	// PartitionKey is the inbox batch the kick-off trigger arrived in and
	// ConversationKey the durable conversation it belongs to. BOTH travel,
	// and both are written onto the row: the conversation is what matches a
	// person's answer back to this run and where the resumed turn reports,
	// the partition tells two runs parked on one direct message apart
	// ([ConversationRef.Best]). See [PendingRun].
	//
	// Named as [turnctx.Turn] names them, field for field, so a copy across
	// cannot read as its own opposite and a swapped pair cannot look
	// correct; [PendingRun] spells them the same way. An empty partition
	// leaves a resumed turn recording nothing.
	PartitionKey    string
	ConversationKey string

	TraceID string
	SpanID  string

	// Reply is who is waiting for this turn, persisted so the resumed turn
	// inherits the same delivery obligation. See [PendingRun.Reply].
	Reply string

	// Depth and Chain are the delegation state a resumed turn inherits.
	Depth int
	Chain []string

	// WorkItem is the item the launching turn is charged to, nil when it is
	// on nothing. Written onto the row, because the resumed turn has no
	// trigger to resolve it from — see [PendingRun.WorkItem].
	WorkItem *types.WorkItem

	// Answered are the answers whose deliveries woke the turn, for the row
	// ([PendingRun.Answered]).
	Answered []string

	// Requester is the seat whose wake started the turn, by its handle,
	// "" for none — what a question addressed to
	// "requester" is put to. See [PendingRun.Requester].
	Requester string
	// WorkingFor is who the turn works for when a colleague's ask, an answer,
	// or a colleague's write handed it on ([turnctx.Turn.WorkingFor]).
	WorkingFor *types.WorkingFor

	// RequesterLogin and RequesterApps are the credential acting as no seat
	// whose write started the turn, and the apps its writes carried — see
	// [PendingRun.RequesterLogin].
	RequesterLogin string
	RequesterApps  []iam.Grant

	// ColleagueAsk is the colleague's A2A question the turn answers, nil
	// for a turn no ask woke — written onto the row, because the segment
	// that ends a parked turn has no trigger to read it from. See
	// [PendingRun.ColleagueAsk].
	ColleagueAsk *ColleagueAsk

	// Deadline is when the turn's wall-clock cap ends, zero for none —
	// written onto the row so the poll can end a run that outlasts it. See
	// [PendingRun.Deadline].
	Deadline time.Time
}

// LaunchRequest is everything one executor run needs.
//
// TWO TEXTS, KEPT APART to the end: what the harness is asked to do
// (Brief) and how it is to work (System). The runner hands the first to its
// harness as the prompt and the second on the harness's channel for adding to its own
// system prompt ([RunRequest.System]), so the vendor's harness prompt stays in
// force and the standing instructions sit beneath it rather than in front of
// the task as a message the model reads as part of the request.
type LaunchRequest struct {
	Turn TurnRef

	// ReadBox asks a fresh box to be read once after its setup ([ReadBoxFacts]):
	// set where no reading is kept for the box as it is made now.
	ReadBox bool

	// System is the run's own standing instructions: the seat's executor
	// prompt. It is FRAMED as the brief is: what the box provides, and how
	// to ask a person, follow it ([buildSystem]).
	System string

	// Brief is the task: the turn's ask, as the executor's prompt renders
	// it, and the whole of what the harness is prompted with. Handover is
	// what it hands of the summary of the session it does not continue.
	Brief    string
	Handover SummaryHandover

	// FreshFor is Brief told under verdict, the launch's own where it decides
	// one after Brief was built: a session refused before the run starts, and
	// the restore its box may fall back from ([fellBack]), so the fresh brief
	// a run reads names its own loss. Nil hands Brief as it is.
	FreshFor func(verdict *executorkeep.NotContinued) FreshBrief

	// Task is what the launching turn's work is read by, in a person's
	// words, redacted (what it was asked, or the line a feed shows its
	// trigger by), carried onto the row ([PendingRun.TaskDescription]) as
	// what the run is listed under. Never the brief: a resume reads its
	// brief off the turn's park, where somebody else's words in it are
	// fenced as the model read them, and a person reading the row would
	// read those fence lines.
	Task string

	// Label is what a person sees the run named by — on the running-runs
	// panel, live and after a restart alike ([LaunchRequest.label]). The
	// launch names what its turn's work is read by ([LaunchRequest.Task]):
	// the brief is the turn's whole message, which opens with a heading
	// rather than with what the run is doing. Empty takes the brief's first
	// line.
	Label string

	// Ask is the addendum telling the harness how to get a person's
	// answer (asked with its tools when it can go on without it, or parked
	// through `ask_and_wait` when it cannot, ADR-0068) with the names it may
	// address. The ENGINE composes it, because the tools and the names are
	// the seat's and the chart's and this package holds neither; empty adds
	// nothing, and leaves a run that holds the tool with no word of when to
	// park on it. A standing instruction, so it rides System's channel
	// ([buildSystem]).
	Ask string

	Spec       Spec
	Setup      []SetupStep
	MCPServers map[string]MCPServer

	// Secrets is every value the ENGINE put in the job's environment and
	// setup out of a credential position, which a capture of the box's
	// session scrubs out by value ([HandedSecrets]): what each `${...}`
	// reference resolved to — in the run environment, and a setup file that
	// is one reference whole — and the value of every variable whose name
	// says it carries a credential, the entry's sign-in among them. The
	// ENGINE'S, because only it knows where a value came from: the
	// environment this package is handed is resolved, a setting and a token
	// alike. What else is in the environment — the engine's identity facts,
	// a setting written in the document, a setup step's script — is not a
	// secret, and scrubbed by value it was lost from every kept transcript
	// that named it. Nil hands none.
	Secrets []string

	// Skills are the skills the job's harness is handed as its own — see
	// [RunRequest.Skills]. Nil hands none. Where the job reports loading one
	// is its opening's to say ([Opening.SkillLoads]), as the job's endpoint
	// is: a job handed skills and opened with nowhere to report a load is
	// refused by its runner.
	Skills []Skill

	// Model is the model the run works under — see [RunRequest.Model].
	// Empty names none, and the harness chooses its own.
	Model string

	// Effort is the reasoning effort the run works at ([RunRequest.Effort]).
	Effort string

	// WebDenied is whether the run's harness is handed its own web tools
	// ([RunRequest.WebDenied]).
	WebDenied bool

	// Browser is a browser seat's setting for the browser the launch readies
	// in its box, nil for a seat with no browser; see [EnsureBrowser].
	Browser *BrowserLaunch

	// ProviderKey is the providers.llm entry the run was launched on: the
	// member of the seat's executor chain that took it, which is not always
	// the chain's head. Written onto the job's record ([LaunchRecord]) so
	// the launch's own record and the resumed executor's are charged to the
	// entry that served them, on whichever node collects the run.
	ProviderKey string

	// Fleet is why ProviderKey took the run where an LLM fleet picked it, for
	// the launch's announcement ([types.ExecutorLaunched]); empty when pinned.
	Fleet types.FleetDecision

	// SignInID is the sign-in identity the run's harness is handed, written onto
	// the job's record ([LaunchRecord.SignInID]).
	SignInID string

	// Entry is the providers.llm entry the run is launched under, as an
	// identity naming no credential (internal/providers/llm/harness's
	// Provider.EntryID): what a session the launch would continue is held to
	// ([EntryContinues]), written onto the job's record ([LaunchRecord.Entry]).
	Entry string

	// Continue is set when this job CONTINUES the job before it (parked on
	// a question now answered, lost with its host ([PendingRun.Lost]),
	// refused in its session ([FollowUpRequestRefused]), or on a sign-in
	// replaced mid-run): the same round, carried on, whose earlier bridged
	// calls stay the round's ([PendingRun.Continues]). Nil otherwise.
	//
	// With no kept capture to take up (Session), the job resumes that job's
	// own harness session when it runs in that job's box, with Continue.Brief
	// as the next prompt ([RunRequest.Resume]); anywhere else it starts fresh
	// with Brief, carrying the answer or how the job was cut off.
	Continue *Continuation

	// BriefRead is the read of the round's log every brief of this launch was
	// built from ([ResumeRequest.BridgedRead]), nil for none: a turn's first
	// launch. Handed to the opening of the job's log ([PendingRun.BriefRead]),
	// which marks late every call of the round the log took after it — calls
	// of the job this one replaces that landed while the relaunch was readied,
	// which no brief told and a later capture of this job, placed by its own
	// read, would otherwise count as held — or carries them late into the new
	// round a send-back starts ([BridgeLog.carried]); the call a person
	// approved apart, which the brief's lead tells. On the launch, never its
	// Continue: a send-back continues nothing, and its read counts as much.
	BriefRead *LogRead

	// Session is the session this job continues, in whatever box it gets:
	// the kept capture of the job before's session, chosen by the caller
	// before it built the surface and briefs ([Coordinator.ContinuedCapture]),
	// or the one the conversation's last turn left
	// ([SessionContinuation.Base]), with the briefs that continue it in the
	// box it was last worked in and in any other. Set on every relaunch whose
	// job before left a continuable capture and on every launch that may
	// continue the conversation's session. Nil starts a session of its own,
	// or continues one in place (Continue). The launch takes it up, or says
	// why not and starts fresh with Brief: see restore.go.
	Session *SessionContinuation

	// NotContinued is the caller's verdict on the newest session the turn or
	// its conversation left that the launch does not take up: an older one
	// continued in its place (Session), or none ([executorkeep.NotContinued]).
	// Nil where nothing was lost. The launch decides the final one before its
	// job starts, on what its own checks find, and records it on the job
	// ([LaunchRecord.NotContinued]) and its launch's event.
	NotContinued *executorkeep.NotContinued

	// FreshOwn is the leads Brief opened with of its own where no Continue
	// names them ([Continuation.Fresh]): a turn's first launch and a
	// send-back, whose fresh brief opens with the turn's task
	// ([LeadKindFresh]). Kept for the job whichever brief the launch hands
	// its run ([BriefLeads.Fresh]). Nil keeps none.
	FreshOwn []Lead

	// Base is the read the turn's FIRST launch made of its conversation — the
	// head the turn's launches continue while it has kept no session of its
	// own, and the turns the read handed out that the turn's brief told
	// ([ConversationBase]) — set by that launch alone: nil on every relaunch,
	// and for a turn that read no conversation. The launch freezes it on the
	// record it creates ([PendingRun.Base]), which every relaunch reads, and
	// records it on the executor log as the turn's begin, whose row then
	// holds the base against every release but a retirement while the turn has
	// kept nothing ([Coordinator.recordBegin]).
	Base *ConversationBase

	// Decided is told how the launch decided its run takes up a session
	// (which brief it hands the run, whether the session is the turn's kept
	// capture, whether the brief is told by a reading of it), once the box
	// is known and before the run starts, so what the caller arms (skills
	// loaded) and states (the prompt sent) match the session it runs in.
	// Called once per launch that reaches its run's start; nil tells nobody.
	// The run's script may still start a session of its own where its box
	// cannot restore after all ([RunRequest.FreshBrief]): the box says so on
	// the bridge token before any call ([Opening.Fresh]), where what the
	// caller armed is put right, and the collection says it
	// ([Result.Continued]).
	Decided func(LaunchDecision)

	// Fence is the launching turn's own seat fence — its seat still held
	// under the grant the turn was admitted on, and no person's pause asking
	// its running work to stop — asked AGAIN once the job's row exists and
	// before any box does. Nil asks nothing: a launch no turn makes.
	//
	// ASKED AFTER THE ROW, not only before the launch, because a stop is
	// carried out by listing the seat's rows ([Coordinator.StopSeat]): one
	// that lands while a launch is between its first check and its row finds
	// no row to end, and the launch then starts a job on a seat a person has
	// just stopped. With the check after the row, every stop either finds
	// the row and ends it, or happened before this check and is refused by
	// it — the node takes the pause into its copy before it lists.
	Fence func() error

	// SteerSeat is the turn's seat, sealed by the engine — the anchor a
	// person's authority to send the turn a note is decided on
	// ([RunSteer.Seat]) — and BriefNotes the notes the job's brief carries,
	// each sealed, which the write that opens the job records read by it
	// ([PendingRun.BriefNotes]). RetoldNotes are the notes the turn has read
	// whose read the record marked unanswered, which the brief tells again,
	// by ID: the same write records each read by the job, told again, where
	// the record still marks it ([PendingRun.RetoldNotes]). HandedNotes are
	// the notes the turn's first segment's box still held unread as the
	// launch began, each sealed, which the same write files WAITING after them
	// ([PendingRun.HandedNotes]). All written onto the record the launch
	// opens, never anywhere else. Empty writes no anchor and files nothing.
	SteerSeat   []byte
	BriefNotes  []RunNote
	RetoldNotes []RunNote
	HandedNotes []RunNote

	// Recorded is told the job's name the moment the store has written the
	// record the launch opens ([PendingStore.BeginLaunch]) — before the
	// launch's fence is asked again, and before anything else can fail. From
	// that write the record holds the turn's notes, the brief's and the
	// box's ([LaunchRequest.HandedNotes]), and a launch that fails after it
	// ENDS the record, whose ending reports every one of them no job ran
	// with ([PendingRun.EndedNotes]). A write whose answer was lost is told
	// once the launch has ended a record that names the job
	// ([UnopenedLaunchError]). A launch that fails before it wrote nothing,
	// and its notes are the caller's still. Nil tells nobody.
	Recorded func(launchID string)

	// Opened is told the job the moment the store has opened it
	// ([PendingStore.BeginLaunch]) and BEFORE ANY BOX EXISTS, for what must
	// name the job before its box acts: the bridge session, which records
	// every call under its own job ([BridgeAppend.Launch]); learned later
	// from the row, the name could be the next job's for a call that
	// outlived its job. Also where calls are numbered from ([OpenedJob.LastSeq]).
	// It answers what the box is given that needs the name ([Opening]): MCP
	// servers BESIDE MCPServers (the tool bridge, its token naming the job,
	// so a takeover node serves the session under the box's token and a
	// relaunch's box cannot reach its predecessor's; a same-named answer
	// replaces the MCPServers one) and where the harness asks for notes. An
	// error abandons the launch before any box exists. Nil adds nothing.
	Opened func(job OpenedJob) (Opening, error)

	// CodeHolds is the turn's Gitlet hold set as the launch read it
	// (internal/agent/turnctx's Holds.Seal): every scratch repository the turn
	// has named, which the row carries from here on ([PendingRun.CodeHolds]) —
	// written by [PendingStore.BeginLaunch], unioned with what the row
	// already holds. The launch's caller keeps the turn's set SEALED from
	// reading it until Opened, so no hold is taken in between that the row
	// would not carry.
	CodeHolds []string

	// CodeFloor is the run's first code floor — the furthest position its
	// repositories were resolved at, as statelog.Position's token — written on
	// the row with Spec.Repos ([PendingRun.CodeFloor]). Set exactly when
	// Spec.Repos is.
	CodeFloor string

	// CodeCeiling is the colleague seat whose ask woke the launching turn,
	// whose own grant bounds the box's on every repository of Spec.Repos
	// ([PendingRun.CodeCeiling]); "" for none. Set only where Spec.Repos is.
	CodeCeiling string

	// CodeWorkingFor is whom the launching turn works for, as the checkout
	// that gave it Spec.Repos composed it ([PendingRun.CodeWorkingFor]): what
	// the box pushes, and the change its collection opens, carry it. Nil for
	// a turn that works for its own seat. Set only where Spec.Repos is.
	CodeWorkingFor *types.WorkingFor

	// RunEnv is the engine's addition for THIS LAUNCH: what a box needs
	// that is minted for the job itself, such as its access to Gitlet
	// (ADR-0093). Asked ONCE, AFTER the store has minted the launch id the
	// job's credentials name and before any box exists, because a credential
	// minted before the store names the job could only name a job the row
	// does not hold; with the name the engine host answers to in the box's
	// cell ([Manager.EngineHost]), under which an address of the engine's it
	// hands is written ([EngineHost.Address]). Its variables win over every
	// other source of the run environment for the names they set, and an
	// error abandons the launch like every failure past the row. Nil adds
	// nothing.
	RunEnv func(launchID string, host EngineHost) (LaunchEnv, error)

	// HomePaths is each variable the job is handed a path in its box under,
	// to that path relative to the box's home: a file a CLI is pointed at by
	// its absolute path, which differs by backend and so is joined only once
	// the box is known ([jobEnvFor]). Beneath every other source of the job's
	// environment for the names it shares. Nil sets none.
	HomePaths map[string]string

	// MCPTools is how many tools MCPServers list the run's harness in all, as
	// the seat's tools hold them: every tool of each, since a box reaches a
	// server whole. What a harness that hides every MCP server's tools past a
	// count of them weighs the box's browser against, beside the bridge's
	// ([Opening.Tools], [EnsureBrowser]). Zero says none.
	MCPTools int

	// Now is the clock. Nil takes time.Now.
	Now func() time.Time
}

// withServers is base with named's servers added, a name both hold taking
// named's: the servers a job's box is handed once the job has a name
// ([LaunchRequest.Opened]). base itself where named adds none.
func withServers(base, named map[string]MCPServer) map[string]MCPServer {
	if len(named) == 0 {
		return base
	}
	servers := make(map[string]MCPServer, len(base)+len(named))
	maps.Copy(servers, base)
	maps.Copy(servers, named)
	return servers
}

// OpenedJob is the job a launch opened, as [LaunchRequest.Opened] is told it.
type OpenedJob struct {
	// LaunchID names the job: the launch id the store minted for it, which
	// the run's row holds ([PendingRun.LaunchID]).
	LaunchID string

	// Round is the round the job works in ([PendingRun.Round]), as the write
	// that named the job decided it: what every append of the job's calls
	// names beside the job ([BridgeAppend.Round]), so a call still running
	// when a relaunch carries the round on to a later job is still listed in
	// the round's log, marked late ([BridgeCall.Late]).
	Round string

	// LastSeq is the place of the last bridged call the run's log keeps from
	// before this job ([BridgeLog.LastSeq]): zero for a job whose round
	// starts its log afresh, and the asking job's last call for a job that
	// continues one which asked a person ([PendingRun.Continues]).
	//
	// READ OFF THE WRITE THAT OPENED THE JOB'S LOG ([PendingRun.OpenedAfter]),
	// which is the write that kept those calls, so the job's own are
	// numbered after exactly what the log holds. Numbered from one instead, a continuation's calls were sorted
	// in among the asking job's by the collection that closes the round,
	// which reads the round's calls in that order: its record, the log its
	// reviewer reads, and its submissions, where the last one made wins.
	LastSeq int

	// CallIDMeta is the `_meta` key the job's harness sends each of its calls'
	// own ids under, as the runner that starts the job's box declares it
	// ([CallIdentifier], [Manager.CallIDMeta]): what serves the box records
	// each call's id by ([BridgeCall.CallID]), and "" where the runner
	// declares none, which records every call with none.
	//
	// THE RUNNER'S, NEVER THE ROW'S: the key is a fact of the harness and of
	// the build that reads its calls, so each node names its own runner's,
	// through the job's one constructor ([PendingRun.NamedJob]). Nothing
	// records it; a build that does not know it reads no id.
	CallIDMeta string

	// CallCeiling is how long the job's harness waits for one call whatever
	// its box's settings say, as the runner that starts the job's box states
	// it ([CallCeilinged], [Manager.MCPCallCeiling]): what serves the box
	// answers a call still running before it, and zero answers none early.
	// THE RUNNER'S, NEVER THE ROW'S, as CallIDMeta is.
	CallCeiling time.Duration

	// ParkedCalls is how many calls the job is parked on already, as its row
	// holds them ([PendingRun.ParkedCalls]): zero for a job just opened, and
	// the row's count for one a node serves again after taking its seat over.
	// What serves the box counts its own parks after it, and admits no call
	// that could park the job past [MaxParkedCalls].
	ParkedCalls int
}

// NamedJob is the job r's row names, as what serves its box is told it
// ([OpenedJob]), its calls numbered after lastSeq, each call's id read under
// callIDMeta, the key the runner of the job's box declares
// ([OpenedJob.CallIDMeta], "" for none), and each call answered before
// ceiling, the wait that runner states ([OpenedJob.CallCeiling], zero for
// none): the job, the round it works in and the calls it is parked on, off
// the one row. ONE CONSTRUCTOR for the launch that opens the job and for a
// node that serves it again after taking its seat over (internal/engine's
// reopenAgentRun), so neither names a job without its round, nor without what
// its runner states of its calls.
func (r PendingRun) NamedJob(lastSeq int, callIDMeta string, ceiling time.Duration) OpenedJob {
	return OpenedJob{LaunchID: r.LaunchID, Round: r.Round, LastSeq: lastSeq, CallIDMeta: callIDMeta,
		CallCeiling: ceiling, ParkedCalls: len(r.ParkedCalls)}
}

// Opening is what a job's box is given once the job has a name — see
// [LaunchRequest.Opened].
type Opening struct {
	// Servers join the box's MCP servers.
	Servers map[string]MCPServer

	// Tools is how many tools Servers list the job's harness in all, as what
	// serves them lists them now: what a harness that hides every MCP
	// server's tools past a count of them weighs the box's browser against
	// ([EnsureBrowser]). Zero for an opening that says none. Read on the
	// launch's opening alone, never a Fresh's.
	Tools int

	// Steer is where the job's harness asks for its turn's notes
	// ([RunRequest.Steer]); the zero value takes none inside the run.
	Steer SteerWiring

	// SkillLoads is where the job's harness reports loading one of the
	// skills it is handed as its own ([RunRequest.SkillLoads]); the zero
	// value names nowhere, which a job handed none needs.
	SkillLoads SkillWiring

	// Fresh is the same three under the job's other bridge token: what its
	// box is wired with where its script starts a session of its own in place
	// of the one the launch continues ([RunRequest.FreshWiring]), so the token
	// its harness dials says which session it started. Handed to the run only
	// where the launch continues a session; nil hands none, and a Fresh's own
	// Fresh is never read.
	Fresh *Opening
}

// Continuation is how a job continues the job before it — see
// [LaunchRequest.Continue].
type Continuation struct {
	// SessionID is the asking job's harness session ([PendingRun.SessionID]).
	// Empty where its harness named none, or the job before was lost, which
	// continues nothing: the job starts fresh with the launch's Brief.
	SessionID string
	// Brief is the prompt the continued session is given: the answer, and
	// what to do with it.
	Brief string
	// Reason is why the round is carried on, where the job's collection
	// acts on it ([LaunchRecord.FollowUp]); empty otherwise.
	Reason FollowUpReason

	// Told is what Brief told of the round, and Fresh what the launch's own
	// Brief, the one a session of the run's own is handed, told of it: the
	// launch records the one of the brief it hands its run on the job it
	// opens, and Fresh beside a brief that continues a session, which its box
	// falls back to where it cannot take the session up
	// ([LaunchRecord.BriefOmitted], [LaunchRecord.FreshOmitted]).
	Told, Fresh BriefTold

	// Summarized is the prompt the copy is given told as a session that may
	// be a summary, beside Brief, where its first request is the launch's to
	// judge in the box (an OpenCode copy whose count has a bound), and
	// SummarizedTold what it told of the round: handed where the box's point
	// does not prove Brief sends the copy whole ([BoxPoint.SendsWhole]).
	// "" where the runner judged the first request itself. Count and
	// CountProvider are the copy's count off the job's record and the
	// provider of the reply it is anchored on, which it is judged by.
	Summarized     string
	SummarizedTold BriefTold
	Count          *ResumedCount
	CountProvider  string
}

// BriefTold is what one brief a launch hands its run told of the round
// ([Continuation.Told], [Continuation.Fresh], [SessionContinuation.Told]):
// the calls it left out, and what it opened with.
//
// A SESSION HOLDS WHAT ITS BRIEF TOLD, NEVER WHAT ITS BUDGETS LEFT OUT. A brief
// counts the reads past the ledger's read cap rather than listing them
// (internal/agent/runner's calledAfter): the session holds that such a read
// was made, none of what it returned. Recorded on the job's record, the set
// reaches every later reader of that session (Resume.Owed, and
// [SessionCapture.Omitted]), each of which tells those reads again.
//
// IN MEMORY ONLY: what lands is the record's, written by the attach.
type BriefTold struct {
	// Omitted is the reads of the round the brief counted without listing
	// them, by their places in the round's log ([BridgeCall.Appended]): of the
	// calls it was to tell (those the session it continues lacks, through the
	// read the brief was built from, [LaunchRequest.BriefRead]), the ones it did
	// not. Nil where it left none out. Never a late call, which every brief
	// tells again (LateOmitted), nor one the log dropped from its middle,
	// which keeps no place to name and is counted by every brief that tells
	// it; and never more than a round's log lists ([MaxBridgeCalls]), with
	// LateOmitted, which a launch refuses past before anything is opened
	// ([LaunchRequest.toldBounded]).
	Omitted []int

	// LateOmitted is the late calls ([BridgeCall.Late], [BridgeCall.Abandoned])
	// the brief counted without listing them, by their places, APART from
	// Omitted: every brief tells such a call again, so none is owed; but the
	// session holds only that count of it, and a continuation of it credits it
	// no telling of the call (internal/agent/runner's heldByTelling). Apart,
	// because once the log drops one from its middle it keeps no mark, and
	// counted among the owed it was counted twice, there and among the late
	// calls dropped ([BridgeLog.LateElided]).
	LateOmitted []int

	// Own is the leads the brief opened with of its own (leads.go): what
	// carries its session on, an answer, a review or the turn's task, opened,
	// none of them named for a job yet. The launch seals them for the job it
	// opens ([Coordinator.sealLeads]) and the attach takes them into the run's
	// leads part ([PendingStore.Leads]), the job's record marking each told
	// ([LaunchRecord.LeadMarks]): a later brief to a session of the work that
	// may lack this one tells them again.
	//
	// Told is the earlier leads it told again, by name: the ones the brief of
	// the job whose session it continues told, where that session may lack
	// that brief. The job's record marks each told, so a continuation of THIS
	// job's session, which may lack this brief, tells them again in turn.
	Own  []Lead
	Told []LeadRef

	// ToldUnread is the earlier leads it was to tell again and could not
	// read, by their numbers in the run's leads part: those the record of the
	// job whose session it continues marks told, where the store could not
	// give the run's leads back to the resume ([TurnLeads.Unread]). The brief
	// says them as not kept, and the job's record marks each told all the
	// same: THIS job's session lacks them as surely as the one it continues
	// did, so a continuation of it that reads them tells them, and one failed
	// read never drops a lead from the chain of sessions that may lack it.
	ToldUnread []int
}

// sealLeads is own, the leads the brief a launch hands its run opened with of
// its own ([BriefTold.Own]), sealed for the job the launch opened (its id and its
// place among the turn's launches, [PendingRun.LaunchSeq]) under the fleet
// keyring ([LeadSeal]): what the attach that names the job's command takes into
// the run's leads part beside its row.
//
// NONE WHERE THE COORDINATOR HOLDS NO SEAL ([CoordinatorOptions.Leads], a
// coordinator built without an engine), and a lead that will not seal is
// logged at error and left out: the launch goes ahead, its brief told, and a
// later continuation of its session that may lack the brief is told less, as
// before leads were kept, never a lead the fleet did not sign.
func (c *Coordinator) sealLeads(ctx context.Context, turnID string, opened PendingRun, own []Lead) []KeptLead {
	if c.leads == nil || len(own) == 0 {
		return nil
	}
	out := make([]KeptLead, 0, len(own))
	for _, lead := range own {
		lead.Job, lead.Seq = opened.LaunchID, opened.LaunchSeq
		kept, err := c.leads.Seal(turnID, lead)
		if err != nil {
			log.ErrorContext(ctx, "sandbox_lead_unsealed", "turn_id", turnID, "launch_id", opened.LaunchID,
				"kind", string(lead.Kind), "error", err.Error())
			continue
		}
		out = append(out, kept)
	}
	return out
}

// freshOwn is the leads the launch's fresh brief opened with of its own: its
// continuation's ([Continuation.Fresh]), else the request's own.
func (r LaunchRequest) freshOwn() []Lead {
	if r.Continue != nil {
		return r.Continue.Fresh.Own
	}
	return r.FreshOwn
}

// toldBounded reports why r carries a set of calls a brief left out longer
// than a round's log lists ([MaxBridgeCalls]), naming the field, nil where it
// carries none: a set past it names calls no log of the round holds, which the
// engine's runner, telling at most a log's calls, never builds. Refused before
// the row is written, so a request built with one costs nothing.
func (r LaunchRequest) toldBounded() error {
	type told struct {
		field string
		set   BriefTold
	}
	var sets []told
	if c := r.Continue; c != nil {
		sets = append(sets, told{"continue.told", c.Told}, told{"continue.fresh", c.Fresh},
			told{"continue.summarized", c.SummarizedTold})
	}
	if s := r.Session; s != nil {
		sets = append(sets, told{"session.told", s.Told})
		if s.Exact != nil {
			sets = append(sets, told{"session.exact.told", s.Exact.Told})
		}
		if s.Summarized != nil {
			sets = append(sets, told{"session.summarized.told", s.Summarized.Told})
		}
		if s.Unheld != nil {
			sets = append(sets, told{"session.unheld.told", s.Unheld.Told})
		}
	}
	for _, t := range sets {
		if n := len(t.set.Omitted) + len(t.set.LateOmitted); n > MaxBridgeCalls {
			return fmt.Errorf("sandbox: %s names %d calls its brief left out, past the %d a round's log "+
				"lists", t.field, n, MaxBridgeCalls)
		}
	}
	return nil
}

// LaunchResult reports what a launch produced.
type LaunchResult struct {
	// LaunchID is the job the launch opened, as the store named it
	// ([PendingRun.LaunchID]).
	LaunchID string

	SandboxID string
	CommandID string
	Harness   string

	// Reused is whether the job continues in the box the turn's previous
	// job used, rather than in a fresh one — see [PriorBox.ContinuesIn].
	Reused bool

	// Read is what the fresh box was found to have ([ReadBoxFacts]), nil for a
	// reused box or a read that did not answer.
	Read *BoxFacts

	// Continues is the harness session the launch asked its run to continue,
	// "" where it started one of its own; Brief is which of the three briefs
	// it handed the run (restore.go). Whether the run did continue it is its
	// box's to say, on its collection ([Result.Continued]).
	Continues string
	Brief     BriefKind
}

// Launch provisions a box, starts the harness detached, and persists the row
// that outlives the turn, holding the seat from the moment that row is written.
// The order is the contract: the row is created and the box attached before the
// job starts, so a crash leaves a row naming an unused box, never a box nothing
// names; after any failure past the box's creation it is reclaimed. The seat is
// counted holding and its inbox held in the step that takes the row's write's
// answer, through countwrite.go. m is the manager the request's [Spec] was
// built with ([Manager.BuildSpec]), since an apply between could swap the
// coordinator's. See the package doc, "Launching a run".
func (c *Coordinator) Launch(ctx context.Context, m *Manager, req LaunchRequest) (_ LaunchResult, err error) {
	defer origin.Stamp(&err)
	store, q := c.pending, c.queue
	now := req.Now
	if now == nil {
		now = time.Now
	}
	if strings.TrimSpace(req.Turn.TurnID) == "" {
		return LaunchResult{}, fmt.Errorf("sandbox: a launch needs a turn id")
	}
	if strings.TrimSpace(req.Brief) == "" {
		return LaunchResult{}, fmt.Errorf("sandbox: a launch needs a brief")
	}
	// THE RUNNER IS THE LAUNCH'S OWN, never a default: it is the harness of the
	// entry whose login and model the run carries, and a box running any
	// other harness could read neither. Refused before the row, so a request
	// built without one costs nothing.
	if req.Spec.Harness == "" {
		return LaunchResult{}, fmt.Errorf("sandbox: a launch needs the harness it runs")
	}
	// THE GRANTS ARE HELD TO THEIR GRAMMAR BEFORE THE ROW IS WRITTEN: the row
	// is what a box's request to Gitlet is admitted against, so a
	// repository it could not carry, or one it names twice, is never on it.
	if err = checkRepos(req); err != nil {
		return LaunchResult{}, err
	}
	// AND THE SETS OF CALLS ITS BRIEFS LEFT OUT, which the row carries one of
	// beside its round's log ([BriefTold]).
	if err = req.toldBounded(); err != nil {
		return LaunchResult{}, err
	}
	// THE SEAT'S LEASE, which the row is stamped with and every write below
	// carries — see [CoordinatorOptions.Lease]. A seat this node does not
	// hold launches nothing: the turn asking is one its lease no longer
	// stands behind.
	fence, held := c.leaseOf(req.Turn.AgentHandle)
	if !held {
		return LaunchResult{}, notHeld(ctx, req.Turn.AgentHandle, req.Turn.TurnID, "launching a run")
	}

	// The row FIRST, so a crash between here and the box leaves a record
	// rather than nothing. It opens in [StatusLaunching] and stays there
	// until the turn writes the conversation a resume re-enters: nothing
	// polls or claims a run in that window, which is what stops a job that
	// finishes before the turn unwinds from being collected into nothing.
	//
	// RECORDED BEFORE IT IS SENT, from where the run is counted to holding:
	// see "The seat is held from the row" above.
	from := ""
	if c.framing(req.Turn.TurnID) {
		from = StatusResumed
	}
	// AND THE SEAT'S SLOT RESERVED IN THE SAME STEP, or nothing is written: a
	// seat at its cap, or with the conversation worked by another of its runs,
	// starts no run (seatcap.go). A relaunch of a run still held takes none.
	write, err := c.reservingCount(PendingRun{TurnID: req.Turn.TurnID, AgentHandle: req.Turn.AgentHandle,
		ConversationKey: req.Turn.ConversationKey}, from, StatusLaunching)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("sandbox: launching %s on %s: %w", req.Turn.TurnID, req.Turn.AgentHandle, err)
	}
	defer c.settleWrite(write)
	opened, err := store.BeginLaunch(ctx, PendingRun{
		TurnID: req.Turn.TurnID, WorkKey: req.Turn.WorkKey,
		WorkSince:   req.Turn.WorkSince,
		AgentHandle: req.Turn.AgentHandle,
		AgentID:     req.Turn.AgentID, Role: req.Turn.Role,
		// WRITTEN WITH THE ROW, before the box exists, because every
		// failure path from here on reclaims through it — and because the
		// process that collects this run may not be this one, nor reading
		// the configuration that chose the cell.
		Placement:       string(req.Spec.Placement),
		Harness:         req.Spec.Harness,
		TaskDescription: req.Task,
		Label:           req.label(),
		PartitionKey:    req.Turn.PartitionKey,
		// The conversation an answer is matched on and the resume
		// reports back to, because a resume days later has neither the
		// trigger nor this frame.
		ConversationKey: req.Turn.ConversationKey,
		Reply:           req.Turn.Reply,
		TraceID:         req.Turn.TraceID, SpanID: req.Turn.SpanID,
		DelegationDepth: req.Turn.Depth, DelegationChain: req.Turn.Chain,
		WorkItem: req.Turn.WorkItem, Answered: slices.Clone(req.Turn.Answered),
		// Who asked, for the park that resolves a question's audience:
		// that frame sees no trigger.
		Requester: req.Turn.Requester, WorkingFor: req.Turn.WorkingFor,
		// Or the credential that did, for every segment that resumes the
		// turn: each call it makes is still bounded by it.
		RequesterLogin: req.Turn.RequesterLogin,
		RequesterApps:  slices.Clone(req.Turn.RequesterApps),
		// THE TURN'S READ OF ITS CONVERSATION, frozen by the write that
		// creates the record and kept by every reset: a relaunch continues
		// the base this read named, never one read again — see
		// [LaunchRequest.Base].
		Base: req.Base,
		// And the colleague's ask the turn answers, for the segment that
		// ends it: that frame sees no trigger either.
		ColleagueAsk: req.Turn.ColleagueAsk,
		// The turn's cap, for the poll — see [PendingRun.Deadline].
		Deadline: req.Turn.Deadline,
		// Whose host the box is on — see [PendingRun.LaunchNode]. This
		// node's: the box is made here. A relaunch whose row still names the
		// previous job's box keeps that box's host until the attach below
		// names this node with this job's box ([BoxRef.Node]).
		LaunchNode: c.node,
		// The round carried on, whose earlier calls the log keeps — see
		// [PendingRun.Continues] — and the read its briefs were built from,
		// past which the opening marks the round's calls late, or carries
		// them into a new round ([PendingRun.BriefRead]).
		Continues: req.Continue != nil, BriefRead: req.BriefRead,
		// The model and the entry the launch's own record is filed under;
		// the store keys the rest of that record on the launch it mints.
		Launch: LaunchRecord{Model: req.Model, ProviderKey: req.ProviderKey,
			SignInID: req.SignInID, Entry: req.Entry, FollowUp: followUpOf(req.Continue),
			TurnCap: req.Spec.MaxTurns},
		// THE TURN'S NOTES: the seat anchor, the brief's notes and the ones it
		// tells again, read by the job this write opens, and the box's,
		// waiting after them. See [PendingStore.BeginLaunch].
		Steer:       RunSteer{Seat: req.SteerSeat},
		BriefNotes:  req.BriefNotes,
		RetoldNotes: req.RetoldNotes,
		HandedNotes: req.HandedNotes,
		// WHAT THE TURN HOLDS FROM GITLET'S EXPIRY, on the row before
		// any box exists: once the turn parks, the row is the only place
		// the expiry can read it from.
		CodeHolds: req.CodeHolds,
		// THE JOB'S GRANTS AND ITS FIRST FLOOR, on the row before any box
		// exists for the same reason: a box's first request to Gitlet
		// is admitted against the row, and it can come before this
		// launch returns.
		Repos: req.Spec.Repos, CodeFloor: req.CodeFloor, CodeCeiling: req.CodeCeiling,
		CodeWorkingFor: req.CodeWorkingFor,
		CreatedAt:      now(),
	}, fence)
	// HELD FROM HERE, before anything below can fail or the turn can
	// return (see [Coordinator.Launch]). Every failure past this point
	// gives it back through [Coordinator.abandon]. An open the store
	// refused, or did not answer, counts nothing: a row it wrote all the
	// same is counted where a recount reads it.
	c.mu.Lock()
	moved := c.wroteCountLocked(write, err == nil)
	c.mu.Unlock()
	moved()
	if err != nil {
		// A ROW THAT MAY NAME THE JOB WITH NO LOG OPENED TO IT is ended, that
		// job alone ([UnopenedLaunchError]): left launching, nothing polls or
		// claims it, and it holds its seat until the seat moves. Ended, the
		// record held the turn's notes, and its ending reports them. The open
		// counted nothing, so the ending moves no count and the seat is
		// recounted from the store.
		if unopened, ok := errors.AsType[*UnopenedLaunchError](err); ok &&
			c.endLaunch(ctx, req, fence, unopened.LaunchID, false) && req.Recorded != nil {
			req.Recorded(unopened.LaunchID)
		}
		return LaunchResult{}, fmt.Errorf("sandbox: recording the run: %w", err)
	}
	// THE RECORD HOLDS THE TURN'S NOTES FROM HERE — see
	// [LaunchRequest.Recorded]: told before anything below can fail, so the
	// caller knows the record, and no longer itself, answers for them.
	if req.Recorded != nil {
		req.Recorded(opened.LaunchID)
	}
	// THE JOB THIS ONE REPLACED IS OVER, and the wakes it never read with it,
	// which the write removed: reported once, the launch going on regardless.
	if c.wakesEnded != nil && len(opened.Prior.Wakes) > 0 {
		c.wakesEnded(ctx, opened, opened.Prior.Wakes)
	}
	// A STOP OF THE TURN'S LAST LAUNCH IS NOT THIS ONE'S: this job is new.
	// Nor is a clock on the store of the last job's host, nor a re-fire's
	// announcement of it this node still owes: that job's run settled here,
	// whatever it came to ([hostUnsettled], [refirePublish]).
	c.mu.Lock()
	delete(c.launchStops, req.Turn.TurnID)
	c.runSettledLocked(req.Turn.TurnID)
	c.mu.Unlock()
	// THE TURN'S FENCE AGAIN, now that a stop can find the row — see
	// [LaunchRequest.Fence]. Refused, the launch is over before any box
	// exists, and the error is the fence's own, so the turn reads it as the
	// stop or the move it is.
	if req.Fence != nil {
		if fenceErr := req.Fence(); fenceErr != nil {
			c.abandon(ctx, m, req, fence, "")
			return LaunchResult{}, fenceErr
		}
	}
	// THE TURN'S FIRST READ, RECORDED ON THE EXECUTOR LOG once the record that
	// froze it exists and the fence has passed: the turn's row then holds the
	// base against every release but a retirement while the turn has kept
	// nothing, which is what lets a relaunch on any node continue it after a
	// sibling ended ([Coordinator.recordBegin]). Asked for a bounded while on
	// this frame, and never a refusal of the launch: a begin the log does not
	// hold in time leaves the launch continuing no base, and one that lands
	// later holds no longer than the turn's first capture or its end.
	begun := false
	if req.Base != nil {
		begun = c.recordBegin(ctx, req, opened)
	}
	// THE JOB IS NAMED BEFORE ITS BOX EXISTS, so nothing the box does can be
	// filed under a job that is not its own — see [LaunchRequest.Opened] —
	// and what it answers joins the box's servers, on the request every step
	// below reads: the job's own runner and the standing instructions that
	// list the servers a box has.
	var steer SteerWiring
	var skillLoads SkillWiring
	// bridgedTools is how many tools the servers naming the job joins list in
	// all ([Opening.Tools]).
	var bridgedTools int
	// fresh is how the job's box is wired where its script starts a session of
	// its own in place of the one this launch continues — see
	// [Opening.Fresh] — nil where what named the job answered none.
	var fresh *Wiring
	if req.Opened != nil {
		// AND HOW ITS HARNESS NAMES ITS CALLS, as the runner the job is
		// started on declares it: m's, which the spec was built with and
		// which starts the box.
		named, openErr := req.Opened(opened.NamedJob(opened.OpenedAfter, m.CallIDMeta(req.Spec.Harness),
			m.MCPCallCeiling(req.Spec.Harness)))
		if openErr != nil {
			c.abandon(ctx, m, req, fence, "")
			return LaunchResult{}, fmt.Errorf("sandbox: naming the job to what serves its box: %w", openErr)
		}
		if f := named.Fresh; f != nil {
			fresh = &Wiring{MCPServers: withServers(req.MCPServers, f.Servers), Steer: f.Steer,
				SkillLoads: f.SkillLoads}
		}
		req.MCPServers, bridgedTools = withServers(req.MCPServers, named.Servers), named.Tools
		steer = named.Steer
		skillLoads = named.SkillLoads
	}

	// THE LAUNCH'S OWN ENVIRONMENT, now that the store has named the job
	// its credentials are minted for ([LaunchRequest.RunEnv]). It is what
	// the box is made with, what its setup steps run with and, with the
	// run's telemetry, what the harness runs with — the clones it runs
	// once a checkout answers need the job's credential; and it lives in this frame alone: the spec
	// is never persisted, and a job that continues the run hours later is
	// launched anew, with credentials of its own.
	env, gitAccess, err := launchEnv(req, opened.LaunchID, m.EngineHost(req.Spec.Placement))
	if err != nil {
		c.abandon(ctx, m, req, fence, "")
		return LaunchResult{}, fmt.Errorf("sandbox: the run's environment: %w", err)
	}
	req.Spec.Env = env
	if gitAccess != "" {
		// THE CREDENTIAL'S FILE, named to the job by its path in the box once the
		// box is known, and the credential kept for the session's scrub: in no
		// variable, it is in nothing else a scrub reads.
		paths := maps.Clone(req.HomePaths)
		if paths == nil {
			paths = map[string]string{}
		}
		paths[GitAccessFileVar] = GitAccessFile
		req.HomePaths = paths
		req.Secrets = append(slices.Clone(req.Secrets), gitAccess)
	}
	// THE BOX IS MADE FOR THIS TURN, which the box records so its host
	// can tell when no run names it any more ([Spec.TurnID]).
	req.Spec.TurnID = req.Turn.TurnID
	// WHAT NO CACHED HOME MAY HOLD ([Spec.Secrets]): the launch's own set,
	// the scrub's, whole and by its JSON strings.
	req.Spec.Secrets = handedSecrets(req, req.Spec.Env).Values
	box, runner, reused, err := c.acquire(ctx, m, req, opened.Prior)
	if err != nil {
		// The row is open and this launch is over. Every failure past this
		// point closes it: a run left LAUNCHING is polled by nothing and
		// claimed by nothing, so it sits on its seat's busy count and its
		// box until the seat happens to move to another node.
		c.abandon(ctx, m, req, fence, "")
		return LaunchResult{}, err
	}
	// Attached before the job starts, for the reason above — WITH ITS CELL,
	// in the same write: a relaunch may run in another cell than the job
	// before it, and the row is how every later reader reaches the box.
	if err = store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), Placement: string(req.Spec.Placement), Node: c.node,
		Harness: req.Spec.Harness, PauseTTLSec: req.Spec.PauseTTLSec,
	}, fence); err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: attaching the box: %w", err)
	}
	// THE JOB'S OWN CREDENTIAL, over whatever an earlier job of a kept box
	// left there, before the job starts: a fresh box's setup ran without it,
	// as no step at launch clones from Gitlet ([SetupStep]).
	if gitAccess != "" {
		if err = ApplySetup(ctx, box, []SetupStep{GitAccessStep(gitAccess)}, nil); err != nil {
			c.abandon(ctx, m, req, fence, box.ID())
			return LaunchResult{}, fmt.Errorf("sandbox: writing the run's Gitlet credential into its box: %w", err)
		}
	}
	var read *BoxFacts
	if !reused && req.ReadBox {
		read = readFresh(ctx, box)
	}
	// THE PREVIOUS JOB'S BOX ON ANOTHER NODE'S HOST is named by nothing from
	// here, which is the moment its host may reclaim it: told now rather
	// than found at its next pass ([Coordinator.acquire]).
	if prior := opened.Prior; prior.SandboxID != "" && !prior.ReachableFrom(c.node) {
		c.tellHost(ctx, PendingRun{TurnID: req.Turn.TurnID, LaunchID: opened.LaunchID})
	}
	// A KEPT BOX'S EXPIRING FILES, brought up to date before the job that
	// continues in it starts: it ran no setup, so they hold what the last
	// poll of the job before wrote, which a pause may have outlived — see
	// [CoordinatorOptions.Refresh]. Handed the row as this launch opened
	// it, which already names this box, its cell, its harness and its host: a
	// kept box is the previous job's, continued only where all four match
	// ([PriorBox.ContinuesIn], [PriorBox.ReachableFrom]). EACH TOKEN IT
	// WRITES IS KEPT FOR THE JOB'S SEALED SET (renewed), which the attach that
	// names the job's command writes: the refresh runs before that set exists.
	var renewed HandedSecrets
	if reused && c.refresh != nil {
		c.refresh(ctx, m, opened, box, func(_ context.Context, value string, expires time.Time) error {
			renewed.record(value, expires)
			return nil
		})
	}
	// A BROWSER SEAT'S BROWSER, READIED IN THE BOX NOW IT IS READY, a fresh one
	// set up and read and a kept one refreshed, and before its session is
	// continued and its harness started: ready, its server joins the box's own,
	// over a server of that name. Only the launch's own context ending fails it.
	var browserOut *BrowserOutcome
	if req.Browser != nil {
		began := time.Now()
		outcome, browserErr := EnsureBrowser(ctx, box, BrowserStep{Placement: req.Spec.Placement,
			Browser: *req.Browser, Env: req.Spec.Env, LaunchID: opened.LaunchID, WebDenied: req.WebDenied,
			ProviderKey: req.ProviderKey, Harness: req.Spec.Harness, DeferAt: defersMCPAt(runner),
			Bridged: bridgedTools, InBox: req.MCPTools})
		if browserErr != nil {
			c.abandon(ctx, m, req, fence, box.ID())
			return LaunchResult{}, fmt.Errorf("sandbox: readying the seat's browser: %w", browserErr)
		}
		logBrowser(ctx, req, opened.LaunchID, outcome, time.Since(began))
		browserOut = &outcome
		if outcome.Server != nil {
			served := map[string]MCPServer{browser.ServerName: *outcome.Server}
			req.MCPServers = withServers(req.MCPServers, served)
			if fresh != nil {
				fresh.MCPServers = withServers(fresh.MCPServers, served)
			}
		}
	}

	// THE TURN'S SESSION, CONTINUED in whatever box this job got — its kept
	// capture restored where the box does not hold it, or, in a box of its
	// own, the session its conversation's last turn left while the turn has
	// kept none — or the job before's own session in the box it lives in, or
	// none: see restore.go. The run carries the brief its session needs, and
	// the one a session of its own does, for its script to hand over the one
	// its restore's outcome chose.
	continued := c.continueSession(ctx, req, opened, box, runner, reused, begun)
	defer continued.release()
	if req.Decided != nil {
		req.Decided(continued.decision())
	}
	// THE ENVIRONMENT THE JOB IS STARTED WITH, the launch's own with the
	// run's telemetry and the paths it is handed in this box, read once: it
	// is what the job is handed, and what its session's capture scrubs out
	// ([HandedSecrets]). One construction, a kept box's job's included.
	//
	// Its TELEMETRY ENDPOINT IS MINTED HERE, at the job's start, rather than
	// with the launch's environment above: its token lives a fixed span from
	// its minting ([OtelReceiverOptions.TokenTTL]), and whatever the box's
	// provisioning took would come off the job's own.
	jobEnv, err := jobEnvFor(m, req, box, runner)
	if err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: the job's telemetry endpoint: %w", err)
	}
	// AND, FOR A RUN THAT CONTINUES A SESSION, HOW ITS BOX IS WIRED WHERE ITS
	// SCRIPT STARTS ONE OF ITS OWN INSTEAD: the job's other bridge token, so
	// the token its harness dials is the box's word on which session it
	// started ([RunRequest.FreshWiring]). A run that starts one of its own
	// from the first has one session, and is handed one wiring.
	var freshWiring *Wiring
	if continued.session != "" {
		freshWiring = fresh
	}
	handle, err := runner.Start(ctx, box, RunRequest{
		System:      buildSystem(req, browserOut),
		Brief:       continued.brief,
		FreshBrief:  continued.fresh,
		FreshWiring: freshWiring,
		Resume:      continued.session,
		Carried:     continued.carried,
		Env:         jobEnv,
		// FROM THE SPEC, which is where the provider default and the
		// seat's override have already been reconciled. A Limits the
		// caller passed alongside would be a second answer to the same
		// question, and it was the one nobody filled in: every launch
		// went out uncapped while the field looked wired.
		Limits:     Limits{MaxTurns: req.Spec.MaxTurns},
		Model:      req.Model,
		Effort:     req.Effort,
		WebDenied:  req.WebDenied,
		MCPServers: req.MCPServers,
		// WHERE THE JOB'S HARNESS ASKS FOR ITS TURN'S NOTES, as the job's own
		// opening answered it — see [Opening.Steer].
		Steer: steer,
		// THE REPOSITORIES WHOSE PUSHES ARE REPORTED FOR THE RUN, which its
		// report is told not to name again ([RunRequest.ReportedRepos]).
		ReportedRepos: repoNames(Pushable(req.Spec.Repos)),
		// THE SKILLS ITS HARNESS IS HANDED AS ITS OWN, and where it reports
		// loading one, as the job's own opening answered it — see
		// [Opening.SkillLoads].
		Skills:     req.Skills,
		SkillLoads: skillLoads,
		// THE CREDIT ON THE JOB'S OWN COMMITS, naming the turn — the run's
		// page is addressed by it, and every job of the turn is that one
		// run's — the seat, and the item the turn is charged to
		// ([Manager.CommitAttribution]).
		Attribution: m.CommitAttribution(CommitAttribution{Run: req.Turn.TurnID,
			Seat: req.Turn.AgentHandle, WorkItem: req.Turn.WorkItem}),
	})
	if err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: starting the harness: %w", err)
	}

	// The command id is written SECOND, once the job exists: a row naming a
	// command that was never started would have the waiter poll for a
	// completion marker nothing is ever going to write.
	briefOmitted, freshOmitted := continued.omitted(req)
	ownLeads := c.sealLeads(ctx, req.Turn.TurnID, opened, continued.told.Own)
	// AND, BESIDE A BRIEF THAT CONTINUES A SESSION, THE FRESH BRIEF'S: its box
	// starts a session of its own on that brief where its restore fails, and
	// that session's task is then this job's fresh lead (protocol.go). A lead
	// both briefs open with is taken in once, as the handed brief's.
	var freshLeads []KeptLead
	if continued.kind.Continues() {
		freshLeads = c.sealLeads(ctx, req.Turn.TurnID, opened, req.freshOwn())
	}
	if err := store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), CommandID: handle.CommandID,
		Placement: string(req.Spec.Placement), Node: c.node,
		Harness: req.Spec.Harness, SessionID: handle.SessionID,
		FreshSessionID: handle.FreshSessionID,
		PauseTTLSec:    req.Spec.PauseTTLSec,
		// AND WHAT THE JOB WAS HANDED, sealed, for its session's scrub —
		// with what the jobs before it handed this box, where it continues
		// in it: the box holds them still, and the session it resumes their
		// lines ([PriorBox.Handed]); and the tokens the refresh above wrote
		// into it.
		Handed: c.sealHanded(ctx, req, jobEnv, opened.Prior, reused, renewed),
		// AND WHICH BRIEF IT WAS HANDED, for the record its collection
		// publishes beside whether its run did continue the session, with the
		// launch's verdict on the session it did not.
		Brief: continued.kind, NotContinued: continued.notContinued,
		// AND WHAT EACH OF ITS BRIEFS HANDS OF THE LOST SESSION'S SUMMARY, counted
		// by the one its run reads.
		Handover: continued.handover, FreshHandover: continued.freshHandover,
		// AND WHAT THAT BRIEF, AND THE FRESH ONE ITS BOX FALLS BACK TO, LEFT
		// OUT OF THE ROUND IT CARRIES ON, for every later reader of what the
		// job's session holds ([BriefTold]).
		BriefOmitted: briefOmitted.Omitted, FreshOmitted: freshOmitted.Omitted,
		BriefLateOmitted: briefOmitted.LateOmitted, FreshLateOmitted: freshOmitted.LateOmitted,
		// AND WHAT THAT BRIEF OPENED WITH: its own leads, sealed for this
		// job, and the earlier ones it told again, or was to and could not
		// read, for every later brief to a session of the work that may lack
		// this one (leads.go). Named for the run and the job AS THIS LAUNCH
		// OPENED THE ROW, never as the store reads it at the attach: a launch
		// of a run that ended carries that run's name, which is what lets the
		// take refuse it.
		Leads: BriefLeads{Run: opened.LeadsRun, Job: opened.LaunchID, Counted: opened.LeadsAppended,
			Own: ownLeads, Fresh: freshLeads, Told: slices.Clone(continued.told.Told),
			ToldUnread: slices.Clone(continued.told.ToldUnread)},
		// AND WHAT THE SESSION IT HANDED ITS RUN CARRIED, along that
		// session's lineage, for the read that collects the job to keep
		// where the job makes no request of its own ([LaunchRecord.Context]).
		Context: continued.measure,
		// AND WHERE THAT SESSION RESTARTS LAST, as the launch knew it, for a
		// later relaunch to know whether the job restarted it after its brief.
		Restart: continued.restart,
		// AND THE POINT OF THE KEPT CAPTURE IT HANDED, for a later capture of
		// the job to be raised to ([LaunchRecord.Continued]).
		Continued: continued.point,
		// AND A BROWSER SEAT'S BROWSER AS THE LAUNCH READIED IT
		// ([LaunchRecord.Browser]).
		Browser: browserOut.record(),
	}, fence); err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: recording the job: %w", err)
	}

	// WHICH JOB THIS IS, as the store opened it: the announcement is what a
	// watcher pairs a start with its job's record by, and what a request for
	// its live output names. The store's own answer rather than a read of the
	// row back, which could fail and cost the pairing — and which could, in
	// principle, find a later job than this one.
	started := types.ExecutorLaunched{
		LaunchID: opened.LaunchID, StartedAt: opened.Launch.StartedAt,
		Agent: req.Turn.AgentID, AgentHandle: req.Turn.AgentHandle,
		RoleName: req.Turn.Role, TurnID: req.Turn.TurnID, WorkKey: req.Turn.WorkKey,
		SandboxID: box.ID(), Harness: req.Spec.Harness,
		// THE MEMBER THAT TOOK IT — see [types.ExecutorLaunched.ProviderKey].
		ProviderKey: req.ProviderKey, Model: req.Model, FleetDecision: req.Fleet,
		// THE IDENTITY on the announcement: this event is read for
		// display, and what a person means by "which conversation is
		// this run for" is the durable thread rather than the batch the
		// trigger arrived in.
		ConversationKey: req.Turn.ConversationKey,
		WorkItem:        req.Turn.WorkItem,
		Task:            req.label(),
		Browser:         browserOut.record(),
		// AND THE SESSION IT DID NOT CONTINUE, as the launch decided it, for
		// the live turn's screens.
		SessionNotContinued: continued.notContinued,
	}
	ev := events.New(started, events.TraceContext{
		TraceID: req.Turn.TraceID, ParentSpanID: req.Turn.SpanID,
	})
	ev.Source = req.Turn.Role

	// TWO PUBLISHES, as for a completion. The events copy is the
	// announcement the dashboard's running-sandboxes panel reads; the
	// per-seat control copy RECOUNTS the seat from the store on whichever
	// node holds it when the copy is processed. The seat is already held
	// here — the row's write counted it — so nothing waits for this; what
	// the copy adds is the store's own answer, after a restart or a handoff
	// as much as now.
	if err := q.Publish(ctx, topics.Event(started.EventType()), ev); err != nil {
		log.WarnContext(ctx, "sandbox_started_publish_failed",
			"turn_id", req.Turn.TurnID, "error", err.Error())
	}
	if control := controlSubject(req.Turn.AgentID); control != "" {
		if err := q.Publish(ctx, control, ev); err != nil {
			log.WarnContext(ctx, "sandbox_started_control_failed",
				"turn_id", req.Turn.TurnID, "error", err.Error())
		}
	}

	log.InfoContext(ctx, "executor_launched",
		"turn_id", req.Turn.TurnID, "agent", req.Turn.AgentHandle,
		"sandbox_id", box.ID(), "placement", string(req.Spec.Placement),
		"harness", req.Spec.Harness, "provider_key", req.ProviderKey,
		"reused", reused, "continues_session", continued.session != "",
		"restores_session", continued.carried != nil, "brief", string(continued.kind),
		"browser", string(browserOut.state()))

	return LaunchResult{
		LaunchID: opened.LaunchID, SandboxID: box.ID(), CommandID: handle.CommandID,
		Harness: req.Spec.Harness, Reused: reused, Read: read,
		Continues: continued.session, Brief: continued.kind,
	}, nil
}

// acquire is the box a launch runs its job in: the turn's previous box where
// the new job may continue in it, else a fresh one, never while the old one
// is unaccounted for.
//
// CONTINUING KEEPS THE CHECKOUT, the expensive half of a run: only the same
// harness in the same cell, once the last job is over, in a box younger than
// [KeptBoxMaxAge] ([PriorBox.ContinuesIn]). A fresh box gets the operator's
// setup, a kept one nothing; neither a Gitlet repository (`code_checkout`).
// EVERY OTHER PRIOR BOX IS RECLAIMED HERE, THROUGH ITS OWN CELL, before the
// fresh box exists: the attach overwrites the row's reference, so nothing
// else would name it. Its own cell: a fallback onto a member with another
// run_in moves the run. An unreattachable box is reclaimed, then replaced.
func (c *Coordinator) acquire(ctx context.Context, m *Manager, req LaunchRequest, prior PriorBox) (_ Sandbox, _ Runner, _ bool, err error) {
	defer origin.Stamp(&err)
	if prior.SandboxID != "" && !prior.ReachableFrom(c.node) {
		// ANOTHER NODE'S HOST: the seat moved since the previous job
		// launched, and its box is a process tree or a container there,
		// which this node can neither continue in nor kill. Left to that
		// node, whose waiter reclaims it once the row names this job's box
		// instead (boxhost.go) — told to look once it does ([Coordinator.Launch]);
		// the job starts fresh here, from the pushed branch, as one whose
		// box was reclaimed does.
		log.InfoContext(ctx, "sandbox_prior_left_to_host",
			"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID,
			"placement", prior.Placement, "node", prior.LaunchNode)
		prior = PriorBox{}
	}
	if prior.SandboxID != "" {
		if prior.ContinuesIn(req.Spec, c.now()) {
			box, runner, reconnectErr := m.Reconnect(ctx, req.Spec.Placement, prior.SandboxID, req.Spec.Harness)
			if reconnectErr == nil {
				return box, runner, true, nil
			}
			log.WarnContext(ctx, "sandbox_reuse_failed",
				"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID, "error", reconnectErr.Error(),
				"detail", "the turn's previous box could not be reattached; it is reclaimed "+
					"and the job starts in a fresh one, from the pushed branch")
		}
		c.reclaimPrior(ctx, m, req, prior)
	}
	box, runner, err := m.Acquire(ctx, req.Spec, req.Setup)
	if err != nil {
		return nil, nil, false, err
	}
	return box, runner, false, nil
}

// checkRepos refuses a launch whose repositories its row could not carry:
// more than [MaxRunRepos], one that is not an executor's [RunRepo]
// ([RunRepo.ValidateFor] [CloneCoding]), one named twice — by its name or by
// its id, since either would give one repository two grants and one checkout
// directory two clones — or a floor that does not go with them, set exactly
// when repositories are; or a colleague's ceiling, or whom its pushes are
// made for, on a run given none, which would bound and carry nothing.
func checkRepos(req LaunchRequest) (err error) {
	defer origin.Stamp(&err)
	repos := req.Spec.Repos
	if len(repos) > MaxRunRepos {
		return fmt.Errorf("sandbox: a run is given at most %d repositories, not %d", MaxRunRepos, len(repos))
	}
	names := make(map[string]bool, len(repos))
	ids := make(map[string]bool, len(repos))
	for _, r := range repos {
		if err := r.ValidateFor(CloneCoding); err != nil {
			return err
		}
		if names[r.Repo] || ids[r.RepoID] {
			return fmt.Errorf("sandbox: repository %s is given twice: one run works on one branch of a "+
				"repository", r.Repo)
		}
		names[r.Repo], ids[r.RepoID] = true, true
	}
	switch {
	case len(repos) > 0 && req.CodeFloor == "":
		return fmt.Errorf("sandbox: a run given repositories needs the code floor they were resolved at")
	case len(repos) == 0 && req.CodeFloor != "":
		return fmt.Errorf("sandbox: a run given no repositories has no code floor")
	case len(repos) == 0 && req.CodeCeiling != "":
		return fmt.Errorf("sandbox: a run given no repositories has no colleague's ceiling on them")
	case len(repos) == 0 && req.CodeWorkingFor != nil:
		return fmt.Errorf("sandbox: a run given no repositories has nobody its pushes are made for")
	}
	return nil
}

// LaunchEnv is what [LaunchRequest.RunEnv] adds to one launch.
type LaunchEnv struct {
	// Env is variables of the run environment, winning over every other
	// source for the names it sets.
	Env map[string]string
	// GitAccess is the run's Gitlet credential, written into the box at
	// [GitAccessFile] once it is made or reattached and named to the job by
	// [GitAccessFileVar], never put in any environment, and kept for the
	// session's scrub; "" for none.
	GitAccess string
}

// launchEnv is the environment of THIS LAUNCH's box: the spec's run
// environment, and then the engine's own variables for the job
// ([LaunchRequest.RunEnv], asked with host, the box's [EngineHost]), which win
// over every other source for the names they set — an operator's value for
// one would break what the engine minted it for, and configuration refuses the
// names where an operator writes the run environment. The box is made with it
// and every setup step runs with it; the harness runs with it and the run's
// telemetry ([withTelemetry]).
//
// A COPY, never the spec's own map, which the caller's [Spec] goes on holding:
// what a box, its setup or its runner does with the map it is handed never
// reaches the configuration the next launch is built from.
func launchEnv(req LaunchRequest, launchID string, host EngineHost) (_ map[string]string, gitAccess string, err error) {
	defer origin.Stamp(&err)
	var added LaunchEnv
	if req.RunEnv != nil {
		var err error
		if added, err = req.RunEnv(launchID, host); err != nil {
			return nil, "", err
		}
	}
	out := make(map[string]string, len(req.Spec.Env)+len(added.Env))
	maps.Copy(out, req.Spec.Env)
	maps.Copy(out, added.Env)
	return out, added.GitAccess, nil
}

// reclaimPrior kills the turn's previous box through the cell that holds it.
//
// THROUGH THE COORDINATOR'S CATALOGUE FIRST, then the launch's. The launch's
// is the epoch its turn pinned, configuring the cells a NEW box may go to;
// the coordinator's is the newest, carrying every cell a box already out may
// be in ([Coordinator.SetManager]), one a later revision dropped included.
//
// BEST EFFORT, the launch goes ahead whatever it answers: a failed kill
// leaves the box for its host to reclaim, and refusing the launch would cost
// the turn too. Under a context of its own ([Coordinator.abandon]).
func (c *Coordinator) reclaimPrior(ctx context.Context, m *Manager, req LaunchRequest, prior PriorBox) {
	killCtx, cancel := detached(ctx)
	defer cancel()
	provider, err := c.mgr().Provider(Placement(prior.Placement))
	if err != nil {
		provider, err = m.Provider(Placement(prior.Placement))
	}
	if err != nil {
		log.WarnContext(ctx, "sandbox_prior_reclaim_no_backend",
			"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID,
			"placement", prior.Placement, "error", err.Error())
		return
	}
	if err := provider.Kill(killCtx, prior.SandboxID); err != nil {
		log.WarnContext(ctx, "sandbox_prior_reclaim_failed",
			"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID,
			"placement", prior.Placement, "error", err.Error())
		return
	}
	// WITH THE BOX'S DATE, the one reason the line could not otherwise show:
	// a box past [KeptBoxMaxAge] is reclaimed in the very cell, and for the
	// very CLI, the next job runs in.
	log.InfoContext(ctx, "sandbox_prior_reclaimed",
		"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID,
		"placement", prior.Placement, "harness", prior.Harness,
		"box_since", prior.Since,
		"next_placement", string(req.Spec.Placement), "next_harness", req.Spec.Harness)
}

// abandon closes out a launch that could not finish: the box reclaimed, the
// run ended and the seat given back. ALL OF IT, EVERY TIME, each step attempted whether or not the one before
// succeeded: a row left OPEN is polled by nothing and would hold its seat and
// a box until recovery reaped it. The box goes first, so an undeletable
// record names a box already gone. The ending is [Coordinator.endRecord]'s,
// not a delete of its own: the one place a stop is reported and what the row
// still owes the seat goes out first ([PendingRun.HandBack]).
//
// A context of its own, because the caller's expiring is often the failure:
// one for the kill, within a box call's grace, one for the ending, whose row
// writes get the row's crossing too ([detachedEnding]).
func (c *Coordinator) abandon(ctx context.Context, m *Manager, req LaunchRequest, fence Fence, sandboxID string) {
	if sandboxID != "" {
		killCtx, cancel := detached(ctx)
		provider, err := m.Provider(req.Spec.Placement)
		if err != nil {
			log.WarnContext(ctx, "sandbox_launch_reclaim_no_backend",
				"sandbox_id", sandboxID, "placement", string(req.Spec.Placement),
				"error", err.Error())
		} else if err := provider.Kill(killCtx, sandboxID); err != nil {
			log.WarnContext(ctx, "sandbox_launch_reclaim_failed",
				"sandbox_id", sandboxID, "error", err.Error())
		}
		cancel()
	}
	// Every live status and every job: this launch is abandoned whatever the
	// row reached. Two boxes can be owed a reclaim: the launch's own is killed
	// here, since it may never have been attached, and the box the row names is
	// reclaimed by the ending after its decision; where they are one box it is
	// killed twice, which every provider answers as done. The seat goes back as
	// the delete lands, once ([ending.counted]); a row no longer this node's to
	// end is recounted from the store. See the package doc, "Launching a run".
	c.endLaunch(ctx, req, fence, EveryLaunch, true)
}

// endLaunch ends the run of a launch that could not finish, licensed to launch
// alone or to [EveryLaunch], under a context of its own ([detachedEnding]),
// reporting whether an ending of the run under that license was decided: a row
// that names another job is left as it stands. counted says the launch counted
// the run as it opened the row ([ending.counted]); where it did not, or the
// ending was not this node's to make, the seat is recounted from the store.
func (c *Coordinator) endLaunch(ctx context.Context, req LaunchRequest, fence Fence, launch string,
	counted bool) bool {
	ctx, done := detachedEnding(ctx)
	defer done()
	run := PendingRun{TurnID: req.Turn.TurnID, AgentHandle: req.Turn.AgentHandle}
	decided, err := c.endRecord(ctx, run, ending{
		fence: fence, whileIn: Active, launch: launch, reclaim: true, counted: counted,
	})
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_launch_finish_kept",
			"turn_id", req.Turn.TurnID, "error", err.Error(),
			"detail", "the abandoned launch's record is not deleted yet; this node finishes "+
				"the ending, and the seat's next recovery pass reaps the row if the seat "+
				"moves first")
	case decided == nil || !counted:
		// THE RECOUNT WITHIN A TEARDOWN'S GRACE, as abandon's kill is
		// ([detached]): the ending's context bounds none of the steps it
		// does not make itself.
		recountCtx, cancel := detached(ctx)
		c.syncSeat(recountCtx, req.Turn.AgentHandle)
		cancel()
	}
	return decided != nil
}

// label is what the run is named by, cut by [summarise]: the caller's own
// label where it gave one, else the brief's first line. ONE ANSWER for the
// announcement and the record, so a run a process learns of from the record
// after a restart is named exactly as the one it saw announced.
func (r LaunchRequest) label() string {
	if strings.TrimSpace(r.Label) != "" {
		return summarise(r.Label)
	}
	return summarise(r.Brief)
}

// summarise is a brief's first line, WHOLE: the label a running-runs panel
// draws for a run. The record keeps the label it cut ([PendingRun.Label]), so
// a process that learns of the run from the record after a restart names it as
// the announcement did, with no second rule of its own.
//
// The first line because that is where a brief states what it is about. Never
// cut to a width: it used to be held to 120 bytes and marked, which put a
// sentence's first half on the run's page as its title — and a row too narrow
// for a label is the renderer's to elide, which the dashboard's rows already
// do, with the whole line still there to read.
func summarise(brief string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(brief), "\n")
	return strings.TrimSpace(line)
}

// buildSystem assembles the run's standing instructions, what its harness adds
// to its system prompt ([RunRequest.System]). THREE PARTS, each present only where it says something: the run's own
// instructions (the seat's executor prompt), what the environment already
// provides (the setup steps' own briefs, so the agent does not rediscover
// that git auth is wired, and a browser seat's browser, browserOut's), and
// how to ask a person. The brief is not among them: these hold for every
// round of the run, which is what a system prompt is for. The runner adds the
// report it must write (only it knows the box's path). THE RUN'S OWN TEXT
// FIRST: it is who the model is; the rest are details of where it works. The
// environment lists the operator's steps alone: a Gitlet repository is not
// the box's at launch, and `code_checkout` tells the run where each checkout
// is and how it pushes.
func buildSystem(req LaunchRequest, browserOut *BrowserOutcome) string {
	own := strings.TrimSpace(req.System)
	if own != "" {
		own = agentRunNote + "\n\n" + own
	}
	parts := []string{
		own,
		strings.TrimSpace(EnvironmentBrief(req.Setup, req.MCPServers, browserOut.Brief())),
		strings.TrimSpace(req.Ask),
	}
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), "\n\n")
}

// agentRunNote opens a run's own instructions — the seat's executor prompt —
// where they follow the harness's system prompt.
//
// The vendor's prompt says what the model is, first, and no flag removes it
// ("You are Claude Code, ..."). A seat prompt that only implies who it is
// loses to that, so this says outright which prompt is the role. The engine's
// own host runs open with the same note (internal/providers/llm/harness
// harnessNote), less its tools sentence: here the box's shell and editor are
// the point. Constant, so it stays part of the prefix a vendor caches.
const agentRunNote = "You are running inside a command-line coding tool as one seat of a " +
	"Crewlet company, and Crewlet — not a person at a terminal — reads what you " +
	"report. The tool's own instructions describe the program you run in, not your " +
	"role: who you are and what you do are the instructions that follow, and they " +
	"win wherever the two disagree."

// withTelemetry adds the run's OTel environment to the spec's, PER LAUNCH:
// the token is scoped to THIS run with a TTL, so a resume is never handed one
// that expired while parked ([launchEnv]). Merged at the job's START, after
// provisioning, since the token's span runs from its minting and setup steps
// would spend it. THE OPERATOR'S OWN VALUES WIN (role.sandbox.env). THE
// ENDPOINT IS THE RECEIVER'S AS THE BOX DIALS IT: a `direct` box exports to
// its node's listener where the node forwards, every other box to the
// fleet's address ([OtelReceiver.Reaches], [EngineAddress]: a loopback one
// reaches nothing from a container). A TRACE ID NO TOKEN CAN CARRY REDACTED
// IS REFUSED ([OtelTokens.Mint]), and the job's start with it. ONLY FOR A
// RUNNER WHOSE HARNESS EXPORTS BY IT ([exportsTelemetry]): any other's
// harness reads none of it, and its model's shell would hold it for nothing.
func withTelemetry(m *Manager, req LaunchRequest, box Sandbox, runner Runner) (map[string]string, error) {
	if m.telemetry == nil || !exportsTelemetry(runner) {
		return req.Spec.Env, nil
	}
	added, err := m.telemetry.RunEnv(req.Turn.TraceID, req.Turn.SpanID,
		req.Turn.TurnID, req.Turn.AgentHandle, req.Spec.Placement == Direct)
	if err != nil {
		return nil, err
	}
	if len(added) == 0 {
		return req.Spec.Env, nil
	}
	if endpoint, ok := added[otelBoxEndpointVar]; ok {
		added[otelBoxEndpointVar] = EngineAddress(box, endpoint)
	}
	env := make(map[string]string, len(req.Spec.Env)+len(added))
	for k, v := range added {
		env[k] = v
	}
	for k, v := range req.Spec.Env {
		env[k] = v
	}
	return env, nil
}

// jobEnvFor is the environment a job is started with in box: the launch's own
// with the run's telemetry ([withTelemetry]), and beneath both each of
// [LaunchRequest.HomePaths] joined onto box's own home, so the path is right
// in every backend's box. A copy where it adds a path, never the launch's map.
// An error where the telemetry's token cannot be minted ([withTelemetry]).
func jobEnvFor(m *Manager, req LaunchRequest, box Sandbox, runner Runner) (map[string]string, error) {
	env, err := withTelemetry(m, req, box, runner)
	if err != nil {
		return nil, err
	}
	return withHomePaths(env, req, box), nil
}

// withHomePaths is env with each of [LaunchRequest.HomePaths] joined onto box's
// own home beneath it, a copy where it adds a path: the environment a job is
// started with but for its telemetry ([jobEnvFor]), which is also what the
// launch reads an OpenCode box's point under ([PointAsk.Env]).
func withHomePaths(env map[string]string, req LaunchRequest, box Sandbox) map[string]string {
	if len(req.HomePaths) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(req.HomePaths))
	for name, rel := range req.HomePaths {
		out[name] = path.Join(box.Home(), rel)
	}
	maps.Copy(out, env)
	return out
}

// RunEnvFor is [withTelemetry] under a name a test can reach.
//
// EXPORTED FOR THE ONE PROPERTY THAT IS OTHERWISE UNOBSERVABLE: the merge
// order between the engine's telemetry variables and the operator's own. Both
// end up inside a box that a test cannot look into, and getting the order
// wrong sends somebody else's spans to the engine — silently, because they do
// arrive somewhere. The runner is the one m holds for the launch's harness.
func RunEnvFor(m *Manager, req LaunchRequest, box Sandbox) (map[string]string, error) {
	runner, err := m.RunnerFor(req.Spec.Harness)
	if err != nil {
		return nil, err
	}
	return withTelemetry(m, req, box, runner)
}

// handBackSubject is where a reply a run let go of goes back to: the seat's
// control topic for an answer BY TURN, which is addressed to a run and is
// settled there ([Coordinator.OnAnswerGiven]), and the seat's inbox for a chat
// reply, which becomes the ordinary message it looks like. Empty when the row
// names no routable seat.
func handBackSubject(agentID string, ev *events.Event) string {
	if _, ok := events.DataAs[*types.ExecutorAnswerGiven](ev); ok {
		return controlSubject(agentID)
	}
	return inboxSubject(agentID)
}

// followUpOf is the reason a launch carries a round on, as its job's record
// keeps it.
func followUpOf(c *Continuation) FollowUpReason {
	if c == nil {
		return ""
	}
	return c.Reason
}
