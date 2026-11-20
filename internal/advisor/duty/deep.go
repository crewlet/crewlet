package duty

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
)

// THE DEEP RUN ON THE DUTY (ADR-0166): an investigation on the Advisor's model,
// asked for by a person (Run deep) or due at its slots (nightly, and early on
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
// when it ends (a person's presses), or under a new id at a slot.
func (d *Duty) startDeep(ctx context.Context, now time.Time, trigger Trigger, by string, ids []string) {
	runCtx, cancel := context.WithCancel(ctx)
	f := &flight{cancel: cancel, done: make(chan struct{})}
	d.deep.mu.Lock()
	d.deep.flying, d.deep.last = f, now
	d.deep.mu.Unlock()
	go func() {
		defer close(f.done)
		defer cancel()
		run := d.runDeep(runCtx, now, trigger, by, ids[0])
		if runCtx.Err() == nil {
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
	// resumes it.
	result, err := d.Deep.Deep(ctx, now, dismissed, live, DeepIO{
		Progress: func(at time.Time, text string) {
			if perr := d.Log.Progress(ctx, id, at, text); perr != nil {
				log.WarnContext(ctx, "advisor_progress_not_recorded", "run", id, "error", perr.Error())
			}
		},
		Checkpoint: func(at time.Time, body json.RawMessage) {
			if cerr := d.Log.Checkpoint(ctx, id, at, body); cerr != nil {
				log.WarnContext(ctx, "advisor_checkpoint_not_recorded", "run", id, "error", cerr.Error())
			}
		},
		Resume: resume,
	})
	run.Model = result.Summary
	if err != nil {
		log.WarnContext(ctx, "advisor_deep_run_failed", "error", err.Error())
		return run
	}
	if !result.Concluded && !result.Partial {
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

// pressedDeep answers the deep runs people asked for: one deep run recorded
// under every press, or, where no model is assigned, each recorded as failed
// saying why. A press this holder already ran is recorded, never run again.
func (d *Duty) pressedDeep(ctx context.Context, now time.Time, deep []Request) error {
	deep, err := d.recordAgain(ctx, deep)
	if len(deep) == 0 {
		return err
	}
	ids := make([]string, len(deep))
	for i, r := range deep {
		ids[i] = r.Run
	}
	if d.Deep != nil && d.Deep.Assigned() {
		d.startDeep(ctx, now, TriggerPerson, deep[0].By, ids)
		return nil
	}
	var errs []error
	for _, r := range deep {
		errs = append(errs, d.conclude(ctx, Run{ID: r.Run, Trigger: TriggerPerson, By: r.By, StartedAt: now,
			EndedAt: now, Status: RunFailed, Kind: advisordomain.RunKindDeep,
			Model: map[string]any{"status": string(advisor.PassFailed),
				"reason": "The Advisor has no model assigned (advisor.model), so it cannot investigate."}}))
	}
	return errors.Join(append(errs, err)...)
}

// A RUN ALWAYS CONCLUDES. A run whose record the log refuses would leave the
// request that started it waiting, and the next tick would run it again from
// the start, spending its whole budget each time. Its record is written again
// as failed, saying why in a few words: the record that cannot be refused. One
// the log could not take at all (it was unreachable) is kept here and recorded
// again on the next tick, which never runs it again.

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

// recordAgain records each of requests this holder already ran and could not
// record, and answers the rest, still to run.
func (d *Duty) recordAgain(ctx context.Context, requests []Request) ([]Request, error) {
	var rest []Request
	var errs []error
	for _, r := range requests {
		d.deep.mu.Lock()
		run, ran := d.deep.unrecorded[r.Run]
		d.deep.mu.Unlock()
		if !ran {
			rest = append(rest, r)
			continue
		}
		errs = append(errs, d.conclude(ctx, run))
	}
	return rest, errors.Join(errs...)
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
