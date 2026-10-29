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
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
	"github.com/crewlet/crewlet/internal/tracing"
)

var log = logging.Get("agent.runner")

// Caps are the round-cap extension's switch and step, for the one phase it can
// reach: onboarding, the only engine-driven phase with a ceiling above its
// budget ([Onboarding.Ceiling]).
//
// The executor has no round knob here at all: it is its coding CLI's own run,
// bounded by the run's own turn cap (role.sandbox.max_turns, else
// providers.sandbox.default_max_turns) rather than by rounds of an engine
// loop. The reviewer has none either: it holds one submission tool, so its
// budget is one round plus the tool loop's own corrective retries — a
// structural fact rather than an operator preference.
type Caps struct {
	ExtensionStep int
	ExtensionOn   bool
}

// reviewRounds is the reviewer's whole budget: one submission, the tool loop's
// two finishing correctives when a model answers without calling it, and one
// spare.
//
// The correctives are armed by the submission the reviewer names as its
// terminator, as they are for every phase that finishes by a call, and they
// are what a reviewer that thought and stopped gets INSTEAD of the rescue —
// which sends the whole turn back for another executor round over the one
// failure a model fixes when it is simply asked again.
const reviewRounds = 4

// Config is everything a runner needs that does not change between rounds.
type Config struct {
	Seat     prompts.Seat
	Registry *tools.Registry
	Models   *phase.Registry
	Caps     Caps

	// Judge decides round-cap extensions. Nil means every exhaustion goes
	// straight to the rescue path.
	Judge extension.Judge

	// Compact rewrites a payload past its budget with the seat's own
	// auxiliary model: a prior round's argument values, failed calls'
	// errors and produced text, as the prior-work ledger renders them. The
	// zero value rewrites nothing, and the ledger then renders a draft
	// whole and omits a payload by its size — never a cut. See
	// internal/agent/ledger/ledgerfit for which is which.
	Compact compact.Bound

	// Subagent is what this turn needs to spawn sub-agents: the company's
	// caps and a way to read the seat's remaining allowance. Nil leaves
	// spawn_subagent off every surface, which is the honest shape for a
	// build with no budget source — and was the shape of every build,
	// because nothing imported internal/agent/subagent at all.
	Subagent *SubagentConfig

	// Budget is the shared token counter a turn charges. Nil disables the
	// per-round charge, which is the embedded single-node case where no
	// counter is shared with anyone.
	Budget toolloop.BudgetMeter

	// Metrics is the node's recorder, which every surface of the turn
	// counts into: its refusal-loop decisions
	// ([metrics.ToolPolicyRefusalLoops]) and the calls answered outside
	// their schema ([metrics.ToolArgsOutsideSchema]), the executor run's
	// bridged calls included, since its surface is built here like every
	// other phase's. Nil counts nothing.
	Metrics *metrics.Recorder

	// Sessions is where every phase the engine drives serves its tools to
	// its CLI — this process's one [toolsession.Host], whose directory is
	// what admits a CLI's relay. Required: a phase that offers tools reaches
	// its CLI through nothing else. The executor's run is the one exception,
	// reaching the seat's tools over the tool bridge from its box.
	Sessions *toolsession.Host

	// Fence stops a turn whose seat this node no longer holds, or one a
	// person stopped: checked before the executor's run is launched, and at
	// the top of every round of every phase the engine drives, before any
	// tokens are spent.
	//
	// It answers the question [internal/seat.Host.MayStart] deliberately
	// does not: admission proves the seat was held when the turn STARTED,
	// and a turn outlives its admission. A node that loses a seat mid-turn
	// keeps calling models, keeps calling tools and keeps publishing
	// records for a seat a peer is now running.
	//
	// Nil disables it, which is the single-node case and every test that
	// has no seat host — and it is why this was dead for so long: the
	// field existed on the loop, the loop checked it, and nothing ever
	// supplied one.
	Fence func() error

	// Task is the ask, and Conversation is the prior-turns block. Both are
	// fixed for the turn — Conversation as [Config.Context] is, and empty
	// with [Config.Resume] for the same reason.
	Task         string
	Conversation string

	// Reply says who is waiting for this turn, so the executor's own
	// submission can be checked against a fact the model does not control.
	// See [turn.Reply].
	Reply turn.Reply

	// Context is what this seat remembers, what its company has written
	// down, what it has done before, and who it is talking to — rendered
	// by the caller BEFORE the turn.
	//
	// Strings rather than a seam the runner could pull on, and that is the
	// freeze: the executor is launched again on a self_iterate loop, and a
	// runner able to re-fetch would produce a different system prompt on
	// each launch. A provider caches on an exact prefix, so a prompt that
	// moves costs the whole prompt again every iteration — and the executor
	// would see its own context change underneath work it is mid-way
	// through.
	// There is nowhere here for a second fetch to happen; a turn that wants
	// fresher knowledge asks for it with search_knowledge.
	//
	// EMPTY WITH [Config.Resume]: a collected turn's context is the one its
	// park froze ([execstate.State.Context]), so a later launch in the
	// collecting segment is built from what the turn started with rather
	// than from a second fetch — [New] refuses a resume handed both.
	Context prefetch.Blocks

	// Skills is the company's tool-skill registry. Nil is a company that
	// has published none, and it disarms the required-skill guard too —
	// which is right: with no skills there is nothing to enforce.
	Skills *skills.Registry

	// SkipNames are the meta-tools the ledger filters out.
	SkipNames []string

	// Publisher receives the phase telemetry — see telemetry.go. Nil
	// publishes nothing, which is the right answer for a runner driven
	// directly by a test and for a sub-agent, whose host phase is already
	// the visible one.
	//
	// This is the ONLY source of every agent_* event in the company: no
	// publisher means a seat that renders as idle for the whole of a turn
	// and leaves no durable record it ran.
	Publisher queue.Publisher

	// Turn identifies the turn these events belong to.
	Turn Turn

	// OnPhase is called as each phase OPENS, with the phase that is about to
	// run. Nil is the ordinary case for anything driving a runner directly.
	//
	// It exists for the working indicator, which has to say which phase a
	// seat is in and had no way to learn it: the engine owns the turn's
	// lifetime but not its phases, and a turn's own indicator must not
	// depend on a broker round trip to find out what it is doing.
	//
	// ONE SEAM, wired where agent_phase_started is published (see
	// [emitter.started]) rather than at each phase's own call site, so the
	// name a reader sees and the name every dashboard reads cannot drift:
	// both are the same argument to the same call. It fires whether or not
	// anything is listening on the stream, because an indicator is not
	// telemetry — a node with no publisher still has a person waiting.
	//
	// Called SYNCHRONOUSLY, immediately before the phase's first provider
	// call, so an implementation that blocks delays the turn. The one in the
	// engine only writes state and wakes a goroutine: see
	// [notify.StatusSession.Phase].
	OnPhase func(phase.Phase)

	// Onboarding wires the dedicated first-turn pass. Zero disables it,
	// which is what a node with no marker store has — a pass that could
	// never be marked would run every turn forever.
	Onboarding Onboarding

	// Resume collects the executor run the turn parked on. Non-nil makes
	// this runner's turn a RESUME: its first round collects what the run
	// did rather than launching one. See [Runner.Resume].
	Resume *Resume

	// Launcher starts the executor: every seat's executor is its coding
	// CLI's own agentic run, detached in a box. REQUIRED — [New] refuses a
	// runner without one, because there is no other way to run the
	// executor and a runner that found out at its first Execute would fail
	// a turn on an assembly mistake. See agentrun.go.
	Launcher AgentLauncher

	// Ask is the tool the executor's run asks a person through and parks
	// on (`ask_and_wait`, ADR-0068), offered on that run's surface beside
	// `submit_work`: the two ways the run ends. Nil offers none, which is a
	// runner built outside an engine.
	Ask tools.Callable

	// Steer is where the turn's notes from a person wait
	// (internal/agent/steer): taken at the opening of every run of the
	// REVIEWER — the turn's own conversation the engine drives — and by the
	// brief of every coding run the turn launches. Never a worker's (a leaf
	// its parent directs), the onboarding pass's (orientation, not the work)
	// or the extension judge's (a question about the phase, not part of it):
	// a note waiting while one of those runs is read by the next step of the
	// turn's own.
	//
	// A note read once is carried into every later phase of the turn, as
	// the same message — into the brief of every later run the turn
	// launches, and the opening of every later review — so a correction
	// binds the work done after it and the reviewer that judges that work.
	// See [Runner.steered].
	//
	// Nil is a turn nobody can steer, which is every test and every runner
	// driven directly. The engine hands a turn's first segment its box, and
	// a segment that resumes the turn its run's record.
	Steer SteerSource
}

