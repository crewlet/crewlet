// Package toolloop drives one phase's model-and-tools conversation.
//
// It is the innermost loop of the engine: the model asks for tools, they run,
// their results go back, until it stops asking or the round budget runs out.
// Every phase the ENGINE drives is this loop with a different surface and a
// different prompt: the reviewer, a delegated worker, the onboarding pass and
// its extension judge, and the Dashboard Assistant's turn. A seat's executor
// is not one of them — it is its coding CLI's own agentic run, in a box, which
// reaches the seat's tools over the tool bridge rather than through a loop
// here.
//
// THE CLI DRIVES THE ROUNDS; THE LOOP HOLDS THE PHASE. Every model call is a
// vendor's coding CLI, which runs its own loop — ask the model, call a tool,
// ask again — and calls the phase's tools over MCP. [Run] starts CLI runs of
// the phase's provider, each served the phase's tools by a [dispatcher]
// (dispatcher.go) that decides every call as it arrives, ends the run when
// the phase needs it ended, and is told what the CLI's own record shows of
// each model request; and between runs it does what only the loop can: a
// person's note, a corrective, a call's result the run ended before. The fold
// (fold.go) turns each run back into the record the loop kept when it drove
// each round itself, so nothing downstream can tell the two apart. A round is
// still one model request. That a phase's tools reach its CLI as one MCP
// server, and every call passes these gates, is ADR-0049.
//
// Seven things here are load-bearing and each replaced an incident:
//
//   - THE BUDGET CHECK AND THE INCREMENT ARE ONE OPERATION, and the refusal
//     names its own scope. Re-reading the caps afterwards to work out which
//     one said no is a read a peer's spend can invalidate between the refusal
//     and the report.
//   - PROGRESS IS PUBLISHED AS A RUN OPENS, AS EACH RESPONSE IS READ, AND
//     BEFORE AND AFTER EACH CALL — the model's prose before its tools run,
//     each call named as it starts — and both the live update and the
//     durable record are built by ONE function over one message list. They
//     used to be assembled separately, so the live view streamed a round's
//     tool calls against an empty response and its prose appeared only when
//     the phase ended. The per-call frame is what makes a round's calls
//     visible WHILE they run: a delegated worker or a slow MCP server takes
//     minutes, and a live view that learned of a call only once it returned
//     showed a seat doing nothing for exactly the stretch somebody was
//     watching it.
//   - THE LOOP IS THE CLOCK. Every model request and every tool call is
//     timed here — a request from the moment its CLI had everything it needed
//     to ask to the moment its record was read, a call around the surface —
//     and carried on the record as a start and a duration. A reader that
//     reconstructs timing from event arrival subtracts two publishers' clocks
//     and a queue's latency, and a request's cache tokens exist nowhere else:
//     the CLI's record reports them and this is the one frame that sees every
//     request.
//   - THE SEAT FENCE RUNS BEFORE EVERY RUN, before any tokens are spent and
//     before anything fires, AND BEFORE AND AFTER EACH TOOL CALL, because a
//     round is one model request but many calls and the calls are what reach
//     outside the engine. A node whose lease moved stops there rather than
//     running the rest of the turn beside the seat's new owner.
//   - A PERSON'S NOTE ENTERS AT A RUN'S OPENING AND NOWHERE ELSE —
//     immediately after the fence, as a user message. One that arrives while
//     a run is going ends the run once the calls of the response it arrived
//     during have their answers, and the next run reads it. Anywhere else it
//     could sit between a call and its result, which a provider rejects and
//     a model reads as the tool's output — and inside a tool's result any
//     tool's output could imitate one; before the fence it would be read by a
//     turn that is about to end. See internal/agent/steer.
//   - A PHASE THAT FINISHES BY A CALL IS ASKED AGAIN WHEN A RUN ENDS WITHOUT
//     ONE. A loop that declares terminators ([Config.TerminateAfter]) has said
//     how it ends — a successful call to one of them — so a run that ends in
//     prose there is not a finish: it is a model that wrote its report where
//     nobody reads it (a submission's arguments typed out as a JSON block is
//     the measured case). The request never forces a call — some endpoints
//     ignore a forced tool choice, some models think-then-stop, and several
//     current ones reject the force outright — so the call is ENFORCED HERE
//     rather than requested of the provider: a bounded corrective re-prompt
//     naming what finishes the phase is the difference between "the model
//     declined" and "the phase produced nothing and said it was fine".
//   - A CALL WHOSE ARGUMENTS DID NOT PARSE IS NEVER RUN WITH NONE. It is
//     answered with why, as a failed result the model can read and fix:
//     running it on an empty object is a write the model never asked for.
package toolloop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolsession"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/words"
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
// call cannot end the phase except into its caller's rescue — a whole extra
// executor run for the reviewer, `no_result` for a worker, an unmarked seat
// that re-runs its onboarding on every turn — so a second send is weighed
// against that, not against nothing. And it caps there because a model that
// cannot emit the call in three attempts will not emit it in ten, while each
// attempt is a full priced round charged like any other. Against the budgets
// that arm it: onboarding's default 10 rounds lose at most 2 to a run of
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
// every round after it. That is measured, not hypothetical: a seat on a
// 24-round budget stalled at round 2, filed a work item at round 3, had a
// submission bounced at round 4, and broke on the stall at round 5 with
// nineteen rounds unspent — one round before the message it had just said it
// was about to send.
const maxEmptyAnswerRetries = 1

