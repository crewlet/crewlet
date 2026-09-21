package engine

import (
	"strings"
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

// COMPACTION RESOLVES ITS SEAT BY HANDLE, NOT BY DISPLAY NAME.
//
// The handle is what the org chart resolves and what [learning.CompleteFunc]
// passes. It was the display name, and on any company whose seats declare a
// handle it resolved nobody: every compaction failed, the memory it was meant
// to fold stayed whole, and the failure was logged on a path whose whole
// contract is best effort.
//
// The lookup is what this pins, so the case ends at the seat rather than at a
// model: a resolved seat with no auxiliary chain gets a DIFFERENT refusal, and
// telling those two apart is the whole assertion.
func TestCompactionResolvesItsSeatByHandle(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	c := companyFor(t, `
name: Acme
providers:
  llm:
    gateway:
      type: anthropic
      model: m
      api_keys: ["${K}"]
roles:
  - name: Senior Developer
    handle: dev
    llm: gateway
    llm_auxiliary: gateway
`)
	// THE FIXTURE MUST BE ABLE TO TELL THE TWO APART. With the handle and
	// the display name the same, a lookup by either works.
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

	// AND THE DISPLAY NAME IS NOT AN ADDRESS, which is what says the
	// assertion above is the handle working rather than any string
	// resolving.
	_, err := complete(t.Context(), "Senior Developer", "system", "user")
	if err == nil || !strings.Contains(err.Error(), "no seat with that handle") {
		t.Errorf("a display name resolved a seat: %v", err)
	}
}
