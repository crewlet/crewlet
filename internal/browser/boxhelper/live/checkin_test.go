package live_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
)

// A CHECKIN SAYS WHETHER THIS JOB'S SHIM HOLDS THE BROWSER: the relay of
// job A is told, after its hello, that A's shim does not hold the browser;
// that it does once A's shim connects, and does not once it closes; and
// nothing of another job's shim, B's, connecting and closing, so the next
// check-in it is told is A's shim back, connected, and the fourth it was
// told; each since the box's clock read it change. Newest alone: the
// check-in is the live view's, which no pin moves.
//
// Mutation: read any model connection as this job's.
func TestACheckinSaysWhetherThisJobsShimHoldsTheBrowser(t *testing.T) {
	box := readyBox(t, boxhelpertest.Newest(t))
	relay := box.DialRelay(t)
	if got := relay.Hello(helperOf(t, box), jobA); got.T != "hello" {
		t.Fatalf("the relay's hello was answered %s", got.Header)
	}
	next := func(what string) browser.Checkin {
		t.Helper()
		var c browser.Checkin
		relay.Next("checkin", what, func(boxhelpertest.Record) bool { return true }).Decode(t, &c)
		if c.Job != jobA || c.V != 1 {
			t.Fatalf("%s is %+v, want job A's", what, c)
		}
		return c
	}
	if c := next("the check-in after the hello"); c.Connected {
		t.Fatalf("with no shim, the check-in says %+v", c)
	}
	a := box.MCP(t, jobA)
	a.Initialize("2025-11-25", map[string]any{})
	held := next("the check-in once A's shim connected")
	if !held.Connected {
		t.Fatalf("with A's shim connected, the check-in says %+v", held)
	}
	a.Close()
	a.AwaitClosed()
	left := next("the check-in once A's shim closed")
	if left.Connected || left.SinceMS < held.SinceMS {
		t.Fatalf("with A's shim closed, the check-in says %+v, after %+v", left, held)
	}
	b := box.MCP(t, jobB)
	b.Initialize("2025-11-25", map[string]any{})
	b.Close()
	b.AwaitClosed()
	again := box.MCP(t, jobA)
	again.Initialize("2025-11-25", map[string]any{})
	if c := next("the check-in once A's shim connected again"); !c.Connected || c.SinceMS < left.SinceMS {
		t.Errorf("B's shim came and went, then A's came back, and the check-in A's relay was told next says %+v", c)
	}
	told := 0
	for _, r := range relay.Records() {
		if r.T == "checkin" {
			told++
		}
	}
	if told != 4 {
		t.Errorf("A's relay was told %d check-ins, want 4: B's shim, another job's, told it something", told)
	}
}
