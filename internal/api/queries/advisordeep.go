package queries

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor/modelpass"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// WHAT THE ADVISOR'S DEEP RUN MAY READ (ADR-0166): the engine's own read
// questions, the answers the dashboard reads, asked in process as the
// Advisor, from an allowlist. The registry checks each question's posture as
// it does for any caller, answers no credential or secret in clear, and has
// nothing that writes. Identity, sessions, credentials and conversations are
// not on the list.

// AskQuestion answers one registered question for the caller ctx carries.
type AskQuestion func(ctx context.Context, what string, p Params) (any, error)

// advisorReads is every question the deep run may ask, with its parameters
// and what it answers, in the order it lists them.
var advisorReads = []modelpass.Question{
	{Name: "advisor_snapshot", Params: "{range: 7d|14d|30d}", About: "the company's stats and chart series over the range, each stat beside the range before"},
	{Name: "advisor_findings", Params: "{state: open|closed, severity, area, class, limit, cursor}", About: "the Advisor's findings as kept, with their facts and fixes"},
	{Name: "advisor_finding", Params: "{id}", About: "one finding with its history"},
	{Name: "advisor_run", Params: "{}", About: "the latest run: what each class concluded, and what it could not read"},
	{Name: "advisor_reports", Params: "{limit, cursor}", About: "the weekly reports' summaries"},
	{Name: "advisor_report", Params: "{week}", About: "one week's report with its metrics"},
	{Name: "seat_activity", Params: "{seat, days, previous}", About: "every seat's turns over the days: started, by outcome, tokens"},
	{Name: "tokens", Params: "{days, since, until, previous, seat}", About: "token spend by seat, model and day"},
	{Name: "schedules", Params: "{}", About: "every schedule with its cadence and its last fires"},
	{Name: "schedule_runs", Params: "{scope_type, scope_id, name, limit}", About: "one schedule's runs and what each did"},
	{Name: "work_workload", Params: "{unit}", About: "who holds how much open work, by status"},
	{Name: "work_flow", Params: "{bucket, points, project}", About: "the company's work over time: created, started, done"},
	{Name: "work_items", Params: "{status_group: " + worklet.StatusGroupWords() + ", assignee: a seat's handle, type, limit, cursor}",
		About: "work items narrowed by status, assignee and type"},
	{Name: "work_projects", Params: "{limit}", About: "the projects with their task counts"},
}

// advisorPrincipal is the Advisor as it asks the engine's questions: the
// engine itself, holding an administrator's reads, never a person's grant and
// never a credential presented over a socket.
func advisorPrincipal() iam.Principal {
	return iam.Principal{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("crewlet/advisor")), Login: "engine:advisor",
		Kind: iam.KindEngine, Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin}}
}

// deepReads is the allowlist answered through ask as the Advisor.
type deepReads struct{ ask AskQuestion }

var _ modelpass.Reads = deepReads{}

func (deepReads) Questions() []modelpass.Question { return advisorReads }

func (r deepReads) Answer(ctx context.Context, name string, params map[string]any) (any, error) {
	if r.ask == nil {
		return nil, errors.New("queries: this node asks no questions for the Advisor")
	}
	return r.ask(iam.WithPrincipal(ctx, advisorPrincipal()), name, FromMap(params))
}

// advisorDeepData is what one deep run reads at now: what the daily pass
// reads, the company's snapshot over the week, and the allowlisted reads.
func (s Sources) advisorDeepData(ask AskQuestion) func(ctx context.Context, now time.Time) (modelpass.DeepData, error) {
	return func(ctx context.Context, now time.Time) (modelpass.DeepData, error) {
		d, err := s.advisorModelData(ctx, now)
		if err != nil {
			return modelpass.DeepData{}, err
		}
		return modelpass.DeepData{Data: d, Snapshot: s.AdvisorSnapshot(ctx, now, 7), Reads: deepReads{ask: ask}}, nil
	}
}

// advisorDeepBounds is a deep run's bounds as the company sets them.
func advisorDeepBounds(c *config.Company) modelpass.DeepBounds {
	b := modelpass.DefaultDeepBounds
	if c == nil {
		return b
	}
	if c.Advisor.Deep.Ceiling > 0 {
		b.Tokens = c.Advisor.Deep.Ceiling
	}
	if c.Advisor.Deep.Minutes > 0 {
		b.Minutes = c.Advisor.Deep.Minutes
	}
	return b
}
