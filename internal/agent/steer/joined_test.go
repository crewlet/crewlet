package steer_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A JOINED RECORD NAMES A STEP ONLY WHERE ITS OUTCOME CAME TO ONE: a wake read
// or handed over and never confirmed names the step and job it came to, and an
// unread one names its job and no step; every record names the wake, the
// deliveries it stood for and who wrote it, and is sent in UTC.
//
// Mutation: name the step on an unread wake, or keep the writer's zone, and
// this goes red.
func TestAJoinedRecordNamesAStepOnlyWhereItsOutcomeCameToOne(t *testing.T) {
	t.Parallel()
	turn := steer.Turn{Agent: "a-1", AgentHandle: "swe", Role: "SWE", TurnID: "run-1", WorkKey: "wk-1"}
	w := steer.Wake{ID: "w-1", Triggers: []string{"ev-1"}, By: "bob", ByKind: iam.ActorHuman,
		Label: provenance.Label{Source: provenance.SourceChatThread}, OwesAnswer: true,
		At: time.Date(2026, 11, 20, 10, 0, 0, 0, time.FixedZone("x", 3600))}
	at := &steer.Reading{Phase: types.PhaseExecute, Iteration: 3, Round: 1,
		Boundary: types.SteerBoundaryToolBatch, Launch: "job-1"}
	for _, outcome := range []types.JoinOutcome{types.JoinDelivered, types.JoinUnconfirmed} {
		got := steer.Joined(turn, w, "job-1", outcome, at)
		if got.Outcome != outcome || got.Boundary != types.SteerBoundaryToolBatch || got.Iteration != 3 ||
			got.LaunchID != "job-1" || got.WakeID != "w-1" || !slices.Equal(got.TriggerIDs, []string{"ev-1"}) ||
			got.WrittenBy != "bob" || got.WrittenByKind != "human" || got.WrittenOn != "chat_thread" ||
			!got.OwesAnswer || got.TurnID != "run-1" || got.SentAt.Location() != time.UTC ||
			!got.SentAt.Equal(w.At) {
			t.Errorf("a %s wake is recorded %+v", outcome, got)
		}
	}
	if got := steer.Joined(turn, w, "job-1", types.JoinUnread, at); got.Boundary != "" || got.Iteration != 0 ||
		got.LaunchID != "job-1" {
		t.Errorf("an unread wake is recorded at a step: %+v", got)
	}
}
