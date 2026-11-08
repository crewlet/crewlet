package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT WHOSE ACCESS CHANGED REVIEWS ITS OPEN WORK ONCE (ADR-0111): a seat
// is measured once applies go quiet, on admission and when a ledger write names
// it, and each change is recorded as its pending review and asked for once.

// accessReviewQuiet is how long a node waits after the last apply before it
// measures its seats: a person's run of saves is one review.
const accessReviewQuiet = 2 * time.Minute

// accessReviewRetryBase and accessReviewRetryCeiling pace a check that could
// not be made: from the quiet window, doubling to a quarter of an hour.
const (
	accessReviewRetryBase    = accessReviewQuiet
	accessReviewRetryCeiling = 15 * time.Minute
)

// accessReviews is one node's debounce of its seats' reviews.
type accessReviews struct {
	// check measures the seats named, or every seat held where all is set,
	// and answers the seats whose check could not be made.
	check func(ctx context.Context, handles []string, all bool) []string

	// after arms a timer and answers its stop: time.AfterFunc's outside a case.
	quiet time.Duration
	after func(d time.Duration, f func()) (stop func() bool)

	ctx context.Context

	mu      sync.Mutex
	timer   func() bool
	all     bool
	handles map[string]bool
	failed  int
	stopped bool

	// asked is the wake each seat was last asked for on this node.
	asked map[string]string
}

// newAccessReviews builds the debounce over check, armed with ctx.
func newAccessReviews(ctx context.Context,
	check func(ctx context.Context, handles []string, all bool) []string) *accessReviews {

	return &accessReviews{check: check, quiet: accessReviewQuiet, after: afterFunc,
		ctx: ctx, handles: map[string]bool{}, asked: map[string]string{}}
}

// afterFunc is time.AfterFunc answering its timer's stop.
func afterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// kick asks for a check of the seats named, or of every seat held when none
// is, after the quiet window, restarting it.
func (r *accessReviews) kick(handles ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if len(handles) == 0 {
		r.all = true
	}
	for _, h := range handles {
		r.handles[h] = true
	}
	r.armLocked(r.quiet)
}

// armLocked (re)starts the timer at d.
func (r *accessReviews) armLocked(d time.Duration) {
	if r.timer != nil {
		r.timer()
	}
	r.timer = r.after(d, r.fire)
}

// fire runs the check the window collected, and retries what it could not make.
func (r *accessReviews) fire() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	all, handles := r.all, make([]string, 0, len(r.handles))
	for h := range r.handles {
		handles = append(handles, h)
	}
	slices.Sort(handles)
	r.all, r.handles, r.timer = false, map[string]bool{}, nil
	r.mu.Unlock()

	unchecked := r.check(r.ctx, handles, all)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.ctx.Err() != nil {
		return
	}
	if len(unchecked) == 0 {
		r.failed = 0
		return
	}
	r.failed++
	for _, h := range unchecked {
		r.handles[h] = true
	}
	if r.timer == nil {
		r.armLocked(backoff.Doubling(r.failed, accessReviewRetryBase, accessReviewRetryCeiling))
	}
}

// seen reports whether seat handle was already asked for wake key here, and
// records it when it was not. A nil debounce has seen nothing.
func (r *accessReviews) seen(handle, key string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.asked[handle] == key {
		return true
	}
	r.asked[handle] = key
	return false
}

// forget drops what seat handle was last asked for, so a failed publish is
// asked for again.
func (r *accessReviews) forget(handle, key string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.asked[handle] == key {
		delete(r.asked, handle)
	}
}

// stop ends the debounce: a timer armed is stopped, and nothing arms another.
func (r *accessReviews) stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	if r.timer != nil {
		r.timer()
		r.timer = nil
	}
}

// kickAccessReviews asks for a check of the seats named, or of every seat this
// node holds when none is.
func (e *Engine) kickAccessReviews(handles ...string) {
	if r := e.accessReviews.Load(); r != nil {
		r.kick(handles...)
	}
}

