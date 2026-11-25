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
// while, on this node: every call in that window fails at once as a rate limit,
// so the seat's chain moves to its next model without launching a process to
// hear the same sentence again, and so does a call already queued for a slot,
// refused once it holds the slot ([Provider.Complete]). A spent subscription
// window lasts hours and a seat makes a call per round, so unbenched every round
// would hold one of the entry's few slots for seconds of runtime to be told the
// plan is spent. It is benched here, not in the chain, because only the backend
// knows the reset instant its harness named.

// A seat's EXECUTOR reads the sit-out at launch ([Provider.SittingOut]): its run
// is started in a box, not through [Provider.Complete], so the engine asks first
// and takes the next member of the executor chain that can run in a box. A run
// that meets the limit after it started is not relaunched (its work lives in its
// box and session), but its ending is classified like any call's when collected
// ([Provider.RunEnded]), so the NEXT launch, and every other seat's on this
// node, skips the spent entry.

// A sit-out takes a working entry from every seat on the node, so only the
// VENDOR'S report may start one, never the model's sentence. The classification
// guarantees it (classifyMarkers): a sentinel counts on stderr, in a run the
// harness reported failed, in its own failure event, or where it OPENS what a
// clean run said, and nowhere else. A seat writing about the GitHub rate limit,
// or a tool call carrying "Usage limit reached", is an answer. That rule makes
// the generic, unmeasured sentinels (Copilot's "rate limit", Cursor's "quota")
// safe to bench on.

// The bench ends at the reset instant the vendor named ([harnessprofile.ResetReading]),
// however far: the plan's status read ([Provider.ReadPlan]) brings it back sooner
// when the plan resets, is raised or the sign-in changes, so no call is a probe.
// Where none was named it grows through [limitBackoff], one step per report that
// finds the window already over, and restarts once the entry answers after it.
//
// A WINDOW IS DATED BY ITS REPORT ([Provider.bench]), not the clock that reads
// it: a run's ending is read on every collection of its job, and the same
// ending read twice is one report, one window, one step.

// THE BENCH IS KEPT ON THE WALL CLOCK ([onTheWall]), the clock a vendor names its
// reset on and every reader of [SitOut.Until] compares against. An instant from
// [time.Now] also carries a monotonic reading, which Go compares alone and which
// stops while the host sleeps, so a window kept with one outlasted a sleep by
// its length: the entry went on refusing while the instant it named, on the wall
// clock, was past, and the probe re-armed for it thousands of times a second.
//
// A LOGIN IS NOT BENCHED. `crewlet llm login` mends it, usually from another
// process, and the next call must find it; a missing login costs a harness that
// exits at once, not a model call.

// limitBackoff is how long an entry sits out after a limit report that named
// no reset instant: the first step for the first report, and each later step
// for a report that finds the previous window already over, so a probe that
// meets the same limit waits longer before the next. Capped at the last step,
// fifteen minutes, which keeps a limit that has lifted from costing more than a
// quarter of an hour, and a limit that has not from costing more than four
// probes an hour.
var limitBackoff = [...]time.Duration{ShortestSitOut, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// ShortestSitOut is the shortest window an entry sits out for a limit: the
// first step of [limitBackoff], taken for a report that named no reset. A
// caller told of a limit whose end it cannot place waits no less than this
// before asking again, since no entry is tried again sooner.
const ShortestSitOut = time.Minute

// onTheWall is t without its monotonic reading, so a comparison against it is
// on the wall clock. See the file's doc for why a window must be.
func onTheWall(t time.Time) time.Time { return t.Round(0) }

// maxNamedReset is the furthest a named reset may lie from its report: a month,
// the longest window a vendor names. One further is a misread and reads as none.
const maxNamedReset = 31 * 24 * time.Hour

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

// bench sits the entry's sign-in out for a report: until reset (the vendor's
// instant, or a step of [limitBackoff] when it named none), and answers when
// the entry is tried again. A window it opens asks for a plan read. A report whose
// window is one model's ([llm.LimitModel]) holds that model alone
// ([Plan.scoped]); every other holds the whole sign-in.
//
// A later bench never SHORTENS an earlier one (two seats report the same
// window, and the last must not cut it), and a report made inside a window does
// not step the backoff: only a probe after the window, refused again, does. THE
// WINDOW RUNS FROM THE REPORT, NOT FROM THE CLOCK THAT READS IT: a run's ending
// is read on every collection, and dated by the reading each opened a fresh
// window and stepped the backoff, a quarter of an hour at a time, forever.
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
	before := b.until
	until := b.record(report)
	if until.After(before) && report.at.After(plan.readWanted) {
		plan.readWanted = onTheWall(report.at)
	}
	return until
}

