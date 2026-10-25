package code

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RETENTION PASS: what removes a retired pack's row once its grace has
// passed, and a deleted repository for good once it has been deleted for
// longer than the company's code.deleted_retention_days.
//
// It is the BODY of the code-retention duty. The duty — a fleet singleton,
// its lease and its tick ([RetentionInterval]) — is the engine's, because
// only the engine holds the fleet's leases and the current epoch. What this
// owns is what a pass works on, in what order, how much of the write lane it
// takes, and when a purged name is handed on.
//
// # What a pass works on
//
// The RETIRED PACKS first: every repository holding a row of
// code_packs_retired whose retired_at the pass's clock puts past
// [RetireGrace] is swept ([Store.RetireSweep]) — one record removing up to
// [RetireSweepRows] of its rows, the repository stepped again while rows past
// the grace are left. Sweeps go first because each is cheap and every hour a
// retired row outlives its grace is an object the collector cannot take;
// purges keep their order behind them. The listing is a page at a time over
// its own index, oldest retirement first.
//
// Then every PURGING repository, whatever the retention: a purge once begun
// is always finished, because its rows are already partly gone, its name is
// held ([ErrPurging]) and nothing restores it — a longer retention saves a
// repository only while it is still merely deleted. Then every DELETED
// repository past the retention, oldest deletion first. Each is stepped until
// its purge is done ([Store.PurgeStep]): one record a step, decided inside its
// own snapshot from the repository's rows and bounded by [PurgePartRows] and
// [PurgePartBytes], so a step is the unit of progress and a purge is
// resumable at every one. The listing is read a page at a time, the head of
// the backlog the pass will reach, and only the columns it uses.
//
// # How much of the write lane
//
// A pass PACES ITSELF and STOPS ITSELF, over a sweep's steps and a purge's
// alike. Every step is a record EVERY data node applies on the replicated
// estate's one write lane, and the holder only ever waits for its own apply:
// taken back to back, a pass held that lane most of the time on every node,
// and a node applying slower than the holder queued the difference in front
// of its own pushes and reads. So after each step a pass waits twice as long
// as the step took ([RetentionLaneShare], a third), and it takes steps for
// [RetentionPassBudget] at most; the next pass resumes from the rows.
// limits.go states the arithmetic of both — what a node slower than the
// holder is left behind by, and how fast a backlog drains.
//
// # When a name is handed on
//
// A purge releases its name, and the epoch may declare it again: a
// repository re-added while it purged, which the reconcile could only wait
// out. [Retention.OnPurged] is handed the name — but only once THIS NODE's
// rows hold the purge, because what it does with the name is decided from
// those rows, and a purge whose last record resolved pending here still reads
// purging. A name the pass cannot hand on yet — its record not applied here
// within a read's budget, OnPurged refusing it, or the pass's context ending
// first — is HELD, reported in [RetentionReport.Unhanded], and handed on by
// the next pass that can. Held in this process: a duty that moves to another
// node, or a process that dies, leaves it to the next activation's reconcile.

// retentionPage is how many repositories one read of the purge listing names,
// and how many rows one read of the retired listing reads: a pass that steps
// one-step repositories at the measured 83 ms a part reaches about 240 of them
// in RetentionPassBudget at RetentionLaneShare, so a page is about what one
// pass consumes, read as one statement over an index.
const retentionPage = 256

// retentionIdlePerStep is how many times a step's own duration the pass
// waits after it: (1 − share) / share, two at a third — exact, as a constant.
const retentionIdlePerStep = (1 - RetentionLaneShare) / RetentionLaneShare