// Surface is the set of tools a phase runs against.
//
// An interface rather than a concrete registry because the phases differ in
// what they expose and the loop must not know which phase it is running: the
// reviewer sees only the tool it submits its decision with, a worker sees a
// subset of its parent's, onboarding sees the seat's own. Its offer is re-read
// whenever its [Surface.Revision] moves, so a surface mutated mid-run — a
// meta-tool activating another tool — is listed to the CLI before its next
// request, or the run is ended for a CLI that never re-lists.
type Surface interface {
	// ToolDefs returns the tools to offer the model now.
	ToolDefs() []llm.ToolDef

	// Revision changes whenever ToolDefs would answer differently — an
	// activation — so a session re-lists only when something moved, and a
	// run whose CLI never re-lists is ended after one.
	Revision() uint64

	// AnnotationsOf is one tool's behavioural hints, which the session
	// advertises to the CLI beside it: whether a call is read-only decides,
	// in several CLIs, whether it may run beside another and whether to ask
	// first.
	AnnotationsOf(name string) mcp.Annotations

	// Admit fixes one call's place in its run's call log NOW — the order the
	// repeat count an operation id carries is read in — and returns the call
	// bound to that place, to be run once.
	//
	// AT ADMISSION, NOT AT RETURN, because a CLI makes a response's calls at
	// once and they return in whatever order their tools finish: a count read
	// against the order calls return in is one a re-run, making the same
	// calls, does not reproduce. The loop admits each call under its own
	// lock, in the order the calls arrive.
	//
	// CALLED CONCURRENTLY with the calls it returns, which run side by side:
	// every call but a terminator or a [Detacher]'s detached one, which runs
	// alone.
	Admit(name string, args map[string]any) Admitted

	// Phase names the phase for telemetry.
	Phase() string
}

// Detacher is a [Surface] with tools whose work outlives their call and may
// park the phase ([ToolResult.Suspend]). Such a call runs alone — two run side
// by side would each start work a parked phase can answer for only one.
//
// Optional, like [Refuser]: only a loop that may park ([Config.AllowSuspend])
// has such a tool — today the Dashboard Assistant's turn, which parks on a
// change waiting for its person's approval — so every other surface, which
// would answer false for every name, implements nothing.
type Detacher interface {
	// Detached reports whether a call of name may park the phase.
	Detached(name string) bool
}

// Refuser is a [Surface] that keeps a record of the calls this loop answers
// without running them: a call whose arguments did not parse, one the run was
// no longer taking, one that arrived after the call that parked its phase.
// Admit never sees such a call, so a surface that records its steps as it runs
// them would otherwise show a model that asked for something and nothing
// happened, with no step saying so.
//
// Optional: a surface with no record of its own does not implement it, and the
// loop answers the call exactly as it does for one that does.
type Refuser interface {
	// Refused records that call did not run, and why, in the words the
	// model was answered with.
	Refused(ctx context.Context, call llm.ToolCall, reason string)
}

// Admitted is one call a [Surface] admitted, bound to its place in the run's
// call log: run it once, with the call as the CLI made it.
//
// It returns a ToolResult rather than an error because a failing tool is
// ORDINARY: its message goes back to the model, which is expected to react to
// it. A Go error means the surface itself broke.
type Admitted func(ctx context.Context, call llm.ToolCall) (ToolResult, error)

