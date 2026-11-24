package queries

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisor/modelpass"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/notice"
)

// AdvisorModelSummary is what the latest run's model pass did.
type AdvisorModelSummary struct {
	Status  advisor.PassStatus   `json:"status"`
	Tokens  int                  `json:"tokens"`
	Listed  int                  `json:"listed"`
	Dropped map[advisor.Drop]int `json:"dropped"`
	// Repaired counts the findings listed after a repair (a fix not built,
	// words reworded by the model or said by the engine), by the repair.
	Repaired map[advisor.Repair]int `json:"repaired"`
	Reason   string                 `json:"reason"`
}

// RunRunning is the status of a Run no run has answered yet.
const RunRunning = "running"

// AdvisorRunSummary is one run as a person reads it.
type AdvisorRunSummary struct {
	RunID     string     `json:"run_id"`
	Trigger   string     `json:"trigger"`
	By        string     `json:"by"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	// Status is running, done, failed, ceiling or interrupted (a deep run
	// whose holder stopped during it, which concluded nothing).
	Status string `json:"status"`
	// ClassesRun is how many scopes the run concluded about, and Unread the
	// sources it could not read, by their words.
	ClassesRun int                  `json:"classes_run"`
	Unread     []string             `json:"unread"`
	Opened     int                  `json:"opened"`
	Resolved   int                  `json:"resolved"`
	Model      *AdvisorModelSummary `json:"model"`
	// Kind is "rules" for a run of the detectors (with the model pass where
	// one ran), "deep" for an investigation (ADR-0166).
	Kind string `json:"kind"`
	// Progress is a deep run's progress lines, oldest first; [] otherwise.
	Progress []AdvisorProgress `json:"progress"`
	// Budget is what a deep run spent of its bounds; nil otherwise.
	Budget *AdvisorRunBudget `json:"budget"`
	// StoppedBy is the bound a deep run stopped at, or "done"; nil otherwise.
	StoppedBy *string `json:"stopped_by"`
	// Agenda is how far a deep run is through the questions it has to
	// answer, in flight or ended; nil for a rules run, or one that kept none.
	Agenda *AdvisorAgenda `json:"agenda"`
	// Classes is what the run concluded about each class: found, clear,
	// silent (with why) or unread; [] for a run that ran no rules.
	Classes []AdvisorClassOutcome `json:"classes"`
	// DeepSkipped is why a press's deep part did not run: "kept" (an
	// investigation from DeepKeptFrom stands) or "no_model"; nil where it
	// ran, or for a run that has none.
	DeepSkipped  *string    `json:"deep_skipped"`
	DeepKeptFrom *time.Time `json:"deep_kept_from"`
}

// AdvisorProgress is one step of a deep run, as its progress line says it.
type AdvisorProgress struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	// Step is note, read, verdict, question or checks; a line written before
	// steps is a note.
	Step string `json:"step"`
	// State is done, failed or running: running only for the newest note,
	// read or checks of a run still in flight.
	State string `json:"state"`
	// Detail is what the step asked: a read's call, a verdict's item and
	// question; Why a failed step's reason. "" where none.
	Detail string `json:"detail"`
	Why    string `json:"why"`
}

// The states a step reads in.
const (
	StepDone    = "done"
	StepFailed  = "failed"
	StepRunning = "running"
)

// stepsOf is lines as a run's steps, oldest first. A failed read folds into
// the newest earlier read of the same call with no outcome yet, so each read
// is one step; running says the run is still in flight.
func stepsOf(lines []advisordomain.ProgressLine, running bool) []AdvisorProgress {
	out := make([]AdvisorProgress, 0, len(lines))
	// open is each read step begun with no outcome yet, by its index.
	open := map[int]bool{}
	for _, l := range lines {
		step := l.Step
		if step == "" {
			step = advisordomain.ProgressNote
		}
		p := AdvisorProgress{At: l.At, Text: l.Text, Step: string(step), State: StepDone, Detail: l.Detail, Why: l.Why}
		if l.Failed {
			p.State = StepFailed
		}
		if l.Failed && step == advisordomain.ProgressRead {
			began := -1
			for i := len(out) - 1; i >= 0; i-- {
				if open[i] && out[i].Detail == l.Detail {
					began = i
					break
				}
			}
			if began >= 0 {
				out[began], open[began] = p, false
				continue
			}
		}
		open[len(out)] = step == advisordomain.ProgressRead && !l.Failed
		out = append(out, p)
	}
	if last := len(out) - 1; running && last >= 0 && out[last].State == StepDone {
		switch advisordomain.ProgressStep(out[last].Step) {
		case advisordomain.ProgressNote, advisordomain.ProgressRead, advisordomain.ProgressChecks:
			out[last].State = StepRunning
		}
	}
	return out
}

// AdvisorAgenda is a deep run's questions, and how many it has answered.
type AdvisorAgenda struct {
	Items    int `json:"items"`
	Answered int `json:"answered"`
}

// agendaOf is items as a run's summary counts them.
func agendaOf(items []modelpass.AgendaItem) *AdvisorAgenda {
	if len(items) == 0 {
		return nil
	}
	out := &AdvisorAgenda{Items: len(items)}
	for _, it := range items {
		switch it.Verdict {
		case modelpass.VerdictConfirmed, modelpass.VerdictRefuted, modelpass.VerdictUnreadable:
			out.Answered++
		}
	}
	return out
}

// AdvisorRunBudget is what a deep run spent of its bounds.
type AdvisorRunBudget struct {
	Tokens         int64 `json:"tokens"`
	TokensCeiling  int64 `json:"tokens_ceiling"`
	Minutes        int64 `json:"minutes"`
	MinutesCeiling int64 `json:"minutes_ceiling"`
}

// AdvisorClassOutcome is what a run concluded about one class.
type AdvisorClassOutcome struct {
	Class  string          `json:"class"`
	Status advisor.Outcome `json:"status"`
	Reason *string         `json:"reason"`
}

// RunKindRules is a run of the detectors, the model pass beside them where one ran.
const RunKindRules = "rules"

// advisorRun answers `advisor_run`: the latest run, or a Run waiting for
// the duty's next tick, which reads as running.
func (s Sources) advisorRun(ctx context.Context, _ Params) (any, error) {
	run, err := s.latestRun(ctx)
	if err != nil || run == nil {
		return nil, err
	}
	return *run, nil
}

// latestRun is the run a person reads as the latest: a deep run in flight
// with its progress so far, a Run waiting for the duty's next tick, which
// reads as running, or the run that ended last; nil where none ran. A run
// heard from before the last run ended, or not for longer than a deep run
// lasts, is not in flight: its holder stopped, and the next one closes it.
func (s Sources) latestRun(ctx context.Context) (*AdvisorRunSummary, error) {
	live, lines, flying, err := s.Advisor.LiveProgress(ctx, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	pending, err := s.Advisor.PendingRequests(ctx, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	// HOW FAR IT IS, from the checkpoint it keeps as it answers its agenda.
	var agenda *AdvisorAgenda
	if flying {
		body, found, cerr := s.Advisor.Checkpoint(ctx, live, advisorFresh)
		if cerr != nil {
			return nil, advisorUnreadable(cerr)
		}
		var kept modelpass.Checkpoint
		if found && json.Unmarshal(body, &kept) == nil {
			agenda = agendaOf(kept.Agenda)
		}
	}
	progress := func(out *AdvisorRunSummary) *AdvisorRunSummary {
		if flying && out.RunID == live {
			out.Kind = string(advisordomain.RunKindDeep)
			out.Agenda = agenda
			out.Progress = stepsOf(lines, true)
		}
		return out
	}
	if len(pending) > 0 {
		p := pending[0]
		return progress(&AdvisorRunSummary{RunID: p.ID, Trigger: string(advisordomain.TriggerPerson), By: p.By,
			StartedAt: p.RequestedAt, Status: RunRunning, Unread: []string{}, Kind: string(p.Kind.OrRules()),
			Progress: []AdvisorProgress{}, Classes: []AdvisorClassOutcome{}}), nil
	}
	row, found, err := s.Advisor.LatestRun(ctx, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	if flying && len(lines) > 0 {
		heard := lines[len(lines)-1].At
		if (!found || !heard.Before(row.EndedAt)) && s.clock().Sub(heard) < duty.DeepLongest {
			// A DEEP RUN AT ITS SLOT, which nobody asked for: its first
			// line is as early as the log says it began.
			return progress(&AdvisorRunSummary{RunID: live, Trigger: string(advisordomain.TriggerSchedule),
				StartedAt: lines[0].At, Status: RunRunning, Unread: []string{}, Progress: []AdvisorProgress{},
				Classes: []AdvisorClassOutcome{}}), nil
		}
	}
	if !found {
		return nil, nil
	}
	out, err := runSummary(row)
	if err != nil || row.Kind != advisordomain.RunKindDeep {
		return out, err
	}
	// A FINISHED RUN'S WHOLE TRAIL is on its progress record, kept with the
	// run: every read it asked and the verdict on each agenda item. Its own
	// record keeps only its last lines, which stand where the record holds
	// no line (a run older than its lines' retention).
	trail, err := s.Advisor.RunProgress(ctx, row.ID, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	if len(trail) > 0 {
		out.Progress = stepsOf(trail, false)
	}
	return out, nil
}

// runSummary is row as a person reads it.
func runSummary(row advisordomain.RunRow) (*AdvisorRunSummary, error) {
	out := &AdvisorRunSummary{RunID: row.ID, Trigger: string(row.Trigger), By: row.By, StartedAt: row.StartedAt,
		EndedAt: instant(row.EndedAt), Status: string(row.Status), Unread: []string{}, Kind: RunKindRules,
		Progress: []AdvisorProgress{}, Classes: []AdvisorClassOutcome{}}
	var scopes []duty.ScopeRecord
	if len(row.Classes) > 0 {
		if err := json.Unmarshal(row.Classes, &scopes); err != nil {
			return nil, fmt.Errorf("queries: run %s's classes do not decode: %w", row.ID, err)
		}
	}
	for _, s := range scopes {
		if s.Outcome != "" {
			// A CLASS'S LINE, not a scope's: what the run concluded about it.
			c := AdvisorClassOutcome{Class: s.Class, Status: s.Outcome}
			if s.Reason != "" {
				c.Reason = &s.Reason
			}
			out.Classes = append(out.Classes, c)
			continue
		}
		out.Opened += s.Opened
		out.Resolved += s.Resolved
		switch s.Status {
		case advisordomain.ClassDone:
			out.ClassesRun++
		case advisordomain.ClassUnread:
			if _, word, ok := strings.Cut(s.Class, "/"); ok && !slices.Contains(out.Unread, word) {
				out.Unread = append(out.Unread, word)
			}
		}
	}
	out.Kind = string(row.Kind.OrRules())
	if len(row.Model) > 0 && string(row.Model) != "null" {
		// A DEEP RUN'S RECORD is a pass with its progress, bounds and stop.
		var pass modelpass.DeepRun
		if err := json.Unmarshal(row.Model, &pass); err != nil {
			return nil, fmt.Errorf("queries: run %s's model pass does not decode: %w", row.ID, err)
		}
		out.Model = &AdvisorModelSummary{Status: pass.Status, Tokens: pass.InputTokens + pass.OutputTokens,
			Listed: pass.Listed, Dropped: nonNilMap(pass.Dropped), Repaired: nonNilMap(pass.Repaired),
			Reason: pass.Reason}
		if row.Kind == advisordomain.RunKindDeep {
			kept := make([]advisordomain.ProgressLine, 0, len(pass.Progress))
			for _, p := range pass.Progress {
				kept = append(kept, advisordomain.ProgressLine{At: p.At, Text: p.Text, Step: p.Step,
					Failed: p.Failed, Detail: p.Detail, Why: p.Why})
			}
			out.Progress = stepsOf(kept, false)
			// A DEEP PART THAT DID NOT RUN spent nothing of bounds it never
			// had, and one interrupted kept no measure of what it spent.
			switch {
			case pass.Skipped != "":
				out.DeepSkipped, out.DeepKeptFrom = &pass.Skipped, pass.KeptFrom
			case row.Status == advisordomain.RunInterrupted:
			default:
				out.Budget = &AdvisorRunBudget{Tokens: int64(pass.InputTokens + pass.OutputTokens),
					TokensCeiling: int64(pass.Bounds.Tokens), Minutes: int64(math.Ceil(pass.Minutes)),
					MinutesCeiling: int64(pass.Bounds.Minutes)}
			}
			if pass.StoppedBy != "" {
				out.StoppedBy = &pass.StoppedBy
			}
			if t := pass.Tally; t != nil && t.Items > 0 {
				out.Agenda = &AdvisorAgenda{Items: t.Items, Answered: t.Items - t.Open}
			}
		}
	}
	return out, nil
}

func nonNilMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
}

// UnreadFindings is the unread word of a node that could not read the advisor
// log itself, so the notices of its blocking findings could not be said.
const UnreadFindings = "findings"

// UnreadGrowthSignals is the unread word of a node that could not read the
// fleet's status records, so what the growth products met at their vendors
// could not be said; the build's own API retirements still are.
const UnreadGrowthSignals = "growth_signals"

// growthNotices is the notices of what the growth products meet: this build's
// catalogs' retirements, and every live node's growth signals off its status
// record; false where those records cannot be read.
func (s Sources) growthNotices(ctx context.Context, now time.Time) ([]notice.Notice, bool) {
	catalogs := growth.Catalogs()
	if s.Coord == nil || s.NodeStatuses == nil {
		return notice.FromCatalogs(catalogs, nil, now), true
	}
	_, records, err := s.liveStatuses(ctx, coord.NodeStatusRead{Growth: true})
	if err != nil {
		log.WarnContext(ctx, "advisor_growth_signals_unread", "error", err.Error())
		return notice.FromCatalogs(catalogs, nil, now), false
	}
	var nodes []notice.NodeGrowth
	for node, r := range records {
		if r.Growth != nil {
			nodes = append(nodes, notice.NodeGrowth{Node: node, Signals: *r.Growth})
		}
	}
	slices.SortFunc(nodes, func(a, b notice.NodeGrowth) int { return strings.Compare(a.Node, b.Node) })
	return slices.Concat(notice.FromCatalogs(catalogs, nodes, now), notice.FromClockSkews(nodes)), true
}

// AdvisorNotices is `advisor_notices`: the platform notices the Advisor's
// blocking findings, the fleet's alarms and the held conversations raise
// (ADR-0116), and what the latest run could not read.
type AdvisorNotices struct {
	Notices []notice.Notice `json:"notices"`
	Unread  []string        `json:"unread"`
}

// advisorNotices answers `advisor_notices`. A SOURCE THAT FAILS IS NAMED,
// never the whole answer lost: an alarm is said whether or not the advisor
// log could be read.
func (s Sources) advisorNotices(ctx context.Context, _ Params) (any, error) {
	now := s.clock()
	fleet, fleetKnown := s.fleetAlarms(ctx)
	held, heldKnown := s.heldConversations(ctx)
	out := AdvisorNotices{Unread: []string{}}
	var listed []notice.Listed
	rows, err := s.Advisor.NonClosed(ctx, advisorFresh)
	if err == nil {
		listed, err = noticeable(rows, now)
	}
	if err == nil {
		var run *AdvisorRunSummary
		if run, err = s.latestRun(ctx); err == nil && run != nil {
			out.Unread = append(out.Unread, run.Unread...)
		}
	}
	if err != nil {
		log.WarnContext(ctx, "advisor_findings_unread", "error", err.Error())
		out.Unread = append(out.Unread, UnreadFindings)
	}
	if !fleetKnown && !slices.Contains(out.Unread, advisor.UnreadAlarms) {
		out.Unread = append(out.Unread, advisor.UnreadAlarms)
	}
	if !heldKnown {
		out.Unread = append(out.Unread, advisor.UnreadHolds)
	}
	findings := make([]advisor.Finding, 0, len(listed))
	for _, l := range listed {
		findings = append(findings, l.Finding)
	}
	vendors, vendorsKnown := s.growthNotices(ctx, now)
	if !vendorsKnown {
		out.Unread = append(out.Unread, UnreadGrowthSignals)
	}
	out.Notices = notice.WithFindings(notice.Read(findings, fleet, held, vendors, now), listed)
	return out, nil
}

// noticeable is every live finding a notice may be raised for: open, and
// neither snoozed at now nor applied and waiting on its effect.
func noticeable(rows []advisordomain.FindingRow, now time.Time) ([]notice.Listed, error) {
	var out []notice.Listed
	for _, r := range rows {
		if advisor.State(r.State) != advisor.StateOpen || r.SnoozedUntil.After(now) {
			continue
		}
		var o advisor.Observation
		if err := json.Unmarshal(r.Body, &o); err != nil {
			return nil, fmt.Errorf("queries: finding %s does not decode: %w", r.ID, err)
		}
		out = append(out, notice.Listed{ID: r.ID, Finding: o.Finding})
	}
	return out, nil
}
