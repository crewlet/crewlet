package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// reflecting reports whether anything on e's broker consumes completed turns
// for the reflect dispatcher, which is the whole of the learning write side.
func reflecting(t *testing.T, e *engine.Engine) bool {
	t.Helper()
	subs, err := e.Backends().Queue.ListSubscriptions(t.Context(),
		topics.Event(types.TurnCompleted{}.EventType()))
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	for _, sub := range subs {
		if sub.Group == learning.ReflectGroup {
			return true
		}
	}
	return false
}

// THE FIRST COMPANY ON AN UNCONFIGURED NODE REFLECTS.
//
// The dispatcher is attached once per process, and boot attaches it only for
// a company it already has. Every apply after that only swaps the workers
// behind it, and with no dispatcher to swap into, the swap returned early: a
// company created on a fresh node, from the dashboard or by the first
// PUT /config, wrote no diary row, no episode and no skill for any turn it
// ran until the process restarted, and nothing anywhere said so.
func TestTheFirstCompanyOnAnUnconfiguredNodeReflects(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	if reflecting(t, e) {
		t.Fatal("an unconfigured node reads completed turns before it has an " +
			"org to resolve their seats against")
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc)); err != nil {
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
	held, err := e.Backends().Coord.Get(t.Context(), coord.WorkerResource("skill-curator"))
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

// THE FIRST COMPANY ON AN UNCONFIGURED NODE RUNS ITS BACKGROUND PASSES.
//
// The loops used to be built from the company the node booted with and never
// again, so a node that booted with none (the node every company created from
// the dashboard starts on) compacted, curated, clustered and promoted nothing
// until the process restarted.
func TestTheFirstCompanyOnAnUnconfiguredNodeRunsItsBackgroundPasses(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc+fastClustering)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	waitFor(t, "the first company's clustering pass to run", func() bool {
		return learningDutyHeld(t, e)
	})
}

// AND A PROVIDER ADDED LATER TURNS ON THE PASSES THAT CALL A MODEL.
//
// A company with no providers.llm is valid and runs, and its compaction,
// clustering and promotion wait for a model. Built once at boot, they waited
// for a restart instead: the apply that added the provider changed nothing
// about what the loops ran.
func TestAProviderAddedLaterTurnsOnTheModelPasses(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, noModelsDoc+fastClustering)})
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc+fastClustering)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	waitFor(t, "the clustering pass the provider made possible to run", func() bool {
		return learningDutyHeld(t, e)
	})
}