// Retention is the code-retention pass over the retired packs past their
// grace and the repositories purging or deleted past the current epoch's
// retention. One value serves every pass of one duty holder: it HOLDS,
// between its passes, the names it purged and could not yet hand on, so it is
// used by pointer and never copied.
type Retention struct {
	// Store takes the sweeps' and the purges' steps.
	Store *Store
	// Reader lists what is due and confirms what a purge left in this
	// node's rows.
	Reader *Reader
	// Retention is code.deleted_retention_days of the CURRENT epoch, read
	// once at the start of every pass — a revision that lengthens it saves
	// a repository that is still deleted, and changes nothing for one
	// already purging. Required: what a purge waits for is the company's
	// decision, never a default taken here.
	Retention func() time.Duration
	// OnPurged is handed the name of every repository purged, under the
	// pass's context, once this node's rows hold the purge — its last
	// record applied here, never merely durable on the log: the purge
	// released the name, and the epoch may declare it again. An error
	// keeps the name, and the next pass hands it on again. It must be safe
	// to call twice for one name, because a purge an earlier pass finished
	// is handed on again by a pass whose listing had not yet applied it.
	// Nil hands it to nobody.
	OnPurged func(ctx context.Context, name string) error
	// Now is the clock the pass judges a retired pack's grace and a
	// deletion's age by; nil is the wall clock.
	Now func() time.Time
	// Logger is where a purge is reported; nil is the package's.
	Logger *slog.Logger

	// clock and sleep measure and pace the steps: the monotonic wall
	// clock and a timer, or a test's (export_test.go).
	clock func() time.Time
	sleep func(context.Context, time.Duration) error
	// budget and page are RetentionPassBudget and retentionPage, or a
	// test's.
	budget time.Duration
	page   int

	// mu guards held.
	mu sync.Mutex
	// held is every name purged on the log and not yet handed on, by name.
	held map[string]heldName
}

// heldName is one purged name the pass has not yet handed on.
type heldName struct {
	id string
	// at is the position of the purge's last record, which this node had not
	// applied when the pass held the name; zero when it had, and only
	// OnPurged or an ended context kept the name.
	at statelog.Position
}

// RetentionReport is what one pass did.
type RetentionReport struct {
	// Sweeps counts the retire_sweep records the pass published, across
	// every repository: each removing up to RetireSweepRows of one
	// repository's retired pack rows past RetireGrace.
	Sweeps int `json:"sweeps"`
	// SweptRows counts the retired pack rows those records remove — each
	// step's decide's own count of the rows at or below its Through.
	SweptRows int `json:"swept_rows"`
	// Purged names every repository whose purge the pass saw through and
	// handed on — its last record applied in this node's rows and its name
	// taken by OnPurged — in that order. A name an earlier pass purged and
	// could not hand on is reported by the pass that does.
	Purged []string `json:"purged,omitempty"`
	// Steps counts the purge steps that answered, across every repository:
	// each a decision on one repository's rows that appended one record —
	// all but a step that found its repository already purged, by an
	// earlier pass whose last record this node's listing had not yet
	// applied, and wrote nothing.
	Steps int `json:"steps"`
	// Failed maps the name of every repository whose sweep or purge failed
	// in this pass to the failure's sentence — both, joined, where both did —
	// keyed by the repository's id where this node holds no row to name it
	// by. The pass went on past each, and the next pass asks again.
	Failed map[string]string `json:"failed,omitempty"`
	// Unhanded names every repository purged on the log that the pass HOLDS
	// at its end for a later pass to hand on: its purge's last record not
	// yet applied on this node, OnPurged refusing it, or the context ending
	// before it could be handed on.
	Unhanded []string `json:"unhanded,omitempty"`
	// Remain says the pass stopped at its budget with repositories still
	// due a sweep or a purge: the designed stop, which the next pass resumes
	// from.
	Remain bool `json:"remain,omitempty"`
}

