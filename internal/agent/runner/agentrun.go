package runner

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/tools"
)

// The executor, run by its harness's own agentic loop.
//
// # What is the harness's, and what stays the engine's
//
// Every seat's executor is its harness's own run, in a box: its own loop,
// its own shell, its own edits and commits. The seat's tools reach it over the
// MCP bridge, which dispatches through the SAME tools.Surface every
// engine-driven phase calls. So what the harness owns is how the executor's work
// happens; everything around it stays the engine's, and each of those is a
// deliberate refusal to fork:
//
//   - The PROMPT is the seat's: identity, chart, memory, skills and workers,
//     built once here and handed to the harness as an appended system prompt and
//     the brief it runs.
//   - The SURFACE is built the same way as every phase's, with the same skill
//     guard. The bridge executes against it, so a tool denied to the seat is
//     denied to its run.
//   - The SUBMISSION is the same tool. The harness ends its run by calling
//     submit_work over the bridge, so the outcome vocabulary and the rescue
//     path are the engine's.
//   - The REVIEWER is the engine's own loop. The point of a reviewer is that
//     it is not the thing being reviewed.
//
// # Why the turn parks
//
// An agentic run outlives its turn — that is what makes it worth doing — so it
// is detached: the phase returns suspended, the row carries what a resume
// needs, and the run's collection re-enters the same turn, possibly in another
// process on another node. What it does NOT carry is a conversation: there is
// no engine loop to re-enter, which is why [execstate.State] holds the turn's
// bookkeeping and nothing a model said — and the context the turn froze at its
// start, since a turn sent back for another iteration launches its next run
// from the segment that collected this one.

// AgentLauncher starts the executor as its harness's own agentic run.
//
// Declared by the consumer, and narrow on purpose: the runner knows what the
// executor should be asked to do and which tools it may use; the engine knows
// what a detached run is, which member of the seat's executor chain takes it,
// where it goes and how to reach it. Neither has to learn the other's half.
type AgentLauncher interface {
	// LaunchExecutor starts the run and returns once it is going, naming
	// the job it opened. It does NOT wait for it: the phase suspends and the
	// run's collection re-enters the turn later.
	//
	// An error fails the phase: there is no other way to run the executor,
	// and the error says why this run could not start. It still names the
	// job where the launch had opened one before it failed — see
	// [Launched.LaunchID].
	LaunchExecutor(ctx context.Context, req AgentRunRequest) (Launched, error)
}

// Launched is what a launch answers.
type Launched struct {
	// LaunchID is the JOB the launch opened — the name the run's record
	// holds it under, which only the launch learns — so the notes its brief
	// carried are recorded delivered to that job.
	//
	// SET ON AN ERROR TOO, where the launch failed after the write that
	// opened the job: that write recorded the brief's notes read by the job
	// ([AgentRunRequest.Notes]), and the record, whose ending reports every
	// note no job ran with, answers for them from then on — so they are not
	// handed back to the turn's source ([SteerSource.Return]), which would
	// report them a second time. Empty with an error is a launch that wrote
	// nothing, whose notes are the turn's still.
	LaunchID string

	// BridgeFrom is the last call the run's record held when the job's
	// session over the bridge opened — the round's calls before it, which
	// the job numbers its own after — recorded on the parked state
	// ([execstate.State.BridgeFrom]) so every later rebuild of the run's
	// guard replays the loads of this job's own session and of no other
	// ([Runner.rebuiltSurface]).
	BridgeFrom int
}

// AgentRunRequest is one executor handed to its harness's agent loop.
//
// THE PROMPT ARRIVES IN TWO HALVES, because the harnesses take it in two: Claude
// Code appends a file to its own system prompt, OpenCode adds an instruction
// file to its default agent's. Both keep the vendor's harness prompt — the
// part that teaches its shell, editor and loop, which the run is FOR — and put
// the seat's standing instructions on the system side of the conversation
// beneath it. Joined into one document, as they once were, the seat's
// identity, policies and memory reached the model as the first user message,
// which a model weighs as a request rather than as who it is.
type AgentRunRequest struct {
	// System is the executor's system prompt: the seat's standing
	// instructions.
	System string

	// Brief is the turn's own ask — with its earlier rounds, every note a
	// person sent the turn that it has already read, and then Notes — and
	// the whole of what the harness is prompted with.
	Brief string

	// Notes are the notes this brief is the FIRST to read, in the order the
	// brief renders them ([SteerSource.Brief]): the launch records them read
	// by the job it opens, in the write that opens it, so the job's own harness
	// never takes them again.
	Notes []steer.Note

	// Surface is what the run may call, bridged; see the package rule
	// above.
	Surface *tools.Surface

	// Skills are the Tool Skills the run's harness is handed as its own
	// (handedskills.go): written into its box, listed to its model, and
	// loaded with its own skill tool — each load its harness reports
	// recorded on the run's bridge session as the run's load_tool_skill
	// call of the skill's key ([HandedSkills.Listed]). Nil hands none.
	Skills HandedSkills

	// Spend is what the calls Surface serves cost the engine, counted
	// apart from this segment because they happen after it — see
	// [BridgedSpend]. The launcher records its running total with every
	// bridged call, for the segment that resumes from the run to pay.
	Spend *BridgedSpend

	// Round is the turn iteration this executor is, so the run's record and
	// the resumed phase agree about which round they are in.
	Round int

	// OnFallback publishes a launch-time hand-off against THIS turn: the
	// launch walks the seat's executor chain for a member that can take the
	// run ([chain.Pick]), and each member it skips is a provider_fallback
	// like a call's. The launcher has no publisher of its own and must not
	// grow one — which turn, phase and iteration a hand-off belongs to is
	// the runner's to say — so the runner hands it this, bound to the
	// launch. Never nil.
	OnFallback func(chain.Fallback)

	// Continue is set when this launch CARRIES ON a run that parked on a
	// person's question, now answered, or whose job was lost with its host —
	// the same round, relaunched: the asking run's harness session, and the
	// prompt that continues it, which the launch uses where it runs in the
	// asking run's box (a lost job's session is never continued, and names
	// none). Brief is then what a fresh run is told instead, the question and
	// the answer, or how the run before it was cut off, included — see
	// [Asked] and [Lost]. Nil for every other launch.
	Continue *Continuation

	// Session is the turn's kept session the launch continues, in whatever box
	// it gets — the session of the job the relaunch follows, as the launcher
	// said it would before the run's briefs were written ([SessionProber]) —
	// with the briefs that continue it: in the box it was last worked in, and
	// in any other. Set on every relaunch whose job before left a kept
	// session that can be continued: a reviewer's send-back as much as a
	// round carried on. Nil starts a session of its own, or continues one in
	// place ([AgentRunRequest.Continue]); and a launch that cannot take it up
	// after all — the object unreadable, the box's harness too old, the
	// seat's budget unable to carry it — starts fresh with Brief.
	Session *SessionContinuation

	// Decided is told how the launch decided its run takes up a session —
	// the turn's kept capture, the job before's own copy in its box, or none
	// ([SessionDecision]) — once the launch has its box and before the run
	// starts there: the runner arms the run's guard with the skills that
	// session holds, and publishes the prompt it was handed. A launcher calls
	// it once per launch that reaches its run's start, and never for one that
	// does not.
	Decided func(SessionDecision)

	// Started is told which session the job's box started, once its harness
	// first reaches the seat's tools and before any call of the run is made
	// there: the session the launch decided to continue (continued), or one
	// of its own its script started in that one's place, its restore having
	// failed in the box. The box says it by the token its harness dials
	// (internal/api/mcpbridge, started.go), which its model cannot forge. The
	// runner holds the run's guard to that session, and where it is one of
	// its own, publishes the fresh brief the run was handed in the
	// continuation's place ([Runner.sessionStarted]). A launcher calls it
	// after Decided, at most once per opening of the job's tools; a box whose
	// harness never reaches them never says, and its run makes no call.
	Started func(continued bool)

	// Fence is the turn's own fence ([Config.Fence]), which the launch asks
	// again immediately before it opens anything: the prompt built since
	// the phase opened can take a model call of its own, and a seat that
	// moved or was stopped meanwhile must not still buy a box. THE SAME
	// CLOSURE, never one the launcher builds: a fence compares the grant it
	// was built under with the one held when it is called, so it is the
	// turn's admission that the launch is held to. Nil where the runner was
	// built without one.
	Fence func() error
}

