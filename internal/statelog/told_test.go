package statelog_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TOLD REFUSAL TELLS ITS SENTENCE ALONE AND KEEPS WHAT IT STANDS ON FOR THE
// LOG: Tell is the sentence, found through any wrap; Error is the sentence and
// what it stands on; errors.Is still finds the sentinel underneath. Mutation:
// return the error's text from Tell, or drop Unwrap, and this fails.
func TestATellingTellsItsSentenceAndKeepsItsFaultForTheLog(t *testing.T) {
	t.Parallel()
	fault := errors.New("nats: timeout")
	told := &statelog.Telling{Say: "the log couldn't be read",
		Err: fmt.Errorf("%w: %w", statelog.ErrReanchorRefused, fault)}
	wrapped := fmt.Errorf("engine: reanchor CREWLET_TRACKER_LOG: %w", told)

	said, ok := statelog.Tell(wrapped)
	if !ok || said != "the log couldn't be read" {
		t.Errorf("Tell = %q, %v; want the sentence alone", said, ok)
	}
	if !strings.Contains(wrapped.Error(), "nats: timeout") {
		t.Errorf("the error %q lost what the sentence stands on", wrapped)
	}
	if !errors.Is(wrapped, statelog.ErrReanchorRefused) || !errors.Is(wrapped, fault) {
		t.Errorf("errors.Is no longer finds what the refusal stands on: %v", wrapped)
	}
	if _, ok := statelog.Tell(fault); ok {
		t.Error("a fault that tells nothing was read as told")
	}
}

// EVERY GATE AND CAPACITY REFUSAL THIS PACKAGE MAKES IS TOLD, without the
// package that wrote it: the eviction and readmission refusals, and the
// capacity window's refusals of a phase this build doesn't know and of a
// raise that has used its attempts. An eviction naming no node says so without
// the empty name it used to print between two spaces. Mutation: put the
// "statelog: " prefix back in any Tell and this fails.
func TestEveryGateAndCapacityRefusalIsTold(t *testing.T) {
	t.Parallel()
	op := coord.MaintenanceOperation{Stream: "CREWLET_TRACKER_LOG",
		Phase: "melting", TargetMaxBytes: 1 << 30, Attempt: statelog.ControlRaiseAttempts}
	_, phaseErr := statelog.PermitPhase(op, statelog.PhaseEvidence{})
	_, _, abandonErr := statelog.AbandonPhase(op)
	_, attemptsErr := statelog.NextAttempt(op, 1<<20, op.EnteredAt)
	for name, c := range map[string]struct {
		err  error
		says string
	}{
		"a live lease": {statelog.PermitEviction("node-4",
			[]statelog.Presence{{NodeID: "node-4"}}, false), "node-4 may not be evicted"},
		"no node": {statelog.PermitEviction("", nil, false), "no node was named"},
		"a node behind the floor": {&statelog.ReadmissionRefusal{NodeID: "node-4",
			Domain: "tracker", Published: true, Seq: 1200,
			Bound: statelog.ReadmissionBound{Domain: "tracker", Floor: 9000, First: 8800}},
			"node-4 may not be readmitted"},
		"an unknown phase":         {phaseErr, `phase "melting"`},
		"an unknown phase abandon": {abandonErr, `phase "melting"`},
		"its attempts used":        {attemptsErr, "needs a person now"},
	} {
		said, ok := statelog.Tell(c.err)
		if !ok {
			t.Errorf("%s: %v tells nothing", name, c.err)
			continue
		}
		if !strings.HasPrefix(said, c.says) && !strings.Contains(said, c.says) {
			t.Errorf("%s: tells %q, want it to say %q", name, said, c.says)
		}
		for _, leak := range []string{"statelog:", "—", "  "} {
			if strings.Contains(said, leak) {
				t.Errorf("%s: tells %q, carrying %q", name, said, leak)
			}
		}
	}
}
