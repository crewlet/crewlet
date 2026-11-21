package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE RETENTION PASS removes what no record of a repository's own will: a
// retired pack's row past its grace, an idle repository's check rows past
// CheckHistory and queue history past QueueHistory, a repository deleted past
// code.deleted_retention_days, a renamed repository's old name once its
// redirect has ended, a scratch repository idle past code.scratch.idle_days
// and a seat's idle branch — never one a live turn holds. It is the BODY of
// the code-retention duty: the duty's singleton lease and tick
// ([RetentionInterval]) are the engine's, which alone holds the fleet's
// leases and the epoch. This owns what a pass works on, in what order, how
// much of the write lane it takes, and when a purged name is handed on.

// # What a pass works on, in order
//
// RETIRED PACKS first ([Store.RetireSweep]): each sweep is cheap, and every
// hour a row outlives [RetireGrace] is an object the collector cannot take.
// Then idle CHECKS ([Store.ChecksSweep]) and QUEUES ([Store.QueueSweep]): a
// repository whose checks keep ending or whose queues keep working removes
// its own history, so these list only one past the history by a grace
// ([Retention.sweepChecks]). A sweep step is one bounded record, repeated
// while rows past the horizon are left; each listing is a keyset page, the
// queues' over the repositories' key and the others' over their own index.

// Then every PURGING repository, whatever the retention — a begun purge has
// removed rows and holds its name ([ErrPurging]), so it is always finished —
// then every DELETED one past the retention, oldest deletion first, each
// stepped ([Store.PurgeStep]) until done: one record a step, decided in its
// own snapshot and bounded by [PurgePartRows] and [PurgePartBytes], so a
// purge resumes at any step. Then old names whose redirect ended
// ([RedirectWindow], [Store.ReleaseName]); then idle scratch repositories
// ([Store.ExpireScratch], restorable as any deleted one) and, LAST, idle seat
// branches ([ViaExpiry], retention_expiry.go) — each unless a live turn holds
// it, read through [Retention.Holds] after its page is fixed
// ([Retention.expireScratch] has why); a hold that cannot be read expires
// nothing.

// # How much of the write lane
//
// A pass PACES and STOPS ITSELF: every step is a record every data node
// applies on the estate's one write lane while the holder waits only for its
// own apply, so back to back a pass would hold that lane on every node and a
// slower node would queue the difference ahead of its own pushes and reads.
// After each step it waits twice the step's duration ([RetentionLaneShare]),
// and it takes steps for [RetentionPassBudget] at most, sweeps first; the
// next pass resumes from the rows. limits.go has the arithmetic.

// # When a name is handed on
//
// A purge releases its name, which the epoch may declare again.
// [Retention.OnPurged] gets it only once THIS NODE's rows hold the purge,
// since what it does is decided from those rows and a last record resolved
// pending here still reads purging. A name not yet handed on — unapplied
// within a read's budget, refused, or the context ended — is HELD in this
// process ([RetentionReport.Unhanded]) for the next pass; a duty that moves
// or a process that dies leaves it to the next activation's reconcile.

// retentionPage is how many entries one read of a listing names, each at
// most one step: about what one pass consumes of any listing — some 240
// one-step purges at the measured 83 ms a part in RetentionPassBudget at
// RetentionLaneShare, some 370 retiring records at Gitlet's design point, a
// hundred-odd checks repositories (CheckSweepGrace; limits.go) — read as one
// statement over an index. A burst of sweeps reads further pages, sixteen at
// most a pass (about four thousand steps of a few rows).
const retentionPage = 256

// retentionIdlePerStep is how many times a step's own duration the pass
// waits after it: (1 − share) / share, two at a third — exact, as a constant.
const retentionIdlePerStep = (1 - RetentionLaneShare) / RetentionLaneShare