// Continuation is how a launch carries on an asking run — see
// [AgentRunRequest.Continue].
type Continuation struct {
	// SessionID is the asking run's harness session; Brief its next prompt —
	// the answer, and the notes [AgentRunRequest.Notes] carries: only those,
	// because the session already holds every note it read before it asked.
	SessionID string
	Brief     string

	// SignInReplaced is a launch carrying on a round whose job was refused
	// on a sign-in replaced while it ran ([Rotated]): the launch records it
	// on the new job, so that job is not carried on again for it.
	SignInReplaced bool

	// ContextTokens is what the last request of that session carried, as
	// its newest capture measured it ([SessionBasis.ContextTokens]): what
	// the relaunch sends again before Brief where it continues the session
	// in its box, which its floor is made of. Zero where no capture measured
	// it, which holds the launch to the static floor.
	ContextTokens int64
}

// SessionDecision is how a launch decided its run takes up a session, told
// before the run starts ([AgentRunRequest.Decided]).
type SessionDecision struct {
	// Continues says the run continues a session rather than starting one
	// of its own; Kept that the session is the one the launch was asked to
	// continue in any box ([AgentRunRequest.Session]) rather than the job
	// before's own copy in its box ([AgentRunRequest.Continue]); and NewBox
	// that the box is not the one the session was last worked in, so the run
	// is handed the continuation that says so ([SessionContinuation.Replaced]).
	Continues, Kept, NewBox bool

	// Base says the session is the one the conversation's last turn left
	// ([SessionBasis.Base]) — always in a new box, and never one whose
	// skills this turn's guard holds, so the guard is armed empty.
	Base bool
}

// SessionContinuation is how a launch continues a session in whatever box it
// gets — the turn's kept capture, or the session its conversation's last turn
// left — see [AgentRunRequest.Session].
type SessionContinuation struct {
	// Basis is the session the launch continues, as the launcher answered it
	// ([SessionProber]); the launch takes up exactly that one.
	Basis SessionBasis

	// Brief is the run's prompt in the box the session was last worked in,
	// and Replaced in any other — Brief and what a box that replaced that
	// one changes (continued.go). A session the conversation's last turn
	// left is continued only in a box of its own, so its Brief is empty.
	Brief, Replaced string
}

// SessionBasis is the kept session a relaunch of the turn continues, or why it
// continues none — a [SessionProber]'s answer.
type SessionBasis struct {
	// LaunchSeq and CaptureSeq are the capture's place among the turn's,
	// zero where the relaunch continues none; SessionID is its harness's
	// session.
	LaunchSeq, CaptureSeq int64
	SessionID             string

	// Harness is the harness that wrote the session, the only one that
	// continues it: a launch hands the continuation on only where the member
	// it picks as it launches runs it, and starts a session of its own where
	// that member runs another.
	Harness string

	// Through and NoteCursor are what the session accounts for, so the brief
	// that continues it carries what came after: its save point — how many of
	// the round's calls the run's log had taken when the capture read it
	// (sandbox.SessionCapture.Through), every call the log took after it
	// ([BridgedCall.Appended] past it) being one the session had not been
	// answered — and the last note read. Zero holds none of the round's calls.
	Through    int
	NoteCursor int

	// Interrupted says the session was captured from a harness that was cut
	// off, so its transcript may end mid-call.
	Interrupted bool

	// Home is where the session was last worked: absolute paths in its
	// transcript are under it.
	Home string

	// ContextTokens is what the last request of the session of the job the
	// relaunch follows carried, as that job's newest capture measured it —
	// kept or not, so set where the relaunch continues no kept capture too:
	// what a launch that continues that session sends first, in a box it is
	// restored into or the job's own, which the launch's floor is made of.
	// Zero where no capture measured any.
	ContextTokens int64

	// Base is the session the conversation's last turn left, which the launch
	// continues where it names no kept capture of the turn's own: on a
	// turn's first launch, the head its dispatch read, and on a relaunch of a
	// turn that has kept nothing yet, the one its first launch froze. Nil
	// where it continues none. SessionID and Harness are then that session's.
	Base *BaseBasis

	// Why is why the launch continues no kept session of the turn's own, ""
	// where it continues one; beside a Base, why the turn's own is not it.
	Why string

	// KeptTurn is the turn whose kept session the probe read, continued or
	// not: the turn's own where a capture of it is kept, else the session
	// its conversation's last turn left where the turn read one — on a
	// relaunch, where the turn's row was read holding it; empty where none
	// is kept, the probe read nothing, or what it read may be gone. A run
	// that starts a session of its own beside it is told it may read it
	// ([prompts.KeptSession]), which its model cannot otherwise know — a
	// round lost a second time too, which continues none.
	KeptTurn string
}

// Continues reports whether the basis names a kept session of the turn's own to
// continue.
func (b SessionBasis) Continues() bool { return b.CaptureSeq > 0 }

// ContinuesBase reports whether the basis names, in place of a kept session of
// the turn's own, the session its conversation's last turn left.
func (b SessionBasis) ContinuesBase() bool { return b.CaptureSeq == 0 && b.Base != nil }

// BaseBasis is the session a conversation's last turn left, as a launch that
// continues it tells its run: the ended turn it is, where that session was last
// worked, whether its harness was cut off when it was saved, what its last
// request carried — which the launch's floor is made of — and when that turn
// ended, which is where the new turn begins; where that turn's end said it went
// on past the session (WentOn), when the session was kept, which is where the
// session above ends instead; and whether the turn was lost (Lost), so the
// session may not hold the last of what it did.
type BaseBasis struct {
	Turn          string
	Home          string
	Interrupted   bool
	ContextTokens int64
	EndedAt       time.Time
	CapturedAt    time.Time
	WentOn        bool
	Lost          bool
}

// SessionProber is an [AgentLauncher] that says, before a launch writes its
// run's briefs, which session the launch may continue: on a relaunch, the kept
// capture of the job it follows where its harness and format let a runner the
// launch can pick restore it; else, and on a turn's first launch, the session
// the conversation's last turn left, where the turn froze one and has kept none
// of its own; or why neither. Which member the launch picks, and whether the
// seat's budget can carry the session, are the launch's to decide, once the
// briefs exist.
//
// ASKED FIRST, by every launch, because what the run is told depends on it: a
// continued session is told what it lacks rather than everything. OPTIONAL: a
// launcher without it continues no session, which every launch then starts
// fresh, or in place.
type SessionProber interface {
	ProbeSession(ctx context.Context) SessionBasis
}

