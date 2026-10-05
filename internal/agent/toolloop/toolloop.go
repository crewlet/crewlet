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
//   - PROGRESS IS PUBLISHED TWICE PER ROUND AND ONCE PER CALL — once the
//     model has spoken, before its tools run, again BEFORE EACH CALL naming
//     the call that is about to run, and once they all return — and both the
//     live update and the durable record are built by ONE function over one
//     message list. They used to be assembled separately, so a reasoning model
//     streamed its tool calls against an empty response and its thinking
//     appeared only when the phase ended. The per-call frame is what makes a
//     round's calls visible WHILE they run: they are serial, a sandbox launch
//     or a slow MCP server takes minutes, and a live view that learned of a
//     call only once the whole round returned showed a seat doing nothing
//     for exactly the stretch somebody was watching it.
//   - THE LOOP IS THE CLOCK. Every round's provider call and every tool call
//     is timed here, where the call is made, and carried on the record as a
//     start and a duration. A reader that reconstructs timing from event
//     arrival subtracts two publishers' clocks and a queue's latency, and a
//     round's cache tokens exist nowhere else: the completion reports them
//     and this is the one frame that sees every completion.
//   - THE SEAT FENCE RUNS AT THE TOP OF EVERY ROUND, before any tokens are
//     spent and before anything fires, AND BEFORE EACH OF THE ROUND'S TOOL
//     CALLS, because a round is one model turn but many calls and the calls
//     are what reach outside the engine. A node whose lease moved stops there
//     rather than running the rest of the turn beside the seat's new owner.
//   - A PERSON'S NOTE ENTERS AT THE ROUND BOUNDARY AND NOWHERE ELSE —
//     immediately after the fence, when every call the previous round made
//     has its answer, as a user message. Anywhere later it could sit
//     between a call and its result, which a provider rejects and a model
//     reads as the tool's output; before the fence it would be read by a
//     turn that is about to end. See internal/agent/steer.
//   - A PHASE THAT FINISHES BY A CALL IS ASKED AGAIN WHEN A ROUND ENDS
//     WITHOUT ONE. A loop that declares terminators ([Config.TerminateAfter])
//     has said how it ends — a successful call to one of them — so a round
//     of prose there is not a finish, whatever tool_choice the request
//     carried: it is a model that wrote its report where nobody reads it
//     (a submission's arguments typed out as a JSON block is the measured
//     case). Some endpoints ignore tool_choice, some models think-then-stop,
//     and some reject a forced choice outright, so the call is ENFORCED HERE
//     rather than requested of the provider: a bounded corrective re-prompt
//     naming what finishes the phase is the difference between "the model
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

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tracing"
)

// MaxFinishingCorrectives bounds the FINISHING correctives: the re-prompts a
// loop that declares terminators issues when a round ended without the call
// that finishes it — in prose, or with nothing at all.
//
// Exported because the bound is the PHASE's, not one invocation's: a caller
// that continues a phase past a corrective this loop withheld on its last
// round ([Result.Withheld]) sizes the continuation by it, and seeds the next
// invocation's count with [Config.CorrectivesSpent], so a phase split across
// two invocations is asked no more often than one that ran in a single one.
//
// Two, and every one of them an IDENTICAL message, which is not the waste it
// would be elsewhere. In a loop that finishes by a call, a round without the
// call cannot end the phase except into its caller's rescue — `incomplete` for
// the executor, a whole extra executor round for the reviewer, an unmarked seat
// that re-runs its onboarding on every turn — so a second send is weighed
// against that, not against nothing. And it caps there because a model that
// cannot emit the call in three attempts will not emit it in ten, while each
// attempt is a full priced round charged like any other. Against the budgets
// that arm it: the executor's default 24 rounds lose at most 2 to a run of
// declines; the reviewer's 4 keep 1 for the submission the corrective asks
// for; a worker with `max_turns: 1` has no round left to read a corrective at
// all, and gets none — no corrective is ever sent on a round that has none
// after it.
//
// PER RUN OF DECLINED ROUNDS, not per phase — the count clears the moment a
// round emits a call. The claim it rests on is about a model that keeps
// declining, and a phase that called a tool in between is not that model.
const MaxFinishingCorrectives = 2

