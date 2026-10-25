// Package llm is the contract a language-model backend implements, and the
// vocabulary a turn speaks to one.
//
// THE BACKEND IS A CODING CLI, and no model is reached over a vendor's HTTP
// API (ADR-0045). Every providers.llm entry is one
// (internal/providers/llm/cliagent), and the only other implementations are
// the seat's fallback chain (internal/providers/llm/chain) and test doubles.
// So the contract is what a CLI can be handed and what it can honestly
// report: a conversation and the tools on offer in, an answer, its tool calls
// and its usage out. A field no CLI reads or writes is not here, because a
// caller that sets one believes it changed something.
//
// It is deliberately small and deliberately dumb about failure. Two things it
// does NOT do, each learned the expensive way:
//
//   - It does not retry. Retrying inside a provider hides the one signal the
//     layer above needs — that this model failed, and why — and fallback to
//     another model belongs to the chain, which cannot act on an error a
//     backend already swallowed and re-tried into a timeout.
//   - It does not decide what a failure MEANS beyond a coarse kind. A backend
//     classifies; the chain decides. Putting the policy in the backend gave
//     three backends three subtly different ideas of "exhausted".
package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Role names a message's author.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one turn of a conversation.
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
	Role       string     `json:"Role,omitempty"`
	Content    string     `json:"Content,omitempty"`
	ToolCalls  []ToolCall `json:"ToolCalls,omitempty"`
	ToolCallID string     `json:"ToolCallID,omitempty"`
	Name       string     `json:"Name,omitempty"`

	// Failed marks a tool message whose call FAILED — the tool refused,
	// errored, or never ran because its arguments did not parse. The
	// Content still says why; this is the flag beside it, which a backend
	// renders so the model reads the result as the failure it is.
	// Meaningless on any other role.
	Failed bool `json:"Failed,omitempty"`
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
	// never asked for (a search over everything, a post with no body). The
	// tool loop answers such a call with this reason as a failed result
	// instead, which the model can read and fix.
	ArgumentsError string `json:"ArgumentsError,omitempty"`
}

// StopReason is why a model stopped writing one response, so the tool loop
// decides on ONE vocabulary and the round's record says which it was.
//
// THE ZERO VALUE IS "NOT REPORTED", and it reads as an ordinary end: a backend
// that names no reason has not said the response was cut short. Every backend
// in this tree reports one; the zero value is for a test double.
//
// TWO REASONS, because a CLI reports two. It is asked for an envelope that is
// either an answer or a set of calls, so those are the only two a backend can
// honestly name; a CLI that hit its own cap or declined the request returns
// that as prose, or as a failure its exit path classifies.
type StopReason string

const (
	// StopEnd is a response the model finished: it wrote what it meant
	// to and stopped.
	StopEnd StopReason = "end"

	// StopToolUse is a response that ended to let its tool calls run.
	StopToolUse StopReason = "tool_use"
)

// stopReasons is the closed set, written once: [StopReason.Valid] reads it,
// and so does the gate holding the dashboard's copy of it.
var stopReasons = []StopReason{StopEnd, StopToolUse}

// StopReasons is every named stop reason — a copy, so no caller can edit the
// set [StopReason.Valid] answers from.
func StopReasons() []StopReason { return slices.Clone(stopReasons) }

// Valid reports whether r is one of the named reasons. The zero value is not
// one: it is the absence of a reason (see [StopReason]).
func (r StopReason) Valid() bool { return slices.Contains(stopReasons, r) }

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
	// model's. Per call for the reason Model is: a chain shared by
	// concurrent callers can only say which member answered on the answer
	// itself. A CHAIN fills it in from the member that answered; a bare
	// backend leaves it empty, because it does not know what it was
	// configured under. A chain nested in a chain keeps the inner member's,
	// which is the more specific of the two.
	ProviderKey string

	Content   string
	ToolCalls []ToolCall

	// StopReason is why the model stopped writing this response — see
	// [StopReason].
	StopReason StopReason

	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
}

// TotalTokens is the figure a budget is charged.
func (c Completion) TotalTokens() int { return c.InputTokens + c.OutputTokens }

// Message is the assistant turn this answer adds to its conversation.
//
// ONE PLACE builds it, so a field added to the answer cannot reach the turn's
// record and miss the conversation.
func (c Completion) Message() Message {
	return Message{Role: RoleAssistant, Content: c.Content, ToolCalls: c.ToolCalls}
}

// Request is one call's inputs. A struct rather than a parameter list because
// it grows, and every feature added over time would otherwise become another
// positional argument at forty call sites.
//
// NO SAMPLING, NO OUTPUT CAP AND NO EFFORT. A coding CLI driven headlessly
// takes none of them per call — each reads its model's behaviour from its own
// configuration — so a field for one would be a setting every caller could
// write and no backend would send. A caller that needs a short answer says so
// in its prompt, which is the one channel every CLI reads.
type Request struct {
	Messages []Message

	// Tools are offered, never forced: there is deliberately no field to
	// force a call. A phase that must end in a call NAMES the call that
	// ends it, and the tool loop asks again when a round finishes without
	// it (internal/agent/toolloop) — which works whatever the model does
	// with a request to call something.
	Tools []ToolDef

	// Agent marks a call made by a seat at work — a round of one phase of
	// its turn — rather than a text function the engine runs on its behalf
	// (a relevance filter, a classifier, a rewrite, a judge).
	//
	// The difference is what the call is FOR, and only the backend knows
	// what that changes: a backend that runs a vendor's own harness keeps
	// the harness's system prompt for an agent call and adds the engine's
	// after it — the vendor writes that prompt for an agent and moves it
	// with every model it ships — and replaces it for any other call, whose
	// system prompt is the whole instruction and whose answer is parsed.
	// False is a text function, which is every caller but the tool loop.
	Agent bool

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

	// StreamTokens asks a backend that streams only by running the call
	// differently to do so: the cli-agent backend then runs a profile's
	// streamed variant (Claude Code's stream-json) instead of the profile as
	// written. Meaningless without OnDelta. Without it, OnDelta streams what
	// the backend streams anyway and nothing about how the call runs changes.
	//
	// AN EXPLICIT OPT-IN rather than implied by OnDelta, because changing how
	// a CLI is run changes what has to be read back, and a caller watching a
	// round for a live view (every seat) must not have its whole runtime moved
	// onto an output shape it never asked for. The Dashboard Assistant asks;
	// seats do not, until a live streamed round has been proven against the
	// installed CLI by `crewlet llm doctor`.
	StreamTokens bool
}