// executeAsAgentRun launches the executor as a detached agentic run.
//
// It builds the surface and the prompt, hands them to the launcher, and
// parks. The state it records carries no conversation — see [execstate.State]
// — but everything a resume needs: the round, the ledger, the surface it may
// still use, and the skills it had loaded.
//
// A LAUNCH THAT DOES NOT HAPPEN STILL CLOSES ITS PHASE. The started record is
// published before the launch, carrying the fresh brief — the prompt a run
// that starts a session of its own is sent, and where a relaunch continues the
// session of the job it follows, the brief its launch hands it follows as the
// phase's opening frame once the launch has chosen it ([Runner.sessionDecided]);
// a launch refused after that publishes the phase's completion too — failed,
// or stopped where a person stopped it — or the phase reads as still running
// for ever on every screen that draws it.
func (r *Runner) executeAsAgentRun(ctx context.Context, round int, notes string,
	history []ledger.Iteration,
) (_ turn.Work, _ turn.Surface, err error) {
	defer origin.Stamp(&err)
	snapshot := r.executorUniverse(r.seatSnapshot())

	// The submission tool is on the surface because it is how the executor
	// says what it did. The harness calls it over the bridge, and the value it
	// captures here is read only when the resume happens in THIS process —
	// otherwise the resume rebuilds it from the run's durable bridged-call
	// log.
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		keptWhole(decodeWork(r.cfg.Reply,
			func() []ledger.Call { return turn.Record(history, calls(surface)) },
			func() turn.Surface { return describe(surface) })))

	// ITS OWN METER: every call this surface serves is the harness's, made
	// over the bridge after this segment has suspended and charged what it
	// spent — see [BridgedSpend].
	bridged := NewBridgedSpend()

	// THE NOTES THIS BRIEF IS THE FIRST TO READ, after every one the turn
	// read before — and the launch's from here: handed back on every way out
	// that reaches no run's record ([Runner.steerReturn]), and recorded
	// delivered only once the launch has opened its job ([Runner.steerBriefed]).
	fresh := r.steerBrief(ctx)
	user := r.executorBrief(ctx, notes, history, fresh)

	// EVERY LAUNCH ASKS WHICH SESSION IT CONTINUES (continued.go): a
	// relaunch — a reviewer's send-back, in a segment that collected the
	// turn's last run — the session of the job it follows where its launcher
	// says it can, else the one the conversation's last turn left where the
	// turn has kept none; and a turn's first launch the one its conversation's
	// last turn left. Asked before the continuation is written, which is built
	// from what that session lacks. THE SURFACE'S GUARD STARTS EMPTY either
	// way: it holds what the session the launch decides its run takes up
	// holds, and the launch decides that once it has its box
	// ([Runner.sessionDecided]).
	basis := r.sessionBasis(ctx)
	built, err := r.surfaceWith(ctx, r.meterFor(bridged), phase.Execute, round, snapshot, submit,
		executorActive(snapshot, r.boxServers()))
	if err != nil {
		r.steerReturn(ctx, Launched{}, fresh)
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built
	briefs := launchBriefs{fresh: user, freshReported: true}
	switch {
	case basis.Continues() && r.cfg.Resume != nil:
		// THE COLLECTED ROUND'S CALLS, the ones its log dropped from the
		// middle among them: the session continued is that round's job's.
		collected := roundCalls{bridged: r.cfg.Resume.Bridged, elided: r.cfg.Resume.Elided,
			absorbed: r.cfg.Resume.Absorbed}
		briefs.kept = r.keptSession(ctx, basis, collected, built, fresh, "").led(r.sendBackLead(notes))
		// THE COLLECTED JOB'S STATE AS ITS SESSION HOLDS IT ([Resume.parked]):
		// the session this continues is that job's, and holds the skills its
		// launch armed only where its box said it took them up.
		briefs.state, briefs.bridged = r.cfg.Resume.parked(), r.cfg.Resume.Bridged
	case basis.ContinuesBase():
		// THE SESSION THE CONVERSATION'S LAST TURN LEFT, told where this turn
		// begins and then handed this launch's own fresh brief — the turn's
		// task, its earlier rounds and notes, and how the conversation's
		// earlier turns ended — which that session never read.
		briefs.kept = r.baseSession(basis, user.Text)
	}
	// AND THE FRESH BRIEF NAMES THE KEPT SESSION IT DOES NOT CONTINUE, after
	// the base's continuation took it whole: a run that starts a session of
	// its own cannot otherwise know one it could read is there.
	user = r.namingKeptSession(user, basis, built)
	briefs.fresh = user
	system := r.executorSystem(built)
	// ITS TOOL SKILLS, handed to its harness as the harness's own rather
	// than listed in its prompt (handedskills.go), matched against the
	// surface it is launched with.
	handed := r.handSkills(ctx, built)
	// AND ASKED AGAIN once the brief exists, before anything is on the
	// record: assembling it rewrites the prior rounds' payloads past their
	// budgets with the seat's auxiliary model — and the calls after a
	// continued session's save ([Runner.keptSession]) — charged to the same
	// counters, and the rewrite can be what fills the window. Asked only at
	// the top of the phase, that run was launched and paid for whole past the
	// ceiling.
	if refused := toolloop.Refusal(ctx, r.cfg.Budget); refused != nil {
		r.steerReturn(ctx, Launched{}, fresh)
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, refused)
	}
	// THE PROMPT REACHES THE RECORD, published here because nothing else
	// will: the collection's own record deliberately carries no prompt (it
	// collects a run rather than opening a conversation). Without this an
	// operator reading a turn sees a run that finished and no sign of what
	// it was asked.
	//
	// The surface travels too, whose definitions are NOT in the prompt
	// below: they reach the harness over the MCP bridge, where the harness's own
	// model is billed for every one of them. See [emitter.promptSize].
	//
	// ONE ROUND OF ONE, the cap the collected record states too: the
	// engine's whole exchange with the run is this launch and its answer,
	// and the harness's own turns inside it are bounded by the run's turn cap
	// (`max_turns`) rather than by anything the engine states here. See
	// [agentRunResult].
	emit := r.emitter()
	began := time.Now()
	caps := roundCaps{max: runRound, ceiling: runRound}
	emit.started(ctx, phase.Execute, round, system, user, surface, caps)
	decided := &launchDecided{}

	launched, err := r.cfg.Launcher.LaunchExecutor(ctx, AgentRunRequest{
		System:     system.Text,
		Brief:      user.Text,
		Notes:      fresh,
		Session:    briefs.kept,
		Decided:    r.sessionDecided(ctx, emit, round, system, surface, caps, briefs, decided),
		Started:    r.sessionStarted(ctx, emit, round, system, surface, caps, briefs, decided),
		Surface:    surface,
		Skills:     handed,
		Spend:      bridged,
		Round:      round,
		OnFallback: func(f chain.Fallback) { emit.fallback(ctx, phase.Execute, round, f) },
		Fence:      r.cfg.Fence,
	})
	if err != nil {
		r.steerReturn(ctx, launched, fresh)
		err = fmt.Errorf("runner: %s: launching the executor's run: %w", phase.Execute, err)
		// A PHASE A PERSON STOPPED DID NOT FAIL, exactly as for every phase
		// the engine drives: it carries the stop as its error and is not
		// marked failed, since a turn somebody deliberately ended is not one
		// that broke.
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: round, System: system, User: user,
			Available: surface.Active(),
			Elapsed:   time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: !turn.Stopped(err), Err: err,
		})
		return turn.Work{}, describe(surface), err
	}
	r.steerBriefed(ctx, round, launched.LaunchID, fresh)

	r.recordAgentSuspension(round, surface, history, began, time.Since(began), handed, launched.BridgeFrom)
	return turn.Work{Calls: calls(surface), Suspended: true}, describe(surface), nil
}