// Pass hands on what an earlier pass purged and could not, sweeps every
// repository's retired pack rows past [RetireGrace], then steps what is due a
// purge — every purging repository, then every one deleted past the current
// retention, each oldest deletion first — until its purge is done, the
// pass's budget runs out, or the context ends, pacing every step to the
// pass's share of the write lane.
//
// A repository whose sweep or purge fails is reported in
// [RetentionReport.Failed] and the pass goes on to the next: one repository's
// refusal never holds back another's. A repository the listing named that is
// NO LONGER DUE when its step decides is not a failure either. The listing is
// a stale read of this node's rows and every step decides again inside its
// own snapshot, so a repository a later activation restored ([ErrNotDeleted])
// — or restored and deleted again, inside its retention once more
// ([ErrNotYet]) — after the listing read it is simply skipped, and so is one
// whose retired rows a sweep or a purge removed since.
//
// The pass's error is the listing's, a field it cannot work without, or the
// context's: when ctx ends the pass stops between steps and reports what it
// did beside ctx.Err(), and a repository it was purging is not counted a
// failure, because the context and not the repository is why it stopped.
// Running out of budget is no error: the report says Remain.
func (r *Retention) Pass(ctx context.Context) (report RetentionReport, err error) {
	switch {
	case r.Store == nil:
		return report, errors.New("code: a retention pass needs the lifecycle store that " +
			"takes its sweeps' and its purges' steps")
	case r.Reader == nil:
		return report, errors.New("code: a retention pass needs the reader that lists the " +
			"repositories due")
	case r.Retention == nil:
		return report, errors.New("code: a retention pass needs the current epoch's " +
			"code.deleted_retention_days")
	}
	retention := r.Retention()
	if retention <= 0 {
		return report, fmt.Errorf("%w: code.deleted_retention_days: %v is not a retention",
			ErrInvalid, retention)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	log := r.Logger
	if log == nil {
		log = logging.Get("code")
	}
	pace := r.pacer()
	defer func() { report.Unhanded = r.heldNames() }()

	if err := r.handOnHeld(ctx, &report, log); err != nil {
		return report, err
	}
	if stop, err := r.sweep(ctx, now, pace, &report); err != nil || stop {
		report.Remain = stop
		return report, err
	}
	page := cmp.Or(r.page, retentionPage)
	var after *dueRepo
	for {
		due, err := r.Reader.due(ctx, dueQuery{cutoff: now.Add(-retention), after: after, limit: page})
		if err != nil {
			return report, fmt.Errorf("code: list the repositories due a purge past %v: %w",
				retention, err)
		}
		for _, repo := range due {
			if r.holds(repo) {
				// ITS PURGE IS DONE ON THE LOG, and this node's stale
				// listing has not applied the end of it: a step would only
				// meet the record it already appended.
				continue
			}
			stop, err := r.purge(ctx, repo, retention, pace, &report, log)
			if err != nil || stop {
				report.Remain = stop
				return report, err
			}
		}
		if len(due) < page {
			return report, nil
		}
		after = &due[len(due)-1]
	}
}

// sweep steps every repository the retired listing names — once each, however
// many of its rows the listing reads — until nothing past the grace is left,
// the pass's budget runs out (stop) or the context ends (the error).
//
// The listing is a stale read and each step decides again inside its own
// snapshot, so a repository whose rows a sweep removed since the listing read
// them, or whose purge took them, is simply answered nothing.
func (r *Retention) sweep(ctx context.Context, now time.Time, pace *pacer,
	report *RetentionReport) (stop bool, err error) {

	q := retiredQuery{cutoff: now.Add(-RetireGrace), limit: cmp.Or(r.page, retentionPage)}
	stepped := map[string]bool{}
	for {
		page, err := r.Reader.retiredDue(ctx, q)
		if err != nil {
			return false, err
		}
		for _, id := range page.repos {
			if stepped[id] {
				continue
			}
			stepped[id] = true
			if stop, err := r.sweepRepo(ctx, id, now, pace, report); err != nil || stop {
				return stop, err
			}
		}
		if page.next == nil {
			return false, nil
		}
		q.after = page.next
	}
}

// sweepRepo steps one repository's sweep while rows past the grace are left
// beyond what the last record removed, a step fails, the pass's budget runs
// out (stop) or the context ends (the error).
//
// A STEP WHOSE RECORD RESOLVED PENDING ENDS THE REPOSITORY'S TURN: its rows
// are removed on the log and not yet in this node's, so a further step would
// decide from rows that still hold them, and wait for this node to apply what
// it already appended. The next pass sweeps what is left.
func (r *Retention) sweepRepo(ctx context.Context, id string, now time.Time, pace *pacer,
	report *RetentionReport) (stop bool, err error) {
	for {
		switch turn, err := pace.wait(ctx); {
		case err != nil:
			return false, err
		case !turn:
			return true, nil
		}
		began := pace.clock()
		res, err := r.Store.RetireSweep(ctx, id, now)
		pace.took(began)
		switch {
		case err != nil && ctx.Err() != nil:
			// THE CONTEXT, NOT THE REPOSITORY, as a purge step's.
			return false, ctx.Err()
		case err != nil:
			report.fail(r.nameOf(ctx, id), err.Error())
			return false, nil
		case res.Rows == 0:
			return false, nil
		}
		report.Sweeps++
		report.SweptRows += res.Rows
		if !res.More || !res.Held() {
			return false, nil
		}
	}
}

// nameOf is the name a failure of repository id's is reported by: the name
// this node's rows hold for it, or the id where they hold none — the report is
// keyed by name, as an operator reads it.
func (r *Retention) nameOf(ctx context.Context, id string) string {
	if name, found, err := r.Store.repoName(ctx, id); err == nil && found {
		return name
	}
	return id
}

// purge steps one repository until its purge is done, a step fails, the
// pass's budget runs out (stop) or the context ends (the error). A purge it
// completes is handed on.
func (r *Retention) purge(ctx context.Context, repo dueRepo, retention time.Duration, pace *pacer,
	report *RetentionReport, log *slog.Logger) (stop bool, err error) {
	steps := 0
	for {
		switch turn, err := pace.wait(ctx); {
		case err != nil:
			return false, err
		case !turn:
			return true, nil
		}
		began := pace.clock()
		res, err := r.Store.PurgeStep(ctx, repo.id, retention)
		pace.took(began)
		switch {
		case err != nil && ctx.Err() != nil:
			// THE CONTEXT, NOT THE REPOSITORY, is why it failed — a step
			// on an ended context refuses before it reads a row — so the
			// pass stops here, and the next one resumes this repository
			// from what its applied steps left.
			return false, ctx.Err()
		case errors.Is(err, ErrNotDeleted), errors.Is(err, ErrNotYet):
			log.DebugContext(ctx, "code_repo_purge_skipped", "name", repo.name, "id", repo.id,
				"reason", err.Error())
			return false, nil
		case err != nil:
			report.fail(repo.name, err.Error())
			return false, nil
		}
		steps++
		report.Steps++
		if res.Done {
			r.purged(ctx, repo, res, steps, report, log)
			return false, nil
		}
	}
}

// purged hands on a repository whose purge is done on the log — once this
// node's rows hold it. A last record that resolved pending here is read
// again at its own position, waiting the read's budget for this node to
// apply it; one still out of reach is held for the next pass.
func (r *Retention) purged(ctx context.Context, repo dueRepo, res PurgeResult, steps int,
	report *RetentionReport, log *slog.Logger) {
	if !res.Held() {
		purged, err := r.Reader.purgedAt(ctx, repo.id, res.Position)
		switch {
		case err != nil:
			r.hold(repo.name, heldName{id: repo.id, at: res.Position})
			log.WarnContext(ctx, "code_repo_purge_unapplied", "name", repo.name, "id", repo.id,
				"position", res.Position.String(), "error", err.Error())
			return
		case !purged:
			report.fail(repo.name, fmt.Sprintf("the purge's last record, at %s, removed nothing "+
				"in this node's rows; the next pass steps it again", res.Position))
			return
		}
	}
	log.InfoContext(ctx, "code_repo_purged", "name", repo.name, "id", repo.id, "steps", steps)
	r.handOn(ctx, repo.name, heldName{id: repo.id}, report, log)
}

// handOnHeld hands on what earlier passes purged and could not, oldest
// purge first. A name whose purge this node has still not applied stops the
// walk, since every later one is further along the same log; a name whose
// purge removed nothing here is forgotten, because its repository is listed
// again and stepped again. Only the context's ending is an error.
func (r *Retention) handOnHeld(ctx context.Context, report *RetentionReport, log *slog.Logger) error {
	r.mu.Lock()
	held := maps.Clone(r.held)
	r.mu.Unlock()
	names := slices.Collect(maps.Keys(held))
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(held[a].at.Packed(), held[b].at.Packed()), cmp.Compare(a, b))
	})
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		h := held[name]
		if !h.at.IsZero() {
			purged, err := r.Reader.purgedAt(ctx, h.id, h.at)
			var refused *statelog.Refused
			switch {
			case err != nil && ctx.Err() != nil:
				return ctx.Err()
			case errors.As(err, &refused) && refused.Code == statelog.RefuseBehind:
				log.DebugContext(ctx, "code_repo_purge_unapplied", "name", name, "id", h.id,
					"position", h.at.String(), "error", err.Error())
				return nil
			case err != nil:
				log.WarnContext(ctx, "code_repo_purge_unapplied", "name", name, "id", h.id,
					"position", h.at.String(), "error", err.Error())
				continue
			case !purged:
				r.forget(name)
				report.fail(name, fmt.Sprintf("the purge's last record, at %s, removed nothing "+
					"in this node's rows; the next pass steps it again", h.at))
				continue
			}
			log.InfoContext(ctx, "code_repo_purged", "name", name, "id", h.id, "steps", 0)
		}
		r.handOn(ctx, name, heldName{id: h.id}, report, log)
	}
	return nil
}

