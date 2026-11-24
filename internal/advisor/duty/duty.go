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
// person's Run is a request on the log, which the holder takes on its next
// tick, so a press on any node runs on the one that holds the duty.
//
// # A run whose holder stopped
//
// A deep run at its slot has no request row, so one whose holder stopped
// mid-run (a restart, a lost lease) would read as running for ever. Every tick
// that holds the lease and runs no deep run closes each run that wrote progress
// and has no row ([Log.Unowned]): a run this holder keeps for its record is
// recorded again; the newest begun within the current slot, while a deep run is
// still due, is taken over under its own id and resumes from its checkpoint;
// any other is recorded interrupted, from its first line to its last. A
// person's press keeps its request, which the next holder runs (ADR-0166).
//
// A holder writes only what it provably owns: its last successful claim is
// younger than [LeaseTTL]. A renewal that fails for that long stops its deep
// run, and a deep run ending past it records nothing, for the next holder.
//
// # Every step has a deadline
//
// The rule pass's reading ([ObserveFor]), the daily model pass
// ([ModelPassFor]) and a report's summary ([SummaryFor]) each have one, and a
// deep run's reads before its loop take [ObserveFor] while its loop takes its
// own minutes. Every read and write of the log is the estate router's, bounded
// a node at a time (internal/estate's ReadAttempt and writeAttempt). A step
// past its deadline says so on its record: a rules run fails with the reason
// on every class, a model pass's summary gives it, and a report is written
// without its summary.
//
// # What it never does
//
// It calls no model in a detector and applies no fix: a fix is sent by a
// person after the engine re-checks it (internal/advisor's Recheck, ADR-0115).
package duty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
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

// The deadlines of what a tick runs (package doc, "Every step has a deadline").
const (
	// ObserveFor bounds one reading of the company: fifteen-odd sources read one
	// after another, each within the estate's 10 s attempt, take 150 s at worst,
	// and the atlas's repositories the rest.
	ObserveFor = 5 * time.Minute
	// ModelPassFor bounds the daily model pass: MaxRounds (16) rounds at 30 s,
	// a round over a busy week's 70k tokens, is eight minutes, and its reads
	// before them the rest.
	ModelPassFor = 10 * time.Minute
	// SummaryFor bounds a report's summary: three rounds over a report of a few
	// thousand tokens, a minute each at most.
	SummaryFor = 3 * time.Minute
)

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
	// Holds asks a finding's premise again: whether it holds now.
	Holds(ctx context.Context, p advisor.Premise) (bool, error)
	// Week reads the week [from, to) a report is written for: its numbers,
	// and the sources it could not read; [advisor.ErrWeekUnmeasured] for a
	// week the company's records do not reach back over all of.
	Week(ctx context.Context, from, to time.Time) ([]advisor.Metric, []string, error)
}

// DayReader is a [Reader] that reads a week day by day as well: each company
// day's turns, those that committed nothing and their tokens, which a report
// draws across its week. A reader without it writes reports with no days.
type DayReader interface {
	Days(ctx context.Context, from, to time.Time) ([]advisor.Day, error)
}

// Observed is one reading of the company.
type Observed struct {
	Observations []advisor.Observation
	Unread       []string
	Held         func(advisor.Subject) bool
	// Silent is each class that read its sources and could not judge all of
	// the company, with why.
	Silent []advisor.Silence
	// Partial is each surface whose findings this reading holds only in part
	// ([advisor.Answer.Partial]).
	Partial []advisor.Cut
}

// ModelPass is the Advisor's model pass, nil where none runs on this node.
type ModelPass interface {
	// Assigned reports whether the company assigns the Advisor a model.
	Assigned() bool
	// Run is one pass at now: what it found, grounded, the explanations it
	// wrote for live's open findings, and its summary.
	Run(ctx context.Context, now time.Time, dismissed, live []advisor.Stored) (ModelRun, error)
	// Summarize is the model's summary of a week's report, checked against
	// its numbers; nil where none was written.
	Summarize(ctx context.Context, now time.Time, r advisor.Report) (*string, error)
}

