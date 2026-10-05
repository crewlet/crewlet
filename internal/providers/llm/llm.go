// Package llm is the contract every language-model backend implements, and
// the vocabulary a turn speaks to one.
//
// It is deliberately small and deliberately dumb about failure. Two things it
// does NOT do, each learned the expensive way:
//
//   - It does not retry. Every backend sets its SDK's retry count to zero,
//     because retrying inside a provider hides the one signal the layers above
//     need — which credential failed and why. Rotation belongs to the
//     credential pool, fallback to the chain, and neither can act on an error
//     the SDK already swallowed and re-tried into a timeout.
//   - It does not decide what a failure MEANS beyond a coarse kind. A backend
//     classifies; the pool and the chain decide. Putting the policy in the
//     backend gave three backends three subtly different ideas of "exhausted".
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Role names a message's author. Strings rather than an enum because they go
// on the wire to providers that each accept their own set, and a backend
// translates.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one turn of a conversation.
//
// An assistant turn carries TWO accounts of what the model wrote, for two
// different readers. The NEUTRAL view — Content, ReasoningContent,
// ThinkingBlocks, ToolCalls — is what the engine reads: the tool loop runs the
// calls, the record shows the prose, and a backend from another vendor
// rebuilds the turn in its own shape from it. Raw is what the VENDOR reads:
// the response's own content blocks, exactly as they arrived, handed back to a
// backend of the same wire format on the next call (see [Message.Raw] for why
// a rebuild from the neutral view is not good enough).
//
// The JSON tags are a WIRE FORMAT, and they are the field names on purpose.
// A suspended agent turn serializes its whole conversation into a durable row
// that another BUILD reads back days later
// ([github.com/crewlet/crewlet/internal/agent/execstate]), and untagged these
// keys are whatever the Go field happens to be called — so a rename here is
// an unresumable run with no compile error and no symptom but a refusal in a
// log. Pinned to the spellings already on the wire; changing one is a format
// change and needs a version bump on the other side.
type Message struct {
	Role             string          `json:"Role,omitempty"`
	Content          string          `json:"Content,omitempty"`
	ReasoningContent string          `json:"ReasoningContent,omitempty"`
	ThinkingBlocks   []ThinkingBlock `json:"ThinkingBlocks,omitempty"`
	ToolCalls        []ToolCall      `json:"ToolCalls,omitempty"`
	ToolCallID       string          `json:"ToolCallID,omitempty"`
	Name             string          `json:"Name,omitempty"`

	// Origin is which backend wrote this turn, and on which model — set on
	// an assistant turn from the [Completion] it records, zero on every
	// other. It is what tells a backend whether Raw is in its own format.
	Origin Origin `json:"Origin,omitzero"`

	// Raw is the vendor's own content blocks for this assistant turn, in
	// the order the model wrote them, each exactly as the response carried
	// it — set by a backend whose wire format has such blocks (Anthropic's
	// Messages API), and replayed by that backend VERBATIM on every later
	// call of the conversation.
	//
	// THE NEUTRAL VIEW CANNOT BE REPLAYED, because it is lossy in exactly
	// the places a vendor checks. Rebuilt from it, a turn written as
	// [thinking, text, thinking, tool_use] comes back as [thinking,
	// thinking, text, tool_use] with the two texts joined, a tool call's
	// input is re-encoded from a decoded map, and a block type the engine
	// does not model is dropped. Claude Opus 5.5, Sonnet 5.5 and Fable 5.1
	// bind every thinking block to the conversation before it, byte for
	// byte; an earlier turn that comes back different invalidates every
	// block after it, and on an account the vendor enforces that is a 400
	// the fallback chain does not retry.
	//
	// THE NEUTRAL VIEW STAYS BESIDE IT, because it is what the engine and
	// every other backend read. The two are written together, from one
	// response, by the backend that decoded it, and nothing edits either
	// afterwards — the conversation is append-only.
	Raw []json.RawMessage `json:"Raw,omitempty"`

	// Binding is what the request that wrote this assistant turn carried
	// BESIDE its messages, as a digest in the writing backend's own terms
	// — set with Raw, by the backend whose vendor binds a turn's reasoning
	// to it, and compared by that backend on every later call. Anthropic's
	// is the top-level system prompt and the set of tool definitions, as
	// sent: Claude Opus 5.5, Sonnet 5.5 and Fable 5.1 refuse a thinking
	// block replayed into a request whose system prompt or tools differ
	// from the ones it was written under, so a turn whose Binding is not
	// the current request's has its thinking SHED rather than replayed —
	// oldest first, which is the one removal the vendor accepts (see
	// internal/providers/llm/anthropic).
	//
	// Empty on a turn written before it existed, which is a binding nobody
	// can show matches: such a turn's thinking is shed too.
	Binding string `json:"Binding,omitempty"`

	// Failed marks a tool message whose call FAILED — the tool refused,
	// errored, or never ran because its arguments did not parse. The
	// Content still says why; this is the vendor's structured flag beside
	// it (Anthropic's `is_error`), for a backend whose wire format has one.
	// A backend without one sends the content alone, which already reads
	// as the failure it is. Meaningless on any other role.
	Failed bool `json:"Failed,omitempty"`
}

