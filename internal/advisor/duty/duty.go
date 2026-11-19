// Package duty runs the Advisor in the background (ADR-0164): one node of the
// fleet at a time, under a coordination lease, it reads what the company did,
// runs the detectors and reconciles what they observed with the findings the
// advisor log keeps, so the list says what is true now without anybody asking.
//
// # When it runs
//
// It asks every [Tick] what is due. The rule pass (the setup and evidence
// groups, one reading of everything) runs hourly and as soon as the setup it
// reads changes: a new revision of the company, a seat paused or resumed, an
// integration's reconcile. The model pass runs once a day at [ModelPassHour]
// on the company clock where the company assigns the Advisor a model. A
// person's Run now is a request on the log, which the holder takes on its next
// tick, so a press on any node runs on the one that holds the duty.
//
// # What it never does
//
// It calls no model in a detector and applies no fix: a fix is sent by a
// person after the engine re-checks it (internal/advisor's Recheck, ADR-0115).
package duty

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("advisor.duty")

// Tick is how often the holder asks what is due: a change to the setup reaches
// the list within a minute, the coalescing the design asks of an event.
const Tick = time.Minute

// LeaseTTL is how long a holder's claim lasts unrenewed: three ticks, so a node
// that stops is replaced within three minutes and a slow tick never loses it.
const LeaseTTL = 3 * Tick

// RulePassEvery is the rule pass's backstop when nothing changes: the evidence
// classes read a week, so an hour moves their counts by little.
const RulePassEvery = time.Hour

// ModelPassHour is the company-clock hour the daily model pass starts at,
// before a working day, after the night's schedules have run.
const ModelPassHour = 6

// Trigger is what started a run.
type Trigger string

// The triggers.
const (
	TriggerSchedule Trigger = "schedule"
	TriggerEvent    Trigger = "event"
	TriggerPerson   Trigger = "person"
)

// Reader is what a run reads.
type Reader interface {
	// Observe reads the company once at now: every finding the rule groups
	// observe, the sources it could not read, and the subjects the company has.
	Observe(ctx context.Context, now time.Time) (Observed, error)
	// Signature is a cheap read of everything the setup group reads, which
	// changes when any of it does.
	Signature(ctx context.Context) (string, error)
}

// Observed is one reading of the company.
type Observed struct {
	Observations []advisor.Observation
	Unread       []string
	Held         func(advisor.Subject) bool
}

// ModelPass is the Advisor's model pass, nil where none runs on this node.
type ModelPass interface {
	// Assigned reports whether the company assigns the Advisor a model.
	Assigned() bool
	// Run is one pass at now: what it found, grounded, and its summary.
	Run(ctx context.Context, now time.Time, dismissed []advisor.Stored) (ModelRun, error)
}

// ModelRun is how one model pass ended.
type ModelRun struct {
	Observations []advisor.Observation
	// Summary is the pass's record as a run keeps it: status, tokens, drops.
	Summary any
	// Concluded reports a pass that read and submitted, so what it did not
	// list again is no longer its finding.
	Concluded bool
}

// Log is the advisor log as the duty reads and writes it.
type Log interface {
	// Live is every open and applied finding.
	Live(ctx context.Context) ([]advisor.Stored, error)
	// Dismissed is every dismissed model finding still held quiet.
	Dismissed(ctx context.Context) ([]advisor.Stored, error)
	// LastDone is when a run last concluded about each scope, and each group.
	LastDone(ctx context.Context) (map[string]time.Time, error)
	// Requested is every Run now nobody has run yet.
	Requested(ctx context.Context) ([]Request, error)
	// Record publishes one transition decided at now.
	Record(ctx context.Context, t advisor.Transition, now time.Time) error
	// RecordRun publishes how one run ended.
	RecordRun(ctx context.Context, r Run) error
	// Expire publishes the day's retention at now, once a day.
	Expire(ctx context.Context, now time.Time) error
}

