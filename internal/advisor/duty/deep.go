package duty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/config"
)

// THE DEEP RUN ON THE DUTY (ADR-0166): an investigation on the Advisor's model,
// asked for by a person (Run, after its checks) or due at its slots (nightly, and early on
// Monday before the weekly report). It runs for minutes, longer than the
// lease's time to live, so it runs beside the tick rather than in it: the tick
// keeps renewing the lease and runs nothing else while it is in flight, and a
// holder that loses the lease cancels it, leaving a person's request pending
// for the next holder. One deep run at a time is the lease's.

// DeepPass is the Advisor's deep run, nil where none runs on this node.
type DeepPass interface {
	// Assigned reports whether the company assigns the Advisor a model.
	Assigned() bool
	// Deep is one investigation at now: what it found, grounded, and its
	// record as a run keeps it, telling io each line and each checkpoint as
	// it writes them, and resuming from io's checkpoint where it has one.
	Deep(ctx context.Context, now time.Time, dismissed, live []advisor.Stored, io DeepIO) (ModelRun, error)
}

// DeepIO is what a deep run tells the log as it goes, and where it resumes
// from (ADR-0166).
type DeepIO struct {
	// Progress is told each line the run writes.
	Progress func(at time.Time, text string)
	// Checkpoint is told where the run stands each time that moves (an item
	// answered, a read asked), so a holder that takes the run over resumes
	// from the newest.
	Checkpoint func(at time.Time, body json.RawMessage)
	// Resume is the newest checkpoint a run taken over wrote, nil for a run
	// that starts fresh.
	Resume json.RawMessage
}

// DeepHours are the hours of the company day a deep run is due at: 03:00
// every day, and 05:00 on Monday, so the 06:00 weekly report reads a fresh one.
const (
	DeepNightHour  = 3
	DeepMondayHour = 5
)

// DeepLongest is the longest a deep run in flight goes without a line: its
// reads' deadline, the largest loop a company may set, and a lease's TTL for
// its records. A run unheard for longer is not in flight.
const DeepLongest = ObserveFor + config.MaxAdvisorDeepMinutes*time.Minute + LeaseTTL

// flight is the deep run in flight on this holder.
type flight struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// deepState is the duty's deep run, guarded apart from the tick.
type deepState struct {
	mu     sync.Mutex
	flying *flight
	// unrecorded is each run this holder ran whose record the log has not
	// taken yet, by its id: the tick records it again rather than run it
	// again from the start.
	unrecorded map[string]Run
	// last is when this holder last started a deep run at a slot.
	last time.Time
	// claimedAt is when this holder's last successful claim began, zero
	// where it holds no lease: what [Duty.holds] proves ownership by.
	claimedAt time.Time
}

// claimed notes a successful claim begun at at, or zero for a lease lost.
func (d *Duty) claimed(at time.Time) {
	d.deep.mu.Lock()
	defer d.deep.mu.Unlock()
	d.deep.claimedAt = at
}

// holds reports whether this holder provably holds the lease at now: a node
// with no fleet always does, a holder only within [LeaseTTL] of its last
// successful claim, past which a peer may hold it.
func (d *Duty) holds(now time.Time) bool {
	if d.Claim == nil {
		return true
	}
	d.deep.mu.Lock()
	defer d.deep.mu.Unlock()
	return !d.deep.claimedAt.IsZero() && now.Before(d.deep.claimedAt.Add(LeaseTTL))
}

// deepFlying reports whether a deep run is in flight here.
func (d *Duty) deepFlying() bool {
	d.deep.mu.Lock()
	defer d.deep.mu.Unlock()
	return d.deep.flying != nil
}

// cancelDeep stops the deep run in flight here, if any, and waits for it.
func (d *Duty) cancelDeep() {
	d.deep.mu.Lock()
	f := d.deep.flying
	d.deep.mu.Unlock()
	if f != nil {
		f.cancel()
		<-f.done
	}
}

// WaitDeep waits for the deep run in flight here, if any: for a caller that
// needs its record written before it reads.
func (d *Duty) WaitDeep() {
	d.deep.mu.Lock()
	f := d.deep.flying
	d.deep.mu.Unlock()
	if f != nil {
		<-f.done
	}
}