// Origin is the backend and the model that wrote an assistant turn.
//
// Provider is the backend's TYPE, which is the name of the wire format [Raw]
// is in — `anthropic`, `openai`, `cli-agent` — and never an entry key or a
// label: an `openai-compatible` entry is still the `openai` wire format, and
// [Error.Provider], which such an entry relabels with its key, is where a
// label goes. Model is the configured model id, as [Completion.Model].
//
// A backend replays Raw when Provider is its own type, WHATEVER THE MODEL. A
// turn one Claude model wrote is handed back unchanged to another — a chain's
// fallback member, a resumed run on a config that moved — because the vendor
// decides which model reads which thinking block and drops the ones the model
// in front of it cannot, unbilled and without failing the call; a client that
// stripped them itself would remove blocks from the middle of the
// conversation's sequence, which is the one edit that fails every block after
// it when the conversation returns to the model that wrote them. Model is the
// record of which one did.
//
// [Raw]: Message.Raw
type Origin struct {
	Provider string `json:"Provider,omitempty"`
	Model    string `json:"Model,omitempty"`
}

// ThinkingBlock is a provider's structured reasoning block, in the neutral
// view. A backend that replays thinking does so from [Message.Raw], which
// holds the same block in its own place among the turn's others; this copy is
// what the engine measures, and what a build that predates Raw replays from a
// conversation a newer one parked (a rolling upgrade resumes runs across the
// two), which is why its fields — Signature included — stay.
type ThinkingBlock struct {
	Type      string `json:"Type,omitempty"`
	Thinking  string `json:"Thinking,omitempty"`
	Signature string `json:"Signature,omitempty"`
	Data      string `json:"Data,omitempty"`
}

// ToolDef is a tool offered to the model. Parameters is a JSON Schema object.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is the model asking for a tool to run.
type ToolCall struct {
	ID        string         `json:"ID,omitempty"`
	Name      string         `json:"Name,omitempty"`
	Arguments map[string]any `json:"Arguments,omitempty"`

	// ArgumentsError is why the arguments the model wrote could not be
	// decoded, and empty when they could. Arguments is then EMPTY, and the
	// call must not run: running it with no arguments is a write the model
	// never asked for (a search over everything, a post with no body), and
	// it is what a round the output cap cut mid-call used to do. The tool
	// loop answers such a call with this reason as a failed result instead,
	// which the model can read and fix.
	ArgumentsError string `json:"ArgumentsError,omitempty"`
}

// StopReason is why a model stopped writing one response, normalised across
// vendors so the tool loop decides on ONE vocabulary — each backend maps its
// own (`end_turn`, `length`, `content_filter`, …) onto these.
//
// THE ZERO VALUE IS "NOT REPORTED", and it reads as an ordinary end: a backend
// that names no reason has not said the response was cut short, and treating
// silence as a truncation would fail every round of a provider that simply
// omits the field. Every backend in this tree reports one; the zero value is
// for a third-party [Provider] and a test double.
type StopReason string

