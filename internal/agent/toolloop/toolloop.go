// Package toolloop drives one phase's model-and-tools conversation.
//
// It is the innermost loop of the engine: ask the model, run whatever tools it
// asked for, feed the results back, repeat until it stops asking or the round
// budget runs out. Everything above it is this loop with a different surface
// and a different prompt: the executor, the reviewer, the extension judge, a
// worker.
//
// Four things here are load-bearing and each replaced an incident:
//
//   - THE BUDGET CHECK AND THE INCREMENT ARE ONE OPERATION, and the refusal
//     names its own scope. Re-reading the caps afterwards to work out which
//     one said no is a read a peer's spend can invalidate between the refusal
//     and the report.
//   - PROGRESS IS PUBLISHED TWICE PER ROUND — once the model has spoken,
//     before its tools run, and again once they return — and both the live
//     update and the durable record are built by ONE function over one message
//     list. They used to be assembled separately, so a reasoning model streamed
//     its tool calls against an empty response and its thinking appeared only
//     when the phase ended.
//   - THE SEAT FENCE RUNS AT THE TOP OF EVERY ROUND, before any tokens are
//     spent and before anything fires, AND BEFORE EACH OF THE ROUND'S TOOL
//     CALLS, because a round is one model turn but many calls and the calls
//     are what reach outside the engine. A node whose lease moved stops there
//     rather than running the rest of the turn beside the seat's new owner.
//   - A FORCED TOOL CALL IS ENFORCED, NOT REQUESTED. Some endpoints ignore
//     tool_choice and some models think-then-stop without emitting the call,
//     which silently defeats a round the caller required to end in a tool. A
//     bounded corrective re-prompt is the difference between "the model
//     declined" and "the phase produced nothing and said it was fine".
package toolloop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tracing"
)

// maxForcedToolRetries bounds the corrective re-prompts issued when the caller
// required a tool call and the model answered with prose.
//
// Two, because the caller's round budget is the harder bound and the rescue
// and judge loops run with a budget of two themselves. A model that cannot
// emit the call in three attempts will not emit it in ten, and each attempt is
// a full priced round.
//
// PER RUN OF DECLINED ROUNDS, not per phase — the count clears the moment a
// round emits a call. The claim it rests on is about a model that keeps
// declining, and a phase that called a tool in between is not that model.
const maxForcedToolRetries = 2

// maxEmptyAnswerRetries bounds the corrective re-prompts issued when a round
// produced NEITHER prose NOR a tool call — a model that spent its whole output
// budget on hidden reasoning and stopped.
//
// One, not two, and the asymmetry with maxForcedToolRetries is the point. A
// declined tool call is a model that misread the surface, so naming the tools
// is a genuinely NEW instruction and a second attempt is worth its round. An
// empty answer is the same prompt against the same model with one sentence
// added; if that sentence does not land, a second identical nudge is the retry
// the provider contract refuses to do for exactly this reason. One also fits
// inside the smallest budget any caller declares — a worker's `max_turns` is
// validated at >= 1 and routinely set to 2 — so the corrective can never eat
// a whole delegated task's allowance.
//
// PER RUN OF EMPTY ROUNDS, and that is precisely what the argument above is
// about: the SAME prompt against the SAME model, asked twice in a row. Counted
// for the phase's lifetime instead, this bounds a different quantity — how
// many times a model may ever stall — and one stall early then disarms the
// corrective for every round after it. That is measured, not hypothetical: an
// executor on a 24-round budget stalled at round 2, filed a work item at round
// 3, had a submission bounced at round 4, and broke on the stall at round 5
// with nineteen rounds unspent — one round before the message it had just said
// it was about to send.
const maxEmptyAnswerRetries = 1

// Surface is the set of tools a phase runs against.
//
// An interface rather than a concrete registry because the phases differ in
// what they expose and the loop must not know which phase it is running: the
// executor sees the world, the reviewer sees only the tool it submits its
// decision with, a worker sees a subset of its parent's. It
// is re-read at the TOP OF EVERY ROUND so a surface mutated mid-loop — a
// meta-tool activating another tool — is visible on the next provider call
// rather than the one after.
type Surface interface {
	// ToolDefs returns the tools to offer the model this round.
	ToolDefs() []llm.ToolDef

	// Execute runs one call. It returns a ToolResult rather than an error
	// because a failing tool is ORDINARY: its message goes back to the
	// model, which is expected to react to it. A Go error from this method
	// means the surface itself broke.
	Execute(ctx context.Context, call llm.ToolCall) (ToolResult, error)

	// Phase names the phase for telemetry.
	Phase() string
}

// ToolResult is what one tool call produced.
type ToolResult struct {
	// Output is fed back to the model as the tool message's content.
	Output string

	// Failed marks a tool that reported failure. The output still goes
	// back to the model — that is the point — but the execution record
	// carries the flag so a reader can tell a tool that ran from one that
	// refused.
	Failed bool

	// Suspend stops the loop with this call UNANSWERED, for a tool whose
	// work outlives the turn (the detached sandbox). Honoured only when
	// the caller set AllowSuspend; elsewhere it is logged and ignored,
	// because a phase that never persists a partial conversation cannot
	// resume one.
	Suspend bool

	// SuspendPayload is handed back to the caller to persist.
	SuspendPayload map[string]any
}

// Execution records one tool call for the prior-work ledger and the phase
// record. It is the engine's own account of what ran, which is what makes a
// self-iterate round able to tell a delivery that already fired from one the
// model only described.
type Execution struct {
	Round int
	Name  string
	Args  map[string]any
	// Output is the tool's own output, already redacted by the surface.
	Output string
	Failed bool
}