// Delta is a fragment of a completion as it is being written.
//
// It carries what ARRIVED, never the accumulation so far: a consumer appends,
// and appending is the only reading that stays correct when a fragment is
// dropped or replayed. The authoritative text is still the [Completion] the
// call returns — a delta stream is a view of a call in progress, not a second
// source of truth to reconcile against.
type Delta struct {
	// Content is the text this fragment appended.
	Content string

	// Restart says the attempt streamed so far was ABANDONED — its
	// provider failed partway through — and what follows is a fresh
	// attempt at the same request.
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

	// Retract says the text THIS attempt streamed so far is not its answer:
	// discard it, and what follows replaces it.
	//
	// Not a Restart, because nothing failed and nothing is tried again. A
	// backend that streams a VIEW of its output rather than the output
	// itself (the cli-agent backend streams the prose inside the envelope
	// it asked the model for) learns only once the call is over whether
	// that view and the authoritative reading agree, and when they do not,
	// keeping the shown text as an abandoned attempt would tell a reader a
	// model had given up when it had answered. The text that follows a
	// Retract is the completion's content from the start.
	Retract bool
}

// Reconcile is what takes a reader who was shown `shown` to `want`: the tail
// to append when shown is a prefix of want, or want whole with restart set
// when it is not, so the reader discards what it drew and starts again.
//
// The one rule every streaming hop applies at the end of what it streamed, so
// "what was shown" and "what is true" can never quietly differ: a backend
// settling its stream against its [Completion], and a consumer settling its
// frames against a round's committed text.
func Reconcile(shown, want string) (tail string, restart bool) {
	if strings.HasPrefix(want, shown) {
		return want[len(shown):], false
	}
	return want, true
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
	if !d.Restart && !d.Retract && d.Content == "" {
		return
	}
	r.OnDelta(d)
}

// Streaming reports whether this request asked to be streamed.
func (r Request) Streaming() bool { return r.OnDelta != nil }

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
// the fallback chain decides what to do about it.
type ErrorKind int

const (
	// KindFatal is a call the engine itself cannot make: a prompt it cannot
	// render, a file of the call's it cannot write, a seat's home this host
	// cannot prepare, a CLI it cannot start — exec's own refusal before any
	// child exists, a binary missing or not executable on this host, or no
	// pipe or process to give it. The chain does NOT try another provider.
	// The fault is in front of the CLI, on the engine's side of the exec, so
	// it is the operator's to fix rather than an outage to wait out: a
	// prompt that cannot be rendered fails identically on every member, and
	// a host that cannot start a CLI or write its files is a deployment
	// fault that a walk would paper over by carrying every call on the node
	// to the next member — and its bill — until somebody read why.
	//
	// NOT a CLI that started and failed, however it failed — its exit, its
	// output, the wait for it: that is [KindServer], [KindTimeout] for a
	// deadline, or a marker's kind where the CLI said why in words its
	// profile recognises.
	KindFatal ErrorKind = iota

	// KindRateLimit is a spent plan or a rate limit the CLI reported in its
	// vendor's own words. The backend sits the entry out for a while
	// (internal/providers/llm/cliagent), and the chain moves on.
	KindRateLimit

	// KindAuth is a login the CLI reports expired or missing. Retryable
	// across the chain, so a seat keeps working while somebody signs the
	// CLI in again.
	KindAuth

	// KindTimeout is a call that ran out of time — the CLI's own wall-clock
	// cap, or a seat queued behind others for a slot. Retryable across the
	// chain.
	KindTimeout

	// KindServer is a CLI that ran and gave no usable answer, for no reason
	// its profile recognises: a run it reports failed — a non-zero exit, or
	// its own failure flag — over a vendor error it relayed, a crash or
	// words no marker knows; a run not seen to its end — its stdout still
	// held by something it left running, a wait for it that failed; no
	// output at all; output clipped past the cap; none where its profile
	// looks. Same treatment as a timeout: the next member is another login,
	// vendor or binary that may well answer, and the entry is not sat out,
	// since nothing says the failure lasts.
	KindServer
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
	default:
		return "fatal"
	}
}

// Retryable reports whether another member of a fallback chain is worth
// trying: every kind but a fatal, which is a fault of the engine's or its
// host's for the operator to fix rather than a failure one member's CLI or
// vendor had.
func (k ErrorKind) Retryable() bool { return k != KindFatal }

// Error is a classified provider failure.
//
// ITS TEXT IS SHOWN, wherever the failure goes — a log line, an event's last
// error, a phase's and a turn's error — so a backend puts nothing in Err it
// would not show.
type Error struct {
	Kind     ErrorKind
	Provider string
	Model    string
	Err      error
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
