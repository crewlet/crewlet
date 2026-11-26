package sandbox

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// stampedAt is the instant node-a's clock stamped on each row below.
var stampedAt = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// rowWaitCase is one reading of a wait timed on the row: the window the row
// holds (nil for none, as an older build writes it), the node judging and its
// clock, and whether the wait is over.
type rowWaitCase struct {
	name   string
	window *RowWindow
	node   string
	at     time.Duration // the judging clock, past stampedAt
	over   bool
}

// rowWaitCases are the readings every give-up timed on the row is held to,
// for a bound: node-a stamped the wait's first instant at stampedAt.
func rowWaitCases(bound time.Duration) []rowWaitCase {
	own := &RowWindow{Node: "node-a", Since: stampedAt, Of: stampedAt}
	moved := stampedAt.Add(30 * time.Minute)
	onB := &RowWindow{Node: "node-b", Since: moved, Of: stampedAt}
	absolute := bound + RowClockAllowance
	return []rowWaitCase{
		{"the stamping node, short of the bound", own, "node-a", bound - time.Second, false},
		{"the stamping node, at the bound", own, "node-a", bound, true},
		// A CLOCK AHEAD of the stamper's by half an hour, a moment in: the
		// bound read off another node's instant would give up at once.
		{"a node half an hour ahead, a second in", own, "node-b", 30*time.Minute + time.Second, false},
		{"a node behind, short of the absolute bound", own, "node-b", absolute - time.Second, false},
		{"a node behind, at the absolute bound", own, "node-b", absolute, true},
		{"the node the wait moved to, short of the bound on its clock", onB, "node-b",
			30*time.Minute + bound - time.Second, false},
		{"the node the wait moved to, at the bound on its clock", onB, "node-b", 30*time.Minute + bound, true},
		{"the node it moved from, timed on the other's window", onB, "node-a", bound, false},
		{"a row naming no node, read by its stamper", nil, "node-a", bound, false},
		{"a row naming no node, at the absolute bound", nil, "node-a", absolute, true},
		{"a window of a former run, kept by a build that carries it", &RowWindow{Node: "node-a",
			Since: stampedAt, Of: stampedAt.Add(-time.Hour)}, "node-a", bound, false},
	}
}

// EVERY GIVE-UP TIMED BY AN INSTANT ON THE ROW IS JUDGED ON ONE CLOCK OR WITH
// THE CLOCKS' ALLOWANCE (rowclock.go): a failed collection's run (a minute), a
// failed resume's (half an hour) and a revived answer's or carried-on wait's
// lost claims (the run's pause_ttl_seconds). Judged on another node's clock
// against the bound alone, a node ahead of the stamper gave up at once, and
// one behind waited up to two hours past it.
//
// Mutation: judge every window as the node's own, ignore the window, judge the
// first instant against the bound alone, or read a window of another run, and
// a reading below goes red.
func TestAGiveUpTimedOnTheRowIsJudgedOnTheStampersClock(t *testing.T) {
	t.Parallel()
	const pauseTTL = time.Hour
	for _, site := range []struct {
		name  string
		bound time.Duration
		over  func(c *Coordinator, w *RowWindow, now time.Time) bool
	}{
		{"a failed collection", ConnectGiveUp, func(c *Coordinator, w *RowWindow, now time.Time) bool {
			run := PendingRun{Launch: LaunchRecord{CollectFailures: MinConnectFailures - 1,
				CollectFailingSince: stampedAt, CollectFailingOn: w}}
			return c.collectGivenUp(run, errors.New("the box did not answer"), now).givenUp
		}},
		{"a failed resume", ResumeGiveUp, func(c *Coordinator, w *RowWindow, now time.Time) bool {
			run := PendingRun{Launch: LaunchRecord{ResumeFailures: MinResumeFailures - 1,
				ResumeFailingSince: stampedAt, ResumeFailingOn: w}}
			return c.resumeGiveUpDue(run, now)
		}},
		{"a revived answer", pauseTTL, func(c *Coordinator, w *RowWindow, _ time.Time) bool {
			run := PendingRun{PauseTTLSeconds: pauseTTL.Seconds(),
				Answer: &RecordedAnswer{LostClaims: 1, FirstLostAt: stampedAt, LostOn: w}}
			return c.revivable(run) != ""
		}},
		{"a carried-on wait", pauseTTL, func(c *Coordinator, w *RowWindow, _ time.Time) bool {
			run := PendingRun{PauseTTLSeconds: pauseTTL.Seconds(),
				WaitOver: &WaitOver{LostClaims: 1, FirstLostAt: stampedAt, LostOn: w}}
			return c.revivableAfter(run, "this run's carrying on", 1, run.WaitOver.lostWait()) != ""
		}},
	} {
		for _, reading := range rowWaitCases(site.bound) {
			now := stampedAt.Add(reading.at)
			c := &Coordinator{node: reading.node, now: func() time.Time { return now }}
			if got := site.over(c, reading.window, now); got != reading.over {
				t.Errorf("%s, %s: given up = %v, want %v", site.name, reading.name, got, reading.over)
			}
		}
	}
}

