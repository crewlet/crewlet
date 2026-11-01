// Package chain is the seat's model fallback: an [llm.Provider] built from
// several, tried in order until one answers.
//
// The rule it exists to hold is the contract's, not its own. A backend
// classifies a failure and this decides what the classification means for the
// NEXT model: [llm.ErrorKind.Retryable] says whether another member is worth
// trying at all. A fatal is where the walk stops — a call the engine or its
// host cannot make, which is the operator's to fix rather than an outage for
// the next member to carry the seat through. A CLI that ran and failed is not
// one: its vendor's outage or its own crash is not the next member's, so the
// walk moves on.
//
// # The next member continues from the session's record
//
// Fallback is per CALL: one CLI run, which for a seat at work is a whole loop
// of model requests and tool calls. A member that fails partway through has
// already had calls run — the phase's tool host ran them, recorded them and
// charged what the run spent — and its failure undoes none of it. Starting the
// call from scratch on the next member would run them again: a second post, a
// second issue, written by a model that never saw the first.
//
// So the next member is handed the SAME request — the same conversation and
// the same host — and its backend shows its CLI what the host holds since the
// conversation was last folded ([llm.ToolHost.Pending]) as work already done,
// which the model reads rather than does again. What is dropped is only the
// failed member's unfinished prose: a streaming consumer is told the attempt
// restarted ([llm.Delta.Restart]) and sets aside the text it was showing,
// never the calls, which are on the record whoever made them.
//
// # The walk stops where the phase is decided
//
// A member whose run the host ENDED — the phase submitted, parked, ran out of
// rounds or was stopped, or the run was cut short for something the next run
// of the loop answers — fails the way any CLI that is stopped mid-run does. No
// other member continues that run: what happens next is the tool loop's, which
// holds the host and reads the outcome off its own record, and a second member
// would only redo or overrun a phase that is over. [llm.ToolHost.Halted]
// answers it, and the chain returns the member's error as it came.
//
// # A seat's executor falls back per launch
//
// A seat's EXECUTOR is not a call this walks ([Pick]): its run is the CLI's
// own, in a box, and hours long, so there is no call to continue on the next
// member once it started — a run that meets a limit midway is not relaunched
// elsewhere. What can still move is where the next run starts, so the launch
// asks each member whether it is ready in the same order and with the same
// vocabulary a call uses: a retryable refusal hands off, a fatal one stops the
// walk, and running out is the same [Error] a call reports.
package chain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

var log = logging.Get("llm.chain")

// Member is one entry in a chain.
type Member struct {
	// Key is the providers.llm key this entry was configured under. It is
	// what an operator recognises; the model id is what telemetry bills.
	Key string

	// Provider is the backend.
	Provider llm.Provider
}

// Fallback describes one hand-off, for the caller's event publishing.
type Fallback struct {
	// From is the key that just failed.
	From string

	// To is the key the call moves to — EMPTY on the last member, where
	// the fallback is to nothing. A literal "next" at both call sites
	// makes every ProviderFallback event record to_provider_key="next":
	// the one field that says which provider took over, naming no
	// provider at all.
	To string

	// Kind is the failure's classification.
	Kind llm.ErrorKind

	// Err is the failure.
	Err error
}

// Chain is an [llm.Provider] that walks its members.
type Chain struct {
	members []Member

	// onFallback is fired once per hand-off. Best effort: telemetry must
	// never fail a call, so a panicking hook is not this package's problem
	// but a blocking one is the caller's to avoid.
	onFallback func(Fallback)
}

var _ llm.Provider = (*Chain)(nil)

// Options configure a chain.
type Options struct {
	// OnFallback is invoked, in order, for every member that failed.
	OnFallback func(Fallback)
}

// New builds a chain.
//
// An empty chain is refused rather than tolerated: a seat wired to no model
// at all fails on its first turn with an error about a nil provider, which
// names neither the seat nor the config that produced it.
func New(members []Member, opts Options) (*Chain, error) {
	if len(members) == 0 {
		return nil, errors.New("chain: needs at least one member")
	}
	c := &Chain{members: make([]Member, len(members)), onFallback: opts.OnFallback}
	for i, m := range members {
		if m.Provider == nil {
			return nil, fmt.Errorf("chain: member %d (%q) has no provider", i, m.Key)
		}
		c.members[i] = m
	}
	return c, nil
}

