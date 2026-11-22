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
	},
	executor.OpenCodeName: {
		reads: true,
		wakeRead: func(t *testing.T, wake string) []executor.TranscriptEntry {
			return readOpenCode(t, openCodeCaptureOf(t, openCodeDoc(openCodeUser("Fix the flake."),
				openCodeUser(wake))), executor.TranscriptReadBounds()).Entries
		},
		confirms: pluginAdmitsNotesAtTheRunsNextStep,
		reaches:  pluginReadsTheBridgesOwnNotesRoute,
	},
	executor.GeminiCLIName: {
		reads: true,
		wakeRead: func(t *testing.T, wake string) []executor.TranscriptEntry {
			return readGemini(t, geminiSessionWith(t, wake))
		},
		confirms: executor.GeminiConfirmsATake,
		reaches:  geminiTakeReachesTheBridgeTool,
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