// Request is a person's Run now.
type Request struct {
	Run string
	By  string
}

// Run is how one run ended, as the log keeps it.
type Run struct {
	ID        string
	Trigger   Trigger
	By        string
	StartedAt time.Time
	EndedAt   time.Time
	Status    RunStatus
	// Scopes is each scope the run concluded about, or could not read, with
	// how many findings each step took in it.
	Scopes []ScopeRun
	// Model is the model pass's summary, nil where none ran.
	Model any
}

// Count is how many findings of the run took step, every scope together.
func (r Run) Count(step advisor.Step) int {
	n := 0
	for _, s := range r.Scopes {
		n += s.Steps[step]
	}
	return n
}

// RunStatus is how a run ended.
type RunStatus string

// The statuses.
const (
	RunDone   RunStatus = "done"
	RunFailed RunStatus = "failed"
)

// ScopeRun is one scope as a run left it.
type ScopeRun struct {
	Scope  string
	Status ScopeStatus
	// Steps is how many of the scope's findings each step took.
	Steps map[advisor.Step]int
}

// ScopeStatus is whether a run concluded about a scope.
type ScopeStatus string

// The scope statuses.
const (
	ScopeDone   ScopeStatus = "done"
	ScopeUnread ScopeStatus = "unread"
)

// Duty runs the Advisor on the node that holds its lease.
type Duty struct {
	Log   Log
	Read  Reader
	Model ModelPass
	// Claim claims or renews the lease, nil on a node with no fleet.
	Claim func(ctx context.Context) (bool, error)
	// NewID mints a run's id.
	NewID func() string
	Now   func() time.Time
	Zone  func() *time.Location

	signature string
}

