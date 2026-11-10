package issuereport

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
)

var log = logging.Get("issuereport")

const (
	// ResendWindow is how long a report the relay accepted holds its
	// fingerprint off, company-wide: the promise "once per seven days".
	ResendWindow = 7 * 24 * time.Hour
	// failedRetryAfter is how long a report the relay never accepted waits:
	// nothing was filed, so the promise does not hold it, and a day keeps a
	// relay's outage from being retried by every node at every occurrence.
	failedRetryAfter = 24 * time.Hour
	// sendingLease is how long a claim stands with no settle: long past
	// [maxAttempts] of its requests and waits, so it only lapses on a node
	// that died mid-send. The relay then answers the retry as a duplicate.
	sendingLease = time.Hour
	// maxAttempts bounds one send, and its waits double from [retryBase] to
	// [retryCeiling]: about eight minutes in all.
	maxAttempts  = 5
	retryBase    = 30 * time.Second
	retryCeiling = 15 * time.Minute
	// retryJitter spreads a fleet's retries by a fifth, as backoff's other
	// callers do.
	retryJitter = 0.2
	// queueDepth bounds what waits for the sender. Every class at once fits;
	// past it a report is dropped, and its next occurrence offers it again.
	queueDepth = 16
	// maxTrackers bounds the subjects counted at once on this node: failures
	// alone make one, and one this many in a window is a broken company rather
	// than a detail worth keeping.
	maxTrackers = 1024
	// MaxReports bounds the records a company keeps. One record is a class at
	// one function, so only a panicking engine grows them, and this many
	// defects are past what another report could add.
	MaxReports = 256
)

// Store is the fleet's sent record, as this package uses it.
type Store interface {
	IssueReport(ctx context.Context, fingerprint string) (coord.IssueReport, bool, error)
	ListIssueReports(ctx context.Context) ([]coord.IssueReport, error)
	ClaimIssueReport(ctx context.Context, want coord.IssueReport, open func(coord.IssueReport) bool) (coord.IssueReport, bool, error)
	SettleIssueReport(ctx context.Context, r coord.IssueReport) (coord.IssueReport, bool, error)
}

// Config is what a [Reporter] runs on.
type Config struct {
	Store Store
	// Topology reads how the engine runs, when a report is built.
	Topology func(ctx context.Context) Topology
	// Origin is this binary, as [OriginOf] read it. An inert one sends nothing,
	// whatever the setting says.
	Origin Origin
	// Client sends to the relay; nil is [NewClient]'s.
	Client *http.Client
	// URL is where a report goes; "" is [RelayURL]. The company document has
	// no say in it.
	URL string
	// Fleet reads the whole fleet as the shadow needs it ([Fleet]); nil
	// shadows nothing. An error shadows as everything it could have said.
	Fleet func(ctx context.Context) (Fleet, error)
	// RetryBase replaces [retryBase] and Now the clock, for a test.
	RetryBase time.Duration
	Now       func() time.Time
}

// Reporter takes typed signals from the wired sites and sends what crosses a
// threshold. Off, it does nothing at all: see [Reporter.SetEnabled].
type Reporter struct {
	cfg Config
	// setting is the company's choice as this node applied it, and active is
	// that choice on a build that may report: what every path here reads.
	setting atomic.Bool
	active  atomic.Bool
	sending atomic.Bool
	queue   chan pending
	// shadow is whether an alarm in [ShadowAlarms] stands in the fleet, and
	// moving whether it hands seats between nodes by design, both as last
	// read: true until a read answers, so an unread fleet counts nothing.
	shadow atomic.Bool
	moving atomic.Bool
	// enabled wakes the fleet watch when the reporter turns on.
	enabled chan struct{}

	mu       sync.Mutex
	trackers map[trackerKey]*tracker
	queued   map[Fingerprint]bool
	quiet    map[Fingerprint]time.Time
	// cancelSend ends the send under way, nil where there is none.
	cancelSend context.CancelFunc
}