// continueAgentRun carries on a run that parked on a person's question, now
// answered ([Asked]), whose job was lost with its host ([Lost]), or which was
// refused on a sign-in replaced while it ran ([Rotated]): the SAME round,
// relaunched rather than collected — continuing the session of the job it
// follows, from that job's kept capture in whatever box the launch gets, or
// as the job's own box holds it, and as a fresh run told what happened where
// the session can be continued neither way. The launch decides which, once it
// knows which box it has.
//
// A ROUND THAT MAY NOT BE CARRIED ON AGAIN ([Lost.Limit]) is not relaunched:
// its phase is closed here as failed, saying why, and the turn ends with it.
//
// THE ROUND'S SURFACE AND CLOCK CARRY ON. The surface is the job's as it left
// it — the tools its launch had active and what its bridged calls did to it
// since — and its calls so far are seeded into the turn's call log through
// that surface before the relaunch, so the relaunched run's writes go on
// counting from them: the one rebuild every segment that re-enters a run makes
// ([Runner.rebuiltSurface]). ITS GUARD IS THE RELAUNCH'S: it holds what the
// session the launch decides its run takes up holds — the job's own, which
// loaded what it did, or none — and is empty from the relaunch's last step
// before its launch until the launch decides ([Runner.sessionDecided]); the
// call a person approved is made before that, under the asking session's
// guard as the rebuild armed it. The round's launch time keeps accumulating from
// the instant the round first launched. No new phase is opened: the round's
// started record was published when it first launched, the prompt this
// relaunch hands its run is published as the phase's opening frame once the
// launch has chosen it, and the round's completion is the collection that
// finally closes it — unless this relaunch is refused, which closes it here.
func (r *Runner) continueAgentRun(ctx context.Context, history []ledger.Iteration,
	state execstate.State, carry carried, bridgedCalls []BridgedCall,
) (_ turn.Work, _ turn.Surface, err error) {
	defer origin.Stamp(&err)
	began := time.Now()
	logged := bridgedLedger(bridgedCalls)
	elided, absorbed := r.cfg.Resume.Elided, r.cfg.Resume.Absorbed
	snapshot := r.executorUniverse(r.seatSnapshot())
	var surface *tools.Surface
	// A SUBMISSION CITES THE CARRIED RUN'S CALLS TOO: the relaunched run is
	// the same round, and the collection judges its submission against
	// every call the round made ([Runner.resumeAgentRun]) — so the check it
	// gets as it submits reads them as well, its record's dropped middle
	// included, or a delivery made before the question, or before the loss,
	// would be refused here and accepted there.
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		keptWhole(decodeWork(r.cfg.Reply,
			func() []ledger.Call {
				carried := append(slices.Clone(logged), ledger.Called(elided)...)
				return turn.Record(history, append(carried, calls(surface)...))
			},
			func() turn.Surface { return describe(surface) })))
	bridged := NewBridgedSpend()
	// THE TURN'S CALL LOG STARTS FROM WHAT THE TURN ALREADY CALLED, the
	// carried run's calls included, as the surface is rebuilt and so BEFORE
	// the launch: the relaunched run's bridged calls are admitted against
	// this log from its first one.
	round := roundCalls{bridged: bridgedCalls, elided: elided, absorbed: absorbed}
	built, err := r.rebuiltSurface(ctx, r.meterFor(bridged), state, snapshot, submit, history, round)
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	emit := r.emitter()
	caps := roundCaps{max: runRound, ceiling: runRound}
	if refused := carry.refusal(); refused != nil {
		// THE ROUND ENDS HERE, and its phase with it: it was opened when the
		// round first launched, and nothing else will close it now.
		refusedErr := fmt.Errorf("runner: %s: %w", phase.Execute, refused)
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: state.Round,
			Available: surface.Active(),
			Elapsed:   time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: true, Err: refusedErr,
		})
		return turn.Work{Calls: logged, Elided: elided, Absorbed: absorbed}, describe(surface), refusedErr
	}

	// THE NOTES WAITING SINCE THE RUN ASKED, which either brief carries:
	// a fresh run's after every note the turn read, as any brief does; the
	// asking run's own session after the answer alone, since that session
	// already holds every note it read.
	fresh := r.steerBrief(ctx)
	system, user := r.executorPrompt(ctx, "", history, built, fresh)
	// ITS TOOL SKILLS HANDED AGAIN, as every launch hands them: the box's
	// copy is the run's artefact, written afresh at every start, and a
	// session carried on keeps what it loaded, which the rebuilt guard
	// already holds.
	handed := r.handSkills(ctx, built)
	// AND THE SESSION OF THE JOB IT FOLLOWS, CONTINUED in whatever box the
	// launch gets (continued.go), where its launcher says it can and the
	// round may be continued from it at all: a round lost a second time
	// starts fresh rather than replay the state it was lost in ([Lost]) —
	// neither from the job's kept capture nor as its box holds it. ITS BRIEF
	// BUILT HERE, all but its lead ([Runner.keptSession]): telling the session
	// the calls after its save can call the seat's auxiliary model, which the
	// budget asked below must count, and the lead says what the call a person
	// approved returned, which is made only after that.
	// THE PROBE IS ASKED OF EVERY RELAUNCH, a round lost a second time
	// included: it continues nothing, but the session the probe read is the
	// only record of what the round's jobs did in their boxes, and its run is
	// told it may read it ([Runner.namingKeptSession]).
	basis := r.sessionBasis(ctx)
	var kept *keptBrief
	if carry.keeps() {
		kept = r.keptSession(ctx, basis, round, built, fresh, carry.branch())
	}
	// ASKED ONCE EVERY BRIEF'S REWRITES ARE SPENT, as a launch asks it
	// ([Runner.executeAsAgentRun]): the prior rounds' payloads and the calls
	// after the session's save, each past its budget, are rewritten by the
	// seat's auxiliary model, charged to the same counters, and the rewrite
	// can be what fills the window.
	if refused := toolloop.Refusal(ctx, r.cfg.Budget); refused != nil {
		r.steerReturn(ctx, Launched{}, fresh)
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, refused)
	}
	// THE CALL A PERSON APPROVED IS MADE HERE, once, through the surface the
	// run carries on with, as the last step before the relaunch: after every
	// check that could refuse the relaunch, so a round refused here never
	// made it, and before the prompt, which tells the run what it returned
	// (ADR-0077).
	//
	// UNDER THE GUARD OF THE SESSION THAT ASKED IT, as the rebuild armed it:
	// the job's own loads, and the skills its launch armed where its box said
	// it took that session up ([Resume.parked]). The surface still asks the
	// guard of an approved call, and a guard emptied for the relaunch first
	// refused a skill-covered call the person approved, recorded the refusal as
	// the call's result, and so used up the approval. A guard that refuses it
	// all the same — a skill made required while the call waited, or one the
	// rebuild cannot hold for want of the box's word — is asked before the call
	// is marked, which leaves the approval unused ([makeHeld]). And only of a
	// call no attempt has marked: one an earlier attempt at the answer made, and
	// whose relaunch was refused after it, is answered from its mark, since the
	// guard decides whether a call nobody made may be made now, never whether
	// one was.
	if asked, ok := carry.(*Asked); ok && asked.Held != nil {
		if heldErr := makeHeld(ctx, surface, asked.Held); heldErr != nil {
			r.steerReturn(ctx, Launched{}, fresh)
			return turn.Work{Calls: logged, Inherited: len(logged), Elided: elided, Absorbed: absorbed},
				describe(surface),
				fmt.Errorf("runner: %s: %s: %w", phase.Execute, carry.what(), heldErr)
		}
	}
	// THE GUARD IS THE RELAUNCH'S FROM HERE, empty until its launch decides
	// which session its run takes up ([Runner.sessionDecided]): the rebuild
	// armed it as the job before left it, which is what a run that continues
	// that job's session holds and a fresh one does not. Before the launch,
	// so the relaunched run's first call meets the guard of the session it
	// runs in, never the asking one's.
	r.armSession(built, nil, nil)
	// UNDER THE TURN'S OWN FENCE TOKEN, which the relaunched run's tools fence
	// what they read with: a reply carried on is somebody's words, fenced as
	// the message that woke the turn is ([Asked.Continuation]).
	fence := r.cfg.Turn.Context.FenceBoundary()
	continued := carry.continuation(fence)
	if rendered := renderedNotes(fresh); len(rendered) > 0 {
		continued += "\n\n" + strings.Join(rendered, "\n\n")
	}
	// A FRESH RUN'S BRIEF is the round's own, and what became of the run
	// before it — what it asked and was told, or how it was cut off: it
	// remembers neither.
	reseed := prompts.Prompt{Text: user.Text + "\n\n" + carry.reseed(fence)}
	// AND THE SESSION OF THE JOB IT FOLLOWS, as its launcher answered it
	// above, led by what carries it on: what cut the run off, or the answer
	// to its question and what the call the person approved returned, now
	// that it is made.
	inPlace := carry.session()
	briefs := launchBriefs{fresh: reseed, state: state, bridged: bridgedCalls}
	if carry.keeps() {
		switch {
		case basis.Continues():
			briefs.kept = kept.led(carry.continuation(fence))
		case basis.ContinuesBase():
			// A TURN THAT HAS KEPT NO SESSION OF ITS OWN — its job lost with
			// its box before anything of it was kept — continues the one its
			// first launch froze, handed the reseed, which says what the
			// round's jobs did and what became of the last.
			briefs.kept = r.baseSession(basis, reseed.Text)
		}
	} else {
		// A ROUND LOST A SECOND TIME CONTINUES NOTHING, but the session its
		// probe read is still kept and still the only record of what the
		// round's jobs did in their boxes: its run is told it may read it,
		// and nothing else the probe answered reaches the launch.
		basis, inPlace = SessionBasis{KeptTurn: basis.KeptTurn}, ""
	}
	// AND THE RESEED NAMES THE KEPT SESSION IT DOES NOT CONTINUE, as a fresh
	// launch's brief does.
	reseed = r.namingKeptSession(reseed, basis, built)
	briefs.fresh = reseed
	if inPlace != "" {
		briefs.inPlace = continued
	}
	decided := &launchDecided{}
	launched, err := r.cfg.Launcher.LaunchExecutor(ctx, AgentRunRequest{
		System: system.Text,
		Brief:  reseed.Text,
		Notes:  fresh,
		Continue: &Continuation{
			SessionID: inPlace, Brief: continued,
			SignInReplaced: isRotated(carry),
			// WHAT THAT SESSION SENDS FIRST, as its newest capture measured
			// it whether the turn kept it or not: the floor of a launch
			// that continues it in its box.
			ContextTokens: basis.ContextTokens,
		},
		Session:    briefs.kept,
		Decided:    r.sessionDecided(ctx, emit, state.Round, system, surface, caps, briefs, decided),
		Started:    r.sessionStarted(ctx, emit, state.Round, system, surface, caps, briefs, decided),
		Surface:    surface,
		Skills:     handed,
		Spend:      bridged,
		Round:      state.Round,
		OnFallback: func(f chain.Fallback) { emit.fallback(ctx, phase.Execute, state.Round, f) },
		Fence:      r.cfg.Fence,
	})
	if err != nil {
		r.steerReturn(ctx, launched, fresh)
		err = fmt.Errorf("runner: %s: %s: %w", phase.Execute, carry.what(), err)
		emit.completed(ctx, phaseRecord{
			Phase: phase.Execute, Iteration: state.Round, System: system, User: user,
			Available: surface.Active(),
			Elapsed:   time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began),
			StartedAt: began.UTC(), Caps: caps,
			Failed: !turn.Stopped(err), Err: err,
		})
		// The asking run's calls, an earlier attempt's: see below.
		return turn.Work{Calls: logged, Inherited: len(logged), Elided: elided, Absorbed: absorbed},
			describe(surface), err
	}
	r.steerBriefed(ctx, state.Round, launched.LaunchID, fresh)
	launchedAt := state.LaunchedAt
	if launchedAt.IsZero() {
		launchedAt = began
	}
	r.recordAgentSuspension(state.Round, surface, history, launchedAt,
		time.Duration(state.LaunchMS)*time.Millisecond+time.Since(began), handed, launched.BridgeFrom)
	// THE ROUND'S CALLS SO FAR, the run's before it asked or was cut off:
	// they happened and stay on the round's record, as an EARLIER ATTEMPT's
	// ([turn.Work.Inherited]) — the job before made them, and a retry of this
	// continuation relaunches the run rather than making them again.
	return turn.Work{
		Calls: logged, Inherited: len(logged), Elided: elided, Absorbed: absorbed, Suspended: true,
	}, describe(surface), nil
}

