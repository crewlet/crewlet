package engine

import "testing"

// THE PROBE NAMES THE KEPT SESSION IT READ, CONTINUED OR NOT, so a run that
// starts a session of its own is told the session it could read: a relaunch's
// probe names the turn's own where a capture of it is kept — continued, and
// not, where no member of the seat's chain restores its archive — and else the
// base the turn's first launch froze, continued and not, where the turn's row
// holds it; a first launch names the head its dispatch read, continued and
// not; and a first launch whose conversation left nothing names none, as a
// relaunch names none whose turn's begin did not hold its base — which may be
// gone, so its run is never sent to read it.
//
// Mutation: name the session only where the launch continues it, and the cases
// not continued go red; name only the turn's own on a relaunch, and the frozen
// base's go red; name a frozen base the turn's row does not hold, and the last
// case goes red.
func TestTheProbeNamesTheKeptCaptureItRead(t *testing.T) {
	t.Parallel()
	k := newKeptLaunch(t, 40000, 0, 0)
	if got := k.l.ProbeSession(k.ctx); !got.Continues() || got.KeptTurn != "t1" {
		t.Errorf("a relaunch continuing the turn's kept session is answered %+v, want it named, t1", got)
	}
	k.jobs.Carries = false
	if got := k.l.ProbeSession(k.ctx); got.Continues() || got.Why == "" || got.KeptTurn != "t1" {
		t.Errorf("a relaunch no member continues the kept session of is answered %+v, want it named, t1, "+
			"and why it is not continued", got)
	}

	c := newConversingLaunch(t, 0, 0)
	c.log.endedTurn("b1", "s-b1", "B1's transcript", 42000)
	read := c.read("t2")
	first := c.launcher("t2", read)
	basis := first.ProbeSession(c.ctx)
	if !basis.ContinuesBase() || basis.KeptTurn != "b1" {
		t.Fatalf("a first launch continuing its conversation's head is answered %+v, want it named, b1", basis)
	}
	if _, err := c.launch(t, first, basis, read.endingsBlock()); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	relaunch := c.launcher("t2", nil)
	if got := relaunch.ProbeSession(c.ctx); !got.ContinuesBase() || got.KeptTurn != "b1" {
		t.Errorf("a relaunch of a turn that kept nothing, continuing the base it froze, is answered %+v, "+
			"want it named, b1", got)
	}
	c.jobs.Carries = false
	if got := first.ProbeSession(c.ctx); got.ContinuesBase() || got.KeptTurn != "b1" {
		t.Errorf("a first launch no member continues the head of is answered %+v, want it named, b1", got)
	}
	if got := relaunch.ProbeSession(c.ctx); got.ContinuesBase() || got.Why == "" || got.KeptTurn != "b1" {
		t.Errorf("a relaunch no member continues the frozen base of is answered %+v, want it named, b1, "+
			"and why it is not continued", got)
	}

	empty := newConversingLaunch(t, 0, 0)
	if got := empty.launcher("t2", empty.read("t2")).ProbeSession(empty.ctx); got.KeptTurn != "" {
		t.Errorf("a conversation's first turn is answered %+v, want no session named", got)
	}

	unheld := newConversingLaunch(t, 0, 0)
	unheld.log.endedTurn("b1", "s-b1", "B1's transcript", 42000)
	unheld.log.mu.Lock()
	unheld.log.holdNothing = true
	unheld.log.mu.Unlock()
	read = unheld.read("t2")
	first = unheld.launcher("t2", read)
	if _, err := unheld.launch(t, first, first.ProbeSession(unheld.ctx), read.endingsBlock()); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	if got := unheld.launcher("t2", nil).ProbeSession(unheld.ctx); got.KeptTurn != "" || got.ContinuesBase() {
		t.Errorf("a relaunch whose turn's begin did not hold its base is answered %+v, want no session "+
			"named", got)
	}
}