// checkAccessReviews measures the seats named, or every seat this node holds
// where all is set, and answers the seats whose check could not be made.
func (e *Engine) checkAccessReviews(ctx context.Context, handles []string, all bool) []string {
	company := e.Company()
	if company == nil || company.Config == nil {
		return nil
	}
	if all {
		if e.node == nil {
			return nil
		}
		handles = e.node.Host().Held()
	}
	var unchecked []string
	for _, handle := range handles {
		if company.Org.AgentSeatByHandle(handle) == nil {
			continue
		}
		if err := e.reviewSeat(ctx, company, handle, e.reviewSeams()); err != nil {
			log.WarnContext(ctx, "access_review_unchecked", "agent", handle, "error", err.Error(),
				"detail", "the seat's access review could not be measured or asked for; it is checked again shortly")
			unchecked = append(unchecked, handle)
		}
	}
	return unchecked
}

// pendingStore is where each seat's pending review is kept.
type pendingStore interface {
	AccessReview(ctx context.Context, handle string) ([]byte, bool, error)
	PutAccessReview(ctx context.Context, handle string, value []byte) error
	DeleteAccessReview(ctx context.Context, handle string) error
}

// reviewSeams are what a review check reads and writes through: the engine's
// own, and a case's.
type reviewSeams struct {
	work    func(ctx context.Context, company *Company, handle string) (accessreview.Work, error)
	publish func(ctx context.Context, req accessreview.Request) error
	pending pendingStore
	now     func() time.Time
}

// reviewSeams are the engine's own.
func (e *Engine) reviewSeams() reviewSeams {
	s := reviewSeams{work: e.openWork, publish: e.publishAccessReview,
		now: func() time.Time { return time.Now().UTC() }}
	if e.backends != nil && e.backends.Fleet != nil {
		s.pending = e.backends.Fleet
	}
	return s
}

// reviewSeat records seat handle's access change as its pending review and asks
// for the review where it has open work; a change undone before any turn was
// told it drops its record.
func (e *Engine) reviewSeat(ctx context.Context, company *Company, handle string, seams reviewSeams) error {
	if seams.pending == nil {
		return errors.New("engine: this node holds no coordination store to keep access reviews in")
	}
	m, owed, err := e.measureAccess(ctx, company, handle)
	if err != nil {
		return err
	}
	pending, found, err := readPending(ctx, seams.pending, handle)
	if err != nil {
		return err
	}
	if !owed {
		if found && pending.ToldTurn == "" {
			return seams.pending.DeleteAccessReview(ctx, handle)
		}
		return nil
	}
	if !found || !pending.Same(m.base.Turn, m.digest) {
		pending = accessreview.Pending{Baseline: m.base.Turn, BaselineAt: m.base.At,
			Digest: m.digest, ChangedAt: seams.now(), Lines: m.personLines}
		if err = writePending(ctx, seams.pending, handle, pending); err != nil {
			return err
		}
	}
	work, err := seams.work(ctx, company, handle)
	if err != nil || work.Empty() {
		return err
	}
	req := accessreview.RequestFor(handle, pending, m.seatLines, work.Count())
	reviews, key := e.accessReviews.Load(), accessreview.WakeID(req).String()
	if reviews.seen(handle, key) {
		return nil
	}
	if err := seams.publish(ctx, req); err != nil {
		reviews.forget(handle, key)
		return err
	}
	log.InfoContext(ctx, "access_review_asked", "agent", handle, "changes", len(req.Lines),
		"open", req.Open, "baseline", req.Baseline)
	return nil
}

// accessMeasure is a seat's access now against what it was last told.
type accessMeasure struct {
	base                   *accessdomain.ToldRow
	digest                 string
	seatLines, personLines []string
}

// measureAccess measures seat handle; false where it owes no review: told as it
// stands, with no baseline, or a difference the closed list does not count.
func (e *Engine) measureAccess(ctx context.Context, company *Company, handle string) (
	accessMeasure, bool, error) {

	in, err := e.capabilityInput(ctx, company, handle)
	if err != nil {
		return accessMeasure{}, false, err
	}
	reads, ok := e.accessReads()
	if !ok {
		return accessMeasure{}, false, nil
	}
	recent, err := reads.Recent(ctx, handle, time.Now().UTC(), accessdomain.DispatchRead())
	if err != nil {
		return accessMeasure{}, false, err
	}
	m, owed := measured(capability.Take(in), recent)
	return m, owed, nil
}

