// Package llm is the contract a language-model backend implements, and the
// vocabulary a turn speaks to one.
//
// THE BACKEND IS A HARNESS, and no model is reached over a vendor's HTTP
// API (ADR-0045). Every providers.llm entry is one
// (internal/providers/llm/harness), and the only other implementations are
// the seat's fallback chain (internal/providers/llm/chain) and test doubles.
// So the contract is what a harness can be handed and what it can honestly
// report: a conversation in, an answer and its usage out — and, for a seat at
// work, the phase's tools beside the conversation as a [ToolHost], which the
// backend serves to its harness as one MCP server. The harness calls them NATIVELY, in
// its own loop, so there are no tool calls in an answer: they happened during
// the call, through the host, which recorded them. A field no harness reads or
// writes is not here, because a caller that sets one believes it changed
// something.
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
//
// # What a call carries
//
// Tools are offered, never forced: there is no tool-choice field. There is no
// temperature, output cap, effort or reasoning field either, because no
// harness takes one per call, and a field no backend sends is a setting every
// caller believes it changed; a caller that needs a short answer says so in
// its prompt. A [Completion] names three stop reasons (end, tool_use, cap),
// the three a harness can name; one that hit its ceiling or declined says so
// in prose or fails.
//
// An entry has no mode. Every phase runs the harness's own loop over the
// phase's tools: a seat's executor launches the harness in a box
// (internal/sandbox) with the seat's tools bridged in, so only an entry whose
// harness has an in-box runner can be an executor; every other phase (the
// reviewer, a worker, the onboarding pass, a side pass) runs on this host over
// a [ToolHost], which any harness serves.
//
// [ServeOptions] is what the backend tells the host about its harness, since
// the host decides with it. A harness whose record shows no requests counts as
// one round and is never refused for it.
package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
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
// A Dashboard Assistant turn parked on a proposal serializes its whole
// conversation into a durable record that another node, or another BUILD,
// reads back once its owner decides (internal/assistant's parked record), and
// untagged these keys are whatever the Go field happens to be called — so a
// rename here is an unresumable turn with no compile error and no symptom but
// a refusal in a log. Pinned to the spellings already on the wire; changing
// one is a format change and needs a version bump on the other side. A seat's
// turn persists none: what it parks on is its executor's run, and the turn's
// bookkeeping across that park is internal/agent/execstate's.
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

// StopReason is why a model stopped writing — one response, or one harness run's
// whole loop — so the tool loop decides on ONE vocabulary and the record says
// which it was.
//
// THE ZERO VALUE IS "NOT REPORTED", and it reads as an ordinary end: a backend
// that names no reason has not said the response was cut short.
//
// THREE REASONS, the ones a harness can honestly name. A model request ends in an
// answer or in calls to run; a harness run ends when its model answers with no
// call, or when the harness's OWN turn cap stops it, which each harness that has one
// reports in a shape its profile recognises. A run the engine stopped is not
// a reason the model gave, and is the tool loop's to record.
type StopReason string

const (
	// StopEnd is a response the model finished: it wrote what it meant
	// to and stopped.
	StopEnd StopReason = "end"

	// StopToolUse is a response that ended to let its tool calls run.
	StopToolUse StopReason = "tool_use"

	// StopCap is a harness run its own turn cap ended, with the model still
	// working: the harness stopped asking it rather than the model stopping.
	StopCap StopReason = "cap"
)

// stopReasons is the closed set, written once: [StopReason.Valid] reads it,
// and so does the gate holding the dashboard's copy of it.
var stopReasons = []StopReason{StopEnd, StopToolUse, StopCap}

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

	// Content is the answer: for a call with tools, what the model said in
	// its LAST response of the run — its earlier responses are the host's,
	// observed as the run went.
	Content string

	// StopReason is why the model stopped writing — see [StopReason].
	StopReason StopReason

	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
}

// TotalTokens is the figure a budget is charged.
func (c Completion) TotalTokens() int { return c.InputTokens + c.OutputTokens }

// Image is one picture a request hands the model: its bytes and the media
// type they were sniffed as (image/png, image/jpeg, image/webp, image/gif).
type Image struct {
	MediaType string
	Data      []byte
}

// ImageReader is a provider that can say whether its calls take images.
type ImageReader interface {
	ReadsImages() bool
}

