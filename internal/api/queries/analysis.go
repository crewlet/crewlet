package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/analyst"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// AnalystService is the advisor's analyst over this node's sources, nil
// where the fleet keeps no record of it or this node runs no models.
func (s Sources) AnalystService() *analyst.Service {
	if s.Analysis == nil || s.Analyst == nil {
		return nil
	}
	return &analyst.Service{Records: s.Analysis, Models: s.Analyst, Read: s.analysisData,
		Company: s.runningCompany, Now: s.clock, Zone: s.zone}
}

// runningCompany is the document this node runs, nil before its first.
func (s Sources) runningCompany() *config.Company {
	if s.Company == nil {
		return nil
	}
	c, _ := s.Company()
	return c
}

// analysisData is what an analyst run reads at now: the reads `advisor`
// answers from, over the redacted document, and the rule findings they raise.
func (s Sources) analysisData(ctx context.Context, now time.Time) (analyst.Data, error) {
	company, revision, err := s.Config.FleetCompany(ctx)
	if err != nil {
		return analyst.Data{}, err
	}
	in := advisor.Inputs{Company: company.Redact(), Revision: revision}
	if states, known := s.reconcileStates(ctx); known {
		in.Integrations = slices.SortedFunc(maps.Values(states), func(a, b integration.State) int {
			return cmp.Compare(a.Kind, b.Kind)
		})
	}
	ev := s.advisorEvidence(ctx, company, now)
	answer := advisor.ReadWithEvidence(in, ev)
	return analyst.Data{Company: in.Company, Evidence: ev, Rules: answer.Findings, Unread: answer.Unread}, nil
}

// AnalysisAnswer is `advisor_analysis`: whether the analyst is on and how it
// runs, its last run, and that run's findings people have not set aside.
type AnalysisAnswer struct {
	On      bool                  `json:"on"`
	Model   string                `json:"model,omitempty"`
	Cadence config.AnalystCadence `json:"cadence"`
	Hour    int                   `json:"hour"`
	Ceiling int                   `json:"ceiling"`
	// Suggest is the entry to turn it on with, Ask Crewlet's, where it is off.
	Suggest string `json:"suggest,omitempty"`
	// Last is the last run, its findings in Findings; Stale says it is past
	// two of its cadence's periods.
	Last     *advisor.Analysis `json:"last,omitempty"`
	Stale    bool              `json:"stale"`
	Findings []AnalystFinding  `json:"findings"`
	// SetAside counts the last run's findings people answered wrong or already known.
	SetAside int `json:"set_aside"`
}

// AnalystFinding is one listed finding, its fingerprint the one an answer names.
type AnalystFinding struct {
	advisor.Finding
	Fingerprint string          `json:"fingerprint"`
	Answer      advisor.Verdict `json:"answer,omitempty"`
}

// advisorAnalysis answers `advisor_analysis`.
func (s Sources) advisorAnalysis(ctx context.Context, _ Params) (any, error) {
	svc := s.AnalystService()
	now := s.clock()
	company, _, err := s.Config.FleetCompany(ctx)
	switch {
	case errors.Is(err, configapi.ErrNoActiveRevision):
		return AnalysisAnswer{Findings: []AnalystFinding{}}, nil
	case err != nil:
		return nil, revisionRefusal("advisor_analysis", err)
	}
	suggest := company.Assistant.Model
	last, found, err := svc.Last(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: the analyst's records could not be read: %w", ErrUnavailable, err)
	}
	answers, err := svc.Answers(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: the analyst's records could not be read: %w", ErrUnavailable, err)
	}
	return analysisOf(company.Advisor.Analyst, suggest, last, found, answers, now), nil
}

// analysisOf is the answer for a, its last run where found and the answers
// given: the run's findings people set aside are counted, never listed.
func analysisOf(a config.AdvisorAnalyst, suggest string, last advisor.Analysis, found bool,
	answers advisor.Answers, now time.Time) AnalysisAnswer {

	out := AnalysisAnswer{On: a.On(), Cadence: a.Every(), Hour: a.StartHour(), Ceiling: a.Ceiling(),
		Findings: []AnalystFinding{}}
	if out.On {
		out.Model = a.LLM[0]
	} else {
		out.Suggest = suggest
	}
	if !found {
		return out
	}
	given := map[string]advisor.Verdict{}
	for _, x := range answers {
		given[x.Fingerprint] = x.Verdict
	}
	holding := answers.Holding(now)
	for _, f := range last.Findings {
		fp := advisor.Fingerprint(f)
		if holding[fp] {
			out.SetAside++
			continue
		}
		out.Findings = append(out.Findings, AnalystFinding{Finding: f, Fingerprint: fp, Answer: given[fp]})
	}
	last.Findings = nil
	out.Last, out.Stale = &last, advisor.Stale(last.Started, now, a.Every())
	return out
}