// maxEmptyAnswerRetries bounds the corrective re-prompts issued when a round
// produced NEITHER prose NOR a tool call — a model that spent its whole output
// budget on hidden reasoning and stopped — in a loop that does NOT finish by a
// call. A loop that does gets the finishing corrective for that round instead,
// under [MaxFinishingCorrectives].
//
// One, not two, and the asymmetry with MaxFinishingCorrectives is keyed on the
// loop's CONTRACT rather than on the round. Here a prose answer is a legitimate
// finish — today that is only a worker whose submission tool a granted tool
// shadowed — so whatever the model writes next IS the phase's result, and the
// rescue a second nudge would be bought against does not exist: a phase that
// still answers nothing simply ends with nothing, which its record counts.
//
// PER RUN OF EMPTY ROUNDS, like the finishing allowance. Counted for the
// phase's lifetime instead, this bounds a different quantity — how many times
// a model may ever stall — and one stall early then disarms the corrective for
// every round after it. That is measured, not hypothetical: an executor on a
// 24-round budget stalled at round 2, filed a work item at round 3, had a
// submission bounced at round 4, and broke on the stall at round 5 with
// nineteen rounds unspent — one round before the message it had just said it
// was about to send.
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

	// Refusal is a failed call's machine-readable class, when the frame
	// that refused wrote the sentence itself — see [mcp.Refusal]. Empty on
	// success and on a third-party MCP server's failure, which the engine
	// does not classify by guessing at prose it did not write. This loop
	// never reads it — a model reads the sentence — so it is for a caller
	// that dispatches through a surface with no model behind it.
	Refusal mcp.Refusal

	// Suspend stops the loop with this call UNANSWERED, for a tool whose
	// work outlives the turn (the detached sandbox). Honoured only when
	// the caller set AllowSuspend; elsewhere it is logged and ignored,
	// because a phase that never persists a partial conversation cannot
	// resume one.
	Suspend bool

	// SuspendPayload is handed back to the caller to persist.
	SuspendPayload map[string]any

	// Origin is where the tool that ANSWERED came from — the registry's
	// grammar, `builtin` or `mcp:<server>` — and Server the bare MCP server
	// name for an `mcp:` origin. Set by the surface, which is the one frame
	// that resolved the name to a registered tool; this loop only carries
	// them onto the [Execution].
	//
	// EMPTY ON A CALL NO TOOL ANSWERED: an unknown name, one not offered to
	// this surface, or one a guard refused. The origin names who served the
	// call, and on those paths nobody did — the surface refused it before
	// any tool ran — so an origin there would attribute the refusal to a
	// server that never saw the request.
	Origin string
	Server string
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

	// StartedAt is when the call was handed to the surface, in UTC, and
	// Duration how long the surface took to answer — measured on the
	// monotonic clock, so a wall-clock correction mid-call cannot report a
	// negative or inflated one.
	//
	// Measured HERE, around [Surface.Execute], because a round's calls are
	// SERIAL and the question a reader asks of a slow round is which call
	// held it: the round's own span covers the provider call only, and the
	// phase's covers everything. Both are zero on an execution this build
	// did not time — a resumed phase's pre-suspend rows written by an older
	// build, an agent-mode run's bridged calls — and zero means "not
	// measured", never "instant".
	StartedAt time.Time
	Duration  time.Duration

	// Origin and Server are [ToolResult.Origin] and [ToolResult.Server]:
	// who answered the call, empty when nobody did.
	Origin string
	Server string
}

// Round is one provider call of the loop: when it was made, how long the model
// took to answer, who answered and what it cost.
//
// THE MODEL'S HALF OF A ROUND ONLY. The round's tool calls are timed on their
// own [Execution] rows, because they are serial and each is its own question
// ("which call held this round?"); folding them into the round's duration would
// make a slow model and a slow tool the same number. A round's wall clock is
// therefore its StartedAt to the end of its last call, and a reader has both
// halves to hand.
//
// Recorded for every round whose completion arrived, including the rounds a
// corrective re-prompt follows: each is a priced provider call and a reader
// adding up where the time went needs all of them.
type Round struct {
	// Round is one-based, on the same scale as [Execution.Round] and
	// [Narration.Round], so the three lists join on it.
	Round int

	// StartedAt is when the provider call was made, in UTC, and Duration
	// how long it took to answer, on the monotonic clock.
	StartedAt time.Time
	Duration  time.Duration

	// Model is the model the completion says served this round — the
	// billable fact, which a fallback chain makes differ between rounds.
	Model string

	// InputTokens and OutputTokens are this round's own spend, and
	// CacheRead and CacheWrite the share of InputTokens the provider's
	// prompt cache served or stored (see [llm.Completion]). A breakdown of
	// InputTokens, never an addition to it.
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int

	// ToolCalls is how many calls the model asked for this round — the
	// count of [Execution] rows on this round, unless the loop stopped
	// before running them all (a suspend, a closed fence).
	ToolCalls int
}

// RunningCall is the tool call in flight: named on the live view BEFORE the
// surface runs it and cleared once it returns.
//
// It is the one thing a round's records cannot say, because an [Execution] is
// appended only once its call has answered. Without it, a seat whose round
// launched a coding run or queried a slow MCP server showed its model's last
// words and nothing after them for as long as the call took — indistinguishable
// from a seat that had stalled.
type RunningCall struct {
	// Round is the round the call belongs to, on the [Execution.Round]
	// scale.
	Round int
	Name  string
	Args  map[string]any
	// StartedAt is when it was handed to the surface, in UTC — what a
	// reader subtracts from now to say "running for 2m 41s".
	StartedAt time.Time
}

// Steerer is a running turn's notes, as the loop reads them.
//
// An interface rather than the box itself because the loop must not know who
// sent a note or how it is worded to a model: the caller renders each one and
// records its delivery, and the loop's whole part is WHERE it lands — see
// [Config.Steer].
type Steerer interface {
	// Drain takes every note waiting, rendered as the user message the
	// model reads, for the round about to run — one-based, on this
	// invocation's scale.
	Drain(round int) []SteerNote
}

// SteerNote is one note, rendered.
type SteerNote struct {
	// ID is the note's identity, carried onto the round's [SteerMark].
	ID string
	// Message is the user message the model reads.
	Message string
}

// SteerMark records that a note entered the conversation, and at which round:
// the round whose provider call was the first to read it.
type SteerMark struct {
	// Round is on the same one-based scale as [Execution.Round], so a
	// reader puts the note beside the round it changed.
	Round int
	ID    string
}

// roundKey carries the round a tool call runs in on the context the surface is
// handed; see [CallRound].
type roundKey struct{}

// roundOffsetKey carries the rounds a caller's phase already ran before this
// invocation of the loop; see [WithRoundOffset].
type roundOffsetKey struct{}

// WithRoundOffset declares that the phase this loop runs for has already run
// `prior` rounds before this invocation, so the round [CallRound] reports to a
// tool is on the PHASE's scale rather than the invocation's.
//
// It exists because a phase can run the loop more than once — an extension
// continues it, a resumed executor re-enters it — and the loop numbers its own
// rounds from 1 every time. The records it returns are renumbered by the
// caller afterwards ([Result] is a value it can shift), but a tool that asks
// its round mid-call cannot wait for that: a worker spawned in extension round
// 1 of a twenty-round phase would otherwise nest itself under round 1.
func WithRoundOffset(ctx context.Context, prior int) context.Context {
	if prior <= 0 {
		return ctx
	}
	return context.WithValue(ctx, roundOffsetKey{}, prior)
}