// ErrNoImages is a request carrying images to a backend that takes none.
var ErrNoImages = errors.New("llm: this model's harness takes no image")

// Request is one call's inputs. A struct rather than a parameter list because
// it grows, and every feature added over time would otherwise become another
// positional argument at forty call sites.
//
// NO SAMPLING, NO OUTPUT CAP AND NO EFFORT. A harness driven headlessly
// takes none of them per call — each reads its model's behaviour from its own
// configuration — so a field for one would be a setting every caller could
// write and no backend would send. A caller that needs a short answer says so
// in its prompt, which is the one channel every harness reads.
type Request struct {
	Messages []Message

	// Tools is the phase's live tool host, served to the harness as ONE MCP
	// server for the length of the call, or nil for a call that offers none
	// — a text function. See [ToolHost].
	//
	// Offered, never forced: there is deliberately no way to force a call.
	// A phase that must end in a call NAMES the call that ends it, and the
	// tool loop asks again when a run finishes without it
	// (internal/agent/toolloop) — which works whatever the model does with a
	// request to call something.
	Tools ToolHost

	// Images are pictures handed to the model beside the prompt, in order:
	// what a person attached to a question. A backend whose harness takes no
	// image refuses a request carrying one ([ErrNoImages]) rather than drop
	// it, so a caller asks [ImageReader] first.
	Images []Image

	// Agent marks a call made by a seat at work (a round of one phase of its
	// turn) rather than a text function the engine runs on its behalf (a
	// relevance filter, a classifier, a rewrite, a judge).
	//
	// A harness backend keeps the harness's own system prompt for an agent call
	// and adds the engine's after it (the vendor writes it for an agent), and
	// replaces it for any other call, whose system prompt is the whole instruction
	// and whose answer is parsed. False is every caller but a seat's tool loop
	// and the Dashboard Assistant's (toolloop's Config.OwnPrompt).
	Agent bool

	// OnDelta, when set, asks the backend to stream and calls this as text
	// arrives. Nil — the common case — takes the ordinary unary path.
	//
	// A REQUEST FIELD, not a method on [Provider] or a capability interface:
	// streaming is a property of one call, and exactly one caller wants it, so a
	// `Stream` method would burden every backend and an assertion at the call
	// site would make "does this stream?" a type switch.
	//
	// Called SYNCHRONOUSLY on the request's goroutine, in order, so a slow
	// callback slows the read loop: coalesce, do not fan out. A backend that
	// cannot stream never calls it; the absence of deltas is not an error.
	OnDelta func(Delta)
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

	// Request is the harness's id of the model request this text belongs to
	// ([Turn.ID]), where the backend knows it. A harness's record can show a
	// request before its prose is written, so text that arrives after the
	// host was told of the request is filed under it by this id — read by
	// arrival instead, it was filed under the request after.
	Request string
}

