package queries

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
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
}

// advisorRun answers `advisor_run`: the latest run, or a Run now waiting for
// the duty's next tick, which reads as running.
func (s Sources) advisorRun(ctx context.Context, _ Params) (any, error) {
	run, err := s.latestRun(ctx)
	if err != nil || run == nil {
		return nil, err
	}
	return *run, nil
}

// latestRun is the run a person reads as the latest, nil where none ran.
func (s Sources) latestRun(ctx context.Context) (*AdvisorRunSummary, error) {
	pending, err := s.Advisor.PendingRequests(ctx, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	if len(pending) > 0 {
		p := pending[0]
		return &AdvisorRunSummary{RunID: p.ID, Trigger: string(advisordomain.TriggerPerson), By: p.By,
			StartedAt: p.RequestedAt, Status: RunRunning, Unread: []string{}}, nil
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
		EndedAt: instant(row.EndedAt), Status: string(row.Status), Unread: []string{}}
	var scopes []duty.ScopeRecord
	if len(row.Classes) > 0 {
		if err := json.Unmarshal(row.Classes, &scopes); err != nil {
			return nil, fmt.Errorf("queries: run %s's classes do not decode: %w", row.ID, err)
		}
	}
	for _, s := range scopes {
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
	if len(row.Model) > 0 && string(row.Model) != "null" {
		var pass advisor.Pass
		if err := json.Unmarshal(row.Model, &pass); err != nil {
			return nil, fmt.Errorf("queries: run %s's model pass does not decode: %w", row.ID, err)
		}
		out.Model = &AdvisorModelSummary{Status: pass.Status, Tokens: pass.InputTokens + pass.OutputTokens,
			Listed: pass.Listed, Dropped: nonNilMap(pass.Dropped), Reason: pass.Reason}
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
