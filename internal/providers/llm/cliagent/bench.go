package cliagent

import (
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// An entry whose CLI reported a spent plan or a rate limit SITS OUT for a
// while, on this node: every call in that window fails at once as a rate
// limit, so the seat's chain moves to its next model without launching a
// process to hear the same sentence again — a call that arrives in it, and a
// call that was already queued for a slot when it began, which is refused
// once it holds the slot ([Provider.Complete]).
//
// # Why the backend benches itself
//
// A spent subscription window lasts hours, and a seat makes a call per round.
// Unbenched, every round of every seat on the entry starts a CLI — seconds of
// a Node or Rust runtime holding one of the entry's few slots — only to be told
// the plan is spent and fall through to the next model anyway. The entry holds
// ONE login, so there is one thing to bench, and it is benched here rather than
// in the chain because only the backend knows the reset instant its CLI named.
//
// # Only on the vendor's word
//
// A sit-out takes a working entry from every seat on the node, so what starts
// one has to be the vendor's report and never the model's sentence. The
// classification is what guarantees it, not this file: a sentinel counts on
// stderr, in a run the CLI reported failed, in the CLI's own failure event, or
// where it OPENS what a clean run said — the vendor's sentence standing where
// the answer should be — and nowhere else (classifyMarkers). A seat writing
// about the GitHub rate limit, or a tool call carrying "Usage limit reached"
// in its arguments, is an answer, so every rate limit that reaches
// [Provider.bench] is evidence from the CLI. That rule, and not the wording of
// a sentinel, is what makes the shipped profiles' generic, unmeasured
// sentinels — Copilot's "rate limit", Cursor's "quota" — safe to bench on.
//
// # On this node, and for not too long
//
// The bench is the provider's own and is not shared with the fleet: every
// other node learns the same thing from its own calls, once per window. It ends
// at the reset instant the CLI named ([cliprofile.LimitMarker]) where its
// profile can read one, and after [limitBench] where it cannot — which is
// every shipped profile — and never later than [maxLimitBench]. Short on
// purpose: a probe costs at most the entry's max_concurrent process launches
// — the calls that take a slot once the window has ended, before the first of
// them reports; every call queued behind them is refused once that report
// lands — while sitting out a plan that has already reset costs the
// fallback's spend for every minute of it.
//
// A LOGIN IS NOT BENCHED. `crewlet llm login` mends it — usually from another
// process this one hears nothing from — and the very next call must find it,
// where a window would hold a signed-in seat on its fallback for nothing; and
// finding out that a login is still missing costs a CLI that exits at once
// rather than a model call.

// limitBench is how long an entry sits out after its CLI reported a spent plan
// or a rate limit with no reset instant: one minute, so a window that turns
// over is noticed within a minute — the fallback's spend for at most that
// long — while one that has not costs at most max_concurrent process launches
// a minute per entry on this node rather than one per round of every seat.
const limitBench = time.Minute

// maxLimitBench caps a reset instant the CLI named: an hour, so an operator
// who signs in again or raises the plan has the entry back within the hour
// even when the vendor's window runs for days.
const maxLimitBench = time.Hour

// bench is the entry's sit-out window, guarded for concurrent calls.
type bench struct {
	mu    sync.Mutex
	until time.Time
	// said is the CLI's own sentence that started it, repeated on every
	// call it refuses so the reason never reads as the engine's.
	said string
}

// bench sits the entry out for retry — the CLI's own reset, or [limitBench]
// when it named none — capped at [maxLimitBench]. A later bench never SHORTENS
// an earlier one: two seats' calls that both met the spent plan report the
// same window, and the one that lands last must not cut it.
func (p *Provider) bench(retry time.Duration, said string) {
	if retry <= 0 {
		retry = limitBench
	}
	until := p.now().Add(min(retry, maxLimitBench))
	p.sitOut.mu.Lock()
	defer p.sitOut.mu.Unlock()
	if until.After(p.sitOut.until) {
		p.sitOut.until, p.sitOut.said = until, said
	}
}

// benched is the classified refusal of a call made while the entry sits out,
// or nil when it does not.
func (p *Provider) benched() error {
	p.sitOut.mu.Lock()
	until, said := p.sitOut.until, p.sitOut.said
	p.sitOut.mu.Unlock()
	if !p.now().Before(until) {
		return nil
	}
	return p.fail(llm.KindRateLimit, fmt.Errorf(
		"%s — sitting out until %s, when this entry is tried again",
		said, until.UTC().Format(time.RFC3339)))
}