// recordAgentSuspension captures what the run's collection needs.
func (r *Runner) recordAgentSuspension(round int, surface *tools.Surface, history []ledger.Iteration,
	began time.Time, launched time.Duration, handed HandedSkills, bridgeFrom int,
) {
	state := execstate.State{
		Version:     execstate.Version,
		ActiveTools: surface.Active(),
		// THE SKILLS THE SESSION THE LAUNCH DECIDED ON HOLDS, as its guard
		// was armed ([Runner.sessionDecided]), and the call the job's own
		// calls are numbered after: every rebuild of the run's guard replays
		// this job's loads on top of these, and no other job's.
		LoadedSkills: r.loadedSkills(),
		BridgeFrom:   bridgeFrom,
		// THE NAMES ITS HARNESS WAS HANDED ITS SKILLS UNDER, which every
		// later opening of its bridge reads a reported load by, and every
		// rebuild of its guard names a refusal by (handedskills.go).
		NativeSkills: handed.Listed(),
		Round:        round,
		// THE LAUNCH'S HALF OF THE PHASE, and where it began, for the
		// record the collection publishes. See [execstate.State.LaunchMS]
		// and [execstate.State.LaunchedAt].
		LaunchMS:   int(launched / time.Millisecond),
		LaunchedAt: began.UTC(),
		Iterations: history,
		Task:       r.cfg.Task,
		// AND WHAT THE TURN STARTED WITH, for the launches after it. See
		// [execstate.State.Context].
		Context: frozen(r.cfg.Context, r.cfg.dispatched()),
	}
	if err := state.Validate(); err != nil {
		// Not recorded, and the absence is what the engine reports: a run
		// whose state cannot be written is one nothing can resume, and it
		// must fail while the launch is still in this process's hands.
		log.Error("agent_run_suspension_invalid", "round", round, "error", err.Error())
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspension = &Suspension{State: state}
}

// ReopenAgentRun rebuilds the tool surface an executor run's box is still
// calling, for a node that has taken the run over while it works — this
// process after a restart, or the seat's next holder — so the run's bridge
// session can be opened again (internal/api/mcpbridge). It returns the
// surface, and the meter the session's calls are counted on.
//
// FROM THE RUN'S OWN STATE, by the one rebuild every segment that re-enters a
// run makes ([Runner.rebuiltSurface]): the surface its launch recorded — its
// active tools and the skills it had loaded — and then what the run did to it
// since, read off the calls it made over the bridge, every tool it activated
// and every skill it loaded. Rebuilt from the launch's record alone, the box
// would find tools it had activated gone and the tools a loaded skill
// unlocked refused again.
//
// AND ITS PLACE IN THE RUN. The turn's call log is seeded with its closed
// rounds and every call the run has made over the bridge — the log's calls, the
// identities of those it does not list (elided) and of those its calls' own
// work made (absorbed) — by identity, in that same rebuild ([seedCalls]), so a
// call made from here counts its
// repeats from the whole run and derives the operation id the run's own
// process would have; a submission cites those calls as well as the ones made
// from here; and the meter starts from what the row says the run's bridged
// calls have cost so far ([NewBridgedSpendFrom]). spent is that total and
// bridged the log's calls, in any order.
//
// AND ITS GUARD, THE SESSION'S ITS BOX STARTED: the skills its launch armed
// only where the job's record says its box took up the session the launch
// continued (continued, [Resume.Continued]), and none but its own loads
// otherwise, a record that says nothing included ([Resume.parked]). A box that
// has not said yet says it on its first request to this opening, which
// [ReopenedRun.Started] is told, before any call there is made.
//
// It launches nothing and parks nothing: the run is going already, and its
// collection resumes the turn as it would have. THE SUBMISSION IT CAPTURES IS
// NEVER READ, exactly as the launch's is not when the run is collected in
// another process: the collection replays submit_work from the run's durable
// log ([Runner.resumeAgentRun]).
func (r *Runner) ReopenAgentRun(ctx context.Context, state execstate.State,
	bridged []BridgedCall, elided, absorbed []ledger.Elided, spent Bridged, continued bool,
) (ReopenedRun, error) {
	history := state.Iterations
	called := append(bridgedLedger(inAdmissionOrder(bridged)), ledger.Called(elided)...)
	held := Resume{State: state, Continued: continued}.parked()

	snapshot := r.executorUniverse(r.seatSnapshot())
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		keptWhole(decodeWork(r.cfg.Reply,
			func() []ledger.Call {
				return turn.Record(history, append(slices.Clone(called), calls(surface)...))
			},
			func() turn.Surface { return describe(surface) })))

	meter := NewBridgedSpendFrom(spent)
	built, err := r.rebuiltSurface(ctx, r.meterFor(meter), held, snapshot, submit, history,
		roundCalls{bridged: inAdmissionOrder(bridged), elided: elided, absorbed: absorbed})
	if err != nil {
		return ReopenedRun{}, err
	}
	surface = built
	own := ownCalls(inAdmissionOrder(bridged), state.BridgeFrom, nil)
	return ReopenedRun{Surface: surface, Meter: meter,
		Started: func(boxContinued bool) {
			var loaded []string
			if boxContinued {
				loaded = state.LoadedSkills
			}
			r.armSession(surface, loaded, own)
		}}, nil
}