// Narration is one round's model turn — what it reasoned and what it said —
// KEPT ATTACHED TO ITS ROUND.
//
// [Result.Text] joins every round's turn into one string, which is the right
// shape for a transcript and the wrong one for a reader: the join is lossy
// about WHICH round said what, so a consumer handed only the blob cannot put a
// round's thinking next to the tools that thinking asked for. It also can only
// be un-joined by guessing — the parts are separated by a blank line, and
// prose contains blank lines — and the reasoning of every round after the
// first ends up rendered as though it were the phase's answer.
//
// So the split is recorded where it is still known, at the point the round's
// assistant message is appended, rather than reconstructed downstream. The
// round number is recorded rather than inferred from position for the same
// reason [Execution] carries one: a resumed phase starts with a conversation
// that already contains assistant turns, so the k-th message is not round k.
type Narration struct {
	Round int
	// Reasoning is the model's thinking, when it emitted any separately.
	Reasoning string
	// Content is the visible prose of that turn.
	Content string
}

// SpendOutcome is the shared counter's answer to a spend.
//
// It carries the REFUSING SCOPE rather than just a boolean, because the
// alternative — re-reading the caps to work out which budget said no — is a
// read a peer's spend can invalidate between the refusal and the report. On a
// refusal, Used is that scope's spend as the refusal found it, before these
// tokens, and Limit is its cap.
type SpendOutcome struct {
	OK    bool
	Scope string
	Used  int
	Limit int
}

// BudgetMeter is the shared token counter a turn charges.
//
// A CHARGE IS SPENT BEFORE IT IS ASKED ABOUT. The tokens a caller charges are
// those of a model call that has already answered — a call's size is known
// only from its answer — so the provider has billed them whatever the counter
// then says. What the counter decides is whether the caller may go on: run the
// round's tools, start another round.
type BudgetMeter interface {
	// Spend counts tokens a model call has billed and reports whether the
	// budget had room for them, checking and counting in ONE operation so
	// two callers racing for the last of a budget cannot both be told yes.
	//
	// A REFUSAL IS NOT A REFUND: OK false still counts the tokens, and the
	// caller must stop. A counter that dropped a refused charge would
	// under-state the company's spend by exactly the rounds that found it
	// at its cap — the moment a cap binds — and disagree with every record
	// of those rounds, which report what the provider billed. An error
	// means the counter could not be reached, which is not a refusal and
	// must not be treated as one.
	Spend(ctx context.Context, tokens int) (SpendOutcome, error)
}

// ErrBudgetExhausted is returned when a spend was refused. Callers branch on
// it: a phase that stopped because the company ran out of tokens is a
// different event from one whose provider failed, and they are reported
// differently.
var ErrBudgetExhausted = errors.New("toolloop: token budget exhausted")

// BudgetError carries which scope refused.
type BudgetError struct {
	Scope string
	Used  int
	Limit int
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%s (%s budget: %d/%d)", ErrBudgetExhausted, e.Scope, e.Used, e.Limit)
}

// Is makes every budget breach match [ErrBudget], so a caller can test the
// class without naming which ceiling was hit.
func (e *BudgetError) Is(target error) bool { return target == ErrBudgetExhausted }

// Progress is the in-flight view of a running loop.
//
// Its whole reason to exist is the FAILURE path. When the loop returns an
// error, or panics, there is no Result to publish, and a phase with nothing
// else to report leaves only its "phase started" event: an in-flight call with
// no response and no reason. The caller holds this, so its error branch — and
// a recovery around a panic — can still publish what the phase managed: the
// conversation so far, the calls that ran, the tokens already billed, what no
// round committed, and which round it died on.
//
// Guarded because the caller reads it from a different goroutine than the one
// running the loop — which is exactly the situation it exists for.
type Progress struct {
	mu           sync.Mutex
	messages     []llm.Message
	executions   []Execution
	narration    []Narration
	inputTokens  int
	outputTokens int
	roundsUsed   int
	model        string

	// truncated and emptyAnswers are the two facts about the loop's
	// rounds that a finished [Result] reports and a snapshot used to drop.
	// A loop that died AFTER a round was cut at its output cap is still a
	// loop whose text stops short, and the failure path is the one that
	// publishes that text — so it has to say so too.
	truncated    bool
	emptyAnswers int

	// abandoned is what [Result.Abandoned] reports: every attempt at a
	// round the loop did not commit, the round it died in included.
	abandoned []Narration
}

// Snapshot freezes the partial state into a Result.
//
// ExhaustedRounds stays false: the loop did not run out of rounds, it died,
// and the phase event's own failure flag carries that. Conflating them makes
// a crashed phase read as one that worked to its limit.
func (p *Progress) Snapshot() Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Result{
		Text:         assistantText(p.messages),
		InputTokens:  p.inputTokens,
		OutputTokens: p.outputTokens,
		Executions:   append([]Execution(nil), p.executions...),
		Narration:    append([]Narration(nil), p.narration...),
		Abandoned:    append([]Narration(nil), p.abandoned...),
		RoundsUsed:   p.roundsUsed,
		Model:        p.model,
		Messages:     append([]llm.Message(nil), p.messages...),
		Truncated:    p.truncated,
		EmptyAnswers: p.emptyAnswers,
	}
}

// abandon records the attempts the loop did not commit, for an exit that no
// publish follows: a provider call that failed or panicked while its round
// was being written adds that round's last attempt, and nothing else has
// changed.
func (p *Progress) abandon(abandoned []Narration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.abandoned = append([]Narration(nil), abandoned...)
}

