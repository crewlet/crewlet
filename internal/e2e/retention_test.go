package e2e

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// TestTheTrimPublishesAFloorAndNamesWhatIsHoldingIt is the whole retention
// wiring, end to end, on a real node.
//
// # Why this case and not a unit test of the arithmetic
//
// [statelog.Trim] is already certified as a pure function over a struct of
// values, and every term's own behaviour is exercised there. What no unit test
// can establish is that anything CALLS it: the trim is a loop, and a loop that
// is never started is indistinguishable from one that is blocked — the log
// simply grows, with no term reporting anything, which is what this engine did
// before the duty existed.
//
// So the assertion is about the published record: the duty ran, it evaluated
// every registered domain, and it named the term holding each one. On a fresh
// single node that term is `backup_floor`, because a company that never backs
// up never trims — the log is the only copy of what no node has applied yet,
// and that is the configuration working as asked rather than a fault.
func TestTheTrimPublishesAFloorAndNamesWhatIsHoldingIt(t *testing.T) {
	noParallel(t)
	n := start(t)

	report, runs := n.engine.RetentionReport(t.Context())
	if !runs {
		t.Fatal("this node reports no retention at all — the trim loop is not " +
			"running, so nothing evaluates the six terms and the log only grows")
	}
	if len(report.Domains) == 0 {
		t.Fatal("the report names no domain: every registered domain has a log " +
			"whose growth somebody has to be able to see")
	}

	// THE FLOOR IS PUBLISHED, which is the half a node not holding the
	// duty depends on. Waited for rather than asserted immediately: the
	// loop ticks at boot, and the boot it ticks at is the one this case
	// just started.
	deadline := time.Now().Add(20 * time.Second)
	for {
		floors, err := n.engine.Backends().Fleet.Floors(t.Context())
		if err != nil {
			t.Fatalf("read the published floors: %v", err)
		}
		if len(floors) >= len(report.Domains) {
			for _, floor := range floors {
				if floor.By == "" {
					t.Fatalf("the floor for %s names no writer: a duty that "+
						"stopped running is indistinguishable from one that "+
						"ran", floor.Domain)
				}
				if !floor.Blocked() {
					// A FRESH COMPANY HAS NO BACKUP, so every
					// domain must be blocked. An advancing trim
					// here would mean a term answered "satisfied"
					// for something nobody has done.
					t.Fatalf("the trim for %s is advancing on a node that has "+
						"never backed up: the log is the only copy of what no "+
						"node has applied, and trimming past it is deleting "+
						"the last thing that could rebuild it", floor.Domain)
				}
				if floor.BlockedBy != string(statelog.TermBackupFloor) {
					t.Fatalf("the trim for %s is blocked by %q, want %q — the "+
						"term an operator is sent to must be the one that is "+
						"actually holding it", floor.Domain, floor.BlockedBy,
						statelog.TermBackupFloor)
				}
				if len(floor.Terms) == 0 {
					t.Fatalf("the floor for %s carries no terms, so a node not "+
						"holding the duty cannot say why", floor.Domain)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d domain(s) published a floor within 20s",
				len(floors), len(report.Domains))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestTheReportNamesTheBackupTermsRemedy is the other half of the same
// wiring: an operator reading a blocked trim has to be told what to do, and
// the remedy travels on the report rather than being looked up beside it.
func TestTheReportNamesTheBackupTermsRemedy(t *testing.T) {
	noParallel(t)
	n := start(t)

	report, runs := n.engine.RetentionReport(t.Context())
	if !runs {
		t.Fatal("this node reports no retention at all")
	}
	for _, domain := range report.Domains {
		if domain.Prose == "" {
			t.Fatalf("the report for %s leads with nothing: the blocking term "+
				"in prose is the answer to the only question anybody runs this "+
				"for", domain.Domain)
		}
		var found bool
		for _, term := range domain.Terms {
			if term.Name != statelog.TermBackupFloor {
				continue
			}
			found = true
			if term.Remedy == "" {
				t.Fatalf("the backup term for %s carries no remedy — a term "+
					"that names a fault and not what to do about it sends "+
					"somebody to read code", domain.Domain)
			}
		}
		if !found {
			t.Fatalf("the report for %s omits the backup term", domain.Domain)
		}
	}
	if len(report.Alarms) == 0 {
		t.Fatal("a node that has never backed up raises no alarm — `backup_age` " +
			"is the one condition that turns \"this company never backs up and " +
			"therefore never trims\" into something that reaches a person")
	}
}