// New builds a reporter, off until [Reporter.SetEnabled] says otherwise.
func New(cfg Config) *Reporter {
	if cfg.Client == nil {
		cfg.Client = NewClient()
	}
	if cfg.URL == "" {
		cfg.URL = RelayURL
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = retryBase
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Reporter{cfg: cfg, queue: make(chan pending, queueDepth), enabled: make(chan struct{}, 1)}
	r.unread()
	r.reset()
	return r
}

// reset forgets everything held in memory.
func (r *Reporter) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trackers = map[trackerKey]*tracker{}
	r.queued = map[Fingerprint]bool{}
	r.quiet = map[Fingerprint]time.Time{}
}

// SetEnabled applies the company's setting. OFF IS OFF: what was counted and
// queued is forgotten and a send under way is cancelled, so a revision that
// turns it off stops it, and turning it on again starts from nothing. On an
// inert build it stays off, and says so once.
func (r *Reporter) SetEnabled(on bool) {
	inert := r.Inert()
	if r.setting.Swap(on) != on && on && inert != "" {
		log.Info("issue_reports_inert", "reason", string(inert),
			"detail", "the setting is on and this build sends no report: only a release, the image of "+
				"main, a clean build from this repository's commit or a go install does")
	}
	active := on && inert == ""
	if r.active.Swap(active) == active {
		return
	}
	if active {
		select {
		case r.enabled <- struct{}{}:
		default:
		}
		return
	}
	r.unread()
	r.mu.Lock()
	cancel := r.cancelSend
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for drained := false; !drained; {
		select {
		case <-r.queue:
		default:
			drained = true
		}
	}
	r.reset()
}

// Enabled reports the company's setting as this node applied it.
func (r *Reporter) Enabled() bool { return r.setting.Load() }

// Sending reports whether [Reporter.Run] is running: a reporter nothing drains
// detects and never sends.
func (r *Reporter) Sending() bool { return r.sending.Load() }

// Inert is why this binary sends no report, "" for one that may.
func (r *Reporter) Inert() InertReason { return r.cfg.Origin.InertReason() }

// Signal is one failure a wired site saw. ITS SUBJECT IS HASHED BEFORE IT IS
// KEPT and no report has a field for it, so a seat or a conversation can be
// told apart here and named nowhere.
type Signal struct {
	Class Class
	// Subject is what the threshold counts per: a seat, a conversation.
	Subject string
	// Kind is what makes two failures the same one; another kind starts over.
	Kind string
	// Location is the function, where the signal knows better than the catalogue.
	Location Location
	// Frames is the stack under it, innermost first, for a class that takes them.
	Frames  []Frame
	Harness *Harness
	Facts   map[FactKey]FactValue
	// Shadowed says an outage of the environment can cause this failure, so
	// it counts nothing while an alarm in [ShadowAlarms] stands. Never a panic.
	Shadowed bool
	// Held says the report is kept in the company's own record and never sent:
	// a defect anybody could raise at will goes through SECURITY.md.
	Held bool
	// Once says this one occurrence crosses the class's threshold: a failure
	// that has already exhausted every retry the threshold stands for.
	Once bool
	// HandBack says this is a delivery handed back, which a fleet moving
	// seats does by design: it counts nothing while one is ([Fleet.Moving]).
	HandBack bool
}

type trackerKey struct {
	class   Class
	subject [sha256.Size]byte
}

type tracker struct {
	kind        string
	count       int
	first, last time.Time
}

// pending is an occurrence past its threshold, waiting for the sender.
type pending struct {
	entry       Entry
	at          Location
	frames      []Frame
	harness     *Harness
	facts       map[FactKey]FactValue
	held        bool
	count       int
	first, last time.Time
	fingerprint Fingerprint
}