// rounds records the loop's round-level facts as they change, rather than at
// the next publish: a round cut at its cap can be followed by a charge that
// refuses, and that return comes before any publish would.
func (p *Progress) rounds(truncated bool, emptyAnswers int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.truncated, p.emptyAnswers = truncated, emptyAnswers
}

// start resets the view to an invocation that has done nothing yet.
//
// A caller may hand ONE Progress to several invocations — an extended phase
// runs the loop again — and until the new invocation first publishes, the
// view still describes the last one. An invocation that died before that
// point, on its first provider call, then reported the previous
// invocation's calls, tokens and rounds as its own, and a caller folding
// that onto what it already held counted them twice.
func (p *Progress) start(msgs []llm.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append([]llm.Message(nil), msgs...)
	p.executions, p.narration, p.abandoned = nil, nil, nil
	p.inputTokens, p.outputTokens, p.roundsUsed, p.model = 0, 0, 0, ""
	p.truncated, p.emptyAnswers = false, 0
}

func (p *Progress) record(msgs []llm.Message, execs []Execution, narr, abandoned []Narration,
	in, out, rounds int, model string,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append([]llm.Message(nil), msgs...)
	p.executions = append([]Execution(nil), execs...)
	p.narration = append([]Narration(nil), narr...)
	p.abandoned = append([]Narration(nil), abandoned...)
	p.inputTokens, p.outputTokens = in, out
	p.roundsUsed, p.model = rounds, model
}

// Result is one loop invocation's outcome.
type Result struct {
	Text         string
	InputTokens  int
	OutputTokens int
	Executions   []Execution
	RoundsUsed   int
	Model        string

	// Narration is per-round what Text is in aggregate. Both are published:
	// Text is what every existing consumer and every already-stored event
	// reads, and the envelope evolves additive-only.
	Narration []Narration

	// Abandoned is every attempt at a round that the loop did not commit,
	// oldest first, each numbered with the round it was an attempt at:
	//
	//   - an attempt a provider or a credential gave up on partway through,
	//     after which another attempt at the same round began (see
	//     [Partial]);
	//   - on a snapshot of a loop that failed or panicked during a round
	//     ([Progress.Snapshot]), that round's last attempt: as far as it had
	//     streamed when the provider call failed or panicked, or the whole
	//     answer when the call returned and the round went no further — its
	//     token charge failed, or a panic came before the round committed.
	//
	// SEPARATE from Narration, for the reason [Partial] is: a round's
	// narration is what the model committed to, and these are what it wrote
	// that nothing took up. A live frame shows each of them while its round
	// is open; this is what keeps them once it closes. Only a streamed call
	// leaves text behind when it fails mid-answer, so a loop without
	// [Config.StreamPartials] can hold one entry here at most: the answer
	// its round never committed.
	Abandoned []Narration

	// Partial is the round being written RIGHT NOW, present only on a live
	// snapshot and never on a finished Result. It is deliberately separate
	// from Narration: a consumer must be able to tell text that is still
	// arriving from text the model has committed to, and appending it to
	// Narration would make an in-flight fragment indistinguishable from a
	// finished round in every downstream reader.
	Partial *Partial

	// Messages is the conversation as the loop left it, including
	// everything it appended.
	Messages []llm.Message

	// ExhaustedRounds means the loop hit MaxRounds with the model still
	// asking for tools. Distinct from a clean finish, because the caller
	// may extend the cap rather than accept a truncated phase.
	ExhaustedRounds bool

	// Truncated means a round ended because the model hit its OUTPUT cap
	// rather than because it had finished — see [llm.Completion.Truncated].
	//
	// IT WAS INVISIBLE. Both backends set the stop reason and nothing in the
	// engine read it, so a sentence that stopped mid-word, a submission
	// missing the field it was called for, and a model's considered final
	// answer were the same value to every frame above this one. The
	// reviewer then judged a half-written answer as the seat's work, and the
	// one signal that would have said otherwise was on the completion the
	// whole time.
	//
	// Reported rather than raised, for the reason [llm.Completion.Truncated]
	// gives: a round that emitted whole tool calls before it ran out has
	// done real work, and whether a cut answer is fatal is the phase's
	// question. What the loop owes is that the fact reaches it.
	Truncated bool

	// EmptyAnswers counts the rounds that produced neither prose nor a tool
	// call — a model that spent its output on hidden reasoning and stopped.
	//
	// It exists because the loop now CORRECTS that round instead of the
	// cli-agent backend failing the call, and a condition that used to fire
	// llm_unavailable would otherwise become invisible: a seat whose model
	// answers nothing every round now produces quiet rescues rather than a
	// failure event. The count rides the phase record, at the layer that
	// knows which seat and which phase, so the diagnosis survives.
	EmptyAnswers int

	// Suspended and its companions are set when a tool suspended the loop.
	// The pending call is left UNANSWERED in Messages — exactly one
	// dangling tool call, which is an invariant checked on both serialize
	// and resume.
	Suspended         bool
	PendingToolCallID string
	PendingToolName   string
	SuspendPayload    map[string]any
}

// partialInterval bounds how often a round in flight republishes.
//
// 200ms. Below the ~250ms at which appearing text stops reading as live, and
// it caps a running seat at five frames a second against a socket hub that
// broadcasts one frame per progress event, throttles nothing itself, and
// drops the oldest once 512 are queued for a client. A frame per token would
// spend that entire queue on a single paragraph and evict the seat's own
// earlier rounds on the way.
const partialInterval = 200 * time.Millisecond