// Retention is the code-retention pass (this file's header). One value
// serves every pass of one duty holder: it HOLDS, between passes, the names
// it purged and could not yet hand on, so it is used by pointer, never copied.
type Retention struct {
	// Store takes the pass's steps.
	Store *Store
	// Reader lists what is due and confirms what a purge left in this
	// node's rows.
	Reader *Reader
	// Retention is code.deleted_retention_days of the CURRENT epoch, read
	// once a pass: lengthening it saves a repository still deleted, never one
	// already purging. Required: what a purge waits for is the company's
	// decision, never a default taken here.
	Retention func() time.Duration
	// OnPurged is handed every purged name, under the pass's context, once
	// this node has applied the purge's last record, since the epoch may
	// declare the name again. An error keeps the name for the next pass. It
	// must be safe to call twice for one name: a pass whose listing had not
	// applied an earlier pass's purge hands it on again. Nil hands it to
	// nobody.
	OnPurged func(ctx context.Context, name string) error
	// Policy is the CURRENT epoch's code.scratch ([ScratchOf]), read once at
	// the scratch expiry's start: how long a scratch repository may sit idle.
	// Required, for Retention's reason.
	Policy func() LifecyclePolicy
	// Holds answers which of keys a LIVE TURN holds (a scratch repository's
	// key is its id; the engine's codeHolds counts every run row, whatever its
	// status, and every running turn), asked once a page of due keys AFTER
	// the page and its position are read. An error is an UNKNOWN, on which
	// nothing expires. Required: an expiry that could not ask would delete a
	// repository a parked run is waiting to come back to.
	Holds func(ctx context.Context, keys []string) (map[string]bool, error)
	// Propose hands an expiry push to its repository's committer (the
	// engine's estate.CodeSeam.Propose, which ranks and fails over as every
	// keyed operation does). Required: the ref lifecycle deletes through the
	// committer or not at all.
	Propose func(ctx context.Context, p Proposal) (ProposeResult, error)
	// Node is this node's id: an expiry proposal's Node, as every push names
	// the node that formed it. Required.
	Node string
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
	// Sweeps counts the retire_sweep records published, each removing up to
	// RetireSweepRows of one repository's retired pack rows past RetireGrace.
	Sweeps int `json:"sweeps"`
	// SweptRows counts the rows those records remove, by each step's decide.
	SweptRows int `json:"swept_rows"`
	// ChecksSweeps counts the checks_sweep records published, each removing
	// up to CheckHorizonRows of one repository's check rows past CheckHistory.
	ChecksSweeps int `json:"checks_sweeps"`
	// ChecksSweptRows counts the rows those records remove, by each step's
	// decide.
	ChecksSweptRows int `json:"checks_swept_rows"`
	// QueueSweeps counts the queue_sweep records published, each removing up
	// to QueueSweepRows of one repository's ended entries and landings below
	// their green row past QueueHistory, its ejections within QueueSweepBytes.
	QueueSweeps int `json:"queue_sweeps"`
	// QueueSweptRows counts the rows those records remove, by each step's
	// decide.
	QueueSweptRows int `json:"queue_swept_rows"`
	// Purged names every repository purged and handed on (applied here and
	// taken by OnPurged), in that order; a name an earlier pass held is
	// reported by the pass that hands it on.
	Purged []string `json:"purged,omitempty"`
	// Steps counts the purge steps that appended a record: all but those that
	// found their repository already purged by an earlier pass this node's
	// listing had not yet applied.
	Steps int `json:"steps"`
	// Failed maps each repository whose sweep or purge failed this pass to
	// the failure's sentence (both joined, where both did), keyed by id where
	// this node holds no row to name it. The pass went on past each, and the
	// next asks again.
	Failed map[string]string `json:"failed,omitempty"`
	// Unhanded names every purged repository the pass HOLDS at its end for a
	// later pass: its last record not yet applied here, OnPurged refusing it,
	// or the context ending first.
	Unhanded []string `json:"unhanded,omitempty"`
	// Remain says the pass stopped at its budget with repositories still due
	// a sweep or a purge: the designed stop, which the next pass resumes.
	Remain bool `json:"remain,omitempty"`
	// Released counts the old names freed, each a release_name record, once
	// their redirect ended ([RedirectWindow]).
	Released int `json:"released,omitempty"`
	// Expired counts the scratch repositories deleted, idle past
	// code.scratch.idle_days and held by no live turn — each restorable for
	// code.deleted_retention_days.
	Expired int `json:"expired,omitempty"`
	// ScratchHeld counts the scratch repositories idle past
	// code.scratch.idle_days that the pass KEPT because a live turn holds them.
	ScratchHeld int `json:"scratch_held,omitempty"`
	// ScratchExpiryUnread says why the scratch expiry stopped with nothing
	// more expired: whose holds could not be read. A hold the pass cannot read
	// is one it does not expire under.
	ScratchExpiryUnread string `json:"scratch_expiry_unread,omitempty"`
	// RefsExpired counts the seat branches deleted, idle past RefExpiry and
	// held by no live turn — each restorable from its push row's Old while
	// the push history keeps it.
	RefsExpired int `json:"refs_expired"`
	// RefsKept counts the idle seat branches proposed and kept by the decide
	// ([ExpiryRule], or a protection): set, held, made the default or put
	// under review since the page listed it, or set last by somebody else.
	RefsKept int `json:"refs_kept,omitempty"`
	// RefsHeld counts the idle seat branches not proposed because a live
	// turn holds them.
	RefsHeld int `json:"refs_held,omitempty"`
	// ExpiryFailed maps a repository's name to why an expiry of its branches
	// failed this pass; what is still there is listed again by the next.
	ExpiryFailed map[string]string `json:"expiry_failed,omitempty"`
	// ExpiryUnread says why the ref expiry stopped with nothing more
	// expired: whose holds could not be read.
	ExpiryUnread string `json:"expiry_unread,omitempty"`
}

