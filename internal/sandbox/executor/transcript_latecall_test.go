package executor_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
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

// hookSaid is the main thread's record of what the run's own hook said at
// event, its answer's systemMessage, as Claude Code writes it just before the
// firing's context (testdata/claude/2.1.330/holdings/n1, measured).
func hookSaid(t *testing.T, event, systemMessage, at string) string {
	t.Helper()
	return `{"parentUuid":null,"isSidechain":false,"attachment":{"type":"hook_system_message","content":` +
		jsonText(t, systemMessage) + `,"hookName":"` + event + `","toolUseID":"hook-1","hookEvent":"` + event +
		`"},"type":"attachment","timestamp":"` + at + `"}`
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

// A LATE CALL ALONE IN A HOOK'S CONTEXT IS AN ENTRY OF ITS OWN, at a batch and
// at the finish alike: a context carrying no note is still searched for one.
//
// Mutation: skip a context holding no note's heading, and neither is read.
func TestALateCallAloneInAHooksContextIsAnEntryOfItsOwn(t *testing.T) {
	t.Parallel()
	kept := lateCall("error: rejected: the branch moved", `git_push(branch: "fix")`)
	finished := lateCall("success", `open_change(title: "Fix the flake")`)
	got := readClaudeRecords(t, []string{
		hookContext(t, kept),
		strings.Replace(hookContext(t, finished), `"PostToolBatch"`, `"Stop"`, 1),
	})
	sameEntries(t, got, []shown{
		{kind: executor.EntryLateCall, text: kept},
		{kind: executor.EntryLateCall, text: finished},
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
		record = append(record, hookSaid(t, "PostToolBatch", "crewlet: "+line+take, "2026-10-30T12:00:03.100Z"))
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

// A HOOK'S ANSWER NAMES WHAT REACHED THE RUN, BY KIND. Its transcript line
// names a person's notes by their senders, then the late calls by how many,
// in the words the session's reader reads them by: so the line the answer
// writes is read back, live and from a captured session, as a note where it
// names one and as a late call where it names calls alone.
//
// Mutation: write every message's line as a person's (hook-line-names-persons),
// and an answer of late calls alone names a person who sent nothing.
func TestAHookAnswerNamesWhatReachedTheRun(t *testing.T) {
	t.Parallel()
	call := executor.HookNote{Message: lateCall("success", "delegate()"), Call: true}
	note := executor.HookNote{By: "ada", Message: "## A note from ada, sent while this turn was running\nuse staging"}
	for _, tc := range []struct {
		name  string
		notes []executor.HookNote
		said  string
		kind  executor.EntryKind
	}{
		{"one late call", []executor.HookNote{call},
			"a call that finished after its run stopped waiting for it reached the run", executor.EntryLateCall},
		{"three late calls", []executor.HookNote{call, call, call},
			"3 calls that finished after their run stopped waiting for them reached the run", executor.EntryLateCall},
		{"a note and a late call", []executor.HookNote{note, call},
			"a note from ada and a call that finished after its run stopped waiting for it reached the run",
			executor.EntryNote},
		{"a note alone", []executor.HookNote{note}, "a note from ada reached the run", executor.EntryNote},
	} {
		answer := executor.HookAnswer(executor.ClaudeCodeName, "PostToolBatch", hookTake, tc.notes)
		var out struct {
			SystemMessage string `json:"systemMessage"`
		}
		if err := json.Unmarshal([]byte(answer), &out); err != nil {
			t.Fatalf("%s: the answer is not JSON: %v\n%s", tc.name, err, answer)
		}
		if want := "crewlet: " + tc.said + " " + hookToken; out.SystemMessage != want {
			t.Errorf("%s: the line is %q, want %q", tc.name, out.SystemMessage, want)
		}
		line := `{"type":"system","subtype":"informational","content":` +
			jsonText(t, "PostToolBatch says: "+out.SystemMessage) + `,"timestamp":"2026-10-30T12:00:03.100Z"}`
		if got := decodeClaude(line); got != "[steer] "+tc.said {
			t.Errorf("%s: the live line reads %q, want %q", tc.name, got, "[steer] "+tc.said)
		}
		sameEntries(t, readClaudeRecords(t, []string{hookSaid(t, "PostToolBatch", out.SystemMessage,
			"2026-10-30T12:00:03.100Z")}), []shown{{kind: tc.kind, text: tc.said}})
	}
}

// THE STANDING INSTRUCTION QUOTES THE HEADING A LATE CALL OPENS WITH, in both
// harnesses', as it quotes a note's: a model told what the engine's own
// account of a late call looks like can tell it from a person's note and from
// a tool's text imitating either. The heading is the agent layer's to render
// (prompts.LateCallArrived), which this package cannot import.
//
// Mutation: reword the heading on either side, and this goes red.
func TestTheStandingInstructionQuotesTheHeadingALateCallOpensWith(t *testing.T) {
	t.Parallel()
	heading, _, _ := strings.Cut(prompts.LateCallArrived("success", "x()", false), "\n")
	if heading != prompts.LateCallHead {
		t.Fatalf("the account opens with %q, not its heading %q", heading, prompts.LateCallHead)
	}
	for _, instruction := range noteInstructions() {
		if !strings.Contains(instruction, `"`+heading+`"`) {
			t.Errorf("the instruction does not quote the heading a late call opens with (%q):\n%s", heading,
				instruction)
		}
	}
}

// THE TRANSCRIPT READS THE HEADING A LATE CALL OPENS WITH: the account the
// engine renders, handed in a hook's context, is one late call entry, from the
// heading the agent layer renders it under.
//
// Mutation: reword the heading on either side, and the account reads as no
// entry.
func TestTheTranscriptReadsTheHeadingALateCallOpensWith(t *testing.T) {
	t.Parallel()
	account := prompts.LateCallArrived("error: denied", "post_message({})", false)
	entries := readClaudeRecords(t, []string{hookContext(t, account)})
	if len(entries) != 1 || entries[0].Kind != executor.EntryLateCall {
		t.Errorf("the rendered account reads as %+v, want one late call entry", entries)
	}
}
