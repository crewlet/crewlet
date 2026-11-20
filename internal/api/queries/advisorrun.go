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
	"github.com/crewlet/crewlet/internal/notice"
)

// AdvisorModelSummary is what the latest run's model pass did.
type AdvisorModelSummary struct {
	Status  advisor.PassStatus   `json:"status"`
	Tokens  int                  `json:"tokens"`
	Listed  int                  `json:"listed"`
	Dropped map[advisor.Drop]int `json:"dropped"`
	Reason  string               `json:"reason"`
}

// RunRunning is the status of a Run now no run has answered yet.
const RunRunning = "running"

// AdvisorRunSummary is one run as a person reads it.
type AdvisorRunSummary struct {
	RunID     string     `json:"run_id"`
	Trigger   string     `json:"trigger"`
	By        string     `json:"by"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	// Status is running, done, failed or ceiling.
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
	// Classes is what the run concluded about each class: found, clear,
	// silent (with why) or unread; [] for a run that ran no rules.
	Classes []AdvisorClassOutcome `json:"classes"`
}

// AdvisorProgress is one line of a deep run's progress.
type AdvisorProgress struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
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

// advisorRun answers `advisor_run`: the latest run, or a Run now waiting for
// the duty's next tick, which reads as running.
func (s Sources) advisorRun(ctx context.Context, _ Params) (any, error) {
	run, err := s.latestRun(ctx)
	if err != nil || run == nil {
		return nil, err
	}
	return *run, nil
}

// latestRun is the run a person reads as the latest: a deep run in flight
// with its progress so far, a Run now waiting for the duty's next tick, which
// reads as running, or the run that ended last; nil where none ran.
func (s Sources) latestRun(ctx context.Context) (*AdvisorRunSummary, error) {
	live, lines, flying, err := s.Advisor.LiveProgress(ctx, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	pending, err := s.Advisor.PendingRequests(ctx, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	progress := func(out *AdvisorRunSummary) *AdvisorRunSummary {
		if flying && out.RunID == live {
			out.Kind = string(advisordomain.RunKindDeep)
			for _, l := range lines {
				out.Progress = append(out.Progress, AdvisorProgress{At: l.At, Text: l.Text})
			}
		}
		return out
	}
	if len(pending) > 0 {
		p := pending[0]
		return progress(&AdvisorRunSummary{RunID: p.ID, Trigger: string(advisordomain.TriggerPerson), By: p.By,
			StartedAt: p.RequestedAt, Status: RunRunning, Unread: []string{}, Kind: string(p.Kind.OrRules()),
			Progress: []AdvisorProgress{}, Classes: []AdvisorClassOutcome{}}), nil
	}
	if flying && len(lines) > 0 {
		// A DEEP RUN AT ITS SLOT, which nobody asked for: its first line is
		// as early as the log says it began.
		return progress(&AdvisorRunSummary{RunID: live, Trigger: string(advisordomain.TriggerSchedule),
			StartedAt: lines[0].At, Status: RunRunning, Unread: []string{}, Progress: []AdvisorProgress{},
			Classes: []AdvisorClassOutcome{}}), nil
	}
	row, found, err := s.Advisor.LatestRun(ctx, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	if !found {
		return nil, nil
	}
	return runSummary(row)
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
			Listed: pass.Listed, Dropped: nonNilMap(pass.Dropped), Reason: pass.Reason}
		if row.Kind == advisordomain.RunKindDeep {
			for _, p := range pass.Progress {
				out.Progress = append(out.Progress, AdvisorProgress{At: p.At, Text: p.Text})
			}
			out.Budget = &AdvisorRunBudget{Tokens: int64(pass.InputTokens + pass.OutputTokens),
				TokensCeiling: int64(pass.Bounds.Tokens), Minutes: int64(math.Ceil(pass.Minutes)),
				MinutesCeiling: int64(pass.Bounds.Minutes)}
			if pass.StoppedBy != "" {
				out.StoppedBy = &pass.StoppedBy
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
	out.Notices = notice.WithFindings(notice.Read(findings, fleet, held, now), listed)
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
