package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// An entry whose harness reported a spent plan or a rate limit SITS OUT for a
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
// harness's own, started in a box rather than through [Provider.Complete], so the
// engine asks before it launches and takes the next member of the seat's
// executor chain that can run in a box when this one sits out. A run that
// meets the limit after it started is not relaunched on another member — its
// work so far lives in its own box and session — but how it ended is read by
// the same classification every call takes, the harness's structured limit report
// first, when it is collected ([Provider.RunEnded]), so the turn's NEXT
// launch, and every other seat's on this node, skips the spent entry as a call
// on this host would.
//
// # Why the backend benches itself
//
// A spent subscription window lasts hours, and a seat makes a call per round.
// Unbenched, every round of every seat on the entry starts a harness — seconds of
// a Node or Rust runtime holding one of the entry's few slots — only to be told
// the plan is spent and fall through to the next model anyway. The entry holds
// ONE login, so there is one thing to bench, and it is benched here rather than
// in the chain because only the backend knows the reset instant its harness named.
//
// # Only on the vendor's word
//
// A sit-out takes a working entry from every seat on the node, so what starts
// one has to be the vendor's report and never the model's sentence. The
// classification is what guarantees it, not this file: a sentinel counts on
// stderr, in a run the harness reported failed, in the harness's own failure event, or
// where it OPENS what a clean run said — the vendor's sentence standing where
// the answer should be — and nowhere else (classifyMarkers). A seat writing
// about the GitHub rate limit, or a tool call carrying "Usage limit reached"
// in its arguments, is an answer, so every rate limit that reaches
// [Provider.bench] is evidence from the harness. That rule, and not the wording of
// a sentinel, is what makes the shipped profiles' generic, unmeasured
// sentinels — Copilot's "rate limit", Cursor's "quota" — safe to bench on.
//
// # On this node, and for not too long
//
// The bench is the provider's own and is not shared with the fleet: every
// other node learns the same thing from its own calls, once per window. It ends
// at the reset instant the vendor named ([harnessprofile.ResetReading]), never
// later than [maxLimitBench]. Where the vendor named none it grows through
// [limitBackoff], one step per report that finds the window already over, and
// starts again from the first once the entry answers after the window it sat
// out: a limit that lifts in a minute costs a minute, and one that lasts all
// afternoon costs four process launches an hour per entry rather than one per
// round of every seat. Short at first on purpose: sitting out a plan that has
// already reset costs the fallback's spend, or the seats' whole afternoon where
// there is no fallback, for every minute of it.
//
// A WINDOW IS DATED BY ITS REPORT, not by the clock that reads it
// ([Provider.bench]): a run's ending is read on every collection of its job,
// and the same ending read twice is one report, which opens one window and
// takes one step.
//
// A LOGIN IS NOT BENCHED. `crewlet llm login` mends it — usually from another
// process this one hears nothing from — and the very next call must find it,
// where a window would hold a signed-in seat on its fallback for nothing; and
// finding out that a login is still missing costs a harness that exits at once
// rather than a model call.

// limitBackoff is how long an entry sits out after a limit report that named
// no reset instant: the first step for the first report, and each later step
// for a report that finds the previous window already over, so a probe that
// meets the same limit waits longer before the next. Capped at the last step,
// fifteen minutes, which keeps a limit that has lifted from costing more than a
// quarter of an hour, and a limit that has not from costing more than four
// probes an hour.
var limitBackoff = [...]time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// maxLimitBench caps a reset instant the harness named: an hour, so an operator
// who signs in again or raises the plan has the entry back within the hour
// even when the vendor's window runs for days.
const maxLimitBench = time.Hour

// bench is one window a sign-in sits out and the report behind it, held in its
// [Plan] and guarded by the plan's mutex.
type bench struct {
	until time.Time
	// said is the harness's own sentence that started it, repeated on every
	// call it refuses so the reason never reads as the engine's.
	said string
	// resetsAt is when the vendor said the limit lifts, zero where it named
	// no instant.
	resetsAt time.Time
	// since is when the report that opened the window in force was made.
	since time.Time
	// window is which of the vendor's limits it is.
	window llm.LimitWindow
	// unnamed counts the windows sat out for reports that named no reset
	// since the sign-in last answered: the step of [limitBackoff] the next
	// one takes.
	unnamed int
}

// limitReport is one report of a limit, as the classification read it.
type limitReport struct {
	// reset is when the vendor said the limit lifts, zero for unnamed.
	reset time.Time
	// said is the vendor's sentence.
	said string
	// at is when the report was made: the instant the window runs from.
	at time.Time
	// window is which limit it is; [llm.LimitModel] holds only the model
	// argument the entry hands its harness.
	window llm.LimitWindow
}

