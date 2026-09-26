package tracker

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A STRIP WITH A VIEWER READS THEIR PINS, SO ITS CLOSURE HOLDS THEIR RECORD.
//
// The pins that order a strip are read from the viewer's person record in the
// strip's own transaction. The closure was the container alone, so a node
// holding a deferred record on somebody's pins served a strip ordered by the
// pins before it — and reported that strip complete. A strip with COUNTS runs
// task queries whose closures are only known inside the transaction, so it
// declares the domain.
func TestAStripsClosureHoldsEverythingItReads(t *testing.T) {
	t.Parallel()
	eng := Container{Kind: ContainerProject, ID: "ENG"}
	person := statelog.ScopeSet{Paths: []string{
		subjectPath(PersonSubject("ana"), ""),
	}}.Normalised()
	task := statelog.ScopeSet{Paths: []string{
		ScopeTerm{Kind: TermObject, Container: "OPS", ID: "t-1"}.Path(),
	}}.Normalised()

	if !stripScope(ViewQuery{Container: eng, Viewer: PartyOf("ana")}).Intersects(person) {
		t.Error("a strip read for ana does not cover her person record, " +
			"which is where the pins that order it are read from")
	}
	if stripScope(ViewQuery{Container: eng}).Intersects(person) {
		t.Error("a shared strip reads no pins, and its closure should not " +
			"wait on a person record")
	}
	counted := stripScope(ViewQuery{Container: eng, Viewer: PartyOf("ana"),
		Counts: true})
	if !counted.Intersects(task) || !counted.Intersects(person) {
		t.Error("a counted strip runs task queries over any container and " +
			"must declare a closure that covers them")
	}
}