// ToolResult is what one tool call produced.
type ToolResult struct {
	// Output is fed back to the model as the tool message's content.
	Output string

	// Failed marks a tool that reported failure. The output still goes
	// back to the model — that is the point — but the execution record
	// carries the flag so a reader can tell a tool that ran from one that
	// refused.
	Failed bool

	// Cause is WHY a failed call failed — the tool's own [mcp.Result.Cause],
	// or the class of a call the surface refused before any tool ran — and
	// nil on success and on a third-party MCP server's failure, which the
	// engine does not classify by guessing at prose it did not write. This
	// loop never reads it — a model reads the sentence — so it is for a
	// caller that dispatches through a surface with no model behind it.
	//
	// THE CAUSE AND NOT A CLASS BESIDE IT: [ToolResult.Refusal] and
	// [ToolResult.Unknown] read the class and the unknown outcome out of
	// it, so a call carries one classifier of its failure rather than two
	// that can disagree.
	Cause error

	// Suspend stops the loop with this call UNANSWERED, for a tool whose
	// work outlives the turn — a change the Dashboard Assistant proposed,
	// waiting on its person's approval. Honoured only when the caller set
	// AllowSuspend; elsewhere the call is refused, because a phase that
	// never persists a partial conversation cannot resume one.
	Suspend bool

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

// Refusal is the call's class, read out of [ToolResult.Cause] by
// [mcp.RefusalOf]: "" for a success and for a failure nothing in this engine
// classified.
func (r ToolResult) Refusal() mcp.Refusal {
	return mcp.RefusalOf(mcp.Result{Output: r.Output, Failed: r.Failed, Cause: r.Cause})
}

// Unknown reports whether the call's write may have landed, by
// [mcp.UnknownOf].
func (r ToolResult) Unknown() bool {
	return mcp.UnknownOf(mcp.Result{Output: r.Output, Failed: r.Failed, Cause: r.Cause})
}

// Execution records one tool call for the prior-work ledger and the phase
// record. It is the engine's own account of what ran, which is what makes a
// self-iterate round able to tell a delivery that already fired from one the
// model only described.
type Execution struct {
	Round int
	Name  string
	Args  map[string]any

	// CallID is the CLI's own id for the call, where its record or the
	// call itself named one, and the loop's otherwise — what the
	// conversation pairs the call's result with.
	CallID string

	// Output is the tool's own output, as it returned it.
	Output string
	Failed bool

	// StartedAt is when the call was handed to the surface, in UTC, and
	// Duration how long the surface took to answer — measured on the
	// monotonic clock, so a wall-clock correction mid-call cannot report a
	// negative or inflated one.
	//
	// Measured HERE, around the [Admitted] call, because the question a reader
	// asks of a slow round is which call held it: the round's own span
	// covers the model's request only, and the phase's covers everything.
	// Both are zero on an execution nothing timed — a call answered rather
	// than run: arguments that did not parse, a tool not offered, a call
	// its run ended before — and zero means "not measured", never
	// "instant".
	StartedAt time.Time
	Duration  time.Duration

	// Origin and Server are [ToolResult.Origin] and [ToolResult.Server]:
	// who answered the call, empty when nobody did.
	Origin string
	Server string
}

// Round is one model request of the loop: when it was made, how long the model
// took to answer, who answered and what it cost.
//
// THE MODEL'S HALF OF A ROUND ONLY. The round's tool calls are timed on their
// own [Execution] rows, because each is its own question ("which call held
// this round?"); folding them into the round's duration would make a slow
// model and a slow tool the same number. A round's wall clock is therefore its
// StartedAt to the end of its last call, and a reader has both halves to hand.
//
// Recorded for every request a CLI's record showed, including the runs a
// corrective re-prompt follows: each is a priced request and a reader adding
// up where the time went needs all of them. A CLI whose record shows no
// requests has its whole run recorded as one round, untimed.
type Round struct {
	// Round is one-based, on the same scale as [Execution.Round] and
	// [Narration.Round], so the three lists join on it.
	Round int

	// StartedAt is when the request was made — when its CLI had everything
	// it needed to ask — in UTC, and Duration how long it took to answer,
	// on the monotonic clock.
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

	// StopReason is why the model stopped writing this round's response
	// ([llm.StopReason]): an answer, or calls to run. Empty where its
	// backend reported none.
	StopReason llm.StopReason
}

// Invocation is one CLI run of the loop: one process, driving its own loop
// over the phase's tools, from launch to exit.
//
// THE UNIT THE ENGINE OBSERVES FOR EVERY CLI. A [Round] is one model request,
// which a CLI's own record shows for most CLIs and not for all; a run is what
// the engine starts and ends itself, so it is on every record whatever the
// CLI reports.
type Invocation struct {
	StartedAt time.Time
	Duration  time.Duration

	// ProviderKey and Model are the entry and the model that served the
	// run, where its answer named them.
	ProviderKey string
	Model       string

	// Outcome is how the run ended — see [InvocationOutcome].
	Outcome InvocationOutcome

	// FirstRound and LastRound are the rounds the run made, both inclusive,
	// on [Round.Round]'s scale; both zero for a run that made no request.
	// Named rather than left to a reader to place by time, because an
	// unobserved run's round has no instant of its own — and a phase's runs
	// are sequential, so a run's rounds are one contiguous stretch.
	FirstRound, LastRound int

	// RequestsObserved says the CLI's record showed the run's model
	// requests, each a [Round]. False: the whole run is recorded as one.
	RequestsObserved bool

	// The run's spend — every request of it, as the backend reported it.
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int

	// Calls is how many of the phase's tools the run called.
	Calls int
}

// InvocationOutcome is how a CLI run ended.
type InvocationOutcome string

const (
	// OutcomeEnd is a run whose model stopped: its last response called
	// nothing.
	OutcomeEnd InvocationOutcome = "end"
	// OutcomeCap is a run its CLI's own turn cap ended.
	OutcomeCap InvocationOutcome = "cap"
	// OutcomeStopped is a run the engine ended: the phase finished,
	// parked, paused, ran out of rounds or was stopped.
	OutcomeStopped InvocationOutcome = "stopped"
	// OutcomeFailed is a run that failed — its CLI's error, or no CLI.
	OutcomeFailed InvocationOutcome = "failed"
)

// InvocationOutcomes is the closed set, in a fresh slice.
func InvocationOutcomes() []InvocationOutcome {
	return []InvocationOutcome{OutcomeEnd, OutcomeCap, OutcomeStopped, OutcomeFailed}
}

// Valid reports whether o is one of [InvocationOutcomes].
func (o InvocationOutcome) Valid() bool { return slices.Contains(InvocationOutcomes(), o) }

// RunningCall is the tool call in flight: named on the live view BEFORE the
// surface runs it and cleared once it returns.
//
// It is the one thing a round's records cannot say, because an [Execution] is
// appended only once its call has answered. Without it, a seat whose round
// launched a coding run or queried a slow MCP server showed its model's last
// words and nothing after them for as long as the call took — indistinguishable
// from a seat that had stalled.
type RunningCall struct {
	// ID is the call's id, on the [Execution.CallID] rule — what tells two
	// calls of one response running side by side apart.
	ID string

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

	// Waiting reports a note waiting, without taking it: a run that has
	// one waiting is ended once its calls in flight are answered, so the
	// next run reads it.
	Waiting() bool
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
// continues it, and a corrective the last invocation withheld is read past its
// budget (internal/agent/runner) — and the loop numbers its own rounds from 1
// every time. The records it returns are renumbered by the caller afterwards
// ([Result] is a value it can shift), but a tool that asks its round mid-call
// cannot wait for that: a call in the first round of an extension continuing
// a twenty-round phase would be told round 1 while the phase's record states
// it in round 21.
func WithRoundOffset(ctx context.Context, prior int) context.Context {
	if prior <= 0 {
		return ctx
	}
	return context.WithValue(ctx, roundOffsetKey{}, prior)
}

// CallRound is the round the calling tool runs in, on the phase's scale — the
// one-based scale its records state the call on, which a caller that runs the
// loop again declares with [WithRoundOffset] — and false outside a tool call
// this loop made.
//
// Read by a tool whose own answer depends on which round asked for it, and
// never by the loop itself, which knows its round without asking: the
// assistant's brief turn refuses a read in a round kept for its answer, says
// so on a read in the last round that may read, and makes a refused answer in
// the turn's last round final (internal/assistant). A worker spawned by a
// delegate call is told its parent's round by its caller instead
// ([subagent.Config]): the delegate tool is served only on an executor run's
// surface, whose calls arrive over the tool bridge, where no loop is iterating
// to stamp one.
func CallRound(ctx context.Context) (int, bool) {
	round, ok := ctx.Value(roundKey{}).(int)
	return round, ok
}

// withCallRound stamps the round a call runs in, offset onto the phase's scale.
func withCallRound(ctx context.Context, round int) context.Context {
	prior, _ := ctx.Value(roundOffsetKey{}).(int)
	return context.WithValue(ctx, roundKey{}, prior+round)
}

// Narration is one round's model turn — what it said — KEPT ATTACHED TO ITS
// ROUND.
//
// [Result.Text] joins every round's turn into one string, which is the right
// shape for a transcript and the wrong one for a reader: the join is lossy
// about WHICH round said what, so a consumer handed only the blob cannot put a
// round's prose next to the tools it asked for. It also can only be un-joined
// by guessing — the parts are separated by a blank line, and prose contains
// blank lines.
//
// So the split is recorded where it is still known, at the point the round's
// assistant message is appended, rather than reconstructed downstream. The
// round number is recorded rather than inferred from position for the same
// reason [Execution] carries one: a resumed phase starts with a conversation
// that already contains assistant turns, so the k-th message is not round k.
type Narration struct {
	Round int
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

	// Used and Limit are the refusing window's spend and ceiling. Used is
	// the spend as the refused round LEFT it — the round is recorded (see
	// [BudgetMeter]) — so a refusal reads past its Limit by that round. A
	// standing refusal ([BudgetMeter.Refused]) refused no round of its
	// own, so its Used is the window's spend as the meter last read it:
	// at the Limit or past it, since that is what made it certain.
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
	// Spend records tokens a model call has ALREADY SPENT, and answers
	// whether they fitted, in ONE operation against the shared counter.
	//
	// Already spent, because the loop charges a round once its reply is
	// in — a round's size is known no sooner — as the CLI's record shows
	// it, which is before any tool the round asked for runs: a call waits
	// for the record of the request that made it. So a refusal decides
	// what FOLLOWS the round: its tools do not run, and no later model call
	// of the turn is made ([BudgetMeter.Refused]). A CLI whose record shows
	// no requests is charged for its whole run once it is over, its tools
	// already run, and the refusal stops the run after it. It does not undo the round, which the vendor
	// has billed, and a meter must record a round it refuses exactly as it
	// records one it admits: one that dropped it under-stated the company
	// by the round that crossed the cap, and admitted the next round
	// smaller than the room left on top of tokens the refused one had
	// already used.
	//
	// An error means the counter could not be reached, which is not the
	// same as a refusal and must not be treated as one.
	Spend(ctx context.Context, tokens int) (SpendOutcome, error)

	// Refused reports a refusal every further charge is CERTAIN to meet,
	// from what this meter has already been told — it asks the counter
	// nothing to decide it. A caller asks it before every model call it is
	// about to make, and on true makes none: the call would be billed by
	// the vendor, then refused, and everything it bought thrown away. That
	// is the half of a refusal Spend cannot carry, because a refusal is
	// answered to one caller and a turn makes its calls from several — the
	// next run of this loop, the round-cap judge, the next phase, a sibling
	// worker, the executor's launch — and before this
	// each of them paid for one call certain to be refused.
	//
	// A TRUE ANSWER IS A REFUSAL, NOT A FORECAST. Every caller asks it
	// immediately before a call and makes no call on true, so the answer is
	// the gate turning that call away, exactly as a refused Spend turns a
	// round's tools away — and a meter whose counter records refusals
	// records this one there too, which is what the context is for. The
	// call it stops is the one whose charge would have recorded the
	// refusal; a meter that recorded nothing left a window refusing every
	// call of the turn while its counter said it had refused none. The
	// record is the meter's, made once per window rather than once per
	// question, and it never changes the answer: a refusal whose record
	// failed is still a refusal. So it is asked only where a call is about
	// to be made, never to look.
	//
	// CERTAIN, NEVER LIKELY. A meter answers true only for a window it has
	// seen with no room left for a single token, judged by ceilings that do
	// not move for the meter's life, on a counter nothing takes spend back
	// from — so every later charge in that window is refused, whatever its
	// size. It stops answering true when that window turns over
	// ([SpendOutcome.ResetsAt]); a window with no calendar, a worker's own
	// slice, never turns over. A meter is told of EVERY charge its turn
	// makes — its rounds, its judge, its workers and the turn's auxiliary
	// calls, which the engine post-charges through it — so what it has not
	// seen is spend outside the turn: another seat spending the company's
	// last room, a background pass on this seat's own counter. That is left
	// to the next charge, which is the gate.
	//
	// Certain ON THE CALENDAR THE METER JUDGES BY, which is the one edge: a
	// counter shared with peers can start counting the next window before
	// this meter's clock says the held one is over — a peer whose clock is
	// ahead, or one cutting windows on a calendar that ends this one sooner
	// — and this meter's next charge could then be admitted in that next
	// window while it still refuses. A meter may hold on through that; the
	// error is in the closed direction, so the caller stops as on any
	// refusal and what it loses is the work it stops, never a call billed
	// and then refused. See the engine's meter for why it holds on.
	//
	// The outcome is a refusal the next call is certain to meet, named by
	// the counter's own rule — the company's before the seat's, the window
	// that ends last within a scope — over every window the meter knows to
	// be full, and is what the caller reports ([Refusal]).
	Refused(ctx context.Context) (SpendOutcome, bool)
}

// Refusal is the error a meter's standing refusal answers, or nil: nil for a
// nil meter, and nil while no refusal stands ([BudgetMeter.Refused]).
//
// It is the question the loop asks before every run and every call, exported
// because the loop is not the only frame that calls a model on a turn's meter:
// the round-cap judge, the executor's launch and the engine's auxiliary
// seam before each of the turn's auxiliary calls ask it too, so every call in
// a turn is stopped by the same answer — and, since a true answer is a
// refusal of the call the caller was about to make, recorded by the same
// meter. Ask it only there.
func Refusal(ctx context.Context, meter BudgetMeter) error {
	if meter == nil {
		return nil
	}
	outcome, refused := meter.Refused(ctx)
	if !refused {
		return nil
	}
	return budgetError(outcome)
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

// Is makes every budget breach match [ErrBudgetExhausted], so a caller can test
// the class without naming which ceiling was hit.
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
	invocations  []Invocation
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
		Invocations:  append([]Invocation(nil), p.invocations...),
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
	p.invocations = append([]Invocation(nil), res.Invocations...)
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

	// Rounds is one entry per model request, in order: when it was made,
	// how long it took, who answered and what it cost. See [Round].
	Rounds []Round

	// MaxRounds echoes the cap this invocation ran under, so a caller that
	// renders "round 3 of 8" reads the cap the loop actually enforced
	// rather than a second copy of the config beside it.
	MaxRounds int

	// RunStartedAt is when the CLI run in flight was started, in UTC — the
	// [Invocation.StartedAt] it will be recorded with — on a live snapshot
	// published while one is out, and zero between runs and on a finished
	// Result. Apart from [Result.RoundStartedAt] because a run is many
	// rounds: "this run has been going for 6m" and "this request for 40s"
	// are two different clocks.
	RunStartedAt time.Time

	// Running is the tool calls in flight, in the order they arrived,
	// present only on a live snapshot published while calls run — see
	// [RunningCall]. A LIST because a CLI may issue a response's calls at
	// once. Never on a finished Result or a failure snapshot: a call that
	// has returned is an Execution, and one that never returned is the
	// failure.
	Running []RunningCall

	// Invocations is one entry per CLI run, in order — see [Invocation].
	Invocations []Invocation

	// RoundStartedAt is when the latest round's request was made, in UTC,
	// on a live snapshot: the request in flight while the model is still
	// answering, and the round whose tools are running after it has. What
	// a live view counts "this round has taken 40s" from. A request is in
	// flight from the frame published as a run opens or as the last call
	// of the previous response is answered, and [Result.RoundsUsed] counts
	// it from there, so a snapshot whose RoundStartedAt is later than every
	// [Round] in [Result.Rounds] is a request in flight and one equal to
	// the last is that round's tools. Zero on a finished Result, whose
	// [Result.Rounds] carry every start.
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
	// dangling tool call, the one its caller's resume answers.
	Suspended         bool
	PendingToolCallID string
	PendingToolName   string
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

	// Host is where this loop's tool sessions are opened — this process's
	// [toolsession.Host]. Required: every caller of this loop offers tools,
	// and a CLI reaches them only through a session.
	Host *toolsession.Host

	// ServerTitle is how the session names itself in a CLI's handshake —
	// the seat, for the CLI's own logs. Empty takes the phase.
	ServerTitle string

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

	// MaxRounds bounds the model requests. Required and positive.
	MaxRounds int

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
	// what a run that ends in prose means. Without terminators, a run whose
	// model stopped with no call is the model's answer and a clean finish.
	// With them, it cannot be — a successful terminator ends the run — and
	// the loop re-prompts with a FINISHING corrective naming these tools,
	// under [MaxFinishingCorrectives]. It names the submission rather than
	// the surface, which on the onboarding pass is a whole catalogue of
	// tools that are not how it finishes.
	//
	// ONLY A TERMINATOR THE SURFACE OFFERS COUNTS. One it does not carry
	// cannot be called, so a corrective naming it is a run spent on
	// nothing, and a loop offering none of its terminators reads prose as
	// its answer exactly as a loop that declared none.
	TerminateAfter []string

	// CorrectivesSpent is how many finishing correctives the run of
	// declines this invocation CONTINUES has already been sent — by an
	// earlier invocation of the same phase, the withheld one its caller
	// appended included. It seeds the allowance, so a continuation shares
	// [MaxFinishingCorrectives] with the invocation before it rather than
	// starting a fresh pair. Zero for every invocation that does not
	// continue a run of declines.
	CorrectivesSpent int

	// AllowSuspend permits a tool to suspend this loop — see
	// [ToolResult.Suspend]. Only the Dashboard Assistant's turn sets it: no
	// phase of a seat's turn persists a partial conversation, since the
	// one that outlives its process, the executor, is its CLI's own run.
	AllowSuspend bool

	// Budget is the shared counter. Nil disables charging, which is for
	// tests and for loops the caller has already charged.
	Budget BudgetMeter

	// Fence is checked before every CLI run and before and after every one
	// of the phase's tool calls — because the calls are what reach outside
	// the engine. A non-nil error ends the loop and is returned unwrapped so
	// the caller can recognise its own sentinel.
	//
	// Nil is an open fence. The engine supplies [seat.Host.Fence], which
	// closes when this node stops holding the seat's grant.
	Fence func() error

	// PartialInterval bounds how often a response in flight republishes.
	// Zero takes [partialInterval]; a caller sets it only to make the
	// coalescing deterministic, which is the one honest reason to vary it.
	PartialInterval time.Duration

	// StreamPartials asks the provider to stream, so a response's text
	// reaches OnProgress WHILE it is being written rather than only once it
	// is finished. Off leaves the provider's text unstreamed.
	StreamPartials bool

	// OwnPrompt says the loop's system prompt is its whole instruction, so
	// its runs are sent as calls that replace a vendor harness's own prompt
	// rather than keep it ([llm.Request.Agent] false). The zero value is a
	// seat at work, whose runs keep the harness's prompt and add the seat's
	// after it: every phase of a turn and every worker.
	//
	// The Dashboard Assistant sets it. It is not a seat: it acts as a
	// person, its CLI is run with no tool of its own beside the engine's
	// server, and the harness's prompt is written for a coding agent at a
	// terminal whose tools it would describe.
	OwnPrompt bool

	// OnProgress receives the live view: when a run opens, as each model
	// response is read, before and after each tool call, and when the run
	// ends. Failures are the caller's business — telemetry must never fail
	// a phase — so this returns nothing. It is called from more than one
	// goroutine (a CLI's calls arrive on their own), never concurrently.
	OnProgress func(Result)

	// Progress is the failure view. Nil means the caller does not want
	// partial state on the error path, which is a choice rather than a
	// default: every phase in the engine passes one.
	Progress *Progress

	// Steer is the running turn's notes from a person. A note enters the
	// conversation as a USER message before a run, and nowhere else: one
	// that arrives while a run is going ends that run once its calls in
	// flight are answered ([Steerer.Waiting]), and the next run reads it —
	// never as part of a tool's result, which any tool's output could
	// imitate.
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
	if c.Host == nil {
		errs = append(errs, errors.New("toolloop: Host is required — a CLI reaches the "+
			"phase's tools only through a session on it"))
	}
	if c.MaxRounds <= 0 {
		errs = append(errs, fmt.Errorf("toolloop: MaxRounds must be positive, got %d", c.MaxRounds))
	}
	if c.CorrectivesSpent < 0 || c.CorrectivesSpent > MaxFinishingCorrectives {
		errs = append(errs, fmt.Errorf("toolloop: CorrectivesSpent must be within 0..%d, got %d",
			MaxFinishingCorrectives, c.CorrectivesSpent))
	}
	return errors.Join(errs...)
}

// Run drives the loop: CLI runs of the phase's provider, each calling the
// phase's tools through one [dispatcher], until the phase finishes, parks,
// runs out of rounds or is stopped.
//
// It returns a Result for every outcome that is not an engine failure —
// including a suspend and an exhausted round budget, both of which are things
// the phase did rather than things that went wrong. An error means the
// provider, the budget, the fence or the surface itself failed, and the caller
// publishes Progress.Snapshot() alongside it.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	d := newDispatcher(ctx, cfg, 0)

	// The loop's record. Written by this goroutine between runs and read by
	// a frame published from a call's goroutine during one, so every touch
	// is under rec.
	var rec sync.Mutex
	msgs := append([]llm.Message(nil), cfg.Messages...)
	var execs []Execution
	var narration []Narration
	var rounds []Round
	var invocations []Invocation
	var inTokens, outTokens, cacheRead, cacheWrite int
	var model, providerKey string
	var steers []SteerMark
	var served, keyServed bool
	var partial *Partial
	var runStarted time.Time

	finishingRetries := cfg.CorrectivesSpent
	emptyRetries := 0
	emptyAnswers := 0
	terminated, exhausted := false, false
	withheld := ""

	// state is the loop's record as it stands, the one shape every exit and
	// every publish is built from — so a field added to the Result cannot
	// reach the finished record and miss the live one, or the reverse. On a
	// live frame it includes what the run in flight has done so far.
	state := func(live bool) Result {
		out := Result{
			Text:         assistantText(msgs),
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			CacheRead:    cacheRead,
			CacheWrite:   cacheWrite,
			Executions:   append([]Execution(nil), execs...),
			Narration:    append([]Narration(nil), narration...),
			Rounds:       append([]Round(nil), rounds...),
			Invocations:  append([]Invocation(nil), invocations...),
			RoundsUsed:   d.roundsUsed(),
			MaxRounds:    cfg.MaxRounds,
			Model:        model,
			ProviderKey:  providerKey,
			Messages:     append([]llm.Message(nil), msgs...),
			Steers:       append([]SteerMark(nil), steers...),
		}
		if live {
			more := d.snapshot()
			out.Messages = append(out.Messages, more.messages...)
			out.Text = assistantText(out.Messages)
			out.Executions = append(out.Executions, more.executions...)
			out.Narration = append(out.Narration, more.narration...)
			out.Rounds = append(out.Rounds, more.rounds...)
			out.Running = d.running()
			// THE ROUND IN FLIGHT, and when it began: the request a run's
			// CLI is making while the model answers, or the round whose
			// calls are running — see [Result.RoundStartedAt].
			round, started := d.inFlight(time.Now())
			out.RoundsUsed, out.RoundStartedAt = round, started.UTC()
		}
		return out
	}
	publish := func() {
		if cfg.Progress == nil && cfg.OnProgress == nil {
			return
		}
		rec.Lock()
		defer rec.Unlock()
		live := state(true)
		if cfg.Progress != nil {
			// WHAT SERVED, never the placeholder: a phase whose every
			// provider refused before one response came back was served by
			// nobody, and naming the head on its failure record would
			// charge it for a call it never answered.
			recorded := live
			recorded.Running = nil
			if len(live.Rounds) == 0 {
				recorded.Model, recorded.ProviderKey = "", ""
			}
			cfg.Progress.record(recorded)
		}
		if cfg.OnProgress != nil {
			// A PARTIAL ENDS WHERE ITS RESPONSE IS READ: from then the
			// narration is the round's text, and a frame carrying both
			// shows one round twice.
			if partial != nil && d.committed(partial.Round) {
				partial = nil
			}
			live.Partial = partial.clone()
			live.RunStartedAt = runStarted
			cfg.OnProgress(live)
		}
	}
	d.onFrame = publish

	for {
		// The fence first, at the cheapest possible point: nothing has been
		// spent on this run and nothing has fired.
		if cfg.Fence != nil {
			if err := cfg.Fence(); err != nil {
				return nil, err
			}
		}
		// NO ROUND LEFT: nothing may ask the model again. Before the budget,
		// because no run is about to be made — a window the last round
		// filled exactly is a phase that ran out of rounds, not one the
		// budget refused, and asking [Refusal] would record the refusal of
		// a run nobody was going to start.
		if d.roundsUsed() >= cfg.MaxRounds {
			exhausted = lastAskedForTools(msgs)
			break
		}
		// A REFUSAL ALREADY CERTAIN ends the loop before the run it would
		// refuse is started — the first included, since the refusal may
		// have been answered to another caller on the turn's meter.
		if err := Refusal(ctx, cfg.Budget); err != nil {
			return nil, err
		}
		// A PERSON'S NOTES, as user messages, before the run that is the
		// first to read them.
		if cfg.Steer != nil {
			next := d.roundsUsed() + 1
			rec.Lock()
			for _, note := range cfg.Steer.Drain(next) {
				msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: note.Message})
				steers = append(steers, SteerMark{Round: next, ID: note.ID})
			}
			rec.Unlock()
		}
		d.reopen()

		if model == "" {
			// The CONFIGURED identity, as a placeholder until a completion
			// names the model that served — see [Result.Model].
			model = cfg.Provider.Model()
		}
		if providerKey == "" {
			providerKey = cfg.ProviderKey
		}
		onDelta := streamTo(cfg, &rec, &partial, d, publish)

		began := time.Now()
		rec.Lock()
		runStarted = began.UTC()
		rec.Unlock()
		publish()
		runCtx, runSpan := tracing.Start(ctx, "agent.toolloop", "llm.run",
			attribute.String("crewlet.phase", cfg.Surface.Phase()),
			attribute.Int("crewlet.round", d.roundsUsed()+1))
		completion, err := cfg.Provider.Complete(runCtx, llm.Request{
			Messages: msgs,
			Tools:    d,
			// A seat at work (a phase of its turn or a worker it
			// delegated to) unless the loop's prompt is its own.
			Agent:   !cfg.OwnPrompt,
			OnDelta: onDelta,
		})
		took := time.Since(began)

		// The run is over, and an end it reached is taken; then EVERY CALL
		// OF THE RUN HAS SETTLED before anything is read: a call answered
		// "still running" goes on after its CLI stopped, and its result is
		// part of what the next run is handed.
		d.finish()
		d.awaitSettled()
		if completion != nil {
			// A backend that reported its spend as it went has charged it
			// through the host; one that reported nothing is charged here,
			// once, for what its answer says the run cost.
			d.chargeUnreported(llm.Usage{
				Input: completion.InputTokens, Output: completion.OutputTokens,
				CacheRead: completion.CacheRead, CacheWrite: completion.CacheWrite,
			})
		}

		rec.Lock()
		if completion != nil {
			if !served && completion.Model != "" {
				model, served = completion.Model, true
			}
			if !keyServed && completion.ProviderKey != "" {
				providerKey, keyServed = completion.ProviderKey, true
			}
		}
		runStarted = time.Time{}
		folded := d.fold(completion, model)
		msgs = append(msgs, folded.messages...)
		execs = append(execs, folded.executions...)
		narration = append(narration, folded.narration...)
		rounds = append(rounds, folded.rounds...)
		used := d.takeSpent()
		inTokens += used.Input
		outTokens += used.Output
		cacheRead += used.CacheRead
		cacheWrite += used.CacheWrite
		inv := d.invocation(began, took, completion, err, used)
		invocations = append(invocations, inv)
		partial = nil
		st, cutErr := d.current()
		rec.Unlock()
		endRunSpan(runSpan, inv, err)
		publish()

		switch st {
		case stateFinished:
			terminated = true
		case stateParked:
			rec.Lock()
			out := state(false)
			rec.Unlock()
			out.EmptyAnswers = emptyAnswers
			out.Suspended = true
			out.PendingToolCallID, out.PendingToolName = d.pending()
			return &out, nil
		case stateCut:
			return nil, cutErr
		case stateExhausted:
			exhausted = true
		case stateLocalPark, statePausing, stateRelist:
			// The run was ended for a reason the next run answers: a
			// call's result, a person's note, a tool the CLI could not
			// see. Not a decline, and no corrective.
			continue
		}
		if terminated || exhausted {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("toolloop: %s: %w", cfg.Surface.Phase(), err)
		}
		if completion != nil && completion.StopReason == llm.StopCap {
			// THE CLI'S OWN TURN CAP stopped its loop with the model still
			// working — a backstop set past this phase's budget, so it is
			// the budget's exhaustion by another name.
			exhausted = true
			break
		}

		// THE CLI ENDED ON ITS OWN: its model's last response called
		// nothing. What that response was decides what follows, exactly as
		// a round of prose did when the loop drove each round.
		if folded.called {
			// A run that called a tool clears every stall allowance —
			// a model that just asked for a tool has shown it can.
			finishingRetries, emptyRetries = 0, 0
		}
		last := folded.lastText
		answeredNothing := strings.TrimSpace(last) == ""
		owed := offered(cfg.TerminateAfter, cfg.Surface.ToolDefs())
		if answeredNothing {
			emptyAnswers++
		} else if len(owed) > 0 {
			rec.Lock()
			if n := len(narration); n > 0 {
				narration[n-1].Declined = true
			}
			rec.Unlock()
		}
		lastRound := d.roundsUsed() >= cfg.MaxRounds

		var corrective string
		var allowance *int
		var bound int
		switch {
		case len(owed) > 0:
			// THE LOOP SAID HOW IT FINISHES, so a run that ended without
			// the call is not a finish however it reads — see
			// [Config.TerminateAfter].
			corrective = finishingCorrective(owed)
			allowance, bound = &finishingRetries, MaxFinishingCorrectives
		case answeredNothing:
			// A RUN THAT REACHED NOBODY IS NOT A FINISH: a model that spent
			// its output on hidden reasoning and stopped.
			corrective = emptyAnswerCorrective
			allowance, bound = &emptyRetries, maxEmptyAnswerRetries
		}
		if corrective == "" || *allowance >= bound {
			break
		}
		if lastRound {
			// A CORRECTIVE NOTHING WILL READ IS NOT SENT — see
			// [Result.Withheld].
			if allowance == &finishingRetries {
				withheld = corrective
			}
			break
		}
		*allowance++
		rec.Lock()
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: corrective})
		rec.Unlock()
	}

	rec.Lock()
	defer rec.Unlock()
	out := state(false)
	out.ExhaustedRounds = !terminated && (exhausted ||
		(d.roundsUsed() >= cfg.MaxRounds && lastAskedForTools(msgs)))
	out.EmptyAnswers = emptyAnswers
	out.Withheld = withheld
	out.CorrectivesSpent = finishingRetries
	return &out, nil
}

