package llm

import (
	"context"
	"time"
)

// ToolHost is the engine's side of a call that offers tools: the phase's live
// tool surface, which the backend serves to its harness as ONE MCP server for the
// length of the call, and the place the backend reports what the harness did.
//
// Declared here, by the contract, and implemented by the tool loop
// (internal/agent/toolloop), which is the frame that holds the phase: every
// gate a call is subject to — the seat's grant, the skill guard, the fence, the
// budget, the round cap, the terminator — is decided behind it. A backend only
// connects its harness to it, tells it what the harness's own record shows, and stops
// the harness when it asks.
//
// # Why the backend reports what the harness did
//
// The host sees every call the harness makes to it, and nothing else: not the
// model's requests between them, not their prose, not what each one cost.
// Those are in the harness's own record — its stream, or a session file it writes
// as it runs — which only the backend reads, so the backend hands them over
// as it reads them ([ToolHost.Observe], [ToolHost.Spent]). A harness whose record
// shows no requests is reported on once, at the end, as one.
type ToolHost interface {
	// Serve opens the host to ONE HARNESS run: the stdio server the harness is to
	// start as its MCP server, and what to call when the run is over.
	Serve(ctx context.Context, opts ServeOptions) (Served, error)

	// Observe reports one model request of the run, as the harness's own record
	// shows it, in order. A request may be reported again as more of it is
	// read (its calls, its usage): the host keys it by [Turn.ID].
	Observe(Turn)

	// Spent reports tokens the run has spent, as the backend learns them:
	// each request's usage where the harness's record carries it while the run
	// goes, and — when the run is over, on EVERY outcome, a failure, a
	// timeout or a run the host ended included — whatever it spent that was
	// not already reported. The sum of what one run reports is its cost.
	Spent(Usage)

	// Done is closed when the host needs the run to END — the phase
	// finished, parked, stopped, ran out of rounds, or is pausing for a
	// person's note. The backend stops its harness.
	Done() <-chan struct{}

	// Halted reports a state no other member of a fallback chain may
	// continue: the phase finished, parked or was stopped. A chain asks it
	// before trying its next member.
	Halted() bool

	// Pending is what the phase did since its caller last read it — the
	// requests the host was told of and the calls it ran, as assistant and
	// tool messages — for a backend that continues a call another member of
	// a chain began, so the work it already did is shown rather than done
	// again.
	Pending() []Message
}

// ServeOptions is what a backend tells the host about the harness it is about to
// run, because the host decides with it.
type ServeOptions struct {
	// Relists says the harness re-reads the tool list when the server says it
	// changed, mid-run. One that does not never sees a tool activated during
	// the run, so the host ends the run after an activation and the next
	// one starts with the new list.
	Relists bool

	// ToolTimeout is how long the harness waits for one call before it gives up
	// on it and tells the model it failed. A call still running near it is
	// answered by the host first (the run ends, and the next one is handed
	// the call's result), because a harness that gives up on a call the engine
	// is still running sends the model to do it again.
	ToolTimeout time.Duration

	// RecordsCallsLate says the harness's record shows a response's calls only
	// once they have run (Gemini CLI, OpenCode): a call always reaches the
	// host before the record of the response that asked for it. The host
	// places such a call at once and joins it to its response when the
	// record arrives, rather than holding the call for a record that cannot
	// come until the call is answered.
	RecordsCallsLate bool

	// CallIDMeta is the key of a call's `_meta` the harness hands the server
	// its own id for the call under — the id its record names the call by
	// ([TurnCall.ID]), which is how the host joins a call to the request
	// that asked for it. Empty: the harness sends none, and a call is joined by
	// its name and arguments. Read under this key alone, so a harness that puts
	// something else in `_meta` is never read as naming a call.
	CallIDMeta string

	// ObservesRequests says the backend will report the run's model
	// requests ([ToolHost.Observe]) as they happen. Without it the host
	// counts the whole run as one request.
	ObservesRequests bool

	// Concurrency is which of one response's calls the harness makes without
	// waiting for the one before it to be answered. The host holds a call
	// that must run alone — one that ends or parks the phase — until the
	// response's other calls have run, and it waits only for calls the harness
	// could be making beside it: a harness that makes the next call once this
	// one is answered sends none while it is held.
	Concurrency CallConcurrency

	// OnCall is told when the first call of a stretch starts and when the
	// last one in flight ends — busy true, then false — so a backend can
	// stop the clock that bounds the model's own working time while the
	// engine's tools run, and look at the harness's login while it waits.
	OnCall func(busy bool)
}