const (
	// StopEnd is a response the model finished: it wrote what it meant
	// to and stopped (or reached a stop sequence the caller set).
	StopEnd StopReason = "end"

	// StopToolUse is a response that ended to let its tool calls run.
	StopToolUse StopReason = "tool_use"

	// StopMaxTokens is a response the output cap cut off. Whatever it
	// was writing is incomplete — prose mid-sentence, a tool call's
	// arguments mid-object — so nothing in it may be acted on as though
	// the model had finished.
	StopMaxTokens StopReason = "max_tokens"

	// StopRefusal is a model declining the request on policy grounds. A
	// backend never hands one back as a Completion: it returns a
	// [KindRefusal] error carrying a [Refusal], so a caller that reads
	// only Content cannot mistake a refusal for an answer. The value
	// exists for the record of the round it ended.
	StopRefusal StopReason = "refusal"

	// StopContextExceeded is a response cut off because the conversation
	// filled the model's context window — the prompt plus what it wrote
	// reached the limit, which no re-prompt can fix by being longer.
	StopContextExceeded StopReason = "context_exceeded"

	// StopPaused is a vendor pausing a long-running SERVER tool turn for
	// the caller to continue (Anthropic's `pause_turn`). The engine sends
	// no server tools, so it is a protocol state this engine never asked
	// for — reported, never continued.
	StopPaused StopReason = "paused"
)

// stopReasons is the closed set, written once: [StopReason.Valid] reads it,
// and so does the gate holding the dashboard's copy of it.
var stopReasons = []StopReason{
	StopEnd, StopToolUse, StopMaxTokens, StopRefusal, StopContextExceeded, StopPaused,
}

// StopReasons is every named stop reason — a copy, so no caller can edit the
// set [StopReason.Valid] answers from.
func StopReasons() []StopReason { return slices.Clone(stopReasons) }

// Valid reports whether r is one of the named reasons. The zero value is not
// one: it is the absence of a reason (see [StopReason]).
func (r StopReason) Valid() bool { return slices.Contains(stopReasons, r) }

// Refusal is a model declining a request on policy grounds — the cause a
// [KindRefusal] error carries.
//
// It carries the Completion the refusal arrived on because that response was
// BILLED: the prompt was read and the vendor charges for it, so the frame that
// meters spend has to see its usage even though nobody may act on its content.
// Without it every refused round would be the one call the budget never
// counted — and [Billed] is how every such frame reads it.
type Refusal struct {
	// Category is the vendor's policy category (Anthropic's
	// `stop_details.category`: cyber, bio, …), and empty when the vendor
	// named none — OpenAI's content filter names none.
	Category string

	// Explanation is the vendor's human-readable account, and empty when
	// it gave none. Not guaranteed stable, so nothing may match on it.
	Explanation string

	// Completion is the refused response: its usage, the model that
	// served it, and StopReason [StopRefusal]. Whatever partial text it
	// carries is NOT an answer.
	Completion *Completion
}

func (r *Refusal) Error() string {
	msg := "the model declined the request"
	if r.Category != "" {
		msg += " (" + r.Category + ")"
	}
	if r.Explanation != "" {
		msg += ": " + r.Explanation
	}
	return msg
}

// Refused builds the classified error a backend returns for a refusal, so
// every backend says it in one shape.
func Refused(provider, model string, refusal *Refusal) *Error {
	return &Error{Kind: KindRefusal, Provider: provider, Model: model, Err: refusal}
}

// Billed is the completion a call's spend is metered from: the answer when
// there is one, otherwise the refused response a [Refusal] in err carries,
// and nil when the call billed nothing a caller can see.
//
// EVERY FRAME THAT METERS SPEND READS THROUGH THIS, never a bare
// `completion != nil`. A refusal is returned as an error with a nil
// completion — so nobody can act on its text — but its prompt was read and
// billed, and a meter that charges only a non-nil completion counts every
// refused call as free: the budget gate keeps reading room for a company
// whose passes the model keeps declining. One helper rather than an
// errors.As at each site, because a site that forgot the unwrap compiled,
// passed its own tests and charged nothing.
func Billed(completion *Completion, err error) *Completion {
	if completion != nil {
		return completion
	}
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Completion
	}
	return nil
}

