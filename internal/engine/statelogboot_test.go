package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A FRESH NODE DOES NOT ASK THE FLEET FOR A SNAPSHOT.
//
// # Why this has to be asserted rather than reasoned about
//
// The join is off a comparison between two numbers whose zero cases do not
// line up: a node that has applied nothing has a checkpoint of 0, and a stream
// nobody has written to reports its first sequence as ONE PAST its last —
// which is 1, not 0. Read carelessly, "the log starts at 1 and I am at 0"
// looks exactly like a node that has missed a record.
//
// It is not a silent bug when it goes wrong. Every boot spends
// [statelog.OfferWindow] asking a fleet that has nothing to donate, and the
// first thing an operator sees of this engine is a five-second pause on the
// only path a quickstart takes. So the assertion is the WALL CLOCK, which is
// the symptom, rather than an internal flag that would go on being true while
// the pause moved somewhere else.
func TestAFreshNodeDoesNotSpendTheOfferWindowAtBoot(t *testing.T) {
	t.Parallel()
	started := time.Now()
	e := newEngine(t, engine.Options{})
	took := time.Since(started)

	if e.Tracker() == nil || e.TrackerWriter() == nil {
		t.Fatal("a default company got no tracker")
	}
	// GENEROUSLY UNDER THE WINDOW rather than tight: what is being caught
	// is a five-second pause, and a bound at a second is one a loaded CI
	// machine fails for reasons that have nothing to do with the join.
	if limit := statelog.OfferWindow; took >= limit {
		t.Errorf("a fresh node took %s to boot and the offer window is %s — a "+
			"node with the whole log ahead of it asked the fleet for a "+
			"snapshot of history nobody has written", took, limit)
	}
}

// EVERY REGISTERED DOMAIN REPORTS A ROW, and a domain that does not gate seat
// admission still reports one.
//
// The two are different questions. Seat admission asks whether a seat's tools
// would answer wrongly, and the vector domain deliberately says "that is not
// mine to say" — a company whose embeddings are behind has a search that is
// less good, not a tracker that lies. The fleet view asks how far along this
// node's copies are, which is a fact about every one of them: a domain missing
// from that count is one an operator cannot watch fall behind.
func TestEveryDomainReportsItsOwnReplicationRow(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	rows := e.NativeStatus(t.Context())
	byName := map[string]engine.ReplicationStatus{}
	for _, row := range rows {
		if _, twice := byName[row.Name]; twice {
			t.Errorf("two rows named %q — the count would double one loop", row.Name)
		}
		byName[row.Name] = row
	}
	// THE TRACKER'S, whose health DOES gate admission.
	if _, held := byName[tracker.Domain{}.Name()]; !held {
		t.Errorf("no row for the tracker's own log; rows = %+v", rows)
	}
	// AND THE VECTOR DOMAIN'S, whose health does not.
	if _, held := byName["vectors"]; !held {
		t.Errorf("no row for the vector domain, so an operator cannot see the "+
			"company's embeddings fall behind; rows = %+v", rows)
	}
	// AND THE WIKI'S, which is still a projector rather than a domain —
	// the count is over REPLICATION LOOPS, not over one mechanism.
	if _, held := byName["pages"]; !held {
		t.Errorf("no row for the wiki's projection; rows = %+v", rows)
	}
	for _, row := range rows {
		switch {
		case row.Kind != "projection" && row.Kind != "domain":
			t.Errorf("%s reports kind %q, which is neither", row.Name, row.Kind)
		case !row.Ready && strings.TrimSpace(row.Detail) == "":
			t.Errorf("%s is not ready and says nothing about why — an operator "+
				"reading the fleet view has no next step", row.Name)
		case row.Ready && row.Detail != "":
			t.Errorf("%s is ready and still carries a detail (%q), which reads "+
				"as a warning on a healthy loop", row.Name, row.Detail)
		}
	}
}
