package coordtest_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// opaque hides a backend's Advancer, leaving only the coord.Backend surface.
type opaque struct{ coord.Backend }

// The suite must certify a backend whose clock it cannot move — which is
// every real coordination store. Advancer is an optimisation, and a case that
// quietly came to depend on it would pass here for years and then fail on the
// first out-of-process backend, where the only way past a TTL is to wait.
//
// So the whole suite runs a second time against the twin with its Advance
// method hidden, taking the sleeping path through every lapse.
func TestSuiteRunsWithoutAnAdvancer(t *testing.T) {
	if _, ok := coord.Backend(memory.New()).(coordtest.Advancer); !ok {
		t.Fatal("the twin no longer implements Advancer — this test would be certifying nothing")
	}
	if _, ok := coord.Backend(opaque{memory.New()}).(coordtest.Advancer); ok {
		t.Fatal("opaque still exposes Advance")
	}
	coordtest.Run(t, func(t *testing.T) coord.Backend { return opaque{memory.New()} })
}