// SteerSource is where a turn's notes from a person wait, as the runner reads
// them: the first segment's box ([steer.Box]), or the record of the coding run
// a resumed segment collects. Declared here, by the consumer.
type SteerSource interface {
	// Take takes every note waiting, recording it read where at says, and
	// answers what it took — what a run of the engine's loop opens with.
	Take(ctx context.Context, at steer.Reading) []steer.Note

	// Brief is the notes a coding run's brief takes, oldest first. They are
	// the launch's from here: recorded read by the write that opens the
	// run's job, or handed back with [SteerSource.Return] where the launch
	// fails before that write.
	Brief(ctx context.Context) []steer.Note

	// Return hands back notes [SteerSource.Brief] gave a launch that failed
	// BEFORE IT WROTE ITS RUN'S RECORD ([Launched.LaunchID] empty): they
	// reached nobody, and are the source's again — a box keeps them, at its
	// front, for its next reader or reports them expired when it closes; a
	// run's record, which a brief only peeks, still holds them waiting. A
	// launch that failed after the write is never handed back: the record
	// holds its brief's notes read by a job that never ran, and its ending is
	// the one party that reports them, once.
	Return(ctx context.Context, notes []steer.Note)

	// Waiting reports a note waiting, WITHOUT TAKING IT and without
	// blocking: the tool loop asks it under its dispatcher's lock, to end a
	// run once the calls in flight are answered so the next run opens with
	// the note.
	Waiting() bool
}

// Resume is the collection of the executor run a turn parked on.
type Resume struct {
	State execstate.State

	// Answer is the run's closing text: what its CLI said last, which the
	// phase reports as its prose and the reviewer reads as what the agent
	// produced.
	Answer string

	// Bridged is what the run called over the MCP bridge, read from the
	// run's own durable row, in the order the row holds them — each carries
	// its place in the order the bridge admitted them ([BridgedCall.Seq]),
	// which is the order the resume records them in — and no round, which
	// is this package's to state ([runRound]).
	//
	// It is the whole record of the phase, and it has to come from the row
	// rather than from memory: the process resuming may not be the one that
	// launched, so its surface is fresh and has executed nothing.
	Bridged []BridgedCall

	// Run describes the detached run this resume is collecting.
	Run RunRecord

	// Asked is set when a person's answer, not the run's completion, drives
	// this resume: the run parked on a question, and the turn CARRIES IT ON
	// with the answer rather than collecting it — see [Asked]. Answer is
	// then empty, and Bridged holds the calls the run made before it asked.
	Asked *Asked

	// Lost is set when the run's job was LOST with the host it ran on rather
	// than ended: the turn carries the round on rather than collecting it —
	// see [Lost] — unless the round's calls already hold its submission.
	// Answer is then the job's own account of itself, marked cut off, and
	// Bridged holds the round's calls so far.
	Lost *Lost

	// Steered is every note the turn had read when the run was collected,
	// and where, in the order read — off the run's record, which every read
	// is written on: a brief's, the run's own and a reviewer's. The
	// resumed turn opens every phase with them ([Runner.steered]), and the
	// collected run's own reads are its record's ([Runner.resumeAgentRun]).
	Steered []steer.Read
}

// BridgedCall is one call an executor run made over the MCP bridge, as the
// run's durable row recorded it: the call the turn's record reads, and when it
// ran, for how long and who answered — what the phase record's execution for
// it states.
//
// ITS OWN TYPE rather than a [ledger.Call], which is the delivery check's and
// the ledger's view of a call and has nowhere to put a call's place or its
// timing: carried as one, a resumed run's phase record stated every call the
// run made as untimed, unattributed and in the order the calls happened to
// finish.
type BridgedCall struct {
	ledger.Call

	// Seq is the call's place in the order the bridge admitted the run's
	// calls, from 1 — the order the CLI made them in, which is not the order
	// they finished in when it made several at once. One order for the
	// round: a job opened again after a recovery, and a job carrying on one
	// that asked a person ([Asked]), number theirs after the calls before
	// them (sandbox.PendingRun.LastBridgeSeq).
	Seq int

	// StartedAt is when the call was handed to the seat's surface, and
	// Duration how long the surface took to answer — measured at the bridge,
	// since the call ran inside the CLI's own loop. Both zero on a call
	// answered without running — arguments that did not parse — and zero
	// means "not measured", never "instant".
	StartedAt time.Time
	Duration  time.Duration

	// Origin and Server are who answered the call — a builtin, or an MCP
	// tool and the server it came from — and empty where no tool did.
	Origin string
	Server string
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

// RunRecord is what a phase event says about the run an executor was, and the
// box it ran in.
//
// It exists because nothing could say it. Every publisher stamped
// [types.BackendNative] unconditionally, so a phase that spent twenty minutes
// in a remote box and one that ran three rounds in this process were reported
// identically: the sandbox badge could never render, and `coding_agent`,
// `sandbox_id` and `delivered_refs` had no producer at all — despite the
// coding agents reporting every one of them. What the run itself cost is NOT
// here: that is the run's own fact, on the `sandbox` phase record its
// collection publishes, which a run that parked on a question also gets.
//
// Carried rather than re-derived, for the same reason the run's placement is:
// the resume may be another process on another node, days later, under a
// company configuration that has been applied again since — and the member
// of the seat's executor chain that took the launch is the launch's own
// answer, not the chain's head.
type RunRecord struct {
	// CodingAgent is the CLI that did the work; SandboxID the box it ran in.
	CodingAgent string
	SandboxID   string

	// ProviderKey is the providers.llm entry the run was launched on, and
	// Model the model it ran — the member of the seat's executor chain that
	// took the launch. The Execute record names them as the entry and model
	// that served the phase, so spend by provider and a turn's
	// `execute_model` are charged to the member that actually ran rather
	// than to nobody.
	ProviderKey string
	Model       string

	// DeliveredRefs are the branches and pull requests the run produced.
	DeliveredRefs []string

	// RefsElided is how many more the run reported than DeliveredRefs lists,
	// which the coordinator bounds — said on the record rather than dropped.
	RefsElided int

	// LaunchID is the job this resume collected — the pending-run row's
	// [sandbox.PendingRun.LaunchID] when the claim took it. A turn can launch
	// more than once, so it is what tells one collected run's record from
	// the next.
	LaunchID string
}

// Sandboxed reports whether the record names the run's CLI or its box, which
// is what makes its phase's backend a sandbox rather than this process.
func (r RunRecord) Sandboxed() bool { return r.CodingAgent != "" || r.SandboxID != "" }

// Runner implements [turn.Phases] against real models and real tools.
//
// One per turn — see [Company.RunnerFor] on the engine side. That is what
// makes the spend tally below a per-turn fact rather than state to reset.
type Runner struct {
	cfg   Config
	spend Spend

	// mu guards suspension, which the Execute phase writes and the engine
	// reads once the turn returns, and guard, which surfaceWith writes and
	// a park reads back.
	mu         sync.Mutex
	suspension *Suspension

	// guard is the required-skill gate of the phase currently being built
	// or run, or nil where this turn arms none.
	//
	// A field rather than a return value because only ONE caller wants it —
	// a park, which persists the keys the run's surface had loaded so the
	// collection's surface is armed the same way. Threading it through every
	// phase's signature to serve that one caller would put it in four places
	// that ignore it. One phase runs at a time on a runner, so the field is
	// that phase's.
	guard *skills.Guard

	// steered is every person's note this turn has read, and where, in the
	// order it was read — opened into every LATER phase's conversation
	// after its opening messages, and into every later run's brief, each
	// rendered as the message the model read ([prompts.SteerMessage]).
	//
	// A phase is a fresh conversation, so a note read in one phase is gone
	// from the next unless it is carried: a later reviewer would judge the
	// work against the task the person corrected. Carried as the SAME
	// message rather than summarised, because the note's words are the
	// person's and a paraphrase is the engine's. A turn that resumes from a
	// coding run starts from the reads its run's record holds
	// ([Resume.Steered]), which is every note the turn read before it — the
	// record is where every read is written. Guarded by mu.
	steered []steer.Read
}

var _ turn.Phases = (*Runner)(nil)

// New builds a runner.
//
// A runner with no session host is refused here rather than at its first
// phase: every turn runs a phase that offers tools — the reviewer, at the
// least — and a phase reaches its CLI through nothing else, so the turn would
// fail after its run was launched and its prefetch spent, naming a toolloop
// field instead of this one.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Registry == nil:
		return nil, fmt.Errorf("runner: no tool registry")
	case cfg.Models == nil:
		return nil, fmt.Errorf("runner: no provider registry")
	case cfg.Sessions == nil:
		return nil, fmt.Errorf("runner: no tool session host (Config.Sessions) — " +
			"every phase serves its tools to its CLI on this process's one host")
	case cfg.Launcher == nil:
		// AN ASSEMBLY MISTAKE, named where it is made. Every executor is
		// a launched run, so a runner without a launcher cannot run the
		// one phase every turn has.
		return nil, fmt.Errorf("runner: no executor launcher (Config.Launcher) — " +
			"every seat's executor is its coding CLI's own run, and the " +
			"assembler must supply what launches it")
	case cfg.Resume != nil && frozen(cfg.Context, cfg.Conversation) != (execstate.Frozen{}):
		// TWO SOURCES FOR ONE FROZEN THING, and no honest way to pick: the
		// parked state is what the turn's first launch was built from, and
		// a context fetched again for the resume is a second answer to the
		// same question. Refused where it is assembled rather than one of
		// them silently dropped.
		return nil, fmt.Errorf("runner: a resumed turn's context is the one its parked " +
			"state froze at the turn's start — leave Config.Context and " +
			"Config.Conversation empty with Config.Resume")
	}
	if cfg.Resume != nil {
		cfg.Context, cfg.Conversation = thawed(cfg.Resume.State.Context)
	}
	return &Runner{cfg: cfg}, nil
}

