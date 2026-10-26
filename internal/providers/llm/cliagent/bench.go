package cliagent

import (
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
// A text call reads the sit-out as it arrives and again once it holds a slot.
// A seat's EXECUTOR reads it at launch ([Provider.SittingOut]): the run is the
// CLI's own, started in a box rather than through [Provider.Complete], so the
// engine asks before it launches and takes the next member of the seat's
// executor chain that can run in a box when this one sits out. A run that
// meets the limit after it started is not relaunched on another member — its
// work so far lives in its own box and session — but how it ended is read
// against the same markers when it is collected ([Provider.RunEnded]), so the
// turn's NEXT launch, and every other seat's on this node, skips the spent
// entry as a text call would.
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

// SittingOut is the classified refusal of a call made while the entry sits
// out — an [llm.KindRateLimit] naming the CLI's own sentence and the instant
// the entry is tried again — or nil when it does not.
//
// Exported for the executor's launch, which is not a [Provider.Complete] and
// so has to ask: an error here is what makes the launch skip this member for
// the next in the chain, classified exactly as a text call's refusal is so
// the hand-off reads the same in either place.
func (p *Provider) SittingOut() error {
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

// RunEnded reads how one of this entry's EXECUTOR runs ended — the answer its
// CLI gave, the error it reported and whether it failed, as the in-box runner
// collected them — against the entry's own limit markers, and sits the entry
// out exactly as a text call that met the same report would. It reports
// whether it did.
//
// THE OTHER HALF OF THE SECOND READER. A launch skips a member that sits out
// ([Provider.SittingOut]), but an executor's run never goes through
// [Provider.Complete], so a run that spent the plan benched nothing: the next
// launch on the entry — a reviewer sending the turn back an hour into the same
// window — walked straight into the same limit while a text call on any other
// seat would already have moved on. The resuming node calls this before the
// turn launches again, and that node is the one whose launch must see it.
//
// ON THE VENDOR'S WORD ALONE, by the one classification every text call takes
// (classifyMarkers): the answer of a run that did not fail counts only where a
// sentinel opens it, its error anywhere — and for a CLI whose failures are
// typed events ([cliprofile.MarkerScopeEvents]), the error the runner
// collected is that event's own payload, read at the profile's marker path as
// the event it came in.
func (p *Provider) RunEnded(answer, errText string, failed bool) bool {
	out := cliOutput{said: nonEmpty(answer, errText), stderr: errText, failed: failed}
	if p.profile.EffectiveMarkerScope() == cliprofile.MarkerScopeEvents {
		out.stdout = failureEvent(p.profile, errText)
	}
	hit, ok := classifyMarkers(p.profile, out)
	if !ok || hit.Kind != llm.KindRateLimit {
		return false
	}
	p.bench(hit.Retry, nonEmpty(hit.Said, firstLine(out.said)))
	log.Warn("cli_agent_run_limit_reported", "provider", p.key, "agent", p.agent,
		"source", string(hit.Source), "said", nonEmpty(hit.Said, firstLine(out.said)))
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
	prev.sitOut.mu.Unlock()
	p.sitOut.mu.Lock()
	defer p.sitOut.mu.Unlock()
	if until.After(p.sitOut.until) {
		p.sitOut.until, p.sitOut.said = until, said
	}
}
