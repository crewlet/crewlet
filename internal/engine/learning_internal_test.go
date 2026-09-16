package engine

import (
	"testing"
	"time"
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