// Completion is one model response.
//
// InputTokens is ALWAYS the full prompt token count for the call, cache reads
// and writes included, so it stays a correct budget figure whatever the cache
// did. CacheRead and CacheWrite break that total down for cost reporting only
// — a reader that adds them to InputTokens is double-counting.
type Completion struct {
	// Model is the model that served THIS call, and it is the field the
	// per-model token breakdown is built from.
	//
	// It lives on the answer rather than on the Provider because that is
	// the only place it can be correct. A fallback chain is shared by
	// concurrent callers, so its Provider.Model() — a method with no
	// argument naming the call it is about — can only ever report the model
	// that answered SOMEBODY's call. Measured on one shared chain with two
	// callers alternating members: 21 reads in 200 named the wrong model.
	// Every backend fills this in; a chain fills it in for a member that
	// left it empty.
	Model string

	// ProviderKey is the providers.llm key of the configured entry that
	// served THIS call — the operator's name for it, where Model is the
	// vendor's. Per call for the reason Model is: a chain shared by
	// concurrent callers can only say which member answered on the answer
	// itself. A CHAIN fills it in from the member that answered; a bare
	// backend leaves it empty, because it does not know what it was
	// configured under. A chain nested in a chain keeps the inner member's,
	// which is the more specific of the two.
	ProviderKey string

	// Provider is the backend that wrote this answer, by its TYPE — the
	// wire format [Completion.Raw] is in. See [Origin.Provider]. Every
	// backend in this tree fills it in.
	Provider string

	Content          string
	ReasoningContent string
	ThinkingBlocks   []ThinkingBlock
	ToolCalls        []ToolCall

	// Raw is the response's own content blocks, verbatim and in order, for
	// a backend whose wire format has them — see [Message.Raw]. Nil from a
	// backend that has none to keep.
	Raw []json.RawMessage

	// Binding is the digest of what this call's request carried beside its
	// messages — see [Message.Binding]. Empty from a backend whose vendor
	// binds nothing to it.
	Binding string

	// StopReason is why the model stopped writing this response — see
	// [StopReason]. Never [StopRefusal] on a Completion a backend returns
	// successfully: a refusal is an error.
	StopReason StopReason

	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
}

// TotalTokens is the figure a budget is charged.
func (c Completion) TotalTokens() int { return c.InputTokens + c.OutputTokens }

// Message is the assistant turn this answer adds to its conversation: the
// neutral view, the vendor's own blocks and where both came from.
//
// ONE PLACE builds it, so a field added to the answer cannot reach the turn's
// record and miss the conversation — which is how a response's blocks would
// stop being replayed with no error anywhere, only a vendor quietly
// discarding the reasoning they carried.
func (c Completion) Message() Message {
	return Message{
		Role:             RoleAssistant,
		Content:          c.Content,
		ReasoningContent: c.ReasoningContent,
		ThinkingBlocks:   c.ThinkingBlocks,
		ToolCalls:        c.ToolCalls,
		Origin:           Origin{Provider: c.Provider, Model: c.Model},
		Raw:              c.Raw,
		Binding:          c.Binding,
	}
}