// bench sits the entry's sign-in out for a report: until reset (the instant
// the vendor named, or a step of [limitBackoff] when it named none), capped at
// [maxLimitBench], and answers when the entry is tried again. A report whose
// window is one model's ([llm.LimitModel]) holds that model alone
// ([Plan.scoped]); every other holds the whole sign-in.
//
// A later bench never SHORTENS an earlier one: two seats' calls that both met
// the spent plan report the same window, and the one that lands last must not
// cut it. For the same reason a report made while a window is still in force
// does not step the backoff: only a probe made after the window ended, and
// refused again, does.
//
// THE WINDOW RUNS FROM THE REPORT, NOT FROM THE CLOCK THAT READS IT. A call's
// report is read as it is made, but a run's ending is read when its job is
// collected ([Provider.RunEnded]), and a completion held on a limit is
// collected again when its hold lifts. Dated by the reading, the same ending
// opened a fresh window on every collection and, naming no reset, stepped the
// backoff as a probe that met the limit again would: the resume that waited
// out one window found the entry benched again before any call was made, held
// itself for a longer one, and went round again at every step, a quarter of an
// hour at a time, with nothing ever asking the vendor whether the quota was
// back. Dated by the report, an ending read again lands inside the window its
// first reading opened, or opens the same one, and moves nothing.
func (p *Provider) bench(report limitReport) time.Time {
	plan := p.Plan()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	b := &plan.wide
	if report.window == llm.LimitModel {
		model := p.ModelArgument()
		if plan.scoped[model] == nil {
			plan.scoped[model] = &bench{}
		}
		b = plan.scoped[model]
	}
	return b.record(report)
}

// record is [Provider.bench] on one window.
func (b *bench) record(report limitReport) time.Time {
	var until time.Time
	if report.reset.IsZero() {
		if report.at.Before(b.until) {
			// Inside a window already in force: the same refusal, heard
			// by another call or read again, which neither extends it
			// nor steps on.
			return b.until
		}
		until = report.at.Add(limitBackoff[min(b.unnamed, len(limitBackoff)-1)])
		b.unnamed++
	} else {
		until = report.at.Add(min(report.reset.Sub(report.at), maxLimitBench))
	}
	if until.After(b.until) {
		b.until, b.said, b.resetsAt, b.since = until, report.said, report.reset, report.at
		b.window = report.window
		if !b.window.Valid() {
			b.window = llm.LimitUnknown
		}
	}
	return b.until
}

// answered ends the reports behind the windows this entry's calls are held by,
// for an answer heard at `at` once those windows are over: the limit has
// lifted, so a later report that names no reset starts the backoff from its
// first step again, and no refusal names the reset of a limit that is over.
// The answer is the sign-in's last ([PlanState.LastAnswer]).
//
// NOT AN ANSWER FROM INSIDE THE WINDOW, OR FROM BEFORE IT. No call is made
// while the entry sits out, but one already running when another call's report
// opened the window can answer inside it; and a run's ending is read when its
// job is collected, which is hours after the run ended for a completion its
// seat's conditions held, and again on every collection. Either says the
// vendor answered at some instant before the report or inside its window,
// which is not that the limit lifted. Taken as one, it erased the reset the
// vendor named while the entry still sat it out, so every refusal in the
// window read "resume time unknown", and it restarted a backoff the limit had
// earned. One comparison holds both: a window runs from the report that
// opened it ([Provider.bench]), so an answer from before that report is from
// before the window's end as well.
func (p *Provider) answered(at time.Time) {
	plan := p.Plan()
	model := p.ModelArgument()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	plan.wide.answered(at)
	if scoped := plan.scoped[model]; scoped != nil && scoped.answered(at) {
		delete(plan.scoped, model)
	}
	if at.After(plan.lastAnswer) {
		plan.lastAnswer = at.UTC()
	}
}

// answered is [Provider.answered] on one window, reporting whether the window
// is over and its report ended.
func (b *bench) answered(at time.Time) bool {
	if at.Before(b.until) {
		return false
	}
	b.resetsAt, b.unnamed = time.Time{}, 0
	return true
}

// sitOut reads the window at now, false when it is not in force.
func (b *bench) sitOut(now time.Time, model string) (SitOut, bool) {
	if !now.Before(b.until) {
		return SitOut{}, false
	}
	return SitOut{Until: b.until.UTC(), ResetsAt: b.resetsAt, Since: b.since.UTC(),
		Window: b.window, Model: model, Said: b.said}, true
}