// measured is snap against recent's baseline, false where nothing is owed.
func measured(snap capability.Snapshot, recent accessdomain.Recent) (accessMeasure, bool) {
	base := recent.Baseline
	switch {
	case !recent.Complete, base == nil, base.Digest == snap.Digest(),
		bytes.Equal(base.Snapshot, toldWithheld):
		return accessMeasure{}, false
	}
	old, err := capability.Parse(base.Snapshot)
	if err != nil {
		return accessMeasure{}, false
	}
	seat := linesOf(capability.DiffFor(old, snap, capability.ForSeat))
	if len(seat) == 0 {
		return accessMeasure{}, false
	}
	return accessMeasure{base: base, digest: snap.Digest(), seatLines: seat,
		personLines: linesOf(capability.DiffFor(old, snap, capability.ForPerson))}, true
}

// linesOf is changes' lines.
func linesOf(changes []capability.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Line)
	}
	return out
}

// readPending is seat handle's pending review, false where it has none.
func readPending(ctx context.Context, store pendingStore, handle string) (accessreview.Pending, bool, error) {
	raw, found, err := store.AccessReview(ctx, handle)
	if err != nil || !found {
		return accessreview.Pending{}, false, err
	}
	p, err := accessreview.DecodePending(raw)
	return p, err == nil, err
}

// writePending records seat handle's pending review.
func writePending(ctx context.Context, store pendingStore, handle string, p accessreview.Pending) error {
	raw, err := accessreview.EncodePending(p)
	if err != nil {
		return err
	}
	return store.PutAccessReview(ctx, handle, raw)
}

// PendingAccessReview is seat handle's access change waiting on its review, the
// one read an Inbox makes per asking seat.
func (e *Engine) PendingAccessReview(ctx context.Context, handle string) (accessreview.Pending, bool, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return accessreview.Pending{}, false, nil
	}
	return readPending(ctx, e.backends.Fleet, handle)
}

// noteReviewTold marks seat handle's pending review as told to turn, once its
// told record of digest landed: questions raised from then on are not stale.
func (e *Engine) noteReviewTold(ctx context.Context, handle, turn, digest string) {
	store := e.reviewSeams().pending
	if store == nil {
		return
	}
	p, found, err := readPending(ctx, store, handle)
	if err != nil || !found || p.Digest != digest || p.ToldTurn != "" {
		logReviewErr(ctx, handle, err)
		return
	}
	p.ToldTurn, p.ToldAt = turn, time.Now().UTC()
	logReviewErr(ctx, handle, writePending(ctx, store, handle, p))
}

// endReviewOnTurnEnd drops seat handle's pending review once the turn told it
// ends: the seat has reviewed the change, and the Inbox notes it no more.
func (e *Engine) endReviewOnTurnEnd(ctx context.Context, handle, turn string) {
	store := e.reviewSeams().pending
	if store == nil || handle == "" {
		return
	}
	p, found, err := readPending(ctx, store, handle)
	if err != nil || !found || p.ToldTurn != turn {
		logReviewErr(ctx, handle, err)
		return
	}
	logReviewErr(ctx, handle, store.DeleteAccessReview(ctx, handle))
}

// logReviewErr says a pending review record that could not be kept current.
func logReviewErr(ctx context.Context, handle string, err error) {
	if err != nil {
		log.WarnContext(ctx, "access_review_record_unkept", "agent", handle, "error", err.Error(),
			"detail", "the seat's pending access review was not updated; the next check measures it again")
	}
}

// publishAccessReview sends one review as the inbound delivery accessreview
// reads.
func (e *Engine) publishAccessReview(ctx context.Context, req accessreview.Request) error {
	if e.backends == nil || e.backends.Queue == nil {
		return errors.New("engine: this node has no queue to ask for an access review on")
	}
	w, err := accessreview.Encode(req)
	if err != nil {
		return err
	}
	ev := events.New(w, events.NewTrace())
	ev.Source = accessreview.Source
	return e.backends.Queue.Publish(ctx, topics.NotificationsInbound, ev)
}