// Request is one call's inputs. A struct rather than a parameter list because
// it grows, and every provider feature added over time would otherwise become
// another positional argument at forty call sites.
//
// Temperature and MaxTokens spell "unset" DIFFERENTLY, on purpose. Making them
// symmetrical would be the worse contract, so the reason is at each field.
type Request struct {
	Messages []Message

	// Tools are offered, never forced: a backend sends no tool choice, so
	// the model decides whether to call one (every vendor's default when
	// tools are present). There is deliberately no field to force a call.
	// Claude Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos 5.1 answer a forced
	// choice with a 400, which the fallback chain does not retry; every
	// Claude model refuses one while thinking; and some endpoints ignore it
	// altogether. A phase that must end in a call NAMES the call that ends
	// it, and the tool loop asks again when a round finishes without it
	// (internal/agent/toolloop) — which works on every backend, including
	// the ones that would have honoured the force.
	Tools []ToolDef

	// Temperature is a POINTER because 0.0 is a real request — it is what a
	// judge or a classifier asks for when it needs a reproducible answer —
	// and a plain float64 cannot tell that apart from a caller who said
	// nothing. Nil means "leave it to the provider".
	//
	// A REQUEST, NOT A GUARANTEE: a backend drops it where the model has
	// no sampling parameter at all or the call is thinking, rather than
	// failing the call over it — the current Claude generation answers
	// any temperature with a 400, and every Claude model takes nothing
	// but 1 while it thinks (internal/providers/llm/anthropic). A model
	// that cannot be asked for a 0 cannot honour one, and a caller asking
	// for reproducibility did not ask for a failed call.
	//
	// This is the rule the whole tree follows: a field whose zero value is
	// a legitimate SETTING may not use the zero value to mean "unset". A
	// plain float here would make omission mean 0.0 and run every phase in
	// the engine deterministic without anyone choosing it. A backend sends
	// it exactly when it is non-nil and sends nothing otherwise — never a
	// default of its own in its place, which would be a temperature nobody
	// chose, and never after testing it against zero, which is how a
	// judge's 0 gets lost.
	Temperature *float64

	// MaxTokens is a plain int, with 0 meaning unset — deliberately not a
	// pointer like Temperature. Nobody ever means "generate zero tokens", so
	// the zero has no honest reading to protect and a pointer would buy a
	// nil check at every call site for nothing.
	//
	// It caps the ANSWER, so it is honoured only on a call that is not
	// thinking. A thinking model spends its thinking from the same output
	// budget, and a cap sized for a short answer is spent before the
	// answer starts — an empty completion that reads as a model with
	// nothing to say. Where the call thinks, the backend sends the model's
	// own ceiling instead; the token budgets, not this, bound the spend.
	MaxTokens int

	// Effort is the MOST effort this call is worth: a CEILING on how hard
	// the model thinks, never a floor. Empty is no ceiling, and the entry's
	// own level applies. A backend sends the lower of this and the level
	// its entry is configured for ([Effort.AtMost]), and sends nothing on a
	// model that takes no effort at all.
	//
	// A ceiling rather than a level because the CALLER knows what the call
	// is for and the OPERATOR knows what the entry costs, and neither may
	// overrule the other upward: a classifier asking for `low` must not
	// run at the `high` an executor's entry is set to, and nothing a call
	// says may spend more than the operator configured. A thinking model
	// spends its thinking out of the same output budget as its answer, so
	// a short classifier answer at a high effort is the empty-answer
	// failure the judge and the knowledge passes describe.
	//
	// The cli-agent backend ignores it: a CLI takes its effort from its own
	// configuration and has no per-call flag to carry this on.
	Effort Effort

	// OnDelta, when set, asks the backend to stream and calls this as text
	// arrives. Nil — the common case — takes the ordinary unary path.
	//
	// A REQUEST FIELD rather than a second method on [Provider], because
	// streaming is a property of one call and not of a backend: thirteen
	// call sites in this engine want an answer, and exactly one wants to
	// watch it being written. A `Stream` method would make every backend
	// implement something twelve of its callers must remember not to use,
	// and a capability interface asserted at the call site would make
	// "does this stream?" a fact the caller discovers by type switch.
	//
	// Called SYNCHRONOUSLY on the goroutine running the request, in order,
	// so a slow callback slows the read loop — coalesce in the callback,
	// do not fan out from it. A backend that cannot stream never calls it
	// and returns the same Completion it always would; nothing above here
	// may treat the absence of deltas as an error.
	OnDelta func(Delta)
}

// Effort is a level of how hard a model thinks: the vocabulary both vendors'
// effort parameters share (Anthropic's `output_config.effort`, OpenAI's
// `reasoning_effort`).
//
// A NAMED TYPE over a closed, ORDERED set, because the one thing a backend
// does with two of them is take the lower ([Effort.AtMost]), and a level it
// cannot place in the order is a level it cannot compare.
type Effort string

