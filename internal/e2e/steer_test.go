package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PERSON STEERS A LIVE CODING RUN, END TO END.
//
// The run is the stand-in Claude Code in a real `direct` box, held open, its
// hooks the settings file the runner wrote. A person sends a note through the
// route the dashboard presses (`/operator/act/steer_turn`) while the run
// works: it waits sealed on the run's record, answered as read at the run's
// next step. Released, the run makes its one call — a batch — and its
// PostToolBatch hook calls steer_take over the run's own bridge session, which
// takes the note: recorded read by the run's job, published delivered at the
// `tool_batch` boundary under that job, and printed into the run's transcript
// by the CLI's own line. The run completes, the turn resumes, and its review
// opens with the note — so the work is judged against the correction.
//
// Mutations: write no settings file and no hook asks, so the note is read by the
// review alone and the delivered record names no job; drop the informational
// line from the transcript decoder and the run's record never shows it arrive.
func TestAPersonSteersALiveCodingRun(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// THE RUN HOLDS UNTIL THIS FILE APPEARS, so the note is filed while it
	// works and taken at the step it makes after.
	release := filepath.Join(t.TempDir(), "release")
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

	const note = "Target the release/2 branch, not main: the fix ships in the patch release."
	status, raw := bearer(t, n.node, http.MethodPost, "/operator/act/"+builtin.SteerTurnTool,
		map[string]any{"args": map[string]any{"turn_id": running.TurnID, "note": note}},
		map[string]string{opkey.Header: uuid.Must(uuid.NewV7()).String()})
	var answer struct {
		Outcome string         `json:"outcome"`
		Receipt map[string]any `json:"receipt"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &answer) != nil {
		t.Fatalf("POST /operator/act/%s answered %d: %s", builtin.SteerTurnTool, status, raw)
	}
	if answer.Outcome != "pending" || answer.Receipt["reads"] != "next_step" {
		t.Fatalf("the note was answered %s, %v; want it pending, read at the run's next step",
			answer.Outcome, answer.Receipt)
	}
	noteID, _ := answer.Receipt["note_id"].(string)
	if filed := n.runFor(t, sandbox.StatusRunning).Steer.Notes; len(filed) != 1 || !filed[0].Waiting() {
		t.Fatalf("the run's record holds %+v, want the one note, waiting", filed)
	}

	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the held run: %v", err)
	}
	waitFor(t, "the steered run's turn to be reviewed", func() bool {
		return slices.Contains(model.seen(), "review")
	}, n.diagnose)
	if !slices.ContainsFunc(model.promptsOf("review"), func(prompt string) bool {
		return strings.Contains(prompt, note)
	}) {
		t.Errorf("the review did not open with the note: %q", model.promptsOf("review"))
	}

	// THE RUN READ IT, AT ITS STEP: delivered at the tool batch, by its job.
	var delivered map[string]any
	waitFor(t, "the note's delivery on the record", func() bool {
		evs, err := n.engine.Backends().Store.Events().Turn(t.Context(), running.TurnID, time.Now())
		if err != nil {
			t.Fatalf("the turn's events: %v", err)
		}
		for _, ev := range evs {
			var rec map[string]any
			if ev.Type == "agent_turn_steered" && json.Unmarshal(ev.Payload, &rec) == nil &&
				rec["outcome"] == "delivered" && rec["note_id"] == noteID {
				delivered = rec
				return true
			}
		}
		return false
	}, n.diagnose)
	if delivered["boundary"] != "tool_batch" || delivered["launch_id"] != running.LaunchID ||
		delivered["phase"] != "execute" {
		t.Errorf("the note was delivered %v; want it read by job %s at its tool batch, in its "+
			"execute phase", delivered, running.LaunchID)
	}

	// AND THE RUN'S OWN RECORD SAYS IT ARRIVED, in the CLI's own words.
	waitFor(t, "the run's transcript to show the note arrive", func() bool {
		rows, _, err := n.engine.Backends().Store.Events().Phases(t.Context(), "", 60, nil, time.Now())
		if err != nil {
			t.Fatalf("phases: %v", err)
		}
		for _, row := range rows {
			var rec map[string]any
			if json.Unmarshal(row.Payload, &rec) == nil && rec["phase"] == "sandbox" {
				transcript, _ := rec["activity_transcript"].(string)
				return strings.Contains(transcript, "[steer] a note from ")
			}
		}
		return false
	}, n.diagnose)
	waitFor(t, "the steered run to settle", func() bool { return len(n.activeRuns(t)) == 0 })
}