// Observe takes a signal. It never blocks and never fails: a report is not
// worth a moment of a turn.
func (r *Reporter) Observe(_ context.Context, s Signal) {
	defer quietly("observe")
	if !r.active.Load() {
		return
	}
	entry, ok := entryOf(s.Class)
	if !ok {
		return
	}
	now := r.cfg.Now()
	key := trackerKey{class: s.Class, subject: sha256.Sum256([]byte(s.Subject))}

	r.mu.Lock()
	defer r.mu.Unlock()
	// A SHADOWED FAILURE IS NOT IN A ROW: the environment alarm standing
	// names its cause, and the row starts again once it clears.
	if (s.Shadowed && r.shadow.Load()) || (s.HandBack && r.moving.Load()) {
		delete(r.trackers, key)
		return
	}
	t := r.trackers[key]
	if t == nil || t.kind != s.Kind || now.Sub(t.first) > entry.Threshold.Window() {
		if t == nil && len(r.trackers) >= maxTrackers && !r.evictLocked(now) {
			return
		}
		t = &tracker{kind: s.Kind, first: now}
		r.trackers[key] = t
	}
	t.count++
	t.last = now
	if t.count < entry.Threshold.Count && !s.Once {
		return
	}
	component, at := entry.placeOf(s.Location)
	fingerprint := entry.fingerprintOf(component, at.Site, s.Harness, s.Facts)
	if r.queued[fingerprint] || now.Before(r.quiet[fingerprint]) {
		return
	}
	select {
	case r.queue <- pending{entry: entry, at: at, frames: s.Frames, harness: s.Harness, facts: s.Facts,
		held: s.Held, count: t.count, first: t.first, last: t.last, fingerprint: fingerprint}:
		r.queued[fingerprint] = true
	default:
		log.Debug("issue_report_queue_full", "class", string(entry.ID))
	}
}

// Passed is a subject's success: its failures are no longer in a row.
func (r *Reporter) Passed(_ context.Context, class Class, subject string) {
	if !r.active.Load() {
		return
	}
	key := trackerKey{class: class, subject: sha256.Sum256([]byte(subject))}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.trackers, key)
}

// evictLocked frees room by dropping every tracker past its window, reporting
// whether it made any.
func (r *Reporter) evictLocked(now time.Time) bool {
	before := len(r.trackers)
	for key, t := range r.trackers {
		if entry, ok := entryOf(key.class); !ok || now.Sub(t.first) > entry.Threshold.Window() {
			delete(r.trackers, key)
		}
	}
	return len(r.trackers) < before
}

// OnPublish is the publish listener: the executor's and the turn's records are
// the real failure paths, and each is typed already. It runs in the publisher's
// goroutine, and reads no more than the event's type while off. A panic does not
// come this way: [Reporter.Panicked] takes every one the engine contains.
func (r *Reporter) OnPublish(ctx context.Context, _ string, ev *events.Event) {
	defer quietly("publish")
	if ev == nil || !r.active.Load() {
		return
	}
	switch data := ev.Data.(type) {
	case *types.ExecutorCollected:
		r.onCollected(ctx, *data)
	case *types.AgentTurnCompleted:
		r.onTurnCompleted(ctx, *data)
	case *types.ExecutorFailed:
		r.onExecutorFailed(ctx, *data)
	}
}

func (r *Reporter) onCollected(ctx context.Context, rec types.ExecutorCollected) {
	session := sessionOf(rec)
	if !rec.Failed {
		if rec.Notes == "" {
			r.Passed(ctx, ClassContextExhaustedLoop, session)
		}
		return
	}
	if rec.ErrorKind != types.ExecutorErrorContextExhausted || rec.ConversationKey == "" {
		r.Passed(ctx, ClassContextExhaustedLoop, session)
		return
	}
	facts := map[FactKey]FactValue{
		FactSessionSizeBucket: enumFact(SizeBucket(rec.SessionTakenBytes)),
		FactSessionContinued:  boolFact(rec.HarnessSessionContinued),
	}
	// The window the run was pinned to, which only a Claude Code record carries.
	if rec.ContextWindowTokens > 0 {
		facts[FactContextWindowTokens] = intFact(int64(rec.ContextWindowTokens))
	}
	r.Observe(ctx, Signal{Class: ClassContextExhaustedLoop, Subject: session,
		Kind: rec.ErrorKind, Harness: harnessOf(rec.Harness, rec.HarnessVersion), Facts: facts})
}