// AccessReviewSettled reports whether a review of seat handle has nothing left
// to tell: its access is what it was last told. Fail open, to the turn.
func (e *Engine) AccessReviewSettled(ctx context.Context, handle string) bool {
	return e.reviewSettled(ctx, e.Company(), handle)
}

// reviewSettled is [Engine.AccessReviewSettled] on company.
func (e *Engine) reviewSettled(ctx context.Context, company *Company, handle string) bool {
	if company == nil || company.Config == nil || company.Org.AgentSeatByHandle(handle) == nil {
		return true
	}
	in, err := e.capabilityInput(ctx, company, handle)
	if err != nil {
		return false
	}
	reads, ok := e.accessReads()
	if !ok {
		return false
	}
	recent, err := reads.Recent(ctx, handle, time.Now().UTC(), accessdomain.DispatchRead())
	if err != nil || !recent.Complete {
		return false
	}
	return recent.Baseline != nil && recent.Baseline.Digest == capability.Take(in).Digest()
}

// reviewingLedger is the seat access ledger kicking a seat's review on every
// write naming it: access a sync or a vendor step changes is measured too.
type reviewingLedger struct {
	seataccess.Ledger
	kick func(handles ...string)
}

// Put implements [seataccess.Ledger].
func (l reviewingLedger) Put(ctx context.Context, rec seataccess.Record) error {
	err := l.Ledger.Put(ctx, rec)
	if err == nil {
		l.kick(rec.Handle)
	}
	return err
}

// Drop implements [seataccess.Ledger].
func (l reviewingLedger) Drop(ctx context.Context, kind integration.Kind, handle string) error {
	err := l.Ledger.Drop(ctx, kind, handle)
	if err == nil {
		l.kick(handle)
	}
	return err
}

// openWork is seat handle's open work an access review covers, read at `stale`:
// questions it asked still open, its runs waiting on a person, and its work
// items in progress or blocked.
func (e *Engine) openWork(ctx context.Context, company *Company, handle string) (accessreview.Work, error) {
	var w accessreview.Work
	if reads := OperatorNative(e); reads.Tracker {
		now, loc := time.Now().UTC(), company.Config.Location()
		asks, err := workPage(ctx, reads.Work, now, loc, worklet.MapParams{"asked_by": handle})
		if err != nil {
			return accessreview.Work{}, err
		}
		for _, row := range asks.Rows {
			w.Asks = append(w.Asks, row.Key)
		}
		w.MoreAsks = max(asks.TotalHint-len(asks.Rows), 0)
		assigned, err := workPage(ctx, reads.Work, now, loc, worklet.MapParams{"assignee": handle,
			"any": `[{"status_group":"` + string(worklet.GroupActive) + `"},{"blocked":true}]`})
		if err != nil {
			return accessreview.Work{}, err
		}
		for _, row := range assigned.Rows {
			w.Items = append(w.Items, accessreview.Item{Key: row.Key, Status: string(row.Status),
				Blocked: row.Blocked})
		}
		w.MoreItems = max(assigned.TotalHint-len(assigned.Rows), 0)
	}
	if rt := e.sandbox.Load(); rt != nil && rt.pending != nil {
		runs, err := rt.pending.ListActiveForSeat(ctx, handle)
		if err != nil {
			return accessreview.Work{}, fmt.Errorf("engine: read %s's executor runs: %w", handle, err)
		}
		for _, run := range waitingRuns(runs) {
			if len(w.Runs) == accessreview.MaxListed {
				w.MoreRuns++
				continue
			}
			w.Runs = append(w.Runs, waitingRun(run))
		}
	}
	return w, nil
}

// workPage is one page of the work items a query names, at most
// [accessreview.MaxListed], over the whole workspace and at `stale`.
func workPage(ctx context.Context, work interface {
	Tasks(context.Context, worklet.Query, time.Time) (worklet.Answer, error)
}, now time.Time, loc *time.Location, params worklet.MapParams) (worklet.Answer, error) {
	params["container"] = "workspace"
	params["limit"] = accessreview.MaxListed
	q, err := worklet.ParseQuery(params, now, loc)
	if err != nil {
		return worklet.Answer{}, err
	}
	q.Level = statelog.ReadStale
	return work.Tasks(ctx, q, now)
}