// Partial is a round in the middle of being written.
//
// Abandoned holds the attempts at THIS round that a provider or credential
// gave up on partway through, oldest first. They are KEPT rather than erased
// because a reader has already seen that text: making it vanish reads as a
// glitch, and "this model wrote four hundred characters and then died" is
// exactly what an operator debugging a flaky provider needs. The partial goes
// when its round commits, and the loop keeps each of these attempts on
// [Result.Abandoned], which outlives the round.
type Partial struct {
	Round     int
	Reasoning string
	Content   string
	Abandoned []Narration
}

func (p *Partial) clone() *Partial {
	if p == nil {
		return nil
	}
	dup := *p
	dup.Abandoned = append([]Narration(nil), p.Abandoned...)
	return &dup
}

// attempt is what the round in flight has streamed since its latest attempt
// began, and false when that is nothing.
func (p *Partial) attempt() (Narration, bool) {
	if p == nil || (p.Content == "" && p.Reasoning == "") {
		return Narration{}, false
	}
	return Narration{Round: p.Round, Reasoning: p.Reasoning, Content: p.Content}, true
}

// Config is one loop run.
type Config struct {
	// Provider and Surface are required.
	Provider llm.Provider
	Surface  Surface

	// Messages is the starting conversation. The loop appends to a copy;
	// the result carries the full conversation.
	Messages []llm.Message

	// MaxRounds bounds the provider calls. Required and positive.
	MaxRounds int

	// ToolChoice is passed to the provider. [llm.ToolChoiceRequired]
	// additionally turns on the corrective re-prompt: a round answering
	// with prose and no tool call is re-prompted rather than accepted.
	ToolChoice llm.ToolChoice

	// TerminateAfter names tools that end the loop once they have run
	// SUCCESSFULLY, even if the model asked for more. A phase whose
	// delivery tool has fired is finished; letting it keep going spends
	// rounds re-deciding something already done — measured at four
	// identical submissions before the round cap stopped it.
	//
	// A failed call does not terminate: its failure went back to the
	// model, and ending the phase there means the retry never happens.
	TerminateAfter []string

	// AllowSuspend permits a tool to suspend this loop. Only Execute sets
	// it — see ToolResult.Suspend.
	AllowSuspend bool

	// Budget is the shared counter. Nil disables charging, which is for
	// tests and for loops the caller has already charged.
	Budget BudgetMeter

	// Fence is checked at the top of every round, before any tokens are
	// spent, AND before each of a round's tool calls — because a round is
	// one model turn but many calls, and the calls are what reach outside
	// the engine. A non-nil error ends the loop immediately and is
	// returned unwrapped so the caller can recognise its own sentinel.
	//
	// Nil is an open fence. The engine supplies [seat.Host.Fence], which
	// closes when this node stops holding the seat's grant.
	Fence func() error

	// PartialInterval bounds how often a round in flight republishes.
	// Zero takes [partialInterval]; a caller sets it only to make the
	// coalescing deterministic, which is the one honest reason to vary it.
	PartialInterval time.Duration

	// StreamPartials asks the provider to stream, so a round's text reaches
	// OnProgress WHILE it is being written rather than only once it is
	// finished. Off leaves the provider call unary and unchanged.
	//
	// A round used to publish exactly twice — after the model answered, and
	// after its tools ran — so a phase composing a long reasoning block sat
	// visibly frozen for the whole provider call. This does not change what
	// a round MEANS: the partial is a view of a call in progress, and the
	// authoritative narration still comes from the completed round.
	StreamPartials bool

	// OnProgress receives the live view twice per round: once the model
	// has spoken and again once its tools have returned. Failures are the
	// caller's business — telemetry must never fail a phase — so this
	// returns nothing.
	OnProgress func(Result)

	// Progress is the failure view. Nil means the caller does not want
	// partial state on the error path, which is a choice rather than a
	// default: every phase in the engine passes one.
	Progress *Progress
}

func (c Config) validate() error {
	var errs []error
	if c.Provider == nil {
		errs = append(errs, errors.New("toolloop: Provider is required"))
	}
	if c.Surface == nil {
		errs = append(errs, errors.New("toolloop: Surface is required"))
	}
	if c.MaxRounds <= 0 {
		errs = append(errs, fmt.Errorf("toolloop: MaxRounds must be positive, got %d", c.MaxRounds))
	}
	return errors.Join(errs...)
}

