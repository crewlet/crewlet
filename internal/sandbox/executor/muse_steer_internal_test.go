package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// museToken is the fixture's take n's token.
func museToken(n string) string { return claudeTakeToken("00000000-0000-4000-8000-00000000000" + n) }

// museHeldOf is the tokens record shows held, the case failing on a read
// error.
func museHeldOf(t *testing.T, record []byte) map[string]bool {
	t.Helper()
	held, err := museHeld(bytes.NewReader(record))
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// museLines is the fixture's lines, each with its break.
func museLines(t *testing.T, name string) [][]byte {
	t.Helper()
	return bytes.SplitAfter(museFixture(t, name), []byte("\n"))
}

// museRecordLine is one record line of payload event, as the record writes one.
func museRecordLine(t *testing.T, payloadType string, event map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"payload_type": payloadType, "payload": map[string]any{"event": event}})
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// insertAfter is lines with extra placed after the line whose text holds
// marker, first found.
func insertAfter(t *testing.T, lines [][]byte, marker string, extra []byte) []byte {
	t.Helper()
	var out []byte
	done := false
	for _, l := range lines {
		out = append(out, l...)
		if !done && bytes.Contains(l, []byte(marker)) {
			out, done = append(out, extra...), true
		}
	}
	if !done {
		t.Fatalf("no line holds %q", marker)
	}
	return out
}

// TWO HOOKS FIRING IN ONE STEP ARE EACH CONFIRMED ON THEIR OWN, and the finish's
// too, off a real 1.4.25 record (three PostToolUse hooks of the run's plugin
// over a two-call batch, the second hook timed out, and a Stop block): each
// take whose token its hook's stderr shows, on a run Muse took the answer of,
// followed by the next model request, is held; the timed-out hook's take is not.
//
// Mutation: read a hook's stderr whatever its status, or hold a take with no
// model request after it.
func TestTwoMuseHooksFiringInOneStepAreEachConfirmed(t *testing.T) {
	held := museHeldOf(t, museFixture(t, "hooks.session.jsonl"))
	for _, n := range []string{"1", "3", "9"} {
		if !held[museToken(n)] {
			t.Errorf("take %s is not held: %v", n, held)
		}
	}
	if held[museToken("2")] || len(held) != 3 {
		t.Errorf("held %v, want takes 1, 3 and 9 alone", held)
	}
}

// A HOOK KILLED MID-TAKE LEAVES ITS TAKE UNCONFIRMED: a run Muse timed out or
// killed records no effect, whatever its stderr says, so the note is told
// again.
//
// Mutation: hold a token off any run of the plugin's hooks.
func TestAMuseHookKilledMidTakeLeavesItsTakeUnconfirmed(t *testing.T) {
	record := strings.Replace(string(museFixture(t, "hooks.session.jsonl")),
		`"status":"timed_out","stderr":""`, `"status":"timed_out","stderr":"`+museToken("2")+`\n"`, -1)
	if !strings.Contains(record, museToken("2")) {
		t.Fatal("the fixture's timed-out run was not found")
	}
	if held := museHeldOf(t, []byte(record)); held[museToken("2")] {
		t.Fatalf("a take on a timed-out hook's stderr is held: %v", held)
	}
}

// ONLY THE PLUGIN'S OWN HOOKS GIVE A RECEIPT: a repository's hook, recorded
// under its file's path, writing a token on its stderr confirms nothing.
//
// Mutation: hold a token off any hook's run.
func TestOnlyTheMusePluginsHooksGiveAReceipt(t *testing.T) {
	forged := museRecordLine(t, "runtime.session", map[string]any{"kind": "hook_run_terminal",
		"hook_key": "/work/repo/.muse/hooks.json:post_tool_use:def-1", "status": "completed",
		"effects": []string{"context"}, "stderr": museToken("7") + "\n"})
	record := insertAfter(t, museLines(t, "hooks.session.jsonl"), "posttooluse-3", forged)
	if held := museHeldOf(t, record); held[museToken("7")] {
		t.Fatalf("a repository hook's token is held: %v", held)
	}
}