// ModelRun is how one model pass ended.
type ModelRun struct {
	Observations []advisor.Observation
	// Summary is the pass's record as a run keeps it: status, tokens, drops.
	Summary any
	// Concluded reports a pass that read and submitted, so what it did not
	// list again is no longer its finding.
	Concluded bool
	// Explained are the explanations the engine checked, each for the
	// verdict it was written for.
	Explained []advisor.Explained
	// Partial reports a pass its bound cut short that listed what it had:
	// its findings open or refresh, and nothing it did not list closes.
	Partial bool
}

// Log is the advisor log as the duty reads and writes it.
type Log interface {
	// Live is every open and applied finding.
	Live(ctx context.Context) ([]advisor.Stored, error)
	// Dismissed is every dismissed model finding still held quiet.
	Dismissed(ctx context.Context) ([]advisor.Stored, error)
	// LastDone is when a run last concluded about each scope, and each group.
	LastDone(ctx context.Context) (map[string]time.Time, error)
	// Requested is every Run nobody has run yet.
	Requested(ctx context.Context) ([]Request, error)
	// Record publishes one transition decided at now.
	Record(ctx context.Context, t advisor.Transition, now time.Time) error
	// RecordRun publishes how one run ended.
	RecordRun(ctx context.Context, r Run) error
	// Expire publishes the day's retention at now, once a day.
	Expire(ctx context.Context, now time.Time) error
	// Report is the report of week, nil where none was written.
	Report(ctx context.Context, week string) (*advisor.Report, error)
	// Tally counts the findings of [from, to).
	Tally(ctx context.Context, from, to time.Time) (advisor.Tally, error)
	// WriteReport writes a week's report, once.
	WriteReport(ctx context.Context, r advisor.Report) error
	// Unowned is every run that wrote progress and has no row (no request,
	// no record): a deep run at its slot whose holder stopped mid-run.
	Unowned(ctx context.Context) ([]Unowned, error)
	// Progress writes one line of a deep run's progress.
	Progress(ctx context.Context, run string, l Line) error
	// Checkpoint writes where a deep run stands, which a holder taking it
	// over resumes from.
	Checkpoint(ctx context.Context, run string, at time.Time, body json.RawMessage) error
	// Resume is the newest checkpoint run wrote, nil where it wrote none.
	Resume(ctx context.Context, run string) (json.RawMessage, error)
	// Explain writes the model's explanation of a finding's verdict; one
	// whose verdict moved since is left for the next pass.
	Explain(ctx context.Context, e advisor.Explained) error
}

// Line is one line of a deep run's progress: when, what it says, what step it
// is, whether that step failed and why, and what it asked (a read's call, a
// verdict's item and question).
type Line struct {
	At     time.Time
	Text   string
	Step   advisordomain.ProgressStep
	Failed bool
	Detail string
	Why    string
}

// Unowned is a run whose holder stopped mid-run, and when it was first and
// last heard from: its first and newest progress lines.
type Unowned struct {
	Run         string
	First, Last time.Time
}