// CallRound is the round the calling tool runs in, on the phase's scale, and
// false outside a tool call this loop made.
//
// Read by a tool whose own record has to say which round asked for it — a
// delegate call's workers nest under the round that spawned them — and never
// by the loop itself, which knows its round without asking.
func CallRound(ctx context.Context) (int, bool) {
	round, ok := ctx.Value(roundKey{}).(int)
	return round, ok
}

// withCallRound stamps the round a call runs in, offset onto the phase's scale.
func withCallRound(ctx context.Context, round int) context.Context {
	prior, _ := ctx.Value(roundOffsetKey{}).(int)
	return context.WithValue(ctx, roundKey{}, prior+round)
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

	// Declined marks a round that answered with prose and NO tool call in
	// a loop that had to end in one — it declares terminators. Recorded on the round rather than inferred
	// downstream, because a reader cannot tell this round from an ordinary
	// last word: both are prose with no calls, and only the loop knows the
	// phase it belonged to could not finish that way.
	//
	// Whether the loop asked again is not a second flag: a later round
	// exists exactly when it did. A declined round that is the phase's
	// LAST is one the bound or the budget left unanswered, and the phase
	// ended without its submission.
	Declined bool
}

// SpendOutcome is the shared counter's answer to a spend.
//
// It carries the REFUSING SCOPE rather than just a boolean, because the
// alternative — re-reading the caps to work out which budget said no — is a
// read a peer's spend can invalidate between the refusal and the report.
//
// And the WINDOW it refused in, for the same reason and one more: a ceiling is
// per calendar window (the day, the ISO week or the month on the company's
// clock), so "the company is out" says nothing about when it will have room
// again. Period, Window and ResetsAt are the refusing window as the counter
// named it — the one that ends last where several refused, which is when the
// scope next admits the charge without a ceiling being raised. All three are
// empty for a refusal that has no calendar window, which is a sub-agent
// call's own slice.
type SpendOutcome struct {
	OK    bool
	Scope string
	Used  int
	Limit int

	// Period is the refusing window's period, Window its label
	// (`2026-09-23`, `2026-W39`, `2026-09`) and ResetsAt the instant it
	// turns over, when its allowance comes back.
	Period   period.Period
	Window   string
	ResetsAt time.Time
}

// BudgetMeter is the shared token counter a turn charges.
type BudgetMeter interface {
	// Spend checks and increments in ONE operation against the shared
	// counter. An error means the counter could not be reached, which is
	// not the same as a refusal and must not be treated as one.
	Spend(ctx context.Context, tokens int) (SpendOutcome, error)
}

// ErrBudgetExhausted is returned when a spend was refused. Callers branch on
// it: a phase that stopped because the company ran out of tokens is a
// different event from one whose provider failed, and they are reported
// differently.
var ErrBudgetExhausted = errors.New("toolloop: token budget exhausted")

// BudgetError carries which scope refused, and in which calendar window — see
// [SpendOutcome], whose fields it carries unchanged.
type BudgetError struct {
	Scope string
	Used  int
	Limit int

	Period   period.Period
	Window   string
	ResetsAt time.Time
}

// Error names the window and when it resets where the refusal has one, because
// that is the half an operator acts on: a day that resets tonight and a month
// that resets in three weeks are different decisions about raising a ceiling.
func (e *BudgetError) Error() string {
	if e.Window == "" {
		return fmt.Sprintf("%s (%s budget: %d/%d)", ErrBudgetExhausted, e.Scope, e.Used, e.Limit)
	}
	return fmt.Sprintf("%s (%s %s budget, window %s: %d/%d, resets %s)", ErrBudgetExhausted,
		e.Scope, e.Period, e.Window, e.Used, e.Limit, e.ResetsAt.UTC().Format(time.RFC3339))
}

// Is makes every budget breach match [ErrBudget], so a caller can test the
// class without naming which ceiling was hit.
func (e *BudgetError) Is(target error) bool { return target == ErrBudgetExhausted }

// Progress is the in-flight view of a running loop.
//
// Its whole reason to exist is the FAILURE path. When the loop returns an
// error there is no Result to publish, so a phase that died used to leave
// behind only its "phase started" event — the dashboard showed an in-flight
// call with no response and no reason. The caller holds this, so its error
// branch can still publish what the phase managed: the conversation so far,
// the calls that ran, the tokens already billed, and which round it died on.
//
// Guarded because the caller reads it from a different goroutine than the one
// running the loop — which is exactly the situation it exists for.
type Progress struct {
	mu           sync.Mutex
	messages     []llm.Message
	executions   []Execution
	narration    []Narration
	rounds       []Round
	inputTokens  int
	outputTokens int
	cacheRead    int
	cacheWrite   int
	roundsUsed   int
	maxRounds    int
	model        string
	providerKey  string
	steers       []SteerMark
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
		CacheRead:    p.cacheRead,
		CacheWrite:   p.cacheWrite,
		Executions:   append([]Execution(nil), p.executions...),
		Narration:    append([]Narration(nil), p.narration...),
		Rounds:       append([]Round(nil), p.rounds...),
		RoundsUsed:   p.roundsUsed,
		MaxRounds:    p.maxRounds,
		Model:        p.model,
		ProviderKey:  p.providerKey,
		Messages:     append([]llm.Message(nil), p.messages...),
		Steers:       append([]SteerMark(nil), p.steers...),
	}
}