// Execute is the executor: one agentic pass that decides what to do and does
// it — launched as the seat's coding CLI's own run, detached in its box, and
// collected later through [Runner.Resume].
//
// It is given the whole first-party surface, and its MCP servers by name only
// — rather than every tool definition a real server publishes, which on a
// large stack is a wall of text in every prompt. Discovery is a tool call,
// which also keeps the prompt prefix stable while a server's catalogue changes
// underneath.
//
// One phase rather than two is the whole redesign. Planning in one
// conversation and acting in another cost the actor everything the planner
// learned, and made the planner NAME its tools in advance against a catalogue
// it was never shown — so it guessed, and the engine spent a whole subsystem
// reconciling those guesses with reality.
//
// TWO GATES BEFORE ANYTHING IS BUILT, each of which would otherwise let a run
// start that must not:
//
//   - A REFUSAL ALREADY CERTAIN launches nothing. The run makes its model
//     calls in a box, post-charged when it is collected, so the meter is asked
//     here — or a run would be paid for whole on a window already past its
//     ceiling — and again once its brief is built, since building it can be
//     what fills the window ([Runner.executeAsAgentRun]).
//   - THE SEAT FENCE. A run outlives the turn segment that launched it and is
//     not fenced once it runs — a seat that moves leaves it running, and a
//     person's stop ends it through the coordinator rather than the fence —
//     so the launch is the last moment a seat that moved, or one a person
//     stopped, can be kept from starting one.
func (r *Runner) Execute(ctx context.Context, round int, notes string, history []ledger.Iteration) (turn.Work, turn.Surface, error) {
	if err := toolloop.Refusal(ctx, r.cfg.Budget); err != nil {
		return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, err)
	}
	if r.cfg.Fence != nil {
		if err := r.cfg.Fence(); err != nil {
			return turn.Work{}, turn.Surface{}, fmt.Errorf("runner: %s: %w", phase.Execute, err)
		}
	}
	return r.executeAsAgentRun(ctx, round, notes, history)
}