// Pass hands on what an earlier pass purged and could not, then takes the
// steps in this file header's order until nothing is due, the budget runs out
// ([RetentionReport.Remain], no error) or ctx ends, pacing every step to the
// pass's share of the write lane.
//
// One repository's failure is reported in [RetentionReport.Failed] and never
// holds back another's. One NO LONGER DUE when its step decides is no failure:
// the listing is a stale read and every step decides again in its own
// snapshot, so one restored since ([ErrNotDeleted]), deleted again within its
// retention ([ErrNotYet]) or swept already is skipped. The error is the
// listing's, a missing field's or ctx's, beside what the pass did; the
// repository it stopped on is not counted a failure.
func (r *Retention) Pass(ctx context.Context) (report RetentionReport, err error) {
	switch {
	case r.Store == nil:
		return report, errors.New("gitlet: a retention pass needs the lifecycle store that " +
			"takes its sweeps' and its purges' steps")
	case r.Reader == nil:
		return report, errors.New("gitlet: a retention pass needs the reader that lists the " +
			"repositories due")
	case r.Retention == nil:
		return report, errors.New("gitlet: a retention pass needs the current epoch's " +
			"code.deleted_retention_days")
	case r.Policy == nil:
		return report, errors.New("gitlet: a retention pass needs the current epoch's code.scratch, " +
			"which its scratch expiry is held to")
	case r.Holds == nil:
		return report, errors.New("gitlet: a retention pass needs the live turns' holds — its scratch " +
			"expiry never deletes a repository a live turn has named, and one that could not ask " +
			"would delete it under a parked run")
	case r.Propose == nil:
		return report, errors.New("gitlet: a retention pass needs the committer seam its ref expiry " +
			"proposes its deletions to")
	case strings.TrimSpace(r.Node) == "":
		return report, errors.New("gitlet: a retention pass needs this node's id, which its ref expiry's " +
			"pushes name")
	}
	retention := r.Retention()
	if retention <= 0 {
		return report, refusedf("%w: code.deleted_retention_days: %v is not a retention",
			ErrInvalid, retention)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	log := r.Logger
	if log == nil {
		log = logging.Get("gitlet")
	}
	pace := r.pacer()
	defer func() { report.Unhanded = r.heldNames() }()

	if err := r.handOnHeld(ctx, &report, log); err != nil {
		return report, err
	}
	// THE STEPS IN ORDER, on one pacer and one budget, the expiries LAST: a
	// purge and a release free names, an expiry only room. Each stops the
	// pass at the budget (Remain), and the next resumes every step from the
	// rows.
	for _, step := range []func() (bool, error){
		func() (bool, error) { return r.sweep(ctx, now, pace, &report) },
		func() (bool, error) { return r.sweepChecks(ctx, now, pace, &report) },
		func() (bool, error) { return r.sweepQueues(ctx, now, pace, &report) },
		func() (bool, error) { return r.purges(ctx, now, retention, pace, &report, log) },
		func() (bool, error) { return r.release(ctx, now, pace, &report) },
		func() (bool, error) { return r.expireScratch(ctx, now, pace, &report, log) },
		func() (bool, error) { return r.expire(ctx, now, pace, &report, log) },
	} {
		if stop, err := step(); err != nil || stop {
			report.Remain = stop
			return report, err
		}
	}
	return report, nil
}

// purges steps every purging repository, then every one deleted past the
// retention, oldest deletion first, until none is left, the budget runs out
// (stop) or the context ends (the error).
func (r *Retention) purges(ctx context.Context, now time.Time, retention time.Duration, pace *pacer,
	report *RetentionReport, log *slog.Logger) (stop bool, err error) {

	page := cmp.Or(r.page, retentionPage)
	var after *dueRepo
	for {
		due, err := r.Reader.due(ctx, dueQuery{cutoff: now.Add(-retention), after: after, limit: page})
		if err != nil {
			return false, fmt.Errorf("gitlet: list the repositories due a purge past %v: %w",
				retention, err)
		}
		for _, repo := range due {
			if r.holds(repo) {
				// ITS PURGE IS DONE ON THE LOG and this node's stale listing
				// has not applied its end: a step would only meet its record.
				continue
			}
			if stop, err := r.purge(ctx, repo, retention, pace, report, log); err != nil || stop {
				return stop, err
			}
		}
		if len(due) < page {
			return false, nil
		}
		after = &due[len(due)-1]
	}
}

// release frees every old name whose redirect has ended by now
// ([Store.ReleaseName]), oldest first, a page at a time — keyset, so a name
// its release left (re-pointed, or not yet due on the decide's clock) is
// passed over rather than heading every page — until none is left, the
// budget runs out (stop) or the context ends (the error). A failed release is
// reported and passed over.
func (r *Retention) release(ctx context.Context, now time.Time, pace *pacer,
	report *RetentionReport) (stop bool, err error) {

	q := redirectsQuery{cutoff: now, limit: cmp.Or(r.page, retentionPage)}
	for {
		due, err := r.Reader.redirectsDue(ctx, q)
		if err != nil {
			return false, fmt.Errorf("gitlet: list the old names whose redirect has ended: %w", err)
		}
		for _, name := range due {
			switch turn, err := pace.wait(ctx); {
			case err != nil:
				return false, err
			case !turn:
				return true, nil
			}
			began := pace.clock()
			released, err := r.Store.ReleaseName(ctx, name.name, name.repoID)
			pace.took(began)
			switch {
			case err != nil && ctx.Err() != nil:
				return false, ctx.Err()
			case err != nil:
				report.fail(name.name, err.Error())
			case released:
				report.Released++
			}
		}
		if len(due) < q.limit {
			return false, nil
		}
		q.after = &due[len(due)-1]
	}
}

// expireScratch deletes every scratch repository idle past
// code.scratch.idle_days that no live turn holds, a page at a time, oldest
// first, until none is left, the budget runs out (stop) or the context ends
// (the error). A hold it cannot read ends the step with nothing more expired
// ([RetentionReport.ScratchExpiryUnread]); an unknown is never "nobody".
//
// THE ORDER IS THE SAFETY ARGUMENT: the CUTOFF is fixed first, each PAGE read
// with the code log's position (Through) in its own transaction, and only
// THEN its holds. A hold it missed was taken after the cutoff, so the
// repository was active past the cutoff ([HoldRecordAfter]) or the turn
// recorded it past Through: the decide refuses both ([Store.ExpireScratch]).
// KEYSET, never the head: held repositories stay idle, filling every page.
func (r *Retention) expireScratch(ctx context.Context, now time.Time, pace *pacer, report *RetentionReport,
	log *slog.Logger) (stop bool, err error) {

	q := idleQuery{cutoff: now.Add(-r.Policy().ScratchIdle), limit: cmp.Or(r.page, retentionPage)}
	for {
		page, err := r.Reader.idleScratch(ctx, q)
		if err != nil {
			return false, fmt.Errorf("gitlet: list the scratch repositories idle since %s: %w",
				q.cutoff.UTC().Format(time.RFC3339), err)
		}
		if len(page.repos) == 0 {
			return false, nil
		}
		ids := make([]string, len(page.repos))
		for i, repo := range page.repos {
			ids[i] = repo.id
		}
		held, err := r.Holds(ctx, ids)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			report.ScratchExpiryUnread = fmt.Sprintf("which scratch repositories live turns hold could not "+
				"be read (%v); none was expired this pass", err)
			log.WarnContext(ctx, "code_scratch_expiry_skipped", "error", err.Error(), "first", ids[0],
				"page", len(ids), "detail", "a repository whose holds are unknown is never expired; the "+
					"next pass asks again")
			return false, nil
		}
		x := ScratchExpiry{Cutoff: q.cutoff, Through: page.through, KeyedTo: page.keyedTo}
		for _, repo := range page.repos {
			if held[repo.id] {
				report.ScratchHeld++
				continue
			}
			switch turn, err := pace.wait(ctx); {
			case err != nil:
				return false, err
			case !turn:
				return true, nil
			}
			began := pace.clock()
			expired, err := r.Store.ExpireScratch(ctx, repo.id, x)
			pace.took(began)
			switch {
			case err != nil && ctx.Err() != nil:
				return false, ctx.Err()
			case err != nil:
				report.fail(repo.name, err.Error())
			case expired:
				report.Expired++
				log.InfoContext(ctx, "code_scratch_expired", "name", repo.name, "id", repo.id,
					"idle_since", repo.lastActivity.UTC().Format(time.RFC3339))
			}
		}
		if len(page.repos) < q.limit {
			return false, nil
		}
		q.after = &page.repos[len(page.repos)-1]
	}
}