// sessionOf is what a context loop counts per: one seat's session in one
// conversation. A thread is shared by every seat in it, and another seat's
// success there says nothing about this seat's session.
func sessionOf(rec types.ExecutorCollected) string {
	return rec.Agent + "\x00" + rec.ConversationKey
}

func (r *Reporter) onTurnCompleted(ctx context.Context, rec types.AgentTurnCompleted) {
	switch {
	case rec.Suspended || rec.Stopped:
	case !rec.Failed || rec.ErrorKind != kindError:
		// A kind the engine attributes elsewhere, and a success, end the row.
		r.Passed(ctx, ClassFailureLoop, rec.Agent)
	default:
		r.Observe(ctx, failureAt(rec.Agent, rec.ErrorFunction, rec.ErrorFile, false))
	}
}

// failureAt is a seat's unclassified failure where its error left the engine,
// as its stamp names it: THE PLACE IS THE KIND, so a failure at another place
// starts the row again, and an unstamped one is a place of its own.
func failureAt(seat, function, file string, lost bool) Signal {
	at := LocationOf(function, file)
	return Signal{Class: ClassFailureLoop, Subject: seat, Kind: string(at.Site), Location: at,
		Facts: map[FactKey]FactValue{FactMessageLost: boolFact(lost)}, Shadowed: true, Once: lost}
}

// Panicked takes a panic the engine contained, as [panics.Observe] hands it on:
// the one path an engine.panicked report is built from, whatever site contained it.
// It runs on the recovering goroutine and never blocks.
func (r *Reporter) Panicked(p *panics.Panic) {
	defer quietly("panicked")
	if p == nil || !r.active.Load() {
		return
	}
	r.Observe(context.Background(), panicSignal(p))
}

// panicSignal is a contained panic as a signal: its function, the module frames
// under it, and its scope and kind. EXPOSED, it keeps its site and drops its
// frames ([exposedPanic]). It counts per site, so one defect is one tracker
// however many seats or requests reach it.
func panicSignal(p *panics.Panic) Signal {
	at := LocationOf(p.Function(), p.File())
	frames := make([]Frame, 0, maxFrames)
	for _, f := range p.Frames {
		if len(frames) == maxFrames {
			break
		}
		if frame, ok := FrameOf(f.Function, f.File, f.Line); ok {
			frames = append(frames, frame)
		}
	}
	if exposedPanic(p, at, frames) {
		frames = nil
	}
	return Signal{Class: ClassEnginePanicked, Subject: string(at.Site), Location: at, Frames: frames,
		Facts: map[FactKey]FactValue{
			FactScope:     enumFact(string(p.Scope)),
			FactPanicKind: enumFact(string(p.Kind)),
		}}
}

// onExecutorFailed takes the endings that are the engine's own invariant. The
// other reasons are its environment or a person's act (an unreachable box, a passed
// deadline, a removed seat), or already a panic's report (resume_broken).
func (r *Reporter) onExecutorFailed(ctx context.Context, rec types.ExecutorFailed) {
	shadowed := false
	switch rec.Reason {
	case types.ExecutorFailureNoExecuteState, types.ExecutorFailureStateRetired:
	case types.ExecutorFailureSuspensionUnrecorded:
		// A coordination store that cannot record a suspension says as much with
		// an alarm, and then the store is what failed.
		shadowed = true
	case types.ExecutorFailureResumeFailing:
		// A turn whose models were out for the whole window failed too.
		if rec.ErrorKind != kindError {
			return
		}
	default:
		return
	}
	r.Observe(ctx, Signal{Class: ClassRunStranded, Subject: rec.Agent, Kind: rec.Reason, Shadowed: shadowed,
		Facts: map[FactKey]FactValue{FactReason: enumFact(rec.Reason)}})
}