// deepSlot is the latest slot a deep run is due at, at or before now.
func (d *Duty) deepSlot(now time.Time) time.Time {
	local := now.In(d.Zone())
	night := time.Date(local.Year(), local.Month(), local.Day(), DeepNightHour, 0, 0, 0, local.Location())
	if night.After(local) {
		night = night.AddDate(0, 0, -1)
	}
	back := (int(local.Weekday()) + 6) % 7
	monday := time.Date(local.Year(), local.Month(), local.Day()-back, DeepMondayHour, 0, 0, 0, local.Location())
	if monday.After(local) {
		monday = monday.AddDate(0, 0, -7)
	}
	if monday.After(night) {
		return monday
	}
	return night
}

// deepDue reports whether a deep run is due at a slot: the model is
// assigned, and no deep run concluded since the latest slot.
func (d *Duty) deepDue(now time.Time, last map[string]time.Time) bool {
	if d.Deep == nil || !d.Deep.Assigned() {
		return false
	}
	slot := d.deepSlot(now)
	d.deep.mu.Lock()
	started := d.deep.last
	d.deep.mu.Unlock()
	return last[DeepScope].Before(slot) && started.Before(slot)
}

// DeepScope is the scope a deep run that concluded is recorded under, so the
// next slot knows it ran.
const DeepScope = "deep"

// startDeep starts one deep run beside the tick, recorded under each of ids
// when it ends (a person's presses), or under one id at a slot: a new one, or
// the id of the run it takes over. prior is what the run did before this
// investigation, recorded with it as one run: a press's checks, or when the
// holder it takes the run over from began it.
func (d *Duty) startDeep(ctx context.Context, now time.Time, trigger Trigger, by string, ids []string, prior *Run) {
	runCtx, cancel := context.WithCancel(ctx)
	f := &flight{cancel: cancel, done: make(chan struct{})}
	d.deep.mu.Lock()
	d.deep.flying, d.deep.last = f, now
	d.deep.mu.Unlock()
	go func() {
		defer close(f.done)
		defer cancel()
		run := d.runDeep(runCtx, now, trigger, by, ids[0])
		if prior != nil {
			run = joined(*prior, run)
		}
		switch {
		case runCtx.Err() != nil:
			// STOPPED: the lease was lost or the duty stops, and the next
			// holder runs the press again, or closes the slot's run.
		case !d.holds(d.Now().UTC()):
			log.WarnContext(ctx, "advisor_deep_run_not_recorded", "run", ids[0],
				"error", "the advisor's lease went unrenewed past its TTL during the run, so the next holder closes it")
		default:
			var errs []error
			for _, id := range ids {
				run.ID = id
				errs = append(errs, d.conclude(ctx, run))
			}
			if err := errors.Join(errs...); err != nil {
				log.WarnContext(ctx, "advisor_deep_run_not_recorded", "error", err.Error())
			}
		}
		d.deep.mu.Lock()
		d.deep.flying = nil
		d.deep.mu.Unlock()
	}()
}

// joined is a run of two parts as one: begun when the prior part began (a
// press's checks), failed where either part failed.
func joined(prior, deep Run) Run {
	out := deep
	out.StartedAt, out.Classes = prior.StartedAt, prior.Classes
	out.Scopes = append(append([]ScopeRun{}, prior.Scopes...), deep.Scopes...)
	if prior.Status == RunFailed {
		out.Status = RunFailed
	}
	return out
}

// InterruptedSaid is what a run recorded interrupted says of why.
const InterruptedSaid = "The engine stopped during this run, so it did not finish. The next run starts fresh."