// Run drives the loop.
//
// It returns a Result for every outcome that is not an engine failure —
// including a suspend and an exhausted round budget, both of which are things
// the phase did rather than things that went wrong. An error means the
// provider, the budget or the surface itself failed, and the caller publishes
// Progress.Snapshot() alongside it. A panic leaves the same account in
// Progress and goes on as the same panic.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	msgs := append([]llm.Message(nil), cfg.Messages...)
	if cfg.Progress != nil {
		cfg.Progress.start(msgs)
	}
	var execs []Execution
	var narration, abandoned []Narration
	var inTokens, outTokens int
	var model string
	// served distinguishes the model a COMPLETION named from the configured
	// placeholder a streamed round shows before one exists.
	var served bool
	terminators := make(map[string]struct{}, len(cfg.TerminateAfter))
	for _, name := range cfg.TerminateAfter {
		terminators[name] = struct{}{}
	}
	forcedRetries := 0
	emptyRetries := 0
	emptyAnswers := 0
	truncated := false

	var partial *Partial
	publish := func(rounds int) {
		if cfg.Progress != nil {
			cfg.Progress.record(msgs, execs, narration, abandoned, inTokens, outTokens, rounds, model)
		}
		if cfg.OnProgress != nil {
			cfg.OnProgress(Result{
				Text:         assistantText(msgs),
				InputTokens:  inTokens,
				OutputTokens: outTokens,
				Executions:   append([]Execution(nil), execs...),
				Narration:    append([]Narration(nil), narration...),
				Abandoned:    append([]Narration(nil), abandoned...),
				Partial:      partial.clone(),
				RoundsUsed:   rounds,
				Model:        model,
				Messages:     append([]llm.Message(nil), msgs...),
			})
		}
	}

	roundsUsed := 0
	// answeredRounds is how many rounds the provider has answered, and so
	// billed; uncommitted is the latest answer from the moment it arrives
	// until its round commits, and nil otherwise. Each error path below knows
	// where in its round it stands, and a panic does not: these tell the
	// account below.
	answeredRounds := 0
	var uncommitted *llm.Completion
	// roundSpan is the span of the provider call in flight, and nil between
	// calls: a panic inside the call ends it, failed, rather than leaving it
	// open and never exported.
	var roundSpan trace.Span

	// A PANIC LEAVES THE ACCOUNT AN ERROR DOES, and goes on as the same panic.
	// The caller's failure view is Progress, which this loop brings up to date
	// only when it publishes — so a panic between two publishes would leave
	// out everything since: the round in flight, an answer not yet committed,
	// the calls the round had run. Each case is recorded as the matching
	// error path records it. Re-panicking with the recovered value hands
	// whoever recovers it the same panic, with the frames it came from still
	// on the stack: they stay there until a deferred call returns.
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if roundSpan != nil {
			tracing.Fail(roundSpan, fmt.Errorf("toolloop: %s round %d: panic: %v",
				cfg.Surface.Phase(), roundsUsed, recovered))
			roundSpan.End()
		}
		if cfg.Progress != nil {
			kept := append([]Narration(nil), abandoned...)
			switch {
			case uncommitted != nil:
				// Answered, not committed: where a refused charge leaves an
				// answer.
				if narrated(uncommitted.ReasoningContent, uncommitted.Content) {
					kept = append(kept, Narration{Round: roundsUsed,
						Reasoning: uncommitted.ReasoningContent, Content: uncommitted.Content})
				}
				cfg.Progress.record(msgs, execs, narration, kept, inTokens, outTokens, answeredRounds, model)
			case partial != nil:
				// A streamed answer still arriving: where a failed provider
				// call leaves its attempt.
				if attempt, ok := partial.attempt(); ok {
					kept = append(kept, attempt)
				}
				cfg.Progress.abandon(kept)
			default:
				cfg.Progress.record(msgs, execs, narration, kept, inTokens, outTokens, answeredRounds, model)
			}
		}
		panic(recovered)
	}()

	for round := range cfg.MaxRounds {
		roundsUsed = round + 1

		// The fence first, at the cheapest possible point: nothing has
		// been spent this round and nothing has fired.
		if cfg.Fence != nil {
			if err := cfg.Fence(); err != nil {
				return nil, err
			}
		}

		// Re-read every round, so a surface mutated by this round's own
		// tools is visible on the next call rather than the one after.
		tools := cfg.Surface.ToolDefs()
		choice := cfg.ToolChoice
		if choice == "" && len(tools) > 0 {
			choice = llm.ToolChoiceAuto
		}

		// ONE SPAN PER ROUND, around the provider call only. The round is
		// the unit whose LATENCY an operator cares about — it is the wait
		// a seat spends on a model — and it is the one thing no event
		// records: agent_turn_progress reports the round's content twice
		// per round, and never how long it took.
		//
		// This is also the ONLY layer of the provider stack that is
		// spanned. Below it sit the fallback chain, each member's backend
		// and the credential pool's rotation, which on a three-member
		// chain over a four-key pool would nest up to twelve spans per
		// round and tell a reader nothing they could act on. Which member
		// and which credential answered is on the completion, and reaches
		// the record through the phase event.
		var roundCtx context.Context
		roundCtx, roundSpan = tracing.Start(ctx, "agent.toolloop", "llm.round",
			attribute.String("crewlet.phase", cfg.Surface.Phase()),
			attribute.Int("crewlet.round", roundsUsed))
		// The CONFIGURED identity, as a placeholder, so a streamed round
		// is not attributed to no model at all. Streaming publishes while
		// the call is still open, and `model` is only latched from the
		// completion once it returns — so every frame of a streamed round
		// reported an empty model, and a running row showed a dash where
		// its model should be. Overwritten below by the model that
		// actually served the call, which is the billable fact.
		if model == "" {
			model = cfg.Provider.Model()
		}

		// One partial per round, replaced by the round's real narration
		// the moment the model finishes.
		var onDelta func(llm.Delta)
		every := cfg.PartialInterval
		if every <= 0 {
			every = partialInterval
		}
		if cfg.StreamPartials && cfg.OnProgress != nil {
			partial = &Partial{Round: roundsUsed}
			// ZERO, so the FIRST fragment publishes immediately. Seeding
			// this to now swallows it for a whole interval, which means a
			// short round shows nothing at all and a long one begins with
			// a fifth of a second of apparent stillness — the exact
			// symptom streaming exists to remove.
			var last time.Time
			onDelta = func(d llm.Delta) {
				if d.Restart {
					// The attempt so far was abandoned. Kept rather than
					// erased — on the partial for the live view, and on
					// the list the Result reports (see [Partial]) — and
					// the replacement starts from empty so two
					// half-answers never concatenate.
					if attempt, ok := partial.attempt(); ok {
						partial.Abandoned = append(partial.Abandoned, attempt)
						abandoned = append(abandoned, attempt)
					}
					partial.Reasoning, partial.Content = "", ""
					publish(roundsUsed)
					last = time.Now()
					return
				}
				partial.Reasoning += d.Reasoning
				partial.Content += d.Content
				// COALESCED on the clock, not on every fragment. The
				// socket hub broadcasts one frame per progress event with
				// no throttling of its own and drops the oldest past 512
				// queued, so a frame per token would spend a running
				// seat's whole queue on one paragraph. See partialInterval.
				//
				// A tail shorter than one interval is not published, and
				// nothing is lost by that: the round commits immediately
				// after and its narration carries the whole text.
				if time.Since(last) < every {
					return
				}
				last = time.Now()
				publish(roundsUsed)
			}
		}

		completion, err := cfg.Provider.Complete(roundCtx, llm.Request{
			Messages:   msgs,
			Tools:      tools,
			ToolChoice: choice,
			OnDelta:    onDelta,
		})
		if err != nil {
			tracing.Fail(roundSpan, err)
			roundSpan.End()
			// THE ATTEMPT IN FLIGHT joins the abandoned ones: its round
			// will never commit it, and the snapshot the caller publishes
			// on this error is the only record that can hold it.
			if attempt, ok := partial.attempt(); ok {
				abandoned = append(abandoned, attempt)
				if cfg.Progress != nil {
					cfg.Progress.abandon(abandoned)
				}
			}
			return nil, fmt.Errorf("toolloop: %s round %d: %w", cfg.Surface.Phase(), roundsUsed, err)
		}
		// BILLED THE MOMENT IT ARRIVES, so an account taken from here on counts
		// it, whatever then becomes of the round.
		inTokens += completion.InputTokens
		outTokens += completion.OutputTokens
		answeredRounds, uncommitted = roundsUsed, completion
		roundSpan.SetAttributes(
			attribute.String("crewlet.model", completion.Model),
			attribute.Int("crewlet.input_tokens", completion.InputTokens),
			attribute.Int("crewlet.output_tokens", completion.OutputTokens),
			attribute.Int("crewlet.cache_read_tokens", completion.CacheRead),
			attribute.Int("crewlet.tool_calls", len(completion.ToolCalls)))
		roundSpan.End()
		roundSpan = nil
		if !served && completion.Model != "" {
			// The completion names the model that actually served this
			// round, which is what the per-model token breakdown is built
			// from; the provider's own name is its CONFIGURED identity and
			// only stands in for a backend that filled nothing in.
			//
			// It OVERRIDES the placeholder set before the call, and latches
			// on the first completion that names one — so a streamed round
			// has something to show while it writes, and the billable fact
			// still wins the moment it exists.
			model, served = completion.Model, true
		}
		if model == "" {
			model = cfg.Provider.Model()
		}
		if completion.Truncated() {
			// LATCHED, never per-round: the phase's question is whether
			// anything it is about to stand behind was cut, and a round
			// whose prose was severed taints the phase's own text
			// however many clean rounds follow it. This package logs
			// nothing by design — the frame that publishes the phase
			// event is the one that says so.
			truncated = true
			if cfg.Progress != nil {
				cfg.Progress.rounds(truncated, emptyAnswers)
			}
		}

		// Charge BEFORE running the tools this round asked for. A round
		// whose spend is refused must not also have fired its side
		// effects — the refusal is the whole point, and tools are where
		// the irreversible things happen.
		if err = charge(ctx, cfg.Budget, completion.TotalTokens()); err != nil {
			// THE ROUND IS NOT COMMITTED, and what it said and what it cost
			// are still facts about the phase: the provider answered and
			// billed its tokens, and the counter refusing them, or not
			// answering, changes neither. So the answer joins the abandoned
			// attempts and the tokens stay counted, in the snapshot the
			// caller publishes on this error — recorded here, because no
			// publish follows this return. A refusing meter has counted the
			// same tokens (see [BudgetMeter]), so the record and the counter
			// agree on what this round cost.
			if narrated(completion.ReasoningContent, completion.Content) {
				abandoned = append(abandoned, Narration{
					Round: roundsUsed, Reasoning: completion.ReasoningContent, Content: completion.Content,
				})
			}
			if cfg.Progress != nil {
				cfg.Progress.record(msgs, execs, narration, abandoned, inTokens, outTokens, roundsUsed, model)
			}
			return nil, err
		}

		msgs = append(msgs, llm.Message{
			Role:             llm.RoleAssistant,
			Content:          completion.Content,
			ReasoningContent: completion.ReasoningContent,
			ThinkingBlocks:   completion.ThinkingBlocks,
			ToolCalls:        completion.ToolCalls,
		})
		// Recorded HERE, beside the message it describes, because this is
		// the last frame that knows which round the turn belongs to. The
		// round is `roundsUsed` — ONE-BASED, matching [Execution.Round] — so a
		// round's narration and the calls it asked for carry the same number
		// and a reader can put them together without a second rule. (Note it
		// does NOT match AgentTurnProgress.RoundNum, which is zero-based and
		// is a different field about a different thing: how many rounds have
		// happened, not which one this is.)
		// The round is committed: its narration is authoritative now, and
		// the partial (with any abandoned attempts) goes. Cleared BEFORE
		// the publish below so the live view never shows a finished round
		// and a fragment of the same round at once.
		partial, uncommitted = nil, nil
		if narrated(completion.ReasoningContent, completion.Content) {
			narration = append(narration, Narration{
				Round:     roundsUsed,
				Reasoning: strings.TrimSpace(completion.ReasoningContent),
				Content:   strings.TrimSpace(completion.Content),
			})
		}

		// The model has spoken: publish before the tools run, so its
		// reasoning and prose reach the live view now rather than after
		// the slowest tool returns.
		publish(roundsUsed)

		if len(completion.ToolCalls) == 0 {
			// Counted whichever corrective follows, and counted for
			// rounds that get none: the number a phase record carries
			// is "rounds that reached nobody", which is the question
			// agent/turn already asks about a turn.
			answeredNothing := strings.TrimSpace(completion.Content) == ""
			if answeredNothing {
				emptyAnswers++
				if cfg.Progress != nil {
					cfg.Progress.rounds(truncated, emptyAnswers)
				}
			}

			// A required tool call that did not arrive. Some endpoints
			// ignore tool_choice and some models think-then-stop, and
			// accepting this as a clean finish is how a forced round
			// silently produces nothing.
			//
			// THIS CORRECTIVE WINS OUTRIGHT for a caller that required a
			// call, empty round or not: "call one of these tools" is
			// strictly the better instruction for a phase whose only
			// output IS a call, and it already covers the model that
			// thought and stopped. Letting both fire would tax every
			// forced caller's round budget — the reviewer's four, the
			// onboarding pass's — for an instruction they already got.
			if cfg.ToolChoice == llm.ToolChoiceRequired {
				if forcedRetries < maxForcedToolRetries {
					forcedRetries++
					msgs = append(msgs, llm.Message{
						Role:    llm.RoleUser,
						Content: forcedToolCorrective(tools),
					})
					continue
				}
				break
			}

			// A ROUND THAT REACHED NOBODY IS NOT A FINISH. No tool call
			// and no prose is a model that spent its output on hidden
			// reasoning — the shape every backend now hands back for it
			// (an empty Content), rather than the transport error the
			// cli-agent backend used to raise. Correcting it is the
			// loop's job and not the provider's: this is the frame that
			// holds the conversation, so it is the only one that can
			// ask again without inventing a second prompt contract the
			// operator cannot see.
			//
			// Keyed on Content alone. Reasoning is not an answer that
			// reached anybody, so a thinking-only round from any
			// backend is the same failure and gets the same corrective.
			if answeredNothing && emptyRetries < maxEmptyAnswerRetries {
				emptyRetries++
				msgs = append(msgs, llm.Message{
					Role:    llm.RoleUser,
					Content: emptyAnswerCorrective,
				})
				continue
			}
			break
		}

		// A ROUND THAT EMITTED A CALL CLEARS BOTH STALL ALLOWANCES. Each
		// corrective above bounds a RUN of rounds that produced nothing,
		// never the phase's lifetime: a model that just asked for a tool
		// has demonstrated it can, so its next stall is a new stall and
		// earns its own nudge.
		//
		// Cleared on the CALL, not on the call's success. A tool that
		// returned an error — a submission the engine refused, a write the
		// vendor rejected — is a round the model has to read and answer,
		// which is the opposite of a model that has stopped responding.
		// Gating this on a successful result would withdraw the nudge
		// exactly where the next round matters most.
		forcedRetries, emptyRetries = 0, 0

		suspended, pendingID, pendingName, payload, err := runCalls(
			ctx, cfg, completion.ToolCalls, roundsUsed, &msgs, &execs)
		if err != nil {
			// THE CALLS THAT RAN ARE PART OF THE ACCOUNT. A fence that closed,
			// or a surface that broke, partway through a round ends the loop
			// after the calls before it had run — and those calls reached
			// outside the engine. No publish follows this return, so the
			// failure view is brought up to date here.
			if cfg.Progress != nil {
				cfg.Progress.record(msgs, execs, narration, abandoned, inTokens, outTokens, roundsUsed, model)
			}
			return nil, err
		}

		// The round's tools have returned: publish again so the live row
		// fills in rather than waiting for the next model turn.
		publish(roundsUsed)

		if suspended {
			return &Result{
				Truncated:         truncated,
				Text:              assistantText(msgs),
				InputTokens:       inTokens,
				OutputTokens:      outTokens,
				Executions:        execs,
				Narration:         narration,
				Abandoned:         abandoned,
				RoundsUsed:        roundsUsed,
				Model:             model,
				Messages:          msgs,
				EmptyAnswers:      emptyAnswers,
				Suspended:         true,
				PendingToolCallID: pendingID,
				PendingToolName:   pendingName,
				SuspendPayload:    payload,
			}, nil
		}

		if ranTerminator(execs, terminators, roundsUsed) {
			break
		}
	}

	exhausted := roundsUsed == cfg.MaxRounds && lastAskedForTools(msgs)
	return &Result{
		Text:            assistantText(msgs),
		InputTokens:     inTokens,
		OutputTokens:    outTokens,
		Executions:      execs,
		Narration:       narration,
		Abandoned:       abandoned,
		RoundsUsed:      roundsUsed,
		Model:           model,
		Messages:        msgs,
		ExhaustedRounds: exhausted,
		EmptyAnswers:    emptyAnswers,
		Truncated:       truncated,
	}, nil
}

