package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// THE SEMANTIC INDEX'S DUTY READS THIS NODE AS CURRENT ON THE VECTOR LOG once
// it has applied everything the log holds: the step decides nothing from a
// node behind it, so a node with nothing left to apply read as behind would
// never take one.
func TestTheIndexDutyReadsANodeThatAppliedTheLogAsCurrent(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	duty := &embedDuty{engine: e, log: s.Domain(search.Domain{}.Name())}
	standing, err := duty.standing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !standing.Current {
		t.Fatal("a node with nothing on its vector log to apply is read as behind it")
	}
}