// executorPrompt is the executor's opening: its system prompt and the turn's
// own ask, which its coding CLI is handed as an appended system prompt and
// the prompt it runs ([AgentRunRequest]).
//
// It also RECORDS what the prompt's tool-skill catalogue offered, as a
// `knowledge_read` with `via: skill_injected` (see [emitter.skillsInjected]):
// here, once, because this is the one frame that renders the catalogue, and
// a record made anywhere else would describe a catalogue matched a second
// time against a live registry rather than the one sent.
//
// TWO VIEWS OF THE TOOLS, and each section reads the one it describes.
// snapshot is the universe the surface was built FROM
// ([Runner.executorUniverse]): the tools to discover are what it holds that
// the surface does not offer yet — on the executor, its MCP servers, since
// every first-party tool is offered from the launch and its definition
// reaches the run over the bridge. built is the surface itself, which also
// holds what [Runner.surfaceWith] adds to its own copy — submit_work, the
// discovery pair and `delegate` — so every section gated on a tool being
// offered, and the tool-skill matching, read built's names. Read off the
// snapshot, they never saw `delegate`, and `## Your workers` was rendered for
// no seat at all.
func (r *Runner) executorPrompt(ctx context.Context, notes string,
	history []ledger.Iteration, snapshot tools.Snapshot, built *tools.Surface, fresh []steer.Note,
) (system, user prompts.Prompt) {
	offer := r.cfg.Skills.Offer()
	system = prompts.BuildExecutor(r.cfg.Seat, prompts.ExecutorInput{
		// What the universe the surface was built from holds and the
		// surface does not offer yet: nothing this executor cannot
		// activate, and nothing it already holds.
		Discoverable:   snapshot.Discoverable(built.Active()),
		AvailableTools: built.Universe().Names(),

		ThreadContext:       r.cfg.Context.ThreadContext,
		PersonalMemory:      r.cfg.Context.PersonalMemory,
		RelevantKnowledge:   r.cfg.Context.RelevantKnowledge,
		EpisodeRecall:       r.cfg.Context.EpisodeRecall,
		CounterpartyProfile: r.cfg.Context.CounterpartyProfile,
		SynthesizedSkills:   r.cfg.Context.SynthesizedSkills,
		Workers:             r.workerCatalogue(),
		// The tool-skill catalogue, offered against the surface this
		// phase actually has — and nil where a company has published
		// none, which keeps the prompt free of skill scaffolding rather
		// than rendering an empty section.
		Skills: offer.Catalogue(),
	})
	r.emitter().skillsInjected(ctx, phase.Execute, offer.Drain())
	user = prompts.BuildPhaseUserMessage(prompts.UserMessage{
		TaskDescription:     r.taskFor(notes),
		PriorWork:           r.priorWork(ctx, history),
		ConversationHistory: r.cfg.Conversation,
		// EVERY NOTE THE TURN HAS READ, so a run launched after a person
		// corrected the turn starts from the turn as corrected — and then
		// the notes this brief is the first to read. The run is a fresh
		// session with no conversation of the engine's to carry them in,
		// so its brief does. See [Runner.steered].
		Steered: append(r.carriedSteers(), renderedNotes(fresh)...),
	})
	return system, user
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
// Rewritten here, at render, and not when the round closed: the record keeps
// every payload whole, so the bound is a property of what this prompt can
// carry rather than of what happened. The compactor caches by content, so the
// block re-rendered for the executor and again for the reviewer pays for each
// payload once.
func (r *Runner) priorWork(ctx context.Context, history []ledger.Iteration) string {
	fitted := ledgerfit.Fit(ctx, r.cfg.Compact, ledger.IterationPieces(history, r.cfg.SkipNames))
	return ledger.RenderIterations(history, r.cfg.SkipNames, fitted)
}

// work is one collected executor run, assembled for reporting.
type work struct {
	submit   *structured.Tool[workPayload]
	res      phaseResult
	surface  *tools.Surface
	snapshot tools.Snapshot

	// run names the run and the box it ran in.
	run RunRecord
}

// finishWork turns a collected executor run into the turn's Work, publishing
// the phase record on the way.
func (r *Runner) finishWork(phaseCtx context.Context, round int, w work) (turn.Work, turn.Surface) {
	payload, submitted := w.submit.Value()
	if !submitted {
		// THE RESCUE PATH. A run that ended without calling submit_work —
		// its CLI stopped, hit its turn cap, or answered only in prose —
		// has produced text and no account of itself. Discarding the turn
		// wastes everything it did; calling it delivered puts words in its
		// mouth on the one question that matters.
		//
		// So the outcome is INCOMPLETE and the work is marked rescued.
		// Both are load-bearing: the engine wrote this word, so no fast
		// path may act on it and the reviewer judges the record instead.
		//
		// AND NO SUMMARY. The executor gave no account of itself, and the
		// engine must not write one for it: Summary is the INTENT line —
		// rendered whole, as "set out to", to every later round and to the
		// reviewer, and kept whole in the conversation ledger — where its
		// prose is already carried as Text, tail-elided at render and
		// sized for what a transcript is. Copying the transcript into the
		// intent line put a whole run's output into the one slot nothing
		// bounds, twice per round, and under a heading that called it the
		// executor's own account.
		log.WarnContext(phaseCtx, "work_never_submitted", "round", round)
		payload = workPayload{Outcome: string(turn.OutcomeIncomplete)}
	}

	// NO PROMPT ON THE RECORD: the prompt went out on the launch's own
	// started record, and this one collects a run rather than opening a
	// conversation — publishing the opening again would show a reader a
	// prompt that was not sent this time.
	r.emitter().completed(phaseCtx, phaseRecord{
		Phase: phase.Execute, Iteration: round,
		Result: w.res.Result, Elapsed: w.res.Elapsed,
		StartedAt: w.res.StartedAt, Caps: w.res.Caps,
		Decision: payload.Outcome, Rescued: !submitted,
		Run:       w.run,
		RunSteers: w.res.RunSteers,
		Available: w.surface.Active(),
		// What the executor could still have discovered, with no schemas:
		// the universe it was built from less what it held by the end.
		// Sending every MCP server's tool definitions is what made a turn
		// expensive, and discovery is what replaced it. Disjoint from
		// Available, so the two together are the whole reachable surface
		// and a reader counts no tool twice.
		Catalogue: undiscovered(w.snapshot, w.surface.Active()),
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
		Rescued:       !submitted,
	}, describe(w.surface)
}

// undiscovered is the names of a universe a surface does not offer.
func undiscovered(universe tools.Snapshot, active []string) []string {
	var out []string
	for _, name := range universe.Names() {
		if !slices.Contains(active, name) {
			out = append(out, name)
		}
	}
	return out
}

// Review judges the round.
//
// It is given the tool LOG as evidence and told the narration is not. A
// reviewer shown only what the executor said about itself grades the prose.
// The log is the run's BRIDGED calls only — its CLI's own shell, edit and test
// work happened in its box and is in no log — so the reviewer is also handed
// the branches and pull requests the run reported delivering, and its rules
// say what the log cannot hold ([prompts.ReviewHeader]).
//
// The rule that holds for some seats only is gated on the seat's surface:
// `a2a_ask` named where the registry carries it at all.
func (r *Runner) Review(ctx context.Context, round int, w turn.Work, history []ledger.Iteration) (turn.Review, error) {
	snapshot := r.cfg.Registry.Snapshot()
	_, offersA2A := snapshot.Lookup(A2AAskTool)
	submit := structured.New(SubmitReviewTool, submitReviewDescription, reviewSchema, decodeReview)
	surface, err := r.surfaceWith(ctx, r.ownMeter(), phase.Review, round, snapshot, submit, nil)
	if err != nil {
		return turn.Review{}, err
	}

	// VERBATIM. Review's evidence log takes the zero FormatOptions: the
	// budgets belong to the cross-round ledger, and a reviewer judging an
	// elided log is judging a summary and calling it evidence.
	offer := r.cfg.Skills.Offer()
	system := prompts.BuildReview(r.cfg.Seat, prompts.ReviewInput{
		Intent:            w.Summary,
		Outcome:           string(w.Outcome),
		Rescued:           w.Rescued,
		Evidence:          w.Evidence,
		OpenQuestions:     w.OpenQuestions,
		Produced:          reviewArtifact(w),
		ToolLog:           ledger.FormatCalls(w.Calls, ledger.FormatOptions{Skip: r.cfg.SkipNames}),
		Delivered:         prompts.DeliveredRefs(w.Refs, w.RefsElided),
		EarlierIterations: r.priorWork(ctx, history),
		OffersA2A:         offersA2A,
		// THE SKILLS AN OPERATOR SCOPED TO REVIEW, which the prompt, the
		// parser and the docs all supported and nothing ever passed: a
		// skill authored with `phases: [review]` reached no reviewer.
		Skills: offer.Catalogue(),
	})
	r.emitter().skillsInjected(ctx, phase.Review, offer.Drain())
	user := reviewTask(r.cfg.Task)

	phaseCtx, res, err := r.runPhase(ctx, phaseRun{
		phase: phase.Review, surface: surface, system: system, user: user,
		rounds: reviewRounds, iteration: round,
		// NEVER FORCED, like every phase — a request has no way to force
		// a call. The reviewer's only tool is its submission, so a forced
		// choice once looked free here — but several current models
		// answer one with a 400, which is fatal to the whole turn rather
		// than one round, and the
		// submission named here is what makes a round of prose get asked
		// again (toolloop.Config.TerminateAfter).
		terminateAfter: []string{SubmitReviewTool},
		steerable:      true,
	})
	if err != nil {
		// Nothing to salvage here, and nothing lost: a reviewer's surface
		// carries its submission tool and no catalogue at all, so its
		// record can never prove an outward write. What the EXECUTOR did
		// before it is already in the caller's hands — [turn.Run] reads it
		// off the Work this Review was handed.
		return turn.Review{}, err
	}

	payload, submitted := submit.Value()
	if !submitted {
		// A reviewer that never decided must not silently pass the turn.
		// Defaulting to `done` here is the difference between "the work
		// was judged good" and "nothing judged it", and those look
		// identical downstream. Loop back instead — the delivery gate and
		// the stall guard bound how long that can go on.
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

// reviewTask frames the trigger for the reviewer.
//
// THE SAME BYTES MEAN DIFFERENT THINGS TO THE TWO PHASES. The executor is
// HANDED this text as the thing to do, so it reaches that phase bare. The
// reviewer is asked about a turn that is already underway, and handed the
// same bytes bare it reads them as a request that has just arrived.
//
// Not hypothetical. On a turn whose first round had already posted a
// clarifying question, reviewers read the unchanged trigger next to an
// `## Earlier rounds` block showing that question, and the only account that
// fits both is that the sender asked again — "the founder ... has now
// re-posted the same vague request". Two consecutive rounds said it, and the
// second built its correction on it, instructing the next round to act on a
// message nobody had sent. So the trigger reaches the reviewer as evidence,
// labelled, rather than as an instruction.
//
// The label is CONSTANT, unlike [Runner.taskFor]'s correction: the review
// phase re-sends this every round, and a per-round frame would move bytes the
// provider's prefix cache is keyed on.
//
// Two sections in its outline: the label, and the trigger whole — whatever
// headings a vendor's body carries are the trigger's, not the reviewer's.
func reviewTask(task string) prompts.Prompt {
	b := prompts.NewBuilder("")
	b.Lead("reference", "For reference", "The trigger this turn is answering, for "+
		"reference. It is the message the agent was ALREADY working on when the "+
		"rounds below ran — not a new one that arrived during the turn, and not a "+
		"repeat of it:\n\n")
	b.Lead("task", "Task", task)
	return b.Build()
}

// reviewRecord builds Review's completed record.
//
// A function because Review reports from TWO places — its decoded payload and
// its rescue — and the two must describe the same phase. Written out twice,
// the rescue path is the one that quietly loses a field.
func reviewRecord(round int, system, user prompts.Prompt, res phaseResult,
	decision, notes string, rescued bool, surface *tools.Surface,
) phaseRecord {
	return phaseRecord{
		Phase: phase.Review, Iteration: round, System: system, User: user,
		Result: res.Result, Exhausted: res.Exhausted, Elapsed: res.Elapsed,
		StartedAt: res.StartedAt, Caps: res.Caps,
		Decision: decision, Notes: notes, Rescued: rescued,
		Available: surface.Active(),
	}
}

// runPhase drives one engine-loop phase, extending it when the judge allows:
// the reviewer and the onboarding pass. Never the executor, which is its
// CLI's own run.
//
// The extension loop lives here rather than in the extension package because
// only the caller knows what FINISHING looks like for its phase — the
// reviewer exits through its submission tool, onboarding through
// mark_onboarded — and a generic loop would have to be told, which is the
// same thing said twice.
//
// phaseRun is one phase's inputs.
//
// A struct rather than nine positional parameters, which is what it was: three
// adjacent ints (rounds, ceiling, iteration) and two adjacent strings (system,
// user) is a signature where a transposition compiles and produces a phase
// that silently runs with the wrong budget.
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
	// SUCCESSFULLY — and, by naming them, declares that the phase finishes
	// by one: a round that ends in prose is re-prompted with a corrective
	// naming them rather than accepted as the phase's end.
	//
	// THAT is what makes a phase end in its submission, and there is
	// nothing beside it: a request has no way to force a call
	// ([llm.Request.Tools]), so the model decides on every round. A forced
	// choice was a request some endpoints ignore and several current
	// models refuse with a 400 (Claude Opus 5.5, Sonnet 5.5, Fable 5.1,
	// Mythos 5.1) — and a 400 is not retried down the chain, so on those
	// models it failed every review and every onboarding pass before the
	// corrective that actually enforces the call could run.
	terminateAfter []string

	// steerable is whether a person's notes are drained into this phase's
	// rounds — the reviewer, and nothing else. See [Config.Steer].
	steerable bool
}

// runPhase returns the PHASE'S CONTEXT as well as its result.
//
// The caller needs it to publish agent_phase_completed under this phase's span
// rather than the turn's, which is what turns the dashboard's trace tree from
// a flat list under one turn node into trigger -> turn -> {execute, review}.
// The span has ENDED by then, and that is fine: an event recording a
// phase belongs to that phase's span, and a span id is a label rather than a
// lifetime.
//
// It is returned under its own name rather than shadowing the caller's ctx on
// purpose — work the caller does AFTER the phase must not be attributed to a
// span that is closed.
func (r *Runner) runPhase(ctx context.Context, in phaseRun) (context.Context, phaseResult, error) {
	ph, surface := in.phase, in.surface
	// ONE SPAN PER PHASE, wrapping the WHOLE extension loop below rather
	// than each toolloop.Run inside it: an extended phase is one phase that
	// ran longer, and a span per invocation would report it as two phases
	// and split its rounds across them.
	//
	// The attributes are deliberately thin. Everything a reader wants about
	// what a phase DID — prompts and response verbatim, every tool call's
	// arguments and result, tokens, the decision — is already on
	// agent_phase_completed, and duplicating it onto a span would
	// send whole prompts to a collector that is not the event store. What
	// no event records is DURATION, per phase and per round, and that is
	// exactly what a span adds.
	ctx, span := tracing.Start(ctx, "agent.runner", "agent.turn."+string(ph),
		attribute.String("crewlet.phase", string(ph)),
		attribute.Int("crewlet.iteration", in.iteration))
	defer span.End()
	// THE SAME BRACKET AS THE SPAN, and deliberately so: a span is a
	// collector's view of how long this phase took and agent_phase_completed
	// is the event store's, and the two disagreeing about one phase is worse
	// than either being absent. A monotonic reading, so a clock correction
	// mid-phase cannot report a negative duration.
	began := time.Now()
	segment := began.UTC()
	system, user := in.system, in.user
	iteration, ceiling := in.iteration, in.ceiling
	terminateAfter := in.terminateAfter
	// The phase's own record of what it managed, so the FAILURE path has
	// something to report. A phase that dies returns no Result at all, and
	// without this the only trace of it is the started event — a dashboard
	// left showing an in-flight call with no response and no reason.
	emit := r.emitter()
	progress := &toolloop.Progress{}
	// The PHASE's record, accumulated across every invocation of the tool
	// loop. Declared up here because the failure path below reports it too.
	var out phaseResult
	// The extension policy, up here because every record this phase
	// publishes — the failure record included — states the ceiling it set.
	policy := extension.Policy{
		Enabled: r.cfg.Caps.ExtensionOn, RoundStep: r.cfg.Caps.ExtensionStep, Ceiling: ceiling,
	}
	// The phase's own wall clock, stamped onto the record on the way out.
	// A closure rather than a line at each `return`, because there are two
	// success returns inside the loop below and a measurement that is
	// written at one of them and not the other is the harder bug: the
	// record still publishes, and only the EXTENDED phases — the long ones
	// somebody is timing — come back without a duration.
	done := func() (context.Context, phaseResult, error) {
		out.Elapsed = time.Since(began)
		out.StartedAt = segment
		out.Caps = capsOf(policy, out.Result.MaxRounds)
		return ctx, out, nil
	}
	// Returns the phase context too, so `return fail(err)` stays a single
	// line now that runPhase hands its context back.
	fail := func(err error) (context.Context, phaseResult, error) {
		// The span carries the failure too. The event below is the record
		// of WHAT broke; the span is what makes the broken phase findable
		// in a trace beside the calls that led to it.
		tracing.Fail(span, err)
		emit.completed(ctx, phaseRecord{
			Phase: ph, Iteration: iteration, System: system, User: user,
			// FOLDED onto what the phase already had, exactly as the success
			// path folds it. `progress` is one object reused across every
			// invocation of the loop and `record` REPLACES rather than
			// appends, so it holds only the invocation that died: a phase
			// that ran twenty rounds, was extended and then failed on
			// extension round 2 published a record claiming two rounds,
			// numbered 1 and 2, with the tokens of those two — on precisely
			// the card an operator opens to see what led up to the failure.
			// `out` is read here, not captured: the closure sees whatever
			// the loop has accumulated by the time it fails.
			Result: foldOnto(out, progress.Snapshot()), Available: surface.Active(),
			// A PHASE THAT DIED STILL TOOK TIME, and on a timeout it is
			// the number that says so: a phase whose provider hung for
			// four minutes and one refused in fifty milliseconds are the
			// same record without it.
			Elapsed:   time.Since(began),
			StartedAt: segment,
			Caps:      capsOf(policy, maxRounds(out, in.rounds)),
			// A PHASE A PERSON STOPPED DID NOT FAIL. It carries the stop as
			// its error, so the card says why it ended, and is not marked
			// failed — a turn is listed as failed when any of its rows is,
			// and a turn somebody deliberately ended is not one that broke.
			Failed: !turn.Stopped(err), Err: err,
		})
		return ctx, phaseResult{}, err
	}

	members, err := r.cfg.Models.Chain(r.cfg.Seat.Role, ph)
	if err != nil {
		return fail(err)
	}
	// A chain even for one member. The wrapper is a pass-through there, and
	// uniform behaviour is worth more than the allocation: a one-member seat
	// and a three-member one then fail, log and report identically.
	//
	// OnFallback is wired HERE rather than inside the chain because this is
	// the innermost frame that knows which turn and which iteration the
	// hand-off belongs to. The callback fires synchronously from inside the
	// chain's own loop, before the next member is tried, so `ctx` and
	// `iteration` are the ones the failing call ran under.
	provider, err := chain.New(members, chain.Options{
		OnFallback: func(f chain.Fallback) { emit.fallback(ctx, ph, iteration, f) },
	})
	if err != nil {
		return fail(fmt.Errorf("runner: %s: %w", ph, err))
	}

	// Published BEFORE the first provider call, so a seat that is thinking
	// says which phase it is thinking in. The completed event is the durable
	// record and may be minutes away.
	//
	// THE SURFACE TRAVELS WITH THE TEXT because the prompt is both: every
	// request carries the tool definitions, which the CLI lists from the
	// phase's session and hands its model, and the vendor bills as input.
	emit.started(ctx, ph, iteration, system, user, surface,
		capsOf(policy, out.Rounds+in.rounds))

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: system.Text},
		{Role: llm.RoleUser, Content: user.Text},
	}
	// EVERY NOTE THE TURN HAS ALREADY READ, after the opening and in the
	// order it was read, so a new phase starts from the turn as the person
	// corrected it. See [Runner.steered].
	for _, note := range r.carriedSteers() {
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: note})
	}
	budget := in.rounds
	// The finishing correctives the run of declines being CONTINUED has
	// already had — nonzero only for an invocation that picks up a
	// corrective the last one withheld. See toolloop.Config.CorrectivesSpent.
	correctivesSpent := 0
	// Rounds this phase has run past an invocation's budget WITHOUT the
	// judge, to read a withheld corrective. A phase's own allowance of
	// finishing correctives, and never more: past it, a phase that keeps
	// working and declining by turns would be extending itself round by
	// round on nobody's decision.
	unjudged := 0
	for {
		// What the phase holds BEFORE this invocation, captured by value:
		// the loop calls OnProgress from inside Run, and `out` is written
		// only after it returns.
		//
		// The loop numbers its rounds per INVOCATION, from 1, and its Result
		// carries only that invocation's own rounds. An extended phase runs
		// it again, so every publish has to be folded onto what came before
		// — and BOTH halves of that fold had their own symptom. Without the
		// offset the second invocation restarts at 1 and every consumer that
		// orders on the round number sees the phase run backwards: the live
		// projection's stale-round guard dropped the whole extension, and
		// the ledger merged extension round 1 into original round 1. Without
		// the prefix the live view — which rebuilds a call from each frame
		// rather than merging — collapsed a twenty-round ledger to a single
		// round the instant the extension's first frame landed, moving
		// everything above the insertion point, which is the one property
		// the round ledger exists to guarantee.
		prior := out
		// The rounds already behind this invocation, declared so a tool
		// that asks which round it is in hears the PHASE's number — the
		// same number the records below are renumbered onto.
		loopCtx := toolloop.WithRoundOffset(ctx, prior.Rounds)
		var steering toolloop.Steerer
		if in.steerable && r.cfg.Steer != nil {
			steering = phaseSteer{r: r, ctx: ctx, ph: ph, iteration: iteration, offset: prior.Rounds}
		}
		res, err := toolloop.Run(loopCtx, toolloop.Config{
			Provider: provider, Messages: messages, Surface: surface,
			Host: r.cfg.Sessions, ServerTitle: sessionTitle(r.cfg.Seat, ph),
			// The chain's head, standing in until a completion names the
			// member that actually served — see toolloop.Config.
			ProviderKey: members[0].Key,
			MaxRounds:   budget, Budget: r.cfg.Budget,
			Fence:            r.cfg.Fence,
			CorrectivesSpent: correctivesSpent,
			// A phase that has SUBMITTED is finished. Without this the
			// loop asks again, the model submits again, and the phase
			// spends its whole round budget re-deciding — measured at
			// four identical submissions before the cap stopped it.
			TerminateAfter: terminateAfter,
			// Progress feeds the failure path above; OnProgress feeds the
			// live view. Both, because they answer different questions:
			// one is read after the loop returns nothing, the other while
			// it is still running.
			Progress: progress,
			// The live view is the only reason to stream, so it is on
			// exactly when something is listening.
			StreamPartials: true,
			// A person's notes, into the reviewer's rounds only.
			Steer: steering,
			OnProgress: func(live toolloop.Result) {
				folded := foldOnto(prior, live)
				emit.progress(ctx, ph, iteration, folded, capsOf(policy, folded.MaxRounds))
			},
		})
		if err != nil {
			return fail(fmt.Errorf("runner: %s: %w", ph, err))
		}
		out.Text = res.Text
		out.Exhausted = res.ExhaustedRounds
		// The same fold the live view got, so the record a phase publishes
		// and the frames it published while running describe one phase.
		out.Result = foldOnto(prior, *res)
		out.Rounds = prior.Rounds + res.RoundsUsed
		messages = res.Messages

		// A CORRECTIVE THE LOOP WITHHELD IS SENT HERE, when the phase may
		// run past this invocation's budget. Its last round ended without
		// the submission and had no round after it to be asked in — the
		// round a phase most naturally submits on, and the one the
		// extension nudge tells it to submit on — so without this the
		// outcome turned on WHICH round a decline landed on: the same
		// fenced JSON on the round before the cap was re-asked and
		// submitted, on the cap it was rescued as incomplete with the
		// ceiling's rounds unspent.
		//
		// No judge. Its question is whether a phase still working deserves
		// more rounds, and this one has finished its work and is missing
		// only the call that reports it. What bounds it instead is the
		// allowance the loop would have spent had the rounds been inside
		// the budget, shared with it rather than reset (CorrectivesSpent),
		// and the phase's ceiling, which an operator who turned extensions
		// off has set to the budget itself.
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
			// Read off the phase TOTALS, never off `res`: an extended
			// phase ran the loop more than once and the last invocation
			// knows only its own slice.
			span.SetAttributes(attribute.Int("crewlet.rounds", out.Rounds))
			return done()
		}
		granted, decision := r.consider(ctx, ph, iteration, out.Rounds, policy, extension.Request{
			Phase: ph, Task: r.cfg.Task,
			Calls: calls(surface), LastText: res.Text, RoundsUsed: out.Rounds,
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
			Content: extension.Nudge(ph, granted, decision.Reason),
		})
		// An exhausted phase was still calling tools, so no run of
		// declines is being continued.
		budget, correctivesSpent = granted, 0
	}
}

