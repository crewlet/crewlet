package cliagent

import (
	"context"
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
// at the reset instant the vendor named ([cliprofile.ResetReading]), never
// later than [maxLimitBench]. Where the vendor named none it grows through
// [limitBackoff], one step per report that finds the window already over, and
// starts again from the first once a call through the entry answers: a limit
// that lifts in a minute costs a minute, and one that lasts all afternoon
// costs four process launches an hour per entry rather than one per round of
// every seat. Short at first on purpose: sitting out a plan that has already
// reset costs the fallback's spend, or the seats' whole afternoon where there
// is no fallback, for every minute of it.
//
// A LOGIN IS NOT BENCHED. `crewlet llm login` mends it — usually from another
// process this one hears nothing from — and the very next call must find it,
// where a window would hold a signed-in seat on its fallback for nothing; and
// finding out that a login is still missing costs a CLI that exits at once
// rather than a model call.

// limitBackoff is how long an entry sits out after a limit report that named
// no reset instant: the first step for the first report, and each later step
// for a report that finds the previous window already over, so a probe that
// meets the same limit waits longer before the next. Capped at the last step,
// fifteen minutes, which keeps a limit that has lifted from costing more than a
// quarter of an hour, and a limit that has not from costing more than four
// probes an hour.
var limitBackoff = [...]time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// maxLimitBench caps a reset instant the CLI named: an hour, so an operator
// who signs in again or raises the plan has the entry back within the hour
// even when the vendor's window runs for days.
const maxLimitBench = time.Hour

// bench is the entry's sit-out window and the report behind it, guarded for
// concurrent calls.
type bench struct {
	mu    sync.Mutex
	until time.Time
	// said is the CLI's own sentence that started it, repeated on every
	// call it refuses so the reason never reads as the engine's.
	said string
	// resetsAt is when the vendor said the limit lifts, zero where it named
	// no instant.
	resetsAt time.Time
	// unnamed counts the windows sat out for reports that named no reset
	// since the entry last answered: the step of [limitBackoff] the next
	// one takes.
	unnamed int
}

// bench sits the entry out until reset (the instant the vendor named, or a
// step of [limitBackoff] when it named none), capped at [maxLimitBench], and
// answers when the entry is tried again. A later bench never SHORTENS an
// earlier one: two seats' calls that both met the spent plan report the same
// window, and the one that lands last must not cut it. For the same reason a
// report that lands while a window is still in force does not step the
// backoff: only a probe made after the window ended, and refused again, does.
func (p *Provider) bench(reset time.Time, said string) time.Time {
	now := p.now()
	p.sitOut.mu.Lock()
	defer p.sitOut.mu.Unlock()
	b := &p.sitOut
	var until time.Time
	if reset.IsZero() {
		if now.Before(b.until) {
			// Inside a window already in force: the same refusal, heard
			// by another call, which neither extends it nor steps on.
			return b.until
		}
		until = now.Add(limitBackoff[min(b.unnamed, len(limitBackoff)-1)])
		b.unnamed++
	} else {
		until = now.Add(min(reset.Sub(now), maxLimitBench))
	}
	if until.After(b.until) {
		b.until, b.said, b.resetsAt = until, said, reset
	}
	return b.until
}

// answered ends the report behind any window: a call through the entry was
// answered, so whatever limit it reported has lifted, and a later report that
// names no reset starts the backoff from its first step again.
func (p *Provider) answered() {
	p.sitOut.mu.Lock()
	defer p.sitOut.mu.Unlock()
	p.sitOut.resetsAt, p.sitOut.unnamed = time.Time{}, 0
}

// benched is the classified refusal of a call made while the entry sits out,
// or nil when it does not.
func (p *Provider) benched() error {
	p.sitOut.mu.Lock()
	until, said, resets := p.sitOut.until, p.sitOut.said, p.sitOut.resetsAt
	p.sitOut.mu.Unlock()
	if !p.now().Before(until) {
		return nil
	}
	return p.limited(fmt.Errorf("%s, sitting out until %s, when this entry is tried again",
		said, until.UTC().Format(time.RFC3339)), until, resets)
}

// classified turns a marker's hit into the call's failure: a limit sits the
// entry out and carries when it is tried again, and anything else is the kind
// the marker named.
func (p *Provider) classified(ctx context.Context, hit markerHit, reason string) error {
	if hit.Kind != llm.KindRateLimit {
		return p.fail(hit.Kind, fmt.Errorf("%s", reason))
	}
	until := p.bench(hit.Reset, reason)
	// WHERE IT MATCHED, beside what it matched: a sit-out takes the entry
	// from every seat on this node, and the one question an operator who
	// doubts it has is whether the vendor said it.
	resets := "unknown"
	if !hit.Reset.IsZero() {
		resets = hit.Reset.UTC().Format(time.RFC3339)
	}
	log.WarnContext(ctx, "cli_agent_limit_reported", "provider", p.key, "agent", p.agent,
		"source", string(hit.Source), "said", reason,
		"retry_at", until.UTC().Format(time.RFC3339), "resets_at", resets)
	return p.limited(fmt.Errorf("%s", reason), until, hit.Reset)
}

// limited is a classified rate limit carrying when the entry is tried again
// and when the vendor said the limit lifts, so a caller telling a person to
// wait can name the instant without reading it out of the vendor's sentence.
func (p *Provider) limited(err error, until, resets time.Time) error {
	failure := p.failure(llm.KindRateLimit, err)
	failure.RetryAt = until
	failure.ResetsAt = resets
	return failure
}