// ReopenedRun is an executor run's tool surface as a node that took the run
// over rebuilt it ([Runner.ReopenAgentRun]): the surface, the meter its
// session's calls are counted on, and what its guard is held to once the run's
// box says which session it started.
type ReopenedRun struct {
	Surface *tools.Surface
	Meter   *BridgedSpend

	// Started holds the run's guard to the session its box says it started,
	// on its first request to the opening this node serves, before any call
	// there: the skills its launch armed and the job's own loads where it
	// continued the session its launch chose, the job's own loads alone where
	// it started one of its own (internal/api/mcpbridge, Session.Started). The
	// box's word over the record's, which a box that had not said when the run
	// was taken over left empty.
	Started func(continued bool)
}

// rebuiltSurface is a run's surface rebuilt by a later segment, as the run
// left it, and the turn's call log seeded with the run's place in the turn —
// the one rebuild every segment that re-enters a run makes: the one that
// collects it ([Runner.resumeAgentRun]), one that carries it on
// ([Runner.continueAgentRun]) and one that reopens it while it works
// ([Runner.ReopenAgentRun]).
//
// AS THE RUN LEFT IT: the tools its launch recorded — every tool it can reach,
// so nothing a run does over the bridge adds one — and the skills its guard
// held then, with every skill the run's own job loaded since over the bridge
// read off its calls ([Runner.observeLoads]): those numbered past the calls the
// round held when the job opened ([execstate.State.BridgeFrom]), never an
// earlier job's of the round, whose session the run's may not be. AND ITS
// PLACE IN THE TURN: the closed rounds in history, and the round's calls as the
// log beside its row holds them (round): the calls it lists, in the order the
// bridge admitted them, and the identities of the ones it does not list and of
// the ones their own work made, seeded into the call log through the rebuilt
// surface ([seedCalls]), so a count read from here sees every call made before
// it.
//
// ONE REBUILD FOR ALL THREE, because they drifted apart when each made its
// own: the collection rebuilt the launch's record alone, on the reasoning that
// it makes no gated call. That much is true, and it is beside the point,
// because the collected surface is also what the round's record states the
// run held (tools_available). The guard is replayed too, although nothing
// calls through the collection's surface: a rebuild that replays half of what
// the run did is the drift this rule exists to stop.
func (r *Runner) rebuiltSurface(ctx context.Context, m meter, state execstate.State,
	snapshot tools.Snapshot, submit tools.Callable, history []ledger.Iteration, round roundCalls,
) (_ *tools.Surface, err error) {
	defer origin.Stamp(&err)
	surface, err := r.surfaceWith(ctx, m, phase.Execute, state.Round, snapshot, submit,
		state.ActiveTools, state.LoadedSkills...)
	if err != nil {
		return nil, err
	}
	seedCalls(r.cfg.Turn.Context.CallLog(), surface, history, round)
	r.observeLoads(surface, ownCalls(round.bridged, state.BridgeFrom, nil))
	// AND THE NAMES ITS HARNESS WAS HANDED ITS SKILLS UNDER, so a call the
	// rebuilt guard refuses names the skill as the run's harness lists it.
	r.handedAs(state.NativeSkills)
	return surface, nil
}

// observeLoads tells the guard the surface was just armed with every skill a
// run's bridged calls loaded, by the guard's own reading of a call — the one
// rule for which call unlocks what. Called once the surface is built, because
// whether a guard is armed at all depends on the loader being active. A call is
// read by its
// canonical arguments, made through the surface ([tools.Surface.ArgsOf]), as
// the guard reads every call the surface admits.
func (r *Runner) observeLoads(surface *tools.Surface, called []ledger.Call) {
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	for _, call := range called {
		if !call.Failed {
			guard.Observe(call.Name, surface.ArgsOf(call.Name, call.Args))
		}
	}
}

// dispatched is what a turn's dispatch rendered once for every launch of it:
// how the conversation's earlier turns ended, the seat's access, and what
// changed in it since the seat was last told.
type dispatched struct{ endings, access, changes string }

// dispatched is the config's dispatch-rendered sections.
func (c Config) dispatched() dispatched {
	return dispatched{endings: c.Endings, access: c.Access, changes: c.CapabilityChanges}
}