// MessageLost is a seat's message going to the dead-letter subject on a failure
// nothing classified, named where its error left the engine (function and file
// as its stamp holds them, empty where none did). One is a report: the
// message's whole delivery budget is spent, and nobody was answered.
func (r *Reporter) MessageLost(ctx context.Context, seat, function, file string) {
	r.Observe(ctx, failureAt(seat, function, file, true))
}

// DeadLettered is a seat's message going to the dead-letter subject after its
// deliveries were handed back, the last for cause.
func (r *Reporter) DeadLettered(ctx context.Context, seat string, cause queue.DeferCause) {
	r.Observe(ctx, Signal{Class: ClassDeadLettered, Subject: seat, Kind: string(cause),
		Facts:    map[FactKey]FactValue{FactDeferCause: enumFact(string(cause))},
		Shadowed: true, HandBack: true})
}

// Run sends what crosses a threshold until ctx ends. One at a time: the rate
// is a handful a week, and a second connection would buy nothing. Beside it
// runs the watch that keeps the shadow current, which ends with it.
func (r *Reporter) Run(ctx context.Context) {
	r.sending.Store(true)
	defer r.sending.Store(false)
	if r.cfg.Fleet != nil {
		var watch sync.WaitGroup
		watch.Go(func() { r.watchFleet(ctx) })
		defer watch.Wait()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-r.queue:
			func() {
				defer quietly("send")
				r.send(ctx, p)
			}()
		}
	}
}

// send claims the fingerprint, posts, and settles what the relay said. It is
// the only place a report leaves.
func (r *Reporter) send(ctx context.Context, p pending) {
	defer func() {
		r.mu.Lock()
		delete(r.queued, p.fingerprint)
		r.mu.Unlock()
	}()
	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	r.cancelSend = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cancelSend = nil
		r.mu.Unlock()
	}()
	if !r.active.Load() {
		return
	}
	report, err := r.reportOf(sendCtx, p)
	if err != nil {
		log.Warn("issue_report_not_built", "class", string(p.entry.ID), "error", err.Error())
		return
	}
	body, err := report.Encode()
	if err != nil {
		log.Warn("issue_report_not_encoded", "class", string(p.entry.ID), "error", err.Error())
		return
	}
	if p.held {
		// KEPT, NOT SENT: the company's own record holds the report for a person to
		// file privately, and the relay never hears of it.
		if claim, ok := r.claim(sendCtx, report, body, coord.IssueReportHeld); ok {
			r.holdOff(p.fingerprint, claim)
			log.Info("issue_report_held", "class", string(p.entry.ID), "fingerprint", claim.Fingerprint)
		}
		return
	}
	claim, ok := r.claim(sendCtx, report, body, coord.IssueReportSending)
	if !ok {
		return
	}
	status, issue := r.post(sendCtx, report, body)
	claim.Status, claim.Issue = status, issue
	settle, done := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer done()
	if _, settled, err := r.cfg.Store.SettleIssueReport(settle, claim); err != nil || !settled {
		log.Info("issue_report_not_settled", "class", string(p.entry.ID), "fingerprint", claim.Fingerprint)
	}
	r.holdOff(p.fingerprint, claim)
	log.Info("issue_report_sent", "class", string(p.entry.ID), "fingerprint", claim.Fingerprint, "status", string(status))
}

// storeTimeout bounds a settle, which runs past a cancelled send: a coordination
// write answers in milliseconds, and a store this slow is not worth a sender.
const storeTimeout = 10 * time.Second

func (r *Reporter) reportOf(ctx context.Context, p pending) (Report, error) {
	topology := NewTopology(1, "", "", "")
	if r.cfg.Topology != nil {
		topology = r.cfg.Topology(ctx)
	}
	return build(p, r.cfg.Origin.Build, topology)
}

