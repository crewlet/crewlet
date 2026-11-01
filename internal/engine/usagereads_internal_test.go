package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/usage"
)

// A NODE WHOSE USAGE COPY IS WRONG ANSWERS NO SPEND SCREEN FROM IT, on a real
// node through the reads the engine hands the API (ADR-0072): its spend and
// activity reads go to a data node whose usage copy serves, and a node with
// no such peer says it served nothing — naming its own copy — rather than
// drawing a screen from rows its applier stopped writing. Read straight off
// the node's own estate, they answered from that copy whatever its verdict,
// while the only trace of it was the line announcing it out of service.
func TestAWrongUsageCopyAnswersNoSpendRead(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	day := time.Now().UTC().Format(time.DateOnly)
	q := usage.SpendQuery{From: day, To: day}
	if _, err := e.UsageReads().Spend(t.Context(), q); err != nil {
		t.Fatalf("the premise: a sound node's spend read failed: %v", err)
	}

	name := usage.Domain{}.Name()
	wrongCopyOf(t, e, name)
	_, err := e.UsageReads().Spend(t.Context(), q)
	var unserved *estate.ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "'s copy of "+name+" is out of service") {
		t.Fatalf("a spend read on a single node whose usage copy is wrong = %v — want "+
			"an unserved estate naming the usage copy", err)
	}
}