// Request is a person's Run.
type Request struct {
	Run string
	By  string
	// Kind is what the person asked to run: the rules, or a deep run.
	Kind advisordomain.RunKind
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
	// Classes is what the rule pass concluded about each class, nil where
	// none ran (ADR-0166).
	Classes []advisor.ClassOutcome
	// Model is the model pass's summary, nil where none ran.
	Model any
	// Kind is what the run did: the rules (the empty kind), or a deep run.
	Kind advisordomain.RunKind
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
	// RunInterrupted is a deep run whose holder stopped during it.
	RunInterrupted RunStatus = "interrupted"
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
	// Deep is the Advisor's investigation, nil where none runs (ADR-0166).
	Deep DeepPass
	// Claim claims or renews the lease, nil on a node with no fleet.
	Claim func(ctx context.Context) (bool, error)
	// NewID mints a run's id.
	NewID func() string
	Now   func() time.Time
	Zone  func() *time.Location

	signature string
	// reported is the week whose report this holder saw written.
	reported string
	// deep is the deep run in flight on this holder.
	deep deepState
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
		at := d.Now().UTC()
		held, err := d.Claim(ctx)
		if err != nil {
			// UNRENEWED FOR ITS TTL the lease may be a peer's, which closes
			// or resumes the deep run here: it stops.
			if !d.holds(d.Now().UTC()) {
				d.cancelDeep()
			}
			return fmt.Errorf("claim the advisor's lease: %w", err)
		}
		if !held {
			// A peer runs it, and this node's setup signature would be
			// stale when it next takes over: forget it. A deep run in
			// flight here is the new holder's to run again.
			d.signature = ""
			d.claimed(time.Time{})
			d.cancelDeep()
			return nil
		}
		d.claimed(at)
	}
	if d.deepFlying() {
		// THE LEASE IS RENEWED, and nothing else runs beside the deep run.
		return nil
	}
	now := d.Now().UTC()
	kept := d.recordKept(ctx)
	all, err := d.Log.Requested(ctx)
	if err != nil {
		return errors.Join(kept, fmt.Errorf("read the requested runs: %w", err))
	}
	if len(all) > 0 {
		return errors.Join(kept, d.pressed(ctx, now, all))
	}
	last, err := d.Log.LastDone(ctx)
	if err != nil {
		return errors.Join(kept, fmt.Errorf("read when each group last ran: %w", err))
	}
	resumed, unowned := d.closeUnowned(ctx, now, last)
	if resumed {
		return errors.Join(kept, unowned)
	}
	kept = errors.Join(kept, unowned)
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
	if d.deepDue(now, last) {
		d.startDeep(ctx, now, TriggerSchedule, "", []string{d.NewID()}, nil)
		return kept
	}
	model := d.Model != nil && d.Model.Assigned() && last[string(advisor.GroupModel)].Before(d.slot(now))
	if !rules && !model {
		return errors.Join(kept, d.report(ctx, now))
	}
	if err == nil {
		d.signature = signature
	}
	run := d.run(ctx, now, rules, model, trigger, "", d.NewID())
	return errors.Join(kept, d.conclude(ctx, run), d.Log.Expire(ctx, now), d.report(ctx, now))
}

// report writes the last whole week's report once it is due: on the Monday
// after it at [advisor.ReportHour], after the run that hour's step made, so the
// model's pass has run first. A week whose report is written is left alone.
func (d *Duty) report(ctx context.Context, now time.Time) error {
	week, from, to, due := advisor.ReportWeek(now, d.Zone())
	if now.Before(due) || d.reported == week {
		return nil
	}
	written, err := d.Log.Report(ctx, week)
	if err != nil {
		return fmt.Errorf("read the report of %s: %w", week, err)
	}
	if written != nil {
		d.reported = week
		return nil
	}
	metrics, unread, err := d.Read.Week(ctx, from, to)
	switch {
	case errors.Is(err, advisor.ErrWeekUnmeasured):
		// A WEEK BEFORE THE COMPANY, or one it began within: its numbers
		// would read as a measured week, so it has no report.
		log.InfoContext(ctx, "advisor_report_week_unmeasured", "week", week)
		d.reported = week
		return nil
	case err != nil:
		return fmt.Errorf("read the week %s: %w", week, err)
	}
	tally, err := d.Log.Tally(ctx, from, to)
	if err != nil {
		return fmt.Errorf("count the findings of %s: %w", week, err)
	}
	live, err := d.Log.Live(ctx)
	if err != nil {
		return fmt.Errorf("read the open findings for %s: %w", week, err)
	}
	before, _, _, _ := advisor.ReportWeek(from, d.Zone())
	last, err := d.Log.Report(ctx, before)
	if err != nil {
		return fmt.Errorf("read the report of %s: %w", before, err)
	}
	r := advisor.Report{Week: week, From: from, To: to, Metrics: advisor.Against(metrics, last),
		Unread: unread, Tally: tally, Top: advisor.TopOf(live, to), Days: d.days(ctx, week, from, to)}
	// THE MODEL'S SUMMARY, where the company assigns one, from the report
	// alone: a report is written without it rather than not at all.
	if d.Model != nil && d.Model.Assigned() {
		summaryCtx, cancel := deadline(ctx, SummaryFor, "the report's summary")
		summary, err := d.Model.Summarize(summaryCtx, now, r)
		cancel()
		if err != nil {
			log.WarnContext(ctx, "advisor_report_summary_not_written", "week", week, "error", err.Error())
		}
		r.Summary = summary
	}
	if err := d.Log.WriteReport(ctx, r); err != nil {
		return fmt.Errorf("write the report of %s: %w", week, err)
	}
	d.reported = week
	return nil
}