// frozen is the turn-start context a runner builds every launch's prompt from,
// as a park carries it: the blocks' rendered text and what the dispatch
// rendered, what [Runner.executorPrompt] reads of them and nothing else.
func frozen(blocks prefetch.Blocks, d dispatched) execstate.Frozen {
	return execstate.Frozen{
		ThreadContext:       blocks.ThreadContext,
		PersonalMemory:      blocks.PersonalMemory,
		RelevantKnowledge:   blocks.RelevantKnowledge,
		EpisodeRecall:       blocks.EpisodeRecall,
		CounterpartyProfile: blocks.CounterpartyProfile,
		SynthesizedSkills:   blocks.SynthesizedSkills,
		Endings:             d.endings,
		Access:              d.access,
		CapabilityChanges:   d.changes,
	}
}

// thawed is a parked turn's frozen context back in the shape a runner's
// prompts read: the inverse of [frozen].
func thawed(f execstate.Frozen) (prefetch.Blocks, dispatched) {
	return prefetch.Blocks{
		ThreadContext:       f.ThreadContext,
		PersonalMemory:      f.PersonalMemory,
		RelevantKnowledge:   f.RelevantKnowledge,
		EpisodeRecall:       f.EpisodeRecall,
		CounterpartyProfile: f.CounterpartyProfile,
		SynthesizedSkills:   f.SynthesizedSkills,
	}, dispatched{endings: f.Endings, access: f.Access, changes: f.CapabilityChanges}
}

// resumeAgentRun turns a finished agentic run into the turn's Work.
//
// THE RUN'S OWN TOOL CALLS ARE THE RECORD, replayed from the durable log the
// bridge wrote rather than from the surface: the process that resumes may not
// be the one that launched, so the surface here is rebuilt as the run left it
// ([Runner.rebuiltSurface]) and has served no call of its own. That log is
// also where the submission is: the harness ended its run by calling submit_work
// over the bridge, and replaying that call through a fresh submission tool is
// what recovers the outcome it declared.
//
// An absent submission is NOT a value. A run that stopped without saying what
// it did is rescued as incomplete and judged on its record, rather than having
// a delivery inferred from the prose the harness happened to end with.
//
// `history` is the turn's closed rounds, which its submission's citations read
// beside the run's own calls — see [turn.Record] — and `bridged` the run's
// calls in the order the bridge admitted them ([inAdmissionOrder]).
func (r *Runner) resumeAgentRun(ctx context.Context, history []ledger.Iteration,
	state execstate.State, answer string, bridged []BridgedCall,
) (_ turn.Work, _ turn.Surface, err error) {
	defer origin.Stamp(&err)
	// THIS SEGMENT'S START, which the collection's record states as its
	// started_at: a start read off neither segment would put the record at
	// the epoch, a day before the turn existed. Its DURATION is the
	// phase's — both segments, the launch's carried on the park — and
	// never the run between them, which is each launch's own fact, on its
	// types.ExecutorCollected. See types.AgentPhaseCompleted.
	began := time.Now()
	snapshot := r.executorUniverse(r.seatSnapshot())
	logged := bridgedLedger(bridged)
	elided, absorbed := r.cfg.Resume.Elided, r.cfg.Resume.Absorbed

	// THE REPLAY'S DECODER IS NOT HELD TO WHAT THE LOG KEEPS WHOLE
	// ([keptWhole]): the submission is already made, and one a build before
	// that bound recorded whole is replayed as it stands.
	var surface *tools.Surface
	submit := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(r.cfg.Reply,
			func() []ledger.Call {
				return turn.Record(history, append(slices.Clone(logged), ledger.Called(elided)...))
			},
			func() turn.Surface { return describe(surface) }))

	// THE SURFACE THE RUN LEFT, the tools it activated over the bridge
	// included, which the round's record states as what the run held and
	// what it could still have discovered ([Runner.finishWork]); and the
	// turn's call log, which starts from what the turn already called.
	built, err := r.rebuiltSurface(ctx, r.ownMeter(), state, snapshot, submit, history,
		roundCalls{bridged: bridged, elided: elided, absorbed: absorbed})
	if err != nil {
		return turn.Work{}, turn.Surface{}, err
	}
	surface = built

	replaySubmission(ctx, submit, sinceAnswer(bridged, r.cfg.Resume.Answered))

	run := r.cfg.Resume.Run
	res := agentRunResult(answer, bridged, elided, run, state.LaunchedAt, began)
	// THE NOTES THE ROUND'S RUN READ, off the record every read is written
	// on: in its briefs, and at the boundaries of its harness's own loop — the
	// reads of this round's jobs, at the run's one round, each with where it
	// was read.
	res.RunSteers = runReads(r.cfg.Resume.Steered, state.Round)
	res.StartedAt = began.UTC()
	res.Elapsed = time.Duration(state.LaunchMS)*time.Millisecond + time.Since(began)
	work, described := r.finishWork(ctx, state.Round, work{
		submit:  submit,
		res:     res,
		surface: surface,
		run:     run,
	})
	// The RUN's calls, not this process's surface's — see the doc above —
	// and every one an EARLIER ATTEMPT's ([turn.Work.Inherited]): the run
	// made them before this segment began, and a retry of the collection
	// collects the same run and makes none of them again. They are on the
	// record, for the delivery check and the ledger; what they are not is
	// a write this attempt would repeat. Counted as one, a collection that
	// met its reviewer's usage limit before writing anything was settled
	// for good as a turn that had written, and the run's result never
	// reached the seat.
	work.Calls = logged
	work.Inherited = len(logged)
	// AND THE CALLS THE LOG DID NOT KEEP, by identity: the round's all the
	// same, for the delivery checks, the closed round's record and the
	// reviewer, who is told how many there were — and the ones the round's
	// own calls' work made, which the closed round's record carries for the
	// next segment's call log and nothing else reads.
	work.Elided, work.Absorbed = elided, absorbed
	return work, described, nil
}

