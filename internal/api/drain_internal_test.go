package api

import "testing"

// A REFUSED GIT REQUEST LEAVES THE DRAIN'S WAIT AS IT FOUND IT. The gate counts
// a git request before it asks whether the node is draining ([App.admit]), so
// for the instant between the two a request it then refuses is counted, and a
// wait that looked in that instant is waiting on it. Taking it back has to
// WAKE that wait, which would otherwise sleep on a request that is never
// answered, and must NOT move the last answer: a refused request started
// nothing, so no exchange's next request follows it and no gap is owed.
func TestARefusedGitRequestLeavesTheDrainsWaitAsItFoundIt(t *testing.T) {
	t.Parallel()
	var g gitServing
	g.begin()
	g.end()
	_, answered, _ := g.state()

	g.begin()
	running, _, moved := g.state() // what a wait sees in that instant
	if running != 1 {
		t.Fatalf("a request counted before it is judged reads as %d running, want 1", running)
	}
	g.abandon()
	select {
	case <-moved:
	default:
		t.Fatal("taking back a refused request did not wake the wait that saw it counted")
	}
	running, ended, _ := g.state()
	if running != 0 {
		t.Errorf("%d running after the refused request was taken back, want 0", running)
	}
	if !ended.Equal(answered) {
		t.Errorf("taking back a refused request moved the last answer from %v to %v, "+
			"which owes a gap for an exchange that never began", answered, ended)
	}
}