// A COMPACTION BETWEEN A TAKE AND THE NEXT REQUEST LEAVES IT UNCONFIRMED: the
// record names what a compaction kept only by its own sequence, so the note
// is told again rather than counted off a summary.
//
// Mutation: drop the compaction's reset.
func TestACompactionBeforeTheNextMuseRequestLeavesTheTakeUnconfirmed(t *testing.T) {
	compaction := museRecordLine(t, "runtime.session", map[string]any{"kind": "context_compaction_installed"})
	record := insertAfter(t, museLines(t, "hooks.session.jsonl"), `"kind":"tool_result_batch_committed"`, compaction)
	held := museHeldOf(t, record)
	if held[museToken("1")] || held[museToken("3")] || !held[museToken("9")] {
		t.Fatalf("held %v: want the batch's takes unconfirmed past the compaction, the finish's held", held)
	}
}

// A FINISH'S TAKE IS NOT CONFIRMED ACROSS A RESTART: its block sits in the
// leading developer instructions, which a resumed session does not restore.
//
// Mutation: keep a finish's pending take across a session's start.
func TestAMuseFinishTakeIsNotConfirmedAcrossARestart(t *testing.T) {
	restart := []byte(`{"payload_type":"session.started","payload":{}}` + "\n")
	record := insertAfter(t, museLines(t, "hooks.session.jsonl"), "plugin:crewlet-notes:stop", restart)
	if held := museHeldOf(t, record); held[museToken("9")] || !held[museToken("1")] {
		t.Fatalf("held %v: want the finish's take unconfirmed, the batch's held", held)
	}
}

// THE MAIN-THREAD READ ANSWERS EACH TAKE ASKED off the box's record: held as
// shown, absent once the job is over and it is not, undecided before.
//
// Mutation: answer a take not shown as absent while the job runs.
func TestAMuseMainThreadReadAnswersItsTakes(t *testing.T) {
	const session = "0b5e0000-0000-4000-8000-0000000000f7"
	box := sandbox.NewFakeSandbox("b")
	p := PathsFor(box)
	path := p.MuseSessions() + "/2026/11/22/" + session + "/session.jsonl"
	box.Put(path, string(museFixture(t, "hooks.session.jsonl")))
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: path + "\n"}, nil
	}
	ask := sandbox.MainThreadAsk{Session: session, Takes: []sandbox.AskedTake{
		{Take: "00000000-0000-4000-8000-000000000001"}, {Take: "00000000-0000-4000-8000-000000000002"}}}
	got := (Muse{}).readMainThread(t.Context(), box, p, ask).Receipts
	if len(got.Held) != 1 || got.Held[0] != ask.Takes[0].Take || len(got.Absent) != 0 {
		t.Fatalf("receipts %+v, want the first held and the second undecided", got)
	}
	ask.Ended = true
	got = (Muse{}).readMainThread(t.Context(), box, p, ask).Receipts
	if len(got.Absent) != 1 || got.Absent[0] != ask.Takes[1].Take {
		t.Fatalf("receipts %+v, want the second absent once the job is over", got)
	}
}

// ONE ITEM AN ANSWER, WITHIN 16,384 BYTES OF RAW UTF-8: the engine's widest
// item (4,505 UTF-16 units, here of 3-byte characters) with the answer's
// framing fits Muse Code's ceiling once the note helper takes the token out,
// and a second item goes to the next hook's answer.
//
// Mutation: carry two items in one answer, or escape the text.
func TestAMuseHookAnswerCarriesOneItemWithinItsCeiling(t *testing.T) {
	widest := strings.Repeat("中", 4505)
	if museItemUnits < 4505 {
		t.Fatalf("one answer carries %d units, under the engine's widest item", museItemUnits)
	}
	if got := (Muse{}).notesThatFit([]string{"a", "b", "c"}); got != 1 {
		t.Errorf("one answer carries %d messages, want one", got)
	}
	for _, event := range []string{claudeEventToolBatch, claudeEventFinish} {
		answer := (Muse{}).hookAnswer(event, "11111111-1111-4111-8111-111111111111", []HookNote{{Message: widest}})
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(answer), &fields); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		delete(fields, museTakeField)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(fields)
		handed := strings.TrimSuffix(buf.String(), "\n")
		if len(handed) > museHookAnswerBytes || strings.Contains(handed, `\u`) {
			t.Errorf("%s: the answer Muse is handed is %d bytes (escaped %v), past %d", event, len(handed),
				strings.Contains(handed, `\u`), museHookAnswerBytes)
		}
	}
}