// Model is the chain's CONFIGURED identity: its head member's model, which is
// the one a call reaches unless something goes wrong.
//
// It deliberately does NOT track which member last answered. It used to, as an
// atomic, and that was a workaround for a contract that asked a no-argument
// method to report a per-call fact — under concurrent use it named the wrong
// model 21 times in 200. [llm.Completion.Model] carries the per-call answer
// now, so this can be the stable, race-free thing an operator expects from a
// name.
func (c *Chain) Model() string {
	if len(c.members) == 0 {
		return ""
	}
	return c.members[0].Provider.Model()
}

// Keys is the chain's configured order, for diagnostics.
func (c *Chain) Keys() []string {
	out := make([]string, len(c.members))
	for i, m := range c.members {
		out[i] = m.Key
	}
	return out
}

// Complete walks the chain. The first member whose call does not fail
// retryably wins — and a member whose run the request's host ended is the
// last one tried, whatever it answered.
func (c *Chain) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if len(c.members) == 0 {
		// A `Chain{}` literal compiles anywhere — the fields are unexported,
		// which does not stop a composite literal — and walking no members
		// would report an exhausted chain whose last error is nil, a message
		// naming neither a provider nor a cause. New is the only way to build
		// a usable one, and this is what says so.
		return nil, &llm.Error{
			Kind: llm.KindFatal,
			Err:  errors.New("chain: no members; build one with chain.New"),
		}
	}

	// A member that streamed before it failed has already shown its text to
	// whoever is watching. The next member's fragments must not simply
	// continue it — two half-answers from two models read as one incoherent
	// paragraph — so a failover announces itself, and the consumer decides
	// what to do with the abandoned attempt. The TEXT is all a restart
	// abandons: the calls the member made are the host's record, which the
	// next member continues from.
	streamed := false
	inner := req
	if req.Streaming() {
		inner.OnDelta = func(d llm.Delta) {
			// TEXT is what counts as "something was shown". A member that
			// forwards a fragment carrying none — a keep-alive, a
			// bookkeeping event — must not make the next member announce a
			// restart of an answer nobody ever saw.
			if d.Content != "" {
				streamed = true
			}
			// Through Send, not straight to the callback: the empty-fragment
			// filter belongs to every hop, and calling OnDelta directly here
			// made the chain depend on each member having applied it first.
			req.Send(d)
		}
	}

	var last error
	failures := make([]error, 0, len(c.members))
	for i, m := range c.members {
		if streamed {
			req.Send(llm.Delta{Restart: true, Model: m.Provider.Model()})
			streamed = false
		}
		completion, err := m.Provider.Complete(ctx, inner)
		if err == nil {
			if completion.Model == "" {
				// A member that filled nothing in — a third-party
				// Provider, a test double — still has to be billable.
				// An empty model on a completion files this call's
				// tokens under no model at all.
				completion.Model = m.Provider.Model()
			}
			if completion.ProviderKey == "" {
				// WHICH ENTRY answered, which only this loop knows:
				// the backend was built from the entry and never told
				// its key, and every frame above sees the chain as one
				// provider. Without it a call that fell through to the
				// second member reads as the head's — the entry the
				// spend-by-provider rollup would then charge for a call
				// it failed.
				completion.ProviderKey = m.Key
			}
			return completion, nil
		}

		kind := llm.KindOf(err)
		if !kind.Retryable() || ctx.Err() != nil {
			// Returned as it came, so errors.As still reaches the
			// backend's own *llm.Error with its provider and model
			// intact.
			//
			// A CALL WHOSE CALLER IS GONE IS NOT WALKED, whatever its
			// member called the failure: the next member would be
			// started on a context that has already ended, and a
			// cancellation logged as a fallback reads as an outage of a
			// member that did nothing wrong.
			return nil, err
		}
		if req.Tools != nil && req.Tools.Halted() {
			// THE HOST ENDED THIS RUN, and a CLI stopped mid-run fails
			// like one that broke: no member continues a phase that is
			// decided — see the package doc. Not a hand-off, so nothing
			// is announced as one.
			return nil, err
		}

		next := ""
		if i+1 < len(c.members) {
			next = c.members[i+1].Key
		}
		log.WarnContext(ctx, "provider_fallback",
			"from", m.Key, "to", next, "kind", kind.String(), "error", err.Error())
		if c.onFallback != nil {
			c.onFallback(Fallback{From: m.Key, To: next, Kind: kind, Err: err})
		}
		last = err
		failures = append(failures, err)
	}

	return nil, &Error{Attempted: c.Keys(), Failures: failures, Err: last}
}

