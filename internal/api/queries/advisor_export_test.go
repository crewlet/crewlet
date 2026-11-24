package queries

import (
	"context"
	"encoding/json"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// SetupSignature is the Advisor duty's signature of the setup, read as its
// tick reads it.
func (s Sources) SetupSignature(ctx context.Context) (string, error) {
	return advisorObserver{s: s}.Signature(ctx)
}

// ObserveAdvisor is the Advisor duty's reading of the company at now, over a
// tracker with no work in it.
func (s Sources) ObserveAdvisor(ctx context.Context, now time.Time) (duty.Observed, error) {
	s.Work = patternsWork{}
	return advisorObserver{s: s}.Observe(ctx, now)
}

// AdvisedOfAtlas is advisedOfAtlas for the external suite, which holds the
// atlas's harness: what the duty observes of the atlas, read as the engine reads it.
func (s Sources) AdvisedOfAtlas(ctx context.Context, answer advisor.Answer, now time.Time) advisor.Answer {
	return s.advisedOfAtlas(ctx, answer, now)
}

// AsRead is f stored as the duty stores it, then read back by the caller ctx
// carries at now: what a person is sent of a finding the duty kept.
func (s Sources) AsRead(ctx context.Context, f advisor.Finding, now time.Time) (AdvisorFinding, error) {
	body, err := json.Marshal(advisor.Observe(f, advisor.Span{From: now, To: now}, "r1"))
	if err != nil {
		return AdvisorFinding{}, err
	}
	principal, err := caller(ctx, "advisor_finding")
	if err != nil {
		return AdvisorFinding{}, err
	}
	row := advisordomain.FindingRow{ID: "f-1", Key: advisor.Key(f), Class: string(f.Class),
		State: advisordomain.State(advisor.StateOpen), Revision: 1, OpenedAt: now, Body: body}
	v := findingView{now: now, atlas: principal.Can(iam.GrantAtlas)}
	if v.atlas {
		v.entries = func() map[string][]advisor.AtlasEntry { return s.atlasEntriesFor(ctx, principal, now) }
	}
	return v.finding(row)
}

// WeekOf is the week [from, to) as the duty reads it for a report.
func (s Sources) WeekOf(ctx context.Context, from, to time.Time) ([]advisor.Metric, []string, error) {
	return advisorObserver{s: s}.Week(ctx, from, to)
}

// AdvisorReadParams is the parameters the deep run's read list gives name.
func AdvisorReadParams(name string) string {
	for _, q := range advisorReads {
		if q.Name == name {
			return q.Params
		}
	}
	return ""
}

// AdvisorReadNames is every question the deep run may ask, by name.
func AdvisorReadNames() []string {
	out := make([]string, len(advisorReads))
	for i, q := range advisorReads {
		out[i] = q.Name
	}
	return out
}