// runCalls executes one round's tool calls in order, appending a tool message
// for each. It reports a suspend rather than returning one as an error,
// because suspending is something the phase DID.
func runCalls(
	ctx context.Context,
	cfg Config,
	calls []llm.ToolCall,
	round int,
	msgs *[]llm.Message,
	execs *[]Execution,
) (suspended bool, pendingID, pendingName string, payload map[string]any, err error) {
	for _, call := range calls {
		// THE FENCE AGAIN, PER CALL. The round check is one per model
		// turn, and a round is every tool the model asked for in it — a
		// sandbox launch, a search, three tracker writes and a chat post,
		// serially, which is minutes. Checking only at the top of the
		// round would let a node that lost the seat run out the whole of
		// the round it was already in, and it is the calls, not the model
		// round, that reach outside the engine.
		//
		// Before Execute rather than after, so a closed fence stops the
		// NEXT call rather than discarding the one that already ran: the
		// surface has recorded everything up to here, and the caller
		// reads that record to decide whether this turn may be retried.
		if cfg.Fence != nil {
			if err := cfg.Fence(); err != nil {
				return false, "", "", nil, err
			}
		}
		res, execErr := cfg.Surface.Execute(ctx, call)
		if execErr != nil {
			return false, "", "", nil, fmt.Errorf(
				"toolloop: surface failed on %q: %w", call.Name, execErr)
		}

		if res.Suspend {
			if !cfg.AllowSuspend {
				// A phase that never persists a partial conversation
				// cannot resume one, so honouring this would strand the
				// turn with a dangling call nothing will ever answer.
				// The output still goes back, so the model sees a
				// refusal rather than silence.
				res.Suspend = false
				res.Failed = true
				res.Output = "This tool cannot suspend in this phase. " + res.Output
			} else {
				// The call is left UNANSWERED on purpose: exactly one
				// dangling tool call is the invariant a resume checks.
				return true, call.ID, call.Name, res.SuspendPayload, nil
			}
		}

		*execs = append(*execs, Execution{
			Round: round, Name: call.Name, Args: call.Arguments,
			Output: res.Output, Failed: res.Failed,
		})
		*msgs = append(*msgs, llm.Message{
			Role:       llm.RoleTool,
			Content:    res.Output,
			ToolCallID: call.ID,
			Name:       call.Name,
		})
	}
	return false, "", "", nil, nil
}

