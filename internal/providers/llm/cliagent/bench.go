package cliagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// An entry whose CLI reported a spent plan or a rate limit SITS OUT for a
// while, on this node: every call in that window fails at once as a rate
// limit, so the seat's chain moves to its next model without launching a
// process to hear the same sentence again — a call that arrives in it, and a
// call that was already queued for a slot when it began, which is refused
// once it holds the slot ([Provider.Complete]).
//
// # Two readers
//
// A call through [Provider.Complete] reads the sit-out as it arrives and again
// once it holds a slot.
// A seat's EXECUTOR reads it at launch ([Provider.SittingOut]): the run is the
// CLI's own, started in a box rather than through [Provider.Complete], so the
// engine asks before it launches and takes the next member of the seat's
// executor chain that can run in a box when this one sits out. A run that
// meets the limit after it started is not relaunched on another member — its
// work so far lives in its own box and session — but how it ended is read by
// the same classification every call takes, the CLI's structured limit report
// first, when it is collected ([Provider.RunEnded]), so the turn's NEXT
// launch, and every other seat's on this node, skips the spent entry as a call
// on this host would.
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

// SittingOut is the classified refusal of a call made while the entry sits
// out — an [llm.KindRateLimit] naming the CLI's own sentence and the instant
// the entry is tried again — or nil when it does not.
//
// Exported for the executor's launch, which is not a [Provider.Complete] and
// so has to ask: an error here is what makes the launch skip this member for
// the next in the chain, classified exactly as a call's refusal is so
// the hand-off reads the same in either place.
func (p *Provider) SittingOut() error {
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

// RunEnd is how one of an entry's EXECUTOR runs ended, as the in-box runner
// collected it ([Provider.RunEnded]).
type RunEnd struct {
	// Answer is what the run said last, Error the error it reported, and
	// Failed whether the CLI said the run failed.
	Answer, Error string
	Failed        bool

	// Event is the CLI's own terminal event, verbatim, where it prints one
	// (Claude Code's result line): how the vendor said the run ended in
	// fields — a status code — beside the words of Answer and Error.
	Event string

	// At is when the run's ending was first read back: what a reset the
	// vendor names as a time of day, or as a delay, is read against. Zero
	// reads it against now.
	At time.Time
}

// RunEnded reads how one of this entry's EXECUTOR runs ended by the one
// classification every call takes, the CLI's structured limit report first
// ([cliprofile.Profile.LimitEvents]), and sits the entry out exactly as a call
// on this host that met the same report would. It reports whether it did.
//
// THE OTHER HALF OF THE SECOND READER. A launch skips a member that sits out
// ([Provider.SittingOut]), but an executor's run never goes through
// [Provider.Complete], so a run that spent the plan benched nothing: the next
// launch on the entry — a reviewer sending the turn back an hour into the same
// window — walked straight into the same limit while a call on any other
// seat would already have moved on. The resuming node calls this before the
// turn launches again, and that node is the one whose launch must see it.
//
// ON THE VENDOR'S WORD ALONE (classifyMarkers): the run's terminal event where
// the profile reads a limit off one — Claude Code says a spent plan as
// `api_error_status` 429 on its result line, in a sentence ("You've hit your
// 5-hour limit") that is deliberately no sentinel — then the sentinels, by the
// rules a call's output is held to: the answer of a run that did not fail
// counts only where a sentinel opens it, its error anywhere, and for a CLI
// whose failures are typed events ([cliprofile.MarkerScopeEvents]) the error
// the runner collected is that event's own payload, read at the profile's
// marker path as the event it came in.
//
// A RUN THAT SUCCEEDED ON NO LIMIT IS AN ANSWER, as a call's is
// ([Provider.answered]): the entry's backoff starts again from its first step.
func (p *Provider) RunEnded(ctx context.Context, end RunEnd) bool {
	at := end.At
	if at.IsZero() {
		at = p.now()
	}
	out := cliOutput{said: nonEmpty(end.Answer, end.Error), stderr: end.Error,
		failed: end.Failed, at: at}
	var lines []string
	if event := strings.TrimSpace(end.Event); event != "" {
		lines = append(lines, event)
	}
	if p.profile.EffectiveMarkerScope() == cliprofile.MarkerScopeEvents {
		if event := failureEvent(p.profile, end.Error); event != "" {
			lines = append(lines, event)
		}
	}
	out.stdout = strings.Join(lines, "\n")
	hit, ok := classifyMarkers(p.profile, out)
	if !ok || hit.Kind != llm.KindRateLimit {
		if !end.Failed {
			p.answered()
		}
		return false
	}
	said := nonEmpty(hit.Said, firstLine(out.said))
	until := p.bench(hit.Reset, said)
	resets := "unknown"
	if !hit.Reset.IsZero() {
		resets = hit.Reset.UTC().Format(time.RFC3339)
	}
	log.WarnContext(ctx, "cli_agent_run_limit_reported", "provider", p.key, "agent", p.agent,
		"source", string(hit.Source), "said", said,
		"retry_at", until.UTC().Format(time.RFC3339), "resets_at", resets)
	return true
}

// failureEvent is the failure event a run's collected error came out of, as one
// line of its CLI's event stream: the payload put back where the profile's
// marker path reads it, under the first of its failure event types. Empty when
// the error is not an object — no payload the CLI typed — or the profile names
// no event to put it in.
func failureEvent(p cliprofile.Profile, errText string) string {
	if len(p.MarkerEvents) == 0 || len(p.EventTypePath) == 0 || len(p.MarkerPath) < 2 {
		return ""
	}
	payload, ok := decodeObject(strings.TrimSpace(errText))
	if !ok {
		return ""
	}
	event := map[string]any{}
	if !setPath(event, p.EventTypePath, p.MarkerEvents[0]) ||
		!setPath(event, p.MarkerPath[:len(p.MarkerPath)-1], payload) {
		return ""
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return ""
	}
	return string(raw)
}

// setPath places v at path in doc, building the objects on the way; false
// where a step of the path is already something other than an object.
func setPath(doc map[string]any, path cliprofile.Path, v any) bool {
	node := doc
	for i, step := range path {
		if i == len(path)-1 {
			node[step] = v
			return true
		}
		next, ok := node[step].(map[string]any)
		if !ok {
			if _, taken := node[step]; taken {
				return false
			}
			next = map[string]any{}
			node[step] = next
		}
		node = next
	}
	return false
}

// CarrySitOut takes over prev's sit-out, for an entry an apply rebuilt with an
// unchanged spec: the same login, so the same spent plan.
//
// A REBUILT PROVIDER IS NOT A NEW PLAN. Every apply builds every entry afresh,
// and a fresh provider sits nothing out — so the first apply after an entry
// met its limit, an edit to an unrelated seat, handed every seat on the node
// the spent entry back, and each of their next calls and launches spent a
// process learning the window again. An entry whose spec CHANGED is a new
// question — another login, another model — and starts clean.
func (p *Provider) CarrySitOut(prev *Provider) {
	if prev == nil || prev == p {
		return
	}
	prev.sitOut.mu.Lock()
	until, said := prev.sitOut.until, prev.sitOut.said
	resets, unnamed := prev.sitOut.resetsAt, prev.sitOut.unnamed
	prev.sitOut.mu.Unlock()
	p.sitOut.mu.Lock()
	defer p.sitOut.mu.Unlock()
	// THE WHOLE REPORT, not only its window: the instant the vendor said the
	// limit lifts, which a refusal names to a person ("resumes at 4:30"), and
	// how far the backoff has stepped, so a report after the rebuild that
	// names no reset takes the next step rather than the first again.
	if until.After(p.sitOut.until) {
		p.sitOut.until, p.sitOut.said, p.sitOut.resetsAt = until, said, resets
	}
	p.sitOut.unnamed = max(p.sitOut.unnamed, unnamed)
}
