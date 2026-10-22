package artifactdata

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A WRITE THE DELETION GATE DROPPED IS A PURGED STORE, as the decide says when
// it is the one that finds the marker: the purge landed ahead of the record,
// after the decide passed, and the framework states that as one of its
// conditions — which the datastore API answered `503` "try again" and a tool
// "try again shortly", about a store nothing will ever write again. As
// [ErrGone] it is the API's `410 data_gone`, the tools' not-found, and a write
// that delivered its answer to the datastore objectives.
//
// Mutation: hand the framework's error back unchanged and every assertion
// fails.
func TestAWriteTheDeletionGateDroppedIsAPurgedStore(t *testing.T) {
	t.Parallel()
	id := artifacts.IDFor("op")
	gated := &statelog.Unavailable{Reason: statelog.ReasonDeleted,
		Detail: "the record at 3:4 was durable and applied nowhere"}
	err := published(id, gated)
	if !errors.Is(err, ErrGone) {
		t.Fatalf("a gated deletion is told %v, want ErrGone", err)
	}
	if strings.Contains(err.Error(), "statelog") || strings.Contains(err.Error(), "3:4") {
		t.Errorf("a purged store carries the framework's words: %v", err)
	}
	if !decided(WriteResult{}, err) {
		t.Error("a purged store found by the gate is not counted as an answer")
	}
	// ANY OTHER REFUSAL OF THE FRAMEWORK IS ITS OWN.
	behind := &statelog.Unavailable{Reason: statelog.ReasonBehind}
	if got := published(id, behind); errors.Is(got, ErrGone) || !errors.Is(got, behind) {
		t.Errorf("a node behind its log is told %v, want the framework's refusal", got)
	}
}

// A SCHEMA CHANGE THAT LOST ITS RACE NAMES NOTHING, carrying the domain's
// conflict over the framework's sentinel: the framework's error names the
// subject the race was lost on, and the translation used to carry it after
// the store's id, which any caller printing the error would have shown a
// person. Its sentence is the state log's. Mutations: wrap the framework's
// error again, or the store's id, and the text check goes red.
func TestASchemaChangeThatLostItsRaceNamesNothing(t *testing.T) {
	t.Parallel()
	id := artifacts.IDFor("op")
	lost := errors.Join(statelog.ErrConflict,
		errors.New("crewlet.artifacts.data."+id.String()+" kept changing under this write"))
	err := configured(id, lost)
	if !errors.Is(err, ErrConflict) || !errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("a lost race is told %v, want both sentinels in its chain", err)
	}
	if strings.Contains(err.Error(), id.String()) || strings.Contains(err.Error(), "crewlet.") {
		t.Errorf("a lost race's error is %q, which names the store", err)
	}
	if said := Sentence(err); said != statelog.LostRace {
		t.Errorf("a lost race is told %q, want %q", said, statelog.LostRace)
	}
	// ANY OTHER ANSWER IS A STORE WRITE'S.
	gated := &statelog.Unavailable{Reason: statelog.ReasonDeleted}
	if got := configured(id, gated); !errors.Is(got, ErrGone) {
		t.Errorf("a gated deletion is told %v, want ErrGone", got)
	}
}