// Pick is the executor's fallback at launch: the first of members that ready
// accepts, walked in order.
//
// ready answers for one member at the moment of the launch — nil when it can
// take the run, a classified error when it cannot. A RETRYABLE refusal (an
// entry sitting out its spent plan) hands off exactly as [Chain.Complete]
// does: it is logged as provider_fallback, reported to onFallback naming the
// member that takes over — or none, on the last — and the walk moves on. A
// fatal refusal is returned as it came, for the reason a fatal call stops the
// walk, and so is any refusal once ctx is done. Every member refusing
// retryably is an [*Error] naming them all, WITH EACH ONE'S REFUSAL
// ([Error.Failures]), so the caller reads an exhausted launch exactly as an
// exhausted call: the same unavailability the seat reports either way, and,
// where every member sits out a limit, the same [Limited] chain a seat is
// parked on until the first is tried again. Without them a launch every
// member of which sat out read as a chain that had failed, and every turn
// launches, so the seat's mail was taken back into the limit until the broker
// dead-lettered it.
//
// The caller filters members that can never take a launch (a CLI with no
// in-box runner) BEFORE calling: that is a fact of the configuration, not a
// refusal of the moment, and announcing it as a hand-off on every launch
// would publish a fallback nothing fell from.
func Pick(ctx context.Context, members []Member, ready func(Member) error,
	onFallback func(Fallback)) (Member, error) {
	if len(members) == 0 {
		return Member{}, &llm.Error{
			Kind: llm.KindFatal,
			Err:  errors.New("chain: no member can take the launch"),
		}
	}
	var (
		last     error
		failures []error
	)
	for i, m := range members {
		err := ready(m)
		if err == nil {
			return m, nil
		}
		kind := llm.KindOf(err)
		// A caller that is gone stops the walk as it stops a call's: no
		// member is asked on behalf of a launch nobody is waiting for.
		if !kind.Retryable() || ctx.Err() != nil {
			return Member{}, err
		}
		next := ""
		if i+1 < len(members) {
			next = members[i+1].Key
		}
		log.WarnContext(ctx, "provider_fallback",
			"from", m.Key, "to", next, "kind", kind.String(), "error", err.Error())
		if onFallback != nil {
			onFallback(Fallback{From: m.Key, To: next, Kind: kind, Err: err})
		}
		last = err
		failures = append(failures, err)
	}
	attempted := make([]string, len(members))
	for i, m := range members {
		attempted[i] = m.Key
	}
	return Member{}, &Error{Attempted: attempted, Failures: failures, Err: last}
}

// Error reports that every member failed retryably.
//
// It carries no kind of its own: the last member's classified error is right
// there under Unwrap, so [llm.KindOf] answers for a chain exactly as it does
// for a backend — which is also what lets a chain be a member of a chain.
type Error struct {
	// Attempted is every key that was tried, in order.
	Attempted []string

	// Failures is every member's failure, in the order Attempted names
	// them: what [Limited] reads to tell a chain every member of which is
	// sat out from one whose last member happened to be.
	Failures []error

	// Err is the last member's failure.
	Err error
}

// Limit is a chain every member of which refused for a rate limit: the keys,
// when the first of them is tried again, and when the vendors said the limits
// lift.
type Limit struct {
	// Keys are the members, in the chain's order.
	Keys []string
	// RetryAt is the earliest instant any member is tried again: before it,
	// a call through the chain is refused by every member without one of
	// them being asked.
	RetryAt time.Time
	// ResetsAt is the earliest instant a member's vendor said its limit
	// lifts, where every member named one; zero where any did not, since a
	// member that named none may lift sooner than the named ones, and a
	// person told "4:30 PM" would wait for nothing.
	ResetsAt time.Time
	// Said is the head member's own sentence, for whoever is shown why.
	Said string
	// Agent and Subscription are the head member's CLI and whether it signs
	// in on a plan ([llm.Error.Agent]): what the limit is called.
	Agent        string
	Subscription bool
}