// The levels, lowest first.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// efforts is every level in ascending order — the order [Effort.AtMost]
// compares by.
var efforts = []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// Valid reports whether e is a level, or empty.
//
// EMPTY IS VALID and means no level was named: a request that does not care
// how hard the model thinks must not have to say so.
func (e Effort) Valid() bool {
	return e == "" || e.rank() >= 0
}

// rank is e's place in the order, -1 for anything that is not a level.
func (e Effort) rank() int {
	for i, level := range efforts {
		if e == level {
			return i
		}
	}
	return -1
}

// AtMost is e lowered to ceiling: the lower of the two, or e when the ceiling
// is empty. An empty e stays empty — a level nobody configured is not one this
// can compare, and inventing one could RAISE the effort above whatever the
// vendor's own default is. Both must be [Effort.Valid]; an invalid ceiling is
// no ceiling, so a backend checks before it calls this.
func (e Effort) AtMost(ceiling Effort) Effort {
	if e == "" || ceiling.rank() < 0 || ceiling.rank() >= e.rank() {
		return e
	}
	return ceiling
}

// Delta is a fragment of a completion as it is being written.
//
// It carries what ARRIVED, never the accumulation so far: a consumer appends,
// and appending is the only reading that stays correct when a fragment is
// dropped or replayed. The authoritative text is still the [Completion] the
// call returns — a delta stream is a view of a call in progress, not a second
// source of truth to reconcile against.
type Delta struct {
	// Content and Reasoning are the text this fragment appended.
	Content   string
	Reasoning string

	// Restart says the attempt streamed so far was ABANDONED — its
	// credential or its provider failed partway through — and what follows
	// is a fresh attempt at the same request.
	//
	// It exists because a failover is invisible from inside a delta stream:
	// without it the consumer concatenates two half-answers from two models
	// into one incoherent paragraph. Whether to erase the abandoned text or
	// keep it is the CONSUMER's decision, which is exactly why the signal
	// says what happened rather than what to do about it.
	Restart bool

	// Model names the backend that is answering now. Set on a Restart, so a
	// consumer can say which model gave up and which took over.
	Model string
}

// Send delivers one fragment, and is a no-op when nothing asked to stream or
// the fragment is empty.
//
// A method on the request so every backend spells the nil check and the
// empty check the same way. Written out at four call sites, one of them
// forgot the empty check and published a frame per keep-alive.
func (r Request) Send(d Delta) {
	if r.OnDelta == nil {
		return
	}
	if !d.Restart && d.Content == "" && d.Reasoning == "" {
		return
	}
	r.OnDelta(d)
}

// Streaming reports whether this request asked to be streamed.
func (r Request) Streaming() bool { return r.OnDelta != nil }

// Temp is a pointer to v, for building a [Request] literal — Go cannot take
// the address of a constant, and `t := 0.0; req.Temperature = &t` at every
// call site is how a caller ends up sharing one variable between two requests.
func Temp(v float64) *float64 { return &v }

// Provider is a language-model backend.
type Provider interface {
	// Model is this provider's CONFIGURED identity — what the entry is by
	// default — for log lines and config display.
	//
	// It does NOT answer "which model served that call", and nothing should
	// bill against it. This method once carried that job and could not do
	// it: a fallback chain shared by concurrent callers has no per-call
	// answer a no-argument method can return, so a chain could only honour
	// it for a sequential caller. [Completion.Model] is the per-call fact,
	// and the per-model token breakdown is built from completions — which
	// is what makes a chain reporting its own configured name here harmless
	// rather than wrong.
	Model() string

	Complete(ctx context.Context, req Request) (*Completion, error)
}

// --- failure classification ------------------------------------------------

// ErrorKind is how a provider failure is classified. The backend classifies;
// the credential pool and the fallback chain decide what to do about it.
type ErrorKind int

