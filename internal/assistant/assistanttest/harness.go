package assistanttest

import (
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/evals"
)

// Company is [evals.Company] for a suite: the assistant on when on is set, and
// each of seats a human seat of that handle.
func Company(t testing.TB, on bool, seats ...string) assistant.Company {
	t.Helper()
	c, err := evals.Company(on, seats...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
