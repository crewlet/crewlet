package duty

import (
	"context"
	"errors"
	"sync"
	"time"

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
	// record as a run keeps it.
	Deep(ctx context.Context, now time.Time, dismissed, live []advisor.Stored) (ModelRun, error)
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
				errs = append(errs, d.Log.RecordRun(ctx, run))
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
	result, err := d.Deep.Deep(ctx, now, dismissed, live)
	run.Model = result.Summary
	if err != nil {
		log.WarnContext(ctx, "advisor_deep_run_failed", "error", err.Error())
		return run
	}
	if !result.Concluded {
		return run
	}
	reading := advisor.Reading{At: now, Groups: []advisor.Group{advisor.GroupModel}}
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
// saying why.
func (d *Duty) pressedDeep(ctx context.Context, now time.Time, deep []Request) error {
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
		errs = append(errs, d.Log.RecordRun(ctx, Run{ID: r.Run, Trigger: TriggerPerson, By: r.By, StartedAt: now,
			EndedAt: now, Status: RunFailed, Kind: advisordomain.RunKindDeep,
			Model: map[string]any{"status": string(advisor.PassFailed),
				"reason": "The Advisor has no model assigned (advisor.model), so it cannot investigate."}}))
	}
	return errors.Join(errs...)
}