// record replaces the partial state with the loop's view as it stands. Handed
// a [Result] rather than a positional list, because the list had grown to
// eleven values of four types and a transposition compiles.
func (p *Progress) record(res Result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append([]llm.Message(nil), res.Messages...)
	p.executions = append([]Execution(nil), res.Executions...)
	p.narration = append([]Narration(nil), res.Narration...)
	p.rounds = append([]Round(nil), res.Rounds...)
	p.inputTokens, p.outputTokens = res.InputTokens, res.OutputTokens
	p.cacheRead, p.cacheWrite = res.CacheRead, res.CacheWrite
	p.roundsUsed, p.maxRounds, p.model = res.RoundsUsed, res.MaxRounds, res.Model
	p.providerKey = res.ProviderKey
	p.steers = append([]SteerMark(nil), res.Steers...)
}

// Result is one loop invocation's outcome.
type Result struct {
	// Text is the conversation's assistant turns as one displayable
	// string — reasoning included, a corrective's repeats excluded. See
	// [assistantText] for which turns it carries and why.
	Text         string
	InputTokens  int
	OutputTokens int
	Executions   []Execution
	RoundsUsed   int
	Model        string

	// ProviderKey is the configured entry that served the phase, latched
	// with Model and by the same precedence: the first completion that
	// names one wins, and [Config.ProviderKey] stands in until then. NOT
	// the same question as Model — a chain serves several models under one
	// key and one model can sit under several keys.
	ProviderKey string

	// CacheRead and CacheWrite total the rounds' own ([Round.CacheRead]):
	// how much of InputTokens a provider's prompt cache served, and how
	// much it stored. A breakdown of InputTokens, never an addition to it.
	//
	// THE ONE PRODUCER of these numbers in the engine. Every backend reports
	// them on its completion and nothing kept them past the round's span
	// attribute, so "how much of this seat's spend did the cache answer"
	// had no answer anywhere a person could read one.
	CacheRead  int
	CacheWrite int

	// Rounds is one entry per provider call, in order: when it was made,
	// how long it took, who answered and what it cost. See [Round].
	Rounds []Round

	// MaxRounds echoes the cap this invocation ran under, so a caller that
	// renders "round 3 of 8" reads the cap the loop actually enforced
	// rather than a second copy of the config beside it.
	MaxRounds int

	// Running is the tool call in flight, present only on a live snapshot
	// published while a call runs — see [RunningCall]. Never on a finished
	// Result or a failure snapshot: a call that has returned is an
	// Execution, and one that never returned is the failure.
	Running *RunningCall

	// RoundStartedAt is when the latest round's provider call was made, in
	// UTC, on a live snapshot: the round in flight while the model is still
	// answering, and the round whose tools are running after it has. What a
	// live view counts "this round has taken 40s" from. Every round's
	// opening frame carries it, published the moment the call is made, so
	// a snapshot whose RoundStartedAt is later than every [Round] in
	// [Result.Rounds] is a round in flight and one equal to the last is
	// that round's tools. Zero on a finished Result, whose [Result.Rounds]
	// carry every start.
	RoundStartedAt time.Time

	// Narration is per-round what Text is in aggregate. Both are published:
	// Text is what every existing consumer and every already-stored event
	// reads, and the envelope evolves additive-only.
	Narration []Narration

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

	// Steers is every person's note this invocation delivered, in order,
	// each with the round that first read it. The note itself is in
	// Messages; this is what says which round it changed.
	Steers []SteerMark

	// ExhaustedRounds means the loop hit MaxRounds with the model still
	// asking for tools. Distinct from a clean finish, because the caller
	// may extend the cap rather than accept a truncated phase.
	//
	// NEVER SET ON A LOOP A TERMINATOR ENDED, even on the budget's last
	// round, where the round's last message is still a call: a phase that
	// submitted is finished, and reporting it exhausted had its caller pay
	// the extension judge, and then run granted rounds, for a phase that
	// had nothing left to do.
	ExhaustedRounds bool

	// Withheld is the finishing corrective this invocation EARNED AND DID
	// NOT SEND: its last round ended without the call that finishes it, the
	// run of declines still had allowance, and there was no round left to
	// read a corrective in. Empty otherwise — including in a loop with no
	// terminators, whose prose is a finish.
	//
	// Not appended to Messages, because nothing may be on the record that
	// nothing answered. Handed back instead, because the round the budget
	// ended on is the round a phase most naturally SUBMITS on — the
	// extension nudge tells it to — and only the caller knows whether the
	// phase may run past this invocation's budget: one that can appends
	// this as a user message and continues, and one that cannot ends there
	// and rescues, exactly as before.
	Withheld string

	// CorrectivesSpent is the finishing allowance the current run of
	// declined rounds has used, as the loop ended — the count a
	// continuation passes back as [Config.CorrectivesSpent], plus the one
	// it sends.
	CorrectivesSpent int

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
// Abandoned holds attempts that a provider or credential gave up on partway
// through, oldest first. They are KEPT rather than erased because a reader has
// already seen that text: making it vanish reads as a glitch, and "this model
// wrote four hundred characters and then died" is exactly what an operator
// debugging a flaky provider needs. They live only as long as the round does —
// once it completes, the authoritative narration replaces the whole thing.
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

// Config is one loop run.
type Config struct {
	// Provider and Surface are required.
	Provider llm.Provider
	Surface  Surface

	// ProviderKey is the providers.llm key Provider was resolved under —
	// a chain's HEAD. It stands in for the entry that served exactly as
	// Provider.Model() stands in for the model: until a completion names
	// one ([llm.Completion.ProviderKey]), and on a phase that never got a
	// completion at all. Optional; empty leaves [Result.ProviderKey] to
	// the completions alone.
	ProviderKey string

	// Messages is the starting conversation. The loop appends to a copy;
	// the result carries the full conversation.
	Messages []llm.Message

	// MaxRounds bounds the provider calls. Required and positive.
	MaxRounds int

	// ToolChoice is passed to the provider, on EVERY round including a
	// corrective one, and changes nothing else about the loop. Empty is
	// `auto` wherever the surface offers a tool, which is what every phase
	// of the engine runs on: a forced choice is a request some endpoints
	// ignore and several current models refuse with a 400, so a phase that
	// must end in a call says so with [Config.TerminateAfter] instead.
	//
	// [llm.ToolChoiceRequired] WITHOUT a terminator is refused: a loop that
	// demands a call every round and names no call that ends it can only
	// stop by exhausting MaxRounds.
	ToolChoice llm.ToolChoice

	// TerminateAfter names tools that end the loop once they have run
	// SUCCESSFULLY, even if the model asked for more. A phase whose
	// delivery tool has fired is finished; letting it keep going spends
	// rounds re-deciding something already done — measured at four
	// identical submissions before the round cap stopped it.
	//
	// A failed call does not terminate: its failure went back to the
	// model, and ending the phase there means the retry never happens.
	//
	// DECLARING ONE IS DECLARING HOW THE LOOP FINISHES, so it also changes
	// what a round of prose means. Without terminators, a round with no
	// tool call is the model's answer and a clean finish. With them, no
	// such round can be a finish — a successful terminator ends the loop
	// before the next round opens, so a prose round always comes before
	// any submission succeeded — and the loop re-prompts with a FINISHING
	// corrective naming these tools, under [MaxFinishingCorrectives]. It
	// names the submission rather than the surface, which on the
	// onboarding pass is a whole catalogue of tools that are not how it
	// finishes.
	//
	// ONLY A TERMINATOR THE ROUND OFFERS COUNTS. One the surface does not
	// carry this round cannot be called, so a corrective naming it is a
	// round spent on nothing, and a loop offering none of its terminators
	// reads prose as its answer exactly as a loop that declared none.
	TerminateAfter []string

	// CorrectivesSpent is how many finishing correctives the run of
	// declined rounds this invocation CONTINUES has already been sent — by
	// an earlier invocation of the same phase, the withheld one its caller
	// appended included. It seeds the allowance, so a continuation shares
	// [MaxFinishingCorrectives] with the invocation before it rather than
	// starting a fresh pair. Zero for every invocation that does not
	// continue a run of declines.
	CorrectivesSpent int

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

	// OnProgress receives the live view twice per round and once per call:
	// once the model has spoken, immediately before each tool call with
	// that call as [Result.Running], and again once its tools have
	// returned. Failures are the
	// caller's business — telemetry must never fail a phase — so this
	// returns nothing.
	OnProgress func(Result)

	// Progress is the failure view. Nil means the caller does not want
	// partial state on the error path, which is a choice rather than a
	// default: every phase in the engine passes one.
	Progress *Progress

	// Steer is the running turn's notes from a person, drained at the top
	// of every round IMMEDIATELY AFTER THE FENCE and appended as user
	// messages before the provider call.
	//
	// That point and no other. It is the one place in a round where the
	// conversation is complete — the previous round's calls all have
	// their answers — so a note can never land between a call and its
	// result; and it is after the fence, so a turn that is about to end
	// is not handed an instruction it will never act on.
	//
	// Nil is a loop nobody can steer: every sub-agent worker, the
	// extension judge and the onboarding pass. A worker is a leaf its
	// parent directs, and a note meant for the turn is read by the turn.
	Steer Steerer
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
	if c.CorrectivesSpent < 0 || c.CorrectivesSpent > MaxFinishingCorrectives {
		errs = append(errs, fmt.Errorf("toolloop: CorrectivesSpent must be within 0..%d, got %d",
			MaxFinishingCorrectives, c.CorrectivesSpent))
	}
	// A loop that demands a call on every round and names none that ends it
	// has no finish but the round cap: prose is refused, every call is
	// followed by another round, and nothing it can do stops it. That is a
	// caller that forgot to say how its phase finishes, not a phase.
	if c.ToolChoice == llm.ToolChoiceRequired && len(c.TerminateAfter) == 0 {
		errs = append(errs, errors.New("toolloop: ToolChoice is required but TerminateAfter "+
			"names no tool, so the loop could only end by exhausting MaxRounds — name the "+
			"call that finishes the phase in TerminateAfter, or leave ToolChoice auto"))
	}
	return errors.Join(errs...)
}

// Run drives the loop.
//
// It returns a Result for every outcome that is not an engine failure —
// including a suspend and an exhausted round budget, both of which are things
// the phase did rather than things that went wrong. An error means the
// provider, the budget or the surface itself failed, and the caller publishes
// Progress.Snapshot() alongside it.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	msgs := append([]llm.Message(nil), cfg.Messages...)
	var execs []Execution
	var narration []Narration
	var rounds []Round
	var inTokens, outTokens, cacheRead, cacheWrite int
	var model, providerKey string
	var steers []SteerMark
	// The latest round's start, for the live view; see Result.RoundStartedAt.
	var roundStarted time.Time
	// served distinguishes the model a COMPLETION named from the configured
	// placeholder a streamed round shows before one exists.
	var served, keyServed bool
	terminators := make(map[string]struct{}, len(cfg.TerminateAfter))
	for _, name := range cfg.TerminateAfter {
		terminators[name] = struct{}{}
	}
	finishingRetries := cfg.CorrectivesSpent
	emptyRetries := 0
	emptyAnswers := 0
	// What ended the loop, where it was not the budget: a terminator that
	// ran, or a corrective withheld for want of a round. See
	// [Result.ExhaustedRounds] and [Result.Withheld].
	terminated := false
	withheld := ""

	var partial *Partial
	// state is the loop's record as it stands, the one shape every exit and
	// every publish is built from — so a field added to the Result cannot
	// reach the finished record and miss the live one, or the reverse.
	state := func(used int) Result {
		return Result{
			Text:         assistantText(msgs),
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			CacheRead:    cacheRead,
			CacheWrite:   cacheWrite,
			Executions:   append([]Execution(nil), execs...),
			Narration:    append([]Narration(nil), narration...),
			Rounds:       append([]Round(nil), rounds...),
			RoundsUsed:   used,
			MaxRounds:    cfg.MaxRounds,
			Model:        model,
			ProviderKey:  providerKey,
			Messages:     append([]llm.Message(nil), msgs...),
			Steers:       append([]SteerMark(nil), steers...),
		}
	}
	// publish hands the live view the state, with the call in flight when
	// one is — see [RunningCall]. Nil clears it, which is what every frame
	// but the one published immediately before a call does.
	publish := func(used int, running *RunningCall) {
		if cfg.Progress == nil && cfg.OnProgress == nil {
			return
		}
		live := state(used)
		if cfg.Progress != nil {
			// WHAT SERVED, never the placeholder. The live view is shown
			// the configured identity while the first round is out (see
			// where it is set below), but this is the record a FAILURE
			// publishes, and a phase whose every provider refused before
			// one round came back was served by nobody: naming the head
			// there would charge it for a call it never answered. Every
			// round's opening frame reaches here before its call, so the
			// placeholder would otherwise be on every such record.
			recorded := live
			if len(rounds) == 0 {
				recorded.Model, recorded.ProviderKey = "", ""
			}
			cfg.Progress.record(recorded)
		}
		if cfg.OnProgress != nil {
			live.Partial = partial.clone()
			live.Running = running
			live.RoundStartedAt = roundStarted
			cfg.OnProgress(live)
		}
	}

	roundsUsed := 0
	for round := range cfg.MaxRounds {
		roundsUsed = round + 1

		// The fence first, at the cheapest possible point: nothing has
		// been spent this round and nothing has fired.
		if cfg.Fence != nil {
			if err := cfg.Fence(); err != nil {
				return nil, err
			}
		}

		// A PERSON'S NOTES, straight after the fence and before anything
		// is spent: the round about to run is the first to read them. See
		// [Config.Steer] for why here and nowhere else.
		if cfg.Steer != nil {
			notes := cfg.Steer.Drain(roundsUsed)
			for _, note := range notes {
				msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: note.Message})
				steers = append(steers, SteerMark{Round: roundsUsed, ID: note.ID})
			}
			// Published with the round's opening frame below, so the live
			// view shows the note taken while the model is still reading
			// it, and a round whose provider call then fails still has the
			// note on the record the caller publishes for it.
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
		roundCtx, roundSpan := tracing.Start(ctx, "agent.toolloop", "llm.round",
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
		if providerKey == "" {
			providerKey = cfg.ProviderKey
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
					// erased — see [Partial] — and the replacement starts
					// from empty so two half-answers never concatenate.
					if partial.Content != "" || partial.Reasoning != "" {
						partial.Abandoned = append(partial.Abandoned, Narration{
							Round:     partial.Round,
							Reasoning: partial.Reasoning,
							Content:   partial.Content,
						})
					}
					partial.Reasoning, partial.Content = "", ""
					publish(roundsUsed, nil)
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
				publish(roundsUsed, nil)
			}
		}

		// THE ROUND'S CLOCK, around the provider call and nothing else —
		// the same bracket as its span. Stamped before the call so a
		// streamed round's frames can say how long it has been writing.
		began := time.Now()
		roundStarted = began.UTC()
		// THE ROUND HAS OPENED, and the live view is told so now rather
		// than when the model answers. Until this frame the last one
		// published was the previous round's — its tools returned, its
		// start still in RoundStartedAt — so for as long as this call ran
		// (minutes, on a slow model) every reader saw round N-1 as the
		// round in flight and had no instant for round N at all: a trace
		// drew the running call from the previous round's start and a
		// stepper named a round that had already finished. A streamed
		// round's first fragment would say it too, but only once text
		// arrives, and a unary one says nothing until it is over.
		publish(roundsUsed, nil)
		completion, err := cfg.Provider.Complete(roundCtx, llm.Request{
			Messages:   msgs,
			Tools:      tools,
			ToolChoice: choice,
			OnDelta:    onDelta,
		})
		took := time.Since(began)
		if err != nil {
			tracing.Fail(roundSpan, err)
			roundSpan.End()
			return nil, fmt.Errorf("toolloop: %s round %d: %w", cfg.Surface.Phase(), roundsUsed, err)
		}
		roundSpan.SetAttributes(
			attribute.String("crewlet.model", completion.Model),
			attribute.Int("crewlet.input_tokens", completion.InputTokens),
			attribute.Int("crewlet.output_tokens", completion.OutputTokens),
			attribute.Int("crewlet.cache_read_tokens", completion.CacheRead),
			attribute.Int("crewlet.cache_write_tokens", completion.CacheWrite),
			attribute.Int("crewlet.tool_calls", len(completion.ToolCalls)))
		roundSpan.End()
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
		if !keyServed && completion.ProviderKey != "" {
			// The ENTRY that served, by the model's own precedence and
			// latched on its own flag: a completion can name a model
			// with no key (a bare backend) and the configured head then
			// stays the answer, exactly as the model's placeholder did
			// before it.
			providerKey, keyServed = completion.ProviderKey, true
		}
		if model == "" {
			model = cfg.Provider.Model()
		}
		inTokens += completion.InputTokens
		outTokens += completion.OutputTokens
		cacheRead += completion.CacheRead
		cacheWrite += completion.CacheWrite
		// Recorded BEFORE the charge below, because the round happened
		// and was billed by the provider whether or not the company's
		// meter then admits it — and a refused charge ends the loop, so a
		// round recorded after it would be the one round missing from the
		// failure record that explains why it failed.
		//
		// The completion's model where it names one, the configured
		// identity where the backend filled nothing in — the same
		// precedence the phase's own model follows, per round.
		roundModel := completion.Model
		if roundModel == "" {
			roundModel = cfg.Provider.Model()
		}
		rounds = append(rounds, Round{
			Round: roundsUsed, StartedAt: roundStarted, Duration: took,
			Model:        roundModel,
			InputTokens:  completion.InputTokens,
			OutputTokens: completion.OutputTokens,
			CacheRead:    completion.CacheRead,
			CacheWrite:   completion.CacheWrite,
			ToolCalls:    len(completion.ToolCalls),
		})

		// Charge BEFORE running the tools this round asked for. A round
		// whose spend is refused must not also have fired its side
		// effects — the refusal is the whole point, and tools are where
		// the irreversible things happen.
		if err = charge(ctx, cfg.Budget, completion.TotalTokens()); err != nil {
			// The round is on the failure record: publish it to the
			// Progress the caller reads on this path, since it is the
			// round that was refused.
			publish(roundsUsed, nil)
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
		partial = nil
		// What this round was, decided once and read twice below: on the
		// record and by the correctives. See [Narration.Declined].
		answeredNothing := strings.TrimSpace(completion.Content) == ""
		// Only a terminator this round OFFERS can finish the phase — see
		// [Config.TerminateAfter] — so it is what makes prose a decline.
		owed := offered(cfg.TerminateAfter, tools)
		declined := len(completion.ToolCalls) == 0 && !answeredNothing && len(owed) > 0
		if narrated(completion.ReasoningContent, completion.Content) {
			narration = append(narration, Narration{
				Round:     roundsUsed,
				Reasoning: strings.TrimSpace(completion.ReasoningContent),
				Content:   strings.TrimSpace(completion.Content),
				Declined:  declined,
			})
		}

		// The model has spoken: publish before the tools run, so its
		// reasoning and prose reach the live view now rather than after
		// the slowest tool returns.
		publish(roundsUsed, nil)

		if len(completion.ToolCalls) == 0 {
			// Counted whichever corrective follows, and counted for
			// rounds that get none: the number a phase record carries
			// is "rounds that reached nobody", which is the question
			// agent/turn already asks about a turn.
			if answeredNothing {
				emptyAnswers++
			}

			// A CORRECTIVE NOTHING WILL READ IS NOT SENT. On the last
			// round of this invocation's budget no round follows, so a
			// corrective appended here would only sit at the end of the
			// conversation the caller records — a user message the model
			// never answered, on the phase's record and in any transcript
			// built from it. A finishing corrective the round earned is
			// HANDED BACK instead ([Result.Withheld]): whether the phase
			// may run past this budget is its caller's to decide, and a
			// caller that may continues the phase with it — while one that
			// may not ends it here, without its submission, which is what
			// its rescue path is for, and the round says so on the record
			// ([Narration.Declined] on the phase's last round).
			lastRound := roundsUsed == cfg.MaxRounds

			// Which corrective this round earns, and the allowance it
			// draws on. Chosen first and spent after, so a round that
			// gets none — the bound reached, or no round left to read
			// it — spends nothing.
			var corrective string
			var allowance *int
			var bound int
			switch {
			case len(owed) > 0:
				// THE LOOP SAID HOW IT FINISHES, so a round with no call
				// is not a finish however it reads — see
				// [Config.TerminateAfter]. This covers the model that
				// wrote its submission out as text (a fenced JSON block
				// of the tool's arguments, measured on a cli-agent text
				// backend) and the one that thought and stopped, so it
				// wins over the empty-answer corrective below, whose
				// "write it in the response itself" would steer a
				// submission phase straight into its rescue. Both
				// failures draw on one allowance.
				//
				// THE TOOL CHOICE IS LEFT AS THE CALLER SET IT — `auto`,
				// on every phase of the engine — and deliberately never
				// escalated to `required` for the corrective round. Several current
				// models reject a forced choice outright: Claude Opus
				// 5.5, Sonnet 5.5, Fable 5.1 and Mythos 5.1 answer 400
				// "tool_choice: type "tool" and "any" are not supported
				// for this model", and Anthropic's documented
				// replacement is `auto` plus an instruction naming the
				// tool, which is exactly this message. The loop is what
				// enforces the call, by asking again and by never
				// counting prose as the finish.
				corrective = finishingCorrective(owed)
				allowance, bound = &finishingRetries, MaxFinishingCorrectives
			case answeredNothing:
				// A ROUND THAT REACHED NOBODY IS NOT A FINISH. No tool
				// call and no prose is a model that spent its output on
				// hidden reasoning — the shape every backend now hands
				// back for it (an empty Content), rather than the
				// transport error the cli-agent backend used to raise.
				// Correcting it is the loop's job and not the provider's:
				// this is the frame that holds the conversation, so it is
				// the only one that can ask again without inventing a
				// second prompt contract the operator cannot see.
				//
				// Keyed on Content alone. Reasoning is not an answer that
				// reached anybody, so a thinking-only round from any
				// backend is the same failure and gets the same
				// corrective.
				//
				// Prose on a loop that declares no terminator matches no
				// case: it is the model's answer, and a clean finish.
				corrective = emptyAnswerCorrective
				allowance, bound = &emptyRetries, maxEmptyAnswerRetries
			}
			if corrective == "" || *allowance >= bound {
				break
			}
			if lastRound {
				if allowance == &finishingRetries {
					withheld = corrective
				}
				break
			}
			*allowance++
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: corrective})
			continue
		}

		// A ROUND THAT EMITTED A CALL CLEARS EVERY STALL ALLOWANCE. Each
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
		finishingRetries, emptyRetries = 0, 0

		suspended, pendingID, pendingName, payload, err := runCalls(
			ctx, cfg, completion.ToolCalls, roundsUsed, &msgs, &execs,
			func(running RunningCall) { publish(roundsUsed, &running) })
		if err != nil {
			return nil, err
		}

		// The round's tools have returned: publish again so the live row
		// fills in rather than waiting for the next model turn. The call
		// in flight is cleared here, by a frame naming none.
		publish(roundsUsed, nil)

		if suspended {
			out := state(roundsUsed)
			out.EmptyAnswers = emptyAnswers
			out.Suspended = true
			out.PendingToolCallID, out.PendingToolName = pendingID, pendingName
			out.SuspendPayload = payload
			return &out, nil
		}

		if ranTerminator(execs, terminators, roundsUsed) {
			terminated = true
			break
		}
	}

	out := state(roundsUsed)
	out.ExhaustedRounds = roundsUsed == cfg.MaxRounds && lastAskedForTools(msgs) && !terminated
	out.EmptyAnswers = emptyAnswers
	out.Withheld = withheld
	out.CorrectivesSpent = finishingRetries
	return &out, nil
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
	announce func(RunningCall),
) (suspended bool, pendingID, pendingName string, payload map[string]any, err error) {
	// Every call this round makes is told which round it is in, on the
	// phase's scale — see [CallRound].
	callCtx := withCallRound(ctx, round)
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
		// Timed on the monotonic clock and stamped in UTC, and ANNOUNCED
		// first — after the fence, so a call the fence stops is never
		// shown as running — because an Execution exists only once the
		// call has answered, and a serial round's calls take minutes.
		began := time.Now()
		startedAt := began.UTC()
		announce(RunningCall{Round: round, Name: call.Name, Args: call.Arguments, StartedAt: startedAt})
		res, execErr := cfg.Surface.Execute(callCtx, call)
		took := time.Since(began)
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
			StartedAt: startedAt, Duration: took,
			Origin: res.Origin, Server: res.Server,
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
	return &BudgetError{
		Scope: scope, Used: outcome.Used, Limit: outcome.Limit,
		Period: outcome.Period, Window: outcome.Window, ResetsAt: outcome.ResetsAt,
	}
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

// offered narrows a phase's terminators to the ones on this round's surface,
// in the order the caller declared them.
func offered(terminators []string, tools []llm.ToolDef) []string {
	var out []string
	for _, name := range terminators {
		for _, t := range tools {
			if t.Name == name {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// finishingCorrective is the re-prompt for a round that ended without a call in
// a loop that finishes by one: it names the tools that finish the phase.
//
// Every clause answers a measured misreading. "Has not finished" because the
// model believed it had. "Nothing written in a reply is read or delivered"
// because the reply it just wrote was its report, typed out — and "JSON
// included" because the case that found this was a submission's ARGUMENTS
// written as a fenced JSON block, which a model reasonably takes for a
// structured answer. And the second branch, because a model that stopped
// halfway must be sent back to the WORK rather than pushed into submitting a
// report of work it has not done.
func finishingCorrective(terminators []string) string {
	names := make([]string, 0, len(terminators))
	for _, name := range terminators {
		names = append(names, "`"+name+"`")
	}
	call := names[0]
	if len(names) > 1 {
		call = "one of " + strings.Join(names, ", ")
	}
	return "Your last reply ended without calling " + call + ", so this phase has not " +
		"finished. Nothing written in a reply is read or delivered — a report written " +
		"out as text, JSON included, is not a submission. If the work is done, call " +
		call + " now with that report as its arguments; if something is still left to " +
		"do, call the tool that does it."
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
//
// A CORRECTIVE'S ANSWER IS NOT THE PHASE'S. In a run of rounds that called no
// tool, the first one that said something is the model's answer to its task;
// every round of that run after it answers a corrective instead — "you have
// not finished, call the tool" — and a model that still does not call writes
// its report out again, near-verbatim. Joined whole, a phase that kept
// declining through both finishing correctives handed three copies of one
// report to whoever reads the text: a rescued executor's to the reviewer as
// "what the agent produced", a worker's `no_result` prose to its parent. So
// those later rounds are left out, reasoning and all, and the text is what the
// model answered the task with — the same one closing reply an agent-mode
// run's text is, so the two runtimes hand a reviewer the same thing. A round
// that called a tool ends the run, so a model that worked and then stopped
// again is a new answer, and kept.
//
// THE FIRST, NOT THE LAST. Every later one is a reply to the corrective rather
// than to the task — whatever it says, it was written to a different question —
// while the first is the report as the model meant it, and the one the phase
// would have ended on had nobody asked again. Nothing is lost by the cut: every round stays in [Result.Narration] and in
// the conversation itself, each marked [Narration.Declined].
//
// Read off the message list rather than a flag kept beside it, so a phase
// that runs the loop more than once — a continuation, an extension, a resume
// — gets the same answer from every invocation over the conversation as it
// stands.
func assistantText(msgs []llm.Message) string {
	var parts []string
	answered := false
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		switch {
		case len(m.ToolCalls) > 0:
			answered = false
		case answered:
			continue
		case strings.TrimSpace(m.Content) != "":
			answered = true
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