// Limited reports whether err is a chain whose every member refused for a rate
// limit with an instant it is tried again, and what that limit is.
//
// EVERY MEMBER, and not the last one: a chain whose head is spent and whose
// fallback failed for a reason nothing says will last (a server error, a
// timeout) is worth trying again on the ordinary retry, and treating it as
// spent would hold a seat for an hour over a blip of its fallback's. A member
// that names no instant it is tried again is not one this can wait out either.
func Limited(err error) (Limit, bool) {
	var exhausted *Error
	if !errors.As(err, &exhausted) || len(exhausted.Failures) == 0 ||
		len(exhausted.Failures) != len(exhausted.Attempted) {
		return Limit{}, false
	}
	limit := Limit{Keys: slices.Clone(exhausted.Attempted)}
	unnamed := false
	for i, failure := range exhausted.Failures {
		retryAt, resetsAt, ok := llm.LimitedUntil(failure)
		if !ok || retryAt.IsZero() {
			return Limit{}, false
		}
		if limit.RetryAt.IsZero() || retryAt.Before(limit.RetryAt) {
			limit.RetryAt = retryAt
		}
		switch {
		case resetsAt.IsZero():
			unnamed = true
		case limit.ResetsAt.IsZero() || resetsAt.Before(limit.ResetsAt):
			limit.ResetsAt = resetsAt
		}
		if i == 0 {
			limit.Said = saidOf(failure)
			var head *llm.Error
			if errors.As(failure, &head) {
				limit.Agent, limit.Subscription = head.Agent, head.Subscription
			}
		}
	}
	if unnamed {
		limit.ResetsAt = time.Time{}
	}
	return limit, true
}

// saidOf is a member's failure as its backend worded it, without the frame
// [llm.Error.Error] adds.
func saidOf(err error) string {
	var failure *llm.Error
	if errors.As(err, &failure) && failure.Err != nil {
		return failure.Err.Error()
	}
	return err.Error()
}

func (e *Error) Error() string {
	return fmt.Sprintf("llm chain exhausted after %d providers (%s): %v",
		len(e.Attempted), strings.Join(e.Attempted, " -> "), e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// unready is a member that can say, without a call, that its next call would
// fail (cliagent's Provider).
type unready interface {
	Unready(ctx context.Context) error
}

// Unready is the failure a call through these members would meet as far as
// they know without a call, or nil when any of them may answer: a member that
// cannot say (a test double, a backend with no memory) may, so it answers nil.
//
// THE CHAIN FAILS ONLY WHEN EVERY MEMBER WOULD, and its failure is the one
// that clears first: a spent plan, which ends at a known reset (the earliest
// of them, an unnamed reset after every named one), over a login somebody has
// to mend, over a CLI that cannot start.
func Unready(ctx context.Context, members []llm.Provider) error {
	if len(members) == 0 {
		return nil
	}
	failures := make([]error, 0, len(members))
	for _, m := range members {
		u, ok := m.(unready)
		if !ok {
			return nil
		}
		err := u.Unready(ctx)
		if err == nil {
			return nil
		}
		failures = append(failures, err)
	}
	slices.SortStableFunc(failures, func(a, b error) int {
		if c := cmp.Compare(clearsAt(a), clearsAt(b)); c != 0 {
			return c
		}
		ra, rb := retryAt(a), retryAt(b)
		if ra.IsZero() != rb.IsZero() {
			return cmp.Compare(boolRank(ra.IsZero()), boolRank(rb.IsZero()))
		}
		return ra.Compare(rb)
	})
	return failures[0]
}

// KnownLimit is the limit a call through members would meet as far as they
// already know without a call ([Unready]), read as [Limited] reads a call's
// failure: every member refusing for a rate limit with an instant it is tried
// again. False where any member may answer or cannot say, and where any refuses
// for another reason, which no park waits out.
//
// THE LIMIT, NOT ONLY ITS INSTANT, because a caller with no call of its own to
// fail parks on it: an executor's completion, collected while its review's
// chain sits out the plan its run spent, parks its seat on the same limit a
// call through that chain would have met, keys, reset and sentence alike,
// without a process being started to hear it.
func KnownLimit(ctx context.Context, members []Member) (Limit, bool) {
	if len(members) == 0 {
		return Limit{}, false
	}
	keys := make([]string, 0, len(members))
	failures := make([]error, 0, len(members))
	for _, m := range members {
		u, ok := m.Provider.(unready)
		if !ok {
			return Limit{}, false
		}
		err := u.Unready(ctx)
		if err == nil {
			return Limit{}, false
		}
		keys = append(keys, m.Key)
		failures = append(failures, err)
	}
	return Limited(&Error{Attempted: keys, Failures: failures, Err: failures[len(failures)-1]})
}

// clearsAt ranks a failure by how soon it clears without anybody acting: a
// spent plan first, then a login, then anything else.
func clearsAt(err error) int {
	switch llm.KindOf(err) {
	case llm.KindRateLimit:
		return 0
	case llm.KindAuth:
		return 1
	default:
		return 2
	}
}

// retryAt is a failure's reset instant, zero where it names none.
func retryAt(err error) time.Time {
	var failure *llm.Error
	if errors.As(err, &failure) {
		return failure.RetryAt
	}
	return time.Time{}
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