// sweep steps every repository the retired listing names, once each however
// many of its retiring records it names, until nothing past the grace is
// left, the budget runs out (stop) or the context ends (the error). Each step
// decides again in its own snapshot, so one swept or purged since the stale
// listing read it is answered nothing.
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
			step := func(ctx context.Context) (SweepResult, error) { return r.Store.RetireSweep(ctx, id, now) }
			if stop, err := r.sweepRepo(ctx, id, pace, report, step, func(res SweepResult) {
				report.Sweeps++
				report.SweptRows += res.Rows
			}); err != nil || stop {
				return stop, err
			}
		}
		if page.next == nil {
			return false, nil
		}
		q.after = page.next
	}
}

// sweepChecks steps every repository holding a check row past [CheckHistory]
// by [CheckSweepGrace], once each however many of its endings the listing
// names, back to the history, until none is left, the budget runs out (stop)
// or the context ends (the error).
//
// LISTED PAST THE GRACE, SWEPT TO THE HISTORY: a repository whose checks keep
// ending removes its own history with each, so stepping it would append, on
// every data node and ahead of the purges, a record its next ending makes
// anyway. One listed is IDLE, or its endings' bounded horizons fell a grace
// behind; its step removes every row past the history, so it is listed a
// grace later at the soonest. Each step decides again in its own snapshot.
func (r *Retention) sweepChecks(ctx context.Context, now time.Time, pace *pacer,
	report *RetentionReport) (stop bool, err error) {

	q := checksQuery{cutoff: now.Add(-CheckHistory - CheckSweepGrace), limit: cmp.Or(r.page, retentionPage)}
	stepped := map[string]bool{}
	for {
		page, err := r.Reader.checksDue(ctx, q)
		if err != nil {
			return false, err
		}
		for _, id := range page.repos {
			if stepped[id] {
				continue
			}
			stepped[id] = true
			step := func(ctx context.Context) (SweepResult, error) { return r.Store.ChecksSweep(ctx, id, now) }
			if stop, err := r.sweepRepo(ctx, id, pace, report, step, func(res SweepResult) {
				report.ChecksSweeps++
				report.ChecksSweptRows += res.Rows
			}); err != nil || stop {
				return stop, err
			}
		}
		if page.next == nil {
			return false, nil
		}
		q.after = page.next
	}
}

