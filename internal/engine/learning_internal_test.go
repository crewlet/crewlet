package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
)

// EACH REVISION GETS ITS OWN PASSES, and a company with no model gets only the
// one that calls none: the loops run whatever the last apply handed them, so
// the handover itself is what is asserted.
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

// COMPACTION RESOLVES ITS SEAT BY HANDLE, NOT BY DISPLAY NAME: the handle is
// what the org chart resolves and [learning.CompleteFunc] passes, and a lookup
// by name resolves nobody, so every compaction fails and memory stays unfolded.
//
// The case ends at the seat rather than at a model: a resolved seat with no
// auxiliary chain gets a different refusal, and telling those apart is the
// assertion.
func TestCompactionResolvesItsSeatByHandle(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    gateway:
      model: m
roles:
  - name: Senior Developer
    handle: dev
    llm: gateway
    llm_auxiliary: gateway
`)
	// The fixture must tell the two apart: were handle and name the same, a
	// lookup by either would work.
	if c.Org.Role("Senior Developer") != nil {
		t.Fatal("the fixture's display name resolves a seat, so this case cannot " +
			"tell a lookup by handle from one by name")
	}

	complete := e.auxSummarizer(c)
	if complete == nil {
		t.Fatal("no summarizer on a company whose seat has an auxiliary model")
	}
	if _, err := complete(t.Context(), "dev", "system", "user"); err != nil &&
		strings.Contains(err.Error(), "no seat with that handle") {

		t.Errorf("the seat's own handle resolved nobody: %v — every compaction on "+
			"this company fails and its memory is never folded", err)
	}

	// The display name is not an address, so the assertion above is the
	// handle working, not any string resolving.
	_, err := complete(t.Context(), "Senior Developer", "system", "user")
	if err == nil || !strings.Contains(err.Error(), "no seat with that handle") {
		t.Errorf("a display name resolved a seat: %v", err)
	}
}

// AN EPISODE IS CONDENSED ON ITS SEAT, FOUND BY HANDLE: the pass names the
// seat by the handle the episodes are keyed on ([learning.FitFunc]), and a
// lookup by display name resolves nobody, so every oversized field would be
// refused rather than condensed.
//
// Ended at the lookup, as the compaction case is: a text under its budget is
// returned untouched by a seat that resolved, so only the refusal names a
// failed lookup.
func TestEpisodeCondensingResolvesItsSeatByHandle(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    gateway:
      model: m
roles:
  - name: Senior Developer
    handle: dev
    llm: gateway
    llm_auxiliary: gateway
`)
	fit := e.episodeFit(c)
	if fit == nil {
		t.Fatal("no condenser on a company whose seat has an auxiliary model")
	}
	if _, err := fit(t.Context(), "dev", learning.FieldTask, "short", 1<<10); err != nil &&
		strings.Contains(err.Error(), "no seat with that handle") {

		t.Errorf("the seat's own handle resolved nobody: %v — every oversized "+
			"field on this company is carried whole", err)
	}
	_, err := fit(t.Context(), "Senior Developer", learning.FieldTask, "short", 1<<10)
	if err == nil || !strings.Contains(err.Error(), "no seat with that handle") {
		t.Errorf("a display name resolved a seat: %v", err)
	}
}