// endRunSpan closes a CLI run's `llm.run` span with what the run came to.
//
// ONCE EVERY CALL OF THE RUN HAS SETTLED, not when the CLI exits: a call's
// `tool.call` span is a child of the run's, and one answered "still running"
// goes on after its CLI stopped. The tokens are the run's whole spend — what
// the CLI reported request by request and what was charged when it ended —
// because a span that carried only the final answer's count would show a
// twenty-request run as one request's worth.
func endRunSpan(span trace.Span, inv Invocation, err error) {
	span.SetAttributes(
		attribute.String("crewlet.model", inv.Model),
		attribute.Int("crewlet.input_tokens", inv.InputTokens),
		attribute.Int("crewlet.output_tokens", inv.OutputTokens),
		attribute.Int("crewlet.cache_read_tokens", inv.CacheRead),
		attribute.Int("crewlet.cache_write_tokens", inv.CacheWrite),
		attribute.Int("crewlet.tool_calls", inv.Calls),
		attribute.String("crewlet.outcome", string(inv.Outcome)))
	tracing.Fail(span, err)
	span.End()
}

// streamTo is the OnDelta a run streams its text through, or nil where the
// caller asked for no streaming. One partial per response, numbered by the
// response it belongs to: the next one the CLI's record has not shown.
func streamTo(cfg Config, rec *sync.Mutex, partial **Partial, d *dispatcher, publish func()) func(llm.Delta) {
	if !cfg.StreamPartials || cfg.OnProgress == nil {
		return nil
	}
	every := cfg.PartialInterval
	if every <= 0 {
		every = partialInterval
	}
	// ZERO, so the FIRST fragment publishes immediately.
	var last time.Time
	return func(delta llm.Delta) {
		// THE REQUEST THE TEXT BELONGS TO, where the backend named one the
		// host already knows; else the request in flight.
		round, known := d.roundOfRequest(delta.Request)
		if !known {
			round, _ = d.inFlight(time.Now())
		}
		rec.Lock()
		p := *partial
		if p == nil || p.Round != round {
			p = &Partial{Round: round}
			*partial = p
		}
		if delta.Restart {
			// The attempt so far was abandoned. Kept rather than erased —
			// see [Partial] — and the replacement starts from empty so two
			// half-answers never concatenate.
			if p.Content != "" {
				p.Abandoned = append(p.Abandoned, Narration{Round: p.Round, Content: p.Content})
			}
			p.Content = ""
			rec.Unlock()
			publish()
			last = time.Now()
			return
		}
		p.Content += delta.Content
		rec.Unlock()
		// COALESCED on the clock, not on every fragment — see
		// partialInterval.
		if time.Since(last) < every {
			return
		}
		last = time.Now()
		publish()
	}
}