// CallConcurrency is which of one model response's calls a harness makes without
// waiting for the one before it to be answered — a fact about the harness's own
// loop, measured per harness and declared by its profile.
type CallConcurrency string

const (
	// CallsAtOnce is a harness that may make any of a response's calls beside
	// another. The zero value, because it is the safe assumption about a
	// harness that does not say: a host that waits for a call that never comes
	// waits a moment, and one that runs a call that must run alone before a
	// call beside it arrives runs the two in the wrong order.
	CallsAtOnce CallConcurrency = ""

	// CallsReadOnlyAtOnce is a harness that makes calls to tools annotated
	// read-only beside each other and makes any other call alone, once the
	// call before it is answered.
	CallsReadOnlyAtOnce CallConcurrency = "read_only"

	// CallsInTurn is a harness that makes one call at a time, each once the last
	// is answered.
	CallsInTurn CallConcurrency = "in_turn"
)

// Valid reports whether c is one of the three.
func (c CallConcurrency) Valid() bool {
	return c == CallsAtOnce || c == CallsReadOnlyAtOnce || c == CallsInTurn
}

// Served is one harness run's connection to the host.
type Served struct {
	// Command and Args are the stdio server the harness is to start.
	Command string
	Args    []string

	// Release ends the run's connection — no new call, every call running
	// on it returned — and answers whether the harness ever reached the tools
	// (listed them). A run that never did had no tools, which is the harness
	// or its configuration failing rather than a model that called none.
	Release func() (connected bool)
}

// Turn is one model request of a harness run, as the harness's own record shows it.
type Turn struct {
	// ID is the harness's own id for the request, which the host keys it by.
	ID string

	// Text is the visible prose of the response.
	Text string

	// Calls are the calls the response asked for, in order — the host's
	// tools and the harness's own (its web search, its fetch) alike.
	Calls []TurnCall

	// Model is the model that answered, where the record names one.
	Model string

	// Stop is why the response ended, where the record says.
	Stop StopReason

	// Usage is what the request cost, where the record carries it per
	// request; nil where it does not. Reported through [ToolHost.Spent] as
	// well — this is the record's copy, for the round.
	Usage *Usage
}

// TurnCall is one call a model request asked for.
type TurnCall struct {
	// ID is the harness's id for the call. Some harnesses hand it to the server with
	// the call, which is how a call the host ran is joined to the request
	// that asked for it.
	ID string

	// Name is the tool the model named. For one of the host's tools it is
	// the host's own name for it, whatever prefix the harness showed the model
	// (`mcp__crewlet__…`, `crewlet_…`) — the backend knows its harness's
	// spelling, and the host matches on the name it offered.
	Name      string
	Arguments map[string]any

	// ArgumentsError is why the arguments the model wrote, as the harness's
	// record shows them, are not an object — empty when they are, or when
	// the record shows none. A harness may hand its server something else for
	// arguments that are not an object (Claude Code hands it an array's
	// elements as one: `["a","b"]` arrives as `{"0":"a","1":"b"}`), and the
	// record is the one place the engine can see that the model never wrote
	// what the server received: the host answers such a call as one whose
	// arguments did not parse rather than running it.
	ArgumentsError string

	// Host marks a call to one of the host's tools. Every other call is the
	// harness's own — its web search, its fetch — which it runs itself and the
	// host never sees.
	Host bool
}

// Usage is tokens spent, on the [Completion] rule: Input is the WHOLE prompt,
// cache included, and CacheRead and CacheWrite break it down.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int

	// Estimated marks a count the backend worked out rather than read,
	// for a harness that reports none.
	Estimated bool
}

// Total is the figure a budget is charged.
func (u Usage) Total() int { return u.Input + u.Output }