const (
	// KindFatal is any non-retryable failure: a malformed request, a 404.
	// The chain does NOT try another provider — the next one will refuse
	// it identically, and trying is two more seconds and another log line
	// saying the same thing.
	KindFatal ErrorKind = iota

	// KindRateLimit is 429 or 402 — quota exhausted or a billing problem.
	// The credential pool puts the key into a rate-limit cooldown.
	KindRateLimit

	// KindAuth is 401 or 403. A shorter cooldown than rate-limit, because
	// a token refresh may rescue it. Repeated auth failures on one key with
	// no success in between back off exponentially, so a permanently bad
	// key stops thrashing the provider; one success resets it.
	KindAuth

	// KindTimeout is a network or context timeout, or 408/425. Retryable
	// across the chain, and it NEVER marks the credential exhausted: a
	// timeout is a transport fact, not a key fact.
	KindTimeout

	// KindServer is 5xx. Same treatment as a timeout.
	KindServer

	// KindRefusal is a model declining the request on policy grounds —
	// an answer the vendor returned with 200, not a failure of the call.
	// Its cause is a [*Refusal]. Never retryable and never a credential's
	// fault, and NOT a fatal either, because what the frames above do with
	// it differs: a fatal is a request nobody can serve, a refusal is a
	// decision one model made about this content. The chain does not hand
	// it to the next member — routing a refused request round the models
	// until one answers is circumventing the decision, not recovering from
	// a fault — and the turn that met it is not run again from its trigger.
	KindRefusal
)

func (k ErrorKind) String() string {
	switch k {
	case KindRateLimit:
		return "rate_limit"
	case KindAuth:
		return "auth"
	case KindTimeout:
		return "timeout"
	case KindServer:
		return "server"
	case KindRefusal:
		return "refusal"
	default:
		return "fatal"
	}
}

// Retryable reports whether another member of a fallback chain is worth
// trying. A fatal is not — the next member refuses it identically — and
// neither is a refusal, for the reason [KindRefusal] gives.
func (k ErrorKind) Retryable() bool { return k != KindFatal && k != KindRefusal }

// ExhaustsCredential reports whether this kind should cool the credential
// down. Transport failures must not: cooling a healthy key on a network blip
// is how a fleet talks itself out of every key it has.
func (k ErrorKind) ExhaustsCredential() bool {
	return k == KindRateLimit || k == KindAuth
}

// Error is a classified provider failure.
//
// RetryAfter is carried rather than applied because the layer that knows how
// long to wait is not the layer that decides whether to wait at all.
type Error struct {
	Kind       ErrorKind
	Provider   string
	Model      string
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	return fmt.Sprintf("llm %s/%s: %s: %v", e.Provider, e.Model, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the classification of an error, or KindFatal for anything
// this package did not classify. Fatal is the safe default for an UNRECOGNISED
// error: treating an unknown failure as retryable means a chain walks every
// provider it has for a request none of them can serve, turning one clear
// failure into N slow ones.
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	return KindFatal
}

// KindForStatus maps an HTTP status onto a kind. Shared by every backend so
// two SDKs cannot drift into disagreeing about what a 402 means.
func KindForStatus(status int) ErrorKind {
	switch {
	case status == 401 || status == 403:
		return KindAuth
	case status == 402 || status == 429:
		return KindRateLimit
	case status == 408 || status == 425:
		return KindTimeout
	case status >= 500 && status < 600:
		return KindServer
	default:
		return KindFatal
	}
}

// ParseRetryAfter parses one Retry-After header value in either RFC 9110 form
// — delta-seconds, or an HTTP-date — and reports whether it parsed.
//
// Both forms, because providers send both, and a client that understands only
// the integer form silently falls back to its own guess exactly when the
// server has told it the answer. Negative and absurd values are clamped: a
// date already in the past means "now", and a cooldown longer than a day is a
// clock disagreement rather than an instruction.
func ParseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		return clampRetryAfter(time.Duration(secs * float64(time.Second))), true
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if when, err := time.Parse(layout, raw); err == nil {
			return clampRetryAfter(when.Sub(now)), true
		}
	}
	return 0, false
}

// maxRetryAfter bounds an honoured cooldown. A server asking for longer than a
// day is telling us about its clock, not about our quota, and parking a seat
// for that long on a header is worse than retrying and being refused.
const maxRetryAfter = 24 * time.Hour

func clampRetryAfter(d time.Duration) time.Duration {
	return max(0, min(d, maxRetryAfter))
}
