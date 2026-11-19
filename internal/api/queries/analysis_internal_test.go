package queries

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
)

// THE ANALYST'S ANSWER LISTS WHAT PEOPLE HAVE NOT SET ASIDE: a finding answered wrong, or known
// within its 30 days, is counted, never listed; one answered useful is listed with its answer; an
// old run is stale; an analyst that is off says which entry would turn it on.
func TestTheAnalystsAnswerListsWhatPeopleHaveNotSetAside(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	finding := func(id string) advisor.Finding {
		return advisor.Finding{Class: advisor.ClassModel, Source: advisor.SourceModel,
			Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "qa"},
			Records: []advisor.Record{{Kind: advisor.RecordTurn, ID: id}}, Fix: advisor.Fix{Kind: advisor.FixNone}}
	}
	wrong, known, useful, fresh := finding("a"), finding("b"), finding("c"), finding("d")
	answers := advisor.Answers{}.
		With(advisor.AnswerState{Fingerprint: advisor.Fingerprint(wrong), Verdict: advisor.VerdictWrong, At: now}).
		With(advisor.AnswerState{Fingerprint: advisor.Fingerprint(known), Verdict: advisor.VerdictKnown, At: now}).
		With(advisor.AnswerState{Fingerprint: advisor.Fingerprint(useful), Verdict: advisor.VerdictUseful, At: now})
	on := config.AdvisorAnalyst{LLM: config.ProviderKeys{"assistant"}}
	last := advisor.Analysis{Status: advisor.StatusDone, Started: now.Add(-time.Hour),
		Findings: []advisor.Finding{wrong, known, useful, fresh}}
	got := analysisOf(on, "assistant", last, true, answers, now)
	if !got.On || got.Model != "assistant" || got.Suggest != "" || got.SetAside != 2 || len(got.Findings) != 2 ||
		got.Findings[0].Answer != advisor.VerdictUseful || got.Findings[1].Answer != "" || got.Stale ||
		got.Last == nil || got.Last.Findings != nil {
		t.Errorf("answer = %+v, want useful and fresh listed, two set aside, fresh", got)
	}
	if later := analysisOf(on, "", last, true, answers, now.Add(31*24*time.Hour)); later.SetAside != 1 || !later.Stale {
		t.Errorf("a month on = %+v, want only the wrong one set aside, and the run stale", later)
	}
	if off := analysisOf(config.AdvisorAnalyst{}, "assistant", advisor.Analysis{}, false, nil, now); off.On ||
		off.Suggest != "assistant" || off.Last != nil {
		t.Errorf("off = %+v, want off, suggesting Ask Crewlet's entry", off)
	}
}
