package queries

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `GET /delegated-decisions` (ADR-0125): the rules a company delegates
// decisions by, the limits the engine holds every delegate to, what delegates
// decided on one company day (the digest, the last whole day by default, or
// the day still running so far, marked incomplete), and the spot-checks and
// flagged denials of the week before now, read off the access log's verdicts.
// Nothing here is written: a digest is read off the records, so it reaches
// whoever is owed it when it is read, and cannot be sent twice or lost.
//
// THE PEOPLE DELEGATES ANSWER TO read it: an administrator, or a person a rule
// names in escalate_to ([authz.ActionDelegatedRead]).

// DelegatedReads is the access log's verdicts, as this read takes them.
type DelegatedReads interface {
	Verdicts(ctx context.Context, q accessdomain.VerdictQuery, fresh statelog.Freshness) (accessdomain.VerdictPage, error)
	OpenVerdict(row accessdomain.VerdictRow) (subject, reason string, findings []string, err error)
}

// delegatedLookback is how far back the spot-checks and flagged denials reach:
// a week, the span a person who reads it once a week still sees whole.
const delegatedLookback = 7 * 24 * time.Hour

// delegatedPage bounds each list: a day of a busy delegate's decisions, past
// which the answer says it holds more.
const delegatedPage = accessdomain.MaxPageSize

// delegatedToday is the `day=` that names the company day still running.
const delegatedToday = "today"

// delegatedDecisions answers the read, `day=` naming the company day the
// digest covers: the last whole one by default, which Home's digest reads, or
// `today` (or today's label) for the running day so far, answered with
// `complete: false`. A day that has not begun is refused.
func (s Sources) delegatedDecisions(ctx context.Context, p Params) (any, error) {
	asking, err := caller(ctx, "delegated_decisions")
	if err != nil {
		return nil, err
	}
	company := s.currentDelegation()
	d := authz.Decide(ctx, asking, authz.ActionDelegatedRead,
		authz.Object{Kind: authz.KindCompany, Named: company.EscalationPeople()}, s.Chart, s.clock())
	switch {
	case d.Unknown():
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, d.Err)
	case !d.Allowed:
		return nil, NewRefusal("delegated_decisions", d)
	}
	now := s.clock()
	loc := s.zone()
	today := period.At(period.Day, now, loc)
	day := today.Shift(-1)
	switch label := p.String("day"); label {
	case "":
	case delegatedToday:
		day = today
	default:
		if day, err = period.Parse(period.Day, label, loc); err != nil {
			return nil, fmt.Errorf("%w: the day parameter is a company day such as 2026-11-09, or %s, "+
				"and %q isn't", ErrBadParams, delegatedToday, label)
		}
		if day.Start.After(today.Start) {
			return nil, fmt.Errorf("%w: %s has not begun; the latest company day is %s", ErrBadParams,
				day.Label, today.Label)
		}
	}
	final, sampled := true, true
	decided, decidedMore, err := s.delegatedVerdicts(ctx, accessdomain.VerdictQuery{
		Final: &final, Since: day.Start.UTC(), Until: day.End.UTC()})
	if err != nil {
		return nil, err
	}
	samples, samplesMore, err := s.delegatedVerdicts(ctx, accessdomain.VerdictQuery{
		Final: &final, Sampled: &sampled, Since: now.Add(-delegatedLookback).UTC()})
	if err != nil {
		return nil, err
	}
	denied, deniedMore, err := s.delegatedVerdicts(ctx, accessdomain.VerdictQuery{
		Final: &final, Verdict: delegated.Deny, ByKind: accessdomain.VerdictByAgent,
		Since: now.Add(-delegatedLookback).UTC()})
	if err != nil {
		return nil, err
	}
	flagged := make([]map[string]any, 0, len(denied))
	for _, row := range denied {
		if row["confidence"] == string(delegated.High) {
			flagged = append(flagged, row)
		}
	}
	rules := make([]map[string]any, 0, len(company.Rules))
	for i, r := range company.Rules {
		checks := make([]map[string]any, 0, len(r.RequiredChecks))
		for _, rc := range r.RequiredChecks {
			checks = append(checks, map[string]any{"repo": rc.Repo, "branch": rc.Branch,
				"checks": nonNilStrings(rc.Checks)})
		}
		rules = append(rules, map[string]any{"rule": i + 1, "kinds": r.Kinds, "delegates": nonNilStrings(r.Delegates),
			"escalate_to": nonNilStrings(r.EscalateTo), "sample_one_in": r.SampleRate(),
			"always_escalate_paths": nonNilStrings(r.AlwaysEscalatePaths), "required_checks": checks})
	}
	return map[string]any{
		"enabled": company.Enabled, "rules": rules,
		"limits": map[string]any{
			"deadline_minutes":       int(delegated.Deadline / time.Minute),
			"decisions_per_hour":     delegated.DecisionsPerHour,
			"server_allows_per_hour": builtin.ServerAllowsPerHour,
		},
		"day": day.Label, "since": day.Start.UTC(), "until": day.End.UTC(), "complete": !now.Before(day.End),
		"decided": decided, "decided_more": decidedMore,
		"spot_checks": samples, "spot_checks_more": samplesMore,
		"flagged": flagged, "flagged_more": deniedMore,
	}, nil
}

// delegatedVerdicts is one page of verdicts as the read shows them, opened,
// and whether the log holds more.
func (s Sources) delegatedVerdicts(ctx context.Context, q accessdomain.VerdictQuery) ([]map[string]any, bool, error) {
	q.Limit = delegatedPage
	page, err := s.Delegated.Verdicts(ctx, q, statelog.Freshness{Level: statelog.ReadConsistentPrefix})
	if err != nil {
		return nil, false, fmt.Errorf("%w: the access log could not be read: %w", ErrUnavailable, err)
	}
	out := make([]map[string]any, 0, len(page.Rows))
	for _, v := range page.Rows {
		row := map[string]any{"request": v.Request, "kind": v.Kind, "rule": v.Rule, "seat": v.Handle,
			"by": v.By, "by_kind": v.ByKind, "through": v.Through, "asked": v.Asked,
			"confidence": string(v.Confidence), "verdict": v.Verdict, "overruled": nonNilStrings(v.Overruled),
			"sampled": v.Sampled, "host": v.Host, "item": v.Item, "at": v.At.UTC()}
		if subject, reason, findings, err := s.Delegated.OpenVerdict(v); err == nil {
			row["subject"], row["reason"], row["findings"] = subject, reason, nonNilStrings(findings)
		} else {
			row["subject_unopened"] = "what was decided did not open under this node's keyring"
		}
		out = append(out, row)
	}
	return out, page.Next != "", nil
}

// currentDelegation is the running document's delegated decisions, the zero
// value where none is running.
func (s Sources) currentDelegation() config.DelegatedDecisions {
	if s.Company == nil {
		return config.DelegatedDecisions{}
	}
	company, _ := s.Company()
	if company == nil {
		return config.DelegatedDecisions{}
	}
	return company.DelegatedDecisions
}