// charge meters one answered round's tokens, and says whether the loop may run
// that round's tools. A refusal stops the round and refunds nothing: the
// provider billed the tokens and the meter has counted them (see
// [BudgetMeter]).
func charge(ctx context.Context, meter BudgetMeter, tokens int) error {
	if meter == nil || tokens <= 0 {
		return nil
	}
	outcome, err := meter.Spend(ctx, tokens)
	if err != nil {
		// Unreachable counter is NOT a refusal. Treating it as one stops
		// every turn in the company on a store blip; treating a refusal
		// as unreachable would spend past the cap. They are different
		// answers and the caller decides.
		return fmt.Errorf("toolloop: charge %d tokens: %w", tokens, err)
	}
	if outcome.OK {
		return nil
	}
	scope := outcome.Scope
	if scope == "" {
		scope = "org"
	}
	return &BudgetError{Scope: scope, Used: outcome.Used, Limit: outcome.Limit}
}

// ranTerminator reports whether this round ran a tool that ends the loop.
//
// A terminator that FAILED does not terminate. Its failure went back to the
// model, which is expected to fix it and try again — ending the phase there
// means the retry never happens and the phase finishes having produced
// nothing, on the one class of failure a model can reliably correct.
func ranTerminator(execs []Execution, terminators map[string]struct{}, round int) bool {
	if len(terminators) == 0 {
		return false
	}
	for _, e := range execs {
		if e.Round != round || e.Failed {
			continue
		}
		if _, ok := terminators[e.Name]; ok {
			return true
		}
	}
	return false
}