// A COLLECTION WAITS FOR A CALL IN FLIGHT UNTIL ITS BOUND ON THE CLOCK THAT SET
// IT: the bridge's node stamps the bound, and any other node waits the clocks'
// allowance past it. A bound is one instant, never recorded again, so there is
// no window to restart.
//
// Mutation: judge a bound another node stamped against itself, and the node
// half an hour ahead stops waiting for a call still inside its bound; never
// stop, and the absolute bound is not kept.
func TestACallInFlightIsWaitedForOnItsBridgesClock(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		on     string
		node   string
		at     time.Duration
		waited bool
	}{
		{"the bridge's node, short of the bound", "node-a", "node-a", -time.Second, true},
		{"the bridge's node, at the bound", "node-a", "node-a", 0, false},
		{"a node half an hour ahead, inside the bound", "node-a", "node-b", 30*time.Minute - time.Second, true},
		{"a node behind, short of the absolute bound", "node-a", "node-b", RowClockAllowance - time.Second, true},
		{"a node behind, at the absolute bound", "node-a", "node-b", RowClockAllowance, false},
		{"a bound naming no node, read by its stamper", "", "node-a", time.Minute, true},
		{"a bound naming no node, at the absolute bound", "", "node-a", RowClockAllowance, false},
	} {
		now := stampedAt.Add(c.at)
		coord := &Coordinator{node: c.node, now: func() time.Time { return now }}
		call := BridgeCall{Name: "slow_tool", Until: stampedAt, UntilOn: c.on}
		waiting, left := coord.stillWaited([]BridgeCall{call}, PendingRun{}, nil)
		if got := len(waiting) == 1 && len(left) == 0; got != c.waited {
			t.Errorf("%s: still waited for = %v, want %v (left %+v)", c.name, got, c.waited, left)
		}
		if due := coord.nextDue([]BridgeCall{call}, PendingRun{}); c.waited && due > callsDrainTick {
			t.Errorf("%s: the next look is %v away, past the drain's tick", c.name, due)
		}
	}
}

// A HELD CALL'S DEADLINE IS TIMED ON THE CLOCK THAT HELD IT, or on the judging
// node's own from when it first saw the call waiting, or the clocks' allowance
// past the instant, whichever comes first; a call naming no node is timed by
// the last two alone.
//
// Mutation: drop any of the three readings, or take the instant at its word on
// another node, and a reading below goes red.
func TestAHeldCallsDeadlineIsTimedOnOneClock(t *testing.T) {
	t.Parallel()
	const deadline = 15 * time.Minute
	held := HeldCall{HeldAt: stampedAt, HeldOn: "node-a"}
	for _, c := range []struct {
		name   string
		held   HeldCall
		node   string
		seen   time.Duration // past stampedAt, on the judging clock; zero for never seen
		at     time.Duration
		waited bool
	}{
		{"the holding node at the deadline", held, "node-a", 0, deadline, true},
		{"the holding node short of it", held, "node-a", 0, deadline - time.Second, false},
		{"a node half an hour ahead that just saw it", held, "node-b", 31 * time.Minute, 31 * time.Minute, false},
		{"that node the deadline after it saw it", held, "node-b", 31 * time.Minute, 46 * time.Minute, true},
		{"a node behind that never saw it, short of the absolute bound", held, "node-b", 0,
			deadline + RowClockAllowance - time.Second, false},
		{"a node behind that never saw it, at the absolute bound", held, "node-b", 0,
			deadline + RowClockAllowance, true},
		{"a call naming no node, read by its holder", HeldCall{HeldAt: stampedAt}, "node-a", 0, deadline, false},
		{"a call naming no node, at the absolute bound", HeldCall{HeldAt: stampedAt}, "node-a", 0,
			deadline + RowClockAllowance, true},
	} {
		var seen time.Time
		if c.seen != 0 {
			seen = stampedAt.Add(c.seen)
		}
		if got := c.held.WaitedFor(c.node, stampedAt.Add(c.at), seen, deadline); got != c.waited {
			t.Errorf("%s: waited its deadline = %v, want %v", c.name, got, c.waited)
		}
	}
}

// A READER IS TOLD A KEPT BOX'S AGE AS ITS HOST READS IT: the box is dated on
// its host's clock and continued only there, so the reader judges it on the
// clock the node holding the seat (the host, where the box is continued at all)
// said with its target, never its own. A target that said no clock leaves it
// undecided.
//
// Mutation: read the age on the reader's clock, and a reader two hours ahead
// is told a box its host still continues in is too old; drop the zero check,
// and a target with no clock reads every box as new.
func TestAReaderReadsAKeptBoxsAgeOnItsHostsClock(t *testing.T) {
	t.Parallel()
	attached := stampedAt
	run := PendingRun{TurnID: "t1", Status: StatusAwaiting, LaunchSeq: 1, SessionID: "s-1", SandboxID: "box-1",
		Placement: string(Direct), Harness: "claude-code", LaunchNode: "node-a", BoxSince: attached}
	row := executorkeep.TurnRow{Harness: "claude-code", SessionID: "s-1", LaunchSeq: 1}
	for _, c := range []struct {
		name string
		at   time.Time
		want boxCarry
	}{
		{"an hour short of the bound on the host's clock", attached.Add(KeptBoxMaxAge - time.Hour), boxContinues},
		{"at the bound on the host's clock", attached.Add(KeptBoxMaxAge), boxNot},
		{"no clock said", time.Time{}, boxUndecided},
	} {
		target := RelaunchTarget{Harness: "claude-code", LaunchRouted: true, Placement: Direct, Node: "node-a",
			At: c.at, Restorer: fakeRestorer{}}
		if got, why := inParkedBox(run, row, target); got != c.want {
			t.Errorf("%s: the box is answered %v (%q), want %v", c.name, got, why, c.want)
		}
	}
}