// record is [Provider.bench] on one window.
func (b *bench) record(report limitReport) time.Time {
	var until time.Time
	if report.reset.Sub(report.at) > maxNamedReset {
		report.reset = time.Time{}
	}
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
		until = report.reset
	}
	if until.After(b.until) {
		b.until, b.said, b.resetsAt, b.since = onTheWall(until), report.said,
			onTheWall(report.reset), onTheWall(report.at)
		b.window = report.window
		if !b.window.Valid() {
			b.window = llm.LimitUnknown
		}
	}
	return b.until
}

// answered ends the reports behind the windows this entry's calls are held by,
// for an answer heard at `at` once those windows are over: the limit has lifted,
// so a later report naming no reset starts the backoff from its first step, and
// no refusal names the reset of a limit that is over. The answer is the
// sign-in's last ([PlanState.LastAnswer]).
//
// NOT AN ANSWER FROM INSIDE THE WINDOW, OR FROM BEFORE IT. A run already going
// when another call's report opened the window can answer inside it, and a run's
// ending is read when its job is collected, hours later, and again on every
// collection; neither says the limit lifted. Taken as one it erased the reset the
// vendor named, so every refusal read "resume time unknown". One comparison holds
// both: a window runs from the report that opened it ([Provider.bench]).
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
	log.WarnContext(ctx, "harness_limit_reported", "provider", p.key, "agent", p.harness,
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

	// NoLimit says the run's runner read its ending as no limit, whatever
	// its words (sandbox.Result.NoLimitExit): a limit it would classify is
	// none, a refused sign-in still one.
	NoLimit bool

	// Event is the harness's own terminal event, verbatim, where it prints one
	// (Claude Code's result line): how the vendor said the run ended in
	// fields — a status code — beside the words of Answer and Error.
	Event string

	// Limits is the run's last limit report, verbatim, where its harness
	// writes one ([harnessprofile.LimitReport]): where the plan's windows
	// stood as the run ended, and the reset of a limit it met.
	Limits string

	// At is when the run's ending was first read back: what a reset the
	// vendor names as a time of day, or as a delay, is read against, and
	// the instant the ending is dated by — the window a limit report opens
	// runs from it, and a success counts only once dated after the entry's
	// window ([Provider.bench], [Provider.answered]). The same on every
	// reading of one job's ending. Zero reads it as made now.
	At time.Time
}

// RunEnded reads how one of this entry's EXECUTOR runs ended by the one
// classification every call takes (the structured limit report first,
// [harnessprofile.Profile.LimitEvents]) and sits the entry out as a call that
// met the same report would. It reports whether it did. An executor's run never
// goes through [Provider.Complete], so the resuming node calls this first. ON THE VENDOR'S WORD ALONE (classifyMarkers): the terminal event where the
// profile reads a limit off one (Claude Code's `api_error_status` 429, whose
// sentence is deliberately no sentinel), then the sentinels. READ AGAINST ITS OWN
// INSTANT, SO ONCE ([RunEnd.At]): the same ending read again is the same report
// ([Provider.bench]); one whose window is over sits nothing out and reports so.
// A run that SUCCEEDED on no limit is an answer once dated after the window
// ([Provider.answered]).
func (p *Provider) RunEnded(ctx context.Context, end RunEnd) bool {
	at := end.At
	if at.IsZero() {
		at = p.now()
	}
	p.observeLimits(end.Limits, at)
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
		log.InfoContext(ctx, "harness_run_limit_report_over", "provider", p.key,
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
	log.WarnContext(ctx, "harness_run_limit_reported", "provider", p.key, "agent", p.harness,
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
	if limits := strings.TrimSpace(end.Limits); limits != "" {
		lines = append(lines, limits)
	}
	if event := strings.TrimSpace(end.Event); event != "" {
		lines = append(lines, event)
	}
	if p.profile.EffectiveMarkerScope() == harnessprofile.MarkerScopeEvents {
		if event := failureEvent(p.profile, end.Error); event != "" {
			lines = append(lines, event)
		}
	}
	out.stdout = strings.Join(lines, "\n")
	// THE SAME LINES AS THE RECORD'S: an in-box runner collects its terminal
	// lines off the run's stream, where a profile whose text phases read their
	// limit off the session's record ([harnessprofile.EventsInRecord]: Copilot's,
	// Kimi Code's) finds the same event. [classifyEvents] reads the record for a
	// failed run alone, as it does a call's.
	out.record = out.stdout
	hit, ok := classifyMarkers(p.profile, out)
	if ok && end.NoLimit && hit.Kind == llm.KindRateLimit {
		return markerHit{}, out, false
	}
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
