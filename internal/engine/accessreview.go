package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT WHOSE ACCESS CHANGED REVIEWS ITS OPEN WORK ONCE (ADR-0111).
//
// A revision that changes a seat's access is told to the seat at its next turn
// (ADR-0104, capability.go), and a seat whose open work waits on a person has
// no next turn until somebody answers it. So the node holding a seat, once an
// apply has gone quiet, measures the seat's access against what the seat was
// last told, and where the closed change list counts a difference and the seat
// has open work, it asks for one review turn: a delivery on the inbound topic
// the notify spine routes to the seat (internal/accessreview).
//
// LEVEL, NOT EDGE. What a check reads is the company document, the seat access
// ledger and the access log, every one durable, so a check this node did not
// live to make is the seat's next holder's, made on admission; a check whose
// publish the broker refused is made again on the retry's spacing; and a check
// that finds the seat already told (its review, or any turn that told the change
// first, ran) asks for nothing.

// accessReviewQuiet is how long a node waits after the last apply before it
// measures the seats it holds: TWO MINUTES, so a person's run of saves on an
// agent's access (a level, then a host, then a permission, each a revision of
// its own, seconds apart) is one review measured from the last of them, and
// the review still starts while the person is at the screen they saved from.
const accessReviewQuiet = 2 * time.Minute

// accessReviewRetryBase and accessReviewRetryCeiling pace a check that could
// not be made (a store or the broker unreachable): from the quiet window's own
// length, doubling to a quarter of an hour, never faster than the quiet window
// the review waits out anyway.
const (
	accessReviewRetryBase    = accessReviewQuiet
	accessReviewRetryCeiling = 15 * time.Minute
)

// accessReviews is one node's debounce of its seats' reviews.
type accessReviews struct {
	// check measures the seats named, or every seat this node holds where
	// all is set, and reports the seats whose check could not be made.
	check func(ctx context.Context, handles []string, all bool) []string

	// quiet is the window an apply waits out; after arms a timer, and is
	// time.AfterFunc outside a case.
	quiet time.Duration
	after func(time.Duration, func()) *time.Timer

	// ctx is the node's own, which every check runs under.
	ctx context.Context

	mu      sync.Mutex
	timer   *time.Timer
	all     bool
	handles map[string]bool
	failed  int
	stopped bool

	// asked is the review each seat was last asked for, keyed by handle:
	// an apply that changes nothing further asks for nothing again.
	asked map[string]string
}

// newAccessReviews builds the debounce over check, armed with ctx.
func newAccessReviews(ctx context.Context,
	check func(ctx context.Context, handles []string, all bool) []string) *accessReviews {

	return &accessReviews{check: check, quiet: accessReviewQuiet, after: time.AfterFunc,
		ctx: ctx, handles: map[string]bool{}, asked: map[string]string{}}
}

// kick asks for a check after the quiet window, of the seats named, or of
// every seat this node holds when none is named, and restarts the window: a
// second apply inside it is measured with the first.
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
		r.timer.Stop()
	}
	r.timer = r.after(d, r.fire)
}

// fire runs the check the window collected, and arms a retry for the seats
// whose check could not be made.
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

// seen reports whether seat handle was already asked for review key, and
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

// forget drops what seat handle was last asked for, so a publish that failed
// is asked for again.
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
		r.timer.Stop()
		r.timer = nil
	}
}

// kickAccessReviews asks this node's debounce for a check of the seats named,
// or of every seat it holds when none is named.
func (e *Engine) kickAccessReviews(handles ...string) {
	if r := e.accessReviews.Load(); r != nil {
		r.kick(handles...)
	}
}

// checkAccessReviews measures the seats named, or every seat this node holds
// where all is set, and asks for each review a seat owes. It answers the seats
// whose check could not be made, for the retry.
func (e *Engine) checkAccessReviews(ctx context.Context, handles []string, all bool) []string {
	company := e.Company()
	if company == nil || company.Config == nil || e.node == nil {
		return nil
	}
	if all {
		handles = e.node.Host().Held()
	}
	var unchecked []string
	for _, handle := range handles {
		if company.Org.AgentSeatByHandle(handle) == nil {
			continue
		}
		if _, held := e.node.Host().MayStart(handle); !held {
			// THE SEAT'S HOLDER CHECKS IT, on its admission: this node
			// would be asking for another node's seat.
			continue
		}
		if err := e.reviewSeat(ctx, company, handle); err != nil {
			log.WarnContext(ctx, "access_review_unchecked", "agent", handle, "error", err.Error(),
				"detail", "the seat's access could not be measured against what it was told, "+
					"or its review could not be published; it is checked again shortly")
			unchecked = append(unchecked, handle)
		}
	}
	return unchecked
}