// Reconcile is what takes a reader who was shown `shown` to `want`: the tail
// to append when shown is a prefix of want, or want whole with restart set
// when it is not, so the reader discards what it drew and starts again.
//
// The one rule a consumer applies at the end of what it streamed, so "what
// was shown" and "what is true" can never quietly differ: the Dashboard
// Assistant settling its frames against a request's committed text.
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
	if !d.Restart && d.Content == "" {
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
	// render, a file it cannot write, a seat's home it cannot prepare, a harness
	// it cannot start (a binary missing or not executable, no pipe or process).
	// The chain does NOT try another provider: the fault is in front of the
	// harness, on the engine's side of the exec, so it is the operator's to fix,
	// and a walk would paper over it by carrying every call on the node to the
	// next member, and its bill, until somebody read why.
	//
	// NOT a harness that started and failed, however it failed: that is
	// [KindServer], [KindTimeout] for a deadline, or a marker's kind where the
	// harness said why in words its profile recognises.
	KindFatal ErrorKind = iota

	// KindRateLimit is a spent plan or a rate limit the harness reported in its
	// vendor's own words. The backend sits the entry out for a while
	// (internal/providers/llm/harness), and the chain moves on — a call to
	// its next member, and a seat's executor launch to the next member that
	// can run it.
	KindRateLimit

	// KindAuth is a login the harness reports expired, missing or not entitled
	// to the harness. Retryable across the chain, so a seat keeps working while
	// somebody signs the harness in again or changes what its account allows.
	KindAuth

	// KindTimeout is a call that ran out of time — the harness's own wall-clock
	// cap, or a seat queued behind others for a slot. Retryable across the
	// chain.
	KindTimeout

	// KindServer is a harness that ran and gave no usable answer, for no reason
	// its profile recognises: a run it reports failed — a non-zero exit, or
	// its own failure flag — over a vendor error it relayed, a crash or
	// words no marker knows; a run not seen to its end — its stdout still
	// held by something it left running, a wait for it that failed; no
	// output at all; output clipped past the cap; none where its profile
	// looks. Same treatment as a timeout: the next member is another login,
	// vendor or binary that may well answer, and the entry is not sat out,
	// since nothing says the failure lasts.
	KindServer

	// KindCancelled is a call its CALLER gave up on: a person stopped the
	// turn, a node began to drain, a seat's turn was stopped. The harness it
	// started was signalled with it (and exits 143, or 130, or not at
	// all), which says nothing about the harness, its login or its vendor, so
	// NOTHING is learned from it: the chain does not try another member,
	// the entry's readiness is not touched, and it is not an incident to
	// log. Decided from the caller's context, never from how the process
	// ended: a harness the engine killed exits exactly as one that crashed on a
	// signal of its own.
	KindCancelled

	// KindProfileDrift is a harness whose output parsed, but where its profile
	// looks for the answer found nothing: this build no longer reads the
	// installed release, which is ours or the operator's to mend and no outage
	// of the vendor's. Treated as [KindServer] is: the chain walks on, since
	// another member may read fine, and the entry is not sat out.
	KindProfileDrift
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
	case KindCancelled:
		return "canceled"
	case KindProfileDrift:
		return "profile_drift"
	default:
		return "fatal"
	}
}

// Retryable reports whether another member of a fallback chain is worth
// trying: every kind but a fatal, which is a fault of the engine's or its
// host's for the operator to fix rather than a failure one member's harness or
// vendor had, and a cancellation, which no member can answer since nobody is
// waiting for the answer.
func (k ErrorKind) Retryable() bool { return k != KindFatal && k != KindCancelled }

// Error is a classified provider failure.
//
// ITS TEXT IS SHOWN, wherever the failure goes — a log line, an event's last
// error, a phase's and a turn's error — so a backend puts nothing in Err it
// would not show.
type Error struct {
	Kind     ErrorKind
	Provider string
	Model    string
	// Key is the providers.llm key of the entry that failed, so a remedy
	// can name the entry an operator signs in again; empty where the
	// backend was built without one.
	Key string
	// RetryAt is when the entry is tried again, for a [KindRateLimit] whose
	// backend sits the entry out until then; zero where no instant is known.
	RetryAt time.Time
	// ResetsAt is when the vendor said the limit lifts, for a
	// [KindRateLimit] whose report named an instant; zero where it did not.
	// It is the fact a person is told ("resumes at 4:30 PM"), and may lie
	// past RetryAt, which a backend caps so that a raised plan is noticed.
	ResetsAt time.Time
	// Window is which of the vendor's limits refused it, for a
	// [KindRateLimit]: the plan's session window, its weekly one, or one
	// model's own ([LimitWindow]). Empty where the backend cannot tell.
	Window LimitWindow
	// Harness is the harness the entry drives (`claude-code`), and Subscription
	// whether that harness signs in on a plan rather than a metered key: what
	// a person is told a limit is OF ("Claude Code subscription limit
	// reached"), without the entry's configuration, which only an admin
	// reads. Empty and false for a backend that is not a harness's.
	Harness      string
	Subscription bool
	Err          error
}

// LimitWindow is which of a vendor's limits a report names: what a person is
// told the limit is OF ("the weekly limit"), and whether it holds every model
// of a plan or one model alone.
//
// AN ENUM OF THE ENGINE'S OWN WORDS, not the vendor's: Claude Code says
// `five_hour` in its events and "session limit" in its sentences, and both are
// [LimitSession]. A profile maps each vendor's spelling onto these, so
// everything that reads a window reads one vocabulary.
type LimitWindow string