// clearBefore ends the window when the report that opened it was made before
// `before`, and the backoff with it ([Plan.Clear]).
func (b *bench) clearBefore(before time.Time) bool {
	if b.until.IsZero() || !b.since.Before(before) {
		return false
	}
	*b = bench{}
	return true
}

// heldBy is the window this entry's calls are held by at now: the later of
// the plan-wide one and the one of the model it hands its harness.
func (p *Provider) heldBy(now time.Time) (SitOut, bool) {
	plan := p.Plan()
	model := p.ModelArgument()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	held, ok := plan.wide.sitOut(now, "")
	if scoped := plan.scoped[model]; scoped != nil {
		if out, in := scoped.sitOut(now, model); in && (!ok || out.Until.After(held.Until)) {
			held, ok = out, true
		}
	}
	return held, ok
}

// SitOut is the window this entry's calls are held by now, and false when they
// are held by none: the sign-in's plan-wide window or its model's own,
// whichever runs later.
func (p *Provider) SitOut() (SitOut, bool) {
	held, ok := p.heldBy(p.now())
	held.Said = boundSaid(held.Said)
	return held, ok
}

// SittingOut is the classified refusal of a call made while the entry sits
// out — an [llm.KindRateLimit] naming the harness's own sentence and the instant
// the entry is tried again — or nil when it does not.
//
// Exported for the executor's launch, which is not a [Provider.Complete] and
// so has to ask: an error here is what makes the launch skip this member for
// the next in the chain, classified exactly as a call's refusal is so
// the hand-off reads the same in either place.
func (p *Provider) SittingOut() error {
	now := p.now()
	held, ok := p.heldBy(now)
	if !ok {
		return nil
	}
	return p.limited(fmt.Errorf("%s, sitting out until %s, when this entry is tried again",
		held.Said, held.Until.Format(time.RFC3339)), held.Until, held.ResetsAt, held.Window)
}

// classified turns a marker's hit into the call's failure: a limit sits the
// entry out and carries when it is tried again, and anything else is the kind
// the marker named.
func (p *Provider) classified(ctx context.Context, hit markerHit, reason string) error {
	if hit.Kind != llm.KindRateLimit {
		return p.fail(hit.Kind, fmt.Errorf("%s", reason))
	}
	until := p.bench(limitReport{reset: hit.Reset, said: reason, at: p.now(), window: hit.Window})
	// WHERE IT MATCHED, beside what it matched: a sit-out takes the entry
	// from every seat on this node, and the one question an operator who
	// doubts it has is whether the vendor said it.
	resets := "unknown"
	if !hit.Reset.IsZero() {
		resets = hit.Reset.UTC().Format(time.RFC3339)
	}
	log.WarnContext(ctx, "cli_agent_limit_reported", "provider", p.key, "agent", p.harness,
		"source", string(hit.Source), "said", reason, "window", string(hit.Window),
		"retry_at", until.UTC().Format(time.RFC3339), "resets_at", resets)
	return p.limited(fmt.Errorf("%s", reason), until, hit.Reset, hit.Window)
}

// limited is a classified rate limit carrying when the entry is tried again,
// when the vendor said the limit lifts and which limit it is, so a caller telling a person to
// wait can name the instant without reading it out of the vendor's sentence.
func (p *Provider) limited(err error, until, resets time.Time, window llm.LimitWindow) error {
	failure := p.failure(llm.KindRateLimit, err)
	failure.RetryAt = until
	failure.ResetsAt = resets
	failure.Window = window
	return failure
}

// RunEnd is how one of an entry's EXECUTOR runs ended, as the in-box runner
// collected it ([Provider.RunEnded]).
type RunEnd struct {
	// Answer is what the run said last, Error the error it reported, and
	// Failed whether the harness said the run failed.
	Answer, Error string
	Failed        bool

	// Event is the harness's own terminal event, verbatim, where it prints one
	// (Claude Code's result line): how the vendor said the run ended in
	// fields — a status code — beside the words of Answer and Error.
	Event string

	// At is when the run's ending was first read back: what a reset the
	// vendor names as a time of day, or as a delay, is read against, and
	// the instant the ending is dated by — the window a limit report opens
	// runs from it, and a success counts only once dated after the entry's
	// window ([Provider.bench], [Provider.answered]). The same on every
	// reading of one job's ending. Zero reads it as made now.
	At time.Time
}