// Charge records tokens a model call has already spent on meter, and answers a
// refusal as the loop reads one: a [*BudgetError], or the counter's own error
// when it could not be reached. Exported for a caller that spends tokens
// outside this loop and must charge them by the same rule: the Dashboard
// Assistant's deep-work run, whose tokens are spent in a coding CLI's own loop.
func Charge(ctx context.Context, meter BudgetMeter, tokens int) error {
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
	return budgetError(outcome)
}

// budgetError is the error a refusal answers, the meter's outcome carried
// unchanged — one constructor for a refusal the loop was just told and one a
// meter already held, so the two read alike everywhere they are reported.
func budgetError(outcome SpendOutcome) *BudgetError {
	scope := outcome.Scope
	if scope == "" {
		scope = "org"
	}
	return &BudgetError{
		Scope: scope, Used: outcome.Used, Limit: outcome.Limit,
		Period: outcome.Period, Window: outcome.Window, ResetsAt: outcome.ResetsAt,
	}
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
	names := make([]words.Said, 0, len(terminators))
	for _, name := range terminators {
		names = append(names, words.Said("`"+name+"`"))
	}
	call := string(names[0])
	if len(names) > 1 {
		call = "one of " + words.Or(names)
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

// assistantText renders a conversation's assistant turns as ONE displayable
// string.
//
// This is the SINGLE builder of the response shown on both the per-round live
// update and the durable phase record. One function over one message list is
// what makes the dashboard's streaming row and the turn you expand afterwards
// the same text — they were assembled separately once, and the live row
// streamed its tool calls against an empty response.
//
// A CORRECTIVE'S ANSWER IS NOT THE PHASE'S. In a run of rounds that called no
// tool, the first one that said something is the model's answer to its task;
// every round of that run after it answers a corrective instead — "you have
// not finished, call the tool" — and a model that still does not call writes
// its report out again, near-verbatim. Joined whole, a phase that kept
// declining through both finishing correctives handed three copies of one
// report to whoever reads the text: a worker's `no_result` prose to its
// parent, a reviewer's to the record of its verdict. So those later rounds are
// left out, and the text is what the model answered the task with. A round that called a tool ends the run of
// declines, so a model that worked and then stopped again is a new answer, and
// kept.
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
		if part := strings.TrimSpace(m.Content); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}