// Serve runs the duty until ctx ends.
func (d *Duty) Serve(ctx context.Context) {
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		if err := d.Step(ctx); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "advisor_duty_step_failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step is one tick: claim the lease, then run what is due.
func (d *Duty) Step(ctx context.Context) error {
	if d.Claim != nil {
		held, err := d.Claim(ctx)
		if err != nil {
			return fmt.Errorf("claim the advisor's lease: %w", err)
		}
		if !held {
			// A peer runs it, and this node's setup signature would be
			// stale when it next takes over: forget it.
			d.signature = ""
			return nil
		}
	}
	now := d.Now().UTC()
	requests, err := d.Log.Requested(ctx)
	if err != nil {
		return fmt.Errorf("read the requested runs: %w", err)
	}
	if len(requests) > 0 {
		// EVERY PRESS WAITING IS ANSWERED BY ONE RUN of everything, recorded
		// under each press's own id, so a press during a run joins the next.
		model := d.Model != nil && d.Model.Assigned()
		run := d.run(ctx, now, true, model, TriggerPerson, requests[0].By, requests[0].Run)
		var errs []error
		for _, r := range requests {
			run.ID, run.By = r.Run, r.By
			errs = append(errs, d.Log.RecordRun(ctx, run))
		}
		return errors.Join(append(errs, d.Log.Expire(ctx, now))...)
	}
	last, err := d.Log.LastDone(ctx)
	if err != nil {
		return fmt.Errorf("read when each group last ran: %w", err)
	}
	signature, err := d.Read.Signature(ctx)
	if err != nil {
		log.WarnContext(ctx, "advisor_setup_signature_unread", "error", err.Error())
	}
	trigger := TriggerSchedule
	rules := now.Sub(last[string(advisor.GroupEvidence)]) >= RulePassEvery ||
		now.Sub(last[string(advisor.GroupSetup)]) >= RulePassEvery
	if !rules && err == nil && signature != d.signature {
		rules, trigger = true, TriggerEvent
	}
	model := d.Model != nil && d.Model.Assigned() && last[string(advisor.GroupModel)].Before(d.slot(now))
	if !rules && !model {
		return nil
	}
	if err == nil {
		d.signature = signature
	}
	run := d.run(ctx, now, rules, model, trigger, "", d.NewID())
	return errors.Join(d.Log.RecordRun(ctx, run), d.Log.Expire(ctx, now))
}

// slot is the latest model pass start at or before now, on the company clock.
func (d *Duty) slot(now time.Time) time.Time {
	local := now.In(d.Zone())
	slot := time.Date(local.Year(), local.Month(), local.Day(), ModelPassHour, 0, 0, 0, local.Location())
	if slot.After(local) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot
}

// run runs the rule groups and the model pass as asked, reconciles each with
// the log, and answers the run's record.
func (d *Duty) run(ctx context.Context, now time.Time, rules, model bool, trigger Trigger, by, id string) Run {
	run := Run{ID: id, Trigger: trigger, By: by, StartedAt: now, Status: RunDone}
	defer func() { run.EndedAt = d.Now().UTC() }()
	reading := advisor.Reading{At: now}
	var observed []advisor.Observation
	if rules {
		o, err := d.Read.Observe(ctx, now)
		if err != nil {
			log.WarnContext(ctx, "advisor_rule_pass_unread", "error", err.Error())
			run.Status = RunFailed
		} else {
			reading.Groups = append(reading.Groups, advisor.GroupSetup, advisor.GroupEvidence)
			reading.Unread, reading.Held = o.Unread, o.Held
			observed = append(observed, o.Observations...)
		}
	}
	if model {
		observed = append(observed, d.modelPass(ctx, now, &reading, &run)...)
	}
	if len(reading.Groups) == 0 {
		return run
	}
	live, err := d.Log.Live(ctx)
	if err == nil {
		reading.LastDone, err = d.Log.LastDone(ctx)
	}
	if err != nil {
		log.WarnContext(ctx, "advisor_findings_unread", "error", err.Error())
		run.Status = RunFailed
		return run
	}
	run.Scopes = scopesOf(reading)
	for _, t := range advisor.Reconcile(live, observed, reading) {
		if err := d.Log.Record(ctx, t, now); err != nil {
			log.WarnContext(ctx, "advisor_finding_not_recorded", "step", string(t.Step), "error", err.Error())
			continue
		}
		f := t.Observed
		if f == nil {
			f = &t.Stored.Observation
		}
		scope := advisor.ScopeOf(f.Finding)
		for i := range run.Scopes {
			if run.Scopes[i].Scope == scope {
				run.Scopes[i].Steps[t.Step]++
			}
		}
	}
	return run
}

// modelPass runs the model, adding its group to reading where it concluded.
func (d *Duty) modelPass(ctx context.Context, now time.Time, reading *advisor.Reading, run *Run) []advisor.Observation {
	dismissed, err := d.Log.Dismissed(ctx)
	if err != nil {
		log.WarnContext(ctx, "advisor_dismissed_unread", "error", err.Error())
		return nil
	}
	pass, err := d.Model.Run(ctx, now, dismissed)
	run.Model = pass.Summary
	if err != nil {
		log.WarnContext(ctx, "advisor_model_pass_failed", "error", err.Error())
		return nil
	}
	if pass.Concluded {
		reading.Groups = append(reading.Groups, advisor.GroupModel)
	}
	return pass.Observations
}

// scopesOf is every scope of every group r ran, done or unread.
func scopesOf(r advisor.Reading) []ScopeRun {
	done := r.Done()
	var out []ScopeRun
	for _, g := range r.Groups {
		out = append(out, ScopeRun{Scope: string(g), Status: ScopeDone, Steps: map[advisor.Step]int{}})
		for _, scope := range advisor.ScopesOf(g) {
			if scope == string(g) {
				continue
			}
			status := ScopeUnread
			if slices.Contains(done, scope) {
				status = ScopeDone
			}
			out = append(out, ScopeRun{Scope: scope, Status: status, Steps: map[advisor.Step]int{}})
		}
	}
	return out
}