// closeUnowned closes every run whose holder stopped mid-run (package doc, "A
// run whose holder stopped"), and reports whether it took one over, which then
// runs beside the tick.
func (d *Duty) closeUnowned(ctx context.Context, now time.Time, last map[string]time.Time) (bool, error) {
	unowned, err := d.Log.Unowned(ctx)
	if err != nil {
		return false, fmt.Errorf("read the runs nobody holds: %w", err)
	}
	d.deep.mu.Lock()
	unowned = slices.DeleteFunc(unowned, func(u Unowned) bool {
		_, kept := d.deep.unrecorded[u.Run]
		return kept
	})
	d.deep.mu.Unlock()
	// THE NEWEST BEGUN WITHIN THE SLOT goes on where a deep run is still
	// due, from its checkpoint, rather than a second run starting afresh.
	slot, resume := d.deepSlot(now), -1
	if d.Deep != nil && d.Deep.Assigned() && last[DeepScope].Before(slot) {
		for i, u := range unowned {
			if !u.First.Before(slot) && (resume < 0 || u.Last.After(unowned[resume].Last)) {
				resume = i
			}
		}
	}
	var errs []error
	for i, u := range unowned {
		if i == resume {
			continue
		}
		errs = append(errs, d.conclude(ctx, Run{ID: u.Run, Trigger: TriggerSchedule, StartedAt: u.First,
			EndedAt: u.Last, Status: RunInterrupted, Kind: advisordomain.RunKindDeep,
			Model: map[string]any{"status": string(advisor.PassFailed), "reason": InterruptedSaid}}))
	}
	if resume < 0 {
		return false, errors.Join(errs...)
	}
	u := unowned[resume]
	log.InfoContext(ctx, "advisor_deep_run_taken_over", "run", u.Run)
	d.startDeep(ctx, now, TriggerSchedule, "", []string{u.Run}, &Run{StartedAt: u.First, Status: RunDone})
	return true, errors.Join(errs...)
}

// runDeep is one deep run, reconciled with the log where it concluded.
func (d *Duty) runDeep(ctx context.Context, now time.Time, trigger Trigger, by, id string) (run Run) {
	run = Run{ID: id, Trigger: trigger, By: by, StartedAt: now, Status: RunDone, Kind: advisordomain.RunKindDeep}
	defer func() {
		if run.EndedAt = d.Now().UTC(); run.EndedAt.Before(run.StartedAt) {
			run.EndedAt = run.StartedAt
		}
	}()
	dismissed, err := d.Log.Dismissed(ctx)
	var live []advisor.Stored
	if err == nil {
		live, err = d.Log.Live(ctx)
	}
	if err != nil {
		log.WarnContext(ctx, "advisor_findings_unread", "error", err.Error())
		run.Status = RunFailed
		run.Model = map[string]any{"status": string(advisor.PassFailed),
			"reason": cutWords("The Advisor's findings could not be read: " + err.Error())}
		return run
	}
	// A RUN TAKEN OVER RESUMES from the newest checkpoint its request's run
	// wrote; one that cannot be read starts over, as a run did before.
	resume, err := d.Log.Resume(ctx, id)
	if err != nil {
		log.WarnContext(ctx, "advisor_checkpoint_unread", "run", id, "error", err.Error())
		resume = nil
	}
	// EACH LINE AND EACH CHECKPOINT ON THE LOG as it is written, so any node
	// reads the run's progress while it is in flight, and the next holder
	// resumes it; none once the lease may be a peer's, which owns the run.
	result, err := d.Deep.Deep(ctx, now, dismissed, live, DeepIO{
		Progress: func(at time.Time, text string) {
			if !d.holds(at) {
				return
			}
			if perr := d.Log.Progress(ctx, id, at, text); perr != nil {
				log.WarnContext(ctx, "advisor_progress_not_recorded", "run", id, "error", perr.Error())
			}
		},
		Checkpoint: func(at time.Time, body json.RawMessage) {
			if !d.holds(at) {
				return
			}
			if cerr := d.Log.Checkpoint(ctx, id, at, body); cerr != nil {
				log.WarnContext(ctx, "advisor_checkpoint_not_recorded", "run", id, "error", cerr.Error())
			}
		},
		Resume: resume,
	})
	run.Model = result.Summary
	if err != nil {
		log.WarnContext(ctx, "advisor_deep_run_failed", "error", err.Error())
		run.Status = RunFailed
		return run
	}
	if (!result.Concluded && !result.Partial) || ctx.Err() != nil || !d.holds(d.Now().UTC()) {
		return run
	}
	reading := advisor.Reading{At: now, Groups: []advisor.Group{advisor.GroupModel}, Partial: !result.Concluded}
	if reading.LastDone, err = d.Log.LastDone(ctx); err != nil {
		log.WarnContext(ctx, "advisor_findings_unread", "error", err.Error())
		run.Status = RunFailed
		return run
	}
	run.Scopes = append(scopesOf(reading), ScopeRun{Scope: DeepScope, Status: ScopeDone,
		Steps: map[advisor.Step]int{}})
	for _, t := range advisor.Reconcile(live, result.Observations, reading) {
		if err := d.Log.Record(ctx, t, now); err != nil {
			log.WarnContext(ctx, "advisor_finding_not_recorded", "step", string(t.Step), "error", err.Error())
			continue
		}
		run.Scopes[0].Steps[t.Step]++
	}
	return run
}