// agentRunResult turns what the run actually did into the phase's record.
//
// This used to hand `finishWork` a zero [toolloop.Result] while holding the
// whole record in `bridged`, so the published event carried no response, no
// tool calls, no tokens, no rounds and no model. The card rendered EMPTY: it
// draws a phase from its rounds, a record with nothing in its ledger has none
// to draw, and the "composing its first round" placeholder is gated on a phase
// being live, which a completed one is not. An operator
// saw a decision word and a blank card for a run that made real tool calls.
//
// ONE ROUND, and that is not a stand-in for a number the engine failed to
// count — it is the honest one. A round is an iteration of the ENGINE's
// exchange with a model: one request, one answer. For the executor the engine
// made a single request — the launch — and got a single answer back, however
// many turns the harness took inside its own loop to produce it. The bridge's call
// log carries no round of its own and inventing one per call would claim a
// structure nobody observed, splitting one run into thirty rounds of one call
// each. So the record's cap is that one round too: the engine allowed the
// run one request, and its turns are bounded by the harness's own cap
// (`max_turns`), which is the run's fact rather than this phase's.
//
// THE ROUND IS TIMED, from the launch (launchedAt, the phase's first instant)
// to the collection (answered, when the run's answer came back), so a
// timeline draws the run as the span the phase waited on and places each
// bridged call inside it at the instant the bridge measured. Two clocks when
// the launch and the collection ran on different nodes, which is why a skew
// that would make the span negative states none. It carries NO TOKENS: what
// the run spent is each launch's own fact, on the `executor_collected` record
// its collection publishes, and repeating it here would charge every rollup
// twice.
//
// EACH CALL WITH ITS TIMING AND WHO ANSWERED IT, as the bridge recorded them,
// in the order the run made them: the bridge is where a run's calls are
// measured, since they ran inside the harness's own loop and not one of the
// engine's.
//
// THE MEMBER THAT RAN IT is the record's entry and model: the launch picked it
// from the seat's executor chain, so the chain's head is not an answer, and a
// record naming nobody files the run's spend under no provider.
//
// AND THE CALLS ITS RECORD DID NOT KEEP (elided): counted in its round's calls,
// and said by tool beside the ones it lists ([phaseResult.RunElided]) — a long
// run's record keeps their head and tail, and the phase's record, which
// outlives the run's, would otherwise read the kept ones as all it did.
func agentRunResult(answer string, bridged []BridgedCall, elided []ledger.Elided, run RunRecord,
	launchedAt, answered time.Time,
) phaseResult {
	res := toolloop.Result{
		Text: answer, RoundsUsed: runRound, MaxRounds: runRound,
		Model: run.Model, ProviderKey: run.ProviderKey,
	}
	if !launchedAt.IsZero() {
		res.Rounds = []toolloop.Round{{
			Round: runRound, StartedAt: launchedAt.UTC(),
			Duration: max(answered.Sub(launchedAt), 0),
			Model:    run.Model, ToolCalls: len(bridged) + ledger.Count(elided),
		}}
	}
	for _, call := range bridged {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: runRound, Name: call.Name, Args: call.Args,
			Output: call.Result, Failed: call.Failed,
			StartedAt: call.StartedAt, Duration: call.Duration,
			Origin: call.Origin, Server: call.Server,
			Source: call.Source, Refusal: call.Refusal,
			// WHICH OF THE TWO CALLS WHOSE ARGUMENTS ARE SEALED, so the record
			// says so in their place rather than drawing a call made with none.
			Held: call.Held, Approved: call.Approved,
		})
	}
	if strings.TrimSpace(answer) != "" {
		// The run's own last word, on the round its calls are on, so the
		// ledger reads as one block rather than as prose beside orphaned
		// tool rows. There is no separate reasoning to report: a harness's
		// thinking stays inside its own loop.
		res.Narration = []toolloop.Narration{{Round: runRound, Content: answer}}
	}
	return phaseResult{Text: answer, Rounds: runRound, Result: res,
		Caps: roundCaps{max: runRound, ceiling: runRound}, RunElided: elided}
}

// runReads is the notes the jobs of a collected round read, in the order read,
// as its execute record lists them: at the run's one round ([runRound]), each
// with the boundary that read it. Nil for a round whose jobs read none.
//
// THE ROUND'S JOBS, NOT THE COLLECTED JOB'S ALONE, because the record is the
// round's: a job carried on with a person's answer continues the round of the
// job that asked, and the record lists the asking job's calls and then its own
// as one order (sandbox.OpenedJob.LastSeq). Keyed on the collected job, the
// record drew the asking job's calls and none of the notes it read between
// them, which were delivered all the same. A read of the round's execute phase
// that names a job is one of those jobs': a reviewer's reads are review's, the
// first segment's name no job, and an earlier round's run has a record of its
// own.
func runReads(reads []steer.Read, iteration int) []types.PhaseSteer {
	var out []types.PhaseSteer
	for _, read := range reads {
		if read.Phase != types.PhaseExecute || read.Iteration != iteration || read.Launch == "" {
			continue
		}
		out = append(out, types.PhaseSteer{Round: runRound, NoteID: read.ID, Boundary: read.Boundary})
	}
	return out
}

// runRound is the one round an executor run's record has, which every call
// the run makes is recorded on — see [agentRunResult] for why there is one.
// The workers a run's delegate call spawns nest under it too, so they sit
// under the call that spawned them.
const runRound = 1

// sinceAnswer is the calls of a round made after a person's answer last
// carried it on: those placed after `answered` ([Resume.Answered]), or every
// call where no answer has.
//
// ONLY WHAT THE RUN DID KNOWING THE ANSWER. A run carried on by a person's
// answer keeps the calls it made before the question (ADR-0068), and a
// `submit_work` among them was made without the answer: replayed, it would
// report the carried-on run as the work submitted before it, whether or not
// the run went on to submit again. The cut is the place the row recorded when
// the continuing job opened, never a search for the round's last ask: the log
// is bounded and drops from the middle (sandbox.MaxBridgeCalls), so the ask
// may be gone from it, and the asking job may have called `submit_work` after
// its question as well as before it. A call with no place (zero) is not after
// any answer.
func sinceAnswer(bridged []BridgedCall, answered int) []BridgedCall {
	if answered <= 0 {
		return bridged
	}
	out := make([]BridgedCall, 0, len(bridged))
	for _, call := range bridged {
		if call.Seq > answered {
			out = append(out, call)
		}
	}
	return out
}

// inAdmissionOrder is a run's bridged calls in the order the bridge admitted
// them — the order the harness made them in — where the log holds them in the
// order they FINISHED: each is appended once it returns, and a harness that makes
// several calls at once finishes them in whatever order their tools take. The
// record states the order the run acted in, and the replay below reads a
// submission made after another as the correction, whichever returned first.
// Stable, so calls the log carries no place for keep the log's order.
func inAdmissionOrder(bridged []BridgedCall) []BridgedCall {
	out := slices.Clone(bridged)
	slices.SortStableFunc(out, func(a, b BridgedCall) int { return cmp.Compare(a.Seq, b.Seq) })
	return out
}

// replaySubmission feeds the run's own submit_work call back through a fresh
// submission tool, so the outcome the harness declared is the one the turn reports.
//
// LAST WRITE WINS, which is the submission contract everywhere else: a second
// call is a correction, and replaying in the order the run made its calls is
// what makes that true here too. A call the decoder rejects is skipped rather than failing the resume —
// the run happened, its other calls happened, and a malformed submission is
// exactly the "said nothing" case the rescue path exists for.
//
// The caller hands it the calls a submission may come from: the round's since
// a person's answer last carried it on ([sinceAnswer]), in the order the
// bridge admitted them.
//
// A SUBMISSION ITS RECORD DID NOT KEEP WHOLE IS NEVER REPLAYED, and where it is
// the newest, nothing is: the round reads as one that submitted nothing, which
// it is rescued from as incomplete. Replayed, the record's marker would be the
// round's summary — a cut that reads as the whole — and an older submission
// replayed in its place would report an outcome the run had corrected. The
// log keeps the newest successful submission whole (sandbox's
// MaxSubmissionArgsBytes), and the live tool refuses one past what it keeps
// ([keptWhole]), so this is a run whose own spelling took a submission the
// tool accepted past what the log holds.
func replaySubmission(ctx context.Context, submit *structured.Tool[workPayload], bridged []BridgedCall) {
	newest := -1
	for i, call := range bridged {
		if call.Name == SubmitWorkTool && !call.Failed {
			newest = i
		}
	}
	if newest >= 0 && bridged[newest].ArgsBytes > 0 {
		log.WarnContext(ctx, "agent_run_submission_not_kept", "seq", bridged[newest].Seq,
			"args_bytes", bridged[newest].ArgsBytes,
			"detail", "the run's newest submission is not on its record whole, so it is not replayed "+
				"and the round is collected as one that submitted nothing")
		return
	}
	for _, call := range bridged {
		if call.Name != SubmitWorkTool || call.Failed || call.ArgsBytes > 0 {
			continue
		}
		if _, _, err := tools.Invoke(ctx, tools.Invocation{Name: SubmitWorkTool, Tool: submit,
			Args: tools.ArgsOf(submit, false, call.Args), Where: "replay"}); err != nil {
			log.WarnContext(ctx, "agent_run_submission_replay_failed", "error", err.Error())
		}
	}
}