// RunEnded reads how one of this entry's EXECUTOR runs ended by the one
// classification every call takes, the harness's structured limit report first
// ([harnessprofile.Profile.LimitEvents]), and sits the entry out exactly as a call
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
// counts only where a sentinel opens it, its error anywhere, and for a harness
// whose failures are typed events ([harnessprofile.MarkerScopeEvents]) the error
// the runner collected is that event's own payload, read at the profile's
// marker path as the event it came in.
//
// READ AGAINST ITS OWN INSTANT, AND SO ONCE ([RunEnd.At]): a held completion
// is collected again when its hold lifts, and the same ending read again is
// the same report, which sits out the window its first reading opened and
// nothing more ([Provider.bench]). One whose window is already over sits
// nothing out, and reports that it did not: the limit's next word is the next
// call's.
//
// A RUN THAT SUCCEEDED ON NO LIMIT IS AN ANSWER, as a call's is, once its
// ending is dated after the entry's window ([Provider.answered]): the entry's
// backoff starts again from its first step.
func (p *Provider) RunEnded(ctx context.Context, end RunEnd) bool {
	at := end.At
	if at.IsZero() {
		at = p.now()
	}
	hit, out, ok := p.endedHit(end, at)
	if ok && hit.Kind == llm.KindAuth {
		// A REFUSED SIGN-IN IS THE SIGN-IN'S, as a call's is: the readiness
		// verdict and the last failure of every entry on the same account.
		// Never sat out ([Provider.remember]).
		p.remember(p.fail(llm.KindAuth, fmt.Errorf("%s", nonEmpty(hit.Said, firstLine(out.said)))))
		return false
	}
	if !ok || hit.Kind != llm.KindRateLimit {
		if !end.Failed {
			p.answered(at)
		}
		return false
	}
	said := nonEmpty(hit.Said, firstLine(out.said))
	until := p.bench(limitReport{reset: hit.Reset, said: said, at: at, window: hit.Window})
	if !p.now().Before(until) {
		log.InfoContext(ctx, "cli_agent_run_limit_report_over", "provider", p.key,
			"agent", p.harness, "said", said, "reported_at", at.UTC().Format(time.RFC3339),
			"retry_at", until.UTC().Format(time.RFC3339),
			"detail", "the run's limit report is older than the window it opened, which is "+
				"over; the entry sits nothing out on it")
		return false
	}
	resets := "unknown"
	if !hit.Reset.IsZero() {
		resets = hit.Reset.UTC().Format(time.RFC3339)
	}
	log.WarnContext(ctx, "cli_agent_run_limit_reported", "provider", p.key, "agent", p.harness,
		"source", string(hit.Source), "said", said, "window", string(hit.Window),
		"retry_at", until.UTC().Format(time.RFC3339), "resets_at", resets)
	return true
}

// EndedKind is how one of this entry's executor runs ended, as the one
// classification every call takes reads it: a limit, a refused sign-in, or
// the kind a sentinel named; false when nothing classified it (an answer, or a
// failure no profile recognises). PURE: it records nothing on the entry's
// [Plan], which [Provider.RunEnded] does. For the engine's question at a
// run's collection, whether the run ended on a refused sign-in.
func (p *Provider) EndedKind(end RunEnd) (llm.ErrorKind, bool) {
	at := end.At
	if at.IsZero() {
		at = p.now()
	}
	hit, _, ok := p.endedHit(end, at)
	if !ok {
		return 0, false
	}
	return hit.Kind, true
}

// endedHit classifies a run's ending: what [Provider.RunEnded] and
// [Provider.EndedKind] both read.
func (p *Provider) endedHit(end RunEnd, at time.Time) (markerHit, harnessOutput, bool) {
	out := harnessOutput{said: nonEmpty(end.Answer, end.Error), stderr: end.Error,
		failed: end.Failed, at: at, zone: p.zone}
	var lines []string
	if event := strings.TrimSpace(end.Event); event != "" {
		lines = append(lines, event)
	}
	if p.profile.EffectiveMarkerScope() == harnessprofile.MarkerScopeEvents {
		if event := failureEvent(p.profile, end.Error); event != "" {
			lines = append(lines, event)
		}
	}
	out.stdout = strings.Join(lines, "\n")
	hit, ok := classifyMarkers(p.profile, out)
	return hit, out, ok
}

// failureEvent is the failure event a run's collected error came out of, as one
// line of its harness's event stream: the payload put back where the profile's
// marker path reads it, under the first of its failure event types. Empty when
// the error is not an object — no payload the harness typed — or the profile names
// no event to put it in.
func failureEvent(p harnessprofile.Profile, errText string) string {
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
func setPath(doc map[string]any, path harnessprofile.Path, v any) bool {
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
