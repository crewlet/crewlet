// Package chain is the seat's model fallback: an [llm.Provider] built from
// several, tried in order until one answers.
//
// The rule it exists to hold is the contract's, not its own. A backend
// classifies a failure and this decides what the classification means for the
// NEXT model: [llm.ErrorKind.Retryable] says whether another member is worth
// trying at all, and a fatal is where the walk stops — a malformed request or
// a content refusal will be refused identically by every member, so trying
// them is two more seconds and another log line saying the same thing.
//
// Fallback is per CALL, not per round. A member that fails is dropped along
// with whatever it produced, and the next one starts the call from scratch,
// which keeps one telemetry span per attempt instead of a conversation
// stitched from two models.
package chain

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// retryably wins.
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

	var last error
	for i, m := range c.members {
		completion, err := m.Provider.Complete(ctx, req)
		if err == nil {
			if completion.Model == "" {
				// A member that filled nothing in — a third-party
				// Provider, a test double — still has to be billable.
				// An empty model on a completion files this call's
				// tokens under no model at all.
				completion.Model = m.Provider.Model()
			}
			return completion, nil
		}

		kind := llm.KindOf(err)
		if !kind.Retryable() {
			// Returned as it came, so errors.As still reaches the
			// backend's own *llm.Error with its provider, model and
			// status intact.
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
	}

	return nil, &Error{Attempted: c.Keys(), Err: last}
}

// Error reports that every member failed retryably.
//
// It carries no kind of its own: the last member's classified error is right
// there under Unwrap, so [llm.KindOf] answers for a chain exactly as it does
// for a backend — which is also what lets a chain be a member of a chain.
type Error struct {
	// Attempted is every key that was tried, in order.
	Attempted []string

	// Err is the last member's failure.
	Err error
}

func (e *Error) Error() string {
	return fmt.Sprintf("llm chain exhausted after %d providers (%s): %v",
		len(e.Attempted), strings.Join(e.Attempted, " -> "), e.Err)
}

func (e *Error) Unwrap() error { return e.Err }
