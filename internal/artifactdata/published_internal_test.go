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