// claim takes the company's right to send this fingerprint now, reporting
// whether this node has it. A STORE IT CANNOT READ ANSWERS NO: the promise is at
// most once, and the next occurrence asks again.
func (r *Reporter) claim(ctx context.Context, report Report, body []byte, status coord.IssueReportStatus,
) (coord.IssueReport, bool) {
	fingerprint := string(report.Fingerprint)
	now := r.cfg.Now().UTC()
	held, found, err := r.cfg.Store.IssueReport(ctx, fingerprint)
	if err != nil {
		log.Debug("issue_report_record_unread", "class", string(report.Class), "error", err.Error())
		return coord.IssueReport{}, false
	}
	first := now
	if found {
		if !mayResend(held, now) {
			r.holdOff(report.Fingerprint, held)
			return coord.IssueReport{}, false
		}
		first = held.FirstSent
	} else {
		all, listErr := r.cfg.Store.ListIssueReports(ctx)
		if listErr != nil || len(all) >= MaxReports {
			log.Debug("issue_report_not_recorded", "class", string(report.Class), "reports", len(all))
			return coord.IssueReport{}, false
		}
	}
	claim, won, err := r.cfg.Store.ClaimIssueReport(ctx, coord.IssueReport{
		Fingerprint: fingerprint, Class: string(report.Class), Status: status,
		FirstSent: first, ClaimedAt: now, Payload: json.RawMessage(body),
	}, func(held coord.IssueReport) bool { return mayResend(held, now) })
	if err != nil {
		log.Debug("issue_report_not_claimed", "class", string(report.Class), "error", err.Error())
		return coord.IssueReport{}, false
	}
	if !won {
		r.holdOff(report.Fingerprint, claim)
		return coord.IssueReport{}, false
	}
	return claim, true
}

// mayResend is the one rule over a held record, by what became of it.
func mayResend(held coord.IssueReport, now time.Time) bool {
	return !now.Before(held.ClaimedAt.Add(holdOf(held.Status)))
}

func holdOf(status coord.IssueReportStatus) time.Duration {
	switch status {
	case coord.IssueReportSending:
		return sendingLease
	case coord.IssueReportFailed:
		return failedRetryAfter
	}
	return ResendWindow
}

// holdOff keeps this node from asking the store again before a held record
// could let it: without it, each occurrence of a loop is a coordination read.
func (r *Reporter) holdOff(fingerprint Fingerprint, held coord.IssueReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quiet[fingerprint] = held.ClaimedAt.Add(holdOf(held.Status))
}

// post sends with jittered, doubling waits, and says what became of it. Only a
// failure a later attempt could mend is tried again.
func (r *Reporter) post(ctx context.Context, report Report, body []byte) (coord.IssueReportStatus, string) {
	class := string(report.Class)
	for attempt := 1; ; attempt++ {
		if !r.active.Load() || ctx.Err() != nil {
			return coord.IssueReportFailed, ""
		}
		answer, err := r.deliver(ctx, body)
		if err == nil {
			return answer.status, answer.issue
		}
		var refused *refusedError
		if errors.As(err, &refused) && !refused.retry {
			log.Info("issue_report_refused", "class", class, "status", refused.status)
			return coord.IssueReportFailed, ""
		}
		log.Debug("issue_report_attempt_failed", "class", class, "attempt", attempt, "error", failureOf(err))
		if attempt >= maxAttempts {
			return coord.IssueReportFailed, ""
		}
		wait := backoff.Jitter(backoff.Doubling(attempt, r.cfg.RetryBase, retryCeiling), retryJitter)
		if refused != nil && refused.after > wait {
			wait = min(refused.after, retryCeiling)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return coord.IssueReportFailed, ""
		case <-timer.C:
		}
	}
}

// failureOf names a failed attempt for the log: an HTTP client's error quotes
// the address it dialled and nothing of the report.
func failureOf(err error) string {
	var refused *refusedError
	if errors.As(err, &refused) {
		return fmt.Sprintf("the relay answered %d", refused.status)
	}
	return err.Error()
}

// quietly is the guard every entry point stands behind: a report is never worth
// a turn, so a defect in this package ends in a log line.
func quietly(where string) {
	if p := recover(); p != nil {
		log.Error("issue_report_panicked", "where", where, "panic", p)
	}
}
