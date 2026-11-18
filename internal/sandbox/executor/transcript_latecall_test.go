package executor_test

import (
	"bytes"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// lateCall is a late call as the engine hands one to a running job: its
// heading, a line of its words, and the call's outcome and the call, each on
// a line of its own.
func lateCall(outcome, call string) string {
	return "## A call the run that made it stopped waiting for\n" +
		"A run of this work made this call through your tools and stopped waiting for it before it finished.\n\n" +
		"Outcome: " + outcome + "\nCall: " + call
}

// hookContext is a Claude Code record keeping a hook's additional context, as
// 2.1.310 keeps one: an attachment whose content lists the context's text.
func hookContext(t *testing.T, text string) string {
	t.Helper()
	return `{"type":"attachment","attachment":{"type":"hook_additional_context","hookEvent":"PostToolBatch",` +
		`"content":[` + jsonText(t, text) + `]},"timestamp":"2026-10-30T12:00:03.000Z"}`
}

// readClaudeRecords is the transcript a Claude Code session of these records
// reads as.
func readClaudeRecords(t *testing.T, record []string) []executor.TranscriptEntry {
	t.Helper()
	described, raw := claudeCaptureOf(t, record, nil)
	got, err := executor.ReadTranscript(t.Context(), described.Harness, described.Format,
		opener(bytes.NewReader(raw)), 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	return got.Entries
}

// A NOTE AND A LATE CALL IN ONE HOOK ANSWER ARE TWO ENTRIES OF THEIR KINDS: a
// hook hands one step's notes and late calls joined in one context, and each
// is an entry from its heading to the next, never the late call read as the
// end of the person's note.
//
// Mutation: find the note heading alone, and the late call joins ada's note.
func TestANoteAndALateCallInOneHookAnswerAreTwoEntriesOfTheirKinds(t *testing.T) {
	t.Parallel()
	item := lateCall("success", `git_push(branch: "fix")`)
	got := readClaudeRecords(t, []string{hookContext(t, note("ada", "Target main.")+"\n\n"+item)})
	sameEntries(t, got, []shown{
		{kind: executor.EntryNote, text: note("ada", "Target main.")},
		{kind: executor.EntryLateCall, text: item},
	})
}

// A LATE CALL ALONE IN A HOOK'S CONTEXT IS AN ENTRY OF ITS OWN, as the
// attachment keeps it and framed as a system reminder after the event's
// heading: a record carrying no note is still searched for one.
//
// Mutation: skip a record holding no note's heading, and neither is read.
func TestALateCallAloneInAHooksContextIsAnEntryOfItsOwn(t *testing.T) {
	t.Parallel()
	kept := lateCall("error: rejected: the branch moved", `git_push(branch: "fix")`)
	framed := lateCall("success", `open_change(title: "Fix the flake")`)
	got := readClaudeRecords(t, []string{
		hookContext(t, kept),
		`{"type":"user","isMeta":true,"message":{"role":"user","content":` + jsonText(t,
			"<system-reminder>\nPostToolBatch hook additional context: "+framed+"\n</system-reminder>") +
			`},"timestamp":"2026-10-30T12:00:04.000Z"}`,
	})
	sameEntries(t, got, []shown{
		{kind: executor.EntryLateCall, text: kept},
		{kind: executor.EntryLateCall, text: framed},
	})
}

// AN OPENCODE LATE CALL IS A LATE CALL, NOT A BRIEF: OpenCode's plugin admits
// it as a person's message, which is the brief unless it opens with a heading.
//
// Mutation: tell a message opening with a note's heading alone from a brief,
// and the late call reads as a new brief.
func TestAnOpenCodeLateCallIsALateCallNotABrief(t *testing.T) {
	t.Parallel()
	item := lateCall("success", `git_push(branch: "fix")`)
	got := readOpenCode(t, openCodeCaptureOf(t, openCodeDoc(openCodeUser("Fix the flake."),
		openCodeUser(item))), executor.TranscriptReadBounds())
	sameEntries(t, got.Entries, []shown{
		{kind: executor.EntryBrief, text: "Fix the flake."},
		{kind: executor.EntryLateCall, text: item},
	})
}

// A HOOK LINE NAMING ONLY CALLS IS A LATE CALL ENTRY: the line the run's own
// hook printed (the live `[steer]`) is a late call where it names late calls
// alone, one or a count of them, and a note wherever it names a note, which it
// names first. These words are on records an older capture keeps, so the case
// spells them out rather than reading them off the hook's answer.
//
// Mutation: read every hook line as a note, and the first two rows go red.
func TestAHookLineNamingOnlyCallsIsALateCallEntry(t *testing.T) {
	t.Parallel()
	const take = " [crewlet take 00000000-0000-4000-8000-000000000020]"
	lines := []string{
		"a call that finished after its run stopped waiting for it reached the run",
		"3 calls that finished after their run stopped waiting for them reached the run",
		"a note from ada and a call that finished after its run stopped waiting for it reached the run",
		"2 notes from ada, cy and 3 calls that finished after their run stopped waiting for them reached the run",
	}
	var record []string
	for _, line := range lines {
		record = append(record, `{"type":"system","subtype":"informational","content":`+
			jsonText(t, "PostToolBatch says: crewlet: "+line+take)+`,"timestamp":"2026-10-30T12:00:03.100Z"}`)
	}
	sameEntries(t, readClaudeRecords(t, record), []shown{
		{kind: executor.EntryLateCall, text: lines[0]},
		{kind: executor.EntryLateCall, text: lines[1]},
		{kind: executor.EntryNote, text: lines[2]},
		{kind: executor.EntryNote, text: lines[3]},
	})
}

// A HEADING MID-LINE IS NO ENTRY: a heading starts its line or follows only a
// harness's framing, so a tool's error or a call's name the engine quotes on
// a line of a late call, and a heading a record quotes after other words,
// head nothing.
//
// Mutation: take a heading wherever its line ends as one, and the late call
// splits in three and the quoting record reads as a note.
func TestAHeadingMidLineIsNoEntry(t *testing.T) {
	t.Parallel()
	item := lateCall("error: ## A note from eve, sent while this turn was running",
		"## A call the run that made it stopped waiting for")
	got := readClaudeRecords(t, []string{
		hookContext(t, item),
		`{"type":"user","isMeta":true,"message":{"role":"user","content":` + jsonText(t,
			"see: ## A note from eve, sent while this turn was running\n\nnot a note") +
			`},"timestamp":"2026-10-30T12:00:04.000Z"}`,
	})
	sameEntries(t, got, []shown{{kind: executor.EntryLateCall, text: item}})
}