// days is the week [from, to) day by day, where the reader reads days: a
// picture of the week's totals, so a report is written without it rather than
// not at all.
func (d *Duty) days(ctx context.Context, week string, from, to time.Time) []advisor.Day {
	reader, ok := d.Read.(DayReader)
	if !ok {
		return nil
	}
	days, err := reader.Days(ctx, from, to)
	if err != nil {
		log.WarnContext(ctx, "advisor_report_days_unread", "week", week, "error", err.Error())
		return nil
	}
	return days
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
// the log, and answers the run's record, ended when it returns. THE RESULT IS
// NAMED so the deferred end lands in what is returned: an unnamed one is copied
// before the defer runs, and a run with no end is one the log refuses.
func (d *Duty) run(ctx context.Context, now time.Time, rules, model bool, trigger Trigger, by, id string) (run Run) {
	run = Run{ID: id, Trigger: trigger, By: by, StartedAt: now, Status: RunDone}
	defer func() {
		// A clock stepped back mid-run still ends it no earlier than it began.
		if run.EndedAt = d.Now().UTC(); run.EndedAt.Before(run.StartedAt) {
			run.EndedAt = run.StartedAt
		}
	}()
	reading := advisor.Reading{At: now}
	var observed []advisor.Observation
	if rules {
		observeCtx, cancel := deadline(ctx, ObserveFor, "the rule pass's reading")
		o, err := d.Read.Observe(observeCtx, now)
		err = causeOf(observeCtx, err)
		cancel()
		if err != nil {
			// EVERY CLASS UNJUDGED, saying why, so the failed run says it.
			log.WarnContext(ctx, "advisor_rule_pass_unread", "error", err.Error())
			run.Status = RunFailed
			run.Classes = unjudged("The rules could not read the company: " + err.Error())
		} else {
			reading.Groups = append(reading.Groups, advisor.GroupSetup, advisor.GroupEvidence)
			reading.Unread, reading.Held, reading.Cuts = o.Unread, o.Held, o.Partial
			observed = append(observed, o.Observations...)
			found := make([]advisor.Finding, len(o.Observations))
			for i, ob := range o.Observations {
				found[i] = ob.Finding
			}
			run.Classes = advisor.Outcomes(found, o.Unread, o.Silent)
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
	moved := map[string]bool{}
	for _, t := range advisor.Reconcile(live, observed, reading) {
		if err := d.Log.Record(ctx, t, now); err != nil {
			log.WarnContext(ctx, "advisor_finding_not_recorded", "step", string(t.Step), "error", err.Error())
			continue
		}
		if t.Stored != nil {
			moved[t.Stored.ID] = true
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
	if rules {
		d.closeDuplicates(ctx, now, live, observed, moved)
		run.Scopes = append(run.Scopes, d.recheckPremises(ctx, now, live, observed, moved))
	}
	return run
}

// closeDuplicates closes every open model finding a rule's finding of this
// run already says with its numbers ([advisor.Restates]), as a duplicate.
func (d *Duty) closeDuplicates(ctx context.Context, now time.Time, live []advisor.Stored,
	observed []advisor.Observation, moved map[string]bool) {
	for i := range live {
		s := &live[i]
		if moved[s.ID] || s.State != advisor.StateOpen || s.Observation.Finding.Source != advisor.SourceModel {
			continue
		}
		if !slices.ContainsFunc(observed, func(o advisor.Observation) bool {
			return advisor.Restates(s.Observation.Finding, o.Finding)
		}) {
			continue
		}
		t := advisor.Transition{Step: advisor.StepClose, Stored: s, State: advisor.StateRetired,
			Reason: advisor.ReasonDuplicate}
		if err := d.Log.Record(ctx, t, now); err != nil {
			log.WarnContext(ctx, "advisor_finding_not_recorded", "step", string(t.Step), "error", err.Error())
			continue
		}
		moved[s.ID] = true
	}
}

// PremiseScope is the scope a rules run records once it asked every open
// finding's premise again, which a finding's premise_checked_at reads.
const PremiseScope = "premise"

// recheckPremises asks again the premise of every open finding that states
// one and this run did not move, and closes each whose premise no longer
// holds (ADR-0166). A premise that could not be asked is left as it stands.
//
// A MOVED PREMISE IS NOT A SOLVED PROBLEM: the number a finding rested on
// can move while what it was about still stands (the blockers changed hands,
// the items are still blocked). Where a rule's finding of this run is about
// the same work, the finding is replaced by it rather than resolved.
func (d *Duty) recheckPremises(ctx context.Context, now time.Time, live []advisor.Stored,
	observed []advisor.Observation, moved map[string]bool) ScopeRun {
	scope := ScopeRun{Scope: PremiseScope, Status: ScopeDone, Steps: map[advisor.Step]int{}}
	for i := range live {
		s := &live[i]
		p := s.Observation.Finding.Premise
		if p == nil || moved[s.ID] || s.State != advisor.StateOpen {
			continue
		}
		holds, err := d.Read.Holds(ctx, *p)
		if err != nil {
			log.WarnContext(ctx, "advisor_premise_unread", "finding", s.ID, "error", err.Error())
			scope.Status = ScopeUnread
			continue
		}
		if holds {
			continue
		}
		t := advisor.Transition{Step: advisor.StepClose, Stored: s, State: advisor.StateResolved,
			Reason: advisor.ReasonConditionCleared}
		if slices.ContainsFunc(observed, func(o advisor.Observation) bool {
			return advisor.AboutTheSameWork(s.Observation.Finding, o.Finding)
		}) {
			t.State, t.Reason = advisor.StateRetired, advisor.ReasonReplaced
		}
		if err := d.Log.Record(ctx, t, now); err != nil {
			log.WarnContext(ctx, "advisor_finding_not_recorded", "step", string(t.Step), "error", err.Error())
			continue
		}
		scope.Steps[advisor.StepClose]++
	}
	return scope
}

// modelPass runs the model, adding its group to reading where it concluded.
func (d *Duty) modelPass(ctx context.Context, now time.Time, reading *advisor.Reading, run *Run) []advisor.Observation {
	dismissed, err := d.Log.Dismissed(ctx)
	if err != nil {
		log.WarnContext(ctx, "advisor_dismissed_unread", "error", err.Error())
		return nil
	}
	live, err := d.Log.Live(ctx)
	if err != nil {
		log.WarnContext(ctx, "advisor_findings_unread", "error", err.Error())
		return nil
	}
	passCtx, cancel := deadline(ctx, ModelPassFor, "the model pass")
	defer cancel()
	pass, err := d.Model.Run(passCtx, now, dismissed, live)
	run.Model = pass.Summary
	if err != nil {
		log.WarnContext(ctx, "advisor_model_pass_failed", "error", err.Error())
		return nil
	}
	for _, e := range pass.Explained {
		if err := d.Log.Explain(ctx, e); err != nil {
			log.WarnContext(ctx, "advisor_explanation_not_recorded", "finding", e.ID, "error", err.Error())
		}
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

// unjudged is every rule class unread, saying why: a reading that failed
// judged none of them.
func unjudged(why string) []advisor.ClassOutcome {
	out := advisor.Outcomes(nil, nil, nil)
	for i := range out {
		out[i].Outcome, out[i].Reason = advisor.OutcomeUnread, cutWords(why)
	}
	return out
}

// deadline is ctx ending within limit, its cause naming the step that ran out
// of time, which the step's record says ([causeOf]).
func deadline(ctx context.Context, limit time.Duration, step string) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, limit,
		fmt.Errorf("%s did not finish within %d minutes", step, int(limit/time.Minute)))
}

// causeOf is err, or the deadline's cause where ctx's deadline ended the step.
func causeOf(ctx context.Context, err error) error {
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return context.Cause(ctx)
	}
	return err
}
