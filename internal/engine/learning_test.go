package engine_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
)

// reflecting reports whether e has built the reflect dispatcher, which the
// seats it holds hand their turns' reflection wakes to.
func reflecting(_ *testing.T, e *engine.Engine) bool { return engine.Reflects(e) }

// THE FIRST COMPANY ON AN UNCONFIGURED NODE REFLECTS: boot builds the
// dispatcher only for a company it already has, so the first apply on a fresh
// node (from the dashboard or the first PUT /config) must build it, not only
// swap the workers behind one.
func TestTheFirstCompanyOnAnUnconfiguredNodeReflects(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	if reflecting(t, e) {
		t.Fatal("an unconfigured node built a dispatcher before it has an " +
			"org to resolve seats against")
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflecting(t, e) {
		t.Fatal("the first company applied to a fresh node reflects on no turn " +
			"until the process restarts")
	}
}

// learningDutyHeld reports whether e holds the fleet duty every background
// learning pass claims before it runs. A loop claims it for a pass that is on
// and never for one that is off, so a held duty is a pass this node ran.
func learningDutyHeld(t *testing.T, e *engine.Engine) bool {
	t.Helper()
	held, err := e.Backends().Coord.Get(t.Context(), coord.WorkerResource(engine.LearningDutyName))
	if err != nil {
		t.Fatalf("read the learning duty: %v", err)
	}
	return held != nil && held.Owner == e.Node().Owner()
}

// fastClustering turns on the one background pass whose cadence a company
// sets in seconds, so a loop running it ticks well inside a test. Every other
// pass ticks hourly or daily.
const fastClustering = `
learning:
  skill_synthesis:
    scheduler_enabled: true
    scheduler_interval_seconds: 1
`

// THE FIRST COMPANY ON AN UNCONFIGURED NODE RUNS ITS BACKGROUND PASSES, though
// the node booted with no company (as every company created from the
// dashboard starts), without waiting for a restart.
func TestTheFirstCompanyOnAnUnconfiguredNodeRunsItsBackgroundPasses(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc+fastClustering), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	waitFor(t, "the first company's clustering pass to run", func() bool {
		return learningDutyHeld(t, e)
	})
}

// AND A PROVIDER ADDED LATER TURNS ON THE PASSES THAT CALL A MODEL: a company
// with no providers.llm runs, its compaction, clustering and promotion waiting
// for a model, and the apply that adds one starts them without a restart.
func TestAProviderAddedLaterTurnsOnTheModelPasses(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, noModelsDoc+fastClustering)})
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc+fastClustering), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	waitFor(t, "the clustering pass the provider made possible to run", func() bool {
		return learningDutyHeld(t, e)
	})
}
