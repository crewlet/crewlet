package e2e

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PERSON'S STOP ENDS THE CODING RUN THE SEAT HAS OUT, END TO END.
//
// The run is the stand-in coding CLI in a real `direct` box, held: nothing
// will ever release it, so it would run for ever. A pause with `stop_running`
// lands in the fleet's record, and the node holding the seat must carry it out
// on the run itself: the run's record gone, its box reclaimed off the disk, its
// turn never resumed into a review — and on the record, the run announced as
// stopped (never lost) and the turn that was parked on it closed as stopped by
// the person who paused the seat.
//
// Mutations: leave the pause watch's stop call out and the run is never ended;
// announce the stop as a loss and a `sandbox_run_failed` is on the turn; close
// no turn and its last turn_completed is still the park.
func TestAStopEndsTheRunningCodingRunAndClosesItsTurnAsStopped(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// NOTHING WRITES THIS FILE: the run holds until something ends it.
	release := filepath.Join(t.TempDir(), "never-released")
	env := map[string]string{codingModeEnv: codingModeHeld, codingReleaseEnv: release}
	model := newScriptedModel(t)
	n := &codingNode{node: bootCompany(t, sandboxCompany(t, model, stateDir), model, env),
		stateDir: stateDir}
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wake(t, "swe", "the api test is flaking, please fix it")
	waitFor(t, "the run to be out with its turn parked on it", func() bool {
		for _, run := range n.activeRuns(t) {
			if run.Status == sandbox.StatusRunning && len(run.ExecuteState) > 0 {
				return true
			}
		}
		return false
	}, n.diagnose)
	running := n.runFor(t, sandbox.StatusRunning)
	if len(n.liveBoxes(t)) == 0 {
		t.Fatal("the running run has no box on disk, so this case reclaims nothing")
	}

	seat := uuid.MustParse(seatAgentID(t, n.node, "swe"))
	if _, created, err := n.engine.Backends().Fleet.CreateSeatPause(t.Context(), coord.SeatPause{
		Seat: seat, By: "e2e", ByKind: iam.ActorOperator, OperatorID: "e2e",
		Reason: "wrong repository", StopRunning: true, At: time.Now().UTC(),
	}); err != nil || !created {
		t.Fatalf("CreateSeatPause = %v, %v", created, err)
	}

	waitFor(t, "the stopped run to leave the record", func() bool {
		return len(n.activeRuns(t)) == 0
	}, n.diagnose)
	waitFor(t, "its box to be reclaimed", func() bool { return len(n.liveBoxes(t)) == 0 })
	if got := countOf(n.model.seen(), "review"); got != 0 {
		t.Fatalf("the stopped run's turn was reviewed %d times: a stop is not a completion", got)
	}

	var (
		stops, losses, turnStops int
		lastEnd, lastStopped     bool
	)
	waitFor(t, "the turn's stop on the record", func() bool {
		evs, err := n.engine.Backends().Store.Events().Turn(t.Context(), running.TurnID, time.Now())
		if err != nil {
			t.Fatalf("the turn's events: %v", err)
		}
		stops, losses, turnStops = 0, 0, 0
		lastEnd, lastStopped = false, false
		for _, ev := range evs {
			var rec struct {
				Suspended bool   `json:"suspended"`
				Stopped   bool   `json:"stopped"`
				Failed    bool   `json:"failed"`
				StoppedBy string `json:"stopped_by"`
			}
			_ = json.Unmarshal(ev.Payload, &rec)
			switch ev.Type {
			case "sandbox_run_stopped":
				if rec.StoppedBy == "e2e" {
					stops++
				}
			case "sandbox_run_failed":
				losses++
			case "agent_turn_stopped":
				if rec.StoppedBy == "e2e" {
					turnStops++
				}
			case "agent_turn_completed":
				lastStopped = rec.Stopped && !rec.Failed
			case "turn_completed":
				lastEnd = !rec.Suspended
			}
		}
		return stops > 0 && turnStops > 0 && lastEnd && lastStopped
	}, n.diagnose)
	if losses != 0 {
		t.Fatalf("the stopped run was announced as lost %d times", losses)
	}
	if stops != 1 || turnStops != 1 {
		t.Fatalf("the stop is on the record %d times for the run and %d for the turn, want once each",
			stops, turnStops)
	}
}