// ONE PRESS, ONE RUN (ADR-0166). A person presses Run and never chooses a kind
// of run: the checks run at once, their findings recorded as they are found,
// and the run goes on into the deep investigation beside the tick, under the
// same id and the same progress line, recorded as one run with both parts.
// The deep part is kept, not run, where an investigation concluded within
// [DeepFreshFor], and cannot run where no model is assigned; the run says
// which, and why.

// DeepFreshFor is how long an investigation that concluded stands for a
// press: a press within it runs the checks and keeps that investigation
// rather than spend another. Six hours: the nightly one at 03:00 (and
// Monday's at 05:00, before the weekly report) serves the presses of the
// working morning, a press in the afternoon investigates again, and a
// company that presses all day pays for at most four investigations beside
// the nightly one, each within its own bound (2M tokens and 20 minutes by
// default), where every press investigating would pay for one a press.
const DeepFreshFor = 6 * time.Hour

// Why a run's deep part did not run, as its record says it.
const (
	// DeepKept is a deep part kept from an investigation that concluded
	// within [DeepFreshFor].
	DeepKept = "kept"
	// DeepNoModel is a deep part that could not run: no model is assigned.
	DeepNoModel = "no_model"
)

// pressed answers every press waiting with one run, recorded under each
// press's own id, so a press during a run joins the next. A press this
// holder already ran waits for its record, never run again.
func (d *Duty) pressed(ctx context.Context, now time.Time, requests []Request) error {
	d.deep.mu.Lock()
	requests = slices.DeleteFunc(requests, func(r Request) bool {
		_, ran := d.deep.unrecorded[r.Run]
		return ran
	})
	d.deep.mu.Unlock()
	if len(requests) == 0 {
		return nil
	}
	last, err := d.Log.LastDone(ctx)
	if err != nil {
		return fmt.Errorf("read when each group last ran: %w", err)
	}
	ids := make([]string, len(requests))
	for i, r := range requests {
		ids[i] = r.Run
	}
	first := requests[0]
	skipped, why := "", ""
	switch {
	case d.Deep == nil || !d.Deep.Assigned():
		skipped, why = DeepNoModel, "The Advisor has no model assigned (advisor.model), so the deep investigation could not run."
	case now.Sub(last[DeepScope]) < DeepFreshFor:
		skipped, why = DeepKept, fmt.Sprintf("The deep investigation from %s ago is kept: another runs on a press "+
			"%s after it, and every night.", hoursAgo(now.Sub(last[DeepScope])), hoursAgo(DeepFreshFor))
	}
	// THE CHECKS FIRST, their findings recorded as they are found; the daily
	// model pass with them only where no investigation follows and it is due.
	model := skipped != "" && d.Model != nil && d.Model.Assigned() &&
		last[string(advisor.GroupModel)].Before(d.slot(now))
	rules := d.run(ctx, now, true, model, TriggerPerson, first.By, first.Run)
	rules.Kind = advisordomain.RunKindDeep
	if skipped == "" {
		if perr := d.Log.Progress(ctx, first.Run, d.Now().UTC(), checksSaid(rules)); perr != nil {
			log.WarnContext(ctx, "advisor_progress_not_recorded", "run", first.Run, "error", perr.Error())
		}
		d.startDeep(ctx, now, TriggerPerson, first.By, ids, &rules)
		return d.Log.Expire(ctx, now)
	}
	rules.Model = withSkipped(rules.Model, skipped, why, last[DeepScope])
	var errs []error
	for _, r := range requests {
		rules.ID, rules.By = r.Run, r.By
		errs = append(errs, d.conclude(ctx, rules))
	}
	return errors.Join(append(errs, d.Log.Expire(ctx, now))...)
}

