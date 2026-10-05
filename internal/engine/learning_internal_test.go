package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// EACH REVISION GETS ITS OWN PASSES, and a company with no model gets only the
// one that calls none.
//
// The loops run whatever the last apply handed them, so what is asserted here
// is the handover itself: what a revision turns on, what it leaves off, and
// that the passes needing a model are left off, quietly, until one exists.
func TestEachRevisionHandsTheLoopsItsOwnPasses(t *testing.T) {
	t.Parallel()
	e := engineOver(t)

	if got := e.learningPasses(t.Context(), nil); got.Skills != nil || got.Lifecycle != nil ||
		got.Cluster != nil || got.Promoter != nil {
		t.Errorf("a node with no company was handed passes: %+v", got)
	}

	noModels := companyFor(t, `
name: Acme
learning:
  skill_curator:
    interval_hours: 6
  skill_synthesis:
    scheduler_enabled: true
roles:
  - name: Engineer
    handle: eng
`)
	got := e.learningPasses(t.Context(), noModels)
	if got.Skills == nil {
		t.Error("the curator calls no model, and a company with none was not handed it")
	}
	if got.Lifecycle != nil || got.Cluster != nil || got.Promoter != nil {
		t.Errorf("a company with no model was handed a pass that calls one: %+v", got)
	}
	if got.CuratorInterval != 6*time.Hour {
		t.Errorf("curator interval = %v, want the revision's own 6h", got.CuratorInterval)
	}

	withModel := learningCompany(t, `
  skill_synthesis:
    scheduler_enabled: true
    scheduler_interval_seconds: 120
`)
	got = e.learningPasses(t.Context(), withModel)
	if got.Cluster == nil {
		t.Error("a company with a model and clustering on was not handed the clustering pass")
	}
	if got.ClusterInterval != 2*time.Minute {
		t.Errorf("cluster interval = %v, want the revision's own 2m", got.ClusterInterval)
	}

	off := learningCompany(t, `
  enabled: false
`)
	if got := e.learningPasses(t.Context(), off); got.Skills != nil || got.Cluster != nil {
		t.Errorf("a company that turned learning off was handed passes: %+v", got)
	}
}

// recordingProvider keeps the requests it was sent.
type recordingProvider struct{ asked []llm.Request }

func (p *recordingProvider) Model() string { return "aux-model" }

func (p *recordingProvider) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	p.asked = append(p.asked, req)
	return &llm.Completion{Model: "aux-model", Content: "a summary"}, nil
}

// A CLUSTER SUMMARY IS WORTH LITTLE THINKING. It describes rows that already
// exist, and on a thinking model the thinking is spent out of the same cap as
// the summary, so one at its entry's level comes back empty and the cluster
// is compacted into nothing.
func TestACompactionSummaryAsksForLowEffort(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Dev"}
	c := meteredCompany(config.TokenBudget{}, seat)
	provider := &recordingProvider{}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "aux", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c.Models = models
	e := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}}

	summarize := e.auxSummarizer(c)
	if summarize == nil {
		t.Fatal("a company whose seat has an auxiliary model was given no summarizer")
	}
	if _, err := summarize(t.Context(), seat.Name, "system", "user"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(provider.asked) != 1 {
		t.Fatalf("the model was asked %d times, want 1", len(provider.asked))
	}
	if got := provider.asked[0].Effort; got != llm.EffortLow {
		t.Errorf("effort = %q, want a ceiling of low", got)
	}
}
