package chart_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// THE CHART IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, and it is the FOURTH
// domain to run it — the third STRICT one.
//
// What the three before it could not exercise is the case this one is: a domain
// whose arbitration is NOT UNIFORM. The tracker, the vectors and the knowledge
// base each contend per object, so "the subject" and "the object" are one thing
// throughout, and every rule the framework states about the pair is trivially
// satisfied. Here one subject carries the whole STRUCTURE while each object's
// CONTENT carries its own — so a record's subject, the objects its apply
// writes and the scope it declares are three genuinely different sets, and the
// suite's determinism and deferral cases are the first to run against a domain
// where they can come apart.
//
// IT COULD NOT RUN UNTIL THE APPLIER EXISTED. The suite's four halves include
// the apply cases, and the engine's boot check refuses a register entry with a
// nil applier — so the declaration landed first and this is where it becomes
// certified rather than merely stated.
func TestTheChartDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain: chart.Domain{},
			// NO REBUILD LISTENER, which is the honest shape here and
			// not a saving: what the applier's change set feeds is a
			// derived company view, and a view rebuilt from the suite's
			// own fixtures would prove nothing about either. The set is
			// drained and dropped, exactly as it is on a node that
			// holds no view.
			Applier: chart.NewApplier("suite-node", nil),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migrations, so a fresh store already has
			// them. A domain that created its tables from test code
			// would be a schema the suite proved and the migration did
			// not.
			Encode: encodeSuiteRecord,
			Kinds:  suiteKinds(),
		}
	})
}
