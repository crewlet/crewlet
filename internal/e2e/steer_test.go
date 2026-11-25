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
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A PERSON STEERS A LIVE EXECUTOR, END TO END.
//
// The run is the stand-in Claude Code in a real `direct` box, held open, with
// the runner's settings file as hooks. A note sent through
// `/operator/act/steer_turn` waits sealed on the run's record; released, the
// run's one batch call has its PostToolBatch hook's steer_take serve it, and
// the collection publishes it delivered at `tool_batch`; the review opens with it.
//
// Mutations: write no settings file, or file no hook records, and the review
// reads the note, naming no job; drop the informational line from the
// transcript decoder and the run's record never shows it arrive; answer no
// version probe and the held process is the probe, not the harness.
func TestAPersonSteersALiveExecutor(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// THE RUN HOLDS UNTIL THIS FILE APPEARS, so the note is filed while it
	// works and taken at the step it makes after.
	release := filepath.Join(t.TempDir(), "release")
	argv := filepath.Join(stateDir, "argv.txt")
	env := map[string]string{codingModeEnv: codingModeHeld, codingReleaseEnv: release, codingArgvEnv: argv}
	model := newScriptedModel(t)
	n := &codingNode{node: bootCompany(t, sandboxCompany(t, model, stateDir), model, env),
		stateDir: stateDir, argvPath: argv}
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
	waitFor(t, "the harness to start", func() bool {
		_, err := os.Stat(argv)
		return err == nil
	}, n.diagnose)

	// THE HARNESS IS WHAT WORKS, started on a release that reads its notes: with
	// its hooks, and its standing instructions told of them. The version
	// probe the run's script makes first is answered and gone. Its hooks are in
	// the notes' settings' guarded copy, which a run with a bridge is handed in
	// the file's place on every release: the pinned copy's settings, with the
	// bridge's guard beside the notes' hooks.
	args := n.agentArgv(t)
	if filepath.Base(valueOf(args, "--settings")) != "claude-settings-notes-guarded.json" ||
		filepath.Base(valueOf(args, "--append-system-prompt-file")) != "system-notes.md" {
		t.Fatalf("the held run is not the harness started with its hooks and told of notes: %q", args)
	}
	system := valueOf(args, "--append-system-prompt-file")
	if text, err := os.ReadFile(system); err != nil || //nolint:gosec // the run's own box, under the case's state dir
		!strings.Contains(string(text), executor.ClaudeCode{}.NoteInstruction()) {
		t.Errorf("the run's standing instructions are not told of notes (%v):\n%s", err, text)
	}

	const note = "Target the release/2 branch, not main: the fix ships in the patch release."
	noteID := steerRunningTurn(t, n, running.TurnID, note)

	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the held run: %v", err)
	}
	reviewedWith(t, n, model, note)

	// THE RUN READ IT, AT ITS STEP: delivered at the tool batch, by its job.
	delivered := deliveredRecord(t, n, running.TurnID, noteID)
	if delivered["boundary"] != "tool_batch" || delivered["launch_id"] != running.LaunchID ||
		delivered["phase"] != "execute" {
		t.Errorf("the note was delivered %v; want it read by job %s at its tool batch, in its "+
			"execute phase", delivered, running.LaunchID)
	}

	// AND THE LAUNCH'S OWN RECORD SAYS IT ARRIVED, in the harness's own words.
	waitFor(t, "the run's transcript to show the note arrive", func() bool {
		rows, _, err := n.engine.Backends().Store.Events().Phases(t.Context(), "", 60, 0, nil, time.Now())
		if err != nil {
			t.Fatalf("phases: %v", err)
		}
		for _, row := range rows {
			var rec map[string]any
			if row.Type == "executor_collected" && json.Unmarshal(row.Payload, &rec) == nil {
				transcript, _ := rec["activity_transcript"].(string)
				return strings.Contains(transcript, "[steer] a note from ")
			}
		}
		return false
	}, n.diagnose)
	waitFor(t, "the steered run to settle", func() bool { return len(n.activeRuns(t)) == 0 })
}

// A HOOK CALL ITS HARNESS GAVE UP SERVES ITS NOTE AT THE NEXT STEP, END TO END.
// The released run's batch hook is served the note and gives the call up, as
// Claude Code does past its hook's timeout: its answer discarded, nothing
// filed in its session, a `notifications/cancelled` sent over the bridge. The
// bridge's front hands the cancel to the run's inbox, which loses the serve,
// so the run's finish is served the note again; its session shows that hook
// took it, and the collection records it delivered there, by the job.
//
// Mutation: hear no cancel at the bridge's front, and the finish is served
// nothing, the review reading the note instead.
func TestAHookCallItsHarnessGaveUpServesItsNoteAtTheNextStep(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	argv := filepath.Join(stateDir, "argv.txt")
	env := map[string]string{codingModeEnv: codingModeHeld, codingReleaseEnv: release, codingArgvEnv: argv,
		codingCancelEnv: "1"}
	model := newScriptedModel(t)
	n := &codingNode{node: bootCompany(t, sandboxCompany(t, model, stateDir), model, env),
		stateDir: stateDir, argvPath: argv}
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
	waitFor(t, "the harness to start", func() bool {
		_, err := os.Stat(argv)
		return err == nil
	}, n.diagnose)
	const note = "Target the release/2 branch, not main: the fix ships in the patch release."
	noteID := steerRunningTurn(t, n, running.TurnID, note)
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the held run: %v", err)
	}
	reviewedWith(t, n, model, note)
	delivered := deliveredRecord(t, n, running.TurnID, noteID)
	if delivered["boundary"] != "finish" || delivered["launch_id"] != running.LaunchID {
		t.Errorf("the note was delivered %v; want it read by job %s at its finish, served again there "+
			"after its batch's hook gave the call up", delivered, running.LaunchID)
	}
	waitFor(t, "the steered run to settle", func() bool { return len(n.activeRuns(t)) == 0 })
}

// steerRunningTurn sends a person's note to the turn whose executor is out,
// through the route the dashboard presses, and returns its id: answered
// pending, read at the run's next step, and waiting on the run's record.
func steerRunningTurn(t *testing.T, n *codingNode, turnID, note string) string {
	t.Helper()
	status, raw := bearer(t, n.node, http.MethodPost, "/operator/act/"+builtin.SteerTurnTool,
		map[string]any{"args": map[string]any{"turn_id": turnID, "note": note}},
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
	return noteID
}

// reviewedWith waits for the steered turn's review, and holds it to opening
// with the note — so the work is judged against the correction.
func reviewedWith(t *testing.T, n *codingNode, model *scriptedModel, note string) {
	t.Helper()
	waitFor(t, "the steered run's turn to be reviewed", func() bool {
		return slices.Contains(model.seen(), "review")
	}, n.diagnose)
	if !slices.ContainsFunc(model.promptsOf("review"), func(prompt string) bool {
		return strings.Contains(prompt, note)
	}) {
		t.Errorf("the review did not open with the note: %q", model.promptsOf("review"))
	}
}

// deliveredRecord waits for the turn's record of the note delivered, and
// returns it.
func deliveredRecord(t *testing.T, n *codingNode, turnID, noteID string) map[string]any {
	t.Helper()
	var delivered map[string]any
	waitFor(t, "the note's delivery on the record", func() bool {
		evs, _, err := n.engine.Backends().Store.Events().Turn(t.Context(), turnID, 0, time.Now())
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
	return delivered
}

// valueOf is the argument following flag in an argv the stand-in recorded, or
// "".
func valueOf(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}