// offsetRounds renumbers one loop invocation's rounds onto the phase's own
// scale, so an extension continues the count instead of restarting it.
//
// Copies rather than mutates: the caller's Result is the loop's live snapshot,
// published from another goroutine, and shifting it in place would renumber a
// slice the loop is still appending to.
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
	// A note's round too, or a note read in extension round 1 is drawn
	// beside the phase's first round.
	steers := make([]toolloop.SteerMark, len(res.Steers))
	for i, m := range res.Steers {
		m.Round += prior
		steers[i] = m
	}
	res.Steers = steers
	// A CLI run's rounds too, or an extension's first run names the phase's
	// round 1 as its own: a run that made no request names none (zero),
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
	// The calls in flight are on the same scale, or a live row names a call
	// in round 1 of a phase that is twenty rounds in.
	running := make([]toolloop.RunningCall, len(res.Running))
	for i, c := range res.Running {
		c.Round += prior
		running[i] = c
	}
	res.Running = running
	// The round IN FLIGHT is on the same scale as the rounds behind it, or it
	// COLLIDES with one of them. A consumer keys the ledger on the round
	// number — the dashboard's `rounds()` builds one block per number and the
	// partial overwrites the block it lands on — so an unshifted partial from
	// extension round 1 is written into the phase's committed round 1: that
	// round's prose is replaced by text the model is writing
	// twenty rounds later, sitting directly above round 1's own tool calls,
	// while the round actually running gets no block at all. The abandoned
	// attempts carry the same number and travel with it.
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
// The judge is a model call like any other and was the only one nothing
// recorded: no phase event, no span, and no charge — it runs outside the tool
// loop, which is where every other call is metered. So it is wrapped here
// rather than inside [extension.Consider], which is policy plus a model and
// has no business knowing about spans, meters or the event vocabulary.
//
// A CHARGE THAT REFUSES DOES NOT FAIL THE PHASE. The extension is a generosity
// on a phase that has already run out of rounds, and a seat at its cap should
// stop extending rather than have the phase break under it: an over-budget
// judgement is logged, published and treated as "no extension", which is the
// same outcome as the judge saying no. Its tokens are on the counter either
// way: the judge has answered by the time it is charged, and a meter records a
// call it refuses as it records one it admits ([toolloop.BudgetMeter]). What
// the refusal does stop is every model call after it: the meter now holds it,
// so the next phase's first call — the executor's launch after onboarding —
// is refused before it is made, and the turn ends on the budget without
// paying for a call certain to be thrown away.
//
// AND A JUDGE IS NOT CALLED AT ALL ON A REFUSAL ALREADY CERTAIN: it is a model
// call like any other, billed and then refused. That happens when a refusal
// lands inside the phase's last round without being the round's own — a
// worker it delegated to, refused by the seat or the company.
func (r *Runner) consider(ctx context.Context, ph phase.Phase, iteration, hostRound int,
	policy extension.Policy, req extension.Request,
) (int, extension.Decision) {
	// The span the turn-engine doc has always promised: one per judge call,
	// nested under the phase that fired it. It is the only place a reader
	// can see what the judgement COST IN TIME, which no event records — and
	// a judge on a slow cheap model is a stall in the middle of a phase that
	// has already been running for minutes.
	ctx, span := tracing.Start(ctx, "agent.runner", "agent.turn.judge",
		attribute.String("crewlet.phase", string(ph)),
		attribute.Int("crewlet.iteration", iteration),
		attribute.Int("crewlet.rounds", req.RoundsUsed))
	defer span.End()

	// A REFUSAL ALREADY CERTAIN calls no judge — see above. Only where the
	// judge WOULD be called: a policy that declines to ask, or a seat with
	// no judge, costs nothing, and its own reason is the truer account of
	// why the phase was not extended.
	if asks, _ := policy.ShouldAsk(req.RoundsUsed); asks && r.cfg.Judge != nil {
		if refused := toolloop.Refusal(ctx, r.cfg.Budget); refused != nil {
			log.WarnContext(ctx, "extension_judge_over_budget", "phase", ph,
				"iteration", iteration, "tokens", 0, "error", refused.Error(),
				"detail", "the budget had already refused this turn, so the judge was not called")
			span.SetAttributes(attribute.Bool("crewlet.judge_called", false))
			return 0, extension.Rescue(extension.ReasonBudgetExhausted)
		}
	}

	// THE JUDGE'S OWN WALL CLOCK, bracketed here rather than inside
	// [extension.Consider] for the same reason the span is: that function is
	// policy plus a model and has no business knowing what is recorded about
	// it. It is the one number that separates a judge stalling a phase from
	// a judge that simply said no — the two are otherwise the same record
	// with the same verdict.
	began := time.Now()
	granted, decision := extension.Consider(ctx, r.cfg.Judge, policy, req)
	judged := time.Since(began)
	if !decision.Asked {
		// The policy declined to ask, or there is no judge. Nothing was
		// called, so there is nothing to report or to charge — and an event
		// here would claim a model call that never happened.
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

// charge meters a model call the tool loop did not make, once it has been
// made: the meter records it whether or not it fits.
//
// An unreachable counter is NOT a refusal — the same three-valued rule the
// loop's own charge follows — but here both answers end the same way, because
// neither is worth failing a turn over: the caller declines to extend and
// says why.
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
// it, producing the record of the PHASE rather than of the invocation.
//
// One function for all three publishers — the live frame, the completed
// record and the failure record — because they were three places that had to
// agree about what an extended phase is, and they did not: the live frame
// carried the invocation alone, the completed record carried the invocation's
// token counters, and the failure record carried the invocation raw. Anything
// a loop invocation counts from zero has to be folded here; anything that
// carries across (the conversation, and so `Text`) is taken from the
// invocation as it stands.
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
	// Every CLI run of the phase, the extension's after the original's.
	res.Invocations = append(
		append([]toolloop.Invocation(nil), done.Result.Invocations...), res.Invocations...)
	res.RoundsUsed = done.Rounds + live.RoundsUsed
	// The cap the PHASE is under: every round already behind it plus what
	// this invocation was granted — which is exactly how an extension
	// moves it.
	res.MaxRounds = done.Rounds + live.MaxRounds
	res.InputTokens += done.Result.InputTokens
	res.OutputTokens += done.Result.OutputTokens
	res.CacheRead += done.Result.CacheRead
	res.CacheWrite += done.Result.CacheWrite
	// Accumulated for the same reason the token counts are: an EXTENDED
	// phase runs the loop again and the second invocation counts only its
	// own rounds, so a phase that answered nothing twice under two
	// invocations would report one.
	res.EmptyAnswers += done.Result.EmptyAnswers
	// The model that served the phase, not the model that served the round
	// that died. An invocation which failed before its first completion
	// names nobody, and a record with no model on it reads as a phase that
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

	// Elapsed is the phase's own wall clock, measured around the whole
	// extension loop rather than around one invocation of it: an extended
	// phase is one phase that ran longer, exactly as its span is one span.
	//
	// Measured here because this is the only frame that brackets the
	// phase — the callers assemble the record AFTER decoding a payload,
	// and a clock read there would fold the decode into the measurement
	// differently on every path.
	Elapsed time.Duration

	// StartedAt is when this segment of the phase began, and Caps the round
	// allowance it ended under — both stamped by the frame that brackets
	// the phase, for the reason Elapsed is.
	StartedAt time.Time
	Caps      roundCaps

	// Result is the loop's own outcome, kept whole so the phase can report
	// what it spent and what it called. The fields above are the ones the
	// loop's CALLER acts on; this is what its telemetry publishes, and
	// re-deriving one from the other is how the two come to disagree.
	Result toolloop.Result

	// RunSteers is what a collected coding run read of the turn's notes —
	// in its brief, and at boundaries of its CLI's own loop — at the run's
	// one round with each one's boundary. A run of the engine's loop reads
	// its notes at its rounds, which its Result records; a coding run's
	// reads are on its record instead ([Runner.resumeAgentRun]).
	RunSteers []types.PhaseSteer
}

// surfaceWith builds a phase surface, registering the phase's submission tool
// on a PRIVATE copy of the snapshot.
//
// Private because the tool is per-phase state: two phases of one turn each get
// their own, and registering into the shared registry would make one phase's
// submission visible to the next.
// The `extra` set is per-TURN rather than per-phase state: today it is the
// sub-agent spawner, which is bound to one parent turn's grant and must reach
// the surface a run is collected against as well as the one it was launched
// with. Injected here because this is the single funnel every phase surface
// goes through, so a phase that lost the tool mid-turn is not representable.
//
// m is where the surface's own calls are charged — the runner's own segment
// for every surface but the executor's ([meter]).
func (r *Runner) surfaceWith(ctx context.Context, m meter, ph phase.Phase, round int,
	snapshot tools.Snapshot, submit tools.Callable, active []string, loaded ...string,
) (*tools.Surface, error) {
	if submit != nil {
		var err error
		snapshot, err = snapshot.With(tools.Entry{Tool: submit, Origin: tools.OriginBuiltin})
		if err != nil {
			return nil, fmt.Errorf("runner: %s: %w", ph, err)
		}
		active = append([]string{submit.Name()}, active...)
	}
	// THE EXECUTOR'S OTHER WAY TO END: parking on a person's answer. Here,
	// in the funnel the launch and the collection both take, so the surface
	// a collection rebuilds holds the tool its run called.
	if ph == phase.Execute && r.cfg.Ask != nil {
		var err error
		snapshot, err = snapshot.With(tools.Entry{Tool: r.cfg.Ask, Origin: tools.OriginBuiltin})
		if err != nil {
			return nil, fmt.Errorf("runner: %s: %w", ph, err)
		}
		active = append(active, r.cfg.Ask.Name())
	}

	// The discovery pair reaches its surface through a closure, because
	// there is a real cycle here: activate must mutate the same Surface the
	// loop reads its tool definitions from, so the tools cannot exist
	// before the surface — and the surface cannot resolve them until they
	// are in its snapshot. The closure is read at call time, by which point
	// the surface exists.
	var surface *tools.Surface
	if discovers(ph) {
		for _, tool := range DiscoveryTools(func() *tools.Surface { return surface }) {
			var err error
			snapshot, err = snapshot.With(tools.Entry{Tool: tool, Origin: tools.OriginBuiltin})
			if err != nil {
				return nil, fmt.Errorf("runner: %s: %w", ph, err)
			}
			active = append(active, tool.Name())
		}
	}

	// The sub-agent spawner, on the same closure and for the same reason:
	// the child inherits the parent's LIVE active list, which does not
	// exist until this surface does.
	//
	// Built HERE rather than by each caller because this is the single
	// funnel every phase surface goes through, so the launch and the
	// collection get it from one place — and a collection that lost the
	// tool mid-run is not representable.
	if entry := r.spawnEntry(ctx, m, ph, round, snapshot,
		func() *tools.Surface { return surface }); entry.Tool != nil {
		next, err := snapshot.With(entry)
		if err != nil {
			// A collision here is an operator's MCP server publishing a
			// first-party tool name. LOSING THE SPAWNER is the
			// proportionate answer: failing the surface would kill every
			// Execute phase on that seat for as long as that server is
			// configured, which is a far larger outage than the missing
			// capability. The subagent package makes the same choice for
			// the same collision one level down.
			log.WarnContext(ctx, "spawn_tool_skipped", "phase", ph,
				"tool", entry.Tool.Name(), "error", err.Error())
		} else {
			snapshot = next
			active = append(active, entry.Tool.Name())
		}
	}
	// Bound to the turn, which is what lets a seat-scoped tool know who is
	// calling it without the seat travelling through the model's arguments.
	// Bound to the turn AS THIS PHASE, so a tool reporting what it did can
	// say which leg of the turn did it — see [turnctx.Turn.InPhase].
	surface = tools.NewSurface(ph.String(), snapshot, active).
		ForTurn(m.turn.InPhase(types.Phase(ph)))
	if r.cfg.Metrics != nil {
		surface = surface.WithCounter(r.cfg.Metrics)
	}
	// THE GUARD IS BUILT FROM THE FINISHED SURFACE, so what it enforces and
	// what the catalogue showed cannot disagree: both match the skills
	// against the surface's universe (catalogueSurface), and the catalogue's
	// "required" marker is the promise this keeps.
	guard := r.guardFor(ph, round, surface, loaded)
	// RECORDED ONLY HERE, which is what keeps it the TURN's guard: this is
	// the funnel every phase of the turn goes through, and a sub-agent's
	// child surface is built elsewhere. Recording inside guardFor would let
	// a child spawned mid-run overwrite the executor's own loaded set, so
	// a park moments later would persist the worker's.
	r.mu.Lock()
	r.guard = guard.skills()
	r.mu.Unlock()
	return surface.WithGuard(guard.tools()), nil
}

// discovers reports whether a phase is offered the discovery pair,
// list_mcp_server_tools and activate_tool.
//
// The executor (launched or collected) and onboarding are, because their
// prompts list the tools they can still discover — each MCP server by name,
// and on onboarding its knowledge-base tools — and tell the model how. The reviewer
// is not, and must not be: it judges the round from the evidence in front of
// it, and its prompt promises it no domain tools. activate_tool reaches every
// tool in the registry snapshot, MCP writes included, so a reviewer offered
// the pair could post to a thread or edit a ticket in the middle of grading
// whether the executor should have. Until this gate, surfaceWith added the
// pair to every phase, the reviewer's included.
func discovers(ph phase.Phase) bool {
	return ph == phase.Execute || ph == phase.Onboarding
}

// armedGuard is one built gate: the concrete guard for the runner's own
// bookkeeping, and the interface value the surface installs.
//
// Two views because A TYPED NIL WOULD NOT BE NIL. Returning the *skills.Guard
// as a tools.Guard would give the surface a non-nil interface wrapping a nil
// pointer, so its `guard != nil` check would pass and every tool call would
// take the guard path to be told yes.
type armedGuard struct {
	guard *skills.Guard
	wrap  tools.Guard
}

func (a armedGuard) skills() *skills.Guard { return a.guard }
func (a armedGuard) tools() tools.Guard    { return a.wrap }

// guardFor arms the required-skill gate for one phase session, or nil.
//
// PER SESSION, because "loaded" means "the body is in this model's context"
// and the executor, the reviewer and each sub-agent are separate message
// histories. A round-cap extension continues the same session AND the same
// surface, so a load carries across it; a self_iterate launches a fresh run
// and therefore a fresh guard, which is correct — its model's context started
// over too.
//
// The loaded keys are a PARKED RUN'S seed: what its guard held when it
// launched, which every later segment's rebuild of the run's surface restores
// before replaying what the run loaded over the bridge since
// ([Runner.rebuiltSurface]). A guard rebuilt empty for a run that goes on
// calling would refuse the calls that session had already unlocked. Empty for
// every other phase.
func (r *Runner) guardFor(ph phase.Phase, iteration int, surface *tools.Surface, loaded []string) armedGuard {
	// THE LOADER MUST BE ACTIVE, not merely in the universe the triggers are
	// matched over: a guard is armed only where the session can satisfy it,
	// and a loader the phase never activated is one its model may not reach.
	if r.cfg.Skills == nil || !slices.Contains(surface.Active(), skills.LoaderTool) {
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

// promptPhase maps a runner phase onto the prompt package's own.
//
// Two vocabularies because two packages own them, and the mapping is
// explicit rather than a string cast: a phase this package adds and forgets
// to map would silently become the zero value, which matches no skill and
// arms no guard.
func promptPhase(ph phase.Phase) prompts.Phase {
	switch ph {
	case phase.Execute:
		return prompts.PhaseExecute
	case phase.Review:
		return prompts.PhaseReview
	case phase.Subagent:
		return prompts.PhaseSubagent
	default:
		// Onboarding and anything later. Not a phase skills are scoped
		// to, and the empty value matches none of them — which is the
		// honest answer rather than a default that quietly picks one.
		return ""
	}
}

// catalogueSurface is the tool surface as the skill triggers see it: every
// tool in the surface's UNIVERSE, what it holds and what it can discover, and
// never its active list alone. A surface activates anything in its universe
// by name, and an executor starts with no MCP tool active, so a guard armed
// over the active list let an activated MCP tool through without the skill its
// prompt had called required.
func catalogueSurface(surface *tools.Surface) prompts.Surface {
	universe := surface.Universe()
	return prompts.Surface{Tools: universe.Names(), MCPServers: universe.MCPServers()}
}

// executorUniverse is the snapshot an executor's surface is built from: the
// registry, less the phase-scoped tools this executor must never reach.
//
// THE UNIVERSE, NOT ONLY THE ACTIVE LIST. A surface activates anything in its
// snapshot by name (`activate_tool`), and the tools to discover the prompt
// lists are read off that same snapshot — so a tool kept off the active list
// alone was still listed to the executor, one call from active: mark_onboarded,
// the one tool phaseScoped exists to keep away from it.
func (r *Runner) executorUniverse(snapshot tools.Snapshot) tools.Snapshot {
	return snapshot.Without(phaseScoped...)
}

// executorActive is what the executor is offered: every first-party tool in
// its universe ([Runner.executorUniverse]). MCP tools are reachable through
// discovery.
//
// The whole first-party surface, not a slice of it chosen in advance. Choosing
// in advance is what the planner used to do — against a catalogue it was never
// shown — and every wrong guess became a tool the actor did not have when it
// turned out to need it.
func executorActive(universe tools.Snapshot) []string {
	var out []string
	for _, e := range universe.Entries() {
		if _, fromMCP := e.FromMCP(); fromMCP {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// phaseScoped names the first-party tools that belong to ONE phase and must
// not be offered by the others.
//
// mark_onboarded is the whole list, and it earns its place: onboarding is its
// own pass, and a seat that could mark itself from inside the executor would
// permanently skip orientation — the marker suppresses the pass for ever
// after, so a single stray call means an agent that never reads its team's
// conventions and never will. Measured: the surface offered it, and a model
// that called it there produced a phase that never submitted at all.
//
// Everything else stays available on purpose. lookup_colleague, use_skill,
// query_episodes and refresh_memory are recon, which the executor now does
// itself; reflect_and_persist is deliberate too — an agent that learns
// something mid-turn should be able to keep it.
var phaseScoped = []string{MarkOnboardedTool}

// taskFor prefixes the reviewer's correction to the ask.
//
// PREFIXED to the brief rather than appended to the task description itself:
// the task text also feeds knowledge search and the episode record, both of
// which want the requester's actual ask and not the engine's running
// commentary on it.
func (r *Runner) taskFor(notes string) string {
	if strings.TrimSpace(notes) == "" {
		return r.cfg.Task
	}
	return r.cfg.Task + "\n\nThe previous round was reviewed. What to do differently:\n" + notes
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
		// Every MCP-served tool, which a phase reaches only by discovering
		// it — so a refusal can tell the tools a seat already holds from
		// the ones it has to go and find. See [turn.Surface.Discoverable].
		Discoverable: u.MCPNames(),
	}
}

// reviewArtifact is what the executor produced, handed to the reviewer.
//
// WHOLE, and that is a bug fix rather than a preference. [turn.Work.Text] is
// the run's closing reply, and the draft is at its END. A 2000-rune cut this
// used to carry kept the HEAD, which was the opening of the run's prose and
// not the draft at all. The reviewer's verdict decides whether the turn ships
// or loops, and it was being asked for that verdict on a prefix that
// frequently did not contain the work.
//
// Bounded by what one reply of the run's model can write — its own output
// ceiling, which the CLI holds it to — which is a real ceiling and the right
// place for one.
//
// "(empty)" rather than "" for a pass with no text: an absent section reads as
// a section the engine forgot to fill in.
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
	// offset is the rounds this phase ran before the loop invocation
	// draining, so the round a note is announced at is on the PHASE's
	// scale — the scale every record of the phase uses.
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
		out = append(out, toolloop.SteerNote{ID: n.ID, Message: prompts.SteerMessage(n.By, n.Text)})
	}
	return out
}

// Waiting reports a note waiting, without taking it: the loop ends a run that
// has one once its calls in flight are answered, so the next run reads it.
func (s phaseSteer) Waiting() bool { return s.r.cfg.Steer.Waiting() }

// sessionTitle is how a phase's tool session names itself to its CLI: the
// seat and the phase, for the CLI's own logs and a person reading them.
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

// steerBrief is the notes a coding run's brief takes — none on a turn nobody
// can steer.
func (r *Runner) steerBrief(ctx context.Context) []steer.Note {
	if r.cfg.Steer == nil {
		return nil
	}
	return r.cfg.Steer.Brief(ctx)
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
