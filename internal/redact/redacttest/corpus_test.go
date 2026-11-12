package redacttest_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// EVERY SHAPE IN THE CORPUS IS ONE THE PASS RECOGNISES, so a canary test that
// plants it proves something: a shape the pass stops knowing fails here
// before it passes anywhere else by accident.
func TestEveryShapeIsAKnownCredential(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, s := range redacttest.Shapes() {
		if seen[s.Name] {
			t.Errorf("the shape %s is listed twice", s.Name)
		}
		seen[s.Name] = true
		if !redact.Contains(s.Sample) {
			t.Errorf("the pass does not recognise the %s shape: %q", s.Name, s.Sample)
		}
		if redact.Secrets(s.Sample) == s.Sample {
			t.Errorf("the pass leaves the %s shape as it is", s.Name)
		}
	}
	if len(seen) < 15 {
		t.Fatalf("%d shapes: the corpus is too small to prove anything", len(seen))
	}
}
