package engine_test

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
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
// only path a quickstart takes.
//
// # Two assertions, because neither one is enough
//
// THE FIRST IS THE DECISION: `statelog_below_the_floor` is warned in exactly
// the case that pays the window, immediately before the join asks, so its
// absence is the fact this test is about rather than a proxy for it.
//
// THE SECOND IS THE WALL CLOCK, and it is what keeps the first honest: a flag
// goes on reading correctly while the pause MOVES somewhere else in boot, and
// a five-second stall is worth catching wherever it came from.
//
// # The bound is a multiple of the window, and the tight one was wrong
//
// This asserted the wall clock ALONE, at exactly [statelog.OfferWindow],
// calling that "generously under" — and it was not. Measured on an idle
// machine, a fresh boot of this package's engine takes about 4.4s of the 5s
// it was allowed: twelve per cent of headroom, over a signal whose two states
// are four seconds apart. Under `make test`, which runs this package beside a
// dozen others under the detector, the same boot took 5.21s and the suite went
// red over a join that was working perfectly.
//
// The tempting repair is a bigger number, which the comment here used to warn
// against in the same breath as setting a small one: raise it past
// OfferWindow and the test no longer catches the pause it exists for. What
// makes a loose bound safe is that it is no longer carrying the argument
// alone — the log line above is exact, so this one only has to be larger than
// a slow boot and smaller than a slow boot plus five seconds.
//
// # What it does NOT assert, measured
//
// It does not exercise [stateLog.replayable]'s arithmetic. A fresh node's
// stream and checkpoint are BOTH at zero here, so neither side of that
// comparison moves: flipping `first > at.Seq+1` to `first > at.Seq` — the
// off-by-one the paragraph above is about — leaves this test green, and left
// the wall-clock version green too. What this fixture does exercise is the
// DECISION the boot reaches, which is why the assertion is on it: forcing
// `behind` non-empty fails the check below and did not fail the wall clock.
// The arithmetic itself is covered against a stream that has actually been
// trimmed, in [TestANodeBelowTheFloorAdoptsWhileRunning] and
// statelog's own TestANodeBelowTheFloorAdoptsAVerifiedArtefact.
//
// # And it is still NOT parallel
//
// A wall clock is only a measurement of this boot while nothing else in this
// process is booting, and the capture below swaps the process-wide logger.
// The sequential phase runs with every parallel test paused, which is what
// makes both of those true.
func TestAFreshNodeDoesNotSpendTheOfferWindowAtBoot(t *testing.T) {
	logs := &logBuffer{}
	logging.Configure(slog.LevelWarn, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	started := time.Now()
	e := newEngine(t, engine.Options{})
	took := time.Since(started)

	if e.Tracker() == nil || e.TrackerWriter() == nil {
		t.Fatal("a default company got no tracker")
	}
	if below := logs.records(t, "statelog_below_the_floor"); len(below) > 0 {
		t.Errorf("a fresh node read itself as below the log's floor (%v), so "+
			"it asked the fleet for a snapshot of history nobody has written "+
			"and paid %s for the answer", below[0]["domains"], statelog.OfferWindow)
	}
	if limit := 3 * statelog.OfferWindow; took >= limit {
		t.Errorf("a fresh node took %s to boot, which is past %s — nothing "+
			"here asked the fleet for a snapshot, so a stall this long is "+
			"somewhere else in the boot", took, limit)
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
