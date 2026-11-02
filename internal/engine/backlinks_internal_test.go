package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/estate"
)

// A PAGE'S BACKLINKS ARE ANSWERED ONLY WHERE BOTH COPIES THEY READ SERVE, on a
// real node through the reader the engine hands the API (ADR-0072): the links
// come from the pages' and the tasks' bodies and are named from both domains'
// rows, so a node whose TRACKER copy is wrong answers none of them though its
// knowledge base serves — and a single node, with no peer to ask, says so,
// naming the copy — rather than listing links derived from rows it stopped
// serving. Read straight off the node's own index, they were listed as
// complete beside a page a peer had answered.
func TestAWrongTrackerCopyAnswersNoBacklinks(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	indexed(t, e)
	pagesReader, ok := OperatorPages(e)
	if !ok {
		t.Fatal("the premise: the node serves no Doclet")
	}
	if _, err := pagesReader.LinkedFrom(t.Context(), "p.none"); err != nil {
		t.Fatalf("the premise: a sound node's backlinks failed: %v", err)
	}

	wrongCopyOf(t, e, trackerName)
	if _, serves := e.local.For(t.Context(), pagesName); !serves {
		t.Fatal("the premise: the knowledge base's copy went out of service too")
	}
	_, err := pagesReader.LinkedFrom(t.Context(), "p.none")
	var unserved *estate.ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "'s copy of "+trackerName+" is out of service") {
		t.Fatalf("backlinks on a single node whose tracker copy is wrong = %v — want an "+
			"unserved estate naming the tracker's copy", err)
	}
}