// checksSaid is the progress line a run's checks end on, before it goes on
// into the investigation.
func checksSaid(r Run) string {
	return fmt.Sprintf("Checks done: %d new, %d closed", r.Count(advisor.StepOpen), r.Count(advisor.StepClose))
}

// withSkipped is a run's model summary saying why its deep part did not run:
// skipped (kept or no_model), the reason in words, and for a kept part when
// the investigation it keeps ran.
func withSkipped(model any, skipped, why string, from time.Time) map[string]any {
	out := map[string]any{}
	if model != nil {
		if raw, err := json.Marshal(model); err == nil {
			_ = json.Unmarshal(raw, &out)
		}
	}
	out["skipped"] = skipped
	if _, said := out["reason"]; !said || skipped == DeepNoModel {
		out["reason"] = why
	}
	if skipped == DeepKept {
		out["kept_from"] = from.UTC()
	}
	if _, has := out["status"]; !has {
		out["status"] = string(advisor.PassDone)
		if skipped == DeepNoModel {
			out["status"] = string(advisor.PassFailed)
		}
	}
	return out
}

// hoursAgo is a span as a person reads it: whole hours, or minutes under one.
func hoursAgo(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d/time.Minute))
	}
	return fmt.Sprintf("%d h", int(d/time.Hour))
}

// A RUN ALWAYS CONCLUDES. A run whose record the log refuses would leave the
// request that started it waiting, and the next tick would run it again from
// the start, spending its whole budget each time. Its record is written again
// as failed, saying why in a few words: the record that cannot be refused. One
// the log could not take at all (it was unreachable) is kept here and recorded
// again on every tick that holds the lease, which never runs it again.

// maxFailedWords bounds what a failed record says of why.
const maxFailedWords = 240

// conclude records run, and where the log refuses its record, records it as
// failed saying why, its scopes kept and then without them; where neither is
// taken, it is kept for the next tick to record.
func (d *Duty) conclude(ctx context.Context, run Run) error {
	err := d.Log.RecordRun(ctx, run)
	if err == nil {
		d.recorded(run.ID)
		return nil
	}
	log.WarnContext(ctx, "advisor_run_not_recorded", "run", run.ID, "error", err.Error())
	failed := run
	failed.Status = RunFailed
	failed.Model = map[string]any{"status": string(advisor.PassFailed),
		"reason": cutWords("The run's record could not be kept, so it is recorded as failed: " + err.Error())}
	ferr := d.Log.RecordRun(ctx, failed)
	if ferr != nil {
		failed.Scopes, failed.Classes = nil, nil
		ferr = d.Log.RecordRun(ctx, failed)
	}
	if ferr != nil {
		d.deep.mu.Lock()
		if d.deep.unrecorded == nil {
			d.deep.unrecorded = map[string]Run{}
		}
		d.deep.unrecorded[run.ID] = failed
		d.deep.mu.Unlock()
		return errors.Join(err, ferr)
	}
	d.recorded(run.ID)
	return nil
}

// recorded forgets a run kept for its record, once the log took it.
func (d *Duty) recorded(id string) {
	d.deep.mu.Lock()
	delete(d.deep.unrecorded, id)
	d.deep.mu.Unlock()
}

// recordKept records again every run this holder ran and could not record:
// a press, a slot's deep run or a run of the rules alike.
func (d *Duty) recordKept(ctx context.Context) error {
	d.deep.mu.Lock()
	kept := slices.SortedFunc(maps.Values(d.deep.unrecorded), func(a, b Run) int { return strings.Compare(a.ID, b.ID) })
	d.deep.mu.Unlock()
	var errs []error
	for _, run := range kept {
		errs = append(errs, d.conclude(ctx, run))
	}
	return errors.Join(errs...)
}

// cutWords is s at most [maxFailedWords] bytes, whole runes.
func cutWords(s string) string {
	if len(s) <= maxFailedWords {
		return s
	}
	s = s[:maxFailedWords]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