// handOn gives OnPurged a purged name this node's rows hold, and holds it
// for the next pass when OnPurged refuses it or the context has ended.
func (r *Retention) handOn(ctx context.Context, name string, h heldName, report *RetentionReport,
	log *slog.Logger) {
	if ctx.Err() != nil {
		r.hold(name, h)
		return
	}
	if r.OnPurged != nil {
		if err := r.OnPurged(ctx, name); err != nil {
			r.hold(name, h)
			log.WarnContext(ctx, "code_repo_purge_unhanded", "name", name, "id", h.id,
				"error", err.Error())
			return
		}
	}
	r.forget(name)
	report.Purged = append(report.Purged, name)
}

// hold keeps a purged name for the next pass.
func (r *Retention) hold(name string, h heldName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held == nil {
		r.held = map[string]heldName{}
	}
	r.held[name] = h
}

// holds reports a listed repository whose purge the pass already holds the
// name of.
func (r *Retention) holds(repo dueRepo) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.held[repo.name]
	return ok && h.id == repo.id
}

// forget drops a held name.
func (r *Retention) forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.held, name)
}

// heldNames is every name held, sorted.
func (r *Retention) heldNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.held) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(r.held))
}

// fail records one repository's failure in the report — beside the one it
// already holds for that name, since a repository's sweep and its purge can
// both fail in one pass, and either sentence alone would hide the other.
func (report *RetentionReport) fail(name, sentence string) {
	if report.Failed == nil {
		report.Failed = map[string]string{}
	}
	if held, ok := report.Failed[name]; ok {
		sentence = held + "; " + sentence
	}
	report.Failed[name] = sentence
}

