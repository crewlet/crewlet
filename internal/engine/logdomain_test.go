package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// EVERY REGISTERED DOMAIN IS NAMED FROM ITS STREAM: an applied assistant
// write tells open screens the domain its position landed on, by the name a
// screen's read floor is kept under, so a stream a domain owns that resolved
// to no name would raise no floor anywhere. A stream no domain owns names
// none.
//
// Mutation: answer the stream's own name and every case here fails.
func TestEveryRegisteredDomainIsNamedFromItsStream(t *testing.T) {
	t.Parallel()
	for _, d := range engine.Domains() {
		if got := engine.LogDomain(d.Stream().Name); got != d.Name() {
			t.Errorf("stream %s names domain %q, want %q", d.Stream().Name, got, d.Name())
		}
	}
	if got := engine.LogDomain("CREWLET_UNKNOWN_LOG"); got != "" {
		t.Errorf("a stream no domain owns names %q, want none", got)
	}
}
