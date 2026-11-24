package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// THE NOTE CONTRACT, held over every registered runner (ADR-0177): each runner
// supplies its fixture below, and a runner added to the registry stays red
// until it does. A fixture's cases are its harness's own mechanism (a hook, a
// plugin); the tables hold that every runner meets the contract by some.

// noteFixture is one runner's cases of the note contract.
type noteFixture struct {
	// reads says the harness takes a person's notes between its steps.
	reads bool
	// wakeRead is what a session handed wake reads back as.
	wakeRead func(t *testing.T, wake string) []executor.TranscriptEntry
	// confirms is the case that a note served is confirmed off the job's own
	// record: its hooks' receipts on its thread, or its plugin's delivery.
	confirms func(t *testing.T)
	// reaches is the case that a real box's notes reach the bridge's route.
	reaches func(t *testing.T)
	// trailing is the case of a note no reply followed (ADR-0177 §3).
	trailing trailingNote
}

// trailingNote is a runner's case of a TRAILING NOTE: one its harness
// recorded or sent, which no reply of the model followed.
type trailingNote struct {
	// handsBack states, once for the runner, whether its harness's resume was
	// measured to hand such a note back; the measurement is named beside it.
	handsBack bool
	// held is whether the runner's confirm holds such a note.
	held func(t *testing.T) bool
}

// noteFixtures is every runner's fixture, by its harness's name.
var noteFixtures = map[string]noteFixture{
	executor.ClaudeCodeName: {
		reads: true,
		wakeRead: func(t *testing.T, wake string) []executor.TranscriptEntry {
			return readClaudeRecords(t, []string{hookContext(t, wake)})
		},
		confirms: executor.ClaudeConfirmsATake,
		reaches:  realBoxsHooksReachTheBridgeTool,
		// A resume hands back a firing's context (2.1.263 to 2.1.322, the
		// hook receipts' measurement).
		trailing: trailingNote{handsBack: true, held: executor.ClaudeTrailingNoteHeld},
	},
	executor.OpenCodeName: {
		reads: true,
		wakeRead: func(t *testing.T, wake string) []executor.TranscriptEntry {
			return readOpenCode(t, openCodeCaptureOf(t, openCodeDoc(openCodeUser("Fix the flake."),
				openCodeUser(wake))), executor.TranscriptReadBounds()).Entries
		},
		confirms: pluginAdmitsNotesAtTheRunsNextStep,
		reaches:  pluginReadsTheBridgesOwnNotesRoute,
		// Its plugin confirms as the request carrying the note plain is sent
		// (the context hook, measured on 2.3.2), before any reply.
		trailing: trailingNote{handsBack: true, held: openCodeTrailingNoteHeld},
	},
	executor.CodexName: {
		reads:    true,
		wakeRead: codexWakeRead,
		confirms: codexConfirmsATake,
		reaches:  codexRealBoxsNotesReachTheBridgesRoute,
		// Not measured on 0.171.0: a later reply is required.
		trailing: trailingNote{held: executor.CodexTrailingNoteHeld},
	},
	executor.GeminiCLIName: {
		reads: true,
		wakeRead: func(t *testing.T, wake string) []executor.TranscriptEntry {
			return readGemini(t, geminiSessionWith(t, wake))
		},
		confirms: executor.GeminiConfirmsATake,
		reaches:  geminiTakeReachesTheBridgeTool,
		// Measured on 0.70.0: the note's user entry is written before the next
		// request, kept through a SIGKILL of every gemini process, and sent by
		// a resume (geminiconfirm.go).
		trailing: trailingNote{handsBack: true, held: executor.GeminiTrailingNoteHeld},
	},
	executor.QwenCodeName: {
		reads: true,
		wakeRead: func(t *testing.T, wake string) []executor.TranscriptEntry {
			return readQwen(t, qwenSessionWith(t, wake))
		},
		confirms: qwenHelperConfirmsEachNoteByItsRecord,
		reaches:  qwenHelperReachesTheBridgesOwnNotesRoute,
		// Qwen Code continues no session: a later reply is required.
		trailing: trailingNote{held: executor.QwenTrailingNoteHeld},
	},
	executor.CopilotName: {
		reads:    true,
		wakeRead: executor.CopilotWakeRead,
		confirms: executor.CopilotConfirmsATake,
		reaches:  copilotReachesTheBridgesRoute,
		// Not measured on 1.0.114: a later reply is required.
		trailing: trailingNote{held: executor.CopilotTrailingNoteHeld},
	},
	executor.KimiName: {
		reads:    true,
		wakeRead: executor.KimiWakeRead,
		confirms: executor.KimiConfirmsATake,
		reaches:  kimiReachesTheBridgesRoute,
		// Not measured on 2.7.2: a later reply is required.
		trailing: trailingNote{held: executor.KimiTrailingNoteHeld},
	},
	executor.GrokName: {
		reads:    true,
		wakeRead: executor.GrokWakeRead,
		confirms: executor.GrokConfirmsATake,
		reaches:  grokReachesTheBridgesTool,
		// Not measured on 1.0.92: a later reply is required.
		trailing: trailingNote{held: executor.GrokTrailingNoteHeld},
	},
	executor.MuseName: {
		reads:    true,
		wakeRead: executor.MuseWakeRead,
		confirms: executor.MuseConfirmsATake,
		reaches:  museReachesTheBridgesTool,
		// Not measured on 1.4.25: a later reply is required.
		trailing: trailingNote{held: executor.MuseTrailingNoteHeld},
	},
	executor.HermesName: {
		reads:    true,
		wakeRead: executor.HermesWakeRead,
		confirms: executor.HermesConfirmsATake,
		reaches:  hermesReachesTheBridgesRoute,
		// Measured on 0.23.4: a note steered while the turn's last request
		// ran, no tool call after it, was kept nowhere and the next resume
		// did not hand it back, so a later reply is required.
		trailing: trailingNote{held: executor.HermesTrailingNoteHeld},
	},
	executor.PiName: {
		reads:    true,
		wakeRead: executor.PiWakeRead,
		confirms: executor.PiConfirmsATake,
		reaches:  piReachesTheBridgesRoute,
		// Not measured on 1.6.0: a later reply is required.
		trailing: trailingNote{held: executor.PiTrailingNoteHeld},
	},
}

