package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// THE SWEEP GETS A WRITER WHILE EVERY DOMAIN IS APPLYING.
//
// A pinned connection is DECLARED, which is what makes the pool grow to hold
// it rather than the pin taking a reader's place. The consequence is that a
// shortfall is not contention that clears under load: it is a refusal that
// never clears at all. Each apply loop takes its pin at boot and keeps it for
// the life of the loop, so a count sized at the registered domains alone left
// nothing for the maintenance worker, and `tracker_notifications` failed on
// every tick from the first one with "declared 3 pinned writer(s) and 3 are
// held" while a company's inbox rows grew for the life of the deployment.
//
// Through a real tick rather than against the constant, because what has to
// hold is how many pins are actually taken by the time the sweep asks for one.
// A tick reports every job's failure joined together, so a job that could not
// take its writer is an error here whichever job it was.
func TestTheSweepGetsAWriterWhileEveryDomainIsApplying(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	if _, err := e.Maintenance().Tick(t.Context()); err != nil {
		t.Fatalf("a sweep running beside this node's applying domains: %v\n"+
			"raise the pinned-writer count openStore declares: the apply loops "+
			"hold theirs for life, so the sweep needs one of its own", err)
	}
}