// lastAskedForTools reports whether the final assistant message was still
// asking for tools, which is what makes an ended loop EXHAUSTED rather than
// finished. A loop that stopped because the model stopped asking used its last
// round legitimately.
func lastAskedForTools(msgs []llm.Message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			return len(msgs[i].ToolCalls) > 0
		}
	}
	return false
}

// forcedToolCorrective is the re-prompt for a round that had to end in a tool
// call and did not. It names the available tools because a model that answered
// with prose has usually misread the surface rather than refused it.
func forcedToolCorrective(tools []llm.ToolDef) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	if len(names) == 0 {
		return "You must call a tool to continue, but no tools are available. " +
			"Report this as an error."
	}
	return "You must respond by calling one of these tools, not with prose: " +
		strings.Join(names, ", ") + "."
}

// emptyAnswerCorrective is the re-prompt for a round that produced neither a
// tool call nor a word of prose.
//
// It names the CAUSE rather than scolding, because the model did work — the
// round bills hundreds of output tokens — and the only thing that went wrong
// is that none of it was written down where anyone could read it.
const emptyAnswerCorrective = "Your last reply was empty: you produced no visible " +
	"response and called no tool. Whatever you worked out, write it in the response " +
	"itself, or call a tool to act on it."

// narrated reports whether a round's turn said anything worth recording. A
// round that only emitted tool calls has no narration, and an empty entry
// would render as a blank paragraph above its own tools.
func narrated(reasoning, content string) bool {
	return strings.TrimSpace(reasoning) != "" || strings.TrimSpace(content) != ""
}

// assistantText renders a conversation's assistant turns as ONE displayable
// string, reasoning included, wrapped so a reader can tell it apart.
//
// This is the SINGLE builder of the response shown on both the per-round live
// update and the durable phase record. One function over one message list is
// what makes the dashboard's streaming row and the turn you expand afterwards
// the same text — they were assembled separately once, so a reasoning model
// streamed its tool calls against an empty response and its thinking appeared
// only when the phase ended.
func assistantText(msgs []llm.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		if part := FormatReasoningAndContent(m.ReasoningContent, m.Content); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}

// FormatReasoningAndContent is the wire-format rule for a model turn that
// carries reasoning: the reasoning wrapped in <think> tags immediately before
// that round's visible content.
//
// Exported because it is a GRAMMAR, not a helper — the dashboard parses these
// tags to render a reasoning block, so a second definition anywhere would put
// a reader's thinking in the wrong place or lose it.
func FormatReasoningAndContent(reasoning, content string) string {
	reasoning, content = strings.TrimSpace(reasoning), strings.TrimSpace(content)
	switch {
	case reasoning == "":
		return content
	case content == "":
		return "<think>" + reasoning + "</think>"
	default:
		return "<think>" + reasoning + "</think>\n" + content
	}
}