// waitingRuns are the runs that wait on a person, not ended, oldest first.
func waitingRuns(runs []sandbox.PendingRun) []sandbox.PendingRun {
	var out []sandbox.PendingRun
	for _, run := range runs {
		if run.Ending != nil || !slices.Contains(sandbox.Awaiting, run.Status) ||
			(run.HeldCall == nil && run.Question == "") {
			continue
		}
		out = append(out, run)
	}
	slices.SortStableFunc(out, func(a, b sandbox.PendingRun) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// waitingRun is one waiting run as a review names it.
func waitingRun(run sandbox.PendingRun) accessreview.Run {
	out := accessreview.Run{Turn: run.TurnID}
	if held := run.HeldCall; held != nil {
		out.Tool, out.Server = held.Tool, held.Server
	}
	if item := run.WorkItem; item != nil && item.Backend == types.WorkWorklet {
		out.Task = item.Key
	}
	return out
}

// withdrawDeps is what withdraw_ask adds to a seat's tracker tools: the
// tracker's writer resolving a question as its author, and the answer desk a
// run's withdrawal is delivered by.
func (e *Engine) withdrawDeps() builtin.WithdrawDeps {
	var out builtin.WithdrawDeps
	if halves, ok := e.trackerHalves(); ok {
		out.Resolver = func(actor builtin.Actor) builtin.AskResolver { return halves.as(actor) }
	}
	if e.backends != nil && e.backends.Fleet != nil && e.backends.Queue != nil {
		out.Desk = sandbox.AnswerDesk{Pending: sandbox.NewCoordStore(e.backends.Fleet),
			Queue: e.backends.Queue, Seal: e.AnswerSeal()}
	}
	return out
}

// draftReviews is, for each agent seat whose access a draft changes by the
// closed list, how many of its distinct open items it will review; the draft is
// written into a copy of cfg by the save's own edits.
func (e *Engine) draftReviews(ctx context.Context, cfg *config.Company, roster *org.Organization,
	current seataccess.Answer, draft seataccess.Draft,
	openWork func(context.Context, *Company, string) (accessreview.Work, error)) ([]seataccess.SeatReview, error) {

	company := e.Company()
	if company == nil || cfg == nil || roster == nil || len(draft.Seats) == 0 {
		return nil, nil
	}
	next, nextOrg, err := draftCompany(cfg, current, draft)
	if err != nil {
		return nil, err
	}
	var out []seataccess.SeatReview
	for _, handle := range sortedHandles(draft.Seats) {
		if roster.AgentSeatByHandle(handle) == nil {
			continue
		}
		was, err := e.capabilityInputOf(ctx, cfg, roster, handle)
		if err != nil {
			return nil, err
		}
		will, err := e.capabilityInputOf(ctx, next, nextOrg, handle)
		if err != nil {
			return nil, err
		}
		changes := capability.DiffFor(capability.Take(was), capability.Take(will), capability.ForPerson)
		if len(changes) == 0 {
			continue
		}
		work, err := openWork(ctx, company, handle)
		if err != nil {
			return nil, err
		}
		if work.Empty() {
			continue
		}
		out = append(out, seataccess.SeatReview{Handle: handle, Open: work.Count(),
			Changes: linesOf(changes[:min(len(changes), capability.MaxLines)])})
	}
	return out, nil
}

// draftCompany is a copy of cfg with the save's edits of draft against current,
// and the chart it resolves to.
func draftCompany(cfg *config.Company, current seataccess.Answer, draft seataccess.Draft) (
	*config.Company, *org.Organization, error) {

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: copy the document to measure a draft on: %w", err)
	}
	next, err := config.DecodeCompany(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: copy the document to measure a draft on: %w", err)
	}
	if err = configapi.EditSeats(next, setupapi.DraftSeatEdits(current, draft.Seats)); err != nil {
		return nil, nil, err
	}
	o, err := next.Organization()
	if err != nil {
		return nil, nil, fmt.Errorf("engine: resolve the draft's chart: %w", err)
	}
	return next, o, nil
}

var _ pendingStore = (coord.Fleet)(nil)