// reviewSeat asks for seat handle's review where it owes one: its access
// differs, by the closed change list, from what it was last told, and it has
// open work.
func (e *Engine) reviewSeat(ctx context.Context, company *Company, handle string) error {
	req, owed, err := e.reviewOwed(ctx, company, handle)
	if err != nil || !owed {
		return err
	}
	reviews := e.accessReviews.Load()
	key := accessreview.WakeID(req).String()
	if reviews.seen(handle, key) {
		return nil
	}
	work, err := e.openWork(ctx, company, handle)
	if err != nil {
		reviews.forget(handle, key)
		return err
	}
	if work.Empty() {
		// NOTHING TO REVIEW: the seat is told the change at its next turn,
		// as every change is, and a turn now would cost a turn nobody needs.
		log.DebugContext(ctx, "access_review_not_needed", "agent", handle,
			"detail", "the seat's access changed and it has no open work to review")
		return nil
	}
	req.Open, req.At = work.Count(), time.Now().UTC()
	if err := e.publishAccessReview(ctx, req); err != nil {
		reviews.forget(handle, key)
		return err
	}
	log.InfoContext(ctx, "access_review_asked", "agent", handle, "changes", len(req.Lines),
		"open", req.Open, "baseline", req.Baseline)
	return nil
}

// reviewOwed is the review seat handle owes, false where it owes none: its
// snapshot is what it was last told, or the difference is none the closed list
// counts, or there is no baseline to measure from (a seat's first turn is told
// its access as a baseline and no change).
func (e *Engine) reviewOwed(ctx context.Context, company *Company, handle string) (
	accessreview.Request, bool, error) {

	in, err := e.capabilityInput(ctx, company, handle)
	if err != nil {
		return accessreview.Request{}, false, err
	}
	reads, ok := e.accessReads()
	if !ok {
		return accessreview.Request{}, false, nil
	}
	recent, err := reads.Recent(ctx, handle, time.Now().UTC(), accessdomain.DispatchRead())
	if err != nil {
		return accessreview.Request{}, false, err
	}
	req, owed := reviewOf(capability.Take(in), recent, handle)
	return req, owed, nil
}

// reviewOf is the review a seat whose snapshot is snap owes, given what it was
// last told: the change lines the closed list counts from its baseline, false
// where there are none or no baseline the difference can be measured from.
func reviewOf(snap capability.Snapshot, recent accessdomain.Recent, handle string) (accessreview.Request, bool) {
	base := recent.Baseline
	switch {
	case !recent.Complete, base == nil, base.Digest == snap.Digest(),
		bytes.Equal(base.Snapshot, toldWithheld):
		return accessreview.Request{}, false
	}
	old, err := capability.Parse(base.Snapshot)
	if err != nil {
		return accessreview.Request{}, false
	}
	changes := capability.Diff(old, snap)
	if len(changes) == 0 {
		return accessreview.Request{}, false
	}
	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		lines = append(lines, c.Line)
	}
	return accessreview.Request{Handle: handle, Baseline: base.Turn, Digest: snap.Digest(),
		Lines: lines}, true
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
// to tell: its access is what it was last told, because its review, or another
// turn that told the change first and asked for the review with it, already
// ran. The dispatcher acks such a wake without a turn.
//
// FAIL OPEN, TO THE TURN: a store this node cannot read says nothing settled,
// and the review runs; a turn told no change is told its access alone.
func (e *Engine) AccessReviewSettled(ctx context.Context, handle string) bool {
	company := e.Company()
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

// openWork is seat handle's open work an access review covers: the work items
// carrying a question it asked that is still open, its executor runs waiting
// on a person, and the work items assigned to it that are in progress or
// blocked.
//
// AT `stale`: a list the seat checks item by item with its own tools, and a
// count a person reads beside a save, neither of which a few records of lag
// changes the meaning of.
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

// waitingRuns are the runs that wait on a person: a question or a held call,
// not ended, oldest first.
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
// tracker's writer resolving a question as its author, where the company runs
// Worklet, and the answer desk a run's withdrawal is delivered by, where the
// node holds a coordination store and a queue.
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