// sweepQueues steps every repository holding queue history past
// [QueueHistory] by [QueueSweepGrace] back to the history, until none is
// left, the budget runs out (stop) or the context ends (the error). Listed
// past the grace for the checks' reason ([Retention.sweepChecks]): a
// repository whose queues keep working removes its own history with every
// candidate, ejection and landing.
func (r *Retention) sweepQueues(ctx context.Context, now time.Time, pace *pacer,
	report *RetentionReport) (stop bool, err error) {

	q := queueSweepQuery{cutoff: now.Add(-QueueHistory - QueueSweepGrace), limit: cmp.Or(r.page, retentionPage)}
	for {
		page, more, err := r.Reader.queueSweepDue(ctx, q)
		if err != nil {
			return false, err
		}
		for _, id := range page {
			step := func(ctx context.Context) (SweepResult, error) { return r.Store.QueueSweep(ctx, id, now) }
			if stop, err := r.sweepRepo(ctx, id, pace, report, step, func(res SweepResult) {
				report.QueueSweeps++
				report.QueueSweptRows += res.Rows
			}); err != nil || stop {
				return stop, err
			}
		}
		if !more {
			return false, nil
		}
		q.after = page[len(page)-1]
	}
}

// sweepRepo steps one repository's sweep (step: the retired packs', the
// checks' or the queues') while rows past its horizon are left, until a step
// fails, the budget runs out (stop) or the context ends (the error); counted
// is told each step that appended a record.
//
// A STEP WHOSE RECORD RESOLVED PENDING ENDS THE REPOSITORY'S TURN: this
// node's rows still hold what it removed, so a further step would decide from
// them and wait for this node to apply it. The next pass sweeps the rest.
func (r *Retention) sweepRepo(ctx context.Context, id string, pace *pacer, report *RetentionReport,
	step func(context.Context) (SweepResult, error), counted func(SweepResult)) (stop bool, err error) {
	for {
		switch turn, err := pace.wait(ctx); {
		case err != nil:
			return false, err
		case !turn:
			return true, nil
		}
		began := pace.clock()
		res, err := step(ctx)
		pace.took(began)
		switch {
		case err != nil && ctx.Err() != nil:
			// THE CONTEXT, NOT THE REPOSITORY, as a purge step's.
			return false, ctx.Err()
		case err != nil:
			// THE REPORT IS KEYED BY NAME, as an operator reads it.
			report.fail(r.Store.NameOf(ctx, id), err.Error())
			return false, nil
		case res.Rows == 0:
			return false, nil
		}
		counted(res)
		if !res.More || !res.Held() {
			return false, nil
		}
	}
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
			// THE CONTEXT, NOT THE REPOSITORY: a step on an ended context
			// refuses before it reads a row, so the pass stops and the next
			// resumes this repository from its rows.
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

// redirectsQuery is one page of the release's listing.
type redirectsQuery struct {
	// cutoff is the pass's now: an old name whose redirect ended at or
	// before it is due.
	cutoff time.Time
	// after is the last name of the page before; nil for the first.
	after *dueRedirect
	limit int
}

// dueRedirect is one old name the release step frees.
type dueRedirect struct {
	token, name, repoID string
	until               int64
}

// redirectsDue lists one page of the old names whose redirect ended at or
// before q.cutoff, oldest first and keyset after q.after — a STALE READ of
// this node's rows, since every release decides again in its own snapshot —
// over code_names_redirect_idx (replicated 0096), the >= beside the OR keeping
// the index's range seek.
func (r *Reader) redirectsDue(ctx context.Context, q redirectsQuery) ([]dueRedirect, error) {
	if q.limit <= 0 {
		return nil, fmt.Errorf("%w: a page of the redirects due lists at least one", ErrInvalid)
	}
	after := dueRedirect{until: math.MinInt64}
	if q.after != nil {
		after = *q.after
	}
	var out []dueRedirect
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, redirectsDueSQL, store.EncodeTime(q.cutoff), after.until,
			after.until, after.token, q.limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d dueRedirect
			if err := rows.Scan(&d.token, &d.name, &d.repoID, &d.until); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// idleQuery is one page of the scratch expiry's listing.
type idleQuery struct {
	// cutoff is the pass's now less code.scratch.idle_days, fixed once.
	cutoff time.Time
	// after is the last repository of the page before; nil for the first.
	after *idleRepo
	limit int
}

// idleRepo is one scratch repository the expiry may take.
type idleRepo struct {
	id, name     string
	lastActivity time.Time
	activity     int64
}

// idlePage is one page of the scratch expiry's listing, with the position it
// was read at — the code log's checkpoint, read INSIDE the page's transaction
// — and the stream identity that checkpoint is keyed to: what every expiry of
// the page is decided against ([ScratchExpiry]).
type idlePage struct {
	repos   []idleRepo
	through statelog.Position
	keyedTo time.Time
}

// idleScratch lists one page of the ACTIVE scratch repositories last active at
// or before q.cutoff, idle longest first and keyset after q.after, over
// code_repos_idle_idx (replicated 0096) — and the position it read them at,
// in the same transaction. A STALE READ: every expiry decides again in its
// own snapshot, and refuses a repository with any record past this page.
func (r *Reader) idleScratch(ctx context.Context, q idleQuery) (idlePage, error) {
	if q.limit <= 0 {
		return idlePage{}, fmt.Errorf("%w: a page of the idle scratch repositories lists at least one",
			ErrInvalid)
	}
	after := idleRepo{activity: math.MinInt64}
	if q.after != nil {
		after = *q.after
	}
	var page idlePage
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err := r.log.ReadThrough(ctx, query, func(tx *sql.Tx, cp statelog.Checkpoint) error {
		page = idlePage{through: cp.At, keyedTo: cp.KeyedTo}
		rows, err := tx.QueryContext(ctx, idleScratchSQL, store.EncodeTime(q.cutoff), after.activity,
			after.activity, after.id, q.limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d idleRepo
			if err := rows.Scan(&d.id, &d.name, &d.activity); err != nil {
				return err
			}
			d.lastActivity = store.DecodeTime(d.activity)
			page.repos = append(page.repos, d)
		}
		return rows.Err()
	})
	if err != nil {
		return idlePage{}, err
	}
	return page, nil
}

// The two listings' statements, each registered in plan_test.go: keyset pages
// over their partial indexes, the predicate repeated verbatim and the >=
// beside the OR keeping the range seek.
const (
	redirectsDueSQL = `SELECT token, name, repo_id, redirect_until FROM code_names
		WHERE redirect_until IS NOT NULL AND redirect_until <= ? AND redirect_until >= ?
			AND (redirect_until > ? OR token > ?)
		ORDER BY redirect_until, token LIMIT ?`
	idleScratchSQL = `SELECT id, name, last_activity_at FROM code_repos
		WHERE kind = 'scratch' AND status = 'active' AND last_activity_at <= ? AND last_activity_at >= ?
			AND (last_activity_at > ? OR id > ?)
		ORDER BY last_activity_at, id LIMIT ?`
)

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