// noteFixtureOf is h's fixture, failing the case where h supplies none.
func noteFixtureOf(t *testing.T, h executor.Harness) noteFixture {
	t.Helper()
	fixture, ok := noteFixtures[h.Name()]
	if !ok {
		t.Fatalf("%s is a registered runner with no note fixture: add its cases to noteFixtures", h.Name())
	}
	return fixture
}

// EVERY RUNNER SAYS WHETHER IT TAKES NOTES AS ITS FIXTURE STATES, to the runner
// it is driven by, so its runs are told notes arrive and one that never asked
// is said at collection; and one that takes notes meets the contract's every
// case: its wakes read back, a served note is confirmed, a real box's notes
// reach the bridge. A runner that reads notes and cannot confirm a take fails.
//
// Mutation: have a runner's harness stop reading notes, or drop a fixture's
// confirm case, and this goes red.
func TestEveryRunnerThatTakesNotesSaysSoAndMeetsTheContract(t *testing.T) {
	t.Parallel()
	for _, h := range executor.Registered() {
		fixture := noteFixtureOf(t, h)
		if got := executor.New(h).ReadsNotes(); got != fixture.reads {
			t.Errorf("%s's runner says it reads notes: %v, its fixture says %v", h.Name(), got, fixture.reads)
		}
		if fixture.reads && (fixture.wakeRead == nil || fixture.confirms == nil || fixture.reaches == nil) {
			t.Errorf("%s reads notes, and its fixture lacks a case of the contract: a wake read, a confirm "+
				"path or a real box's route", h.Name())
		}
	}
}

// A TRAILING NOTE NO REPLY FOLLOWED IS HELD ONLY WHERE A RESUME HANDS IT BACK
// (ADR-0177 §3): a harness records or sends a note before the request carrying
// it is answered, so a run killed or refused after the hand-over leaves one.
// Each runner's fixture states once whether its harness's resume was measured
// to hand such a note back, and its confirm holds the note exactly there.
//
// Mutation: hold a runner's trailing note off its record alone, or state a
// measured resume for a runner whose confirm waits for a reply, and its row
// goes red.
func TestATrailingNoteNoReplyFollowedIsHeldOnlyWhereAResumeHandsItBack(t *testing.T) {
	t.Parallel()
	for _, h := range executor.Registered() {
		fixture := noteFixtureOf(t, h)
		if !fixture.reads {
			continue
		}
		if fixture.trailing.held == nil {
			t.Errorf("%s reads notes, and its fixture states no trailing note's case", h.Name())
			continue
		}
		t.Run(h.Name(), func(t *testing.T) {
			t.Parallel()
			if got := fixture.trailing.held(t); got != fixture.trailing.handsBack {
				t.Errorf("%s holds a trailing note no reply followed: %v; its resume hands one back: %v",
					h.Name(), got, fixture.trailing.handsBack)
			}
		})
	}
}

// A NOTE SERVED IS CONFIRMED OFF ITS JOB'S OWN RECORD, on every runner that
// reads notes: its hooks' records name the takes asked (Claude Code), or its
// plugin confirms only what the harness delivered (OpenCode). A note no record
// confirms waits for its review, and a wake for its own turn.
//
// Mutation: each runner's case names its own.
func TestANoteServedIsConfirmedOffItsJobsOwnRecord(t *testing.T) {
	t.Parallel()
	for _, h := range executor.Registered() {
		if fixture := noteFixtureOf(t, h); fixture.confirms != nil {
			t.Run(h.Name(), fixture.confirms)
		}
	}
}

// A REAL BOX'S NOTES REACH THE BRIDGE'S ROUTE, on every runner that reads notes:
// the wiring a run in a real box is handed is played against a real bridge, so
// the file and the bridge are held to each other rather than to copies.
//
// Mutation: each runner's case names its own.
func TestARealBoxsNotesReachTheBridgesRoute(t *testing.T) {
	t.Parallel()
	for _, h := range executor.Registered() {
		if fixture := noteFixtureOf(t, h); fixture.reaches != nil {
			t.Run(h.Name(), fixture.reaches)
		}
	}
}

// noteInstructions is the standing instruction about notes of every registered
// runner that reads notes.
func noteInstructions() []string {
	var out []string
	for _, h := range executor.Registered() {
		if reader, reads := h.(interface{ NoteInstruction() string }); reads {
			out = append(out, reader.NoteInstruction())
		}
	}
	return out
}