const (
	// LimitSession is a plan's short rolling window (Claude Code's five
	// hours), shared by every model on the plan.
	LimitSession LimitWindow = "session"
	// LimitWeekly is a plan's weekly window, shared by every model on it.
	LimitWeekly LimitWindow = "weekly"
	// LimitModel is a window one model has of its own ("Opus limit"): it
	// holds that model only, and another model on the same plan still
	// answers.
	LimitModel LimitWindow = "model"
	// LimitUnknown is a limit whose report named no window.
	LimitUnknown LimitWindow = "unknown"
)

// LimitWindows is the closed set, in the order a person reads them: what
// [LimitWindow.Valid] admits, and what a reader that draws a window (the
// Models screen) is held to.
var LimitWindows = []LimitWindow{LimitSession, LimitWeekly, LimitModel, LimitUnknown}

// Valid reports whether w is one of [LimitWindows].
func (w LimitWindow) Valid() bool { return slices.Contains(LimitWindows, w) }

// LimitState is where a vendor says one of its windows stands.
type LimitState string

const (
	// LimitOK is a window with room left.
	LimitOK LimitState = "ok"
	// LimitWarning is a window the vendor says is close to its limit.
	LimitWarning LimitState = "warning"
	// LimitReached is a window at its limit: the vendor refuses calls on it.
	LimitReached LimitState = "reached"
)

// LimitStates is the closed set, in escalating order.
var LimitStates = []LimitState{LimitOK, LimitWarning, LimitReached}

// Valid reports whether s is one of [LimitStates].
func (s LimitState) Valid() bool { return slices.Contains(LimitStates, s) }

// LimitReading is what a vendor said of one of its limit windows on a call,
// in the engine's words.
type LimitReading struct {
	Window LimitWindow
	// Hours is the window's length as the vendor names it (5, 168), 0 where
	// it names none.
	Hours int
	// Model is the model family a [LimitModel] window is of ("opus"), "" for
	// a window of the whole plan.
	Model string
	State LimitState
	// Used is the fraction of the window used, nil where the vendor did not
	// say. It may pass 1: a vendor lets some work run past a window's cap.
	Used *float64
	// ResetsAt is when the window turns over, zero where the vendor did not say.
	ResetsAt time.Time
}

// Key is what tells two of a plan's windows apart.
func (r LimitReading) Key() string { return string(r.Window) + "/" + r.Model }

// LimitWarnAt is the fraction of a window from which it reads as a warning
// whatever the vendor's status says: a fifth of a window is what is left to
// move seats to another model before it refuses them.
const LimitWarnAt = 0.8

// LimitStaleAfter is how old a window's reading may be before it is out of
// date: a tenth of the shortest window a vendor names (five hours).
const LimitStaleAfter = 30 * time.Minute

// At is the reading as it stands at now: a window whose reset has passed has
// turned over (ok, its use unknown), and one used from [LimitWarnAt] reads as
// a warning. The Models page and the LLM fleet balancer read windows by it.
func (r LimitReading) At(now time.Time) LimitReading {
	if !r.ResetsAt.IsZero() && !r.ResetsAt.After(now) {
		r.State, r.Used = LimitOK, nil
		return r
	}
	if r.State == LimitOK && r.Used != nil && *r.Used >= LimitWarnAt {
		r.State = LimitWarning
	}
	return r
}

// LimitedUntil reports whether err is a rate limit, and when the entry that
// refused it is tried again and when its vendor said the limit lifts (either
// zero where unknown): the question a caller that runs on a schedule of its
// own asks, so that a limit already reported is treated as the wait it is
// rather than as another failure.
func LimitedUntil(err error) (retryAt, resetsAt time.Time, ok bool) {
	var failure *Error
	if KindOf(err) != KindRateLimit || !errors.As(err, &failure) {
		return time.Time{}, time.Time{}, false
	}
	return failure.RetryAt, failure.ResetsAt, true
}

func (e *Error) Error() string {
	return fmt.Sprintf("llm %s/%s: %s: %v", e.Provider, e.Model, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ErrNotRunnable is a harness that could not be started on this host at all: its
// binary missing or not executable, or no process to give it. A backend wraps
// it under [KindFatal], so a caller telling a person why the model did not
// answer can say the model's program is missing rather than that it failed.
var ErrNotRunnable = errors.New("the model's harness could not be started on this host")

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
	if errors.Is(err, context.Canceled) {
		return KindCancelled
	}
	return KindFatal
}