// pacer spaces one pass's steps to its share of the write lane, and ends the
// pass at its budget.
type pacer struct {
	clock func() time.Time
	sleep func(context.Context, time.Duration) error
	// end is when the budget runs out; next is the earliest the next step
	// may begin — zero before the first.
	end, next time.Time
}

// pacer is the pacing of one pass, its budget starting now.
func (r *Retention) pacer() *pacer {
	p := &pacer{clock: r.clock, sleep: r.sleep}
	if p.clock == nil {
		p.clock = time.Now
	}
	if p.sleep == nil {
		p.sleep = sleepFor
	}
	p.end = p.clock().Add(cmp.Or(r.budget, RetentionPassBudget))
	return p
}

// wait waits for the next step's turn, and answers false — the pass's
// designed stop — when that turn would come at or after the budget's end,
// rather than wait out a turn it may not take. Its error is the context's.
func (p *pacer) wait(ctx context.Context) (bool, error) {
	now := p.clock()
	at := p.next
	if at.Before(now) {
		at = now
	}
	if !at.Before(p.end) {
		return false, nil
	}
	if d := at.Sub(now); d > 0 {
		if err := p.sleep(ctx, d); err != nil {
			return false, err
		}
	}
	return true, nil
}

// took records a step that began at began and has just answered: the next
// one waits retentionIdlePerStep times as long as this one took.
func (p *pacer) took(began time.Time) {
	ended := p.clock()
	p.next = ended.Add(time.Duration(float64(ended.Sub(began)) * retentionIdlePerStep))
}

// sleepFor waits d, or until ctx ends.
func sleepFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
