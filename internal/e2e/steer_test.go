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
// line from the transcript decoder and the run's record never shows it arrive;
// answer no version probe and the held process is the probe, not the CLI.
func TestAPersonSteersALiveCodingRun(t *testing.T) {
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
	waitFor(t, "the CLI to start", func() bool {
		_, err := os.Stat(argv)
		return err == nil
	}, n.diagnose)

	// THE CLI IS WHAT WORKS, started on a release that reads its notes: with
	// its hooks, and its standing instructions told of them. The version
	// probe the run's script makes first is answered and gone.
	args := n.agentArgv(t)
	system := ""
	if i := slices.Index(args, "--append-system-prompt-file"); i >= 0 && i+1 < len(args) {
		system = args[i+1]
	}
	if !slices.Contains(args, "--settings") || filepath.Base(system) != "system-notes.md" {
		t.Fatalf("the held run is not the CLI started with its hooks and told of notes: %q", args)
	}
	if text, err := os.ReadFile(system); err != nil || //nolint:gosec // the run's own box, under the case's state dir
		!strings.Contains(string(text), executor.ClaudeCodeSteerInstruction) {
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

	// AND THE LAUNCH'S OWN RECORD SAYS IT ARRIVED, in the CLI's own words.
	waitFor(t, "the run's transcript to show the note arrive", func() bool {
		rows, _, err := n.engine.Backends().Store.Events().Phases(t.Context(), "", 60, nil, time.Now())
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

// A CLAUDE CODE OLDER THAN ITS NOTE FLOOR LEAVES A NOTE TO THE REVIEW, END TO
// END. The stand-in answers the run's version probe with 2.1.150, a release
// that runs a settings file's hooks and drops what its Stop hook answers. So the
// run's script starts it without its settings file and with its standing
// instructions as they stand — never told of notes it cannot read — and says so
// on the run's error stream. A note a person sends while it works waits on the
// record through the run, which takes nothing; the review reads it, and the
// run's collection says the run never asked for its notes.
//
// Mutations: name the settings whatever the release, or hand an older release
// the instructions told of notes, and the argv here shows it; answer no version
// probe and the run starts on the gate's fallback, with its hooks.
func TestAClaudeCodeOlderThanItsNoteFloorLeavesANoteToTheReview(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	argv := filepath.Join(stateDir, "argv.txt")
	env := map[string]string{codingModeEnv: codingModeHeld, codingReleaseEnv: release,
		codingArgvEnv: argv, codingVersionEnv: "2.1.150 (Claude Code)"}
	model := newScriptedModel(t)
	n := &codingNode{node: bootCompany(t, sandboxCompany(t, model, stateDir), model, env),
		stateDir: stateDir, argvPath: argv}
	mark := logs.loudMark()
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
	waitFor(t, "the CLI to start", func() bool {
		_, err := os.Stat(argv)
		return err == nil
	}, n.diagnose)

	// STARTED WITHOUT ITS HOOKS, ON ITS INSTRUCTIONS AS THEY STAND.
	args := n.agentArgv(t)
	if slices.Contains(args, "--settings") {
		t.Errorf("a release older than the floor was started with its hooks: %q", args)
	}
	system := ""
	if i := slices.Index(args, "--append-system-prompt-file"); i >= 0 && i+1 < len(args) {
		system = args[i+1]
	}
	if filepath.Base(system) != "system.md" {
		t.Fatalf("the run was handed the system text %q, want its instructions as they stand (%q)",
			system, args)
	}
	text, err := os.ReadFile(system) //nolint:gosec // the run's own box, under the case's state dir
	if err != nil {
		t.Fatalf("read the run's system text: %v", err)
	}
	if strings.Contains(string(text), executor.ClaudeCodeSteerInstruction) {
		t.Errorf("a run started without its hooks was told of notes:\n%s", text)
	}
	said, err := os.ReadFile(filepath.Join(filepath.Dir(system), "err.log")) //nolint:gosec // as above
	if err != nil || !strings.Contains(string(said), "older than 2.1.163") {
		t.Errorf("the run's error stream does not say why it runs without its hooks (%v): %q", err, said)
	}

	const note = "Target the release/2 branch, not main: the fix ships in the patch release."
	noteID := steerRunningTurn(t, n, running.TurnID, note)
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the held run: %v", err)
	}
	reviewedWith(t, n, model, note)

	// THE REVIEW READ IT, AT ITS ROUND: no job of the run took it.
	delivered := deliveredRecord(t, n, running.TurnID, noteID)
	if delivered["boundary"] != "round" || delivered["launch_id"] != nil {
		t.Errorf("the note was delivered %v; want it read by the review at its round, by no job", delivered)
	}
	waitFor(t, "the collection to say the run never asked for its notes", func() bool {
		lines, _ := logs.loudSince(mark)
		return slices.ContainsFunc(lines, func(line string) bool {
			return strings.Contains(line, "steer_reader_never_ran") && strings.Contains(line, running.TurnID)
		})
	}, n.diagnose)
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
		evs, err := n.engine.Backends().Store.Events().Turn(t.Context(), turnID, time.Now())
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
